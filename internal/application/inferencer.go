package application

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/embed"
)

// LoadEmbedder loads the in-process, binary-bundled static embedder. It is a
// package variable so tests can replace the embedding backend.
var LoadEmbedder = func(context.Context) (assist.Embedder, error) {
	model, err := embed.Load()
	if err != nil {
		return nil, err
	}
	return model, nil
}

// DefaultInferencer resolves the semantic-assist backend. Embeddings always
// come from the binary-bundled static code embedder. It is in-process, needs no
// daemon, and is code-tuned rather than a general-purpose text embedder.
//
// Stored vectors are keyed by their producing model. Choosing an embedder by
// whether another service is reachable would invalidate and rebuild the vector
// index whenever that service came or went. One embedder keeps the vector space
// stable. Generation and rerank need a language model: Ollama when reachable,
// then a coding-agent CLI, then no helper while vector and lexical search remain.
func DefaultInferencer(ctx context.Context, cfg config.Config) assist.Inferencer {
	if !cfg.AI.Enabled {
		return nil
	}
	var helper assist.Inferencer
	if cfg.AI.Provider != "agent" && cfg.AI.AssistModel != "" {
		ollama := assist.NewOllama(cfg.AI.OllamaURL, cfg.AI.AssistModel)
		if ollama.Available(ctx) && ollama.HasModel(ctx, cfg.AI.AssistModel) {
			helper = ollama
		}
	}
	if helper == nil {
		command := cfg.AI.AgentCommand
		if command == "" {
			command = DetectAgentCLI()
		}
		if command != "" {
			if agent := assist.NewAgentCLI(command); agent.Available(ctx) {
				helper = agent
			}
		}
	}
	embedder, err := LoadEmbedder(ctx)
	if err == nil {
		return assist.Composite{Emb: embedder, Assist: helper}
	}
	if helper != nil {
		fmt.Fprintf(os.Stderr, "prowl: built-in embedder unavailable (%v); using rerank without embeddings\n", err)
		return helper
	}
	fmt.Fprintf(os.Stderr, "prowl: built-in embedder unavailable (%v); structural search only\n", err)
	return nil
}

// DetectAgentCLI returns a headless completion command for the first installed
// coding-agent CLI. Reranking is a lightweight ordering task, so each command
// uses the client's lower-cost model tier.
func DetectAgentCLI() string {
	for _, candidate := range []struct{ binary, command string }{
		{"claude", "claude -p --model haiku"},
		{"omp", "omp -p --model haiku"},
		{"codex", "codex exec -m gpt-5-mini"},
	} {
		if _, err := exec.LookPath(candidate.binary); err == nil {
			return candidate.command
		}
	}
	return ""
}
