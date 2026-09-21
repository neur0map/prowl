package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// serveThroughGuard drives a fully built request through the public handler,
// which is the surface that wraps the mux in guardCrossSite. Building the
// request by hand (rather than via do) is what lets a test set Host and the
// browser-only fetch-metadata headers an attacker's page would carry.
func serveThroughGuard(t *testing.T, s *Server, req *http.Request) (*http.Response, string) {
	t.Helper()
	if req.RemoteAddr == "" {
		req.RemoteAddr = "127.0.0.1:50000"
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func stateChangingReq(method, target, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	return httptest.NewRequest(method, target, r)
}

const crossSiteRefusal = "cross-site requests are not accepted"

// TestCrossSiteGuardRefusesHostileFetchMetadata is the drive-by defence: a
// state-changing request the browser labels as coming from another site is
// refused before it reaches the router, so the surface that needs no session
// (account bootstrap in the reference) cannot be driven by an attacker's page.
//
// "same-site" is refused alongside "cross-site": both mean another origin is
// acting through the user's browser, which a same-origin session token cannot.
func TestCrossSiteGuardRefusesHostileFetchMetadata(t *testing.T) {
	t.Parallel()
	s, _ := keyedServer(t)

	for _, site := range []string{"cross-site", "same-site"} {
		req := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
		req.Header.Set("Sec-Fetch-Site", site)
		resp, body := serveThroughGuard(t, s, req)

		require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s must be refused", site)
		require.Contains(t, body, crossSiteRefusal, "%s must name the reason", site)
		require.Equal(t, string(TypeInvalidRequest), errorType(t, body),
			"%s refusal is about the caller, not a request shape error", site)
		// The dashboard ends its session on authentication_error alone: a
		// cross-site verdict must never be mistaken for "sign the operator out".
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
			"%s refusal must not read as a session-end", site)
	}
}

// TestCrossSiteGuardIgnoresACredential is the point of the guard: it runs
// before the credential gate, so even a request carrying a valid session token
// is refused when the browser says it came from another site. A guard that
// only fired on unauthenticated requests would leave the authenticated write
// surface open to exactly the ride-along it exists to stop.
func TestCrossSiteGuardIgnoresACredential(t *testing.T) {
	t.Parallel()
	s, headers := keyedServer(t)

	req := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, body := serveThroughGuard(t, s, req)

	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a valid credential must not exempt a cross-site write")
	require.Contains(t, body, crossSiteRefusal)
}

// TestCrossSiteGuardAllowsFirstPartyAndNonBrowserCallers keeps the guard from
// breaking the two callers it must never touch: the dashboard itself
// (same-origin), a direct navigation, and non-browser clients (curl, SDKs, the
// harness) that send no fetch metadata at all. Each must pass the guard and
// reach the credential gate - observed here as the gate's 401, never the
// guard's 403.
func TestCrossSiteGuardAllowsFirstPartyAndNonBrowserCallers(t *testing.T) {
	t.Parallel()
	s, _ := keyedServer(t)

	for name, site := range map[string]string{
		"dashboard same-origin": "same-origin",
		"direct navigation":     "none",
		"non-browser client":    "", // header absent entirely
	} {
		req := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, body := serveThroughGuard(t, s, req)

		require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
			"%s must pass the guard and reach the gate", name)
		require.NotContains(t, body, crossSiteRefusal,
			"%s must not be refused by the cross-site guard", name)
	}
}

// TestCrossSiteGuardNeverBlocksSafeMethods keeps a read from being refused: a
// GET carries no state change, so the guard leaves it alone even when the
// browser labels it cross-site. Blocking reads would break embedding the
// gateway's own status in another page without protecting anything.
func TestCrossSiteGuardNeverBlocksSafeMethods(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{})

	req := stateChangingReq(http.MethodGet, "/api/ping", "")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, body := serveThroughGuard(t, s, req)

	require.Equal(t, http.StatusOK, resp.StatusCode, "a safe method must not be guarded")
	require.Contains(t, body, `"status":"ok"`)
}

// TestCrossSiteGuardRefusesForeignOrigin covers the older-browser path: no
// Sec-Fetch-Site, but an Origin header on the cross-origin request. A foreign
// Origin is refused; an Origin that names this very host is allowed through to
// the gate.
func TestCrossSiteGuardRefusesForeignOrigin(t *testing.T) {
	t.Parallel()
	s, _ := keyedServer(t)

	// httptest.NewRequest defaults Host to example.com, so an Origin naming a
	// different host is the cross-origin case.
	foreign := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
	foreign.Header.Set("Origin", "https://evil.example")
	resp, body := serveThroughGuard(t, s, foreign)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "a foreign Origin must be refused")
	require.Contains(t, body, crossSiteRefusal)

	same := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
	same.Header.Set("Origin", "http://example.com")
	resp, body = serveThroughGuard(t, s, same)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"an Origin naming this host must pass the guard")
	require.NotContains(t, body, crossSiteRefusal)
}

// TestCrossSiteGuardTreatsLoopbackNamesAsOneHost keeps the dashboard working
// however the operator typed the address: a browser at http://localhost:PORT
// and a request addressed to 127.0.0.1:PORT are the same local server, so that
// Origin must be allowed while a mismatched loopback port is still refused.
func TestCrossSiteGuardTreatsLoopbackNamesAsOneHost(t *testing.T) {
	t.Parallel()
	s, _ := keyedServer(t)

	allowed := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
	allowed.Host = "127.0.0.1:8811"
	allowed.Header.Set("Origin", "http://localhost:8811")
	resp, body := serveThroughGuard(t, s, allowed)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"loopback aliases on the same port are one host")
	require.NotContains(t, body, crossSiteRefusal)

	mismatched := stateChangingReq(http.MethodPost, "/api/keys", `{"platform":"groq","key":"sk-x"}`)
	mismatched.Host = "127.0.0.1:8811"
	mismatched.Header.Set("Origin", "http://localhost:9999")
	resp, body = serveThroughGuard(t, s, mismatched)
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a different loopback port is a different server")
	require.Contains(t, body, crossSiteRefusal)
}
