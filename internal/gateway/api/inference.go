package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// The inference plane: the OpenAI-compatible surface applications point at.
//
// This is where the ported engine meets a real request. The failover loop owns
// the attempt sequence, the chain owns candidate order, the ledger owns
// admission, and this file owns exactly one job - turning one chosen route
// into one upstream call and relaying the answer. Keeping that boundary sharp
// is what makes streaming safe: the loop never sees a half-flushed response,
// and this code never decides what to try next.

// maxInferenceBody bounds a request body. The reference allows 25 MiB on the
// inference plane because a vision request carries inline images.
const maxInferenceBody = 25 << 20

func (s *Server) registerInferenceRoutes() {
	s.mux.HandleFunc("POST /v1/chat/completions", s.RequireMachineKey(s.handleChatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.RequireMachineKey(s.handleListModels))
	s.registerCompatRoutes()
}

// chatRequestBody is the parsed inbound request. Everything beyond the fields
// the router needs is kept verbatim in rest, so a provider receives whatever
// the client sent rather than a lossy re-serialisation.
type chatRequestBody struct {
	Model            string
	Messages         []map[string]any
	Stream           bool
	Params           map[string]any
	raw              map[string]any
	excludePlatforms map[string]bool
	research         *researchHandoff
	// sessionID is the client's x-session-id, threaded to the sticky-session
	// key so a pinned conversation stays on its model across every inference
	// dialect. Empty falls back to the first-user-message hash.
	sessionID string
}

func parseChatBody(body []byte) (*chatRequestBody, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	out := &chatRequestBody{raw: raw, Params: map[string]any{}}
	for key, value := range raw {
		switch key {
		case "model":
			out.Model, _ = value.(string)
		case "stream":
			out.Stream, _ = value.(bool)
		case "messages":
			list, _ := value.([]any)
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					out.Messages = append(out.Messages, m)
				}
			}
		default:
			out.Params[key] = value
		}
	}
	if out.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	return out, nil
}

// prependSystem puts an enforced system prompt ahead of everything the caller
// sent. Only Messages needs changing: the dispatcher relays this slice rather
// than the raw JSON, so the prompt reaches the provider without the request
// having to be re-serialised.
func (b *chatRequestBody) prependSystem(prompt string) {
	if prompt == "" {
		return
	}
	b.Messages = append([]map[string]any{{
		"role": "system", "content": prompt,
	}}, b.Messages...)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBody))
	if err != nil {
		WriteErrorCode(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			"request_too_large", "request body is too large")
		return
	}
	req, err := parseChatBody(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}

	// The native surface relays the provider's OpenAI-shaped bytes unchanged.
	s.runInference(w, r, req, openAIShaper{}, requestID)
}

// ensureRequestID reflects a caller-supplied request id or mints one, and
// echoes it so a client can correlate its call with the gateway's records.
func ensureRequestID(w http.ResponseWriter, r *http.Request) string {
	id := r.Header.Get("X-Request-ID")
	if id == "" {
		id = newRequestID()
	}
	w.Header().Set("X-Request-ID", id)
	return id
}

// runInference is the shared spine of every inference surface. The caller has
// already parsed its own wire format into the normalised request and chosen a
// shaper for the response; from here the engine path - profile prompt, chain
// resolution, failover, quota leases, per-key selection and the trail - is
// identical, so a compat surface can never drift into being a second, weaker
// gateway.
func (s *Server) runInference(w http.ResponseWriter, r *http.Request, req *chatRequestBody, shaper responseShaper, requestID string) {
	// The sticky pin reads the session id in buildChain, so it must be captured
	// before the chain resolves. Every dialect funnels through here, so a single
	// read covers chat, Responses, legacy completions and the Anthropic surface.
	req.sessionID = r.Header.Get("x-session-id")
	chain, err := s.buildChain(r.Context(), req)
	if err != nil {
		s.writeChainError(w, err, shaper)
		return
	}
	chain, req.research = s.prepareResearchHandoff(r.Context(), req, chain)
	if req.research != nil {
		w.Header().Set("X-Prowl-Research-Provider", perplexityPlatform)
	}

	relay := &chatRelay{
		server:      s,
		writer:      w,
		request:     req,
		requestID:   requestID,
		started:     time.Now(),
		shaper:      shaper,
		requestCtx:  r.Context(),
		profile:     chain.profile,
		strategy:    chain.strategy,
		smartScores: chain.smartScores,
		class:       string(chain.profile.Domain),
		effort:      chain.profile.ComplexityLabel(),
	}

	result := s.engine.Failover().Run(r.Context(), gateway.DispatchRequest{
		Chain:      chain,
		Dispatcher: relay,
		ClientGone: func() bool { return r.Context().Err() != nil },
	})

	s.finishInference(w, relay, result)
}

// finishInference writes whatever the loop could not: a failure. A success has
// already been relayed by the dispatcher, because streaming cannot be buffered
// until the loop finishes.
func (s *Server) finishInference(w http.ResponseWriter, relay *chatRelay, result *gateway.Result) {
	if result == nil {
		relay.shaper.writeError(w, http.StatusBadGateway, gateway.ExhaustUpstream, "", "the router returned no result", time.Time{})
		return
	}

	// The client disconnected mid-flight: there is no socket to render an
	// error to, and a request nobody awaited is not a provider failure to log.
	if result.Status == gateway.StatusClientGone {
		return
	}

	// On a success the relay already emitted the trail before its status line;
	// only an error response still needs the sanitised X-Fallback-* headers,
	// and only while nothing has been flushed.
	if !relay.committed {
		for name, value := range result.TrailHeaders(false) {
			w.Header().Set(name, value)
		}
	}

	switch {
	case result.Status == gateway.StatusSucceeded:
		return // already relayed
	case relay.committed:
		// Bytes were already flushed, so the status line is long gone. The
		// only honest thing left is to stop; the stream carried its own error
		// frame.
		return
	}
	// A run that reaches here never succeeded, so it is recorded before the
	// error goes out: an operator hitting a provider outage should find the
	// attempt in the logs, not an empty page next to a failing client.
	relay.recordFailure(context.WithoutCancel(relay.requestCtx), result)

	// A fatal hop ends the run carrying the provider's own verdict, and that
	// verdict is the only thing that explains the failure: a 400 for an
	// unsupported parameter reads nothing like an outage. Replacing it with
	// "every provider attempt failed" hid a dispatched, rejected request
	// behind a message saying nothing had been tried.
	if result.Exhaustion == nil && result.Status == gateway.StatusFatal {
		if ue := gateway.AsUpstreamError(result.Err); ue != nil {
			status := ue.Status
			if status < 400 {
				status = http.StatusBadGateway
			}
			relay.shaper.writeError(w, status, gateway.ExhaustUpstream, ue.Code,
				gateway.RedactWith(gateway.ProviderMessage(ue), relay.secrets...), time.Time{})
			return
		}
	}

	if result.Exhaustion != nil {
		ex := result.Exhaustion
		// The message is upstream text: a provider rejecting a credential
		// routinely quotes it back. It is scrubbed with the exact keys this
		// request revealed BEFORE the shaper formats it, so no surface's
		// envelope can leak a credential regardless of how it renders errors.
		message := gateway.RedactWith(ex.Message, relay.secrets...)
		relay.shaper.writeError(w, ex.Status, ex.Kind, ex.Code, message, ex.RetryAt)
		return
	}

	relay.shaper.writeError(w, http.StatusBadGateway, gateway.ExhaustUpstream, "", "every provider attempt failed", time.Time{})
}

// exhaustionType maps an exhaustion status onto the envelope type the client
// expects. A 401 here is an upstream verdict about a provider credential, not
// about the operator's session, so it must never be TypeAuthentication.
func exhaustionType(status int) ErrorType {
	switch status {
	case http.StatusTooManyRequests:
		return TypeRateLimit
	case http.StatusRequestEntityTooLarge, http.StatusBadRequest:
		return TypeInvalidRequest
	case http.StatusNotFound:
		return TypeNotFound
	case http.StatusServiceUnavailable:
		return TypeServiceUnavailable
	default:
		return TypeProvider
	}
}

// chatRelay dispatches one attempt and relays its response.
type chatRelay struct {
	server    *Server
	writer    http.ResponseWriter
	request   *chatRequestBody
	requestID string
	started   time.Time
	// shaper renders the engine's OpenAI-shaped result on the surface this
	// request arrived through. It is never nil: the native surface uses
	// openAIShaper, which relays the upstream bytes unchanged.
	shaper responseShaper

	// committed records that bytes reached the client, which forecloses any
	// further attempt: a stream cannot be unsent.
	committed bool

	// attempts accumulates the failed hops this request has seen, in order. The
	// relay tracks them rather than reading the loop's result because the
	// X-Fallback-* headers must be set BEFORE the status line, and by then the
	// loop has not finished. keyOrdinals mints the per-request key1/key2 labels
	// the sanitised trail renders, keyed by (platform, key) so the same
	// credential shares an ordinal across models.
	attempts    []gateway.Attempt
	keyOrdinals map[keyOrdinalKey]int

	// trail is the same hops in structured form, for the persisted record.
	trail []RequestAttemptLog

	// class is the routing class the chain resolved, carried through so the
	// analytics can show which tier served a request.
	class string
	// effort is the request complexity bucket inferred before routing.
	effort string
	// profile and smartScores explain why a smart route won without exposing
	// prompt text or credentials.
	profile     gateway.PromptProfile
	strategy    gateway.RoutingStrategy
	smartScores map[int64]gateway.SmartScore

	// requestCtx is the inbound request's context, kept so a failure can
	// still be recorded after the client has gone: the row is what an
	// operator diagnoses the outage from, and cancelling the write with the
	// request would lose exactly the rows that matter most.
	requestCtx context.Context

	// secrets holds every provider key this request actually used, so a
	// relayed upstream message can be scrubbed of the exact credential
	// rather than only of the key shapes the patterns happen to know. The
	// failover loop dispatches one hop at a time, so this needs no lock.
	secrets []string
}

// trailLog returns the failed hops for persistence.
func (c *chatRelay) trailLog() []RequestAttemptLog { return c.trail }

func (c *chatRelay) Dispatch(ctx context.Context, route gateway.Route, attempt int) gateway.DispatchResult {
	prov, ok := c.server.engine.Registry().Resolve(route.Platform, route.BaseURL)
	if !ok {
		// A configured candidate with no wire adapter is our own gap, not a
		// provider fault, and it is exactly what an operator needs surfaced.
		c.server.logServerEvent(c.requestCtx, serverLogRecord{
			Level: "warn", Source: "gateway", Provider: route.Platform, Model: route.ModelID,
			Event: "no_provider_adapter", RequestID: c.requestID,
			Message: "no wire adapter is registered for " + route.Platform,
		})
		res := gateway.DispatchResult{Err: gateway.UndispatchableError(route.Platform)}
		c.recordAttempt(route, res)
		return res
	}

	apiKey := ""
	if !prov.Keyless() {
		revealed, err := c.server.engine.Vault().Reveal(ctx, route.KeyID)
		if err != nil {
			res := gateway.DispatchResult{Err: fmt.Errorf("read key: %w", err)}
			c.noteFailure(route, res)
			return res
		}
		apiKey = revealed
		c.secrets = append(c.secrets, revealed)
	}

	// Reserve the projected tokens before calling out, so concurrent requests
	// cannot each see the same headroom and collectively exceed a limit. All
	// four published windows reach the ledger - the minute caps are what stop a
	// burst from blowing an RPM/TPM ceiling between two trailing-day reads - and
	// the Navy multiplier bills a heavy model's provider-visible drain against
	// the shared account pool at its own rate.
	lease, admitted := c.server.engine.Ledger().Acquire(gateway.Admission{
		Platform:        route.Platform,
		ModelID:         route.ModelID,
		KeyID:           route.KeyID,
		EstimatedTokens: estimateTokens(c.request),
		Limits: gateway.WindowLimits{
			RPM: derefLimit(route.RPMLimit), RPD: derefLimit(route.RPDLimit),
			TPM: derefLimit(route.TPMLimit), TPD: derefLimit(route.TPDLimit),
		},
		TokenMultiplier: route.TokenMultiplier,
	})
	if !admitted {
		res := gateway.DispatchResult{
			Status: http.StatusTooManyRequests,
			Err:    fmt.Errorf("rate limit reached for %s/%s", route.Platform, route.ModelID),
		}
		c.noteFailure(route, res)
		return res
	}
	settled := false
	defer func() {
		if !settled {
			lease.Release()
		}
	}()

	// Faithful passthrough: the gateway forwards exactly what the harness sent -
	// every message, the system prompt, and every param. It never strips a field
	// or truncates context; a provider adapter only translates the wire protocol
	// (e.g. reasoning_effort -> Claude thinking), which preserves intent rather
	// than editorialising it.
	upstream := &provider.ChatRequest{
		Model:    route.ModelID,
		Messages: c.request.Messages,
		Stream:   c.request.Stream,
		Params:   c.request.Params,
	}

	var res attemptResult
	if c.request.Stream {
		res = c.stream(ctx, prov, apiKey, upstream, route)
	} else {
		res = c.buffered(ctx, prov, apiKey, upstream, route, attempt)
	}

	switch {
	case res.Outcome == gateway.OutcomeCommitted && res.Err != nil:
		// The bytes are out and cannot be retracted, so the lease settles and
		// the caller keeps its partial answer - but this is NOT a success.
		// Recording it as one is what made a broken stream invisible: the
		// client showed an error while the logs showed 200.
		settled = true
		lease.Settle(int64(res.total()))
		c.recordBrokenStream(ctx, route, res)
	case res.succeeded():
		settled = true
		lease.Settle(int64(res.total()))
		c.recordSuccess(ctx, route, res)
	default:
		// A pre-commit failure that still reached the provider and carries a
		// usage reading - a rejected 200 (empty choices, a malformed tool call,
		// or an unrenderable envelope) - consumed the provider's request and
		// token budget. Settle that spent quota rather than releasing it, so the
		// rejected hop charges RPM/TPM while staying uncommitted and eligible
		// for failover. A failure that never reached the provider carries no
		// metering and is released by the deferred guard.
		if res.metered() {
			settled = true
			lease.Settle(int64(res.total()))
		}
		c.noteFailure(route, res.DispatchResult)
	}
	return res.DispatchResult
}

// recordBrokenStream logs a stream that committed and then failed.
//
// It is neither a clean success nor a retryable failure: the caller has part
// of an answer and an error frame. Booking it as a success is what hid a real
// provider overload - the client reported a stream error while the logs said
// 200 - so it is recorded with the upstream reason and the hops that led here.
func (c *chatRelay) recordBrokenStream(ctx context.Context, route gateway.Route, res attemptResult) {
	keyID := route.KeyID
	if _, err := c.server.RecordRequest(ctx, RequestLog{
		Platform:      route.Platform,
		ModelID:       route.ModelID,
		EndpointScope: routeScope(route),
		KeyID:         &keyID,
		Outcome:       "error",
		StatusCode:    http.StatusOK,
		InputTokens:   int64(res.inputTokens),
		OutputTokens:  int64(res.outputTokens),
		Estimated:     res.estimated,
		UsageKnown:    res.usageKnown,
		CostUSD:       res.costUSD,
		CostKnown:     res.costKnown,
		LatencyMs:     res.latency.Milliseconds(),
		Attempts:      len(c.attempts) + 1,
		Class:         c.class,
		Effort:        c.effort,
		ErrorKind:     "stream_broken",
		ErrorMessage:  gateway.RedactWith(res.Err.Error(), c.secrets...),
		Trail:         c.trailLog(),
	}); err != nil {
		slog.Debug("Could not record broken stream", "error", err)
	}
	c.server.logServerEvent(ctx, serverLogRecord{
		Level: "error", Source: "gateway", Provider: route.Platform, Model: route.ModelID,
		Event: "stream_broken", RequestID: c.requestID,
		Message: redactedProviderMessage(res.Err, c.secrets),
	})
}

// keyOrdinalKey groups a request's attempts by the credential they used, so the
// trail's key1/key2 labels match the reference: two models reached through the
// same key share an ordinal.
type keyOrdinalKey struct {
	platform string
	keyID    int64
}

// keyOrdinal returns the stable 1-based label for a route's key within this
// request, minting a new one the first time the key is seen.
func (c *chatRelay) keyOrdinal(route gateway.Route) int {
	if c.keyOrdinals == nil {
		c.keyOrdinals = map[keyOrdinalKey]int{}
	}
	k := keyOrdinalKey{platform: route.Platform, keyID: route.KeyID}
	if ord, ok := c.keyOrdinals[k]; ok {
		return ord
	}
	ord := len(c.keyOrdinals) + 1
	c.keyOrdinals[k] = ord
	return ord
}

// recordAttempt books one failed hop into the trail the next successful attempt
// advertises and into the persisted request record, WITHOUT a log line. Every
// failure funnels through here -- dispatched, or rejected before dispatch (no
// adapter, key reveal, admission) -- so a later success's X-Fallback headers
// and the stored attempt trail account for all of them.
func (c *chatRelay) recordAttempt(route gateway.Route, res gateway.DispatchResult) {
	class := gateway.ClassifyAttempt(res.Err)
	c.attempts = append(c.attempts, gateway.Attempt{
		Platform:   route.Platform,
		ModelID:    route.ModelID,
		KeyOrdinal: c.keyOrdinal(route),
		Class:      class,
	})

	keyID := route.KeyID
	// The upstream body can carry the rejected credential, so it is scrubbed
	// with the exact keys this request used -- not only the shapes the patterns
	// happen to know -- before it reaches a stored attempt row or a dashboard.
	errMessage := ""
	if res.Err != nil {
		errMessage = gateway.RedactWith(res.Err.Error(), c.secrets...)
	}
	c.trail = append(c.trail, RequestAttemptLog{
		Attempt:       len(c.attempts),
		Platform:      route.Platform,
		ModelID:       route.ModelID,
		EndpointScope: routeScope(route),
		KeyID:         &keyID,
		StatusCode:    res.Status,
		ErrorKind:     string(class),
		ErrorMessage:  errMessage,
		CreatedAt:     time.Now(),
	})
}

// noteFailure records a hop and logs it. Most failures use this; a caller that
// already emitted a more specific server-log line (a missing adapter) calls
// recordAttempt directly to avoid a duplicate.
func (c *chatRelay) noteFailure(route gateway.Route, res gateway.DispatchResult) {
	c.recordAttempt(route, res)
	c.server.logServerEvent(c.requestCtx, serverLogRecord{
		Level: "warn", Source: "gateway", Provider: route.Platform, Model: route.ModelID,
		Event: string(gateway.ClassifyAttempt(res.Err)), RequestID: c.requestID,
		Message: redactedProviderMessage(res.Err, c.secrets),
	})
}

// recordFailure persists a request that never succeeded.
//
// Only successes were being written, so a run that exhausted every candidate
// left nothing in the logs or analytics - the operator saw an error in their
// client and an empty Logs page, which is the worst possible pairing when
// diagnosing a provider outage. The failed hops are already in the trail; this
// gives them a parent row to hang from.
func (c *chatRelay) recordFailure(ctx context.Context, result *gateway.Result) {
	status := http.StatusBadGateway
	kind := "upstream"
	message := "every provider attempt failed"
	switch {
	case result != nil && result.Exhaustion != nil:
		status = result.Exhaustion.Status
		kind = string(result.Exhaustion.Kind)
		message = gateway.RedactWith(result.Exhaustion.Message, c.secrets...)
	case result != nil && result.Status == gateway.StatusFatal:
		// A fatal hop has no exhaustion body and leaves no trail entry, so
		// without this the row records a 502 with no provider, no model and
		// no reason - a failure that disappeared.
		if ue := gateway.AsUpstreamError(result.Err); ue != nil {
			if ue.Status >= 400 {
				status = ue.Status
			}
			kind = string(gateway.ClassifyKind(ue))
			message = gateway.RedactWith(gateway.ProviderMessage(ue), c.secrets...)
		}
	}

	// Attribute the row to the last candidate tried: it is the one whose
	// failure ended the run, and an unattributed row is not diagnosable.
	log := RequestLog{
		Outcome:      "error",
		StatusCode:   status,
		ErrorKind:    kind,
		ErrorMessage: message,
		LatencyMs:    time.Since(c.started).Milliseconds(),
		Attempts:     len(c.attempts),
		Class:        c.class,
		Effort:       c.effort,
		Trail:        c.trailLog(),
	}
	if last := len(c.trail); last > 0 {
		hop := c.trail[last-1]
		log.Platform, log.ModelID, log.KeyID = hop.Platform, hop.ModelID, hop.KeyID
		log.EndpointScope = hop.EndpointScope
	} else if result != nil && result.Route != nil {
		// The fatal path reports its route instead of a trail hop.
		keyID := result.Route.KeyID
		log.Platform, log.ModelID, log.KeyID = result.Route.Platform, result.Route.ModelID, &keyID
		log.EndpointScope = routeScope(*result.Route)
	}
	c.server.logServerEvent(ctx, serverLogRecord{
		Level: "error", Source: "gateway", Provider: log.Platform, Model: log.ModelID,
		Event: "exhausted", RequestID: c.requestID,
		Message: fmt.Sprintf("%s (%d attempt(s))", message, len(c.attempts)),
	})
	if _, err := c.server.RecordRequest(ctx, log); err != nil {
		slog.Debug("Could not record failed request", "error", err)
	}
}

// redactedProviderMessage is the provider's own explanation for a failed
// attempt, scrubbed of the credentials this request revealed. It prefers the
// upstream message over the raw Go error so a stored log line reads like the
// provider's verdict, never its wire body.
func redactedProviderMessage(err error, secrets []string) string {
	if err == nil {
		return ""
	}
	if ue := gateway.AsUpstreamError(err); ue != nil {
		return gateway.RedactWith(gateway.ProviderMessage(ue), secrets...)
	}
	return gateway.RedactWith(err.Error(), secrets...)
}

// writeTrailHeaders advertises the failover that happened, if any. It must be
// called before the status line, which is why the relay owns it rather than
// the caller that sees the loop's final result.
func (c *chatRelay) writeTrailHeaders() {
	if len(c.attempts) == 0 {
		return
	}
	// Reuse the loop's sanitised header renderer so the success and failure
	// paths advertise the failover identically -- key ordinals, never raw key
	// ids, and a value scrubbed of anything a hostile model id could inject.
	result := gateway.Result{Attempts: c.attempts, FailedAttempts: len(c.attempts)}
	for name, value := range result.TrailHeaders(false) {
		c.writer.Header().Set(name, value)
	}
}

// attemptResult carries the metering the loop does not care about but the
// ledger and analytics do.
type attemptResult struct {
	gateway.DispatchResult
	inputTokens  int
	outputTokens int
	estimated    bool
	usageKnown   bool
	costUSD      float64
	costKnown    bool
	latency      time.Duration
	ttfb         time.Duration
}

// total is the tokens spent against quota - prompt plus completion - which is
// what the ledger lease settles on, distinct from the split the analytics row
// records.
func (a attemptResult) total() int { return a.inputTokens + a.outputTokens }

func (a attemptResult) succeeded() bool {
	return a.Outcome == gateway.OutcomeDone || a.Outcome == gateway.OutcomeCommitted
}

// metered reports whether this attempt reached the provider and produced a
// usage reading: an exact count (usageKnown, including an explicit zero) or an
// estimate taken from a real response. A metered FAILURE consumed the
// provider's request/token budget, so its lease settles (charging RPM/TPM)
// rather than releasing (refunding); an attempt that never reached the provider
// carries neither and is released.
func (a attemptResult) metered() bool { return a.usageKnown || a.estimated }

func (c *chatRelay) buffered(ctx context.Context, prov provider.Provider, apiKey string,
	upstream *provider.ChatRequest, route gateway.Route, _ int,
) attemptResult {
	start := time.Now()
	resp, err := prov.ChatCompletion(ctx, apiKey, upstream)
	latency := time.Since(start)
	if err != nil {
		// A failure is the most informative moment for quota: a 429 usually
		// states the limit and when it resets.
		c.observeQuota(route, err)
		return attemptResult{DispatchResult: dispatchFromError(err), latency: latency}
	}
	c.server.observeQuotaHeaders(route, http.StatusOK, resp.Headers)
	// Hyper (and any provider that reports a balance rather than a window)
	// publishes what is left inside the payload, which the header observer
	// cannot see.
	c.server.engine.Ledger().ObserveBodyQuota(route.Platform, route.KeyID, resp.Raw)

	// The provider processed this request and reported (or lets us estimate) its
	// usage. That metering is captured BEFORE any rejection below: a 200 refused
	// as empty or malformed still consumed the provider's request and token
	// budget, so the failure path settles this spent quota rather than releasing
	// it. A present usage object is exact even when both counts are zero - a
	// reasoning model that spent its whole budget on hidden reasoning - while its
	// absence falls back to the marked estimate, matching the served path.
	inTokens, outTokens := 0, 0
	estimated := false
	usageKnown := false
	if resp.Usage != nil {
		inTokens, outTokens = resp.Usage.PromptTokens, resp.Usage.CompletionTokens
		usageKnown = true
	} else {
		inTokens = int(estimateTokens(c.request))
		outTokens = estimatedTokensForBytes(responseTextBytes(resp))
		estimated = true
	}
	costUSD, costKnown := reportedCost(resp.Raw)

	// A rejected 200 carries its metering so the settle path can charge the
	// quota the provider already spent; it stays uncommitted, so the loop is
	// free to fail over to the next candidate.
	reject := func(err error) attemptResult {
		return attemptResult{
			DispatchResult: gateway.DispatchResult{Err: err},
			inputTokens:    inTokens,
			outputTokens:   outTokens,
			estimated:      estimated,
			usageKnown:     usageKnown,
			latency:        latency,
		}
	}

	// A success envelope carrying no choices is a degenerate answer, not a
	// served completion: committing it hands the client an empty body as if it
	// were the truth and forfeits the failover the loop could still do. It is
	// the buffered twin of a stream that ends without a frame, so it reports the
	// same empty-completion exemption.
	if len(resp.Choices) == 0 {
		return reject(errEmptyCompletion)
	}
	// A tool call whose arguments are present but unparseable is a broken turn:
	// an agent reads them verbatim and calls the tool with garbage. Reject it
	// before commit so the loop tries another candidate rather than serving a
	// malformed call the client cannot recover from.
	if bufferedToolArgumentsMalformed(resp) {
		return reject(errInvalidToolArguments)
	}

	payload, shapeErr := c.shaper.buffered(resp)
	if shapeErr != nil {
		return reject(shapeErr)
	}

	c.setRoutedVia(route)
	c.writeTrailHeaders()
	c.writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.writer.WriteHeader(http.StatusOK)
	_, _ = c.writer.Write(payload)
	c.committed = true

	return attemptResult{
		DispatchResult: gateway.DispatchResult{Outcome: gateway.OutcomeDone},
		inputTokens:    inTokens,
		outputTokens:   outTokens,
		estimated:      estimated,
		usageKnown:     usageKnown,
		costUSD:        costUSD,
		costKnown:      costKnown,
		latency:        latency,
		ttfb:           latency,
	}
}

// bufferedToolArgumentsMalformed reports whether any tool call in a buffered
// response carries arguments that are present but not valid JSON. Empty
// arguments are a valid no-argument call, so only a non-empty unparseable
// string counts. A tool_calls array in a shape this does not recognise is left
// to the shaper rather than rejected here, so a response is never failed for a
// wire shape the check simply does not model.
func bufferedToolArgumentsMalformed(resp *provider.ChatResponse) bool {
	for i := range resp.Choices {
		raw := resp.Choices[i].Message.ToolCalls
		if len(raw) == 0 {
			continue
		}
		var calls []struct {
			Function struct {
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &calls) != nil {
			continue
		}
		for _, call := range calls {
			args := strings.TrimSpace(call.Function.Arguments)
			if args != "" && !json.Valid([]byte(args)) {
				return true
			}
		}
	}
	return false
}

func (c *chatRelay) stream(ctx context.Context, prov provider.Provider, apiKey string,
	upstream *provider.ChatRequest, route gateway.Route,
) attemptResult {
	start := time.Now()

	// A provider that accepts a stream and then says nothing is the one
	// failure the loop cannot see: Recv simply never returns, so no error is
	// ever classified and the caller waits until its own patience runs out.
	// Prowl's harness gives up after a minute, which surfaced as a dead turn
	// with a healthy pool behind it. The guard fires well inside that, and
	// only until real content arrives -- a long generation is not a stall.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var stalled atomic.Bool
	stallTimer := time.AfterFunc(streamStallTimeout(), func() {
		stalled.Store(true)
		cancelStream()
	})
	defer stallTimer.Stop()
	// Every relayed frame restarts the clock. A stall AFTER the response
	// commits cannot be rerouted -- the bytes are gone -- but leaving the
	// relay parked in Recv is worse: the turn dies on the caller's own
	// timeout and nothing is ever recorded, which is exactly how a silent
	// provider stayed invisible in the logs.
	keepAlive := func() { stallTimer.Reset(streamIdleTimeout()) }

	stream, err := prov.StreamChatCompletion(streamCtx, apiKey, upstream)
	if err != nil {
		c.observeQuota(route, err)
		if stalled.Load() {
			err = errStreamStalled
		}
		return attemptResult{DispatchResult: dispatchFromError(err), latency: time.Since(start)}
	}
	defer func() { _ = stream.Close() }()

	// Streaming is the interactive path, so a quota reading that only fired
	// for buffered calls would almost never be taken.
	if carrier, ok := stream.(provider.HeaderCarrier); ok {
		c.server.observeQuotaHeaders(route, http.StatusOK, carrier.ResponseHeaders())
	}

	strm := c.shaper.newStream()
	flusher, _ := c.writer.(http.Flusher)
	var (
		wroteHeader          bool
		ttfb                 time.Duration
		inTokens             int
		outTokens            int
		estimatedOutputBytes int
		usageReported        bool
		costUSD              float64
		costKnown            bool
		// pending holds frames that arrived before the response committed.
		// A provider often opens with a frame carrying only a role, and
		// committing on that spends the one chance to fail over on a frame
		// the client gains nothing from: a live run lost a whole request that
		// way when the overload notice arrived immediately after the opener.
		// They are held and flushed the moment real content appears.
		pending [][]byte
	)

	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if stalled.Load() {
				// The cancel came from our own guard, so report the stall
				// rather than a context error the loop would read as the
				// client walking away.
				recvErr = errStreamStalled
			}
			if !wroteHeader {
				// Nothing has been flushed, so this is still a clean failure
				// the loop may retry elsewhere.
				return attemptResult{DispatchResult: dispatchFromError(recvErr), latency: time.Since(start)}
			}
			// Bytes are already out. The client owns a partial stream, so the
			// only honest close is this surface's own in-stream error frame.
			c.writeStreamError(recvErr, strm)
			return c.withEstimatedUsage(attemptResult{
				DispatchResult: gateway.DispatchResult{Outcome: gateway.OutcomeCommitted, Err: recvErr},
				inputTokens:    inTokens, outputTokens: outTokens, costUSD: costUSD, costKnown: costKnown,
				latency: time.Since(start), ttfb: ttfb,
			}, estimatedOutputBytes, usageReported)
		}

		// A provider may report a failure INSIDE a 200 stream rather than as
		// an HTTP status: NVIDIA sends "service temporarily overloaded" this
		// way. Treating that frame as content would commit the response and
		// destroy the only chance to fail over, which is why it is classified
		// before the headers go out.
		if reason, isError := inBandStreamError(chunk); isError {
			if !wroteHeader {
				// Nothing is flushed yet, so this is an ordinary retryable
				// failure: the loop moves to the next candidate and the
				// caller never learns this provider was tried.
				return attemptResult{
					DispatchResult: dispatchFromError(&provider.HTTPError{
						Status:  http.StatusServiceUnavailable,
						Message: reason,
					}),
					latency: time.Since(start),
				}
			}
			err := errors.New(reason)
			c.writeStreamError(err, strm)
			return c.withEstimatedUsage(attemptResult{
				DispatchResult: gateway.DispatchResult{Outcome: gateway.OutcomeCommitted, Err: err},
				inputTokens:    inTokens, outputTokens: outTokens, costUSD: costUSD, costKnown: costKnown,
				latency: time.Since(start), ttfb: ttfb,
			}, estimatedOutputBytes, usageReported)
		}

		for _, choice := range chunk.Choices {
			estimatedOutputBytes += len(choice.Delta)
		}
		frame := strm.frame(chunk)

		if !wroteHeader {
			if !chunkCarriesContent(chunk) {
				// Nothing a caller can use yet, so stay uncommitted and keep
				// the frame for replay.
				if len(frame) > 0 {
					pending = append(pending, frame)
				}
				// Deliberately NOT refreshing the guard: a provider that
				// keeps sending content-free frames is still delivering
				// nothing, and treating those as progress let one hold a
				// request open indefinitely with no bytes reaching the
				// caller.
				continue
			}
			// Headers are held until the first frame with real content: until
			// then the attempt can still be moved to another provider.
			ttfb = time.Since(start)
			c.setRoutedVia(route)
			c.writeTrailHeaders()
			c.writer.Header().Set("Content-Type", "text/event-stream")
			c.writer.Header().Set("Cache-Control", "no-cache")
			c.writer.Header().Set("Connection", "keep-alive")
			c.writer.Header().Set("X-Accel-Buffering", "no")
			c.writer.WriteHeader(http.StatusOK)
			wroteHeader = true
			c.committed = true
			for _, held := range pending {
				_, _ = c.writer.Write(held)
			}
			pending = nil
		}

		// Usage is not modelled on a chunk: providers put it in a late frame,
		// so it is read from the raw JSON as it goes past, regardless of the
		// wire shape the shaper renders.
		if in, out, ok := usageFromFrame(chunk.Raw); ok {
			inTokens, outTokens = in, out
			usageReported = true
		}
		if cost, known := reportedCost(chunk.Raw); known {
			costUSD, costKnown = cost, true
		}
		// A provider that reports a balance rather than a rate window (Hyper)
		// publishes it inside the payload, which the header observer never sees.
		// The streaming path is the interactive one, so a reading taken only for
		// buffered calls would almost never fire.
		c.server.engine.Ledger().ObserveBodyQuota(route.Platform, route.KeyID, chunk.Raw)
		if len(frame) > 0 {
			_, _ = c.writer.Write(frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		keepAlive()
	}

	if !wroteHeader {
		// A stream that ended without a single frame is a degenerate answer,
		// not a success: report it so the loop can try the next candidate.
		return attemptResult{
			DispatchResult: gateway.DispatchResult{Err: errEmptyCompletion},
			latency:        time.Since(start),
		}
	}

	if tail := strm.done(); len(tail) > 0 {
		_, _ = c.writer.Write(tail)
		if flusher != nil {
			flusher.Flush()
		}
	}
	return c.withEstimatedUsage(attemptResult{
		DispatchResult: gateway.DispatchResult{Outcome: gateway.OutcomeDone},
		inputTokens:    inTokens, outputTokens: outTokens, costUSD: costUSD, costKnown: costKnown,
		latency: time.Since(start), ttfb: ttfb,
	}, estimatedOutputBytes, usageReported)
}

// streamFirstContentTimeout bounds how long an accepted stream may stay silent
// before the attempt is abandoned. It must be comfortably inside the harness's
// own one-minute patience so the gateway reroutes rather than letting the caller
// time out, and comfortably beyond a slow model's first token. Extended
// thinking on a large prompt pushes time-to-first-token well past twenty
// seconds (a big prefill plus the thinking phase begins before any token
// streams), so this sits at forty-five seconds: long enough that a reasoning
// model on a 100k-token prompt is not killed mid-think, short enough to still
// act before the harness gives up.
var streamFirstContentTimeout = 45 * time.Second

// streamStallTimeout reads the guard through one accessor so a test can drive
// it without waiting the production interval.
func streamStallTimeout() time.Duration { return streamFirstContentTimeout }

// streamIdleTimeout bounds silence between frames once the response has
// committed. It sits under the harness's one-minute patience so the gateway
// terminates and records the stream rather than letting the caller give up on
// a request the log never mentions.
var streamIdleGap = 40 * time.Second

func streamIdleTimeout() time.Duration { return streamIdleGap }

// errStreamStalled marks an attempt abandoned because the provider accepted
// the stream and then sent nothing. The wording matters: it is classified as a
// transport cut, so the model and key are skipped while the rest of the
// platform's catalogue stays in play.
var errStreamStalled = errors.New("stream error: the provider accepted the request and then sent no content")

// chunkCarriesContent reports whether a streamed frame holds something a
// caller can use. A frame announcing only the assistant role carries nothing,
// and treating it as the commit point forfeits failover for the rest of the
// request.
func chunkCarriesContent(chunk *provider.ChatChunk) bool {
	if len(chunk.Choices) == 0 {
		// A usage-only or otherwise choice-less frame still ends the answer,
		// so it counts: holding it forever would stall the stream.
		return len(chunk.Raw) > 0 && bytes.Contains(chunk.Raw, []byte(`"usage"`))
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			return true
		}
		if len(choice.Delta) == 0 {
			continue
		}
		// Anything beyond the opening role announcement counts. Enumerating
		// the fields instead would withhold a stream whose shape we do not
		// model -- reasoning arrives under at least three different keys
		// across providers, and one of them held a whole answer back.
		var delta map[string]json.RawMessage
		if json.Unmarshal(choice.Delta, &delta) != nil {
			return true
		}
		for field, raw := range delta {
			if field == "role" {
				continue
			}
			if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) &&
				!bytes.Equal(raw, []byte(`""`)) && !bytes.Equal(raw, []byte("[]")) &&
				!bytes.Equal(raw, []byte("{}")) {
				return true
			}
		}
	}
	return false
}

// inBandStreamError recognises a provider failure delivered inside a 200
// stream. Only a frame carrying an error and no choices qualifies: a normal
// frame that happens to include an error field alongside content is content.
func inBandStreamError(chunk *provider.ChatChunk) (string, bool) {
	if len(chunk.Choices) > 0 || len(chunk.Raw) == 0 {
		return "", false
	}
	var frame struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(chunk.Raw, &frame) != nil || frame.Error == nil {
		return "", false
	}
	reason := strings.TrimSpace(frame.Error.Message)
	if reason == "" {
		reason = strings.TrimSpace(frame.Error.Code)
	}
	if reason == "" {
		// The frame said "error" without saying what, which is still a
		// failure: reporting it as a successful empty answer would be worse.
		reason = "provider reported an error mid-stream"
	}
	return reason, true
}

// writeStreamError delivers a mid-stream failure as an SSE frame. The HTTP
// status was already sent as 200, so this is the only channel left.
func (c *chatRelay) writeStreamError(err error, strm streamShaper) {
	frame := strm.streamError(gateway.RedactWith(err.Error(), c.secrets...))
	if len(frame) == 0 {
		return
	}
	_, _ = c.writer.Write(frame)
	if flusher, ok := c.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// setRoutedVia tells the caller which provider actually served them, which is
// the whole point of a gateway that reroutes silently.
func (c *chatRelay) setRoutedVia(route gateway.Route) {
	value := url.PathEscape(route.Platform + "/" + route.ModelID)
	if len(value) > 256 {
		value = value[:256]
	}
	c.writer.Header().Set("X-Routed-Via", value)
	c.writer.Header().Set("X-Prowl-Route-Class", trimHeaderValue(c.class))
	c.writer.Header().Set("X-Prowl-Route-Effort", trimHeaderValue(c.effort))
	c.writer.Header().Set("X-Prowl-Route-Reason", trimHeaderValue(c.routeReason(route)))

	// The harness shows "selected model → model that answered" from these
	// two, so a gateway turn names its real provider and model instead of
	// the "auto" placeholder the user picked.
	c.writer.Header().Set("X-Prism-Model-Id", trimHeaderValue(route.ModelID))
	c.writer.Header().Set("X-Prism-Model-Name",
		trimHeaderValue(route.Platform+"/"+route.ModelID))
}

func (c *chatRelay) routeReason(route gateway.Route) string {
	reason := fmt.Sprintf("%s; %s; strategy %s", c.profile.Domain, c.profile.ComplexityLabel(), c.strategy)
	score, ok := c.smartScores[route.ModelDBID]
	if !ok {
		return reason
	}
	return fmt.Sprintf(
		"%s; capability %.2f reliability %.2f speed %.2f economy %.2f headroom %.2f quota %.2f effective %.2f; source %s",
		reason, score.Capability, score.Reliability, score.Speed, score.Economy,
		score.Headroom, score.RateLimit, score.Effective, score.Source)
}

// trimHeaderValue keeps a header inside a sane length and strips the bytes a
// header cannot carry.
func trimHeaderValue(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if len(value) > 200 {
		return value[:200]
	}
	return value
}

// recordSuccess writes the evidence the router learns from. A served request
// is also proof the key works, so it promotes a key previously marked in
// error.
func (c *chatRelay) recordSuccess(ctx context.Context, route gateway.Route, res attemptResult) {
	if err := c.server.engine.Vault().MarkHealthyFromRequest(ctx, route.KeyID); err != nil {
		slog.Debug("Could not promote key health", "key_id", route.KeyID, "error", err)
	}

	var ttfbMs *int64
	if res.ttfb > 0 {
		ms := res.ttfb.Milliseconds()
		ttfbMs = &ms
	}
	keyID := route.KeyID

	// One transactional write covers the trail row, the failover attempts,
	// the hourly rollup and the lifetime counters. Writing them separately
	// would let a crash leave analytics disagreeing with itself.
	if _, err := c.server.RecordRequest(ctx, RequestLog{
		Platform:      route.Platform,
		ModelID:       route.ModelID,
		EndpointScope: routeScope(route),
		KeyID:         &keyID,
		Outcome:       "success",
		StatusCode:    http.StatusOK,
		InputTokens:   int64(res.inputTokens),
		OutputTokens:  int64(res.outputTokens),
		Estimated:     res.estimated,
		UsageKnown:    res.usageKnown,
		CostUSD:       res.costUSD,
		CostKnown:     res.costKnown,
		LatencyMs:     res.latency.Milliseconds(),
		TTFBMs:        ttfbMs,
		Attempts:      len(c.attempts) + 1,
		Class:         c.class,
		Effort:        c.effort,
		Trail:         c.trailLog(),
	}); err != nil {
		slog.Debug("Could not record request", "error", err)
	}

	// Pin the model that actually served this request to its session, so the
	// next turn prefers it. Failover may have moved off the requested model, so
	// the served route -- not the request -- is the truth to persist. Sticky
	// resolution is owned by the routing layer; this is the one success-path
	// seam it exposes.
	c.server.recordStickySuccess(ctx, c.request, route)
}

// dispatchFromError turns a provider error into the loop's vocabulary. An
// HTTPError carries the status and stated back-off the classifier needs; a
// transport failure has status 0, which is what tells the classifier it never
// reached the provider at all.
func dispatchFromError(err error) gateway.DispatchResult {
	// A typed capability refusal - the resolved adapter has no wire for this
	// verb (chat, embeddings, or a media modality) - is the gateway's own
	// routing gap, not a provider fault: nothing was asked of the upstream. It
	// maps to the loop's undispatchable outcome so the platform is skipped for
	// this request and a compatible candidate is tried, exactly as an
	// unregistered adapter is, rather than halting the run as a fatal 400. The
	// refusal earns no cooldown, penalty, or provider-health verdict.
	if errors.Is(err, provider.ErrChatUnsupported) ||
		errors.Is(err, provider.ErrEmbeddingsUnsupported) ||
		errors.Is(err, provider.ErrModalityUnsupported) {
		return gateway.DispatchResult{Err: &gateway.UpstreamError{
			Undispatchable: true,
			Code:           "capability_unsupported",
			Message:        err.Error(),
			Cause:          err,
		}}
	}
	var httpErr *provider.HTTPError
	if errors.As(err, &httpErr) {
		return gateway.DispatchResult{
			Status:     httpErr.Status,
			Body:       string(httpErr.Body),
			Err:        err,
			RetryAfter: httpErr.RetryAfter,
		}
	}
	return gateway.DispatchResult{Err: err}
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.engine.DB().QueryContext(r.Context(),
		`SELECT platform, model_id, context_window FROM models
		 WHERE enabled = 1 AND available = 1 ORDER BY platform, model_id`)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not list models")
		return
	}
	defer func() { _ = rows.Close() }()

	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Created int64  `json:"created"`
	}
	// The routing aliases come first: they are the ids a client should
	// normally use, because they are what lets the gateway choose.
	//
	// Named lists belong here too. A chain resolves by name at request time,
	// so `auto:<name>` was already callable - but listing only `auto` left it
	// undiscoverable: nothing in the product told you the id existed, and a
	// typo fell back to the active chain instead of failing. Listing them is
	// what makes a custom list usable from any OpenAI-compatible client.
	out := []model{
		{ID: "auto", Object: "model", OwnedBy: "prowl", Created: 0},
	}
	for _, alias := range gateway.GlobalSortAliases() {
		out = append(out, model{ID: "auto:" + alias, Object: "model", OwnedBy: "prowl"})
	}
	for _, name := range s.chainNames(r.Context()) {
		out = append(out, model{ID: "auto:" + name, Object: "model", OwnedBy: "prowl"})
	}
	for rows.Next() {
		var platform, modelID string
		var context *int64
		if err := rows.Scan(&platform, &modelID, &context); err != nil {
			continue
		}
		out = append(out, model{ID: platform + "/" + modelID, Object: "model", OwnedBy: platform})
	}

	WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// chainNames lists the named lists a client may call as auto:<name>, lowercased
// because that is how resolution matches them.
func (s *Server) chainNames(ctx context.Context) []string {
	rows, err := s.engine.DB().QueryContext(ctx,
		"SELECT name FROM profiles ORDER BY LOWER(name) ASC")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return out
		}
		if trimmed := strings.ToLower(strings.TrimSpace(name)); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func newRequestID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// estimateTokens is the lightweight pre-flight guess used for admission and
// as an explicitly marked fallback when an upstream omits usage.
func estimateTokens(req *chatRequestBody) int64 {
	chars := 0
	for _, message := range req.Messages {
		if content, ok := message["content"].(string); ok {
			chars += len(content)
		}
	}
	return int64(estimatedTokensForBytes(chars))
}

func estimatedTokensForBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return max((n+3)/4, 1)
}

func (c *chatRelay) withEstimatedUsage(
	result attemptResult,
	outputBytes int,
	reported bool,
) attemptResult {
	if reported {
		result.usageKnown = true
		return result
	}
	result.inputTokens = int(estimateTokens(c.request))
	result.outputTokens = estimatedTokensForBytes(outputBytes)
	result.estimated = true
	result.usageKnown = false
	return result
}

func responseTextBytes(response *provider.ChatResponse) int {
	chars := 0
	for _, choice := range response.Choices {
		chars += rawTextBytes(choice.Message.Content)
		chars += rawTextBytes(choice.Message.Reasoning)
		chars += rawTextBytes(choice.Message.ReasoningAlt)
		chars += rawTextBytes(choice.Message.ToolCalls)
	}
	return chars
}

func rawTextBytes(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return len(raw)
	}
	return textValueBytes(value)
}

func textValueBytes(value any) int {
	switch value := value.(type) {
	case string:
		return len(value)
	case []any:
		total := 0
		for _, item := range value {
			total += textValueBytes(item)
		}
		return total
	case map[string]any:
		total := 0
		for _, item := range value {
			total += textValueBytes(item)
		}
		return total
	default:
		return 0
	}
}

func (s *Server) writeChainError(w http.ResponseWriter, err error, shaper responseShaper) {
	var chainErr *gateway.ChainError
	if errors.As(err, &chainErr) {
		shaper.writeError(w, chainErr.Status, "", "", chainErr.Message, time.Time{})
		return
	}
	shaper.writeError(w, http.StatusServiceUnavailable, gateway.ExhaustUnavailable, "", err.Error(), time.Time{})
}

// derefLimit reads a nullable published limit. Nil means the provider states
// none, which the ledger treats as unknown rather than as zero allowance.
func derefLimit(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// routeScope is the endpoint identity a request is accounted under. A route
// carries its model's endpoint_scope directly - "linked:<platform>" for a
// subscription-backed model, the normalized base URL for a custom relay - and
// that is the identity the models table, the scorer's per-model evidence and
// the monthly budget all key on. Only an ordinary catalogue route leaves it
// empty, and there the normalized base URL is the correct fallback (a built-in
// platform's empty scope). Deriving the scope from the base URL for every route
// would collapse a linked model onto its raw login endpoint and pool its spend
// with an unrelated catalogue row sharing the same model id.
func routeScope(route gateway.Route) string {
	if route.EndpointScope != "" {
		return route.EndpointScope
	}
	return normalizeBaseURL(route.BaseURL)
}

// errEmptyCompletion marks a completion that carried no answer: a stream that
// ended without a single frame, or a buffered envelope with no choices. It is a
// turn-integrity exemption candidate, not a provider-health verdict - a
// reasoning model that spent its whole budget on hidden reasoning produces one
// - so it is typed with SkipBench, which the streak logic honours for the first
// two before the third takes the normal cooldown. The "empty completion"
// wording is load-bearing: the classifier also recognises it by message
// (errclass.go).
var errEmptyCompletion = &gateway.UpstreamError{
	SkipBench: true,
	Message:   "empty completion from upstream",
}

// errInvalidToolArguments marks a buffered tool call whose arguments were
// present but not valid JSON. An agent reads tool_call.arguments verbatim, so
// committing malformed JSON calls the tool with garbage; the model produced it
// and a sibling key would reproduce it, so it is scoped to the whole model for
// this request. The "invalid tool arguments" wording is what the classifier
// matches on (errclass.go).
var errInvalidToolArguments = &gateway.UpstreamError{
	SkipModelForRequest: true,
	Message:             "invalid tool arguments from upstream",
}

// usageFromFrame pulls prompt/completion or input/output token counts out of a
// streamed frame. Providers emit usage in a late frame rather than per chunk,
// and Responses-style APIs use different field names. A present usage object
// is exact even when both counts are legitimately zero.
func usageFromFrame(raw []byte) (int, int, bool) {
	if len(raw) == 0 {
		return 0, 0, false
	}
	var frame struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.Usage == nil {
		return 0, 0, false
	}
	input, output := frame.Usage.PromptTokens, frame.Usage.CompletionTokens
	if input == 0 && output == 0 {
		input, output = frame.Usage.InputTokens, frame.Usage.OutputTokens
	}
	return input, output, true
}

// reportedCost reads an authoritative per-request total from providers that
// publish one. A present zero remains known; absence is not silently free.
func reportedCost(raw []byte) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var frame struct {
		Usage *struct {
			Cost *struct {
				Total *float64 `json:"total_cost"`
			} `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Usage == nil ||
		frame.Usage.Cost == nil || frame.Usage.Cost.Total == nil ||
		*frame.Usage.Cost.Total < 0 {
		return 0, false
	}
	return *frame.Usage.Cost.Total, true
}
