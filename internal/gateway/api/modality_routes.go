package api

// The non-chat inference plane: OpenAI-compatible embeddings, image
// generation, speech synthesis and transcription.
//
// These surfaces parse their own wire dialect into the gateway's normalised
// request, resolve candidates from the embedding_models / media_models
// catalogues (modality.go), then run the SAME failover loop chat uses over a
// modalityChain. Only the upstream capability call and the response rendering
// differ per surface; routing, quota admission, cooldowns and the request
// trail are identical, so a media call can never drift into being a second,
// weaker gateway. Each surface degrades honestly: a pool the catalogue cannot
// serve is refused before any upstream is tried, and a candidate whose adapter
// has no wire for the modality fails over rather than 404ing the caller.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// registerModalityRoutes wires the four non-chat OpenAI-compatible surfaces
// behind the same machine-credential gate as /v1/chat/completions.
func (s *Server) registerModalityRoutes() {
	s.mux.HandleFunc("POST /v1/embeddings", s.RequireMachineKey(s.handleEmbeddings))
	s.mux.HandleFunc("POST /v1/images/generations", s.RequireMachineKey(s.handleImageGeneration))
	s.mux.HandleFunc("POST /v1/audio/speech", s.RequireMachineKey(s.handleSpeech))
	s.mux.HandleFunc("POST /v1/audio/transcriptions", s.RequireMachineKey(s.handleTranscription))
}

// modalityKind names which non-chat surface a request arrived through, so one
// relay can carry the shared failover machinery while dispatching to the right
// capability interface and rendering the right wire shape.
type modalityKind int

const (
	kindEmbeddings modalityKind = iota
	kindImages
	kindSpeech
	kindTranscription
)

// label is the human word used in an unavailable/unsupported message, matching
// the modality vocabulary the catalogue queries speak.
func (k modalityKind) label() string {
	switch k {
	case kindEmbeddings:
		return "embedding"
	case kindImages:
		return "image"
	case kindSpeech:
		return "speech"
	case kindTranscription:
		return "transcription"
	default:
		return "modality"
	}
}

// ── Parsed request bodies ─────────────────────────────────────────────────────

// embeddingRequestBody is the parsed /v1/embeddings request. Input is always a
// slice: a bare string is expanded to one element so the adapter never has to
// branch on shape.
type embeddingRequestBody struct {
	Model      string
	Input      []string
	Dimensions *int
	// EncodingFormat is the OpenAI vector encoding: "float" (default) renders
	// each embedding as a JSON number array, "base64" as a base64 string of
	// the little-endian float32 bytes.
	EncodingFormat string
}

func parseEmbeddingBody(body []byte) (*embeddingRequestBody, error) {
	var raw struct {
		Model          string          `json:"model"`
		Input          json.RawMessage `json:"input"`
		Dimensions     *int            `json:"dimensions"`
		EncodingFormat string          `json:"encoding_format"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if strings.TrimSpace(raw.Model) == "" {
		return nil, errors.New("model is required")
	}
	inputs, err := parseEmbeddingInput(raw.Input)
	if err != nil {
		return nil, err
	}
	if raw.Dimensions != nil && *raw.Dimensions <= 0 {
		return nil, errors.New("dimensions must be a positive integer")
	}
	format, err := normalizeEncodingFormat(raw.EncodingFormat)
	if err != nil {
		return nil, err
	}
	return &embeddingRequestBody{
		Model: raw.Model, Input: inputs, Dimensions: raw.Dimensions, EncodingFormat: format,
	}, nil
}

// normalizeEncodingFormat validates the OpenAI encoding_format field. An empty
// value defaults to float; anything the gateway cannot render is refused so a
// client asking for base64 never silently receives float arrays.
func normalizeEncodingFormat(raw string) (string, error) {
	switch raw {
	case "", "float":
		return "float", nil
	case "base64":
		return "base64", nil
	default:
		return "", fmt.Errorf("encoding_format must be 'float' or 'base64', got %q", raw)
	}
}

// parseEmbeddingInput accepts a single string or an array of strings, the two
// shapes the OpenAI embeddings surface defines. Token-id arrays are refused
// rather than silently mis-serialised, so a caller learns the gateway cannot
// serve them instead of getting a wrong vector back.
func parseEmbeddingInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("input is required")
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if strings.TrimSpace(one) == "" {
			return nil, errors.New("input is required")
		}
		return []string{one}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		if len(many) == 0 {
			return nil, errors.New("input is required")
		}
		return many, nil
	}
	return nil, errors.New("input must be a string or an array of strings")
}

// imageRequestBody is the parsed /v1/images/generations request.
type imageRequestBody struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	ResponseFormat string
}

func parseImageBody(body []byte) (*imageRequestBody, error) {
	var raw struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              *int   `json:"n"`
		Size           string `json:"size"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if strings.TrimSpace(raw.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	n := 1
	if raw.N != nil {
		n = *raw.N
		if n < 1 || n > 10 {
			return nil, errors.New("n must be between 1 and 10")
		}
	}
	return &imageRequestBody{
		Model: raw.Model, Prompt: raw.Prompt, N: n,
		Size: raw.Size, ResponseFormat: raw.ResponseFormat,
	}, nil
}

// speechRequestBody is the parsed /v1/audio/speech request. Both response_format
// (OpenAI) and format (some clients) are accepted for the audio codec.
type speechRequestBody struct {
	Model  string
	Input  string
	Voice  string
	Format string
}

func parseSpeechBody(body []byte) (*speechRequestBody, error) {
	var raw struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
		Format         string `json:"format"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if strings.TrimSpace(raw.Input) == "" {
		return nil, errors.New("input is required")
	}
	format := raw.ResponseFormat
	if format == "" {
		format = raw.Format
	}
	if !knownSpeechFormat(format) {
		return nil, fmt.Errorf(
			"response_format must be one of mp3, opus, aac, flac, wav, pcm, got %q", format)
	}
	return &speechRequestBody{Model: raw.Model, Input: raw.Input, Voice: raw.Voice, Format: format}, nil
}

// transcriptionRequestBody is the parsed /v1/audio/transcriptions upload. The
// audio bytes are held in memory: they never touch disk.
type transcriptionRequestBody struct {
	Model          string
	File           []byte
	Filename       string
	MimeType       string
	Language       string
	Prompt         string
	Temperature    *float64
	ResponseFormat string
}

func parseTranscriptionForm(r *http.Request) (*transcriptionRequestBody, error) {
	file, header, err := r.FormFile("file")
	if err != nil {
		return nil, errors.New("file is required")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("could not read audio upload: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	format := r.FormValue("response_format")
	if !knownTranscriptionFormat(format) {
		return nil, fmt.Errorf(
			"response_format must be one of json, text, verbose_json, srt, vtt, got %q", format)
	}
	out := &transcriptionRequestBody{
		Model:          r.FormValue("model"),
		File:           data,
		Filename:       header.Filename,
		MimeType:       header.Header.Get("Content-Type"),
		Language:       r.FormValue("language"),
		Prompt:         r.FormValue("prompt"),
		ResponseFormat: format,
	}
	if temp := strings.TrimSpace(r.FormValue("temperature")); temp != "" {
		f, err := strconv.ParseFloat(temp, 64)
		if err != nil {
			return nil, errors.New("temperature must be a number")
		}
		out.Temperature = &f
	}
	return out, nil
}

// ── Format validation ────────────────────────────────────────────────────────

// knownSpeechFormat reports whether a requested TTS codec is one the OpenAI
// speech surface defines. The set is universally knowable, so a name outside it
// (a typo, or a codec no OpenAI-compatible provider names) is a client error
// refused at the boundary. Whether a PARTICULAR provider can produce a valid
// codec is a capability question left to the failover loop, not decided here.
func knownSpeechFormat(f string) bool {
	switch f {
	case "", "mp3", "opus", "aac", "flac", "wav", "pcm":
		return true
	}
	return false
}

// knownTranscriptionFormat reports whether a requested transcription
// response_format is one this gateway actually renders. An unknown value is
// refused rather than silently answered as JSON, so a client that asked for a
// format the gateway cannot produce learns it instead of receiving the wrong
// shape.
func knownTranscriptionFormat(f string) bool {
	switch f {
	case "", "json", "text", "verbose_json", "srt", "vtt":
		return true
	}
	return false
}

// errSpeechFormatMismatch marks a speech attempt that succeeded upstream but
// answered in a different codec than the caller requested. It is a typed
// refusal so the failover loop tries a provider that can produce the requested
// codec rather than committing honest bytes in the wrong format as a 200.
var errSpeechFormatMismatch = errors.New("provider produced a different audio codec than requested")

// speechFormatMediaTypes maps each OpenAI TTS response_format to the response
// media types that legitimately carry it, including the common aliases a
// provider may send. A provider that returns a media type in some OTHER
// format's set produced a different codec than the caller asked for.
var speechFormatMediaTypes = map[string]map[string]bool{
	"mp3":  {"audio/mpeg": true, "audio/mp3": true, "audio/mpeg3": true, "audio/x-mpeg-3": true},
	"opus": {"audio/ogg": true, "audio/opus": true},
	"aac":  {"audio/aac": true, "audio/x-aac": true, "audio/aacp": true, "audio/mp4": true},
	"flac": {"audio/flac": true, "audio/x-flac": true},
	"wav":  {"audio/wav": true, "audio/x-wav": true, "audio/wave": true, "audio/vnd.wave": true},
	"pcm":  {"audio/l16": true, "audio/pcm": true, "audio/basic": true},
}

// speechCodecSatisfied reports whether a served response's content type carries
// the codec the caller requested. It only flags a mismatch it can PROVE: a
// media type that is a recognised audio codec for some other format. An empty
// request (no codec named), an empty content type (the provider stated no
// codec), or an unrecognised/ambiguous type is not second-guessed, so an honest
// answer is never failed over on a hunch.
func speechCodecSatisfied(format, contentType string) bool {
	if format == "" {
		return true
	}
	accepted, ok := speechFormatMediaTypes[format]
	if !ok {
		return true // an unknown format is refused at the boundary, not here
	}
	mt := mediaTypeOnly(contentType)
	if mt == "" {
		return true
	}
	if accepted[mt] {
		return true
	}
	return !isKnownSpeechMediaType(mt)
}

// isKnownSpeechMediaType reports whether a media type is the recognised carrier
// of any OpenAI TTS codec, i.e. a type the gateway can name a format for.
func isKnownSpeechMediaType(mt string) bool {
	for _, set := range speechFormatMediaTypes {
		if set[mt] {
			return true
		}
	}
	return false
}

// mediaTypeOnly strips a content type's parameters and normalises case, so
// "audio/L16; rate=24000" compares as "audio/l16".
func mediaTypeOnly(contentType string) string {
	mt := contentType
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	body, ok := readModalityBody(w, r)
	if !ok {
		return
	}
	req, err := parseEmbeddingBody(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	ctx := r.Context()
	candidates, err := embeddingCandidates(ctx, s.engine.DB(), req.Model)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not resolve embedding models")
		return
	}
	if len(candidates) == 0 {
		modalityUnavailable(w, "embedding", req.Model,
			hasEnabledEmbeddingModels(ctx, s.engine.DB()),
			embeddingModelKnown(ctx, s.engine.DB(), req.Model))
		return
	}
	relay := s.newModalityRelay(w, r, kindEmbeddings, requestID, candidates)
	relay.embedding = req
	relay.execute(ctx)
}

func (s *Server) handleImageGeneration(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	body, ok := readModalityBody(w, r)
	if !ok {
		return
	}
	req, err := parseImageBody(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	s.serveMedia(w, r, kindImages, "image", req.Model, requestID, func(relay *modalityRelay) {
		relay.image = req
	})
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	body, ok := readModalityBody(w, r)
	if !ok {
		return
	}
	req, err := parseSpeechBody(body)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	s.serveMedia(w, r, kindSpeech, "audio", req.Model, requestID, func(relay *modalityRelay) {
		relay.speech = req
	})
}

func (s *Server) handleTranscription(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, maxInferenceBody)
	if err := r.ParseMultipartForm(maxInferenceBody); err != nil {
		WriteErrorCode(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			"request_too_large", "audio upload is too large or not valid multipart form data")
		return
	}
	req, err := parseTranscriptionForm(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	s.serveMedia(w, r, kindTranscription, "transcription", req.Model, requestID, func(relay *modalityRelay) {
		relay.transcription = req
	})
}

// serveMedia is the shared spine for the three media_models surfaces: resolve
// the modality's pool, refuse honestly when empty, otherwise run the failover
// loop. Embeddings resolve their own pool (a different table and the family
// unit), so they wire the relay directly rather than through here.
func (s *Server) serveMedia(
	w http.ResponseWriter, r *http.Request,
	kind modalityKind, modality, model, requestID string,
	bind func(*modalityRelay),
) {
	ctx := r.Context()
	candidates, err := mediaCandidates(ctx, s.engine.DB(), modality, model)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer,
			"could not resolve "+modality+" models")
		return
	}
	if len(candidates) == 0 {
		modalityUnavailable(w, modality, model,
			hasEnabledMediaModels(ctx, s.engine.DB(), modality),
			mediaModelKnown(ctx, s.engine.DB(), modality, model))
		return
	}
	relay := s.newModalityRelay(w, r, kind, requestID, candidates)
	bind(relay)
	relay.execute(ctx)
}

// readModalityBody reads a bounded JSON request body, answering the same
// too-large envelope the chat plane uses when the ceiling is hit.
func readModalityBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInferenceBody))
	if err != nil {
		WriteErrorCode(w, http.StatusRequestEntityTooLarge, TypeInvalidRequest,
			"request_too_large", "request body is too large")
		return nil, false
	}
	return body, true
}

// ── Relay ─────────────────────────────────────────────────────────────────────

// modalityResult carries the metering and the deferred response writer one
// successful attempt produced. The write closure runs only after the trail
// headers are set, because a header written after the status line is discarded.
type modalityResult struct {
	inputTokens  int
	outputTokens int
	estimated    bool
	usageKnown   bool
	// headers carries the successful upstream response's headers so the ledger
	// can read its rate-limit families from a served call, not only a failed
	// one. A provider that publishes remaining quota does so on every response.
	headers http.Header
	write   func()
}

// modalityRelay dispatches one non-chat attempt and, on success, relays its
// response. It is the Dispatcher the failover loop drives; it mirrors the chat
// relay's bookkeeping (trail, secrets, redaction) so media traffic lands in the
// same logs and analytics as chat.
type modalityRelay struct {
	server    *Server
	writer    http.ResponseWriter
	kind      modalityKind
	requestID string
	started   time.Time
	chain     *modalityChain

	// Exactly one of these is set, per kind.
	embedding     *embeddingRequestBody
	image         *imageRequestBody
	speech        *speechRequestBody
	transcription *transcriptionRequestBody

	// committed records that bytes reached the client, foreclosing any retry.
	committed bool
	// unsupported counts hops refused because the adapter has no wire for this
	// modality, so an all-unsupported exhaustion can be named honestly rather
	// than reported as a generic upstream failure.
	unsupported int
	// formatMismatch counts hops that served successfully but produced a
	// different audio codec than the caller asked for. When every hop mismatches
	// the exhaustion names the codec rather than committing a misleading 200.
	formatMismatch int

	failed  []string
	trail   []RequestAttemptLog
	secrets []string

	// requestCtx keeps the inbound context so a failure can still be recorded
	// after the client has gone.
	requestCtx context.Context
}

func (s *Server) newModalityRelay(
	w http.ResponseWriter, r *http.Request,
	kind modalityKind, requestID string, candidates []modalityCandidate,
) *modalityRelay {
	return &modalityRelay{
		server:     s,
		writer:     w,
		kind:       kind,
		requestID:  requestID,
		started:    time.Now(),
		chain:      &modalityChain{candidates: candidates},
		requestCtx: r.Context(),
	}
}

// execute runs the failover loop and finalises whatever it could not: a
// success has already been relayed inside Dispatch, so only a failure remains
// to record and render here.
func (m *modalityRelay) execute(ctx context.Context) {
	result := m.server.engine.Failover().Run(ctx, gateway.DispatchRequest{
		Chain:      m.chain,
		Dispatcher: m,
		ClientGone: func() bool { return ctx.Err() != nil },
	})
	if result != nil && result.Status == gateway.StatusSucceeded {
		return
	}
	if m.committed {
		return
	}
	m.recordFailure(context.WithoutCancel(m.requestCtx), result)
	if m.allFormatMismatch() {
		WriteErrorCode(m.writer, http.StatusBadGateway, TypeUpstream, "format_unavailable",
			fmt.Sprintf("no configured provider produced %s in the requested %q format",
				m.kind.label(), m.speech.Format))
		return
	}
	if m.allUnsupported() {
		detail := ""
		if model := m.requestedModel(); model != "" && model != "auto" {
			detail = fmt.Sprintf(" %q", model)
		}
		WriteErrorCode(m.writer, http.StatusServiceUnavailable, TypeServiceUnavailable,
			"modality_unsupported",
			fmt.Sprintf("no configured provider serves %s%s - the catalogue lists a model but its adapter has no %s wire",
				m.kind.label(), detail, m.kind.label()))
		return
	}
	writeModalityFailure(m.writer, result, m.secrets)
}

// allUnsupported reports that every failed hop was an adapter-capability
// refusal, which is a catalogue inconsistency rather than an upstream outage.
func (m *modalityRelay) allUnsupported() bool {
	return len(m.failed) > 0 && m.unsupported == len(m.failed)
}

// allFormatMismatch reports that every failed hop served a different audio
// codec than the caller requested - the served answer was honest bytes in the
// wrong format, so committing any one of them would be a misleading 200.
func (m *modalityRelay) allFormatMismatch() bool {
	return len(m.failed) > 0 && m.formatMismatch == len(m.failed) && m.speech != nil
}

func (m *modalityRelay) Dispatch(ctx context.Context, route gateway.Route, _ int) gateway.DispatchResult {
	prov, ok := m.server.engine.Registry().Resolve(route.Platform, route.BaseURL)
	if !ok {
		m.server.logServerEvent(m.requestCtx, serverLogRecord{
			Level: "warn", Source: "gateway", Provider: route.Platform, Model: route.ModelID,
			Event: "no_provider_adapter", RequestID: m.requestID,
			Message: "no wire adapter is registered for " + route.Platform,
		})
		res := gateway.DispatchResult{Err: gateway.UndispatchableError(route.Platform)}
		m.noteFailure(route, res)
		return res
	}

	apiKey := ""
	if !prov.Keyless() {
		revealed, err := m.server.engine.Vault().Reveal(ctx, route.KeyID)
		if err != nil {
			res := gateway.DispatchResult{Err: fmt.Errorf("read key: %w", err)}
			m.noteFailure(route, res)
			return res
		}
		apiKey = revealed
		m.secrets = append(m.secrets, revealed)
	}

	lease, admitted := m.server.engine.Ledger().Acquire(gateway.Admission{
		Platform:        route.Platform,
		ModelID:         route.ModelID,
		KeyID:           route.KeyID,
		EstimatedTokens: m.estimatedTokens(),
		Limits: gateway.WindowLimits{
			RPD: derefLimit(route.RPDLimit), TPD: derefLimit(route.TPDLimit),
		},
	})
	if !admitted {
		res := gateway.DispatchResult{
			Status: http.StatusTooManyRequests,
			Err:    fmt.Errorf("rate limit reached for %s/%s", route.Platform, route.ModelID),
		}
		m.noteFailure(route, res)
		return res
	}
	settled := false
	defer func() {
		if !settled {
			lease.Release()
		}
	}()

	start := time.Now()
	out, err := m.call(ctx, prov, apiKey, route)
	latency := time.Since(start)
	if err != nil {
		if errors.Is(err, provider.ErrEmbeddingsUnsupported) || errors.Is(err, provider.ErrModalityUnsupported) {
			m.unsupported++
		}
		if errors.Is(err, errSpeechFormatMismatch) {
			m.formatMismatch++
		}
		m.observeQuota(route, err)
		res := dispatchFromError(err)
		m.noteFailure(route, res)
		return res
	}

	// The bytes are committed inside this attempt, so the trail headers must
	// be set first: after the status line they are silently dropped.
	m.setRoutedVia(route)
	m.writeFallbackHeaders()
	out.write()
	m.committed = true
	settled = true
	lease.Settle(int64(out.inputTokens + out.outputTokens))
	// A served response is a quota reading too: hand its headers to the ledger
	// so a provider's remaining budget is observed on success, not only on the
	// 429 that finally exhausts it.
	m.server.observeQuotaHeaders(route, http.StatusOK, out.headers)
	m.recordSuccess(ctx, route, out, latency)
	return gateway.DispatchResult{Outcome: gateway.OutcomeDone}
}

// call performs the modality-specific upstream request against the resolved
// adapter, type-asserting the optional capability interface. A platform whose
// adapter does not implement the capability is a typed refusal, so the loop
// moves to the next candidate rather than sending a body to an endpoint that
// would reject it.
func (m *modalityRelay) call(
	ctx context.Context, prov provider.Provider, apiKey string, route gateway.Route,
) (modalityResult, error) {
	switch m.kind {
	case kindEmbeddings:
		embedder, ok := prov.(provider.Embedder)
		if !ok {
			return modalityResult{}, fmt.Errorf("%w: %s", provider.ErrEmbeddingsUnsupported, route.Platform)
		}
		if cand, ok := m.chain.candidateFor(route); ok && cand.MaxInputTokens > 0 {
			if est := int64(estimatedTokensForBytes(embeddingInputBytes(m.embedding.Input))); est > cand.MaxInputTokens {
				return modalityResult{}, fmt.Errorf(
					"input of ~%d tokens exceeds the %d-token limit of %s", est, cand.MaxInputTokens, route.ModelID)
			}
		}
		resp, err := embedder.Embeddings(ctx, apiKey, &provider.EmbeddingRequest{
			Model: route.ModelID, Input: m.embedding.Input, Dimensions: m.embedding.Dimensions,
		})
		if err != nil {
			return modalityResult{}, err
		}
		reported := resp.InputTokens != nil
		tokens := 0
		if reported {
			tokens = *resp.InputTokens
		} else {
			tokens = estimatedTokensForBytes(embeddingInputBytes(m.embedding.Input))
		}
		return modalityResult{
			inputTokens: tokens,
			estimated:   !reported,
			usageKnown:  reported,
			headers:     resp.Headers,
			write:       func() { m.writeEmbeddings(route, resp, tokens) },
		}, nil

	case kindImages:
		gen, ok := prov.(provider.ImageGenerator)
		if !ok {
			return modalityResult{}, fmt.Errorf("%w: %s image", provider.ErrModalityUnsupported, route.Platform)
		}
		resp, err := gen.Images(ctx, apiKey, &provider.ImageRequest{
			Model: route.ModelID, Prompt: m.image.Prompt, N: m.image.N,
			Size: m.image.Size, ResponseFormat: m.image.ResponseFormat,
		})
		if err != nil {
			return modalityResult{}, err
		}
		return modalityResult{headers: resp.Headers, write: func() { m.writeImages(resp) }}, nil

	case kindSpeech:
		synth, ok := prov.(provider.SpeechSynthesizer)
		if !ok {
			return modalityResult{}, fmt.Errorf("%w: %s speech", provider.ErrModalityUnsupported, route.Platform)
		}
		resp, err := synth.Speech(ctx, apiKey, &provider.SpeechRequest{
			Model: route.ModelID, Input: m.speech.Input, Voice: m.speech.Voice, Format: m.speech.Format,
		})
		if err != nil {
			return modalityResult{}, err
		}
		// The caller asked for a specific codec: a provider that answered in a
		// different one served honest bytes in the wrong format. Refuse it here
		// so the loop fails over to a provider that can produce the requested
		// codec rather than committing a misleading 200.
		if !speechCodecSatisfied(m.speech.Format, resp.ContentType) {
			return modalityResult{}, &gateway.UpstreamError{
				// 502 is what makes the loop treat this as retryable, so it fails
				// over; SkipModelForRequest scopes the skip to this model rather
				// than benching the whole platform, so a sibling that CAN produce
				// the codec is still tried. errSpeechFormatMismatch rides as the
				// cause so the relay counts the hop and names the codec honestly.
				Status:              http.StatusBadGateway,
				SkipModelForRequest: true,
				Code:                "format_unavailable",
				Message: fmt.Sprintf("%s produced %q, not the requested %q codec",
					route.Platform, resp.ContentType, m.speech.Format),
				Cause: errSpeechFormatMismatch,
			}
		}
		return modalityResult{headers: resp.Headers, write: func() { m.writeSpeech(resp) }}, nil

	case kindTranscription:
		transcriber, ok := prov.(provider.Transcriber)
		if !ok {
			return modalityResult{}, fmt.Errorf("%w: %s transcription", provider.ErrModalityUnsupported, route.Platform)
		}
		var temp *float64
		if m.transcription.Temperature != nil {
			t := *m.transcription.Temperature
			temp = &t
		}
		style := ""
		if cand, ok := m.chain.candidateFor(route); ok {
			style = cand.RequestStyle
		}
		resp, err := transcriber.Transcribe(ctx, apiKey, &provider.TranscriptionRequest{
			Model:          route.ModelID,
			File:           m.transcription.File,
			Filename:       m.transcription.Filename,
			MimeType:       m.transcription.MimeType,
			Language:       m.transcription.Language,
			Prompt:         m.transcription.Prompt,
			Temperature:    temp,
			ResponseFormat: m.transcription.ResponseFormat,
			RequestStyle:   style,
		})
		if err != nil {
			return modalityResult{}, err
		}
		return modalityResult{headers: resp.Headers, write: func() { m.writeTranscription(resp) }}, nil

	default:
		return modalityResult{}, fmt.Errorf("unknown modality")
	}
}

// estimatedTokens is the ledger admission's pre-flight guess. Only embeddings
// carry a meaningful token count; media requests reserve nothing.
func (m *modalityRelay) estimatedTokens() int64 {
	if m.kind == kindEmbeddings && m.embedding != nil {
		return int64(estimatedTokensForBytes(embeddingInputBytes(m.embedding.Input)))
	}
	return 0
}

func (m *modalityRelay) requestedModel() string {
	switch {
	case m.embedding != nil:
		return m.embedding.Model
	case m.image != nil:
		return m.image.Model
	case m.speech != nil:
		return m.speech.Model
	case m.transcription != nil:
		return m.transcription.Model
	}
	return ""
}

// ── Response rendering ────────────────────────────────────────────────────────

func (m *modalityRelay) writeEmbeddings(route gateway.Route, resp *provider.EmbeddingResponse, promptTokens int) {
	type embItem struct {
		Object string `json:"object"`
		Index  int    `json:"index"`
		// Embedding is a []float64 for the float encoding and a base64 string
		// for the base64 encoding, matching the OpenAI response contract.
		Embedding any `json:"embedding"`
	}
	base64Format := m.embedding.EncodingFormat == "base64"
	data := make([]embItem, len(resp.Vectors))
	for i, v := range resp.Vectors {
		item := embItem{Object: "embedding", Index: i}
		if base64Format {
			item.Embedding = embeddingBase64(v)
		} else {
			item.Embedding = v
		}
		data[i] = item
	}
	m.writeJSON(http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"model":  route.ModelID,
		"usage": map[string]any{
			"prompt_tokens": promptTokens,
			"total_tokens":  promptTokens,
		},
	})
}

// embeddingBase64 encodes one vector as OpenAI's base64 embedding: the raw
// little-endian IEEE-754 float32 bytes of the vector, base64-standard encoded.
func embeddingBase64(vec []float64) string {
	buf := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(f)))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func (m *modalityRelay) writeImages(resp *provider.ImageResponse) {
	m.writeJSON(http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    resp.Data,
	})
}

func (m *modalityRelay) writeSpeech(resp *provider.SpeechResponse) {
	ct := resp.ContentType
	if ct == "" {
		ct = "audio/mpeg"
	}
	m.writer.Header().Set("Content-Type", ct)
	m.writer.Header().Set("Content-Length", strconv.Itoa(len(resp.Audio)))
	m.writer.WriteHeader(http.StatusOK)
	_, _ = m.writer.Write(resp.Audio)
}

func (m *modalityRelay) writeTranscription(resp *provider.TranscriptionResponse) {
	switch m.transcription.ResponseFormat {
	case "text":
		m.writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		m.writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(m.writer, resp.Text)
	case "vtt":
		m.writer.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		m.writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(m.writer, transcriptionVTT(resp))
	case "srt":
		m.writer.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		m.writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(m.writer, transcriptionSRT(resp))
	case "verbose_json":
		payload := map[string]any{"task": "transcribe", "text": resp.Text}
		if resp.Language != "" {
			payload["language"] = resp.Language
		}
		if resp.Duration > 0 {
			payload["duration"] = resp.Duration
		}
		if len(resp.Segments) > 0 {
			payload["segments"] = resp.Segments
		}
		m.writeJSON(http.StatusOK, payload)
	default:
		m.writeJSON(http.StatusOK, map[string]any{"text": resp.Text})
	}
}

// transcriptionVTT renders a WebVTT document. A provider that emits native
// subtitles supplies them directly; otherwise the transcript is wrapped in a
// single cue spanning the reported duration so the response is still valid VTT.
func transcriptionVTT(resp *provider.TranscriptionResponse) string {
	if strings.TrimSpace(resp.VTT) != "" {
		return resp.VTT
	}
	end := resp.Duration
	if end <= 0 {
		end = 1
	}
	return fmt.Sprintf("WEBVTT\n\n00:00:00.000 --> %s\n%s\n", vttTimestamp(end), resp.Text)
}

// transcriptionSRT renders a SubRip (.srt) document - the same transcript the
// VTT path serves, in the codec-mismatched format's sibling wire so a client
// that asked for SRT is never silently handed JSON. When the provider reported
// per-segment timing the cues carry it; otherwise the whole transcript is one
// cue spanning the reported duration, which is still valid SRT.
func transcriptionSRT(resp *provider.TranscriptionResponse) string {
	if segs := parseTranscriptSegments(resp.Segments); len(segs) > 0 {
		var b strings.Builder
		for i, s := range segs {
			fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
				i+1, srtTimestamp(s.Start), srtTimestamp(s.End), strings.TrimSpace(s.Text))
		}
		return b.String()
	}
	end := resp.Duration
	if end <= 0 {
		end = 1
	}
	return fmt.Sprintf("1\n%s --> %s\n%s\n", srtTimestamp(0), srtTimestamp(end), resp.Text)
}

// transcriptSegment is the timed slice of a verbose transcript, the fields SRT
// and VTT cues need. Providers carry more, but only these three are rendered.
type transcriptSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// parseTranscriptSegments reads the OpenAI verbose_json segment array a
// provider may return, keeping only segments that carry text. A shape it cannot
// parse yields no segments, so the caller falls back to a single cue rather
// than failing the response.
func parseTranscriptSegments(raw json.RawMessage) []transcriptSegment {
	if len(raw) == 0 {
		return nil
	}
	var segs []transcriptSegment
	if err := json.Unmarshal(raw, &segs); err != nil {
		return nil
	}
	out := segs[:0]
	for _, s := range segs {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// vttTimestamp renders seconds as the HH:MM:SS.mmm a WebVTT cue expects.
func vttTimestamp(seconds float64) string {
	h, m, s, ms := splitClock(seconds)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// srtTimestamp renders seconds as the HH:MM:SS,mmm a SubRip cue expects - the
// comma millisecond separator is the one wire difference from WebVTT.
func srtTimestamp(seconds float64) string {
	h, m, s, ms := splitClock(seconds)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// splitClock decomposes a second count into whole hours, minutes, seconds and
// milliseconds, clamping a negative to zero so a malformed segment cannot emit
// a nonsensical cue.
func splitClock(seconds float64) (h, m, s, ms int64) {
	if seconds < 0 {
		seconds = 0
	}
	total := int64(seconds*1000 + 0.5)
	h = total / 3600000
	total -= h * 3600000
	m = total / 60000
	total -= m * 60000
	s = total / 1000
	ms = total - s*1000
	return
}

func (m *modalityRelay) writeJSON(status int, payload any) {
	m.writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	m.writer.WriteHeader(status)
	if b, err := json.Marshal(payload); err == nil {
		_, _ = m.writer.Write(b)
	}
}

// ── Bookkeeping (mirrors the chat relay so media lands in the same trail) ─────

func (m *modalityRelay) setRoutedVia(route gateway.Route) {
	value := url.PathEscape(route.Platform + "/" + route.ModelID)
	if len(value) > 256 {
		value = value[:256]
	}
	m.writer.Header().Set("X-Routed-Via", value)
}

// writeFallbackHeaders advertises the failover that happened before a served
// response, so a client can see it was rerouted.
func (m *modalityRelay) writeFallbackHeaders() {
	if len(m.failed) == 0 {
		return
	}
	m.writer.Header().Set("X-Fallback-Attempts", strconv.Itoa(len(m.failed)))
	shown := m.failed
	suffix := ""
	if len(shown) > 10 {
		suffix = fmt.Sprintf("; +%d more", len(shown)-10)
		shown = shown[:10]
	}
	trail := strings.Join(shown, "; ") + suffix
	if len(trail) > 1024 {
		trail = trail[:1024]
	}
	m.writer.Header().Set("X-Fallback-Trail", trail)
}

// observeQuota hands a failed attempt's rate-limit headers to the ledger. A
// bare 429/402 with no headers still states the pool is spent, so the forward
// is unconditional once the error is an HTTPError; observeQuotaHeaders decides
// what a headerless status is worth recording.
func (m *modalityRelay) observeQuota(route gateway.Route, err error) {
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		return
	}
	m.server.observeQuotaHeaders(route, httpErr.Status, httpErr.Headers)
}

// noteFailure records one failed hop for the trail the next successful attempt
// advertises and the failure row records, scrubbing the credential this request
// revealed from the stored provider message.
func (m *modalityRelay) noteFailure(route gateway.Route, res gateway.DispatchResult) {
	class := string(gateway.ClassifyAttempt(res.Err))
	m.failed = append(m.failed,
		fmt.Sprintf("%s/%s key%d: %s", route.Platform, route.ModelID, route.KeyID, class))

	keyID := route.KeyID
	errMessage := ""
	if res.Err != nil {
		errMessage = gateway.RedactWith(res.Err.Error(), m.secrets...)
	}
	m.trail = append(m.trail, RequestAttemptLog{
		Attempt:       len(m.failed),
		Platform:      route.Platform,
		ModelID:       route.ModelID,
		EndpointScope: normalizeBaseURL(route.BaseURL),
		KeyID:         &keyID,
		StatusCode:    res.Status,
		ErrorKind:     class,
		ErrorMessage:  errMessage,
		CreatedAt:     time.Now(),
	})
	m.server.logServerEvent(m.requestCtx, serverLogRecord{
		Level: "warn", Source: "gateway", Provider: route.Platform, Model: route.ModelID,
		Event: class, RequestID: m.requestID,
		Message: redactedProviderMessage(res.Err, m.secrets),
	})
}

// recordSuccess persists a served modality call on the same trail the dashboard
// reads, carrying the endpoint scope so a custom endpoint's identity is never
// collapsed, and promotes a key previously marked in error.
func (m *modalityRelay) recordSuccess(ctx context.Context, route gateway.Route, out modalityResult, latency time.Duration) {
	if err := m.server.engine.Vault().MarkHealthyFromRequest(ctx, route.KeyID); err != nil {
		slog.Debug("Could not promote key health", "key_id", route.KeyID, "error", err)
	}
	keyID := route.KeyID
	if _, err := m.server.RecordRequest(ctx, RequestLog{
		Platform:      route.Platform,
		ModelID:       route.ModelID,
		EndpointScope: normalizeBaseURL(route.BaseURL),
		KeyID:         &keyID,
		Outcome:       "success",
		StatusCode:    http.StatusOK,
		InputTokens:   int64(out.inputTokens),
		OutputTokens:  int64(out.outputTokens),
		Estimated:     out.estimated,
		UsageKnown:    out.usageKnown,
		LatencyMs:     latency.Milliseconds(),
		Attempts:      len(m.failed) + 1,
		Trail:         m.trail,
	}); err != nil {
		slog.Debug("Could not record modality request", "error", err)
	}
}

// recordFailure persists a modality run that never succeeded, so an operator
// hitting an outage finds the attempt in the logs instead of an empty page.
func (m *modalityRelay) recordFailure(ctx context.Context, result *gateway.Result) {
	status := http.StatusBadGateway
	kind := "upstream"
	message := "every provider attempt failed"
	switch {
	case result != nil && result.Exhaustion != nil:
		status = result.Exhaustion.Status
		kind = string(result.Exhaustion.Kind)
		message = gateway.RedactWith(result.Exhaustion.Message, m.secrets...)
	case result != nil && result.Status == gateway.StatusFatal:
		if ue := gateway.AsUpstreamError(result.Err); ue != nil {
			if ue.Status >= 400 {
				status = ue.Status
			}
			kind = string(gateway.ClassifyKind(ue))
			message = gateway.RedactWith(gateway.ProviderMessage(ue), m.secrets...)
		}
	}

	log := RequestLog{
		Outcome:      "error",
		StatusCode:   status,
		ErrorKind:    kind,
		ErrorMessage: message,
		LatencyMs:    time.Since(m.started).Milliseconds(),
		Attempts:     len(m.failed),
		Trail:        m.trail,
	}
	if last := len(m.trail); last > 0 {
		hop := m.trail[last-1]
		log.Platform, log.ModelID, log.KeyID = hop.Platform, hop.ModelID, hop.KeyID
		log.EndpointScope = hop.EndpointScope
	} else if result != nil && result.Route != nil {
		keyID := result.Route.KeyID
		log.Platform, log.ModelID, log.KeyID = result.Route.Platform, result.Route.ModelID, &keyID
		log.EndpointScope = normalizeBaseURL(result.Route.BaseURL)
	}
	m.server.logServerEvent(ctx, serverLogRecord{
		Level: "error", Source: "gateway", Provider: log.Platform, Model: log.ModelID,
		Event: "exhausted", RequestID: m.requestID,
		Message: fmt.Sprintf("%s (%d attempt(s))", modalityFailureMessage(result, m.secrets), len(m.failed)),
	})
	if _, err := m.server.RecordRequest(ctx, log); err != nil {
		slog.Debug("Could not record failed modality request", "error", err)
	}
}

// embeddingInputBytes sums the character length of every input text, the basis
// of the token estimate when the provider reports none.
func embeddingInputBytes(inputs []string) int {
	n := 0
	for _, s := range inputs {
		n += len(s)
	}
	return n
}
