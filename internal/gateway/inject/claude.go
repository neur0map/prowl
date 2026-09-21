package inject

import (
	"encoding/json"
	"path/filepath"
)

// ── Claude Code ──────────────────────────────────────────────────────────────
//
// Claude Code reads ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / a model pin
// from the `env` map in settings.json. The token goes as a bearer (ANTHROPIC_AUTH_TOKEN,
// not ..._API_KEY, which is sent as x-api-key); the gateway accepts both.
// The model pin is the gateway's plain `auto` alias; using the harness provider
// id here would ask the gateway for a model that does not exist.
//
// ANTHROPIC_BASE_URL is only honest with the /v1 suffix (the client appends
// /messages), so it points at the gateway root including /v1.

type claudeWriter struct{}

func (claudeWriter) apply(o Options) (Target, error) {
	path := filepath.Join(o.Home, ".claude", "settings.json")
	var entry writtenEntry
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		// mergeContainer records any pre-existing env var we overwrite and a
		// non-object `env` it had to replace, so removal restores them.
		entry = mergeContainer(obj, "env", []kv{
			{"ANTHROPIC_BASE_URL", json.RawMessage(jsonStr(o.BaseURL))},
			{"ANTHROPIC_AUTH_TOKEN", json.RawMessage(jsonStr(o.Token))},
			{"ANTHROPIC_MODEL", json.RawMessage(jsonStr("auto"))},
		})
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	entry.Path = path
	if created {
		entry.CreatedFile = true
	}
	t := Target{Harness: "claude", Files: []string{path},
		Note:   "claude now routes through the gateway; restart claude to pick up settings",
		Ledger: []writtenEntry{entry}}
	return t, nil
}

func (claudeWriter) remove(home, _ string) (Target, error) {
	return removeJSONProvider(home, "claude", filepath.Join(home, ".claude", "settings.json"))
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func errNoRecord(harness string) error {
	return &NoRecordError{Harness: harness}
}

// NoRecordError is returned when Remove is asked for a harness Apply never
// recorded: guessing what to delete from a user config would be worse than
// refusing.
type NoRecordError struct{ Harness string }

func (e *NoRecordError) Error() string {
	return "no injection record for " + e.Harness + " - refusing to guess what to remove"
}
