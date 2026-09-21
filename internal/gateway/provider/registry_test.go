package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/gateway/catalog"
)

func compatOf(t *testing.T, r *Registry, platform string) *compat {
	t.Helper()
	p, ok := r.Get(platform)
	if !ok {
		t.Fatalf("platform %q not registered", platform)
	}
	c, ok := p.(*compat)
	if !ok {
		t.Fatalf("platform %q is not a *compat", platform)
	}
	return c
}

func TestRegistryCoverage(t *testing.T) {
	r := NewRegistry()
	// A representative spread of platforms across adapter kinds must exist.
	for _, p := range []string{
		"groq", "openrouter", "cohere", "cloudflare", "aihorde",
		"modelscope", "pollinations", "zhipu", "electronhub", "router9",
		"septor", "kilo", "ovh", "nvidia", "mistral", "custom",
	} {
		if !r.Has(p) {
			t.Errorf("missing platform %q", p)
		}
	}
	if r.Has("github") {
		t.Fatal("retired GitHub Models endpoint must not be registered")
	}
}

func TestResolveCustom(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Resolve("custom", "   "); ok {
		t.Fatal("custom with blank base URL must not resolve")
	}
	p, ok := r.Resolve("custom", "https://relay.example/v1")
	if !ok {
		t.Fatal("custom with base URL should resolve")
	}
	if p.BaseURL() != "https://relay.example/v1" {
		t.Fatalf("custom base URL = %q", p.BaseURL())
	}
}

func TestAuthHeaderStyles(t *testing.T) {
	r := NewRegistry()

	// Bearer default.
	if h := compatOf(t, r, "groq").authMap("k"); h["Authorization"] != "Bearer k" {
		t.Fatalf("groq auth = %v", h)
	}
	// Truly key-less providers send no auth header.
	if h := compatOf(t, r, "kilo").authMap("ignored"); h["Authorization"] != "" {
		t.Fatalf("kilo (keyless) must send no auth header, got %v", h)
	}
	// OpenRouter carries its identity headers.
	o := compatOf(t, r, "openrouter").authMap("k")
	if o["X-Title"] != "FreeLLMAPI" || o["HTTP-Referer"] != "http://localhost:3001" {
		t.Fatalf("openrouter extra headers = %v", o)
	}
}

func TestCloudflareEndpointParse(t *testing.T) {
	base, token, err := cloudflareEndpoint("acct123:secrettoken")
	if err != nil {
		t.Fatal(err)
	}
	if token != "secrettoken" {
		t.Fatalf("token = %q", token)
	}
	if base != "https://api.cloudflare.com/client/v4/accounts/acct123/ai/v1" {
		t.Fatalf("base = %q", base)
	}
	if _, _, err := cloudflareEndpoint("no-colon"); err == nil {
		t.Fatal("compound key without a colon must error")
	}
}

func TestAIHordeEndpointSentinel(t *testing.T) {
	for _, in := range []string{"", "no-key", "0000000000"} {
		_, k, _ := aihordeEndpoint(in)
		if k != aihordeAnonKey {
			t.Fatalf("aihordeEndpoint(%q) key = %q, want anon", in, k)
		}
	}
	if _, k, _ := aihordeEndpoint("real-key"); k != "real-key" {
		t.Fatalf("registered key should pass through, got %q", k)
	}
}

func TestStrictMessageWhitelist(t *testing.T) {
	c := NewRegistry()
	groq := compatOf(t, c, "groq")
	body := groq.buildBody(&ChatRequest{
		Model: "m",
		Messages: []map[string]any{{
			"role": "user", "content": "hi",
			"partial": true, "reasoning_content": "secret",
		}},
	}, false)
	msgs := body["messages"].([]map[string]any)
	if _, ok := msgs[0]["partial"]; ok {
		t.Fatal("strict platform must drop `partial`")
	}
	if _, ok := msgs[0]["reasoning_content"]; ok {
		t.Fatal("strict platform must drop `reasoning_content`")
	}
	if msgs[0]["content"] != "hi" {
		t.Fatal("strict whitelist dropped a legitimate field")
	}
}

func TestForceSingleToolCall(t *testing.T) {
	r := NewRegistry()
	nvidia := compatOf(t, r, "nvidia")
	body := nvidia.buildBody(&ChatRequest{
		Model:  "m",
		Params: map[string]any{"tools": []any{map[string]any{"type": "function"}}},
	}, false)
	if body["parallel_tool_calls"] != false {
		t.Fatalf("nvidia must pin parallel_tool_calls=false when tools present, got %v", body["parallel_tool_calls"])
	}
	// No tools => the flag is not forced.
	body = nvidia.buildBody(&ChatRequest{Model: "m"}, false)
	if _, ok := body["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls must not be set when there are no tools")
	}
}

func TestCohereStripsSchemaKeys(t *testing.T) {
	r := NewRegistry()
	cohere := compatOf(t, r, "cohere")
	body := cohere.buildBody(&ChatRequest{
		Model: "m",
		Params: map[string]any{"tools": []any{
			map[string]any{"function": map[string]any{"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"$schema":              "http://json-schema.org/draft-07/schema#",
				"properties":           map[string]any{"x": map[string]any{"type": "string"}},
			}}},
		}},
	}, false)
	params := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params["additionalProperties"]; ok {
		t.Fatal("cohere must strip additionalProperties")
	}
	if _, ok := params["$schema"]; ok {
		t.Fatal("cohere must strip $schema")
	}
	if _, ok := params["type"]; !ok {
		t.Fatal("cohere stripped a legitimate schema key")
	}
}

func TestAIHordeBodyConstraints(t *testing.T) {
	r := NewRegistry()
	horde := compatOf(t, r, "aihorde")
	body := horde.buildBody(&ChatRequest{
		Model: "m",
		Params: map[string]any{
			"max_tokens": 5,
			"stop":       "END",
			"tools":      []any{map[string]any{"type": "function"}},
		},
	}, false)
	if body["max_tokens"] != aihordeMinMaxTokens {
		t.Fatalf("max_tokens floored to %d, got %v", aihordeMinMaxTokens, body["max_tokens"])
	}
	if _, ok := body["stop"].([]any); !ok {
		t.Fatalf("stop must be wrapped in an array, got %T", body["stop"])
	}
	if _, ok := body["tools"]; ok {
		t.Fatal("aihorde must drop tools")
	}
}

// ── generic adapter over HTTP ────────────────────────────────────────────────

func customProvider(t *testing.T, url string) Provider {
	t.Helper()
	p, ok := NewRegistry().Resolve("custom", url)
	if !ok {
		t.Fatal("custom did not resolve")
	}
	return p
}

func TestGenericChatCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("missing bearer, got %q", req.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`)
	}))
	defer srv.Close()

	resp, err := customProvider(t, srv.URL).ChatCompletion(context.Background(), "sk-test", &ChatRequest{
		Model:    "m",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "c1" || string(resp.Choices[0].Message.Content) != `"hello"` {
		t.Fatalf("bad response: %+v", resp)
	}
	if resp.RoutedVia == nil || resp.RoutedVia.Platform != "custom" || resp.RoutedVia.Model != "m" {
		t.Fatalf("routed_via not stamped: %+v", resp.RoutedVia)
	}
}

func TestGenericValidateTriState(t *testing.T) {
	// 200 from an endpoint that ACTUALLY checks the credential => valid. The
	// server must reject a wrong key, or a success proves nothing about the
	// key and the contrast probe correctly refuses to call it healthy.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		io.WriteString(w, `{"data":[]}`)
	}))
	defer ok.Close()
	if r := customProvider(t, ok.URL).ValidateKey(context.Background(), "k"); !r.IsValid() {
		t.Fatalf("200 => %+v, want valid", r)
	}

	// An endpoint that serves everyone cannot confirm a key. Ollama Cloud does
	// exactly this, which had a dead credential showing as healthy.
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"data":[]}`)
	}))
	defer public.Close()
	if r := customProvider(t, public.URL).ValidateKey(context.Background(), "k"); !r.IsInconclusive() {
		t.Fatalf("public endpoint => %+v, want inconclusive", r)
	}

	// 401 => invalid with the upstream reason.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"revoked"}}`)
	}))
	defer bad.Close()
	r := customProvider(t, bad.URL).ValidateKey(context.Background(), "k")
	if !r.IsInvalid() {
		t.Fatalf("401 => %+v, want invalid", r)
	}

	// Transport failure => inconclusive, never invalid: a dead endpoint must
	// not disable a key.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	if r := customProvider(t, url).ValidateKey(context.Background(), "k"); !r.IsInconclusive() {
		t.Fatalf("dead endpoint => %+v, want inconclusive", r)
	}
}
func TestCodestralValidatesWithoutModelsEndpointOrCharge(t *testing.T) {
	var requestedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer live-key" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requestedModel = body.Model
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	defer srv.Close()

	c := &compat{
		cfg: providerCfg{
			platform:   "codestral",
			name:       "Codestral",
			baseURL:    srv.URL,
			validate:   codestralValidate,
			listModels: codestralListModels,
		},
		client: srv.Client(),
	}
	if result := c.ValidateKey(context.Background(), "live-key"); !result.IsValid() {
		t.Fatalf("accepted key = %+v, want valid", result)
	}
	if requestedModel != codestralProbeModel {
		t.Fatalf("probe model = %q, want deliberate non-model", requestedModel)
	}
	models, err := c.ListModels(context.Background(), "live-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "codestral-2508" {
		t.Fatalf("models = %+v, want documented Codestral model", models)
	}
	if result := c.ValidateKey(context.Background(), "revoked"); !result.IsInvalid() {
		t.Fatalf("rejected key = %+v, want invalid", result)
	}
	if _, err := c.ListModels(context.Background(), "revoked"); err == nil {
		t.Fatal("rejected key must not discover models")
	}
}

func TestGenericStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	stream, err := customProvider(t, srv.URL).StreamChatCompletion(context.Background(), "k", &ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var frames int
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Model != "m" {
			t.Fatalf("chunk model = %q", chunk.Model)
		}
		frames++
	}
	if frames != 2 {
		t.Fatalf("got %d frames, want 2", frames)
	}
}

func TestRegistryExcludesProvidersWithoutAnInferenceWire(t *testing.T) {
	// sail is validation-only - no chat, embedding or media wire - so it must
	// not be advertised as an inference adapter. Google is the counter-case: it
	// has embedding and speech wires and IS registered (see the test below).
	if NewRegistry().Has("sail") {
		t.Fatal("sail is validation-only and must not be advertised as an inference adapter")
	}
}

// TestGoogleRegisteredForModalityButRefusesChat proves Google is registered so a
// modality route can resolve it for embeddings and speech, while its
// unimplemented native chat wire is refused with a typed error rather than
// falsely advertised - a mis-routed chat request must fail over, not POST an
// OpenAI chat body to an endpoint that does not speak it.
func TestGoogleRegisteredForModalityButRefusesChat(t *testing.T) {
	g, ok := NewRegistry().Get("google")
	if !ok {
		t.Fatal("google must be registered for its embedding and speech wires")
	}
	if _, ok := g.(Embedder); !ok {
		t.Fatal("google must serve embeddings")
	}
	if _, ok := g.(SpeechSynthesizer); !ok {
		t.Fatal("google must serve speech")
	}
	req := &ChatRequest{Model: "gemini-2.5-flash"}
	if _, err := g.ChatCompletion(context.Background(), "k", req); !errors.Is(err, ErrChatUnsupported) {
		t.Fatalf("google ChatCompletion = %v, want ErrChatUnsupported", err)
	}
	if _, err := g.StreamChatCompletion(context.Background(), "k", req); !errors.Is(err, ErrChatUnsupported) {
		t.Fatalf("google StreamChatCompletion = %v, want ErrChatUnsupported", err)
	}
}

// TestEveryRoutableCatalogProviderHasAnAdapter is the parity guard: a provider
// the catalogue advertises as routable (OpenAI-compatible, no unfilled vars, a
// base URL) must resolve to a registered adapter, or its models are candidates
// the router can never dispatch. It resolves through the same canonical mapping
// the auto-registration uses, so the two cannot drift apart.
func TestEveryRoutableCatalogProviderHasAnAdapter(t *testing.T) {
	r := NewRegistry()
	routable, err := catalog.Routable()
	if err != nil {
		t.Fatalf("load routable catalog: %v", err)
	}
	if len(routable) == 0 {
		t.Fatal("catalog reports no routable providers")
	}
	for _, p := range routable {
		platform, ok := catalog.AdapterPlatform(p.ID)
		if !ok {
			platform = p.ID
		}
		if !r.Has(platform) {
			t.Errorf("routable catalog provider %q resolves to platform %q, which has no adapter", p.ID, platform)
		}
	}
}

// TestAutoRegistrationNeverOverridesHandwritten proves the handwritten wins
// rule: an auto-registered adapter must never replace one registerAll wrote by
// hand, so a real wire deviation survives. SiliconFlow is the sharp case - the
// catalogue ships a .cn host while the handwritten adapter uses .com - so the
// registered base URL must be the handwritten one.
func TestAutoRegistrationNeverOverridesHandwritten(t *testing.T) {
	c := compatOf(t, NewRegistry(), "siliconflow")
	if got := c.BaseURL(); got != "https://api.siliconflow.com/v1" {
		t.Fatalf("siliconflow base URL = %q, want the handwritten .com host", got)
	}
}

// TestListModelsDiscoversAndDoesNotFabricateHealth is the discovery proof:
// against a fake upstream, ListModels parses the model list, and an auth
// failure yields an error carrying neither the credential nor the body - it is
// a pure lister, so it can never mark a credential healthy.
func TestListModelsDiscoversAndDoesNotFabricateHealth(t *testing.T) {
	const secret = "sk-discover-me-0123456789"

	// data envelope, gated on the credential.
	data := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/models" {
			t.Errorf("discovery hit %s, want /models", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"revoked"}}`)
			return
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"m-a","name":"Model A"},{"id":"m-b"}]}`)
	}))
	defer data.Close()

	lister, ok := customProvider(t, data.URL).(ModelLister)
	if !ok {
		t.Fatal("the generic compat adapter must implement ModelLister")
	}

	got, err := lister.ListModels(context.Background(), secret)
	if err != nil {
		t.Fatalf("discovery against a live key: %v", err)
	}
	if len(got) != 2 || got[0].ID != "m-a" || got[0].Name != "Model A" {
		t.Fatalf("data envelope parsed as %+v", got)
	}
	if got[1].Name != "m-b" {
		t.Fatalf("a nameless entry must default its name to the id, got %q", got[1].Name)
	}

	// An auth failure discovers nothing and leaks nothing.
	badLister := customProvider(t, data.URL).(ModelLister)
	models, err := badLister.ListModels(context.Background(), "sk-wrong-key-secret")
	if err == nil {
		t.Fatal("a rejected credential must fail discovery, not return a model list")
	}
	if len(models) != 0 {
		t.Fatalf("a rejected credential returned %d models", len(models))
	}
	if strings.Contains(err.Error(), "sk-wrong-key-secret") {
		t.Fatalf("discovery error leaked the credential: %v", err)
	}

	// The models envelope is the second standard shape.
	alt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"models":[{"id":"x"},{"id":"y"},{"id":"x"}]}`)
	}))
	defer alt.Close()
	altProv := customProvider(t, alt.URL).(ModelLister)
	altModels, err := altProv.ListModels(context.Background(), "k")
	if err != nil {
		t.Fatalf("models envelope: %v", err)
	}
	if len(altModels) != 2 {
		t.Fatalf("models envelope with a duplicate parsed as %+v, want 2 unique", altModels)
	}
}

// TestParseModelsEnvelopeGeminiShape covers the Gemini-style models list, where
// the identifier lives in "name" under a "models/" resource prefix and the
// friendly label is "displayName".
func TestParseModelsEnvelopeGeminiShape(t *testing.T) {
	got, err := parseModelsEnvelope([]byte(
		`{"models":[{"name":"models/gemini-1.5-pro","displayName":"Gemini 1.5 Pro"},{"name":"models/gemini-1.5-flash"}]}`))
	if err != nil {
		t.Fatalf("parse gemini envelope: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %+v, want 2 models", got)
	}
	if got[0].ID != "gemini-1.5-pro" || got[0].Name != "Gemini 1.5 Pro" {
		t.Fatalf("first entry = %+v, want id gemini-1.5-pro / name Gemini 1.5 Pro", got[0])
	}
	if got[1].ID != "gemini-1.5-flash" || got[1].Name != "gemini-1.5-flash" {
		t.Fatalf("a nameless gemini entry must strip models/ and default its name, got %+v", got[1])
	}
}

// ── Perplexity Agent API adapter ─────────────────────────────────────────────

// perplexityFixture is a trimmed but faithful Agent API response: a search
// results block (the citations) followed by the final assistant message, plus
// the usage block that carries both token counts and the exact monetary cost.
const perplexityFixture = `{
  "id": "resp_test",
  "object": "response",
  "model": "perplexity/sonar",
  "status": "completed",
  "output": [
    {
      "type": "search_results",
      "queries": ["Go 1.24 release notes"],
      "results": [
        {"id": 1, "title": "Go 1.24 Release Notes", "url": "https://go.dev/doc/go1.24", "snippet": "generic type aliases"}
      ]
    },
    {
      "type": "message",
      "id": "msg_test",
      "role": "assistant",
      "status": "completed",
      "content": [
        {"type": "output_text", "annotations": [], "logprobs": [], "text": "Go 1.24 added generic type aliases."}
      ]
    }
  ],
  "usage": {
    "input_tokens": 20,
    "output_tokens": 222,
    "total_tokens": 242,
    "cost": {"currency": "USD", "input_cost": 4e-05, "output_cost": 0.00311, "total_cost": 0.00315}
  }
}`

func perplexityAt(t *testing.T, url string) Provider {
	t.Helper()
	p, ok := NewRegistry().Resolve("perplexity", url)
	if !ok {
		t.Fatal("perplexity did not resolve")
	}
	return p
}

// TestPerplexityResolvesAndDefaultsBaseURL proves the platform is registered,
// that a blank base resolves to the official-endpoint singleton, and that a
// custom relay base builds a fresh adapter bound to it with the trailing slash
// trimmed - the safe custom-base contract, where blank never yields a broken URL.
func TestPerplexityResolvesAndDefaultsBaseURL(t *testing.T) {
	r := NewRegistry()
	p, ok := r.Resolve("perplexity", "")
	if !ok {
		t.Fatal(`Resolve("perplexity", "") must return the adapter`)
	}
	if _, isNative := p.(*perplexityProvider); !isNative {
		t.Fatalf("Resolve returned %T, want *perplexityProvider", p)
	}
	if p.Platform() != "perplexity" || p.Name() != "Perplexity" {
		t.Fatalf("identity = %q/%q", p.Platform(), p.Name())
	}
	if p.BaseURL() != perplexityDefaultBaseURL {
		t.Fatalf("blank base = %q, want official default %q", p.BaseURL(), perplexityDefaultBaseURL)
	}
	if _, ok := r.Get("perplexity"); !ok {
		t.Fatal(`Get("perplexity") must exist`)
	}
	custom, ok := r.Resolve("perplexity", "https://relay.example/agent/")
	if !ok {
		t.Fatal("a custom relay base must resolve")
	}
	if custom.BaseURL() != "https://relay.example/agent" {
		t.Fatalf("custom base = %q, want the trailing slash trimmed", custom.BaseURL())
	}
}

// TestPerplexityChatCompletionParsesAgentResponse is the end-to-end proof: the
// normalized chat request is translated to the /v1/agent body (system hoisted
// to instructions, preset in its slot, max_tokens renamed, chat-only params
// dropped) and the response is folded into an OpenAI completion whose content
// is the final assistant text, whose Usage carries the upstream tokens, and
// whose Raw preserves the exact cost and the citations.
func TestPerplexityChatCompletionParsesAgentResponse(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/agent" {
			t.Fatalf("path = %q, want /v1/agent", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer pk-live" {
			t.Fatalf("auth = %q", req.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		io.WriteString(w, perplexityFixture)
	}))
	defer srv.Close()

	resp, err := perplexityAt(t, srv.URL).ChatCompletion(context.Background(), "pk-live", &ChatRequest{
		Model: "perplexity/sonar",
		Messages: []map[string]any{
			{"role": "system", "content": "Be concise."},
			{"role": "user", "content": "What changed in Go 1.24?"},
		},
		Params: map[string]any{"preset": "low", "max_tokens": 512, "n": 3, "instructions": "Prefer primary sources."},
	})
	if err != nil {
		t.Fatal(err)
	}

	// request translation
	if gotBody["model"] != "perplexity/sonar" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	if gotBody["preset"] != "low" {
		t.Fatalf("preset = %v", gotBody["preset"])
	}
	if gotBody["instructions"] != "Be concise.\n\nPrefer primary sources." {
		t.Fatalf("instructions = %v, want system and request instructions preserved", gotBody["instructions"])
	}
	if gotBody["max_output_tokens"] != float64(512) {
		t.Fatalf("max_output_tokens = %v, want max_tokens translated", gotBody["max_output_tokens"])
	}
	if _, leaked := gotBody["max_tokens"]; leaked {
		t.Fatal("max_tokens must not reach the Responses API")
	}
	if _, leaked := gotBody["n"]; leaked {
		t.Fatal("chat-only param n must be dropped, not forwarded")
	}
	if input, _ := gotBody["input"].([]any); len(input) != 1 {
		t.Fatalf("input length = %d, want the single user turn (system hoisted out)", len(input))
	}

	// response normalization
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil {
		t.Fatalf("content is not a JSON string: %v", err)
	}
	if !strings.Contains(content, "Go 1.24") {
		t.Fatalf("content = %q, want the final assistant text", content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish = %q", resp.Choices[0].FinishReason)
	}
	if resp.RoutedVia == nil || resp.RoutedVia.Platform != "perplexity" || resp.RoutedVia.Model != "perplexity/sonar" {
		t.Fatalf("routed_via = %+v", resp.RoutedVia)
	}

	// usage tokens
	if resp.Usage == nil || resp.Usage.PromptTokens != 20 || resp.Usage.CompletionTokens != 222 || resp.Usage.TotalTokens != 242 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	// exact cost and citations survive in Raw
	var raw struct {
		Usage struct {
			Cost struct {
				Total float64 `json:"total_cost"`
			} `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp.Raw, &raw); err != nil {
		t.Fatalf("raw is not the upstream body: %v", err)
	}
	if raw.Usage.Cost.Total != 0.00315 {
		t.Fatalf("raw usage.cost.total_cost = %v, want the provider-reported cost", raw.Usage.Cost.Total)
	}
	if !strings.Contains(string(resp.Raw), "https://go.dev/doc/go1.24") {
		t.Fatal("Raw must retain the search_results citations")
	}
}

// TestPerplexityPresetVsModelSlotting proves the small explicit research-preset
// category: a Model that names a preset is sent in the preset slot (never as a
// model id), while a concrete id is sent as the model.
func TestPerplexityPresetVsModelSlotting(t *testing.T) {
	preset := perplexityRequest(&ChatRequest{
		Model:    "medium",
		Messages: []map[string]any{{"role": "user", "content": "hi"}},
	}, false)
	if preset["preset"] != "medium" {
		t.Fatalf("preset slot = %v, want the research preset name", preset["preset"])
	}
	if _, ok := preset["model"]; ok {
		t.Fatal("a preset name must not be sent as a model id")
	}

	model := perplexityRequest(&ChatRequest{Model: "perplexity/sonar"}, false)
	if model["model"] != "perplexity/sonar" {
		t.Fatalf("model slot = %v", model["model"])
	}
	if _, ok := model["preset"]; ok {
		t.Fatal("a concrete model id must not be sent as a preset")
	}
}

// TestPerplexityValidateKey pins the tri-state credential probe against
// GET /v1/models: a 200 confirms the key, only 401/403 confirm it is bad, and a
// transport failure is inconclusive so a network blip never disables a key.
func TestPerplexityValidateKey(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		io.WriteString(w, `{"object":"list","data":[{"id":"perplexity/sonar"}]}`)
	}))
	defer good.Close()
	if r := perplexityAt(t, good.URL).ValidateKey(context.Background(), "good"); !r.IsValid() {
		t.Fatalf("200 => %+v, want valid", r)
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		code := status
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, `{"error":{"message":"revoked"}}`)
		}))
		r := perplexityAt(t, bad.URL).ValidateKey(context.Background(), "k")
		bad.Close()
		if !r.IsInvalid() {
			t.Fatalf("%d => %+v, want invalid", code, r)
		}
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	if r := perplexityAt(t, url).ValidateKey(context.Background(), "k"); !r.IsInconclusive() {
		t.Fatalf("dead endpoint => %+v, want inconclusive", r)
	}
}

// TestPerplexityBufferedStream proves the stream contract is honoured by a
// bounded buffered replay: one terminal frame carries the whole answer rather
// than faking token-by-token streaming the /v1/agent call does not provide.
func TestPerplexityBufferedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, perplexityFixture)
	}))
	defer srv.Close()

	stream, err := perplexityAt(t, srv.URL).StreamChatCompletion(context.Background(), "k", &ChatRequest{Model: "perplexity/sonar"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	frames := 0
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Model != "perplexity/sonar" {
			t.Fatalf("chunk model = %q", chunk.Model)
		}
		frames++
	}
	if frames != 1 {
		t.Fatalf("buffered stream produced %d frames, want exactly 1", frames)
	}
}

// TestPerplexityRejectsUncompletedRun proves the adapter never fabricates text
// for a run that did not complete: a failed run and a still-queued background
// run both surface as errors, not as an empty answer.
func TestPerplexityRejectsUncompletedRun(t *testing.T) {
	for _, status := range []string{"failed", "queued"} {
		state := status
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, `{"id":"resp_x","model":"perplexity/sonar","object":"response","status":"`+state+`","output":[]}`)
		}))
		_, err := perplexityAt(t, srv.URL).ChatCompletion(context.Background(), "k", &ChatRequest{Model: "perplexity/sonar"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %q must be an error, not a silent empty answer", state)
		}
	}
}

// TestCohereDeepCopiesSchemaBeforeStrip proves the Cohere schema strip does not
// reach back into the caller's request: the rendered body is stripped, but the
// source tool schema is left whole so a fallback provider after a Cohere 400
// receives the original request.
func TestCohereDeepCopiesSchemaBeforeStrip(t *testing.T) {
	r := NewRegistry()
	cohere := compatOf(t, r, "cohere")
	params := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"properties":           map[string]any{"x": map[string]any{"type": "string"}},
	}
	req := &ChatRequest{
		Model: "m",
		Params: map[string]any{"tools": []any{
			map[string]any{"function": map[string]any{"parameters": params}},
		}},
	}
	body := cohere.buildBody(req, false)

	// The rendered wire body is stripped for Cohere.
	got := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := got["additionalProperties"]; ok {
		t.Fatal("cohere body must strip additionalProperties")
	}
	if _, ok := got["$schema"]; ok {
		t.Fatal("cohere body must strip $schema")
	}

	// The caller's source schema is untouched - a fallback provider sees it whole.
	if _, ok := params["additionalProperties"]; !ok {
		t.Fatal("cohere transform mutated the source schema (additionalProperties lost)")
	}
	if _, ok := params["$schema"]; !ok {
		t.Fatal("cohere transform mutated the source schema ($schema lost)")
	}
	if got["additionalProperties"] != nil {
		t.Fatal("stripped copy still aliases the source map")
	}
}

// TestStreamNon2xxBodyIsBounded proves a streaming error body is bounded like a
// buffered reply: a hostile endpoint that answers a stream with a huge non-2xx
// body cannot force an unbounded read.
func TestStreamNon2xxBodyIsBounded(t *testing.T) {
	over := maxProviderResponse + (1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 'a'
		}
		for written := 0; written < over; {
			n, err := w.Write(buf)
			written += n
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, err := customProvider(t, srv.URL).StreamChatCompletion(context.Background(), "k", &ChatRequest{Model: "m"})
	he, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("want *HTTPError, got %T: %v", err, err)
	}
	if he.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", he.Status)
	}
	if len(he.Body) > maxProviderResponse {
		t.Fatalf("stream error body %d exceeds cap %d", len(he.Body), maxProviderResponse)
	}
}

// TestStreamRequiresObservedModelIdentity proves a guarded stream must OBSERVE
// a matching model identity before it is accepted: frames that never name the
// requested model fail even when the stream closes cleanly, while a stream that
// does name it succeeds.
func TestStreamRequiresObservedModelIdentity(t *testing.T) {
	guarded := func(url string) *compat {
		return NewRegistry().newCompat(providerCfg{platform: "guarded", name: "g", baseURL: url, verifyModelIdentity: true})
	}

	t.Run("absent identity fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"he\"}}]}\n\n")
			io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		stream, err := guarded(srv.URL).StreamChatCompletion(context.Background(), "k", &ChatRequest{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		var last error
		for {
			if _, last = stream.Recv(); last != nil {
				break
			}
		}
		he, ok := last.(*HTTPError)
		if !ok || he.Status != 502 {
			t.Fatalf("want 502 identity error, got %v", last)
		}
	})

	t.Run("matching identity succeeds", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer srv.Close()

		stream, err := guarded(srv.URL).StreamChatCompletion(context.Background(), "k", &ChatRequest{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("matching-identity stream must succeed, got %v", err)
			}
		}
	})
}

// TestProviderTransportEnforcesDialGuard proves the routed inference transport
// consults DialGuard at connect time and aborts the dial when it refuses - the
// chokepoint the gateway installs the SSRF verdict into. It sets the guard
// directly so it exercises the registry client's Control hook (the actual
// routed transport) without importing gateway.
func TestProviderTransportEnforcesDialGuard(t *testing.T) {
	orig := DialGuard
	t.Cleanup(func() { DialGuard = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("DialGuard let a refused dial reach the upstream")
	}))
	defer srv.Close()

	DialGuard = func(address string) error { return fmt.Errorf("refused %s", address) }
	p, ok := NewRegistry().Resolve("custom", srv.URL)
	if !ok {
		t.Fatal("custom did not resolve")
	}
	_, err := p.ChatCompletion(context.Background(), "k", &ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("dial must be aborted by DialGuard, got %v", err)
	}
}

// TestBufferedStreamSynthesizesNormalizedChunkShape proves a buffered-to-stream
// adapter's single frame declares the streamed object and is marked Native, so
// the OpenAI surface renders the re-normalised delta rather than relaying the
// buffered completion's Raw (a chat.completion or native envelope) as a stray
// stream frame - while Raw is retained so the gateway can still read usage.
func TestBufferedStreamSynthesizesNormalizedChunkShape(t *testing.T) {
	content, _ := json.Marshal("hi")
	resp := &ChatResponse{
		ID:    "resp_1",
		Model: "some/model",
		Choices: []Choice{{
			Index:        0,
			Message:      RespMessage{Role: "assistant", Content: content},
			FinishReason: "stop",
		}},
		// A native (non-OpenAI) envelope that must never reach the client as a
		// stream frame, but does carry usage the gateway reads off Raw.
		Raw:    json.RawMessage(`{"object":"response","status":"completed","usage":{"input_tokens":3,"output_tokens":4}}`),
		Native: false,
	}

	stream := newBufferedStream(resp)
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if chunk.Object != "chat.completion.chunk" {
		t.Fatalf("chunk object = %q, want chat.completion.chunk", chunk.Object)
	}
	if !chunk.Native {
		t.Fatal("buffered chunk must be Native so the normalised delta is rendered, not the buffered Raw")
	}
	// The delta is the re-normalised OpenAI shape, not the native envelope.
	if !strings.Contains(string(chunk.Choices[0].Delta), `"content":"hi"`) {
		t.Fatalf("delta = %q, want the normalised message", string(chunk.Choices[0].Delta))
	}
	// Raw is retained for usage extraction.
	if !strings.Contains(string(chunk.Raw), `"usage"`) {
		t.Fatal("buffered chunk must retain Raw so streamed usage survives")
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("second Recv = %v, want io.EOF", err)
	}
}
