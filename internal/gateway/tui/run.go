package tui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/service"
)

// Run is the interactive console. It attaches to a gateway already listening
// on the port when one exists - several terminals sharing one gateway is the
// point of a daemon - and otherwise starts an ephemeral one in-process and
// stops it on exit. The client path is identical either way.
func Run(ctx context.Context, port int, version string) error {
	_, restoreLogs, err := gateway.RouteLogsToFile("")
	if err != nil {
		return err
	}
	defer restoreLogs()
	if port == 0 {
		port = gateway.DefaultPort
	}
	token, err := gateway.EnsureToken(gateway.Dir())
	if err != nil {
		return err
	}

	// Attach first: cheap, and a running daemon is always the right answer.
	if url, ok := gateway.Running(ctx, port, token); ok {
		_ = url
		app, tuiErr := runProgram(ctx, NewClient(port, token), false, version, port)
		if app != nil && app.DaemonManageable && !app.KeepGateway {
			stopCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			_, stopErr := gateway.StopTrackedDaemon(stopCtx, port)
			cancel()
			return errors.Join(tuiErr, stopErr)
		}
		return tuiErr
	}

	// Own the port: bind it for the whole session, then serve in background.
	ln, err := gateway.ListenLoopback(port)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) {
			return fmt.Errorf("cannot bind 127.0.0.1:%d and no gateway is answering there: %w", port, err)
		}
		return err
	}
	svc, err := service.Open(ctx, service.Options{Port: port})
	if err != nil {
		_ = ln.Close()
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = svc.Close()
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- svc.Serve(runCtx, ln) }()

	// Confirm it is actually answering before painting a TUI over a corpse.
	if _, ok := gateway.Running(ctx, port, token); !ok {
		// Give the cold start (catalogue seed) one more beat; a seed that
		// takes seconds is normal on first boot.
		if !waitForGateway(ctx, port, token) {
			select {
			case err := <-serveErr:
				if err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("gateway failed to start: %w", err)
				}
			default:
			}
			return fmt.Errorf("gateway did not come up on port %d", port)
		}
	}

	app, tuiErr := runProgram(ctx, NewClient(port, token), true, version, port)
	cancel()
	serverErr := <-serveErr
	closeErr := svc.Close()
	closed = true
	if app == nil || !app.KeepGateway {
		return errors.Join(tuiErr, serverErr, closeErr)
	}

	bin, executableErr := os.Executable()
	if executableErr != nil {
		return errors.Join(tuiErr, serverErr, closeErr, executableErr)
	}
	startCtx, startCancel := context.WithTimeout(context.Background(), 22*time.Second)
	_, daemonErr := gateway.StartDaemon(startCtx, bin, port)
	startCancel()
	return errors.Join(tuiErr, serverErr, closeErr, daemonErr)
}

func waitForGateway(ctx context.Context, port int, token string) bool {
	for range 40 {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if url, ok := gateway.Running(ctx, port, token); ok {
			_ = url
			return true
		}
		// 250ms * 40 ≈ 10s of patience for a cold catalogue seed.
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

func runProgram(ctx context.Context, client *Client, owned bool, version string, port int) (*App, error) {
	app := New(client, owned, version)
	app.DaemonManageable = owned || gateway.TrackedDaemonRunning(port)
	app.KeepGateway = !owned && app.DaemonManageable
	p := tea.NewProgram(app, tea.WithContext(ctx))
	final, err := p.Run()
	if result, ok := final.(*App); ok {
		return result, err
	}
	return app, err
}
