package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
)

// testServer builds the HTTP surface over an engine with an EMPTY catalog.
//
// Production seeds the shipped models on open, but a route test almost always
// wants to assert on rows it inserted itself: a seeded catalog makes list
// shapes, counts and generated ids depend on what the catalog happens to ship,
// so a test would fail when the catalog grows rather than when its subject
// breaks. Tests that genuinely need the shipped catalog use testSeededServer.
func testServer(t *testing.T, opts Options) *Server {
	t.Helper()
	return newTestServer(t, opts, gateway.EngineOptions{SkipCatalogSeed: true})
}

// testSeededServer builds the surface over an engine in its PRODUCTION state,
// catalog and all, for the routes whose whole job is to serve it.
func testSeededServer(t *testing.T, opts Options) *Server {
	t.Helper()
	return newTestServer(t, opts, gateway.EngineOptions{})
}

func newTestServer(t *testing.T, opts Options, engineOpts gateway.EngineOptions) *Server {
	t.Helper()
	// The surface has no operator account to bootstrap: the only credentials
	// are the unified key and the machine-local token. So the harness pins a
	// machine key by default, which is what every gated route test then
	// presents. A test that specifically wants an unconfigured credential
	// passes one explicitly ("" is not expressible, so it uses bareServer).
	if opts.MachineKey == "" {
		opts.MachineKey = compatMachineKey
	}
	engine, err := gateway.OpenEngine(context.Background(), t.TempDir(), engineOpts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })

	s, err := NewServer(engine, opts)
	require.NoError(t, err)
	return s
}

func do(t *testing.T, s *Server, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:50000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func errorType(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed), "body was %q", body)
	return parsed.Error.Type
}

// bareServer is a surface whose gate has no pinned key and no local token, so
// the stored unified key is the only thing that can authenticate. Used by the
// credential-lifecycle tests.
func bareServer(t *testing.T) *Server {
	t.Helper()
	engine, err := gateway.OpenEngine(context.Background(), t.TempDir(), gateway.EngineOptions{SkipCatalogSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = engine.Close() })
	s, err := NewServer(engine, Options{})
	require.NoError(t, err)
	return s
}

// authed presents the pinned key the way a client does.
func authed(token string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
}

// keyedServer is the standard gated surface: a machine key pinned so every
// /api and /v1 route answers, with an empty catalog.
func keyedServer(t *testing.T) (*Server, map[string]string) {
	t.Helper()
	s := testServer(t, Options{MachineKey: compatMachineKey})
	return s, authed(compatMachineKey)
}

// TestInferenceKeyFailureIsNotASessionEnd keeps the one surviving half of the
// reference's subtlest constraint: a 401 from the credential gate must never
// carry authentication_error, so a stale application key can never be
// interpreted as "sign the operator out".
func TestInferenceKeyFailureIsNotASessionEnd(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey})

	// A management route with no credential at all.
	resp, body := do(t, s, http.MethodGet, "/api/keys", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
		"a missing credential must not read as a session-end")

	// An inference-plane key failure is a 401 too, for an application.
	gated := s.RequireMachineKey(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"served": "yes"})
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec := httptest.NewRecorder()
	gated(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, rec.Body.String()),
		"an application's bad api key must not carry authentication_error")
	require.NotContains(t, rec.Body.String(), "served")
}

// TestManagementGateAcceptsBothCredentials covers the two bearer forms: the
// pinned machine key and the machine-local token.
func TestManagementGateAcceptsBothCredentials(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: compatMachineKey, LocalToken: "local-token"})

	for name, headers := range map[string]map[string]string{
		"machine key": authed(compatMachineKey),
		"local token": authed("local-token"),
		"lowercase":   {"Authorization": "bearer " + compatMachineKey},
		"x-api-key":   {"X-Api-Key": compatMachineKey, "Content-Type": "application/json"},
	} {
		resp, _ := do(t, s, http.MethodGet, "/api/keys", "", headers)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s must authenticate", name)
	}
}

// TestUnmatchedAPIPathAnswersInJSON stops the SPA fallback from swallowing a
// mistyped endpoint. Returning HTML with a 200 makes a client report "the API
// isn't reachable at this origin", which sends whoever is debugging it after
// the wrong problem entirely.
func TestUnmatchedAPIPathAnswersInJSON(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	for _, path := range []string{"/api/nope", "/api/keys/typo", "/v1/nope", "/v1beta/models"} {
		resp, body := do(t, s, http.MethodGet, path, "", nil)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, "%s must 404", path)
		require.Contains(t, resp.Header.Get("Content-Type"), "application/json", "%s must answer in JSON", path)
		require.NotContains(t, body, "<div id=\"root\">", "%s must not return an app shell", path)
	}
}

// TestNoWebUICroute is the port's one deliberate deviation stated out loud: a
// browser pointed at this gateway gets told the UI is the TUI, not a blank
// page or a redirect to a login nobody can complete.
func TestNoWebUICroute(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	resp, body := do(t, s, http.MethodGet, "/", "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, body, "no web UI")
}

// TestPingNeedsNoCredential keeps the health check usable: one that requires
// a token is one nobody wires up.
func TestPingNeedsNoCredential(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	resp, body := do(t, s, http.MethodGet, "/api/ping", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `"status":"ok"`)
}

func TestPingChallengeProofIsBoundToListenerPort(t *testing.T) {
	t.Parallel()
	const (
		token     = "opaque-machine-token"
		challenge = "fresh-client-challenge"
		port      = 17342
	)
	s := testServer(t, Options{LocalToken: token})
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	req = req.WithContext(context.WithValue(
		req.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port},
	))
	req.Header.Set(gateway.PingChallengeHeader, challenge)
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Proof string `json:"proof"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, gateway.PingProof(token, port, challenge), body.Proof)
	require.NotEqual(t, gateway.PingProof(token, port+1, challenge), body.Proof)
	require.NotContains(t, rec.Body.String(), token)
}

// TestUnconfiguredInferencePlaneRefusesRatherThanServingOpen is the safe
// default: with no credential configured, every gated route is closed.
//
// It answers 401 rather than "not configured", deliberately. A 503 would tell
// an unauthenticated caller whether this gateway has been set up yet, and
// "no credential matches" is the honest answer either way.
func TestUnconfiguredInferencePlaneRefusesRatherThanServingOpen(t *testing.T) {
	t.Parallel()
	// A genuinely unconfigured surface: no pinned machine key, no local
	// token, no stored unified key. testServer would silently pin the
	// compatibility key, so the gate would close on a mismatched credential
	// and the "unconfigured serves open" branch would never be exercised.
	s := bareServer(t)

	handler := s.RequireMachineKey(func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"served": "yes"})
	})

	for name, headers := range map[string]map[string]string{
		"no credential":   {},
		"invented bearer": {"Authorization": "Bearer made-up"},
		"empty bearer":    {"Authorization": "Bearer "},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.RemoteAddr = "127.0.0.1:50000"
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s must be refused", name)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, rec.Body.String()),
			"%s must not sign the operator out", name)
		require.NotContains(t, rec.Body.String(), "served")
	}
}

// TestRateLimiterBoundsItsMemory stops a spray of forged source addresses
// from growing the limiter without limit.
func TestRateLimiterBoundsItsMemory(t *testing.T) {
	t.Parallel()

	l := newRateLimiter()
	// Spray more than the cap in GENUINELY DISTINCT sources. The old
	// generator only produced 3*26*10 = 780 unique strings, far under
	// maxTrackedIPs, so len(per) could never reach the drop threshold and
	// the assertion held even for an unbounded limiter. Each i maps to a
	// unique 10.0.X.Y here (i = X*256 + Y), so all maxTrackedIPs+100 are
	// distinct and the bound must actually engage.
	for i := range maxTrackedIPs + 100 {
		l.allow(adminBucket, fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	require.LessOrEqual(t, len(l.windows[adminBucket]), maxTrackedIPs+1,
		"the limiter must not grow without bound under spoofed sources")
}

// TestBucketsAreIndependent keeps busy inference traffic from throttling the
// operator out of their own settings page.
func TestBucketsAreIndependent(t *testing.T) {
	t.Parallel()

	l := newRateLimiter()
	const ip = "127.0.0.1"
	for range bucketLimits[proxyBucket] {
		require.True(t, l.allow(proxyBucket, ip))
	}
	require.False(t, l.allow(proxyBucket, ip), "the proxy budget must be spent")
	require.True(t, l.allow(adminBucket, ip), "the dashboard budget must be untouched")
}

// TestSignInRoutesAreCredentialGated pins the new sign-in surface behind the
// same gate as everything else: no browser flow may start unauthenticated.
// (The attached-surface refusal is the second half: this harness has no
// sign-in registry, so an authenticated call answers 409, never a 404.)
func TestSignInRoutesAreCredentialGated(t *testing.T) {
	t.Parallel()
	bare := bareServer(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/logins/platforms"},
		{http.MethodPost, "/api/signin"},
		{http.MethodGet, "/api/signin/fake123"},
		{http.MethodDelete, "/api/signin/fake123"},
	} {
		resp, body := do(t, bare, tc.method, tc.path, `{}`, nil)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s %s", tc.method, tc.path)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
	}

	s := testServer(t, Options{}) // pinned machine key, no sign-in registry
	resp, body := do(t, s, http.MethodPost, "/api/signin", `{"provider":"anthropic"}`,
		authed(compatMachineKey))
	require.Equal(t, http.StatusConflict, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "attached")
}
