// Package config loads and saves a project's .prowl/config.toml and rules.toml.
package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/neur0map/prowl/internal/boundedio"
)

// AI holds the optional semantic-assist settings. There is deliberately no embed
// model setting: embeddings always come from the code embedder bundled into the
// binary, so semantic search is identical on every machine and a vector index is
// never keyed to whichever daemon happened to be running when it was built.
type AI struct {
	Enabled     bool   `toml:"enabled"`
	RerankModel string `toml:"rerank_model"`
	AssistModel string `toml:"assist_model"`
	OllamaURL   string `toml:"ollama_url"`
	// Provider selects the backend for the generate/rerank half only: "" /
	// "ollama" uses a local Ollama daemon; "agent" borrows a coding-agent CLI
	// named by AgentCommand. Neither supplies embeddings.
	// Reranking is a lightweight ordering task, so AgentCommand should name a
	// cheap/fast model (e.g. "claude -p --model haiku"); prowl is a support tool
	// and the spawn must stay cheap.
	Provider     string `toml:"provider,omitempty"`
	AgentCommand string `toml:"agent_command,omitempty"`
}

// Config is the per-project configuration.
type Config struct {
	Languages []string `toml:"languages"`
	Ignore    []string `toml:"ignore"`
	AI        AI       `toml:"ai"`
}

// Rule is a deterministic architecture/health rule consumed by violations/doctor.
type Rule struct {
	Name        string `toml:"name"`
	Kind        string `toml:"kind"`
	Description string `toml:"description"`
}

// Forbid declares a forbidden dependency crossing: any resolved edge whose
// source path matches From and target path matches To is a violation.
type Forbid struct {
	Name string `toml:"name"`
	From string `toml:"from"` // glob on the source file path
	To   string `toml:"to"`   // glob on the target file path
}

// Rules is the set of architecture rules for a project.
type Rules struct {
	Rule   []Rule   `toml:"rule"`
	Forbid []Forbid `toml:"forbid"`
}

const (
	configName = "config.toml"
	rulesName  = "rules.toml"
	// MaxConfigBytes bounds deadline-sensitive workbench configuration reads.
	MaxConfigBytes int64 = 1 << 20
)

// Default returns the starting configuration for a new workspace.
func Default() Config {
	p := PresetByName(DefaultTier)
	return Config{
		Languages: []string{"auto"},
		Ignore:    []string{".mcp.json", "opencode.json", "*.log", "*.png", "*.jpg", "*.jpeg", "*.gif", "*.ttf", "*.otf", "*.woff", "*.woff2"},
		AI: AI{
			// Semantic assist is always on: init no longer offers to skip it, and
			// the runtime resolver degrades gracefully (local vectors -> borrowed
			// coding-agent rerank -> structural) so "enabled" never blocks a query.
			Enabled:     true,
			AssistModel: p.AssistModel,
			OllamaURL:   "http://localhost:11434",
		},
	}
}

// ModelPreset is a named assist model for the AI layer. Embeddings are not part
// of a tier: they always come from the bundled in-process code embedder, so a
// tier only chooses how much machine the optional rewrite/rerank model uses.
type ModelPreset struct {
	Name        string
	Desc        string
	AssistModel string
}

// DefaultTier is the recommended preset when none is chosen.
const DefaultTier = "smart"

// Presets are the assist model tiers offered at init, fastest to best. Sizes and
// VRAM are rough guidance for choosing one.
var Presets = []ModelPreset{
	{"fast", "runs anywhere, CPU ok (~1 GB)", "gemma3:1b"},
	{"smart", "newer assist, ~10 GB VRAM", "gemma4:e2b"},
	{"max", "best quality, ~16 GB VRAM", "gemma4:e4b"},
}

// KnownEmbedModels lists Ollama models that are embedding-only, by base name (tag
// stripped). prowl never uses Ollama for embeddings; this list exists so init does
// not mistake an installed embedder for a usable assist (generate/rerank) model.
var KnownEmbedModels = []string{
	"nomic-embed-text",
	"mxbai-embed-large",
	"embeddinggemma",
	"qwen3-embedding",
	"snowflake-arctic-embed",
	"snowflake-arctic-embed2",
	"bge-m3",
	"bge-large",
	"all-minilm",
	"granite-embedding",
	"paraphrase-multilingual",
}

// PresetByName returns the named preset, falling back to the default tier.
func PresetByName(name string) ModelPreset {
	for _, p := range Presets {
		if p.Name == name {
			return p
		}
	}
	for _, p := range Presets {
		if p.Name == DefaultTier {
			return p
		}
	}
	return Presets[0]
}

// DefaultRules returns general repository rules. Desktop-specific rules are
// available through RiceRules and the doctor rice profile.
func DefaultRules() Rules {
	return Rules{Rule: []Rule{
		{Name: "no-dangling-includes", Kind: "dangling_includes", Description: "every source/include/import/require must resolve to a file in the project"},
	}}
}

// RiceRules returns optional desktop/dotfile rules layered on top of the general
// rules by clients that explicitly opt into the rice profile.
func RiceRules() Rules {
	rules := DefaultRules()
	rules.Rule = append(rules.Rule,
		Rule{Name: "no-orphan-scripts", Kind: "orphan_script", Description: "scripts should be referenced by some config or keybind"},
		Rule{Name: "use-theme-variables", Kind: "hardcoded_color", Description: "prefer theme variables over hardcoded color literals"},
	)
	return rules
}

// Load reads config.toml from dir, returning Default() if absent.
func Load(dir string) (Config, error) {
	c := Default()
	p := filepath.Join(dir, configName)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return c, nil
	}
	_, err := toml.DecodeFile(p, &c)
	return c, err
}

// LoadContext reads a regular config.toml through one pinned directory root.
// It is used by bounded workbench startup; ordinary Load behavior is unchanged.
func LoadContext(ctx context.Context, dir string) (Config, error) {
	c := Default()
	if err := ctx.Err(); err != nil {
		return c, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return c, err
	}
	defer root.Close()
	file, err := boundedio.OpenRegular(root, configName)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer file.Close()
	data, err := boundedio.ReadAllContext(ctx, file, MaxConfigBytes)
	if err != nil {
		return c, err
	}
	_, err = toml.Decode(string(data), &c)
	return c, err
}

// Save writes config.toml into dir.
func Save(dir string, c Config) error {
	return encode(filepath.Join(dir, configName), c)
}

// LoadRules reads rules.toml from dir, returning DefaultRules() if absent.
func LoadRules(dir string) (Rules, error) {
	r := DefaultRules()
	p := filepath.Join(dir, rulesName)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return r, nil
	}
	r = Rules{}
	_, err := toml.DecodeFile(p, &r)
	return r, err
}

// SaveRules writes rules.toml into dir.
func SaveRules(dir string, r Rules) error {
	return encode(filepath.Join(dir, rulesName), r)
}

func encode(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(v)
}
