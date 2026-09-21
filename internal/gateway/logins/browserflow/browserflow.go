// Package browserflow implements loopback OAuth authorization with PKCE.
package browserflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neur0map/prowl/internal/gateway/logins/callback"
	"github.com/neur0map/prowl/internal/gateway/logins/oauth"
)

// Config contains the provider-specific authorization and exchange contract.
type Config struct {
	AuthorizeURL string
	ClientID     string
	RedirectURI  string
	Scope        string
	ExtraAuth    url.Values
	Subject      string
	Exchange     func(ctx context.Context, code, redirectURI, verifier, state string) (*oauth.Token, error)
}

// Flow owns a callback listener. Call Close when its UI or CLI is dismissed.
type Flow struct {
	URL       string
	ctx       context.Context
	cancel    context.CancelFunc
	server    *http.Server
	result    chan result
	closeOnce sync.Once
	accepted  atomic.Bool
	waited    atomic.Bool
}

type result struct {
	token *oauth.Token
	err   error
}

// Start binds the registered callback port before publishing the browser URL.
func Start(ctx context.Context, cfg Config) (*Flow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	redirect, err := url.Parse(cfg.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("parse OAuth redirect: %w", err)
	}
	port, err := strconv.Atoi(redirect.Port())
	host := redirect.Hostname()
	if err != nil || port < 1 || port > 65535 || redirect.Scheme != "http" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
		return nil, errors.New("OAuth redirect must be an HTTP loopback URL with a fixed port")
	}
	authorize, err := url.Parse(cfg.AuthorizeURL)
	if err != nil || authorize == nil || authorize.Scheme != "https" || authorize.Host == "" || authorize.User != nil || authorize.Fragment != "" || cfg.ClientID == "" || cfg.Exchange == nil {
		return nil, errors.New("invalid OAuth authorization configuration")
	}
	verifier := base64.RawURLEncoding.EncodeToString(randomBytes(64))
	state := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	sum := sha256.Sum256([]byte(verifier))
	query := authorize.Query()
	for key, values := range cfg.ExtraAuth {
		query[key] = append([]string(nil), values...)
	}
	query.Set("response_type", "code")
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", cfg.RedirectURI)
	query.Set("scope", cfg.Scope)
	query.Set("state", state)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	query.Set("code_challenge_method", "S256")
	authorize.RawQuery = query.Encode()
	listeners, err := listenLoopbacks(ctx, host, redirect.Port())
	if err != nil {
		return nil, fmt.Errorf("bind OAuth callback port %d: %w", port, err)
	}
	flowCtx, cancel := context.WithCancel(ctx)
	f := &Flow{URL: authorize.String(), ctx: flowCtx, cancel: cancel, result: make(chan result, 1)}
	path := redirect.Path
	if path == "" {
		path = "/"
	}
	f.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			return
		}
		if q.Get("error") == "" && (len(q["code"]) != 1 || q.Get("code") == "") {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			return
		}
		if !f.accepted.CompareAndSwap(false, true) {
			http.Error(w, "Authorization already received", http.StatusConflict)
			return
		}
		var outcome result
		if q.Get("error") != "" {
			outcome.err = errors.New("authorization was denied; retry login to continue")
		} else {
			outcome.token, outcome.err = cfg.Exchange(f.ctx, q.Get("code"), cfg.RedirectURI, verifier, state)
		}
		page := callback.Result{Subject: cfg.Subject}
		if outcome.err != nil {
			page.ErrorCode = "authorization_failed"
			page.ErrorDescription = "Return to Prowl for details and retry login."
		}
		_ = callback.Serve(w, page)
		_ = http.NewResponseController(w).Flush()
		f.result <- outcome
	})}
	for _, listener := range listeners {
		go func() {
			if err := f.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				cancel()
			}
		}()
	}
	go func() { <-flowCtx.Done(); _ = f.Close() }()
	return f, nil
}

// Wait returns the exchanged token, or stops the flow when either context ends.
func (f *Flow) Wait(ctx context.Context) (*oauth.Token, error) {
	if !f.waited.CompareAndSwap(false, true) {
		return nil, errors.New("OAuth flow has already been awaited")
	}
	defer f.Close()
	select {
	case value := <-f.result:
		return value.token, value.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

// Close cancels token exchange and releases the listener, including before Wait.
func (f *Flow) Close() error {
	f.closeOnce.Do(func() { f.cancel(); _ = f.server.Close() })
	return nil
}

func listenLoopbacks(ctx context.Context, host, port string) ([]net.Listener, error) {
	config := &net.ListenConfig{}
	if host != "localhost" {
		network := "tcp4"
		if host == "::1" {
			network = "tcp6"
		}
		listener, err := config.Listen(ctx, network, net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		return []net.Listener{listener}, nil
	}

	ipv4, err := config.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, err
	}
	listeners := []net.Listener{ipv4}

	// localhost commonly resolves to ::1 first. Probe the IPv6 loopback before
	// requiring the companion listener: hosts with IPv6 disabled should still
	// use IPv4, while a real collision on this fixed port must fail the flow
	// rather than hand the authorization code to another local process.
	probe, err := config.Listen(ctx, "tcp6", net.JoinHostPort("::1", "0"))
	if err != nil {
		return listeners, nil
	}
	_ = probe.Close()
	ipv6, err := config.Listen(ctx, "tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		_ = ipv4.Close()
		return nil, err
	}
	return append(listeners, ipv6), nil
}

func randomBytes(size int) []byte {
	value := make([]byte, size)
	// crypto/rand.Read is guaranteed to fill the buffer or terminate the process.
	_, _ = rand.Read(value)
	return value
}
