package inject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── Pi, Hermes, OpenClaw ──────────────────────────────────────────────────────
//
// Three more harnesses that route through the gateway, each written the same
// additive, ledger-reversible way as the writers in writers.go: a single named
// provider carrying only the canonical `auto` route, never the free catalogue.
// Pi and OpenClaw keep a native OpenAI-compatible provider map in JSON; Hermes
// keeps a YAML provider mapping. Every writer records exactly what it changed
// so Remove reverts that and nothing a user has since edited.

// ── Pi ────────────────────────────────────────────────────────────────────────
//
// Pi reads custom providers from ~/.pi/agent/models.json: a top-level
// `providers` map whose entries carry baseUrl, api (openai-completions), apiKey,
// authHeader and an explicit `models` array. Routing-only on purpose: the file
// declares only `auto`, so Pi's `/model` picker follows the set and strategy
// selected in Prowl instead of duplicating its internal controls.

type piWriter struct{}

func (piWriter) apply(o Options) (Target, error) {
	path := filepath.Join(o.Home, ".pi", "agent", "models.json")
	entry := map[string]any{
		"baseUrl":    o.BaseURL,
		"api":        "openai-completions",
		"apiKey":     o.Token,
		"authHeader": true,
		"models":     compatModels(o.Models),
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return Target{}, err
	}
	var led writtenEntry
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		led = mergeContainer(obj, "providers", []kv{{ProviderID, blob}})
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	led.Path = path
	if created {
		led.CreatedFile = true
	}
	return Target{Harness: "pi", Files: []string{path},
		Note: "pick any Prowl model in pi", Ledger: []writtenEntry{led}}, nil
}

func (piWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, "pi",
		filepath.Join(home, ".pi", "agent", "models.json"))
}

// ── OpenClaw ───────────────────────────────────────────────────────────────
//
// OpenClaw's top-level config, ~/.openclaw/openclaw.json, is JSON5 with comments
// this package cannot rewrite without destroying them. OpenClaw also reads a
// plain-JSON custom-provider file per agent, ~/.openclaw/agents/<id>/agent/
// models.json, whose top-level `providers` map is exactly the `models.providers`
// surface. Writing that file leaves the commented config untouched. The default
// agent id is `main`.

type openclawWriter struct{}

func (openclawWriter) apply(o Options) (Target, error) {
	path := openclawModelsPath(o.Home)
	entry := map[string]any{
		"baseUrl":    o.BaseURL,
		"api":        "openai-completions",
		"apiKey":     o.Token,
		"authHeader": true,
		"models":     compatModels(o.Models),
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return Target{}, err
	}
	var led writtenEntry
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		led = mergeContainer(obj, "providers", []kv{{ProviderID, blob}})
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	led.Path = path
	if created {
		led.CreatedFile = true
	}
	return Target{Harness: "openclaw", Files: []string{path},
		Note:   "pick any Prowl model in openclaw (models.json, not the commented openclaw.json)",
		Ledger: []writtenEntry{led}}, nil
}

func (openclawWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, "openclaw", openclawModelsPath(home))
}

func openclawModelsPath(home string) string {
	return filepath.Join(home, ".openclaw", "agents", "main", "agent", "models.json")
}

// removeJSONProvider reverts a JSON provider-map injection (pi, openclaw). A
// file this package created is deleted only while it still holds exactly the
// bytes we authored; once the user has edited it, we excise only our own
// provider key - and only while its value is still ours - so their later work
// survives. A merged-into file is always reverted key-by-key.
func removeJSONProvider(home, harness, path string) (Target, error) {
	t := Target{Harness: harness, Files: []string{path}}
	loaded, err := loadRecord(home)
	if err != nil {
		return t, err
	}
	rec := loaded.Targets[harness]
	if len(rec.Ledger) == 0 {
		return t, errNoRecord(harness)
	}
	e := rec.Ledger[0]
	// Refuse a symlink swapped in after apply before any write, exactly as
	// apply does; the error keeps the ledger intact for a retry once the user
	// resolves it to a regular file.
	if err := refuseSymlink(path); err != nil {
		return t, err
	}
	if e.CreatedFile {
		if !exists(path) {
			t.Note = "removed the " + filepath.Base(path) + " we created"
			return t, nil
		}
		cur, err := os.ReadFile(path)
		if err != nil {
			return t, err
		}
		if createdFileUnchanged(cur, e.CreatedContent) {
			if err := os.Remove(path); err != nil {
				return t, err
			}
			t.Note = "removed the " + filepath.Base(path) + " we created"
			return t, nil
		}
		// The file diverged after we created it: revert only our provider key.
		undone, _, err := removeWritten([]writtenEntry{e})
		if err != nil {
			return t, err
		}
		t.Note = fmt.Sprintf("kept your edits; reverted %d injected key(s)", len(undone))
		return t, nil
	}
	undone, _, err := removeWritten([]writtenEntry{e})
	if err != nil {
		return t, err
	}
	t.Note = fmt.Sprintf("reverted %d injected key(s)", len(undone))
	return t, nil
}

// compatModels renders the canonical route as OpenAI-compatible model entries,
// the shape Pi and OpenClaw both accept. Text-only: this is a routing target,
// not a vision model, and marking it so keeps the picker honest.
func compatModels(models []Model) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":            m.ID,
			"name":          m.Name,
			"input":         []string{"text"},
			"contextWindow": m.Context,
			"maxTokens":     m.MaxTokens,
		})
	}
	return out
}

// ── Hermes ─────────────────────────────────────────────────────────────────
//
// Hermes reads a `providers` YAML mapping from ~/.hermes/config.yaml. The block
// carries the endpoint (api), the key (api_key), discover_models:false - so
// Hermes routes only the canonical `auto` model instead of probing /models for
// a whole catalogue. config.yaml is hand-edited, so this writer manages exactly
// the `prowl:` block: it reuses the same text-surgical YAML provider-block
// helpers as the OMP writer, leaving every other key byte-identical.

type hermesWriter struct{}

func hermesConfigPath(home string) string {
	return filepath.Join(home, ".hermes", "config.yaml")
}

func (hermesWriter) apply(o Options) (Target, error) {
	path := hermesConfigPath(o.Home)
	block := renderHermesBlock(o)

	created, err := writeBackup(o.tx, path)
	if err != nil {
		return Target{}, err
	}
	raw, err := os.ReadFile(path)
	yamlPrior := ""
	switch {
	case os.IsNotExist(err):
		blob := "providers:\n" + block
		if err := writeFile(path, []byte(blob), 0o600); err != nil {
			return Target{}, err
		}
	case err != nil:
		return Target{}, err
	default:
		text := string(raw)
		if start, end, ok := ompBlockRangeFor(text, ProviderID); ok {
			yamlPrior = text[start:end]
			text = text[:start] + block + text[end:]
		} else {
			text, err = insertProviderBlock(text, block)
			if err != nil {
				return Target{}, err
			}
		}
		if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
			return Target{}, err
		}
	}
	t := Target{Harness: "hermes", Files: []string{path},
		Note: "select prowl/auto in hermes; created " + boolWord(created)}
	if created {
		t.Ledger = []writtenEntry{{Path: path, CreatedFile: true}}
	} else {
		t.Ledger = []writtenEntry{{Path: path, YamlBlock: ProviderID,
			YamlAuthored: block, YamlPrior: yamlPrior}}
	}
	return t, nil
}

func (hermesWriter) remove(home, _ string) (Target, error) {
	path := hermesConfigPath(home)
	t := Target{Harness: "hermes", Files: []string{path}}
	loaded, err := loadRecord(home)
	if err != nil {
		return t, err
	}
	rec := loaded.Targets["hermes"]
	if len(rec.Ledger) == 0 {
		return t, errNoRecord("hermes")
	}
	e := rec.Ledger[0]
	// Refuse a symlink swapped in after apply before any read or write, exactly
	// as apply does - a dangling one would otherwise read as "already removed"
	// and let Remove forget the ledger. The error keeps the ledger for a retry.
	if err := refuseSymlink(path); err != nil {
		return t, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	// A file we created is deleted only while it still holds exactly what we
	// wrote; a diverged file falls through to block revision so later user
	// keys survive.
	if e.CreatedFile && createdFileUnchanged(raw, e.CreatedContent) {
		if err := os.Remove(path); err != nil {
			return t, err
		}
		t.Note = "removed the config.yaml we created"
		return t, nil
	}
	newText, outcome := reviseYAMLBlock(string(raw), e, ProviderID)
	switch outcome {
	case blockAbsent:
		t.Note = "the gateway block is not present (already removed?)"
		return t, nil
	case blockConflict:
		t.Note = "kept your edits to the gateway provider block (conflict); left the file unchanged"
		return t, nil
	}
	if err := writeFile(path, []byte(newText), credentialMode(path)); err != nil {
		return t, err
	}
	t.Note = yamlRemovalNote(outcome, e.CreatedFile)
	return t, nil
}

func renderHermesBlock(o Options) string {
	var b strings.Builder
	b.WriteString("  " + ProviderID + ":\n")
	b.WriteString("    api: " + o.BaseURL + "\n")
	b.WriteString("    api_key: " + quoteYAML(o.Token) + "\n")
	b.WriteString("    discover_models: false\n")
	b.WriteString("    models:\n")
	for _, m := range o.Models {
		b.WriteString("      - " + quoteYAML(m.ID) + "\n")
	}
	return b.String()
}
