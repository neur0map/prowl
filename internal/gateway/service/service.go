// Package service assembles the gateway as a running thing: the engine, its
// HTTP surface, and the subscription login vault, over one state directory.
// The TUI embeds a Service (it is the interactive client), and `prowl gateway
// up` runs the same Service headless; both speak to the same API on loopback,
// so there is exactly one management surface.
package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/api"
)

// Service owns the gateway's lifetime.
type Service struct {
	engine   *gateway.Engine
	server   *api.Server
	vault    *gateway.LoginVault
	signIn   *gateway.SignInRegistry
	listener net.Listener
	dir      string
	port     int
	token    string

	stopBackground func()
}

// Options configures Open. The zero value is usable.
type Options struct {
	// Dir overrides the state directory (default gateway.Dir()).
	Dir string
	// Port is the port Serve should run on (default gateway.DefaultPort).
	Port int
	// SkipCatalogSeed opts out of seeding the shipped catalogue; production
	// always seeds - an empty catalogue is a broken product.
	SkipCatalogSeed bool
}

// Open assembles the service: state directory, engine, master-key-backed
// login vault, credential-source wiring, and the HTTP surface.
func Open(ctx context.Context, opts Options) (*Service, error) {
	dir := opts.Dir
	if dir == "" {
		dir = gateway.Dir()
	}
	token, err := gateway.EnsureToken(dir)
	if err != nil {
		return nil, err
	}

	engine, err := gateway.OpenEngine(ctx, dir, gateway.EngineOptions{SkipCatalogSeed: opts.SkipCatalogSeed})
	if err != nil {
		return nil, err
	}

	svc := &Service{engine: engine, dir: dir, port: opts.Port, token: token}

	vault, err := gateway.OpenLoginVault(dir)
	if err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("open login vault: %w", err)
	}
	svc.vault = vault
	// The gateway is its own harness: subscription credentials resolve
	// through the vault this binary owns, refreshed at dispatch time.
	engine.SetCredentialSource(vault)

	server, err := api.NewServer(engine, api.Options{LocalToken: token})
	if err != nil {
		_ = engine.Close()
		return nil, err
	}
	svc.server = server
	svc.signIn = gateway.NewSignInRegistry(vault)
	server.SetSignIn(svc.signIn)
	server.SyncRoutingRuntimeConfig(ctx)
	return svc, nil
}

// Listen binds the service's port on loopback. The daemon path records the
// listener; the TUI's in-process path calls Serve directly.
func (s *Service) Listen(port int) (net.Listener, error) {
	ln, err := gateway.ListenLoopback(port)
	if err != nil {
		return nil, err
	}
	s.listener = ln
	s.port = port
	return ln, nil
}

// Serve runs the HTTP surface until ctx ends, then drains. The background
// subsystems (health passes, quota persistence, cooldown flushing) start
// with the server and stop before it returns.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	if s.listener == nil {
		s.listener = ln
	}
	s.stopBackground = s.engine.StartBackground(ctx)
	httpSrv := &http.Server{
		Handler:           s.server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(drain)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			if s.stopBackground != nil {
				s.stopBackground()
			}
			s.stopBackground = nil
			return err
		}
	}
	if s.stopBackground != nil {
		s.stopBackground()
		s.stopBackground = nil
	}
	return nil
}

// ServeInProcess runs the surface against a test/harness caller without a
// real socket: the returned handler is the same composed surface.
func (s *Service) ServeInProcess() http.Handler { return s.server.Handler() }

// Close releases every resource the service owns. Safe after Serve.
func (s *Service) Close() error {
	var first error
	if err := s.engine.Close(); err != nil {
		first = err
	}
	if err := s.server.Shutdown(context.Background()); err != nil && first == nil {
		first = err
	}
	return first
}

// Engine exposes the assembled routing engine for in-process consumers.
func (s *Service) Engine() *gateway.Engine { return s.engine }

// Server exposes the HTTP surface.
func (s *Service) Server() *api.Server { return s.server }

// Logins exposes the subscription vault for sign-in screens.
func (s *Service) Logins() *gateway.LoginVault { return s.vault }

// SignIn exposes the interactive flow registry.
func (s *Service) SignIn() *gateway.SignInRegistry { return s.signIn }

// Addr is the bound address once listening.
func (s *Service) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return fmt.Sprintf("127.0.0.1:%d", s.Port())
}

// Port is the port the service listens (or will listen) on.
func (s *Service) Port() int {
	if s.port == 0 {
		return gateway.DefaultPort
	}
	return s.port
}

// Token is the machine-local credential every local client presents.
func (s *Service) Token() string { return s.token }

// BaseURL is the OpenAI-compatible root a harness registers.
func (s *Service) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1", s.Port())
}

// UnifiedAPIKey returns the machine credential the TUI shows and harnesses
// hold, minting it on first use.
func (s *Service) UnifiedAPIKey(ctx context.Context) (string, error) {
	return s.server.UnifiedAPIKey(ctx)
}
