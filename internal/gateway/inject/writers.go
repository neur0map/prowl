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
// ~/.codex/config.toml and authenticates with a key taken from the environment
// variable named by env_key - it cannot store the key itself, so injection
// also writes a sourceable env file and says so. The top-level `model_provider`
// and `model` keys are set only when the user has not pinned their own:
// silently replacing someone's default model is exactly the overreach the
// ledger discipline exists to prevent.

type codexWriter struct{}

const codexEnvKey = "PROWL_GATEWAY_API_KEY"

func (codexWriter) apply(o Options) (Target, error) {
	path := filepath.Join(o.Home, ".codex", "config.toml")
	envPath := filepath.Join(o.Home, ".codex", "prowl.env")
	legacyEnvPath := filepath.Join(o.Home, ".codex", legacyProviderID+".env")

	table := "[model_providers." + ProviderID + "]\n" +
		"name = \"" + ProviderName + "\"\n" +
		"base_url = \"" + o.BaseURL + "\"\n" +
		"env_key = \"" + codexEnvKey + "\"\n" +
		"wire_api = \"responses\"\n"

	// Refuse a symlinked env before touching anything: writing through it would
	// clobber the link target. writeBackup enforces the same for the config.
	if err := refuseSymlink(envPath); err != nil {
		return Target{}, err
	}
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
	// Defaults, only when the user has not set them: silently replacing
	// someone's default model or provider is not an addition, it is a hijack.
	if !lineHasKey(text, "model_provider") {
		text = "model_provider = \"" + ProviderID + "\"\n" + text
		added = append(added, writtenEntry{Path: path, TomlTop: "model_provider", TomlVal: "\"" + ProviderID + "\""})
	}
	if !lineHasKey(text, "model") {
		text = "model = \"auto\"\n" + text
		added = append(added, writtenEntry{Path: path, TomlTop: "model", TomlVal: `"auto"`})
	}

	// Capture the env file's pre-apply state so removal restores a pre-existing
	// one instead of destroying it, then snapshot everything this apply mutates
	// (config, env, legacy env) so a partial failure rolls all three back as
	// one transaction.
	envEntry := writtenEntry{Path: envPath, EnvFile: true}
	if exists(envPath) {
		prior, rerr := os.ReadFile(envPath)
		if rerr != nil {
			return Target{}, rerr
		}
		envEntry.EnvPrior = string(prior)
	} else {
		envEntry.EnvCreated = true
	}
	if err := o.tx.snapshot(legacyEnvPath); err != nil {
		return Target{}, err
	}
	if err := o.tx.snapshot(envPath); err != nil {
		return Target{}, err
	}
	if legacyEnvPath != envPath && exists(legacyEnvPath) {
		if err := os.Remove(legacyEnvPath); err != nil {
			return Target{}, fmt.Errorf("remove legacy Codex environment file: %w", err)
		}
	}
	if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
		return Target{}, err
	}
	envContent := "export " + codexEnvKey + "=" + quoteShell(o.Token) + "\n"
	if err := writeFile(envPath, []byte(envContent), 0o600); err != nil {
		return Target{}, err
	}
	envEntry.CreatedContent = envContent

	t := Target{Harness: "codex", Files: []string{path, envPath},
		Note: "codex reads the key from the environment: source " + envPath +
			" (or add it to your shell profile)"}
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
	t.Ledger = append(t.Ledger, envEntry)
	return t, nil
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
	// Refuse a symlink swapped in after apply for either the config or the env
	// file, exactly as apply does, before any write; the error keeps the
	// ledger intact for a retry once the user resolves it to a regular file.
	envPath := filepath.Join(home, ".codex", "prowl.env")
	if err := refuseSymlink(path); err != nil {
		return t, err
	}
	if err := refuseSymlink(envPath); err != nil {
		return t, err
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
	} else if exists(envPath) {
		// A ledger recorded before env entries existed: fall back to the
		// historical delete of the env file we always wrote.
		if err := os.Remove(envPath); err != nil {
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

func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
