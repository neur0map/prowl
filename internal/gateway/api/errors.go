// Package api is the gateway's HTTP surface: the dashboard's JSON API, the
// OpenAI-compatible inference plane, and the single-page app itself, all on
// one origin because the vendored client assumes that.
package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"time"
)

// ErrorType is the machine-readable discriminator the client branches on.
//
// The values are not decorative. The dashboard ends its session on a 401 if
// and only if the body carries TypeAuthentication, so a relayed upstream 401 -
// testing a provider key with a bad credential, say - must use a different
// type or checking a key would sign the operator out.
type ErrorType string

const (
	TypeInvalidRequest     ErrorType = "invalid_request_error"
	TypeAuthentication     ErrorType = "authentication_error"
	TypeRateLimit          ErrorType = "rate_limit_error"
	TypeServer             ErrorType = "server_error"
	TypeProvider           ErrorType = "provider_error"
	TypeStream             ErrorType = "stream_error"
	TypeServiceUnavailable ErrorType = "service_unavailable"
	TypeUpstream           ErrorType = "upstream_error"
	TypeNotFound           ErrorType = "not_found_error"
	TypeSetupComplete      ErrorType = "setup_complete"
	TypeSetupCodeRequired  ErrorType = "setup_code_required"
	TypeInvalidPassword    ErrorType = "invalid_password"
	TypeEmailTaken         ErrorType = "email_taken"
)

// errorBody is the OpenAI-shaped envelope the client and OpenAI SDKs parse.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string    `json:"message"`
	Type    ErrorType `json:"type,omitempty"`
	Code    string    `json:"code,omitempty"`

	// RetryAtMs appears only on a rate-limit exhaustion, mirrored into
	// Retry-After so a client backs off by the same amount it is told.
	RetryAtMs int64 `json:"retryAtMs,omitempty"`
}

// WriteError emits the OpenAI-style envelope.
func WriteError(w http.ResponseWriter, status int, kind ErrorType, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: message, Type: kind}})
}

// WriteErrorCode emits the envelope with a machine-readable code, for the
// cases the client distinguishes beyond the type.
func WriteErrorCode(w http.ResponseWriter, status int, kind ErrorType, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: message, Type: kind, Code: code}})
}

// WriteRateLimited emits a 429 carrying both the structured retry moment and
// the Retry-After header, kept consistent with each other.
func WriteRateLimited(w http.ResponseWriter, message string, retryAt time.Time) {
	if !retryAt.IsZero() {
		seconds := int(math.Ceil(time.Until(retryAt).Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{
			Message:   message,
			Type:      TypeRateLimit,
			RetryAtMs: retryAt.UnixMilli(),
		}})
		return
	}
	WriteError(w, http.StatusTooManyRequests, TypeRateLimit, message)
}

// WriteBareError emits the older bare-string family that several dashboard
// routes still use. The client tolerates both shapes, but reproducing the
// family per route keeps a faithful contract.
func WriteBareError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// WriteJSON emits a success body.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	writeJSON(w, status, payload)
}

// WriteNoContent answers a successful mutation with no body, which the client
// reads as undefined rather than as an empty object.
func WriteNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshalling our own response failed, so the only honest answer is
		// a server error with a fixed body that cannot itself fail.
		slog.Error("Gateway could not marshal a response", "error", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"response encoding failed","type":"server_error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// maxManagementBody bounds a dashboard request body. The inference plane has
// its own, much larger ceiling; nothing on the management surface legitimately
// posts megabytes, and without a cap an unauthenticated route will read
// whatever a caller sends.
const maxManagementBody = 4 << 20

// DecodeJSON reads a request body into dst, answering with the validation
// envelope the client expects when it cannot.
//
// Unknown fields are tolerated rather than rejected: the vendored client is a
// moving target, and a stricter parse would break a whole screen over one
// field the server does not happen to need.
//
// The Content-Type requirement is a second line of defence against a
// cross-site write. A browser can send text/plain or a form encoding without a
// CORS preflight, but it cannot send application/json cross-origin unless this
// server approves the preflight, which it never does.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, err := mime.ParseMediaType(ct); err != nil || mediaType != "application/json" {
			WriteError(w, http.StatusUnsupportedMediaType, TypeInvalidRequest,
				"request body must be application/json")
			return false
		}
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxManagementBody)).Decode(dst); err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "invalid JSON body")
		return false
	}
	return true
}
