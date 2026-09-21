package gateway

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	daemonStartTimeout = 20 * time.Second
	daemonStopTimeout  = 10 * time.Second
)

// DaemonStartResult describes a successful request to keep the gateway running.
// AlreadyRunning distinguishes attaching to an existing process from spawning
// one, while LogPath is always the file operators can inspect.
type DaemonStartResult struct {
	AlreadyRunning bool
	URL            string
	LogPath        string
}

// DaemonStopState is the safe outcome of stopping a tracked gateway.
type DaemonStopState string

const (
	DaemonNotTracked DaemonStopState = "not_tracked"
	DaemonStale      DaemonStopState = "stale"
	DaemonStopped    DaemonStopState = "stopped"
)

// DaemonStopResult describes what happened without turning an absent daemon into
// an error. PID is populated for stale and stopped records.
type DaemonStopResult struct {
	State DaemonStopState
	PID   int
}

// TrackedDaemonRunning reports whether the requested port's state record names
// the same live process that identifies itself on that port. PID liveness alone
// is insufficient because operating systems reuse process ids. Legacy integer
// records are accepted only for the historical default port, and the
// authenticated probe still has to bind that process to the listener.
func TrackedDaemonRunning(port ...int) bool {
	return trackedDaemonRunning(context.Background(), Dir(), pidFilePort(port))
}

func trackedDaemonRunning(ctx context.Context, dir string, port int) bool {
	record, err := ReadPIDRecord(dir, port)
	if err != nil || record.PID == 0 || !pidRecordMatchesPort(record, port) || !ProcessAlive(record.PID) {
		return false
	}
	token, err := EnsureToken(dir)
	if err != nil {
		return false
	}
	livePID, ok := probeGatewayPID(ctx, port, token)
	return ok && livePID == record.PID
}

func pidRecordMatchesPort(record PIDRecord, port int) bool {
	return record.Port == port || (record.Port == 0 && port == DefaultPort)
}

// StartDaemon starts the current Prowl executable as a detached gateway and
// returns only after its authenticated health endpoint answers. The child does
// not inherit ctx: a daemon selected in the TUI must survive that TUI exiting.
func StartDaemon(ctx context.Context, executable string, port int) (DaemonStartResult, error) {
	if port == 0 {
		port = DefaultPort
	}
	token, err := EnsureToken(Dir())
	if err != nil {
		return DaemonStartResult{}, err
	}
	logPath := filepath.Join(Dir(), "daemon.log")
	if url, ok := Running(ctx, port, token); ok {
		return DaemonStartResult{AlreadyRunning: true, URL: url, LogPath: logPath}, nil
	}
	if executable == "" {
		return DaemonStartResult{}, fmt.Errorf("start gateway daemon: executable path is empty")
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return DaemonStartResult{}, err
	}
	defer func() { _ = logFile.Close() }()

	child := exec.Command(executable, "gateway", "serve", "--port", fmt.Sprint(port))
	child.Stdout = logFile
	child.Stderr = logFile
	DetachCommand(child)
	if err := child.Start(); err != nil {
		return DaemonStartResult{}, fmt.Errorf("spawn gateway daemon: %w", err)
	}
	_ = child.Process.Release()

	deadline := time.NewTimer(daemonStartTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return DaemonStartResult{}, ctx.Err()
		case <-deadline.C:
			return DaemonStartResult{}, fmt.Errorf("gateway did not come up within %s; check %s", daemonStartTimeout, logPath)
		case <-ticker.C:
			if url, ok := Running(ctx, port, token); ok {
				return DaemonStartResult{URL: url, LogPath: logPath}, nil
			}
		}
	}
}

// StopTrackedDaemon gracefully terminates only the process recorded for the
// requested port after that gateway confirms the same process id. Missing and
// stale records are normal states; an unverified listener is never signalled.
func StopTrackedDaemon(ctx context.Context, port ...int) (DaemonStopResult, error) {
	return stopTrackedDaemon(ctx, Dir(), pidFilePort(port))
}

func stopTrackedDaemon(ctx context.Context, dir string, port int) (DaemonStopResult, error) {
	record, err := ReadPIDRecord(dir, port)
	if err != nil {
		return DaemonStopResult{}, err
	}
	pid := record.PID
	if pid == 0 {
		return DaemonStopResult{State: DaemonNotTracked}, nil
	}
	if !ProcessAlive(pid) {
		if err := RemovePIDFileIfPID(dir, pid, port); err != nil {
			return DaemonStopResult{}, err
		}
		return DaemonStopResult{State: DaemonStale, PID: pid}, nil
	}
	if !pidRecordMatchesPort(record, port) {
		return DaemonStopResult{}, fmt.Errorf("refusing to signal gateway pid %d: record port %d does not match requested port %d", pid, record.Port, port)
	}
	token, err := EnsureToken(dir)
	if err != nil {
		return DaemonStopResult{}, fmt.Errorf("read gateway identity credential: %w", err)
	}
	livePID, ok := probeGatewayPID(ctx, port, token)
	if !ok {
		return DaemonStopResult{}, fmt.Errorf("refusing to signal gateway pid %d: the gateway on port %d did not prove its identity", pid, port)
	}
	if livePID != pid {
		if err := RemovePIDFileIfPID(dir, pid, port); err != nil {
			return DaemonStopResult{}, err
		}
		return DaemonStopResult{State: DaemonStale, PID: pid}, nil
	}
	if err := SignalTerminate(pid); err != nil {
		return DaemonStopResult{}, err
	}

	deadline := time.NewTimer(daemonStopTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return DaemonStopResult{}, ctx.Err()
		case <-deadline.C:
			return DaemonStopResult{}, fmt.Errorf("gateway pid %d did not exit within %s", pid, daemonStopTimeout)
		case <-ticker.C:
			if ProcessAlive(pid) {
				continue
			}
			if err := RemovePIDFileIfPID(dir, pid, port); err != nil {
				return DaemonStopResult{}, err
			}
			return DaemonStopResult{State: DaemonStopped, PID: pid}, nil
		}
	}
}
