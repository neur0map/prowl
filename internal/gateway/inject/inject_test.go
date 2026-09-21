package inject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests pin the two contracts the merge-into-foreign-config job rests
// on: injection must not disturb the user's bytes, and removal must revert
// exactly what was written - never a value the user has since changed.

func testHome(t *testing.T) string {
	t.Helper()
	// Apply/Remove take Home explicitly, so a plain temp dir isolates the
	// test from the real ~/.claude and the real ledger.
	return t.TempDir()
}

func optsFor(home, harness string) Options {
	return Options{
		Home:    home,
		BaseURL: "http://127.0.0.1:8788/v1",
		Token:   "prowlag-test00000000000000000000000000000000000000000000000000ff",
		Models:  RoutingModels(),
	}
}

func TestApplyThenRemoveIsByteExact(t *testing.T) {
	t.Parallel()
	home := testHome(t)

	// A settings file the user hand-wrote, in their key order.
	user := "{\n  \"zebra\": true,\n  \"model\": \"opus[1m]\",\n  \"env\": { \"ZED\": \"1\" },\n  \"apple\": 2\n}\n"
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(optsFor(home, ""), "claude"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)

	// The user's untouched keys keep their exact order and bytes; only env
	// re-renders, because Apply edited it.
	if !strings.Contains(after, "\"zebra\": true,\n  \"model\"") {
		t.Errorf("user key order was rewritten by injection:\n%s", after)
	}
	if !strings.Contains(after, "\"ZED\": \"1\"") {
		t.Errorf("existing env entry was dropped:\n%s", after)
	}
	if !strings.Contains(after, "ANTHROPIC_BASE_URL") {
		t.Errorf("env was not injected:\n%s", after)
	}
	if !strings.Contains(after, `"ANTHROPIC_MODEL": "auto"`) {
		t.Errorf("Claude was pinned to a provider id instead of the routable auto alias:\n%s", after)
	}

	if _, err := Remove(home, "claude"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	restored := read(t, path)
	var want, got any
	if err := json.Unmarshal([]byte(user), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(restored), &got); err != nil {
		t.Fatalf("restored file is not JSON: %v\n%s", err, restored)
	}
	if !jsonEq(mustMarshal(t, want), mustMarshal(t, got)) {
		t.Errorf("removal did not restore the user's content:\ngot:\n%s\nwant:\n%s", restored, user)
	}
	// The env mapping we created must not linger as an empty object.
	if strings.Contains(restored, `"env": {}`) {
		t.Errorf("removal left an empty env mapping:\n%s", restored)
	}
}

func TestRemoveNeverRevertsAUserEdit(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(optsFor(home, ""), "claude"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The user repoints the base URL at another gateway after injection.
	body := read(t, path)
	edited := strings.Replace(body, "http://127.0.0.1:8788/v1", "http://127.0.0.1:9999/v1", 1)
	if edited == body {
		t.Fatal("edit did not apply")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(home, "claude"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, "http://127.0.0.1:9999/v1") {
		t.Errorf("removal rolled back the user's edit; the changed value must survive:\n%s", after)
	}
	// The value the user never touched is still ours to remove.
	if strings.Contains(after, "ANTHROPIC_MODEL") {
		t.Errorf("untouched injected keys were not reverted:\n%s", after)
	}
}

func TestReapplyIsIdempotentAndKeepsOwnership(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	// No pre-existing file: the first apply creates it, and that fact must
	// survive a re-apply so Remove deletes rather than edits.
	path := filepath.Join(home, ".omp", "agent", "models.yml")

	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := read(t, path)
	if !strings.Contains(first, "prowl:") {
		t.Fatalf("provider missing:\n%s", first)
	}
	if strings.Contains(first, "discovery") {
		t.Error("the injected provider must not enable catalogue discovery (picker flood)")
	}

	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second := read(t, path)
	if strings.Count(second, "prowl:") != 1 {
		t.Errorf("re-apply duplicated the provider block:\n%s", second)
	}

	if _, err := Remove(home, "omp"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a file injection created must be deleted by removal; still present:\n%s", read(t, path))
	}
}

func TestCodexNeverOverwritesExistingDefaults(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	user := "model = \"gpt-5.6-sol\"\nmodel_reasoning_effort = \"high\"\n\n[tui]\nx = 1\n"
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, "\nmodel = \"gpt-5.6-sol\"\n") && !strings.HasPrefix(after, "model = \"gpt-5.6-sol\"") {
		t.Errorf("the user's model pin was replaced:\n%s", after)
	}
	if strings.Count(after, "model = ") != 1 {
		t.Errorf("a second model key was written:\n%s", after)
	}
	if strings.Contains(after, "model = \"auto\"") {
		t.Errorf("a default was added beside the user's own model key:\n%s", after)
	}
	// A top-level key must precede every [table] or the file is invalid TOML.
	if idx := strings.Index(after, "model_provider"); idx > strings.Index(after, "[tui]") {
		t.Errorf("model_provider was appended inside/after a table (invalid TOML):\n%s", after)
	}

	if _, err := Remove(home, "codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	restored := read(t, path)
	if strings.TrimSpace(restored) != strings.TrimSpace(user) {
		t.Errorf("codex removal left residue:\ngot:\n%s\nwant:\n%s", restored, user)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prowl.env")); !os.IsNotExist(err) {
		t.Error("the env file injection wrote survived removal")
	}
}

func TestApplyMigratesLegacyProviderIdentity(t *testing.T) {
	home := testHome(t)
	ompPath := filepath.Join(home, ".omp", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(ompPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyOMP := "providers:\n  prowl-agent-gateway:\n    baseUrl: http://old/v1\n    models: []\n  other:\n    baseUrl: http://other/v1\n"
	if err := os.WriteFile(ompPath, []byte(legacyOMP), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("migrate OMP: %v", err)
	}
	migratedOMP := read(t, ompPath)
	if strings.Contains(migratedOMP, "prowl-agent-gateway:") ||
		strings.Count(migratedOMP, "  prowl:") != 1 ||
		!strings.Contains(migratedOMP, "  other:") {
		t.Fatalf("OMP provider migration was not clean:\n%s", migratedOMP)
	}

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCodex := "model_provider = \"prowl-agent-gateway\"\nmodel = \"auto\"\n\n[model_providers.prowl-agent-gateway]\nname = \"Prowl Agent Gateway\"\nbase_url = \"http://old/v1\"\n"
	if err := os.WriteFile(codexPath, []byte(legacyCodex), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyEnv := filepath.Join(home, ".codex", "prowl-agent-gateway.env")
	if err := os.WriteFile(legacyEnv, []byte("old secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("migrate Codex: %v", err)
	}
	migratedCodex := read(t, codexPath)
	if strings.Contains(migratedCodex, "prowl-agent-gateway") ||
		!strings.Contains(migratedCodex, `model_provider = "prowl"`) ||
		!strings.Contains(migratedCodex, "[model_providers.prowl]") {
		t.Fatalf("Codex provider migration was not clean:\n%s", migratedCodex)
	}
	if _, err := os.Stat(legacyEnv); !os.IsNotExist(err) {
		t.Fatalf("legacy Codex environment file survived migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prowl.env")); err != nil {
		t.Fatalf("new Codex environment file missing: %v", err)
	}
}

func TestRemoveWithoutRecordRefuses(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	if _, err := Remove(home, "claude"); err == nil {
		t.Fatal("removal with no ledger must refuse rather than guess")
	}
}

func TestModelsListIsRoutingOnly(t *testing.T) {
	t.Parallel()
	models := RoutingModels()
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m.ID] {
			t.Fatalf("duplicate model id %q in the picker list", m.ID)
		}
		seen[m.ID] = true
		if !strings.HasPrefix(m.ID, "auto") {
			t.Errorf("picker offers %q, which is not a routing alias", m.ID)
		}
	}
	if !seen["auto"] || !seen["auto:cheap"] || !seen["auto:balanced"] {
		t.Errorf("routing axes missing: %v", seen)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(blob)
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// The three writers added for Pi, Hermes, and OpenClaw share the same two
// contracts every writer here rests on: the picker carries only the routing
// aliases, and Apply then Remove is exact for foreign config - never a byte a
// user changed after injection.

func TestSupportedListsNewHarnesses(t *testing.T) {
	t.Parallel()
	sup := Supported()
	for _, h := range []string{"pi", "hermes", "openclaw"} {
		if !containsHarness(sup, h) {
			t.Errorf("Supported() omits %q: %v", h, sup)
		}
		if _, err := writerFor(h); err != nil {
			t.Errorf("no writer wired for %q: %v", h, err)
		}
	}
}

func newHarnessPath(home, harness string) string {
	switch harness {
	case "pi":
		return filepath.Join(home, ".pi", "agent", "models.json")
	case "hermes":
		return filepath.Join(home, ".hermes", "config.yaml")
	case "openclaw":
		return filepath.Join(home, ".openclaw", "agents", "main", "agent", "models.json")
	}
	return ""
}

// TestNewHarnessCreateApplyRemove proves the created-from-scratch path: a first
// apply writes a routing-only picker, a re-apply does not duplicate it, and a
// file this package created is deleted (not edited) on removal.
func TestNewHarnessCreateApplyRemove(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"pi", "hermes", "openclaw"} {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			path := newHarnessPath(home, harness)

			if _, err := Apply(optsFor(home, ""), harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			first := read(t, path)
			if !strings.Contains(first, ProviderID+":") && !strings.Contains(first, `"`+ProviderID+`"`) {
				t.Fatalf("provider missing:\n%s", first)
			}
			if strings.Contains(first, "discovery") || strings.Contains(first, "discover_models: true") {
				t.Errorf("the injected provider must not enable catalogue discovery:\n%s", first)
			}
			assertAliasesOnly(t, harness, first)

			if _, err := Apply(optsFor(home, ""), harness); err != nil {
				t.Fatalf("re-apply: %v", err)
			}
			if n := providerCount(harness, read(t, path)); n != 1 {
				t.Errorf("re-apply produced %d provider blocks, want 1:\n%s", n, read(t, path))
			}

			if _, err := Remove(home, harness); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("a file injection created must be deleted by removal; still present:\n%s", read(t, path))
			}
		})
	}
}

// TestPiOpenClawMergeIsExactForForeignConfig proves the JSON writers merge into
// an existing providers map without disturbing a foreign provider, and that
// removal restores the file logically byte-for-byte.
func TestPiOpenClawMergeIsExactForForeignConfig(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"pi", "openclaw"} {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			path := newHarnessPath(home, harness)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			user := "{\n  \"providers\": {\n    \"ollama\": { \"baseUrl\": \"http://localhost:11434/v1\", \"api\": \"openai-completions\" }\n  },\n  \"mode\": \"merge\"\n}\n"
			if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := Apply(optsFor(home, ""), harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			after := read(t, path)
			if !strings.Contains(after, "\"ollama\"") {
				t.Errorf("foreign provider was dropped by injection:\n%s", after)
			}
			assertAliasesOnly(t, harness, after)

			if _, err := Remove(home, harness); err != nil {
				t.Fatalf("remove: %v", err)
			}
			restored := read(t, path)
			var want, got any
			if err := json.Unmarshal([]byte(user), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(restored), &got); err != nil {
				t.Fatalf("restored file is not JSON: %v\n%s", err, restored)
			}
			if !jsonEq(mustMarshal(t, want), mustMarshal(t, got)) {
				t.Errorf("removal did not restore the user's content:\ngot:\n%s\nwant:\n%s", restored, user)
			}
			if strings.Contains(restored, `"`+ProviderID+`"`) {
				t.Errorf("the gateway provider was not removed:\n%s", restored)
			}
		})
	}
}

// TestHermesMergeIsByteExactForForeignConfig proves the YAML writer inserts the
// provider block into an existing providers mapping, leaves every other key
// byte-identical, records discover_models:false, and excises exactly its block
// on removal.
func TestHermesMergeIsByteExactForForeignConfig(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := newHarnessPath(home, "hermes")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "model:\n  default: claude-opus-4\nproviders:\n  openrouter:\n    api: https://openrouter.ai/api/v1\n    api_key: sk-or-x\nterminal:\n  backend: local\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Apply(optsFor(home, ""), "hermes"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, "  "+ProviderID+":") {
		t.Fatalf("provider block not written:\n%s", after)
	}
	if !strings.Contains(after, "discover_models: false") {
		t.Errorf("discover_models:false not written (catalogue would flood the picker):\n%s", after)
	}
	if !strings.Contains(after, "  openrouter:") || !strings.Contains(after, "backend: local") {
		t.Errorf("a foreign key was disturbed by injection:\n%s", after)
	}
	assertAliasesOnly(t, "hermes", after)

	if _, err := Remove(home, "hermes"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if restored := read(t, path); restored != user {
		t.Errorf("hermes removal was not byte-exact:\ngot:\n%q\nwant:\n%q", restored, user)
	}
}

func containsHarness(list []string, want string) bool {
	for _, h := range list {
		if h == want {
			return true
		}
	}
	return false
}

func providerCount(harness, cfg string) int {
	if harness == "hermes" {
		return strings.Count(cfg, "  "+ProviderID+":")
	}
	return strings.Count(cfg, `"`+ProviderID+`":`)
}

// aliasIDs extracts the model ids the picker exposes from a written config.
func aliasIDs(t *testing.T, harness, cfg string) []string {
	t.Helper()
	if harness == "hermes" {
		var ids []string
		for _, ln := range strings.Split(cfg, "\n") {
			s := strings.TrimSpace(ln)
			if strings.HasPrefix(s, "- ") {
				ids = append(ids, strings.Trim(strings.TrimPrefix(s, "- "), `"`))
			}
		}
		return ids
	}
	var doc struct {
		Providers map[string]struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatalf("parse %s config: %v\n%s", harness, err, cfg)
	}
	var ids []string
	for _, m := range doc.Providers[ProviderID].Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// assertAliasesOnly proves the picker exposes exactly the auto* routing aliases
// and nothing else - no catalogue model leaked in.
func assertAliasesOnly(t *testing.T, harness, cfg string) {
	t.Helper()
	ids := aliasIDs(t, harness, cfg)
	if len(ids) == 0 {
		t.Fatalf("%s wrote no models:\n%s", harness, cfg)
	}
	want := map[string]bool{}
	for _, m := range RoutingModels() {
		want[m.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !strings.HasPrefix(id, "auto") {
			t.Errorf("%s picker offers %q, which is not a routing alias", harness, id)
		}
		if !want[id] {
			t.Errorf("%s picker offers unexpected model %q", harness, id)
		}
		seen[id] = true
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s picker is missing routing alias %q", harness, id)
		}
	}
}

// TestRemoveCreatedFileKeepsUserEditsJSON proves the created-file path is not a
// blanket delete: once the user adds their own provider to a models.json we
// created, removal excises only our key and leaves the file - and their
// provider - in place.
func TestRemoveCreatedFileKeepsUserEditsJSON(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"pi", "openclaw"} {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			path := newHarnessPath(home, harness)

			if _, err := Apply(optsFor(home, ""), harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			// The user adds a provider of their own after injection. Re-marshalling
			// diverges the bytes from what we authored, exactly as a hand-edit does.
			var doc map[string]any
			if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
				t.Fatalf("parse created config: %v", err)
			}
			provs, ok := doc["providers"].(map[string]any)
			if !ok {
				t.Fatalf("providers map missing:\n%s", read(t, path))
			}
			provs["ollama"] = map[string]any{
				"baseUrl": "http://localhost:11434/v1", "api": "openai-completions",
			}
			edited, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, edited, 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := Remove(home, harness); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("removal deleted a file the user had edited: %v", err)
			}
			after := read(t, path)
			if !strings.Contains(after, `"ollama"`) {
				t.Errorf("the user's own provider was destroyed by removal:\n%s", after)
			}
			if strings.Contains(after, `"`+ProviderID+`"`) {
				t.Errorf("our provider key was not reverted:\n%s", after)
			}
		})
	}
}

// TestRemoveCreatedFileKeepsUserEditsHermes proves the same for the YAML
// created-file path: an appended user section survives, only our block is cut,
// and the file is not deleted.
func TestRemoveCreatedFileKeepsUserEditsHermes(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := newHarnessPath(home, "hermes")

	if _, err := Apply(optsFor(home, ""), "hermes"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	created := read(t, path)
	edited := created + "\nterminal:\n  backend: local\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(home, "hermes"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("removal deleted a file the user had edited: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, "terminal:") || !strings.Contains(after, "backend: local") {
		t.Errorf("the user's appended section was destroyed by removal:\n%s", after)
	}
	if strings.Contains(after, "  "+ProviderID+":") {
		t.Errorf("our provider block was not removed:\n%s", after)
	}
}

// TestOMPRemoveReportsWriteFailure proves a failed excision propagates: Remove
// must return the error so it does not forget ownership while the block is
// still in the file. A directory at the writer's temp path makes the atomic
// write fail for any user, including root.
func TestOMPRemoveReportsWriteFailure(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".omp", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-existing foreign provider forces a merge, so removal excises the
	// block (the write path under test) rather than deleting the file.
	user := "providers:\n  other:\n    baseUrl: http://other/v1\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(home, "omp"); err == nil {
		t.Fatal("removal must report the write failure, not swallow it")
	}
	loadedOMP, ompErr := loadRecord(home)
	if ompErr != nil {
		t.Fatal(ompErr)
	}
	if _, ok := loadedOMP.Targets["omp"]; !ok {
		t.Error("ownership was forgotten despite the failed write")
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), "  "+ProviderID+":") {
		t.Errorf("the block was reported removed but is gone from the file:\n%s", read(t, path))
	}
}

// TestBlockMatchIgnoresNestedProwlKey proves the block matcher touches only the
// provider that is a direct child of the top-level providers mapping - a
// coincidental nested `prowl:` key elsewhere is left byte-for-byte intact
// through both apply and remove.
func TestBlockMatchIgnoresNestedProwlKey(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"omp", "hermes"} {
		harness := harness
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			var path string
			if harness == "omp" {
				path = filepath.Join(home, ".omp", "agent", "models.yml")
			} else {
				path = newHarnessPath(home, harness)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			// `prowl:` here is a leaf under other.extra, not a provider.
			user := "providers:\n  other:\n    baseUrl: http://other/v1\n    extra:\n      prowl: keepme\n"
			if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := Apply(optsFor(home, ""), harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			after := read(t, path)
			if !strings.Contains(after, "prowl: keepme") {
				t.Errorf("apply mangled the unrelated nested prowl key:\n%s", after)
			}
			if !strings.Contains(after, "  "+ProviderID+":") {
				t.Errorf("apply did not add the real provider block:\n%s", after)
			}

			if _, err := Remove(home, harness); err != nil {
				t.Fatalf("remove: %v", err)
			}
			restored := read(t, path)
			if restored != user {
				t.Errorf("apply+remove was not byte-exact around the nested key:\ngot:\n%q\nwant:\n%q", restored, user)
			}
		})
	}
}

// TestProwlLegacyAppliesAndRemoves proves the retargeted writer: prowl-legacy
// is an advertised, detectable target that writes the standalone CLI's
// provider file and reverts it.
func TestProwlLegacyAppliesAndRemoves(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	if !containsHarness(Supported(), "prowl-legacy") {
		t.Error("prowl-legacy must be an advertised inject target")
	}
	path := filepath.Join(home, ".local", "share", "prowl", "prowl.json")
	if _, err := Apply(optsFor(home, ""), "prowl-legacy"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, `"`+ProviderID+`"`) || !strings.Contains(after, `"openai-compat"`) {
		t.Fatalf("prowl-legacy provider not written:\n%s", after)
	}
	if !containsHarness(Installed(home), "prowl-legacy") {
		t.Error("prowl-legacy with its data dir must be reported installed")
	}
	if _, err := Remove(home, "prowl-legacy"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if exists(path) {
		t.Errorf("a file prowl-legacy created must be deleted on removal:\n%s", read(t, path))
	}
}

// TestProwlAliasIsRemoveOnly proves the retired `prowl` id is not advertised or
// applied but still cleans up a pre-rename injection.
func TestProwlAliasIsRemoveOnly(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	if containsHarness(Supported(), "prowl") {
		t.Error("the retired `prowl` id must not be advertised")
	}
	if err := os.MkdirAll(filepath.Join(home, ".local", "share", "prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if containsHarness(Installed(home), "prowl") {
		t.Error("the retired `prowl` id must not be reported installed")
	}
	if _, err := Apply(optsFor(home, ""), "prowl"); err == nil {
		t.Error("applying the retired `prowl` id must be rejected")
	}
	path := filepath.Join(home, ".local", "share", "prowl", "prowl.json")
	if exists(path) {
		t.Error("a rejected apply must not write prowl.json")
	}
	loadedAlias, aliasErr := loadRecord(home)
	if aliasErr != nil {
		t.Fatal(aliasErr)
	}
	if _, ok := loadedAlias.Targets["prowl"]; ok {
		t.Error("a rejected apply must not record an injection")
	}
	// A pre-rename injection is still cleanable via --remove prowl.
	if err := os.WriteFile(path, []byte("{\n  \"providers\": {}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(home, Target{Harness: "prowl", Files: []string{path},
		Ledger: []writtenEntry{{Path: path, CreatedFile: true}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(home, "prowl"); err != nil {
		t.Fatalf("cleanup remove must still work: %v", err)
	}
	if exists(path) {
		t.Error("legacy injection file was not cleaned up")
	}
}

// TestProwlLegacyMigratesPriorProwlOwnership proves that applying prowl-legacy
// absorbs ownership recorded under the old `prowl` id, so neither id strands
// the other and removal still deletes the file it owns.
func TestProwlLegacyMigratesPriorProwlOwnership(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".local", "share", "prowl", "prowl.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\n  \"providers\": {}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveRecord(home, Target{Harness: "prowl", Files: []string{path},
		Ledger: []writtenEntry{{Path: path, CreatedFile: true}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "prowl-legacy"); err != nil {
		t.Fatalf("apply prowl-legacy: %v", err)
	}
	rec, recErr := loadRecord(home)
	if recErr != nil {
		t.Fatal(recErr)
	}
	if _, ok := rec.Targets["prowl"]; ok {
		t.Error("the old `prowl` ledger entry must be absorbed, not left to strand")
	}
	leg, ok := rec.Targets["prowl-legacy"]
	if !ok || len(leg.Ledger) == 0 || !leg.Ledger[0].CreatedFile {
		t.Fatalf("prowl-legacy must inherit whole-file ownership: %+v", leg)
	}
	if _, err := Remove(home, "prowl-legacy"); err != nil {
		t.Fatalf("remove prowl-legacy: %v", err)
	}
	if exists(path) {
		t.Error("migrated ownership did not delete the created file on removal")
	}
}

// TestRemoveRestoresShadowedProviderKey proves removal gives back a provider
// entry the user already had under our key, instead of deleting theirs.
func TestRemoveRestoresShadowedProviderKey(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "{\n  \"providers\": {\n    \"prowl\": { \"baseUrl\": \"http://mine/v1\", \"api\": \"custom\" }\n  }\n}\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "pi"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if strings.Contains(read(t, path), "http://mine/v1") {
		t.Fatalf("apply should have shadowed the user's own prowl provider:\n%s", read(t, path))
	}
	if _, err := Remove(home, "pi"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	restored := read(t, path)
	if !strings.Contains(restored, "http://mine/v1") || !strings.Contains(restored, `"custom"`) {
		t.Errorf("removal did not restore the user's shadowed provider:\n%s", restored)
	}
}

// TestRemoveRestoresShadowedClaudeEnv proves the same for Claude's env map: a
// pre-existing ANTHROPIC_MODEL is restored, unrelated keys survive.
func TestRemoveRestoresShadowedClaudeEnv(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "{\n  \"env\": { \"ANTHROPIC_MODEL\": \"opus\", \"KEEP\": \"1\" }\n}\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "claude"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := Remove(home, "claude"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	restored := read(t, path)
	if !strings.Contains(restored, `"opus"`) {
		t.Errorf("the user's own ANTHROPIC_MODEL was not restored:\n%s", restored)
	}
	if !strings.Contains(restored, `"KEEP"`) {
		t.Errorf("an unrelated env key was dropped:\n%s", restored)
	}
	if strings.Contains(restored, "ANTHROPIC_BASE_URL") {
		t.Errorf("the keys we added were not reverted:\n%s", restored)
	}
}

// TestRemoveRestoresNonObjectContainer proves a provider container that was not
// an object is restored verbatim rather than silently destroyed.
func TestRemoveRestoresNonObjectContainer(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "{\n  \"providers\": \"see other file\",\n  \"mode\": \"x\"\n}\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "pi"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(read(t, path), `"`+ProviderID+`"`) {
		t.Fatalf("apply did not inject over the non-object container:\n%s", read(t, path))
	}
	if _, err := Remove(home, "pi"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	restored := read(t, path)
	if !strings.Contains(restored, `"see other file"`) {
		t.Errorf("the non-object providers value was not restored:\n%s", restored)
	}
	if strings.Contains(restored, `"`+ProviderID+`":`) {
		t.Errorf("our injected provider was not removed:\n%s", restored)
	}
	if !strings.Contains(restored, `"mode"`) {
		t.Errorf("an unrelated key was lost:\n%s", restored)
	}
}

// TestCreatedFileKeepsUserEditsClaudeAndOMP extends the created-file divergence
// contract to Claude and OMP: a file we created but the user later edited is
// preserved, with only our own additions reverted.
func TestCreatedFileKeepsUserEditsClaudeAndOMP(t *testing.T) {
	t.Parallel()
	t.Run("claude", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".claude", "settings.json")
		if _, err := Apply(optsFor(home, ""), "claude"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
			t.Fatal(err)
		}
		doc["theme"] = "dark"
		edited, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, edited, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Remove(home, "claude"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("removal deleted an edited file: %v", err)
		}
		after := read(t, path)
		if !strings.Contains(after, `"theme"`) {
			t.Errorf("the user's edit was destroyed:\n%s", after)
		}
		if strings.Contains(after, "ANTHROPIC_BASE_URL") {
			t.Errorf("our env was not reverted:\n%s", after)
		}
	})
	t.Run("omp", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".omp", "agent", "models.yml")
		if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		edited := read(t, path) + "\ndefaults:\n  temperature: 0.7\n"
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Remove(home, "omp"); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("removal deleted an edited file: %v", err)
		}
		after := read(t, path)
		if !strings.Contains(after, "temperature: 0.7") {
			t.Errorf("the user's edit was destroyed:\n%s", after)
		}
		if strings.Contains(after, "  "+ProviderID+":") {
			t.Errorf("our provider block was not removed:\n%s", after)
		}
	})
}

// TestCredentialConfigsArePrivate proves a created credential config is 0600
// and a merge into a group/other-readable file strips those read bits.
func TestCredentialConfigsArePrivate(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	created := filepath.Join(home, ".omp", "agent", "models.yml")
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("apply omp: %v", err)
	}
	if fi, err := os.Stat(created); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("created credential config is %o, want 0600", fi.Mode().Perm())
	}

	merged := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(merged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(merged, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(merged, 0o644); err != nil { // force group/other read despite umask
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "pi"); err != nil {
		t.Fatalf("apply pi: %v", err)
	}
	if fi, err := os.Stat(merged); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o044 != 0 {
		t.Errorf("merged credential config keeps group/other read bits: %o", fi.Mode().Perm())
	}
}

// TestApplyRollsBackOnLedgerFailure proves a config mutation is undone when the
// ownership ledger cannot be persisted, so no unremovable injection is left.
func TestApplyRollsBackOnLedgerFailure(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "{\n  \"theme\": \"dark\"\n}\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	// Wedge the ledger PUBLISH (not the dir creation): a directory sitting at
	// the record's temp path makes the atomic write fail AFTER the config has
	// been mutated, so the snapshot-based rollback is the path under test.
	gwDir := filepath.Join(home, ".local", "share", "prowl-agent", "gateway")
	if err := os.MkdirAll(gwDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(gwDir, "inject.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "claude"); err == nil {
		t.Fatal("apply must fail when the ledger cannot be persisted")
	}
	restored := read(t, path)
	if !strings.Contains(restored, `"theme"`) || strings.Contains(restored, "ANTHROPIC_BASE_URL") {
		t.Errorf("apply did not roll the config back on ledger failure:\n%s", restored)
	}
}

// TestCodexModelDefaultIgnoresNestedTable proves the top-level `model` default
// is written even when a `model` key exists only inside a nested table.
func TestCodexModelDefaultIgnoresNestedTable(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "[tui]\nmodel = \"themed\"\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if !strings.Contains(after, "model = \"auto\"") {
		t.Errorf("top-level model default not added despite only a nested [tui].model:\n%s", after)
	}
	if !strings.Contains(after, "model = \"themed\"") {
		t.Errorf("the nested table's model was altered:\n%s", after)
	}
}

// TestMalformedLedgerIsRejected proves a corrupt ownership record is refused,
// not silently discarded: a fresh apply must fail rather than overwrite the
// ledger and strand every already-recorded harness.
func TestMalformedLedgerIsRejected(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("seed apply omp: %v", err)
	}
	ledger := recordPath(home)
	corrupt := "{ this is not json "
	if err := os.WriteFile(ledger, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "pi"); err == nil {
		t.Fatal("apply over a malformed ledger must fail, not overwrite it")
	}
	if got := read(t, ledger); got != corrupt {
		t.Errorf("malformed ledger was overwritten (recorded ownership stranded):\n%s", got)
	}
	if exists(filepath.Join(home, ".pi", "agent", "models.json")) {
		t.Error("the failed apply left an unrecorded pi injection behind")
	}
	if !exists(filepath.Join(home, ".omp", "agent", "models.yml")) {
		t.Error("the pre-existing omp injection was disturbed")
	}
}

// TestConcurrentApplyKeepsValidLedger proves the cross-process lock serializes
// the ledger read-modify-write: concurrent applies of distinct harnesses all
// land in one valid ledger with none lost to a torn write or a lost update.
func TestConcurrentApplyKeepsValidLedger(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	harnesses := []string{"omp", "pi", "claude", "codex", "opencode", "hermes", "openclaw", "prowl-legacy"}
	var wg sync.WaitGroup
	errs := make([]error, len(harnesses))
	for i, h := range harnesses {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			_, errs[i] = Apply(optsFor(home, ""), h)
		}(i, h)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply %s: %v", harnesses[i], err)
		}
	}
	rec, err := loadRecord(home)
	if err != nil {
		t.Fatalf("ledger is not valid after concurrent applies: %v", err)
	}
	if len(rec.Targets) != len(harnesses) {
		t.Errorf("concurrent applies lost a target: got %d, want %d\n%v",
			len(rec.Targets), len(harnesses), rec.Targets)
	}
	for _, h := range harnesses {
		if _, ok := rec.Targets[h]; !ok {
			t.Errorf("%s was dropped from the ledger by a racing apply", h)
		}
	}
}

// TestRefusesSymlinkConfig proves a symlinked config is refused without
// mutation: the link and its target are left exactly as they were.
func TestRefusesSymlinkConfig(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	real := filepath.Join(home, "real-settings.json")
	body := "{\n  \"theme\": \"dark\"\n}\n"
	if err := os.WriteFile(real, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "claude"); err == nil {
		t.Fatal("apply must refuse a symlinked config")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	if got := read(t, real); got != body {
		t.Errorf("the symlink target was mutated:\n%s", got)
	}
}

// TestCredentialFilesAndBackupsAreOwnerOnly proves both the merged config and
// its backup are clamped to owner-only: a token must never be group- or
// world-readable OR writable, however permissive the user left the file.
func TestCredentialFilesAndBackupsAreOwnerOnly(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil { // group/other write despite umask
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "pi"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("merged credential config keeps group/other bits: %o", fi.Mode().Perm())
	}
	if fi, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("backup missing: %v", err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("credential backup keeps group/other bits: %o", fi.Mode().Perm())
	}
}

// TestReapplyOnDivergedCreatedFileDowngrades proves whole-file ownership is
// downgraded once the user edits a file we created: a re-apply must not keep
// claiming the whole file, so removal excises only our block and the user's
// later edits survive.
func TestReapplyOnDivergedCreatedFileDowngrades(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".omp", "agent", "models.yml")
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	edited := read(t, path) + "\ndefaults:\n  temperature: 0.7\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-apply after the user has diverged the file we created.
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if _, err := Remove(home, "omp"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !exists(path) {
		t.Fatal("removal deleted a file the user had edited after we created it")
	}
	after := read(t, path)
	if !strings.Contains(after, "temperature: 0.7") {
		t.Errorf("the user's edit was destroyed by a whole-file delete:\n%s", after)
	}
	if strings.Contains(after, "  "+ProviderID+":") {
		t.Errorf("our provider block was not reverted:\n%s", after)
	}
}

// TestFailedReapplyRestoresImmediatePreState proves rollback restores the state
// immediately BEFORE the failed apply, not the stale first backup: a re-apply
// that cannot publish its ledger must leave both the earlier injection and the
// user's later edit intact.
func TestFailedReapplyRestoresImmediatePreState(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\n  \"theme\": \"dark\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "claude"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// The user adds a key of their own after the first injection.
	var doc map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	doc["custom"] = "keep"
	edited, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	// Wedge the ledger publish so the second apply fails after mutating.
	gwDir := filepath.Join(home, ".local", "share", "prowl-agent", "gateway")
	if err := os.Mkdir(filepath.Join(gwDir, "inject.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "claude"); err == nil {
		t.Fatal("second apply must fail when the ledger cannot be published")
	}
	after := read(t, path)
	if !strings.Contains(after, `"custom"`) {
		t.Errorf("rollback restored the STALE first backup and lost the user's later edit:\n%s", after)
	}
	if !strings.Contains(after, "ANTHROPIC_BASE_URL") {
		t.Errorf("rollback dropped the earlier injection instead of restoring the immediate pre-state:\n%s", after)
	}
}

// TestCodexPreservesPreexistingEnv proves a pre-existing env file is restored,
// not destroyed: removal must give back the user's own prowl.env, never blindly
// unlink a file they already had.
func TestCodexPreservesPreexistingEnv(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	envPath := filepath.Join(home, ".codex", "prowl.env")
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		t.Fatal(err)
	}
	userEnv := "export FOO=bar\n"
	if err := os.WriteFile(envPath, []byte(userEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := read(t, envPath); !strings.Contains(got, codexEnvKey) {
		t.Fatalf("apply did not write the gateway key into the env file:\n%s", got)
	}
	if _, err := Remove(home, "codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := read(t, envPath); got != userEnv {
		t.Errorf("removal did not restore the user's pre-existing env file:\n%s", got)
	}
}

// TestCodexCreatedConfigDivergenceKeepsEdits proves a config.toml we created is
// reverted surgically once the user edits it, instead of being deleted whole:
// their added table survives, only our provider table and defaults go.
func TestCodexCreatedConfigDivergenceKeepsEdits(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	edited := read(t, path) + "\n[custom]\nx = 1\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(home, "codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !exists(path) {
		t.Fatal("removal deleted a config.toml the user had edited after we created it")
	}
	after := read(t, path)
	if !strings.Contains(after, "[custom]") || !strings.Contains(after, "x = 1") {
		t.Errorf("the user's added table was destroyed:\n%s", after)
	}
	if strings.Contains(after, "[model_providers."+ProviderID+"]") {
		t.Errorf("our provider table was not reverted:\n%s", after)
	}
	if strings.Contains(after, "model_provider = \""+ProviderID+"\"") {
		t.Errorf("our default was not reverted:\n%s", after)
	}
}

// TestCodexApplyRollsBackAllOnEnvFailure proves the config+env mutation is one
// transaction: if the env write fails after the config was written, the config
// is rolled back too rather than left half-injected.
func TestCodexApplyRollsBackAllOnEnvFailure(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "model = \"gpt-5.6-sol\"\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory at the env's temp path makes the env write fail AFTER the
	// config has already been rewritten.
	if err := os.Mkdir(filepath.Join(home, ".codex", "prowl.env.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "codex"); err == nil {
		t.Fatal("apply must fail when the env file cannot be written")
	}
	after := read(t, path)
	if strings.TrimSpace(after) != strings.TrimSpace(user) {
		t.Errorf("config was left mutated after the env write failed:\n%s", after)
	}
	if exists(filepath.Join(home, ".codex", "prowl.env")) {
		t.Error("a partial env file survived the failed apply")
	}
}

// TestProwlLegacyMigratesMergedProwlOwnership proves applying prowl-legacy
// absorbs a MERGED injection recorded under the old `prowl` id - carrying the
// user's ORIGINAL shadowed value - so removal restores the user's provider,
// not our earlier injected one, and the old id no longer strands.
func TestProwlLegacyMigratesMergedProwlOwnership(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".local", "share", "prowl", "prowl.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	oldInjected := `{"base_url":"http://old/v1","type":"openai-compat"}`
	userOriginal := `{"base_url":"http://mine/v1","type":"custom"}`
	onDisk := "{\n  \"providers\": {\n    \"" + ProviderID + "\": " + oldInjected + "\n  }\n}\n"
	if err := os.WriteFile(path, []byte(onDisk), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-rename MERGED injection: on disk our old value shadows the
	// user's, and the ledger records their original as the value to give back.
	if err := saveRecord(home, Target{Harness: prowlAliasHarness, Files: []string{path},
		Ledger: []writtenEntry{{
			Path:        path,
			Container:   "providers",
			Nested:      map[string]string{ProviderID: oldInjected},
			NestedPrior: map[string]string{ProviderID: userOriginal},
		}}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "prowl-legacy"); err != nil {
		t.Fatalf("apply prowl-legacy: %v", err)
	}
	rec, err := loadRecord(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Targets[prowlAliasHarness]; ok {
		t.Error("the old `prowl` merged entry must be absorbed, not left to strand")
	}
	if _, err := Remove(home, "prowl-legacy"); err != nil {
		t.Fatalf("remove prowl-legacy: %v", err)
	}
	var doc struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal([]byte(read(t, path)), &doc); err != nil {
		t.Fatalf("restored file is not JSON: %v", err)
	}
	got, ok := doc.Providers[ProviderID]
	if !ok {
		t.Fatalf("the user's shadowed provider was deleted rather than restored:\n%s", read(t, path))
	}
	if !jsonEq(got, json.RawMessage(userOriginal)) {
		t.Errorf("removal restored our injected value, not the user's original:\ngot:  %s\nwant: %s", got, userOriginal)
	}
}

// TestOMPInsertionNeverEmitsInvalidYAML proves the one structural insertion
// rule handles a providers block with no trailing newline and an empty
// non-block `providers:` form without splicing the block onto another line or
// leaving an inline flow value behind.
func TestOMPInsertionNeverEmitsInvalidYAML(t *testing.T) {
	t.Parallel()
	t.Run("missing final newline", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".omp", "agent", "models.yml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// A providers block whose last line has no trailing newline.
		user := "providers:\n  other:\n    baseUrl: http://other/v1"
		if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		after := read(t, path)
		if strings.Contains(after, "v1  "+ProviderID+":") {
			t.Errorf("the block was spliced onto the last line (invalid YAML):\n%s", after)
		}
		if !strings.Contains(after, "http://other/v1\n  "+ProviderID+":") {
			t.Errorf("our block was not inserted on its own line under providers:\n%s", after)
		}
		if strings.Count(after, "  "+ProviderID+":") != 1 || !strings.Contains(after, "  other:") {
			t.Errorf("insertion did not preserve the user's provider exactly once:\n%s", after)
		}
	})
	t.Run("empty flow providers", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".omp", "agent", "models.yml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		user := "providers: {}\nother: 1\n"
		if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		after := read(t, path)
		if strings.Contains(after, "providers: {}") {
			t.Errorf("the empty flow mapping was left beside our child (invalid YAML):\n%s", after)
		}
		if !strings.Contains(after, "providers:\n  "+ProviderID+":") {
			t.Errorf("providers was not converted to a block mapping with our child:\n%s", after)
		}
		if !strings.Contains(after, "other: 1") {
			t.Errorf("an unrelated top-level key was lost:\n%s", after)
		}
	})
}

// TestRemoveRefusesSymlinkSwappedInAfterApply proves a config swapped for a
// symlink between apply and remove is refused before any write, the link and
// its target are left exactly as they were, and the ownership ledger survives
// so a retry (once the link is resolved) still removes cleanly.
func TestRemoveRefusesSymlinkSwappedInAfterApply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		harness string
		path    func(home string) string
	}{
		{"omp", func(h string) string { return filepath.Join(h, ".omp", "agent", "models.yml") }},
		{"hermes", func(h string) string { return filepath.Join(h, ".hermes", "config.yaml") }},
		{"codex", func(h string) string { return filepath.Join(h, ".codex", "config.toml") }},
		{"claude", func(h string) string { return filepath.Join(h, ".claude", "settings.json") }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			if _, err := Apply(optsFor(home, ""), tc.harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			path := tc.path(home)
			injected := read(t, path)
			// The user relocates the config and leaves a symlink in its place.
			real := filepath.Join(home, tc.harness+"-real")
			if err := os.WriteFile(real, []byte(injected), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
			if _, err := Remove(home, tc.harness); err == nil {
				t.Fatal("remove must refuse a symlinked config")
			}
			fi, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the symlink was replaced by a regular file")
			}
			if got := read(t, real); got != injected {
				t.Errorf("the symlink target was mutated:\n%s", got)
			}
			// The ledger survived the refusal: resolve the link and the retry
			// removes without error.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(injected), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Remove(home, tc.harness); err != nil {
				t.Fatalf("remove after resolving the symlink: %v", err)
			}
		})
	}
}

// TestRemoveRefusesDanglingSymlinkAndRetainsLedger proves a DANGLING symlink
// swapped in after apply is refused rather than read as "already removed": a
// naive ReadFile follows the link, gets os.IsNotExist, and would let Remove
// forget the ledger. The refusal must error and keep the ledger for a retry.
func TestRemoveRefusesDanglingSymlinkAndRetainsLedger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		harness string
		path    func(home string) string
	}{
		{"omp", func(h string) string { return filepath.Join(h, ".omp", "agent", "models.yml") }},
		{"hermes", func(h string) string { return filepath.Join(h, ".hermes", "config.yaml") }},
		{"codex", func(h string) string { return filepath.Join(h, ".codex", "config.toml") }},
		{"claude", func(h string) string { return filepath.Join(h, ".claude", "settings.json") }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()
			home := testHome(t)
			if _, err := Apply(optsFor(home, ""), tc.harness); err != nil {
				t.Fatalf("apply: %v", err)
			}
			path := tc.path(home)
			injected := read(t, path)
			// Point the config at a target that does not exist.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(home, "does-not-exist"), path); err != nil {
				t.Fatal(err)
			}
			if _, err := Remove(home, tc.harness); err == nil {
				t.Fatal("remove must refuse a dangling symlinked config, not treat it as already removed")
			}
			if fi, err := os.Lstat(path); err != nil {
				t.Fatal(err)
			} else if fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the dangling symlink was replaced by a regular file")
			}
			// The ledger survived: resolve the link and the retry removes.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(injected), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Remove(home, tc.harness); err != nil {
				t.Fatalf("remove after resolving the dangling symlink: %v", err)
			}
		})
	}
}

// TestRemoveRestoresManualOMPProviderBlock proves a hand-written prowl provider
// block that apply took over is restored byte-for-byte on removal, with the
// surrounding config untouched.
func TestRemoveRestoresManualOMPProviderBlock(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".omp", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "providers:\n  prowl:\n    baseUrl: http://mine/v1\n    apiKey: mykey\n    models:\n      - id: my-model\n  other:\n    baseUrl: http://other/v1\ndefaults:\n  temperature: 0.5\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if strings.Contains(after, "http://mine/v1") {
		t.Fatalf("apply did not take over the manual prowl block:\n%s", after)
	}
	if !strings.Contains(after, "  other:") || !strings.Contains(after, "temperature: 0.5") {
		t.Errorf("apply disturbed the surrounding config:\n%s", after)
	}
	if _, err := Remove(home, "omp"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if restored := read(t, path); restored != user {
		t.Errorf("apply+remove did not restore the manual block byte-for-byte:\ngot:\n%q\nwant:\n%q", restored, user)
	}
}

// TestRemoveRestoresManualCodexProviderTable proves a hand-written
// [model_providers.prowl] table apply replaced is restored byte-for-byte on
// removal, preserving the user's own pinned model and following table.
func TestRemoveRestoresManualCodexProviderTable(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "model = \"gpt-5.6-sol\"\n\n[model_providers.prowl]\nname = \"My Prowl\"\nbase_url = \"http://mine/v1\"\nenv_key = \"MY_KEY\"\n\n[tui]\nx = 1\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := read(t, path)
	if strings.Contains(after, "http://mine/v1") {
		t.Fatalf("apply did not take over the manual prowl table:\n%s", after)
	}
	if !strings.Contains(after, "[tui]") {
		t.Errorf("apply disturbed a following table:\n%s", after)
	}
	if _, err := Remove(home, "codex"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if restored := read(t, path); restored != user {
		t.Errorf("apply+remove did not restore the manual table byte-for-byte:\ngot:\n%q\nwant:\n%q", restored, user)
	}
}

// TestRemovePreservesUserEditedProviderBlock proves a provider block the user
// edited after apply is preserved on removal (reported as a conflict), never
// overwritten, for both the YAML block and the Codex TOML table paths.
func TestRemovePreservesUserEditedProviderBlock(t *testing.T) {
	t.Parallel()
	t.Run("omp", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".omp", "agent", "models.yml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// A pre-existing file so apply INSERTS our block and records what it
		// authored (the created-file path records no authored bytes).
		user := "providers:\n  other:\n    baseUrl: http://other/v1\n"
		if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		injected := read(t, path)
		edited := strings.Replace(injected, "http://127.0.0.1:8788/v1", "http://127.0.0.1:9999/v1", 1)
		if edited == injected {
			t.Fatal("edit did not apply")
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		tgt, err := Remove(home, "omp")
		if err != nil {
			t.Fatalf("remove: %v", err)
		}
		if after := read(t, path); after != edited {
			t.Errorf("removal overwrote the user's edited block:\ngot:\n%q\nwant:\n%q", after, edited)
		}
		if !strings.Contains(tgt.Note, "conflict") {
			t.Errorf("removal did not report a conflict; note = %q", tgt.Note)
		}
	})
	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		home := testHome(t)
		path := filepath.Join(home, ".codex", "config.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		user := "model = \"gpt-5.6-sol\"\n"
		if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(optsFor(home, ""), "codex"); err != nil {
			t.Fatalf("apply: %v", err)
		}
		injected := read(t, path)
		edited := strings.Replace(injected, "http://127.0.0.1:8788/v1", "http://127.0.0.1:9999/v1", 1)
		if edited == injected {
			t.Fatal("edit did not apply")
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		tgt, err := Remove(home, "codex")
		if err != nil {
			t.Fatalf("remove: %v", err)
		}
		after := read(t, path)
		if !strings.Contains(after, "[model_providers.prowl]") || !strings.Contains(after, "9999") {
			t.Errorf("removal overwrote the user's edited provider table:\n%s", after)
		}
		if !strings.Contains(tgt.Note, "conflict") {
			t.Errorf("removal did not report a conflict; note = %q", tgt.Note)
		}
	})
}

// TestReapplyThenRemoveRestoresOriginalManualBlock proves the prior-content
// ledger survives a re-apply: after two applies over a hand-written prowl
// block, removal still restores the USER's original block, not our earlier
// authored one.
func TestReapplyThenRemoveRestoresOriginalManualBlock(t *testing.T) {
	t.Parallel()
	home := testHome(t)
	path := filepath.Join(home, ".omp", "agent", "models.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "providers:\n  prowl:\n    baseUrl: http://mine/v1\n    models:\n      - id: my-model\n  other:\n    baseUrl: http://other/v1\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := Apply(optsFor(home, ""), "omp"); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if _, err := Remove(home, "omp"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if restored := read(t, path); restored != user {
		t.Errorf("re-apply then remove did not restore the user's original block:\ngot:\n%q\nwant:\n%q", restored, user)
	}
}
