package api

import (
	"net/http"
	"net/url"
	"strings"
)

// guardCrossSite refuses state-changing requests that a browser tells us came
// from another site.
//
// The dashboard carries its session in the X-Dashboard-Token header, so an
// attacker's page cannot ride an existing session: setting that header
// requires a CORS preflight this server never approves. The gap is the surface
// that needs NO session - POST /api/auth/setup in particular. A cross-origin
// page can reach it with a "simple" request (no preflight) and, on an install
// that has no account yet, create one. That would hand a drive-by visitor the
// dashboard, every provider credential in it, and the inference plane.
//
// The check trusts only headers a page cannot forge. Sec-Fetch-Site is set by
// the browser itself and is unavailable to script; a non-browser client (curl,
// an SDK, the harness itself) sends neither header and is unaffected, which
// matters because the gateway is a local API as much as a web app.
func guardCrossSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// "none" is a direct navigation or a non-browser caller; "same-origin"
		// is the dashboard itself. Everything else - cross-site and same-site
		// alike - is another origin acting through the user's browser.
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "none", "same-origin":
		default:
			writeCrossSiteRefusal(w)
			return
		}

		// Older browsers send no Sec-Fetch-Site but do send Origin on
		// cross-origin requests, so it is a second, independent signal.
		if origin := r.Header.Get("Origin"); origin != "" && !originMatchesHost(origin, r.Host) {
			writeCrossSiteRefusal(w)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// writeCrossSiteRefusal answers a blocked request. It must not carry
// TypeAuthentication: the dashboard ends its session on that type alone, and a
// cross-site refusal is a statement about the caller, not about the operator's
// credentials.
func writeCrossSiteRefusal(w http.ResponseWriter) {
	WriteError(w, http.StatusForbidden, TypeInvalidRequest,
		"cross-site requests are not accepted")
}

// originMatchesHost compares an Origin header against the host the request was
// addressed to. Loopback names are treated as one host: a browser at
// http://localhost:8811 and a request to 127.0.0.1:8811 are the same server,
// and refusing that pairing would break the dashboard depending on which name
// the operator typed.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, host) {
		return true
	}
	oh, op := splitHostPort(u.Host)
	rh, rp := splitHostPort(host)
	return op == rp && isLoopbackName(oh) && isLoopbackName(rh)
}

func splitHostPort(hostport string) (host, port string) {
	if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.HasSuffix(hostport[i:], "]") {
		return strings.Trim(hostport[:i], "[]"), hostport[i+1:]
	}
	return strings.Trim(hostport, "[]"), ""
}

func isLoopbackName(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}
