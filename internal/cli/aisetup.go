package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/huh/v2"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/embed"
)

// selectTier asks the user to choose an AI model tier.
func selectTier() string {
	tier := config.DefaultTier
	opts := make([]huh.Option[string], 0, len(config.Presets))
	for _, p := range config.Presets {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-6s %s", p.Name, p.Desc), p.Name))
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Pick an AI model tier").
			Description("Bigger tiers retrieve better but need more VRAM and disk.").
			Options(opts...).
			Value(&tier),
	))
	if form.Run() != nil {
		return config.DefaultTier
	}
	return tier
}

// selectBackend asks which backend powers the higher-quality half of semantic
// assist. Semantic search itself is always on via the built-in in-process
// embedder; a local Ollama model gives higher-quality embeddings, while a
// coding-agent CLI needs no daemon and adds query rewrite + reranking.
func selectBackend(agentCommand string, ollamaInstalled bool) string {
	choice := "agent"
	if ollamaInstalled {
		choice = "ollama"
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Choose the semantic-assist upgrade").
			Description("Semantic search is already on (built-in embedder); pick an optional upgrade.").
			Options(
				huh.NewOption("Local model (Ollama): higher-quality embeddings, needs VRAM/disk", "ollama"),
				huh.NewOption("Coding agent ("+agentCommand+"): rewrite + reranking, no daemon, cheap", "agent"),
			).
			Value(&choice),
	))
	if form.Run() != nil {
		if ollamaInstalled {
			return "ollama"
		}
		return "agent"
	}
	return choice
}

// setupAI is an optional upgrade for the generate/rerank half only: it ensures
// Ollama is installed, brings the daemon up, pulls the tier's assist model, and
// warms it. Semantic vector search never depends on any of this -- embeddings
// always come from the code embedder bundled into the binary -- so every failure
// path here simply leaves search fully working without a rewrite/rerank model.
func setupAI(ctx context.Context, out io.Writer, p config.ModelPreset, interactive bool) {
	fmt.Fprintf(out, "AI tier %q: embeddings built in (%s), assist %s\n", p.Name, embed.ModelName, p.AssistModel)
	oll := assist.NewOllama("", p.AssistModel)
	root, _ := os.Getwd()

	// Ensure Ollama is installed before trying to start it.
	if !oll.Available(ctx) {
		if _, lookErr := exec.LookPath("ollama"); lookErr != nil {
			if interactive && confirmAI("Ollama is not installed. Install it now? (runs the official installer; may ask for sudo)") {
				installOllama(out)
			} else {
				uiLog.Info("Ollama is not installed; semantic search works without it")
				uiLog.Info("optional query rewrite and rerank: curl -fsSL https://ollama.com/install.sh | sh")
				return
			}
		}
	}

	// Bring the daemon up and keep it up for long coding sessions.
	if !ensureOllama(ctx, oll, root) {
		uiLog.Info("Ollama not reachable; semantic search works without it (Ollama would add query rewrite and rerank)")
		return
	}

	if p.AssistModel == "" {
		return
	}
	if !oll.HasModel(ctx, p.AssistModel) {
		if interactive && confirmAI(fmt.Sprintf("Pull %s now?", p.AssistModel)) {
			if err := pullModel(p.AssistModel); err != nil {
				uiLog.Warnf("pull %s failed: %v (run: ollama pull %s)", p.AssistModel, err, p.AssistModel)
			}
		} else {
			uiLog.Infof("pull it: ollama pull %s", p.AssistModel)
		}
	}
	// Warm the assist model so the first rerank does not pay a cold start.
	if oll.HasModel(ctx, p.AssistModel) {
		if err := oll.Warm(ctx, p.AssistModel, ollamaKeepAlive); err != nil {
			uiLog.Warnf("warm %s: %v", p.AssistModel, err)
		} else {
			uiLog.Infof("warmed %s; query rewrite and rerank ready", p.AssistModel)
		}
	}
}

func confirmAI(title string) bool {
	ok := true
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok),
	))
	if form.Run() != nil {
		return false
	}
	return ok
}

func installOllama(out io.Writer) {
	fmt.Fprintln(out, "Installing Ollama ...")
	cmd := exec.Command("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "  installer exited with: %v\n", err)
	}
}

func pullModel(model string) error {
	cmd := exec.Command("ollama", "pull", model)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// resolveAssistModel prefers an assist model already installed on the local
// Ollama so init does not ask for a redundant pull, keeping the tier preset when
// it is installed and falling back to the preset name (to be pulled) when nothing
// suitable is installed or Ollama is unreachable. Embedding models are excluded:
// they cannot generate or rerank, and prowl never uses Ollama for embeddings.
func resolveAssistModel(ctx context.Context, oll *assist.Ollama, p config.ModelPreset) string {
	gen := p.AssistModel
	have, err := oll.Models(ctx)
	if err != nil || len(have) == 0 {
		return gen
	}
	if !installedModel(have, gen) {
		if m := pickChatModel(have); m != "" {
			gen = m
		}
	}
	return gen
}

// installedModel reports whether want is among have, tolerating Ollama's implicit
// :latest tag and bare-name requests.
func installedModel(have []string, want string) bool {
	for _, h := range have {
		if h == want || h == want+":latest" || modelBase(h) == modelBase(want) {
			return true
		}
	}
	return false
}

// modelBase strips an Ollama ":tag" suffix.
func modelBase(m string) string {
	if i := strings.IndexByte(m, ':'); i >= 0 {
		return m[:i]
	}
	return m
}

// pickChatModel returns the first installed model that is not an embedding model,
// for use as the assist model, or "" when none is installed.
func pickChatModel(have []string) string {
	for _, h := range have {
		if !isEmbedModel(modelBase(h)) {
			return h
		}
	}
	return ""
}

// isEmbedModel reports whether a base model name is a known embedding model.
func isEmbedModel(base string) bool {
	for _, known := range config.KnownEmbedModels {
		if base == known {
			return true
		}
	}
	return false
}
