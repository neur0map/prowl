package gateway

// Upstream-error classification for the failover loop, ported from FreeLLMAPI's
// server/src/lib/error-classify.ts (github.com/tashfeenahmed/freellmapi, MIT,
// v0.9.9 - see NOTICE.md). Pure functions over an error's HTTP status, message,
// and provider code - no I/O - so the loop, the trail, and the exhaustion
// taxonomy all read the same verdicts and can never drift.
//
// These deliberately do NOT reuse router.go's coarse FailureKind/ClassifyStatus
// (auth/rate_limit/quota/server/network/request): that maps a bare status to a
// cooldown for the pre-port loop, whereas failover needs message-marker rules,
// skip scope, penalty weight, and limit-learning per attempt. The mapping from
// the rich ErrorClass here to the old FailureKind values is documented on
// ClassifyError, for the integration pass that retires the old classifier.

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// UpstreamError is the typed provider failure the dispatcher throws. It carries
// exactly the fields the classifiers read - mirroring what FreeLLMAPI's
// providerHttpError attaches (err.status/message/code) plus the turn-integrity
// markers the stream layer sets on synthetic errors (skipBench,
// skipModelForRequest) and the two gateway-owned aborts (client hang-up, the
// wall-clock hedge). A dispatcher MAY instead throw any plain error; then
// Status/Code are 0/"" and only the message-marker rules apply.
type UpstreamError struct {
	// Status is the HTTP status the provider returned, 0 when the failure
	// carried none (a timeout, a transport fault, a synthetic dead-turn error).
	Status int

	// Code is the provider's machine error code (e.g. context_length_exceeded).
	Code string

	// Message is the human-readable failure text the message rules match on.
	Message string

	// RetryAfter is the provider's own backoff instruction; 0 when it said
	// nothing. It is a cooldown floor, never shortened by our ceiling.
	RetryAfter time.Duration

	// SkipBench marks a dead turn that is REQUEST behaviour, not provider
	// health: a reasoning model that spent its whole max_tokens budget on
	// hidden reasoning (finish_reason 'length'). It still fails over, but
	// records no cooldown/penalty/limit-learning while the streak exemption
	// holds. See EMPTY_COMPLETION_STREAK_LIMIT.
	SkipBench bool

	// SkipModelForRequest marks a failure the thrower scoped to the MODEL for
	// this request (ignored response_format, JSON truncated at max_tokens): a
	// sibling key would reproduce it, so rule out the whole model rather than
	// burning one hop per key.
	SkipModelForRequest bool

	// Undispatchable marks a candidate the gateway has no wire adapter for.
	// Nothing was asked of the provider, so it earns no cooldown and no
	// penalty - only exclusion for the rest of this request.
	Undispatchable bool

	// ClientAbort is set when the gateway's OWN client hung up mid-attempt.
	// HedgeAbort is set when the wall-clock retry budget expired in-flight.
	// Neither is provider health: the loop stops without any bookkeeping.
	ClientAbort bool
	HedgeAbort  bool

	// Cause is the underlying error (e.g. a buried transport fault undici did
	// not surface in the top-level message). Walked by IsTransportError.
	Cause error
}

func (e *UpstreamError) Error() string { return e.Message }
func (e *UpstreamError) Unwrap() error { return e.Cause }

// errStatus/errCode read the TOP-LEVEL status/code exactly as the reference
// reads err?.status / err?.code: a provider failure wrapped in a transport
// wrapper has no top-level status, so it correctly falls through to the message
// rules. errMsg is err.Error() (the reference's err?.message).
func errStatus(err error) int {
	if u, ok := err.(*UpstreamError); ok && u != nil {
		return u.Status
	}
	return 0
}

func errCode(err error) string {
	if u, ok := err.(*UpstreamError); ok && u != nil {
		return u.Code
	}
	return ""
}

// errMsg is the reference's err?.message. A typed-nil *UpstreamError carried in
// a non-nil error interface (the loop's lastError before any failure) must read
// as empty, not panic in Error().
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	if u, ok := err.(*UpstreamError); ok && u == nil {
		return ""
	}
	return err.Error()
}

func errRetryAfter(err error) time.Duration {
	if u, ok := err.(*UpstreamError); ok && u != nil {
		return u.RetryAfter
	}
	return 0
}

// ── Transport failures hidden in the cause chain ─────────────────────────────
// Every message rule reads the top-level text, but a socket that dies mid-
// request surfaces as a generic wrapper whose Cause carries the real fault.
// Everything matched here is a transient network fault that says nothing about
// the request or the model, so the next candidate can serve it. The walk is
// depth-bounded so an adversarial cause chain cannot hang this hot path.

const transportCauseMaxDepth = 5 // error-classify.ts:130

var transportErrorCodes = map[string]struct{}{ // error-classify.ts:132-139
	"ECONNRESET": {}, "ECONNREFUSED": {}, "ECONNABORTED": {}, "EPIPE": {},
	"ETIMEDOUT": {}, "EAI_AGAIN": {}, "ENOTFOUND": {}, "EHOSTUNREACH": {},
	"ENETUNREACH": {}, "EADDRNOTAVAIL": {},
}

// undici stamps its own transport failures with a UND_ERR_* code; matched by
// prefix so the list grows with releases (error-classify.ts:145).
const undiciErrorCodePrefix = "UND_ERR_"

var transportMessageHints = []string{ // error-classify.ts:147-159
	"socket hang up",
	"premature close",
	"other side closed",
	"client network socket disconnected",
	"econnreset", "econnrefused", "epipe", "etimedout", "eai_again",
}

// coder is any error that exposes a machine code (e.g. a wrapped provider
// error), so a transport code carried out-of-band is still matched.
type coder interface{ Code() string }

type chainLink struct {
	code    string
	message string
}

// errorChainLinks walks err's Unwrap chain, yielding each link's code and
// message. Includes err itself as the first link, bounded to
// transportCauseMaxDepth hops (error-classify.ts:164-179).
func errorChainLinks(err error) []chainLink {
	links := make([]chainLink, 0, 2)
	cur := err
	for depth := 0; cur != nil && depth <= transportCauseMaxDepth; depth++ {
		code := ""
		if u, ok := cur.(*UpstreamError); ok && u != nil {
			code = u.Code
		} else if c, ok := cur.(coder); ok {
			code = c.Code()
		}
		links = append(links, chainLink{code: code, message: cur.Error()})
		cur = errors.Unwrap(cur)
	}
	return links
}

// IsTransportError reports whether err - or anything in its bounded cause chain
// - is a transient network transport failure (error-classify.ts:184-207). The
// two gateway-owned aborts are authoritative and win over any transport
// evidence: neither is provider health.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}
	if IsClientAbortError(err) || IsHedgeAbortError(err) {
		return false
	}
	for _, l := range errorChainLinks(err) {
		if l.code != "" {
			if _, ok := transportErrorCodes[l.code]; ok || strings.HasPrefix(l.code, undiciErrorCodePrefix) {
				return true
			}
		}
		m := strings.ToLower(l.message)
		if m == "" {
			continue
		}
		for _, hint := range transportMessageHints {
			if strings.Contains(m, hint) {
				return true
			}
		}
		// "terminated" is matched as a WHOLE message only: a terminated ACCOUNT
		// is a fatal billing/auth condition, not a socket death.
		if strings.TrimSpace(m) == "terminated" {
			return true
		}
	}
	return false
}

// IsRateLimitSignal reports a genuine provider QUOTA signal - a structured 429
// or rate-limit/quota wording (error-classify.ts:221-227). Distinct from the
// far broader IsRetryableError: timeouts and 5xx are retryable but say nothing
// about quotas, so only a real signal may feed the null-limit cooldown ladder.
func IsRateLimitSignal(err error) bool {
	if s := errStatus(err); s != 0 {
		return s == 429
	}
	m := strings.ToLower(errMsg(err))
	return strings.Contains(m, "429") || strings.Contains(m, "rate limit") ||
		strings.Contains(m, "too many requests") || strings.Contains(m, "quota") ||
		strings.Contains(m, "resource_exhausted")
}

// timeoutErrorMarkers are the message markers that mean "this attempt ran out
// of time", in every wording the stack produces (error-classify.ts:244).
var timeoutErrorMarkers = []string{"timeout", "stalled", "etimedout", "aborted"}

// IsTimeoutErrorText reports whether raw message text reads as a timeout
// (error-classify.ts:248-251).
func IsTimeoutErrorText(message string) bool {
	m := strings.ToLower(message)
	for _, marker := range timeoutErrorMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// IsClientAbortError reports whether err is (or wraps) the gateway's own
// client-disconnect abort (error-classify.ts:272-277).
func IsClientAbortError(err error) bool {
	if u, ok := err.(*UpstreamError); ok && u != nil && u.ClientAbort {
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		if u, ok := inner.(*UpstreamError); ok && u != nil && u.ClientAbort {
			return true
		}
	}
	return strings.Contains(strings.ToLower(errMsg(err)), "client disconnected")
}

// IsHedgeAbortError reports whether err is (or wraps) the wall-clock hedge abort
// (error-classify.ts:294-299).
func IsHedgeAbortError(err error) bool {
	if u, ok := err.(*UpstreamError); ok && u != nil && u.HedgeAbort {
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		if u, ok := inner.(*UpstreamError); ok && u != nil && u.HedgeAbort {
			return true
		}
	}
	return strings.Contains(strings.ToLower(errMsg(err)), "fallback time budget expired")
}

// IsKeyAuthError reports a 401 / invalid-API-key failure: KEY-fatal, not
// request-fatal, so the loop rotates past the bad key and revalidates it
// (error-classify.ts:331-347). A 400 is key-auth only for Google's key-specific
// wordings; generic auth wording on a 400 stays a payload rejection.
func IsKeyAuthError(err error) bool {
	s := errStatus(err)
	if s == 401 {
		return true
	}
	m := strings.ToLower(errMsg(err))
	keySpecific := strings.Contains(m, "api key not valid") ||
		strings.Contains(m, "api key expired") ||
		strings.Contains(m, "api_key_invalid")
	if s == 400 {
		return keySpecific
	}
	if s != 0 {
		return false
	}
	return keySpecific ||
		strings.Contains(m, "401") ||
		strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "invalid api key") ||
		strings.Contains(m, "invalid_api_key") ||
		strings.Contains(m, "incorrect api key") ||
		strings.Contains(m, "authentication failed")
}

var dailyMarkerRe = regexp.MustCompile(`daily|per[ -_]?day|\btoday\b`)
var dailyQuotaRe = regexp.MustCompile(`allocation|quota|limit|exhaust|used up`)

// IsDailyQuotaExhaustedError reports a 429 whose body says the DAILY free
// allocation is spent (error-classify.ts:355-359). Requires both a daily marker
// and a quota/allocation marker so an ordinary per-minute 429 never matches.
func IsDailyQuotaExhaustedError(err error) bool {
	m := strings.ToLower(errMsg(err))
	if !dailyMarkerRe.MatchString(m) {
		return false
	}
	return dailyQuotaRe.MatchString(m)
}

// IsProviderDegradedError reports a provider-side "hosted model temporarily
// degraded" condition dressed up as a 400 (error-classify.ts:367-370). The
// request is fine; the deployment is sick - never a bad request.
func IsProviderDegradedError(err error) bool {
	return strings.Contains(strings.ToLower(errMsg(err)), "degraded")
}

// IsProviderLevelError reports a failure of the PROVIDER, not this key - a 5xx,
// a timeout, a transport fault, or a degraded deployment (error-classify.ts:
// 387-406). The loop skips the whole platform for the request. Key-scoped
// failures (auth, quota, tier) stay out so a dead key can rotate to a sibling.
func IsProviderLevelError(err error) bool {
	if u, ok := err.(*UpstreamError); ok && u != nil && u.SkipModelForRequest {
		return false
	}
	s := errStatus(err)
	if s >= 500 {
		return true
	}
	if IsProviderDegradedError(err) {
		return true
	}
	if s != 0 {
		return false
	}
	if IsEdgeUnreachableError(err) {
		return true
	}
	// A cut connection is NOT provider-level: the edge answered, so the
	// failure belongs to this model and key.
	if IsTransportCutError(err) {
		return false
	}
	m := strings.ToLower(errMsg(err))
	return IsTimeoutErrorText(m) ||
		strings.Contains(m, "econnrefused") ||
		strings.Contains(m, "econnreset") ||
		strings.Contains(m, "fetch failed")
}

// IsProviderBadRequestError reports a provider-side 400/422 the client should
// see as invalid_request rather than a misleading rate-limit exhaustion when
// every provider rejects it (error-classify.ts:430-438). A degraded 400 is
// provider health, not request shape.
func IsProviderBadRequestError(err error) bool {
	if IsProviderDegradedError(err) {
		return false
	}
	s := errStatus(err)
	m := strings.ToLower(errMsg(err))
	if s == 400 {
		return strings.Contains(m, "api error 400")
	}
	if s == 422 {
		return strings.Contains(m, "api error 422") || strings.Contains(m, "unprocessable entity")
	}
	if s != 0 {
		return false
	}
	return strings.Contains(m, "api error 400") ||
		strings.Contains(m, "api error 422") ||
		strings.Contains(m, "unprocessable entity")
}

var exceedsContextRe = regexp.MustCompile(`exceeds[^.]*context (length|window|size)`)
var inputTokenExceedsRe = regexp.MustCompile(`input token count[^.]*exceeds`)

// IsContextTooLargeError reports a "request/prompt too large" rejection in all
// its provider wordings and disagreeing statuses (error-classify.ts:461-483).
// Retryable (a sibling may have a larger window); when EVERY attempt dies this
// way the exhaustion ladder renders an honest 413.
func IsContextTooLargeError(err error) bool {
	if errStatus(err) == 413 {
		return true
	}
	if strings.ToLower(errCode(err)) == "context_length_exceeded" {
		return true
	}
	m := strings.ToLower(errMsg(err))
	return strings.Contains(m, "context_length_exceeded") ||
		strings.Contains(m, "maximum context length") ||
		strings.Contains(m, "context length exceeded") ||
		exceedsContextRe.MatchString(m) ||
		strings.Contains(m, "prompt is too long") ||
		strings.Contains(m, "input is too long") ||
		inputTokenExceedsRe.MatchString(m) ||
		strings.Contains(m, "exceeds the maximum number of tokens") ||
		strings.Contains(m, "request too large") ||
		strings.Contains(m, "payload too large") ||
		strings.Contains(m, "request entity too large") ||
		strings.Contains(m, "request body too large") ||
		strings.Contains(m, "content too large") ||
		strings.Contains(m, "exceeds max length") ||
		strings.Contains(m, "api error 413")
}

// IsPaymentRequiredError reports a 402 / out-of-credits failure (error-classify.
// ts:488-493): it won't recover on the next window, so the caller benches the
// key for a full day rather than the 90s transient cooldown. A 402 status is
// authoritative on its own - providers that return it with an empty or opaque
// body still mean "no credit", so the status short-circuits the body markers.
func IsPaymentRequiredError(err error) bool {
	if errStatus(err) == 402 {
		return true
	}
	m := strings.ToLower(errMsg(err))
	return strings.Contains(m, "402") ||
		strings.Contains(m, "payment required") ||
		strings.Contains(m, "insufficient_quota") ||
		strings.Contains(m, "insufficient credit") ||
		strings.Contains(m, "insufficient balance")
}

// IsModelNotFoundError reports a 404/410 "model removed/deprecated upstream"
// failure (error-classify.ts:500-509). MODEL-level: every key 404s the same
// way, so skip the whole model for the request.
func IsModelNotFoundError(err error) bool {
	if s := errStatus(err); s == 404 || s == 410 {
		return true
	}
	m := strings.ToLower(errMsg(err))
	return strings.Contains(m, "404") ||
		strings.Contains(m, "not found") ||
		strings.Contains(m, "no endpoints found") ||
		strings.Contains(m, "410") ||
		strings.Contains(m, "gone")
}

var modelAccessDeniedPhrases = []string{ // error-classify.ts:536-552
	"not allowed to access",
	"not allowed to use",
	"not authorized to access",
	"not authorized to use",
	"unauthorized to access",
	"not permitted to access",
	"not permitted to use",
	"do not have access to",
	"does not have access to",
	"no access to model",
	"no access to this model",
	"model access denied",
	"access to this model is restricted",
	"action plan limited",
}

// IsModelAccessForbiddenError reports a 403 (or a 400/401 access-denial wording)
// meaning this model is off the key's tier (error-classify.ts:517-534). Drives
// the same whole-model skip as a 404; distinct from a dead key.
func IsModelAccessForbiddenError(err error) bool {
	if errStatus(err) == 403 {
		return true
	}
	m := strings.ToLower(errMsg(err))
	if strings.Contains(m, "403") || strings.Contains(m, "forbidden") {
		return true
	}
	s := errStatus(err)
	if s != 0 && s != 400 && s != 401 {
		return false
	}
	for _, phrase := range modelAccessDeniedPhrases {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// isModelAccessDenied reports whether the message carries explicit
// model-access-denial wording - this key's tier cannot use this model, as
// opposed to a rejected credential. On a 400/401 it must beat the generic
// key-auth verdict: the credential is valid, the model is simply off the
// key's tier, so the loop skips the whole model instead of rotating the key.
// True credential failures carry none of these phrases and stay key-scoped.
func isModelAccessDenied(err error) bool {
	m := strings.ToLower(errMsg(err))
	for _, phrase := range modelAccessDeniedPhrases {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// Go's transport vocabulary.
//
// The reference is Node, and its error text ("ECONNRESET", "ECONNREFUSED",
// "fetch failed") never appears in a Go program. Ported literally, every
// transport failure fell through as NON-retryable and the failover loop
// returned the provider's error after a single attempt: a live agent run died
// on "unexpected EOF" with hundreds of candidates untried. The Node spellings
// are kept because a relayed upstream body can still contain them.
var (
	// transportCutMarkers are failures AFTER the provider accepted the
	// request. The request reached the edge, so only this model and key are
	// implicated -- sweeping the platform would bench an aggregator's whole
	// catalogue because one sub-provider was overloaded.
	transportCutMarkers = []string{
		"unexpected eof",
		"connection reset by peer",
		"broken pipe",
		"http2: server sent goaway",
		"stream error",
		"response body closed",
		"unexpected end of json input",
	}

	// edgeUnreachableMarkers are failures BEFORE the request was accepted:
	// the platform's endpoint could not be reached at all, which does
	// implicate every key pointing at it.
	edgeUnreachableMarkers = []string{
		"connection refused",
		"no such host",
		"network is unreachable",
		"no route to host",
		"tls:",
		"certificate",
		"server misbehaving",
		"dial tcp",
	}
)

// IsTransportCutError reports whether err is a connection that died after the
// provider took the request.
func IsTransportCutError(err error) bool {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	m := strings.ToLower(errMsg(err))
	for _, marker := range transportCutMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	// A bare "EOF" only counts as a cut when nothing else explains it; the
	// substring appears inside unrelated provider text too.
	return m == "eof" || strings.HasSuffix(m, ": eof")
}

// IsEdgeUnreachableError reports whether err means the platform's endpoint
// could not be reached.
func IsEdgeUnreachableError(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	m := strings.ToLower(errMsg(err))
	for _, marker := range edgeUnreachableMarkers {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

// IsRetryableError reports whether a thrown error should fail over to the next
// candidate rather than 502 the request (error-classify.ts:6-111). Status first
// (408/409/410/422/429/5xx), then the message markers, then a buried transport
// fault. A 400 or 401 with no matching rule is FATAL - it has no status branch
// here and matches no marker.
func IsRetryableError(err error) bool {
	m := strings.ToLower(errMsg(err))
	if s := errStatus(err); s == 408 || s == 409 || s == 410 || s == 422 || s == 429 || s >= 500 {
		return true
	}
	if IsTransportCutError(err) || IsEdgeUnreachableError(err) {
		return true
	}
	return strings.Contains(m, "429") ||
		strings.Contains(m, "rate limit") ||
		strings.Contains(m, "too many requests") ||
		strings.Contains(m, "quota") ||
		strings.Contains(m, "resource_exhausted") ||
		strings.Contains(m, "aborted") ||
		strings.Contains(m, "timeout") ||
		strings.Contains(m, "etimedout") ||
		strings.Contains(m, "econnrefused") ||
		strings.Contains(m, "econnreset") ||
		strings.Contains(m, "fetch failed") ||
		strings.Contains(m, "503") ||
		strings.Contains(m, "unavailable") ||
		strings.Contains(m, "degraded") ||
		strings.Contains(m, "500") ||
		strings.Contains(m, "internal server error") ||
		strings.Contains(m, "413") ||
		strings.Contains(m, "payload too large") ||
		strings.Contains(m, "request body too large") ||
		strings.Contains(m, "request entity too large") ||
		strings.Contains(m, "content too large") ||
		IsContextTooLargeError(err) ||
		strings.Contains(m, "404") ||
		strings.Contains(m, "not found") ||
		strings.Contains(m, "no endpoints found") ||
		strings.Contains(m, "410") ||
		strings.Contains(m, "gone") ||
		IsModelAccessForbiddenError(err) ||
		strings.Contains(m, "api error 400") ||
		strings.Contains(m, "api error 422") ||
		strings.Contains(m, "unprocessable entity") ||
		IsPaymentRequiredError(err) ||
		strings.Contains(m, "empty completion") ||
		strings.Contains(m, "ignored response_format") ||
		strings.Contains(m, "truncated json") ||
		strings.Contains(m, "in-band provider error") ||
		strings.Contains(m, "stream ended unexpectedly") ||
		strings.Contains(m, "stream stalled") ||
		strings.Contains(m, "no first byte") ||
		strings.Contains(m, "unparseable inline tool-call dialect") ||
		strings.Contains(m, "invalid tool arguments") ||
		IsTransportError(err)
}

// ── Attempt-trail class ──────────────────────────────────────────────────────

// AttemptClass is the per-hop label the attempt trail renders. Ported verbatim
// from AttemptErrorClass (fallback-loop.ts:419-433).
type AttemptClass string

const (
	AttemptAuth                 AttemptClass = "auth"
	AttemptOutOfCredits         AttemptClass = "out_of_credits"
	AttemptDailyQuotaExhausted  AttemptClass = "daily_quota_exhausted"
	AttemptModelNotFound        AttemptClass = "model_not_found"
	AttemptForbidden            AttemptClass = "forbidden"
	AttemptContextTooLarge      AttemptClass = "context_too_large"
	AttemptProviderBadRequest   AttemptClass = "provider_bad_request"
	AttemptEmptyCompletion      AttemptClass = "empty_completion"
	AttemptFormatIgnored        AttemptClass = "format_ignored"
	AttemptInvalidToolArguments AttemptClass = "invalid_tool_arguments"
	AttemptTimeout              AttemptClass = "timeout"
	AttemptRateLimited          AttemptClass = "rate_limited"
	AttemptUpstreamError        AttemptClass = "upstream_error"
	AttemptGenericError         AttemptClass = "error"
)

// ClassifyAttempt labels an error for the attempt trail, most-specific first
// (fallback-loop.ts:442-467). Context-too-large is checked before the generic
// bad-request rule (OpenAI-compat context errors arrive as "API error 400");
// a degraded 400 is provider health, kept out of provider_bad_request.
func ClassifyAttempt(err error) AttemptClass {
	switch {
	case IsKeyAuthError(err) && !isModelAccessDenied(err):
		return AttemptAuth
	case IsPaymentRequiredError(err):
		return AttemptOutOfCredits
	case IsDailyQuotaExhaustedError(err):
		return AttemptDailyQuotaExhausted
	case IsModelNotFoundError(err):
		return AttemptModelNotFound
	case IsModelAccessForbiddenError(err):
		return AttemptForbidden
	case IsContextTooLargeError(err):
		return AttemptContextTooLarge
	case IsProviderDegradedError(err):
		return AttemptUpstreamError
	case IsProviderBadRequestError(err):
		return AttemptProviderBadRequest
	}
	m := strings.ToLower(errMsg(err))
	switch {
	case strings.Contains(m, "empty completion"):
		return AttemptEmptyCompletion
	case strings.Contains(m, "ignored response_format") || strings.Contains(m, "truncated json"):
		return AttemptFormatIgnored
	case strings.Contains(m, "invalid tool arguments"):
		return AttemptInvalidToolArguments
	case IsTimeoutErrorText(m):
		return AttemptTimeout
	case strings.Contains(m, "429") || strings.Contains(m, "rate limit") ||
		strings.Contains(m, "too many requests") || strings.Contains(m, "quota"):
		return AttemptRateLimited
	}
	if s := errStatus(err); s >= 500 || strings.Contains(m, "500") || strings.Contains(m, "502") ||
		strings.Contains(m, "503") || strings.Contains(m, "unavailable") ||
		strings.Contains(m, "internal server error") {
		return AttemptUpstreamError
	}
	return AttemptGenericError
}

// ── Rich classification for the loop ─────────────────────────────────────────

// SkipScope is the widest request-scoped exclusion a failure earns: the key
// alone, the whole model, or the whole platform.
type SkipScope string

const (
	SkipScopeKey      SkipScope = "key"
	SkipScopeModel    SkipScope = "model"
	SkipScopePlatform SkipScope = "platform"
)

// cooldownDisposition is how the loop prices the bench after a retryable
// failure - the ported branches of cooldownDecisionForError (fallback-loop.ts:
// 205-222).
type cooldownDisposition int

const (
	// cooldownNone is the streak-exempt path: no bench recorded.
	cooldownNone cooldownDisposition = iota
	// cooldownTransient defers to the escalation ladder (CooldownEngine.Decide),
	// honouring a provider Retry-After as a floor.
	cooldownTransient
	// cooldownPayment benches a full day for an out-of-credits key ('credit').
	cooldownPayment
	// cooldownForbidden benches a full day for a tier-gated model ('tier').
	cooldownForbidden
	// cooldownDaily benches to the provider's own daily reset ('authoritative').
	cooldownDaily
)

// penaltyWeight is the model-level scorer demotion a failure earns when the
// model is at fault (no usable sibling key), expressed in "priority positions"
// so it demotes within the ordering rather than scaling the score. Heavy on
// ANY rate-limit signal, light on every other retryable failure (fallback-loop.
// ts:343-346). The magnitudes and decay live in Slice B's penalty state, which
// the loop reaches only through the Scorer interface: heavy = recordRateLimitHit
// = PENALTY_PER_429 = 3 (router.ts:267,299); light = recordModelFailure =
// PENALTY_PER_FAIL = 1 (router.ts:268); both decay 1 per 2 min, floor 0, applied
// lazily on the next hit (router.ts:270-271,284-285).
type penaltyWeight int

const (
	penaltyNone penaltyWeight = iota
	penaltyLight
	penaltyHeavy
)

// ErrorClass is the failover loop's complete verdict for one upstream error:
// retry vs fatal, skip scope, cooldown pricing, penalty weight, and whether to
// learn a tighter limit. It is the single source the loop reads so the
// skip/cooldown/penalty decisions can never drift from the classifier.
//
// Mapping to router.go's pre-port FailureKind (for the integration pass that
// retires ClassifyStatus):
//
//	KeyAuth                          -> FailureAuth
//	Cooldown == cooldownForbidden    -> FailureAuth   (403 tier)
//	Cooldown == cooldownPayment      -> FailureQuota  (402)
//	Cooldown == cooldownDaily        -> FailureQuota  (daily exhausted)
//	QuotaSignal (transient 429/tpm)  -> FailureRateLimit
//	SkipPlatform (5xx/timeout/xport) -> FailureServer / FailureNetwork
//	!Retryable                       -> FailureRequest (fatal 400/401)
type ErrorClass struct {
	// Retryable is true when the loop should fail over rather than onFatal.
	Retryable bool
	// KeyAuth is true for a 401/invalid-key failure: rotate the key and
	// revalidate it; no penalty and no limit-learning.
	KeyAuth bool
	// Attempt is the trail label.
	Attempt AttemptClass

	// Scope is the widest request-scoped exclusion. The key is ALWAYS skipped
	// on a retryable failure; Scope reports model/platform when wider.
	Scope        SkipScope
	SkipModel    bool
	SkipPlatform bool

	// SkipBench marks a turn-integrity exemption candidate (empty completion /
	// format violation). SkipModelForRequest marks model behaviour for this
	// request. The loop combines these with the empty-completion streak to
	// decide whether the bench/penalty/learn actually apply.
	SkipBench           bool
	SkipModelForRequest bool

	// Cooldown is how to price the bench; QuotaSignal is a real 429 feeding the
	// null-limit ladder and the heavy penalty; Penalty is the scorer demotion
	// when the model is at fault; LearnLimit permits tightening a stored ceiling.
	Cooldown    cooldownDisposition
	QuotaSignal bool
	Penalty     penaltyWeight
	LearnLimit  bool
}

// normalizeError folds the loop's three failure signals into the single
// *UpstreamError the Is* predicates read. status 0 means "no response" (a
// transport failure or dead turn); body is the upstream response text on an
// HTTP failure; err carries transport/timeout/abort detail and the
// turn-integrity markers. The message rules match on the body when there is
// one, else on err's text.
func normalizeError(status int, body string, err error) *UpstreamError {
	ue := &UpstreamError{
		Status:  status,
		Message: firstNonEmpty(body, errMsg(err)),
		Cause:   err,
	}
	if u, ok := err.(*UpstreamError); ok && u != nil {
		ue.Undispatchable = u.Undispatchable
		ue.Code = u.Code
		ue.RetryAfter = u.RetryAfter
		ue.SkipBench = u.SkipBench
		ue.SkipModelForRequest = u.SkipModelForRequest
		ue.ClientAbort = u.ClientAbort
		ue.HedgeAbort = u.HedgeAbort
		if ue.Status == 0 {
			ue.Status = u.Status
		}
	}
	return ue
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ClassifyError computes the full ErrorClass for one failed attempt from its
// upstream status, response body, and any transport/dead-turn error. This is
// the loop's entry point; a rate limit is the common case (status 429 + JSON
// body, no Go error), while a timeout or transport fault arrives as status 0
// with err set. Skip scope, cooldown, penalty, and limit-learning follow
// §5.3's table; the loop then applies the empty-completion streak to lift or
// hold the SkipBench exemption.
func ClassifyError(status int, body string, err error) ErrorClass {
	return classifyNormalized(normalizeError(status, body, err))
}

func classifyNormalized(err *UpstreamError) ErrorClass {
	c := ErrorClass{
		Attempt: ClassifyAttempt(err),
		Scope:   SkipScopeKey,
	}

	// A candidate the gateway cannot dispatch at all - no wire adapter for its
	// platform - is our own configuration gap, not a verdict on the request or
	// on the provider's health. It must skip the whole platform and let the
	// loop advance: treated as fatal it halted a run that still had healthy
	// candidates behind it, which is exactly what failover exists to prevent.
	if err != nil && err.Undispatchable {
		c.Retryable = true
		c.SkipPlatform = true
		c.Scope = SkipScopePlatform
		c.Cooldown = cooldownNone
		c.Penalty = penaltyNone
		return c
	}

	// A 400/401 whose body says this MODEL is off the key's tier is a
	// whole-model skip, not a dead key: the credential is valid, so it must
	// beat the generic key-auth branch a bare 401 status would otherwise win.
	// The retryable path below then rules out the model and prices the tier
	// cooldown. True credential failures carry no such wording and stay here.
	if IsKeyAuthError(err) && !isModelAccessDenied(err) {
		// 401: key-fatal, its own branch in the loop. Not retryable on its own,
		// no penalty, no limit-learning.
		c.KeyAuth = true
		return c
	}

	c.Retryable = IsRetryableError(err)
	if !c.Retryable {
		return c
	}

	c.SkipBench = err.SkipBench
	c.SkipModelForRequest = err.SkipModelForRequest

	// Skip scope: the key is always skipped; a model-level failure rules out the
	// whole model; a provider-level failure rules out the whole platform.
	if IsModelNotFoundError(err) || IsModelAccessForbiddenError(err) ||
		IsContextTooLargeError(err) || c.SkipModelForRequest {
		c.SkipModel = true
		c.Scope = SkipScopeModel
	}
	if IsProviderLevelError(err) {
		c.SkipPlatform = true
		c.Scope = SkipScopePlatform
	}

	// Cooldown pricing, most-specific first (payment, then tier, then daily,
	// else the transient ladder).
	c.QuotaSignal = IsRateLimitSignal(err)
	switch {
	case IsPaymentRequiredError(err):
		c.Cooldown = cooldownPayment
	case IsModelAccessForbiddenError(err):
		c.Cooldown = cooldownForbidden
	case IsDailyQuotaExhaustedError(err):
		c.Cooldown = cooldownDaily
	default:
		c.Cooldown = cooldownTransient
	}

	// Penalty weight when the model is at fault. Ported from the CODE, not the
	// spec table: recordRetryableFailure demotes heavy on ANY rate-limit signal
	// (recordRateLimitHit), light otherwise (recordModelFailure). So both a
	// daily-exhausted 429 and a transient rpm/tpm 429 earn the heavy demotion -
	// the spec's "transient -> light" row is wrong (fallback-loop.ts:339-347).
	if c.QuotaSignal {
		c.Penalty = penaltyHeavy
	} else {
		c.Penalty = penaltyLight
	}

	// A non-exempt retryable failure always attempts limit-learning; the learner
	// only writes when it can parse a tighter ceiling from the body.
	c.LearnLimit = true
	return c
}

// AsUpstreamError recovers the provider verdict carried by a Result.Err. The
// fatal path returns the error as-is rather than wrapping it in an Exhaustion,
// so a caller that wants to render what the provider actually said has to
// reach for it.
func AsUpstreamError(err error) *UpstreamError {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue
	}
	return nil
}

// ClassifyKind labels an upstream failure for the request record, so a fatal
// hop is filed under what went wrong instead of a bare "upstream".
func ClassifyKind(err *UpstreamError) ExhaustionKind {
	if err == nil {
		return ExhaustUpstream
	}
	switch cls := classifyNormalized(err); {
	case cls.KeyAuth:
		return ExhaustAuth
	case err.Status == 429:
		return ExhaustRateLimit
	case err.Status == 413, cls.Attempt == AttemptContextTooLarge:
		return ExhaustContextTooLarge
	case cls.Attempt == AttemptModelNotFound:
		return ExhaustModelNotFound
	case err.Status == 503:
		return ExhaustUnavailable
	default:
		return ExhaustUpstream
	}
}

// ProviderMessage pulls the human sentence out of an upstream failure.
//
// UpstreamError.Message deliberately holds the provider's raw response body,
// because the classification rules match markers in it. Rendering that body
// verbatim nests JSON inside our own error envelope, so a client shows a
// quoted blob instead of a sentence. Every provider we route to shapes errors
// as {"error":{"message":…}} (OpenAI) or {"message":…} (a few gateways), so
// unwrap those and fall back to the raw text.
func ProviderMessage(err *UpstreamError) string {
	if err == nil {
		return ""
	}
	raw := strings.TrimSpace(err.Message)
	if !strings.HasPrefix(raw, "{") {
		return raw
	}

	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal([]byte(raw), &body) != nil {
		return raw
	}
	for _, candidate := range []string{body.Error.Message, body.Message, body.Detail} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return raw
}

// UndispatchableError reports a candidate the gateway cannot reach because no
// adapter is registered for its platform. The loop skips the platform and
// carries on; the message names the platform so the gap is diagnosable from
// the request record rather than from a silent 502.
func UndispatchableError(platform string) *UpstreamError {
	return &UpstreamError{
		Undispatchable: true,
		Code:           "no_provider_adapter",
		Message:        "no provider adapter for " + platform,
	}
}
