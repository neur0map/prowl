package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/embed"
)

// tagsServer serves a fixed /api/tags body so model detection can be tested
// without a live Ollama.
func tagsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// When the tier's assist model is absent but another chat model is installed,
// resolveAssistModel substitutes the installed one instead of the absent preset.
func TestResolveAssistModelPrefersInstalled(t *testing.T) {
	srv := tagsServer(t, `{"models":[{"name":"nomic-embed-text:latest"},{"name":"qwen3:0.6b"}]}`)
	oll := assist.NewOllama(srv.URL, "gemma3:1b")
	if got := resolveAssistModel(context.Background(), oll, config.PresetByName("fast")); got != "qwen3:0.6b" {
		t.Errorf("assist = %q, want qwen3:0.6b", got)
	}
}

// An installed embedding model is never chosen as the assist model: it cannot
// generate or rerank, and prowl never uses Ollama for embeddings.
func TestResolveAssistModelSkipsEmbeddingModels(t *testing.T) {
	srv := tagsServer(t, `{"models":[{"name":"embeddinggemma:latest"},{"name":"bge-m3:latest"}]}`)
	oll := assist.NewOllama(srv.URL, "gemma3:1b")
	if got := resolveAssistModel(context.Background(), oll, config.PresetByName("fast")); got != "gemma3:1b" {
		t.Errorf("assist = %q, want the preset gemma3:1b (no usable chat model installed)", got)
	}
}

// When the preset assist model is present, resolveAssistModel keeps it.
func TestResolveAssistModelKeepsPresetWhenInstalled(t *testing.T) {
	srv := tagsServer(t, `{"models":[{"name":"embeddinggemma:latest"},{"name":"gemma3:1b"}]}`)
	oll := assist.NewOllama(srv.URL, "gemma3:1b")
	if got := resolveAssistModel(context.Background(), oll, config.PresetByName("fast")); got != "gemma3:1b" {
		t.Errorf("assist = %q, want preset gemma3:1b", got)
	}
}

// When Ollama is unreachable, resolveAssistModel falls back to the preset name so
// init can still print the pull instructions.
func TestResolveAssistModelFallsBackWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close() // dead endpoint: connections are refused
	oll := assist.NewOllama(url, "gemma3:1b")
	if got := resolveAssistModel(context.Background(), oll, config.PresetByName("fast")); got != "gemma3:1b" {
		t.Errorf("assist = %q, want preset fallback", got)
	}
}

// fakeEmbedder is a stub Embedder for wiring tests (no model files or network).
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (fakeEmbedder) EmbedModelID() string                                 { return "static:test" }

// stubEmbedder swaps the package embedder loader for the duration of a test.
func stubEmbedder(t *testing.T, emb assist.Embedder, err error) {
	t.Helper()
	prev := loadEmbedder
	loadEmbedder = func(context.Context) (assist.Embedder, error) { return emb, err }
	t.Cleanup(func() { loadEmbedder = prev })
}

// The bundled in-process embedder is the embedding backend, always. A local
// Ollama with embedding models installed must not take over: it is ~14x slower
// per chunk, and because stored vectors are keyed by their producing model,
// selecting an embedder by "is the daemon up" silently invalidated and rebuilt
// the entire vector index whenever Ollama came or went.
func TestMaybeInferencerAlwaysEmbedsWithBundledModel(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hide any claude/codex/omp
	srv := tagsServer(t, `{"models":[{"name":"embeddinggemma:latest"},{"name":"nomic-embed-text:latest"},{"name":"bge-m3:latest"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, OllamaURL: srv.URL}}
	c, ok := maybeInferencer(context.Background(), cfg).(assist.Composite)
	if !ok {
		t.Fatalf("backend = %T, want assist.Composite carrying the bundled embedder", maybeInferencer(context.Background(), cfg))
	}
	if _, isOllama := c.Emb.(*assist.Ollama); isOllama {
		t.Fatal("embeddings were routed to Ollama")
	}
	if id := c.EmbedModelID(); id != embed.ModelName {
		t.Fatalf("embed model id = %q, want the bundled %q", id, embed.ModelName)
	}
}

// The bundled embedder is compiled into the binary, so it needs no download, no
// daemon, and no cache: it loads with an empty PATH and no network.
func TestBundledEmbedderLoadsWithNoNetworkOrPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, err := loadEmbedder(context.Background())
	if err != nil {
		t.Fatalf("bundled embedder failed to load: %v", err)
	}
	vecs, err := m.Embed(context.Background(), []string{"func main() {}"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		t.Fatalf("bundled embedder returned %d vectors", len(vecs))
	}
}

// Ollama supplies the generate/rerank half when its assist model is installed.
func TestMaybeInferencerUsesOllamaForAssistOnly(t *testing.T) {
	stubEmbedder(t, fakeEmbedder{}, nil)
	srv := tagsServer(t, `{"models":[{"name":"gemma3:1b"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, AssistModel: "gemma3:1b", OllamaURL: srv.URL}}
	c, ok := maybeInferencer(context.Background(), cfg).(assist.Composite)
	if !ok {
		t.Fatal("want assist.Composite")
	}
	if _, ok := c.Assist.(*assist.Ollama); !ok {
		t.Fatalf("Assist = %T, want *assist.Ollama", c.Assist)
	}
	if _, isOllama := c.Emb.(*assist.Ollama); isOllama {
		t.Fatal("embeddings were routed to Ollama")
	}
}

// With no Ollama assist model, embeddings still come from the bundled model and
// there is simply no rewrite/rerank half.
func TestMaybeInferencerStaticWhenNoAssistModel(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hide any claude/codex/omp
	stubEmbedder(t, fakeEmbedder{}, nil)
	srv := tagsServer(t, `{"models":[{"name":"some-other-model:latest"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, AssistModel: "gemma3:1b", OllamaURL: srv.URL}}
	c, ok := maybeInferencer(context.Background(), cfg).(assist.Composite)
	if !ok {
		t.Fatal("want assist.Composite (bundled embeddings) when no Ollama assist model")
	}
	if c.Assist != nil {
		t.Fatalf("Assist = %T, want nil (no agent available)", c.Assist)
	}
}

// With no Ollama assist model but a coding-agent command, embeddings come from the
// bundled model and rewrite/rerank from the agent: a Composite carrying both.
func TestMaybeInferencerStaticPlusAgent(t *testing.T) {
	stubEmbedder(t, fakeEmbedder{}, nil)
	srv := tagsServer(t, `{"models":[{"name":"some-other-model:latest"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, OllamaURL: srv.URL, AgentCommand: "go"}}
	c, ok := maybeInferencer(context.Background(), cfg).(assist.Composite)
	if !ok {
		t.Fatal("want assist.Composite")
	}
	if _, ok := c.Assist.(*assist.AgentCLI); !ok {
		t.Fatalf("Assist = %T, want *assist.AgentCLI", c.Assist)
	}
}

// Only when the embedder itself is unavailable does the backend fall back to
// agent-only rewrite/rerank.
func TestMaybeInferencerAgentOnlyWhenEmbedderUnavailable(t *testing.T) {
	stubEmbedder(t, nil, fmt.Errorf("offline"))
	srv := tagsServer(t, `{"models":[{"name":"some-other-model:latest"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, OllamaURL: srv.URL, AgentCommand: "go"}}
	if _, ok := maybeInferencer(context.Background(), cfg).(*assist.AgentCLI); !ok {
		t.Fatal("want *assist.AgentCLI when embedder unavailable but agent present")
	}
}

// With no embedder and no agent, search is structural (nil).
func TestMaybeInferencerStructuralWhenNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stubEmbedder(t, nil, fmt.Errorf("offline"))
	srv := tagsServer(t, `{"models":[{"name":"some-other-model:latest"}]}`)
	cfg := config.Config{AI: config.AI{Enabled: true, OllamaURL: srv.URL}}
	if inf := maybeInferencer(context.Background(), cfg); inf != nil {
		t.Fatalf("want nil (structural), got %T", inf)
	}
}
