// Package api is the gateway's HTTP surface: the /v1 inference plane and the
// management API the TUI speaks. There is no browser UI. Every route except
// the minimal liveness endpoint is authenticated by the same machine
// credentials as /v1 because the only client is a local trusted process.
package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// Server owns the gateway's HTTP surface.
type Server struct {
	engine *gateway.Engine
	mux    *http.ServeMux

	// settings owns the unified key that authenticates the inference plane.
	settings *settingsStore

	// machineKey, when set, overrides the stored key. It exists for tests and
	// for an operator pinning a key by configuration; normal operation reads
	// the stored value so a regenerate takes effect at once.
	machineKey string

	// localToken is the machine-local bootstrap credential. It exists because
	// Prowl registers the gateway into harnesses using this token,
	// while the unified key is what external applications hold; a stored copy
	// of the unified key would go stale the moment the operator regenerates
	// it, whereas this token is never regenerated. It grants everything this
	// surface does, and it lives at mode 0600 in the gateway state directory.
	localToken string

	// signIn, when set, runs the interactive subscription flows. The
	// gateway binary always sets it; an API surface assembled without it
	// answers the sign-in routes with a conflict rather than pretending.
	signIn *gateway.SignInRegistry

	limiter *rateLimiter

	// stats caches the aggregated request trail the scorer reads. It is on
	// the server rather than per request so a week of rows is re-read once a
	// minute instead of once a request.
	stats statsCache

	// keyRotation is the process-lived round-robin cursor the per-request chain
	// consults when the per-key bandit has no signal to rank a model's keys on,
	// so unmeasured keys rotate fairly instead of always leading with the
	// lowest row id. The zero value is usable.
	keyRotation keyRotator
}

// Options configures the surface.
type Options struct {
	// MachineKey pins the credential guarding /v1. Leave it empty in normal
	// operation: the key is then read from settings on every check, which is
	// what makes a regenerate invalidate the old one immediately instead of
	// at the next restart.
	MachineKey string

	// LocalToken is the machine-local bootstrap credential, accepted by every
	// gated route alongside the unified key.
	LocalToken string
}

// NewServer builds the surface over an assembled engine.
func NewServer(engine *gateway.Engine, opts Options) (*Server, error) {
	s := &Server{
		engine:     engine,
		mux:        http.NewServeMux(),
		machineKey: opts.MachineKey,
		localToken: opts.LocalToken,
		limiter:    newRateLimiter(),
	}
	if err := s.routes(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetSignIn installs the sign-in flow registry over the service's login vault.
func (s *Server) SetSignIn(r *gateway.SignInRegistry) {
	s.signIn = r
	r.SetOnComplete(func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		s.engine.ReconcileLoginModels(ctx)
	})
}

// Handler returns the composed handler. The cross-site guard wraps everything
// rather than individual routes: a loopback service that speaks JSON must
// still refuse a drive-by browser request, even though the intended client is
// the TUI.
func (s *Server) Handler() http.Handler { return guardCrossSite(s.mux) }

func (s *Server) routes() error {
	// Liveness is deliberately ungated, but a credential-holding client can
	// challenge the listener before sending a bearer token. The proof is bound
	// to the actual local port, so another local listener cannot relay a
	// challenge to a genuine gateway on a different port.
	s.mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"status": "ok", "pid": os.Getpid(), "timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		if challenge := r.Header.Get(gateway.PingChallengeHeader); challenge != "" {
			if proof := gateway.PingProof(s.localToken, localListenerPort(r), challenge); proof != "" {
				body["proof"] = proof
			}
		}
		WriteJSON(w, http.StatusOK, body)
	})

	s.registerKeysRoutes()
	s.registerKeysCustomRoutes()
	s.registerInferenceRoutes()
	s.registerModalityRoutes()
	s.registerSettingsRoutes()
	s.registerRoutingRoutes()
	s.registerStatusRoutes()
	s.registerUsageRoutes()
	s.registerStatusV1Routes()

	s.registerDirectoryRoutes()
	s.registerLoginRoutes()
	s.registerKeyActivityRoutes()
	s.registerChainPresetRoutes()
	s.registerCatalogBrowseRoutes()

	// An unmatched API or inference path must answer in JSON: this surface
	// serves no HTML, and a 200 with a body no client asked for is the worst
	// possible answer to a typo'd endpoint.
	s.mux.HandleFunc("/api/", s.handleAPINotFound)
	s.mux.HandleFunc("/v1/", s.handleAPINotFound)
	s.mux.HandleFunc("/v1beta/", s.handleAPINotFound)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotFound, TypeNotFound,
			"Prowl's gateway has no web UI - launch `prowl` for the unified console")
	})
	return nil
}
func localListenerPort(r *http.Request) int {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || addr == nil {
		return 0
	}
	_, rawPort, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return 0
	}
	return port
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, http.StatusNotFound, TypeNotFound, "no such endpoint: "+r.URL.Path)
}

// RequireKey gates the management API with the same credentials as /v1: the
// machine-local token or the unified key. There is no separate operator
// session - the product's only administrator is a local process holding a
// local credential, and a second credential system (accounts, sessions,
// reset codes) would be ceremony that never stops running.
//
// A failure answers with TypeInvalidRequest, NOT TypeAuthentication: the
// error type the reference reserved for ending a browser session has no
// session to end here, and reusing it would teach a future client to log a
// human out over someone else's stale key.
func (s *Server) RequireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(adminBucket, clientIP(r)) {
			WriteError(w, http.StatusTooManyRequests, TypeRateLimit, "too many requests")
			return
		}
		if !s.keyAuthenticates(r) {
			WriteError(w, http.StatusUnauthorized, TypeInvalidRequest, "invalid api key")
			return
		}
		next(w, r)
	}
}

// RequireMachineKey wraps an inference-plane handler with the same gate.
func (s *Server) RequireMachineKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow(proxyBucket, clientIP(r)) {
			WriteError(w, http.StatusTooManyRequests, TypeRateLimit, "too many requests")
			return
		}
		if !s.keyAuthenticates(r) {
			WriteError(w, http.StatusUnauthorized, TypeInvalidRequest, "invalid api key")
			return
		}
		next(w, r)
	}
}

// keyAuthenticates checks the presented credential against live state, so
// regenerating the unified key stops the old one working on the very next
// request rather than after a restart.
func (s *Server) keyAuthenticates(r *http.Request) bool {
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	if secureEqual(presented, s.localToken) {
		return true
	}
	if s.machineKey != "" {
		return secureEqual(presented, s.machineKey)
	}
	if s.settings == nil {
		return false
	}
	ok, err := s.settings.authenticateMachineKey(r.Context(), presented)
	return err == nil && ok
}

func bearerToken(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	auth := r.Header.Get("Authorization")
	if after, ok := cutPrefixFold(auth, "bearer "); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(auth)
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// bucket names the two independent per-IP budgets. The inference plane is far
// busier than the management API, so one shared budget would let normal
// inference traffic throttle the operator out of their own settings.
type bucket string

const (
	adminBucket bucket = "admin"
	proxyBucket bucket = "proxy"
)

// Per-minute allowances, matching the reference's limiters.
var bucketLimits = map[bucket]int{
	adminBucket: 600,
	proxyBucket: 120,
}

// maxTrackedIPs bounds the limiter's memory so a spray of forged source
// addresses cannot grow it without limit.
const maxTrackedIPs = 10000

type rateLimiter struct {
	mu      sync.Mutex
	windows map[bucket]map[string]*fixedWindow
	now     func() time.Time
}

type fixedWindow struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		windows: map[bucket]map[string]*fixedWindow{
			adminBucket: {}, proxyBucket: {},
		},
		now: time.Now,
	}
}

func (l *rateLimiter) allow(b bucket, ip string) bool {
	limit := bucketLimits[b]
	if limit <= 0 || ip == "" {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	per := l.windows[b]

	if len(per) > maxTrackedIPs {
		// Drop the table rather than scan it: the budget is per minute, so
		// the cost of forgetting is at most one minute of leniency, and the
		// alternative is unbounded growth under a spoofed-source flood.
		l.windows[b] = map[string]*fixedWindow{}
		per = l.windows[b]
	}

	win, ok := per[ip]
	if !ok || now.Sub(win.start) >= time.Minute {
		per[ip] = &fixedWindow{start: now, count: 1}
		return true
	}
	if win.count >= limit {
		return false
	}
	win.count++
	return true
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// secureEqual compares credentials in constant time.
func secureEqual(got, want string) bool {
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	var diff byte
	for i := range len(got) {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

// Shutdown is a placeholder for symmetry with the engine's lifecycle; the
// surface itself holds no resources beyond the engine.
func (s *Server) Shutdown(context.Context) error { return nil }

// UnifiedAPIKey returns the current machine credential in plaintext, minting
// it on first use. The TUI's overview and the harness injector are the only
// callers; both already hold the state directory, so revealing here leaks to
// no transport.
func (s *Server) UnifiedAPIKey(ctx context.Context) (string, error) {
	if s.settings == nil {
		return "", errors.New("the settings surface is not mounted")
	}
	key, err := s.settings.unifiedAPIKey(ctx)
	if err != nil {
		return "", err
	}
	return key.Reveal(), nil
}
