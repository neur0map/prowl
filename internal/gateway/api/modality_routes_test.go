package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

const modalityMachineKey = "prowl-test-modality-key"

// fakeModality stands in for an OpenAI-compatible provider serving the non-chat
// endpoints. One server answers every modality path so a single fake can back
// any surface; a non-200 status turns every path into a failure, which is how
// the failover and exhaustion tests force a hop.
type fakeModality struct {
	*httptest.Server
	calls  atomic.Int64
	status int
}

func newFakeModality(t *testing.T, status int) *fakeModality {
	t.Helper()
	f := &fakeModality{status: status}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.status != 0 && f.status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"upstream says %d"}}`, f.status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
		case strings.HasSuffix(r.URL.Path, "/images/generations"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"url":"https://example.test/img-1.png"}]}`)
		case strings.HasSuffix(r.URL.Path, "/audio/speech"):
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("ID3fake-audio-bytes"))
		case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"hello from whisper","language":"en","duration":1.5}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func addCustomModalityKey(t *testing.T, s *Server, label, baseURL string) int64 {
	t.Helper()
	keyID, err := s.engine.Vault().Add("custom", "test-key-"+label, gateway.AddOptions{
		Label:   label,
		BaseURL: baseURL,
	})
	require.NoError(t, err)
	return keyID
}

// seedEmbeddingRoute registers a custom-endpoint key and an embedding_models
// row bound to it, aimed at a fake upstream.
func seedEmbeddingRoute(t *testing.T, s *Server, label, baseURL, family, modelID string, priority int) {
	t.Helper()
	keyID := addCustomModalityKey(t, s, label, baseURL)
	_, err := s.engine.DB().Exec(
		`INSERT INTO embedding_models (family, platform, model_id, display_name, priority, enabled, key_id)
		 VALUES (?, 'custom', ?, ?, ?, 1, ?)`,
		family, modelID, modelID, priority, keyID)
	require.NoError(t, err)
}

// seedMediaRoute registers a custom-endpoint key and a media_models row bound
// to it, for one modality.
func seedMediaRoute(t *testing.T, s *Server, label, baseURL, modality, modelID string, priority int) {
	t.Helper()
	keyID := addCustomModalityKey(t, s, label, baseURL)
	_, err := s.engine.DB().Exec(
		`INSERT INTO media_models (platform, model_id, display_name, modality, priority, enabled, key_id)
		 VALUES ('custom', ?, ?, ?, ?, 1, ?)`,
		modelID, modelID, modality, priority, keyID)
	require.NoError(t, err)
}

func postModalityJSON(t *testing.T, s *Server, path, key, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func postTranscription(t *testing.T, s *Server, key string, fields map[string]string, filename string, data []byte) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Result(), rec.Body.String()
}

func latestRequestRow(t *testing.T, s *Server) (platform, outcome, endpointScope string, status int) {
	t.Helper()
	err := s.engine.DB().QueryRow(
		`SELECT platform, outcome, endpoint_scope, status FROM requests ORDER BY id DESC LIMIT 1`,
	).Scan(&platform, &outcome, &endpointScope, &status)
	require.NoError(t, err)
	return
}

// TestEmbeddingsRouteReturnsVectorsAndRecordsUsage is the end-to-end proof the
// embeddings surface is wired: parsed, routed through the failover loop to a
// real adapter, shaped into the OpenAI list envelope, and recorded on the same
// trail the dashboard reads with the custom endpoint's scope preserved.
func TestEmbeddingsRouteReturnsVectorsAndRecordsUsage(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedEmbeddingRoute(t, s, "only", upstream.URL, "e5", "e5-small", 1)

	resp, body := postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"e5","input":"embed this"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), upstream.calls.Load())

	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Equal(t, "list", parsed.Object)
	require.Len(t, parsed.Data, 1)
	require.Equal(t, "embedding", parsed.Data[0].Object)
	require.Equal(t, []float64{0.1, 0.2, 0.3}, parsed.Data[0].Embedding)
	require.Equal(t, 4, parsed.Usage.PromptTokens, "the provider-reported prompt tokens must be relayed")
	require.Contains(t, resp.Header.Get("X-Routed-Via"), "custom")

	platform, outcome, scope, status := latestRequestRow(t, s)
	require.Equal(t, "custom", platform)
	require.Equal(t, "success", outcome)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, upstream.URL, scope, "the custom endpoint identity must be recorded, not collapsed")
}

// TestEmbeddingsBase64EncodingFormat proves encoding_format is honoured, not
// ignored: a client asking for base64 receives each vector as a base64 string
// of the little-endian float32 bytes, never a silently-substituted float array.
func TestEmbeddingsBase64EncodingFormat(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedEmbeddingRoute(t, s, "b64", upstream.URL, "e5", "e5-small", 1)

	resp, body := postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"e5","input":"embed this","encoding_format":"base64"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)

	var parsed struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed),
		"the embedding must decode as a base64 string, not a number array: %s", body)
	require.Len(t, parsed.Data, 1)

	raw, err := base64.StdEncoding.DecodeString(parsed.Data[0].Embedding)
	require.NoError(t, err, "the embedding string must be valid base64")

	want := []float32{0.1, 0.2, 0.3}
	require.Len(t, raw, 4*len(want))
	for i, f := range want {
		bits := binary.LittleEndian.Uint32(raw[i*4:])
		require.Equal(t, f, math.Float32frombits(bits),
			"vector element %d must be the little-endian float32 bytes", i)
	}
}

// TestEmbeddingsRejectsInvalidEncodingFormat proves the field is validated
// rather than passed through: an unsupported value is a client error, not a
// route to a provider.
func TestEmbeddingsRejectsInvalidEncodingFormat(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedEmbeddingRoute(t, s, "only", upstream.URL, "e5", "e5-small", 1)

	resp, body := postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"e5","input":"embed this","encoding_format":"gzip"}`)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
	require.Contains(t, body, "encoding_format")
	require.Equal(t, int64(0), upstream.calls.Load(), "an invalid request must not reach a provider")
}

// TestModalityEndpointsRequireAuth proves every non-chat surface is behind the
// machine-credential gate, and that a rejection never carries authentication_
// error, which would sign a dashboard operator out.
func TestModalityEndpointsRequireAuth(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})

	cases := []struct {
		path, body string
	}{
		{"/v1/embeddings", `{"model":"e5","input":"x"}`},
		{"/v1/images/generations", `{"model":"auto","prompt":"a cat"}`},
		{"/v1/audio/speech", `{"model":"auto","input":"hi"}`},
	}
	for _, c := range cases {
		resp, body := postModalityJSON(t, s, c.path, "", c.body)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s must be gated: %s", c.path, body)
		require.NotEqual(t, string(TypeAuthentication), errorType(t, body),
			"%s: a gate refusal must not end a browser session", c.path)
	}

	// The multipart transcription surface is gated on the same credential.
	resp, body := postTranscription(t, s, "", map[string]string{"model": "auto"}, "a.wav", []byte("RIFFfake"))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "body was %s", body)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
}

// TestEmbeddingsFailOverToSibling proves the embeddings surface drives the real
// failover loop: the first candidate rate-limits, the second answers, and the
// caller is told a hop happened without ever seeing the failure.
func TestEmbeddingsFailOverToSibling(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	// A 429 is scoped to the (model, key) that hit it, so the sibling stays
	// eligible for the same logical family.
	broken := newFakeModality(t, http.StatusTooManyRequests)
	working := newFakeModality(t, http.StatusOK)
	seedEmbeddingRoute(t, s, "broken", broken.URL, "e5", "e5-a", 1)
	seedEmbeddingRoute(t, s, "working", working.URL, "e5", "e5-b", 2)

	resp, body := postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"e5","input":"embed this"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "a failing candidate must not reach the caller: %s", body)
	require.GreaterOrEqual(t, broken.calls.Load(), int64(1), "the first candidate must have been tried")
	require.Equal(t, int64(1), working.calls.Load(), "the sibling must have served it")
	require.Equal(t, "1", resp.Header.Get("X-Fallback-Attempts"))
	require.NotEmpty(t, resp.Header.Get("X-Fallback-Trail"))
}

// TestEmbeddingsUnavailableSeparatesTheReasons pins the honest first-run
// answers: a gateway with no embedding models says so, and a named model the
// catalogue does not carry is a 404 rather than a generic outage.
func TestEmbeddingsUnavailableSeparatesTheReasons(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})

	// No embedding models at all.
	resp, body := postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"e5","input":"x"}`)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "body was %s", body)
	require.Contains(t, strings.ToLower(body), "no embedding models")

	// A model the catalogue does not know is a not-found, not an outage.
	seedEmbeddingRoute(t, s, "known", newFakeModality(t, http.StatusOK).URL, "e5", "e5-small", 1)
	resp, body = postModalityJSON(t, s, "/v1/embeddings", modalityMachineKey,
		`{"model":"does-not-exist","input":"x"}`)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "body was %s", body)
	require.Equal(t, string(TypeNotFound), errorType(t, body))
}

// TestImageGenerationReturnsData proves the image surface routes to the
// ImageGenerator adapter and renders the OpenAI images envelope.
func TestImageGenerationReturnsData(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedMediaRoute(t, s, "img", upstream.URL, "image", "sdxl", 1)

	resp, body := postModalityJSON(t, s, "/v1/images/generations", modalityMachineKey,
		`{"model":"sdxl","prompt":"a red bicycle","n":1}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), upstream.calls.Load())

	var parsed struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Len(t, parsed.Data, 1)
	require.Equal(t, "https://example.test/img-1.png", parsed.Data[0].URL)

	_, outcome, _, _ := latestRequestRow(t, s)
	require.Equal(t, "success", outcome, "an image call must be recorded")
}

// TestSpeechReturnsBinaryAudio proves the speech surface relays the provider's
// audio bytes and content type unchanged rather than wrapping them in JSON.
func TestSpeechReturnsBinaryAudio(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedMediaRoute(t, s, "tts", upstream.URL, "audio", "tts-1", 1)

	resp, body := postModalityJSON(t, s, "/v1/audio/speech", modalityMachineKey,
		`{"model":"tts-1","input":"hello there","voice":"alloy","response_format":"mp3"}`)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))
	require.Equal(t, "ID3fake-audio-bytes", body, "the raw audio bytes must be relayed unchanged")
	require.Equal(t, int64(1), upstream.calls.Load())
}

// TestTranscriptionReturnsText proves the multipart upload is parsed, routed to
// the Transcriber adapter, and rendered as the default JSON transcript shape.
func TestTranscriptionReturnsText(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedMediaRoute(t, s, "stt", upstream.URL, "transcription", "whisper-1", 1)

	resp, body := postTranscription(t, s, modalityMachineKey,
		map[string]string{"model": "whisper-1"}, "clip.wav", []byte("RIFFfake-audio"))

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Equal(t, int64(1), upstream.calls.Load())

	var parsed struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	require.Equal(t, "hello from whisper", parsed.Text)

	_, outcome, _, _ := latestRequestRow(t, s)
	require.Equal(t, "success", outcome)
}

// TestTranscriptionRequiresFile proves the upload is bounded: a request with no
// audio part is a client error, not a route to an adapter.
func TestTranscriptionRequiresFile(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	seedMediaRoute(t, s, "stt", newFakeModality(t, http.StatusOK).URL, "transcription", "whisper-1", 1)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("model", "whisper-1"))
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+modalityMachineKey)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Result().StatusCode, "body was %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "file is required")
}

// TestModalityExhaustionIsNotA500AndIsRecorded proves that when every candidate
// fails, the surface names an upstream failure (never its own 500) and leaves a
// diagnosable error row rather than an empty logs page.
func TestModalityExhaustionIsNotA500AndIsRecorded(t *testing.T) {
	t.Parallel()

	s := testServer(t, Options{MachineKey: modalityMachineKey})
	down := newFakeModality(t, http.StatusInternalServerError)
	alsoDown := newFakeModality(t, http.StatusInternalServerError)
	seedMediaRoute(t, s, "img-a", down.URL, "image", "img-a", 1)
	seedMediaRoute(t, s, "img-b", alsoDown.URL, "image", "img-b", 2)

	resp, body := postModalityJSON(t, s, "/v1/images/generations", modalityMachineKey,
		`{"model":"auto","prompt":"a red bicycle"}`)

	require.NotEqual(t, http.StatusInternalServerError, resp.StatusCode,
		"an upstream failure is never the gateway's own 500: %s", body)
	require.GreaterOrEqual(t, resp.StatusCode, 400)
	require.NotEqual(t, string(TypeAuthentication), errorType(t, body))
	require.GreaterOrEqual(t, down.calls.Load(), int64(1))

	platform, outcome, _, _ := latestRequestRow(t, s)
	require.Equal(t, "error", outcome, "an exhausted modality run must be recorded")
	require.Equal(t, "custom", platform)
}

// ── Round-2 helpers ───────────────────────────────────────────────────────────

// quotaRemaining reads the observed requests-remaining a pool's state row
// carries, reporting whether an observation was recorded at all. A miss means
// no observation landed - distinct from a recorded zero.
func quotaRemaining(t *testing.T, s *Server, platform string, keyID int64) (int64, bool) {
	t.Helper()
	var rem sql.NullInt64
	err := s.engine.DB().QueryRow(
		`SELECT requests_remaining FROM provider_quota_state WHERE quota_pool_key = ?`,
		gateway.PoolKey(platform, keyID)).Scan(&rem)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	if !rem.Valid {
		return 0, false
	}
	return rem.Int64, true
}

// newFakeSpeech is an upstream that answers /v1/audio/speech with a fixed
// content type and audio body, so a test can force the served codec to match or
// mismatch the requested one.
func newFakeSpeech(t *testing.T, contentType string, audio []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(audio)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeModalityProvider is an in-process provider.Provider that also satisfies
// the three modality capability interfaces, so a relay's call() can be driven
// with a stub that captures its request or dictates its response without a real
// upstream. Only the funcs a test sets are exercised.
type fakeModalityProvider struct {
	transcribe func(*provider.TranscriptionRequest) (*provider.TranscriptionResponse, error)
	embed      func(*provider.EmbeddingRequest) (*provider.EmbeddingResponse, error)
	speak      func(*provider.SpeechRequest) (*provider.SpeechResponse, error)
}

func (f *fakeModalityProvider) Platform() string { return "fake" }
func (f *fakeModalityProvider) Name() string     { return "fake" }
func (f *fakeModalityProvider) BaseURL() string  { return "" }
func (f *fakeModalityProvider) Keyless() bool    { return false }

func (f *fakeModalityProvider) ChatCompletion(context.Context, string, *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("chat not used")
}

func (f *fakeModalityProvider) StreamChatCompletion(context.Context, string, *provider.ChatRequest) (provider.ChatStream, error) {
	return nil, errors.New("chat not used")
}

func (f *fakeModalityProvider) ValidateKey(context.Context, string) provider.KeyValidationResult {
	return provider.Inconclusive("not used")
}

func (f *fakeModalityProvider) Transcribe(_ context.Context, _ string, req *provider.TranscriptionRequest) (*provider.TranscriptionResponse, error) {
	return f.transcribe(req)
}

func (f *fakeModalityProvider) Embeddings(_ context.Context, _ string, req *provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return f.embed(req)
}

func (f *fakeModalityProvider) Speech(_ context.Context, _ string, req *provider.SpeechRequest) (*provider.SpeechResponse, error) {
	return f.speak(req)
}

// ── Round-2 tests ─────────────────────────────────────────────────────────────

// TestModalityBareQuotaDepletionIsObserved proves a 429/402 depletes the pool
// even with no rate-limit headers: the status is itself the signal. It also
// pins the boundary - a headerless success must NOT fabricate a zero - and that
// the relay's error-path observer forwards a bare 429 too.
func TestModalityBareQuotaDepletionIsObserved(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})

	s.observeQuotaHeaders(gateway.Route{Platform: "custom", ModelID: "m", KeyID: 7},
		http.StatusTooManyRequests, http.Header{})
	rem, ok := quotaRemaining(t, s, "custom", 7)
	require.True(t, ok, "a bare 429 must record a quota observation")
	require.Equal(t, int64(0), rem, "a bare 429 depletes the pool to zero")

	s.observeQuotaHeaders(gateway.Route{Platform: "custom", ModelID: "m", KeyID: 8},
		http.StatusPaymentRequired, http.Header{})
	rem, ok = quotaRemaining(t, s, "custom", 8)
	require.True(t, ok, "a bare 402 must record a quota observation")
	require.Equal(t, int64(0), rem)

	s.observeQuotaHeaders(gateway.Route{Platform: "custom", ModelID: "m", KeyID: 9},
		http.StatusOK, http.Header{})
	_, ok = quotaRemaining(t, s, "custom", 9)
	require.False(t, ok, "a headerless success must leave no observation, not a fabricated zero")

	relay := s.newModalityRelay(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil), kindSpeech, "req", nil)
	relay.observeQuota(gateway.Route{Platform: "custom", ModelID: "m", KeyID: 10},
		&provider.HTTPError{Status: http.StatusTooManyRequests})
	rem, ok = quotaRemaining(t, s, "custom", 10)
	require.True(t, ok, "the relay must observe a bare 429 carried on a failed attempt")
	require.Equal(t, int64(0), rem)
}

// TestMediaCandidatesLoadRequestStyle proves a media row's meta_json
// requestStyle is loaded into the candidate, and a row without meta_json
// degrades to the empty (binary) default rather than failing.
func TestMediaCandidatesLoadRequestStyle(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})

	keyID := addCustomModalityKey(t, s, "cf", "https://example.test")
	_, err := s.engine.DB().Exec(
		`INSERT INTO media_models (platform, model_id, display_name, modality, priority, enabled, key_id, meta_json)
		 VALUES ('custom', 'whisper-json', 'whisper-json', 'transcription', 1, 1, ?, ?)`,
		keyID, `{"subtitleFormats":["vtt"],"requestStyle":"json"}`)
	require.NoError(t, err)

	cands, err := mediaCandidates(context.Background(), s.engine.DB(), "transcription", "whisper-json")
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, "json", cands[0].RequestStyle, "meta_json.requestStyle must load into the candidate")

	seedMediaRoute(t, s, "bin", "https://example.test", "transcription", "whisper-bin", 2)
	cands, err = mediaCandidates(context.Background(), s.engine.DB(), "transcription", "whisper-bin")
	require.NoError(t, err)
	require.Len(t, cands, 1)
	require.Equal(t, "", cands[0].RequestStyle, "a row with no meta_json defaults to the binary style")
}

// TestTranscriptionPropagatesRequestStyle proves the candidate's requestStyle
// rides into the transcription request, which is what makes Cloudflare's JSON
// deployment receive base64-in-JSON rather than raw bytes it cannot parse.
func TestTranscriptionPropagatesRequestStyle(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})

	var captured *provider.TranscriptionRequest
	fake := &fakeModalityProvider{
		transcribe: func(req *provider.TranscriptionRequest) (*provider.TranscriptionResponse, error) {
			captured = req
			return &provider.TranscriptionResponse{Text: "ok"}, nil
		},
	}
	relay := s.newModalityRelay(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil),
		kindTranscription, "req",
		[]modalityCandidate{{ModelDBID: 42, KeyID: 7, Platform: "cloudflare", ModelID: "@cf/whisper", RequestStyle: "json"}})
	relay.transcription = &transcriptionRequestBody{Model: "@cf/whisper", File: []byte("audio"), ResponseFormat: "json"}

	route := gateway.Route{ModelDBID: 42, KeyID: 7, Platform: "cloudflare", ModelID: "@cf/whisper"}
	out, err := relay.call(context.Background(), fake, "acct:token", route)
	require.NoError(t, err)
	require.NotNil(t, out.write)
	require.NotNil(t, captured)
	require.Equal(t, "json", captured.RequestStyle,
		"the candidate's requestStyle must propagate to the transcription request")
}

// TestTranscriptionRendersSRTAndRejectsUnknownFormat proves SRT is rendered as
// a real SubRip document rather than silently answered as JSON, that VTT still
// works, and that an unimplemented format is a client error - never a silent
// JSON fallback that hands the caller the wrong shape.
func TestTranscriptionRendersSRTAndRejectsUnknownFormat(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedMediaRoute(t, s, "stt", upstream.URL, "transcription", "whisper-1", 1)

	resp, body := postTranscription(t, s, modalityMachineKey,
		map[string]string{"model": "whisper-1", "response_format": "srt"}, "clip.wav", []byte("RIFFfake"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, resp.Header.Get("Content-Type"), "subrip")
	require.True(t, strings.HasPrefix(body, "1\n"), "SRT opens with a cue index: %q", body)
	require.Contains(t, body, "00:00:00,000 --> 00:00:01,500",
		"SRT uses comma millisecond separators and the reported duration")
	require.Contains(t, body, "hello from whisper")

	resp, body = postTranscription(t, s, modalityMachineKey,
		map[string]string{"model": "whisper-1", "response_format": "vtt"}, "clip.wav", []byte("RIFFfake"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.True(t, strings.HasPrefix(body, "WEBVTT"), "VTT must still render a WebVTT document: %q", body)

	callsBefore := upstream.calls.Load()
	resp, body = postTranscription(t, s, modalityMachineKey,
		map[string]string{"model": "whisper-1", "response_format": "ass"}, "clip.wav", []byte("RIFFfake"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
	require.Contains(t, body, "response_format")
	require.Equal(t, callsBefore, upstream.calls.Load(), "an unimplemented format must not reach a provider")
}

// TestModalitySuccessObservesResponseHeaders proves a served modality response
// is a quota reading too: call() captures the upstream response headers, and the
// success path hands them to the ledger, which records the remaining budget just
// as it would from the 429 that finally exhausts the pool.
func TestModalitySuccessObservesResponseHeaders(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})

	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "100")
	h.Set("x-ratelimit-remaining-requests", "42")
	fake := &fakeModalityProvider{
		embed: func(*provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
			return &provider.EmbeddingResponse{Vectors: [][]float64{{0.1, 0.2}}, Headers: h}, nil
		},
	}
	relay := s.newModalityRelay(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil),
		kindEmbeddings, "req",
		[]modalityCandidate{{ModelDBID: 9, KeyID: 7, Platform: "groq", ModelID: "e5"}})
	relay.embedding = &embeddingRequestBody{Model: "e5", Input: []string{"hello"}}

	route := gateway.Route{ModelDBID: 9, KeyID: 7, Platform: "groq", ModelID: "e5"}
	out, err := relay.call(context.Background(), fake, "key", route)
	require.NoError(t, err)
	require.Equal(t, "42", out.headers.Get("x-ratelimit-remaining-requests"),
		"the successful upstream response headers must be captured for observation")

	s.observeQuotaHeaders(route, http.StatusOK, out.headers)
	rem, ok := quotaRemaining(t, s, "groq", 7)
	require.True(t, ok, "a served response's rate-limit headers must be observed")
	require.Equal(t, int64(42), rem)
}

// TestSpeechRejectsUnknownCodecAtBoundary proves a universally-invalid codec is
// refused before any provider is tried: the boundary knows the OpenAI codec set
// even though which provider can produce which codec is left to failover.
func TestSpeechRejectsUnknownCodecAtBoundary(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeModality(t, http.StatusOK)
	seedMediaRoute(t, s, "tts", upstream.URL, "audio", "tts-1", 1)

	resp, body := postModalityJSON(t, s, "/v1/audio/speech", modalityMachineKey,
		`{"model":"tts-1","input":"hi","response_format":"banana"}`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Equal(t, string(TypeInvalidRequest), errorType(t, body))
	require.Contains(t, body, "response_format")
	require.Equal(t, int64(0), upstream.calls.Load(), "a universally-invalid codec must not reach a provider")
}

// TestSpeechCodecMismatchNeverReturns200 proves a provider that answers in a
// different codec than requested never yields a misleading 200 carrying the
// wrong bytes: with no provider able to produce the codec, the run fails and
// names it instead.
func TestSpeechCodecMismatchNeverReturns200(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})
	upstream := newFakeSpeech(t, "audio/mpeg", []byte("ID3mp3-bytes"))
	seedMediaRoute(t, s, "mp3only", upstream.URL, "audio", "tts-1", 1)

	resp, body := postModalityJSON(t, s, "/v1/audio/speech", modalityMachineKey,
		`{"model":"tts-1","input":"hi","response_format":"opus"}`)
	require.NotEqual(t, http.StatusOK, resp.StatusCode,
		"an mp3 answer to an opus request must never be a 200: %s", body)
	require.NotEqual(t, "audio/mpeg", resp.Header.Get("Content-Type"))
	require.NotContains(t, body, "ID3mp3-bytes", "the wrong-codec bytes must not be relayed as the answer")
	require.Contains(t, strings.ToLower(body), "opus", "the failure must name the requested codec")
}

// TestSpeechCodecMismatchFailsOverToMatchingProvider proves the refusal is a
// failover, not a dead end: the mp3-only provider's mismatch is one hop, and the
// opus-capable sibling serves the request.
func TestSpeechCodecMismatchFailsOverToMatchingProvider(t *testing.T) {
	t.Parallel()
	s := testServer(t, Options{MachineKey: modalityMachineKey})
	wrong := newFakeSpeech(t, "audio/mpeg", []byte("mp3-bytes"))
	right := newFakeSpeech(t, "audio/ogg", []byte("opus-bytes"))
	seedMediaRoute(t, s, "mp3only", wrong.URL, "audio", "tts-a", 1)
	seedMediaRoute(t, s, "opuscap", right.URL, "audio", "tts-b", 2)

	resp, body := postModalityJSON(t, s, "/v1/audio/speech", modalityMachineKey,
		`{"model":"auto","input":"hi","response_format":"opus"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the opus-capable sibling must serve it: %s", body)
	require.Equal(t, "audio/ogg", resp.Header.Get("Content-Type"))
	require.Equal(t, "opus-bytes", body)
	require.Equal(t, "1", resp.Header.Get("X-Fallback-Attempts"),
		"the mp3 provider's codec mismatch must count as one failover hop")
}
