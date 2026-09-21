package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/embed"
)

// In non-interactive mode setupAI reports the chosen tier and never runs an
// installer or a pull. The daemon-management hook is stubbed so the test never
// touches systemd or spawns ollama.
func TestSetupAINonInteractive(t *testing.T) {
	orig := ensureOllama
	ensureOllama = func(context.Context, *assist.Ollama, string) bool { return false }
	defer func() { ensureOllama = orig }()

	var b strings.Builder
	setupAI(context.Background(), &b, config.PresetByName("fast"), false)
	s := b.String()
	if !strings.Contains(s, "fast") || !strings.Contains(s, "gemma3:1b") {
		t.Fatalf("setupAI output missing tier/assist model:\n%s", s)
	}
	// A tier configures the optional rewrite/rerank model only; embeddings are
	// bundled, so setupAI must say so rather than name an Ollama embed model.
	if !strings.Contains(s, embed.ModelName) {
		t.Fatalf("setupAI output does not report the bundled embedder:\n%s", s)
	}
}
