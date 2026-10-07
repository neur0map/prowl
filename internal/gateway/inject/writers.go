package inject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ── Codex CLI ────────────────────────────────────────────────────────────────
//
// Codex reads custom providers from the `[model_providers.<id>]` table in
// ~/.codex/config.toml. The provider embeds the machine-local gateway token
// with experimental_bearer_token, so no shell environment setup is required.
// The top-level `model_provider` and `model` keys are additive by default.
// Options.Activate is the explicit exception: activation replaces those two
// defaults and the ledger restores their prior values on removal.

type codexWriter struct{}

func (codexWriter) apply(o Options) (Target, error) {
	path := filepath.Join(o.Home, ".codex", "config.toml")
	envPath := filepath.Join(o.Home, ".codex", "prowl.env")
	legacyEnvPath := filepath.Join(o.Home, ".codex", legacyProviderID+".env")

	previous, err := loadRecord(o.Home)
	if err != nil {
		return Target{}, err
	}
	prior := previous.Targets["codex"]
	var priorEnv *writtenEntry
	for i := range prior.Ledger {
		if prior.Ledger[i].EnvFile &&
			filepath.Clean(prior.Ledger[i].Path) == filepath.Clean(envPath) {
			priorEnv = &prior.Ledger[i]
			break
		}
	}
	if priorEnv != nil {
		if err := refuseSymlink(envPath); err != nil {
			return Target{}, err
		}
	}

	table := "[model_providers." + ProviderID + "]\n" +
		"name = \"" + ProviderName + "\"\n" +
		"base_url = \"" + o.BaseURL + "\"\n" +
		"experimental_bearer_token = \"" + o.Token + "\"\n" +
		"wire_api = \"responses\"\n"

	created, err := writeBackup(o.tx, path)
	if err != nil {
		return Target{}, err
	}
	raw, err := os.ReadFile(path)
	text := ""
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return Target{}, err
	default:
		text = string(raw)
	}
	originalEmpty := len(strings.TrimSpace(text)) == 0
	added := []writtenEntry{}
	// Collapse the provider table and default written by the old product name.
	if start, end, ok := tomlTableRange(text, "[model_providers."+legacyProviderID+"]"); ok {
		text = text[:start] + text[end:]
	}
	if migrated, changed := rewriteExactTOMLValue(text, "model_provider", legacyProviderID, ProviderID); changed {
		text = migrated
		added = append(added, writtenEntry{Path: path, TomlTop: "model_provider", TomlVal: `"` + ProviderID + `"`})
	}

	tomlCreated := created || originalEmpty
	tomlPrior := ""
	start, end, present := tomlTableRange(text, "[model_providers."+ProviderID+"]")
	switch {
	case present:
		tomlPrior = text[start:end]
		text = text[:start] + table + text[end:]
	default:
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + table
	}
	// Defaults are added only when the user has not set them. A value still
	// owned by the previous injection remains in the ledger across re-apply.
	providerValue := `"` + ProviderID + `"`
	if !lineHasKey(text, "model_provider") {
		text = "model_provider = " + providerValue + "\n" + text
		added = append(added, writtenEntry{Path: path, TomlTop: "model_provider", TomlVal: providerValue})
	} else if tomlTopValue(text, "model_provider") == providerValue {
		if entry, ok := priorTomlTopEntry(prior.Ledger, path, "model_provider", providerValue); ok {
			added = append(added, entry)
		}
	}
	if !lineHasKey(text, "model") {
		text = "model = \"auto\"\n" + text
		added = append(added, writtenEntry{Path: path, TomlTop: "model", TomlVal: `"auto"`})
	} else if tomlTopValue(text, "model") == `"auto"` {
		if entry, ok := priorTomlTopEntry(prior.Ledger, path, "model", `"auto"`); ok {
			added = append(added, entry)
		}
	}

	if err := o.tx.snapshot(legacyEnvPath); err != nil {
		return Target{}, err
	}
	if priorEnv != nil {
		if err := o.tx.snapshot(envPath); err != nil {
			return Target{}, err
		}
	}
	if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
		return Target{}, err
	}
	if priorEnv != nil {
		if err := removeEnvFile(*priorEnv); err != nil {
			return Target{}, fmt.Errorf("remove prior Codex environment file: %w", err)
		}
	}
	if legacyEnvPath != envPath && exists(legacyEnvPath) {
		if err := os.Remove(legacyEnvPath); err != nil {
			return Target{}, fmt.Errorf("remove legacy Codex environment file: %w", err)
		}
	}

	t := Target{
		Harness: "codex",
		Files:   []string{path},
		Note:    "codex stores the gateway credential in config.toml",
	}
	// The provider table's ownership carries the exact authored table and any
	// pre-existing table it replaced, so removal restores a user's manual
	// table byte-for-byte and never discards an edit the user made since.
	configEntry := writtenEntry{Path: path,
		TomlTable:    "[model_providers." + ProviderID + "]",
		TomlAuthored: table, TomlPrior: tomlPrior}
	if tomlCreated {
		// Whole-file ownership, but keeping the table info so a file the user
		// later diverges is reverted surgically instead of deleted wholesale.
		configEntry.CreatedFile = true
	}
	t.Ledger = append([]writtenEntry{configEntry}, added...)
	return t, nil
}

func priorTomlTopEntry(entries []writtenEntry, path, key, value string) (writtenEntry, bool) {
	for _, entry := range entries {
		if filepath.Clean(entry.Path) == filepath.Clean(path) &&
			entry.TomlTop == key && entry.TomlVal == value {
			return entry, true
		}
	}
	return writtenEntry{}, false
}

func (codexWriter) remove(home, _ string) (Target, error) {
	path := filepath.Join(home, ".codex", "config.toml")
	t := Target{Harness: "codex", Files: []string{path}}
	loaded, err := loadRecord(home)
	if err != nil {
		return t, err
	}
	ent := loaded.Targets["codex"]
	if len(ent.Ledger) == 0 {
		return t, errNoRecord("codex")
	}
	var configLedger []writtenEntry
	var envEntry *writtenEntry
	created := false
	var createdContent string
	for i := range ent.Ledger {
		e := ent.Ledger[i]
		if e.EnvFile {
			ee := e
			envEntry = &ee
			continue
		}
		if e.CreatedFile {
			created = true
			createdContent = e.CreatedContent
		}
		configLedger = append(configLedger, e)
	}
	// Refuse a symlink swapped in after apply before any write. The retired
	// env file is checked only when its ledger proves this injection owns it.
	if err := refuseSymlink(path); err != nil {
		return t, err
	}
	if envEntry != nil {
		if err := refuseSymlink(envEntry.Path); err != nil {
			return t, err
		}
	}
	switch {
	case created && !exists(path):
		t.Note = "removed the config.toml we created"
	case created:
		cur, rerr := os.ReadFile(path)
		if rerr != nil {
			return t, rerr
		}
		if createdFileUnchanged(cur, createdContent) {
			if err := os.Remove(path); err != nil {
				return t, err
			}
			t.Note = "removed the config.toml we created"
		} else {
			// The file diverged after we created it: excise our table and the
			// value-guarded defaults only, so the user's edits survive.
			_, conflicts, err := removeWritten(configLedger)
			if err != nil {
				return t, err
			}
			t.Note = "kept your edits; reverted the injected provider table"
			if len(conflicts) > 0 {
				t.Note = "kept your edited provider table (conflict); reverted only the surrounding defaults"
			}
		}
	default:
		// Same discipline as the JSON writers: excise the table and revert the
		// default keys, but only while their values are still ours.
		undone, conflicts, err := removeWritten(configLedger)
		if err != nil {
			return t, err
		}
		t.Note = "reverted " + fmt.Sprint(len(undone)) + " injected item(s)"
		if len(conflicts) > 0 {
			t.Note += "; kept your edited provider table (conflict)"
		}
	}
	if envEntry != nil {
		if err := removeEnvFile(*envEntry); err != nil {
			return t, err
		}
	}
	return t, nil
}

// removeWritten calls back into this via the TomlTable case below; tomlTableRange
// finds a top-level table's header through the next header.
func tomlTableRange(text, header string) (int, int, bool) {
	lines := strings.SplitAfter(text, "\n")
	var off int
	start := -1
	for _, ln := range lines {
		body := strings.TrimSpace(strings.TrimRight(ln, "\n"))
		if start < 0 {
			if body == header {
				start = off
			}
		} else if strings.HasPrefix(body, "[") {
			return start, off, true
		}
		off += len(ln)
	}
	if start >= 0 {
		return start, len(text), true
	}
	return 0, 0, false
}

// lineHasKey reports whether a TOML top-level key is already set. Detection
// stops at the first table header: in TOML a top-level key must precede every
// `[table]`, so a `model = …` nested inside `[tui]` is a different key and
// must not suppress writing the real top-level default.
func lineHasKey(text, key string) bool {
	for _, ln := range strings.Split(text, "\n") {
		body := strings.TrimSpace(ln)
		if strings.HasPrefix(body, "#") {
			continue
		}
		if strings.HasPrefix(body, "[") {
			return false // entered a table; top-level scope has ended
		}
		if k, _, found := strings.Cut(body, "="); found && strings.TrimSpace(k) == key {
			return true
		}
	}
	return false
}

func rewriteExactTOMLValue(text, key, oldValue, newValue string) (string, bool) {
	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		body := strings.TrimSpace(strings.TrimRight(line, "\n"))
		if body != key+` = "`+oldValue+`"` {
			continue
		}
		lines[i] = strings.Replace(line, `"`+oldValue+`"`, `"`+newValue+`"`, 1)
		return strings.Join(lines, ""), true
	}
	return text, false
}

// ── OpenCode ───────────────────────────────────────────────────────────────

type opencodeWriter struct{}

func (opencodeWriter) apply(o Options) (Target, error) {
	path := filepath.Join(o.Home, ".config", "opencode", "opencode.json")

	models := map[string]any{}
	for _, m := range o.Models {
		models[m.ID] = map[string]any{
			"name":       m.Name,
			"context":    m.Context,
			"maxTokens":  m.MaxTokens,
			"modalities": map[string]any{"input": []string{"text"}, "output": []string{"text"}},
		}
	}
	entry := map[string]any{
		"npm":  "@ai-sdk/openai-compatible",
		"name": ProviderName,
		"options": map[string]any{
			"baseURL": o.BaseURL,
			"apiKey":  o.Token,
		},
		"models": models,
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return Target{}, err
	}

	var led writtenEntry
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		led = mergeContainer(obj, "provider", []kv{{ProviderID, blob}}, legacyProviderID)
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	led.Path = path
	if created {
		led.CreatedFile = true
	}
	return Target{Harness: "opencode", Files: []string{path},
		Note: "pick any Prowl model in opencode", Ledger: []writtenEntry{led}}, nil
}

func (opencodeWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, "opencode",
		filepath.Join(home, ".config", "opencode", "opencode.json"))
}

// ── Prowl-legacy ─────────────────────────────────────────────────────────────
//
// The standalone Prowl-legacy CLI reads providers.<id> = {id,name,base_url,
// api_key,type,models[]} from JSON at ~/.local/share/prowl/prowl.json. Writing
// it makes the gateway selectable in that CLI's own picker - a routing target,
// not recursion, because requests arrive at the /v1 plane as any client's do.
// The retired `prowl` harness id survives only as a remove-only alias
// (prowlAliasWriter) that cleans up ledger state written before the rename.

func prowlLegacyPath(home string) string {
	return filepath.Join(home, ".local", "share", "prowl", "prowl.json")
}

type prowlLegacyWriter struct{}

func (prowlLegacyWriter) apply(o Options) (Target, error) {
	path := prowlLegacyPath(o.Home)

	type modelEntry struct {
		ID              string   `json:"id"`
		Name            string   `json:"name"`
		ContextWindow   int      `json:"context_window"`
		DefaultMaxToken int      `json:"default_max_tokens"`
		CanReason       bool     `json:"can_reason"`
		ReasoningLevels []string `json:"reasoning_levels,omitempty"`
	}
	models := make([]modelEntry, 0, len(o.Models))
	for _, m := range o.Models {
		me := modelEntry{ID: m.ID, Name: m.Name,
			ContextWindow: m.Context, DefaultMaxToken: m.MaxTokens}
		if m.ID == "auto" || m.ID == "auto:smart" {
			me.CanReason = true
			me.ReasoningLevels = []string{"low", "medium", "high"}
		}
		models = append(models, me)
	}
	entry := map[string]any{
		"id":       ProviderID,
		"name":     ProviderName,
		"base_url": o.BaseURL,
		"api_key":  o.Token,
		"type":     "openai-compat",
		"models":   models,
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return Target{}, err
	}
	var led writtenEntry
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		led = mergeContainer(obj, "providers", []kv{{ProviderID, blob}}, legacyProviderID)
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	led.Path = path
	if created {
		led.CreatedFile = true
	}
	return Target{Harness: prowlLegacyHarness, Files: []string{path},
		Note:   "the gateway now appears in prowl-legacy's model picker",
		Ledger: []writtenEntry{led}}, nil
}

func (prowlLegacyWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, prowlLegacyHarness, prowlLegacyPath(home))
}

// prowlAliasWriter is the retired `prowl` harness id: apply refuses (the id was
// renamed to prowl-legacy), remove reverts whatever the old id recorded so
// `--remove prowl` still cleans up a pre-rename injection.
type prowlAliasWriter struct{}

func (prowlAliasWriter) apply(Options) (Target, error) {
	return Target{}, fmt.Errorf(
		"`prowl` was renamed to `prowl-legacy`; inject that instead " +
			"(`--remove prowl` still cleans up an injection made under the old id)")
}

func (prowlAliasWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, prowlAliasHarness, prowlLegacyPath(home))
}
