package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Generative-media transport (image, speech, transcription), ported from
// FreeLLMAPI's services/media.ts (github.com/tashfeenahmed/freellmapi, MIT).
//
// Media models live in their own media_models table, never the chat catalog, so
// a chat request can never misroute into an image model. The engine still owns
// routing, failover, quota and cooldowns; this file is pure transport - one
// chosen (platform, model, key) turned into one upstream call and its answer
// normalised into an OpenAI-shaped result. Each modality is an OPTIONAL
// interface an adapter may implement; the *compat adapter implements all three
// and dispatches per platform, so a platform with no branch reports a typed
// refusal rather than sending an OpenAI body to an endpoint that would reject
// it. Video has no adapter here on purpose: the catalog ships no video models
// and the /v1/videos/generations route refuses before it ever selects a route.

// ErrModalityUnsupported is the typed refusal an adapter returns when its
// platform has no wire for the requested modality. It is a transport-shaped
// failure (no upstream call happened), so the failover loop moves to the next
// candidate rather than surfacing it as the caller's fatal error.
var ErrModalityUnsupported = errors.New("provider has no adapter for this media modality")

// mediaFetchTimeout bounds one media upstream call. A cold FLUX/SDXL run or a
// whisper transcription can take tens of seconds, so the chat timeout floor is
// too tight (services/media.ts:184, FETCH_TIMEOUT_MS).
const mediaFetchTimeout = 60_000_000_000 // 60s in nanoseconds.

// ── Image generation ─────────────────────────────────────────────────────────

// ImageRequest is one image-generation call. Everything the OpenAI images
// surface carries that a provider can act on.
type ImageRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	ResponseFormat string
}

// ImageData is one produced image, either a hosted URL or inline base64. The
// json tags render the OpenAI images response shape directly.
type ImageData struct {
	URL     string `json:"url,omitempty"`
	B64JSON string `json:"b64_json,omitempty"`
}

// ImageResponse is the normalised set of images one generation produced.
type ImageResponse struct {
	Data []ImageData

	// Headers carries the successful upstream response headers so the API layer
	// can read the provider's rate-limit families for quota observation. Nil
	// when the wire returned none.
	Headers http.Header
}

// ImageGenerator is implemented by adapters that can serve an image request.
type ImageGenerator interface {
	Images(ctx context.Context, apiKey string, req *ImageRequest) (*ImageResponse, error)
}

// ── Speech synthesis (text to speech) ────────────────────────────────────────

// SpeechRequest is one text-to-speech call.
type SpeechRequest struct {
	Model  string
	Input  string
	Voice  string
	Format string
}

// SpeechResponse carries the raw audio bytes and the content type that
// describes them, so the route can relay them without knowing the codec.
type SpeechResponse struct {
	Audio       []byte
	ContentType string

	// Headers carries the successful upstream response headers for quota
	// observation. Nil when the wire returned none.
	Headers http.Header
}

// SpeechSynthesizer is implemented by adapters that can serve a speech request.
type SpeechSynthesizer interface {
	Speech(ctx context.Context, apiKey string, req *SpeechRequest) (*SpeechResponse, error)
}

// ── Transcription (speech to text) ───────────────────────────────────────────

// TranscriptionRequest is one speech-to-text call. File holds the whole upload
// in memory: the audio bytes never touch disk. RequestStyle rides on the
// catalog row's meta_json for platforms that host more than one deployment
// flavour (Cloudflare: "json" vs binary).
type TranscriptionRequest struct {
	Model          string
	File           []byte
	Filename       string
	MimeType       string
	Language       string
	Prompt         string
	Temperature    *float64
	ResponseFormat string // json | text | verbose_json | vtt
	RequestStyle   string // cloudflare: "json" | "" (raw binary, the default)
}

// TranscriptionResponse is the normalised transcript. VTT is populated only by
// providers that produce subtitles natively.
type TranscriptionResponse struct {
	Text     string
	Language string
	Duration float64
	Segments json.RawMessage
	VTT      string

	// Headers carries the successful upstream response headers for quota
	// observation. Nil when the wire returned none.
	Headers http.Header
}

// Transcriber is implemented by adapters that can serve a transcription request.
type Transcriber interface {
	Transcribe(ctx context.Context, apiKey string, req *TranscriptionRequest) (*TranscriptionResponse, error)
}

// ── compat implementations ───────────────────────────────────────────────────

// Images dispatches to the platform's image wire (callImageProvider,
// services/media.ts:362-472). Only the platforms the catalog actually ships
// image rows for are wired; anything else is a typed refusal.
func (c *compat) Images(ctx context.Context, apiKey string, req *ImageRequest) (*ImageResponse, error) {
	switch c.cfg.platform {
	case "custom":
		return c.openAIImages(ctx, apiKey, req)
	case "cloudflare":
		return c.cloudflareImages(ctx, apiKey, req)
	default:
		return nil, fmt.Errorf("%w: %s image", ErrModalityUnsupported, c.cfg.platform)
	}
}

// openAIImages is the OpenAI /images/generations wire an operator's own
// endpoint speaks (services/media.ts:370-382).
func (c *compat) openAIImages(ctx context.Context, apiKey string, req *ImageRequest) (*ImageResponse, error) {
	baseURL, effKey, err := c.endpoint(apiKey)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		return nil, errors.New("custom image provider is missing base_url")
	}
	body := map[string]any{"model": req.Model, "prompt": req.Prompt}
	if req.N > 0 {
		body["n"] = req.N
	}
	if req.Size != "" {
		body["size"] = req.Size
	}
	// The OpenAI images wire lets the caller choose a hosted url or inline
	// base64; dropping response_format forced every operator endpoint back to
	// its own default and ignored what the client asked for.
	if req.ResponseFormat != "" {
		body["response_format"] = req.ResponseFormat
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, baseURL+"/images/generations",
		mediaBearer(effKey), raw, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var j struct {
		Data []ImageData `json:"data"`
	}
	if err := json.Unmarshal(respBody, &j); err != nil {
		return nil, fmt.Errorf("%s: parse image response: %w", c.cfg.name, err)
	}
	return &ImageResponse{Data: j.Data, Headers: header}, nil
}

// cloudflareImages calls Cloudflare Workers AI's account-scoped native image
// endpoint (services/media.ts:422-458). FLUX returns JSON with base64 in
// result.image; SDXL returns raw PNG bytes, told apart by the content type.
func (c *compat) cloudflareImages(ctx context.Context, apiKey string, req *ImageRequest) (*ImageResponse, error) {
	account, token, err := splitCloudflareKey(apiKey)
	if err != nil {
		return nil, err
	}
	w, h := mediaParseSize(req.Size)
	// FLUX.1 Schnell's schema is prompt-only and 400s on width/height that the
	// other JSON image models accept (services/media.ts:437-441).
	var body map[string]any
	if req.Model == "@cf/black-forest-labs/flux-1-schnell" {
		body = map[string]any{"prompt": req.Prompt}
	} else {
		body = map[string]any{"prompt": req.Prompt, "width": w, "height": h}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/run/" + req.Model
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, url,
		map[string]string{"Authorization": "Bearer " + token}, raw, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	if strings.Contains(header.Get("Content-Type"), "application/json") {
		var j struct {
			Result struct {
				Image string `json:"image"`
			} `json:"result"`
		}
		if err := json.Unmarshal(respBody, &j); err != nil {
			return nil, fmt.Errorf("cloudflare: parse image response: %w", err)
		}
		if j.Result.Image == "" {
			return nil, errors.New("cloudflare returned no image")
		}
		return &ImageResponse{Data: []ImageData{{B64JSON: j.Result.Image}}, Headers: header}, nil
	}
	return &ImageResponse{Data: []ImageData{{B64JSON: base64.StdEncoding.EncodeToString(respBody)}}, Headers: header}, nil
}

// Speech dispatches to the platform's text-to-speech wire (callSpeechProvider,
// services/media.ts:606-707).
func (c *compat) Speech(ctx context.Context, apiKey string, req *SpeechRequest) (*SpeechResponse, error) {
	switch c.cfg.platform {
	case "custom":
		return c.openAISpeech(ctx, apiKey, req)
	case "cloudflare":
		return c.cloudflareSpeech(ctx, apiKey, req)
	case "google":
		return c.googleSpeech(ctx, apiKey, req)
	default:
		return nil, fmt.Errorf("%w: %s speech", ErrModalityUnsupported, c.cfg.platform)
	}
}

// openAISpeech is the OpenAI /audio/speech wire, returning raw audio bytes
// (services/media.ts:613-627).
func (c *compat) openAISpeech(ctx context.Context, apiKey string, req *SpeechRequest) (*SpeechResponse, error) {
	baseURL, effKey, err := c.endpoint(apiKey)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		return nil, errors.New("custom audio provider is missing base_url")
	}
	fmtName := req.Format
	if fmtName == "" {
		fmtName = "mp3"
	}
	body := map[string]any{"model": req.Model, "input": req.Input}
	if req.Voice != "" {
		body["voice"] = req.Voice
	}
	if req.Format != "" {
		body["response_format"] = req.Format
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, baseURL+"/audio/speech",
		mediaBearer(effKey), raw, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	ct := header.Get("Content-Type")
	if ct == "" {
		ct = audioContentType(fmtName)
	}
	return &SpeechResponse{Audio: respBody, ContentType: ct, Headers: header}, nil
}

// cloudflareSpeech calls Cloudflare's native MeloTTS deployment
// (services/media.ts:629-641). MeloTTS takes a language selector, never a
// voice, so OpenAI's default `alloy` must not be forwarded as `lang`.
func (c *compat) cloudflareSpeech(ctx context.Context, apiKey string, req *SpeechRequest) (*SpeechResponse, error) {
	// MeloTTS emits MP3 and nothing else, so honouring any other requested
	// response_format would hand back a codec the caller did not ask for.
	if err := enforceFixedSpeechFormat(c.cfg.name, req.Format, "mp3"); err != nil {
		return nil, err
	}
	account, token, err := splitCloudflareKey(apiKey)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"prompt": req.Input, "lang": "en"})
	if err != nil {
		return nil, err
	}
	url := "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/run/" + req.Model
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, url,
		map[string]string{"Authorization": "Bearer " + token}, raw, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var j struct {
		Result struct {
			Audio string `json:"audio"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &j); err != nil {
		return nil, fmt.Errorf("cloudflare: parse audio response: %w", err)
	}
	if j.Result.Audio == "" {
		return nil, errors.New("cloudflare returned no audio")
	}
	audio, err := base64.StdEncoding.DecodeString(j.Result.Audio)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: decode audio: %w", err)
	}
	return &SpeechResponse{Audio: audio, ContentType: "audio/mpeg", Headers: header}, nil
}

// googleSpeech calls Gemini TTS via generateContent (services/media.ts:676-702).
// It returns base64 PCM (L16 mono, rate declared in the part's mime type),
// wrapped in a WAV header so a client can play it without knowing the rate.
func (c *compat) googleSpeech(ctx context.Context, apiKey string, req *SpeechRequest) (*SpeechResponse, error) {
	// Gemini TTS returns PCM this adapter wraps into a WAV container and nothing
	// else, so a different requested response_format cannot be honoured.
	if err := enforceFixedSpeechFormat(c.cfg.name, req.Format, "wav"); err != nil {
		return nil, err
	}
	reqBody := map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": req.Input}}}},
		"generationConfig": map[string]any{
			"responseModalities": []any{"AUDIO"},
		},
	}
	if v := geminiVoice(req.Voice); v != "" {
		reqBody["generationConfig"].(map[string]any)["speechConfig"] = map[string]any{
			"voiceConfig": map[string]any{
				"prebuiltVoiceConfig": map[string]any{"voiceName": v},
			},
		}
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + req.Model + ":generateContent"
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, url,
		map[string]string{"x-goog-api-key": apiKey}, raw, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var j struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						Data     string `json:"data"`
						MimeType string `json:"mimeType"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &j); err != nil {
		return nil, fmt.Errorf("gemini: parse audio response: %w", err)
	}
	for _, cand := range j.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData.Data == "" {
				continue
			}
			pcm, derr := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if derr != nil {
				return nil, fmt.Errorf("gemini: decode audio: %w", derr)
			}
			rate := parseSampleRate(part.InlineData.MimeType)
			if rate == 0 {
				rate = 24000
			}
			return &SpeechResponse{Audio: wrapPCMAsWAV(pcm, rate), ContentType: "audio/wav", Headers: header}, nil
		}
	}
	return nil, errors.New("gemini returned no audio")
}

// Transcribe dispatches to the platform's speech-to-text wire
// (callTranscriptionProvider, services/media.ts:949-1044).
func (c *compat) Transcribe(ctx context.Context, apiKey string, req *TranscriptionRequest) (*TranscriptionResponse, error) {
	switch c.cfg.platform {
	case "custom":
		return c.openAITranscribe(ctx, apiKey, c.cfg.baseURL, req)
	case "groq":
		return c.openAITranscribe(ctx, apiKey, "https://api.groq.com/openai/v1", req)
	case "cloudflare":
		return c.cloudflareTranscribe(ctx, apiKey, req)
	default:
		return nil, fmt.Errorf("%w: %s transcription", ErrModalityUnsupported, c.cfg.platform)
	}
}

// openAITranscribe is the OpenAI /audio/transcriptions multipart wire that Groq
// and any operator STT endpoint (faster-whisper-server, LocalAI, vLLM) speak
// (services/media.ts:956-1000). verbose_json is forwarded; every other format
// is derived locally from the json shape so failover output stays uniform.
func (c *compat) openAITranscribe(ctx context.Context, apiKey, baseURL string, req *TranscriptionRequest) (*TranscriptionResponse, error) {
	if baseURL == "" {
		return nil, errors.New("custom transcription provider is missing base_url")
	}
	wire := "json"
	if req.ResponseFormat == "verbose_json" {
		wire = "verbose_json"
	}
	body, contentType, err := buildTranscriptionForm(req, wire)
	if err != nil {
		return nil, err
	}
	// FormData supplies the multipart boundary; never set Content-Type by hand
	// beyond the boundary the writer produced.
	headers := mediaBearer(apiKey)
	headers["Content-Type"] = contentType
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, baseURL+"/audio/transcriptions",
		headers, body, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var j struct {
		Text     string          `json:"text"`
		Language string          `json:"language"`
		Duration float64         `json:"duration"`
		Segments json.RawMessage `json:"segments"`
	}
	if err := json.Unmarshal(respBody, &j); err != nil {
		return nil, fmt.Errorf("%s: parse transcription response: %w", c.cfg.name, err)
	}
	if j.Text == "" {
		return nil, errors.New("transcription endpoint returned no text")
	}
	return &TranscriptionResponse{Text: j.Text, Language: j.Language, Duration: j.Duration, Segments: j.Segments, Headers: header}, nil
}

// cloudflareTranscribe calls Cloudflare's native whisper deployment
// (services/media.ts:1002-1040). "json" style sends base64 audio in a JSON
// body; the default binary style sends the raw bytes. Whisper returns text and
// optionally native VTT subtitles.
func (c *compat) cloudflareTranscribe(ctx context.Context, apiKey string, req *TranscriptionRequest) (*TranscriptionResponse, error) {
	account, token, err := splitCloudflareKey(apiKey)
	if err != nil {
		return nil, err
	}
	url := "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/run/" + req.Model
	var (
		reqBody []byte
		headers map[string]string
	)
	if req.RequestStyle == "json" {
		payload := map[string]any{"audio": base64.StdEncoding.EncodeToString(req.File)}
		if req.Language != "" {
			payload["language"] = req.Language
		}
		if req.Prompt != "" {
			payload["initial_prompt"] = req.Prompt
		}
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		headers = map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token}
	} else {
		reqBody = req.File
		headers = map[string]string{"Content-Type": "application/octet-stream", "Authorization": "Bearer " + token}
	}
	status, respBody, header, err := c.httpCall(ctx, http.MethodPost, url, headers, reqBody, mediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, httpErrorFrom(status, header, respBody)
	}
	var j struct {
		Result struct {
			Text              string          `json:"text"`
			VTT               string          `json:"vtt"`
			Segments          json.RawMessage `json:"segments"`
			TranscriptionInfo struct {
				Language string  `json:"language"`
				Duration float64 `json:"duration"`
			} `json:"transcription_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &j); err != nil {
		return nil, fmt.Errorf("cloudflare: parse transcription response: %w", err)
	}
	if j.Result.Text == "" {
		return nil, errors.New("cloudflare returned no transcription text")
	}
	return &TranscriptionResponse{
		Text:     j.Result.Text,
		Language: j.Result.TranscriptionInfo.Language,
		Duration: j.Result.TranscriptionInfo.Duration,
		Segments: j.Result.Segments,
		VTT:      j.Result.VTT,
		Headers:  header,
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// mediaBearer builds the Authorization header, falling back to the "no-key"
// sentinel a keyless custom endpoint expects (services/media.ts:377,974).
func mediaBearer(key string) map[string]string {
	if key == "" {
		key = "no-key"
	}
	return map[string]string{"Authorization": "Bearer " + key}
}

// enforceFixedSpeechFormat refuses a speech request whose requested output
// format a fixed-codec deployment cannot produce. A native TTS wire (Cloudflare
// MeloTTS emits MP3, Gemini emits WAV) has exactly one output container;
// honouring a different response_format by returning that fixed codec anyway
// would hand the caller bytes whose format does not match what it asked for.
// The refusal is transport-shaped (ErrModalityUnsupported), so the failover
// loop moves to a provider that can produce the requested format instead of
// surfacing it as the caller's fatal error. An empty request format defers to
// the adapter's native output and is always accepted.
func enforceFixedSpeechFormat(name, requested, fixed string) error {
	if requested == "" || strings.EqualFold(requested, fixed) {
		return nil
	}
	return fmt.Errorf("%w: %s speech only produces %s, not %s",
		ErrModalityUnsupported, name, fixed, requested)
}

// splitCloudflareKey splits the compound "account_id:token" credential, the
// same shape the chat adapter's cloudflareEndpoint expects (cloudflare.ts).
func splitCloudflareKey(apiKey string) (account, token string, err error) {
	i := strings.IndexByte(apiKey, ':')
	if i < 0 {
		return "", "", errors.New(`Cloudflare key must be in format "account_id:api_token"`)
	}
	return apiKey[:i], apiKey[i+1:], nil
}

// mediaParseSize reads a "WxH" size, defaulting to 1024x1024
// (services/media.ts:276-282).
func mediaParseSize(size string) (int, int) {
	if size != "" {
		if parts := strings.SplitN(size, "x", 2); len(parts) == 2 {
			w, werr := strconv.Atoi(parts[0])
			h, herr := strconv.Atoi(parts[1])
			if werr == nil && herr == nil && w > 0 && h > 0 {
				return w, h
			}
		}
	}
	return 1024, 1024
}

// audioContentType maps a response_format to its MIME type
// (services/media.ts:321-331).
func audioContentType(fmtName string) string {
	switch fmtName {
	case "wav":
		return "audio/wav"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "pcm":
		return "audio/L16"
	default:
		return "audio/mpeg"
	}
}

var sampleRateRe = regexp.MustCompile(`rate=(\d+)`)

// parseSampleRate reads the "rate=" hint from a PCM mime type
// (services/media.ts:333-336). Zero means the caller should apply its default.
func parseSampleRate(mime string) int {
	if m := sampleRateRe.FindStringSubmatch(mime); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// geminiOpenAIVoiceMap translates the OpenAI TTS voice names clients send by
// default onto Gemini's prebuilt voices, so an ordinary OpenAI request does not
// fail on an unknown voice (services/media.ts:135-149).
var geminiOpenAIVoiceMap = map[string]string{
	"alloy": "Kore", "echo": "Puck", "fable": "Charon", "onyx": "Enceladus",
	"nova": "Aoede", "shimmer": "Leda",
}

// geminiVoice resolves the voice Gemini should speak in: a recognised OpenAI
// name is mapped, anything else passes through, and an empty request gets
// Gemini's default (services/media.ts:175-181).
func geminiVoice(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "Kore"
	}
	if mapped, ok := geminiOpenAIVoiceMap[strings.ToLower(requested)]; ok {
		return mapped
	}
	return requested
}

// wrapPCMAsWAV prepends a 44-byte WAV header to raw 16-bit mono PCM
// (services/media.ts:340-360).
func wrapPCMAsWAV(pcm []byte, sampleRate int) []byte {
	const numChannels, bitsPerSample = 1, 16
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	buf := new(bytes.Buffer)
	buf.Grow(44 + len(pcm))
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(numChannels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}

// buildTranscriptionForm renders the multipart upload the OpenAI STT wire
// expects. The returned content type carries the boundary the writer chose.
func buildTranscriptionForm(req *TranscriptionRequest, responseFormat string) ([]byte, string, error) {
	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)

	filename := req.Filename
	if filename == "" {
		filename = "audio"
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(req.File); err != nil {
		return nil, "", err
	}
	fields := [][2]string{{"model", req.Model}, {"response_format", responseFormat}}
	if req.Language != "" {
		fields = append(fields, [2]string{"language", req.Language})
	}
	if req.Prompt != "" {
		fields = append(fields, [2]string{"prompt", req.Prompt})
	}
	if req.Temperature != nil {
		fields = append(fields, [2]string{"temperature", strconv.FormatFloat(*req.Temperature, 'g', -1, 64)})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
