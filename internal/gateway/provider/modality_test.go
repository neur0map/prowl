package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// customCompatAt builds a plain OpenAI-compatible adapter bound to a test
// server, the same shape Resolve("custom", url) produces, so a modality handler
// can be exercised against a captured upstream.
func customCompatAt(url string, client *http.Client) *compat {
	return &compat{
		cfg:    providerCfg{platform: "custom", name: "Custom", baseURL: url},
		client: client,
	}
}

// TestOpenAIImagesForwardsResponseFormat proves the client's response_format
// reaches the upstream images wire - dropping it forced every operator endpoint
// back to its own default - and that the successful response headers are carried
// back for quota observation.
func TestOpenAIImagesForwardsResponseFormat(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/images/generations", r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		w.Header().Set("X-Ratelimit-Remaining-Images", "11")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"b64_json":"AAAA"}]}`)
	}))
	defer srv.Close()

	c := customCompatAt(srv.URL, srv.Client())
	resp, err := c.Images(context.Background(), "k", &ImageRequest{
		Model: "img", Prompt: "a cat", ResponseFormat: "b64_json",
	})
	require.NoError(t, err)
	require.Equal(t, "b64_json", body["response_format"], "response_format must be forwarded to the upstream")
	require.Equal(t, "11", resp.Headers.Get("X-Ratelimit-Remaining-Images"),
		"successful image response headers must be retained")
}

// TestFixedSpeechFormatRefused proves a native TTS deployment with one output
// codec refuses a mismatched requested format rather than returning a codec the
// caller did not ask for, while an empty or matching format is accepted.
func TestFixedSpeechFormatRefused(t *testing.T) {
	// The helper is the enforcement seam every fixed-codec adapter shares.
	require.NoError(t, enforceFixedSpeechFormat("Cloudflare", "", "mp3"), "an empty format defers to native output")
	require.NoError(t, enforceFixedSpeechFormat("Cloudflare", "MP3", "mp3"), "a matching format is accepted case-insensitively")
	require.ErrorIs(t, enforceFixedSpeechFormat("Cloudflare", "opus", "mp3"), ErrModalityUnsupported)

	// Dispatched through Speech, a mismatch refuses before any upstream call.
	cf := &compat{cfg: providerCfg{platform: "cloudflare", name: "Cloudflare"}}
	_, err := cf.Speech(context.Background(), "acct:token", &SpeechRequest{Model: "melotts", Input: "hi", Format: "opus"})
	require.ErrorIs(t, err, ErrModalityUnsupported, "MeloTTS emits MP3, so an opus request must be refused")

	gg := &compat{cfg: providerCfg{platform: "google", name: "Google Gemini"}}
	_, err = gg.Speech(context.Background(), "k", &SpeechRequest{Model: "gemini-tts", Input: "hi", Format: "opus"})
	require.ErrorIs(t, err, ErrModalityUnsupported, "Gemini TTS emits WAV, so an opus request must be refused")
}

// TestSpeechRetainsHeaders proves the OpenAI speech wire carries the successful
// response headers back for quota observation.
func TestSpeechRetainsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/audio/speech", r.URL.Path)
		w.Header().Set("X-Ratelimit-Remaining-Audio", "3")
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte{0x00, 0x01})
	}))
	defer srv.Close()

	c := customCompatAt(srv.URL, srv.Client())
	resp, err := c.Speech(context.Background(), "k", &SpeechRequest{Model: "tts", Input: "hi"})
	require.NoError(t, err)
	require.Equal(t, "3", resp.Headers.Get("X-Ratelimit-Remaining-Audio"))
	require.Equal(t, "audio/mpeg", resp.ContentType)
}

// TestEmbeddingsRetainHeaders proves the embedding wire carries the successful
// response headers back for quota observation.
func TestEmbeddingsRetainHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "990")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":4}}`)
	}))
	defer srv.Close()

	c := customCompatAt(srv.URL, srv.Client())
	resp, err := c.Embeddings(context.Background(), "k", &EmbeddingRequest{Model: "embed", Input: []string{"x"}})
	require.NoError(t, err)
	require.Equal(t, "990", resp.Headers.Get("X-Ratelimit-Remaining-Tokens"))
	require.Len(t, resp.Vectors, 1)
}

// TestTranscriptionRetainsHeaders proves the transcription wire carries the
// successful response headers back for quota observation.
func TestTranscriptionRetainsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/audio/transcriptions", r.URL.Path)
		w.Header().Set("X-Ratelimit-Remaining-Requests", "5")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"hello"}`)
	}))
	defer srv.Close()

	c := customCompatAt(srv.URL, srv.Client())
	resp, err := c.Transcribe(context.Background(), "k", &TranscriptionRequest{
		Model: "whisper", File: []byte("audio"), Filename: "a.wav", ResponseFormat: "json",
	})
	require.NoError(t, err)
	require.Equal(t, "hello", resp.Text)
	require.Equal(t, "5", resp.Headers.Get("X-Ratelimit-Remaining-Requests"))
}
