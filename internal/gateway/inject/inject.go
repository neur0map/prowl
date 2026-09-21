// Package inject writes the gateway into a coding harness's own config so the
// harness can route through it: one canonical `auto` model that follows the
// active set and strategy selected in Prowl. It deliberately never exports the
// full catalogue, strategy overrides, or named-set overrides into model pickers.
//
// Every writer is additive and reversible: it manages one named key or one
// marker-bounded block and records exactly what it changed, so `remove`
// restores the file and a hand-edited config is never silently rewritten.
package inject

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ProviderID is the stable provider key shown in harness model pickers.
const ProviderID = "prowl"

// legacyProviderID is recognized only while replacing an injection written by
// the former prowl-agent product name.
const legacyProviderID = "prowl-agent-gateway"

// prowlLegacyHarness is the harness id for the standalone Prowl-legacy CLI,
// whose model picker reads ~/.local/share/prowl/prowl.json.
const prowlLegacyHarness = "prowl-legacy"

// prowlAliasHarness is the retired harness id kept only so `--remove prowl`
// (and the prowl-legacy migration) can reconcile ledger state written before
// the id was renamed to prowl-legacy.
const prowlAliasHarness = "prowl"

// ProviderName is the display name in a model picker.
const ProviderName = "Prowl"

// Model is one picker entry: an id, a label and a conservative envelope. The
// gateway enforces the real per-model limits at dispatch.
type Model struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Context     int    `json:"contextWindow" yaml:"contextWindow"`
	MaxTokens   int    `json:"maxTokens" yaml:"maxTokens"`
}

// RoutingModels returns the single canonical route exposed in harness pickers.
// Explicit auto:<strategy> and auto:<set> routes remain valid API overrides,
// but Prowl's active set and strategy own normal harness routing.
func RoutingModels() []Model {
	return []Model{{
		ID:        "auto",
		Name:      ProviderName + " - Auto (active set + strategy)",
		Context:   128_000,
		MaxTokens: 16_000,
	}}
}

// Target describes one injected harness for display and removal. Ledger is
// the removal contract: what Apply wrote, so Remove can verify-then-revert
// instead of guessing.
type Target struct {
	Harness string         `json:"harness"`
	Files   []string       `json:"files"`
	Note    string         `json:"note,omitempty"`
	Ledger  []writtenEntry `json:"ledger,omitempty"`
}

// Options carries what to write.
type Options struct {
	Home    string
	BaseURL string // e.g. http://127.0.0.1:8788/v1
	Token   string // the unified key or local token (both authenticate)
	// Models are picker entries; normally the single RoutingModels() result.
	Models []Model
	// tx captures the immediate pre-apply bytes of every file this apply
	// touches, so a failure rolls each file back to exactly its pre-apply
	// state. Set by Apply; nil in a bare Options is harmless.
	tx *applyTx
}

// injectMu serializes Apply/Remove within this process; the per-home flock
// beneath withInjectLock serializes them across processes. It reuses the
// gofrs/flock convention already used for the gateway pid file and the
// index-refresh lock rather than inventing a second mechanism.
var injectMu sync.Mutex

// withInjectLock runs fn while holding the exclusive cross-process lock for
// this home's injection ledger, so a config mutation and its ownership record
// publish as one transaction: a second process can never observe (or race) a
// mutated config whose ledger has not yet been written.
func withInjectLock(home string, fn func() error) error {
	injectMu.Lock()
	defer injectMu.Unlock()
	dir := filepath.Dir(recordPath(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create gateway state dir: %w", err)
	}
	lock := flock.New(filepath.Join(dir, "inject.lock"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("injection lock was not acquired")
	}
	return errors.Join(fn(), lock.Unlock())
}

// Apply installs the provider into the named harness and records what it
// changed under the gateway state dir so Remove can undo exactly that. The
// config mutation and the ownership record are one cross-process locked
// transaction: if the record cannot be published, the mutation is rolled back
// to its immediate pre-apply state rather than left orphaned and unremovable.
func Apply(o Options, harness string) (Target, error) {
	w, err := writerFor(harness)
	if err != nil {
		return Target{}, err
	}
	tx := newApplyTx()
	o.tx = tx
	var t Target
	err = withInjectLock(o.Home, func() error {
		var aerr error
		t, aerr = w.apply(o)
		if aerr != nil {
			tx.rollback()
			return aerr
		}
		if serr := saveRecord(o.Home, t, tx); serr != nil {
			// The mutation is on disk but unrecorded, so it could never be
			// cleanly removed. Undo it to the pre-apply state rather than
			// leave an orphaned injection.
			tx.rollback()
			return fmt.Errorf("injected %s but could not record it for removal; rolled back the change: %w", harness, serr)
		}
		return nil
	})
	if err != nil {
		return Target{}, err
	}
	return t, nil
}

// Remove restores a harness to its pre-injection state, under the same
// cross-process lock as Apply so a config revert and its ledger update publish
// together.
func Remove(home, harness string) (Target, error) {
	w, err := writerFor(harness)
	if err != nil {
		return Target{}, err
	}
	var t Target
	err = withInjectLock(home, func() error {
		var rerr error
		t, rerr = w.remove(home, harness)
		if rerr != nil {
			return rerr
		}
		return forgetRecord(home, harness)
	})
	if err != nil {
		return Target{}, err
	}
	return t, nil
}

// Installed reports which supported harnesses are present on this machine.
// Detection is by config directory or a launcher on PATH, mirroring
// internal/setup's rule: the tool the user actually runs, not a guess.
func Installed(home string) []string {
	var out []string
	has := func(dir, bin string) bool {
		if _, err := os.Stat(filepath.Join(home, dir)); err == nil {
			return true
		}
		_, err := lookPath(bin)
		return err == nil
	}
	if has(".omp", "omp") {
		out = append(out, "omp")
	}
	if has(".pi", "pi") {
		out = append(out, "pi")
	}
	if has(".claude", "claude") {
		out = append(out, "claude")
	}
	if has(".codex", "codex") {
		out = append(out, "codex")
	}
	if has(".config/opencode", "opencode") {
		out = append(out, "opencode")
	}
	if has(".hermes", "hermes") {
		out = append(out, "hermes")
	}
	if has(".openclaw", "openclaw") {
		out = append(out, "openclaw")
	}
	if has(".config/prowl", "prowl-legacy") || has(".local/share/prowl", "prowl-legacy") {
		out = append(out, prowlLegacyHarness)
	}
	return out
}

// Supported lists every harness this package can inject, installed or not, in
// display order. The retired `prowl` id is deliberately absent - it survives
// only as the remove-only cleanup alias in writerFor; `prowl-legacy` is the
// real standalone-CLI target that replaced it.
func Supported() []string {
	return []string{"omp", "pi", "claude", "codex", "opencode", "hermes", "openclaw", prowlLegacyHarness}
}

// Record is the removal ledger: what Apply actually changed, per harness.
type Record struct {
	Targets map[string]Target `json:"targets"`
}

func recordPath(home string) string {
	return filepath.Join(home, ".local", "share", "prowl-agent", "gateway", "inject.json")
}

// loadRecord reads the ownership ledger. A malformed file is REJECTED rather
// than silently discarded: overwriting it with a fresh record (as a silent
// reset would) strands every other harness's ownership - a caller can never
// again cleanly remove them. A missing file is the empty ledger.
func loadRecord(home string) (Record, error) {
	r := Record{Targets: map[string]Target{}}
	blob, err := os.ReadFile(recordPath(home))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if len(bytes.TrimSpace(blob)) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(blob, &r); err != nil {
		return Record{}, fmt.Errorf("injection ledger at %s is malformed; refusing to overwrite it and strand recorded ownership: %w", recordPath(home), err)
	}
	if r.Targets == nil {
		r.Targets = map[string]Target{}
	}
	return r, nil
}

func saveRecord(home string, t Target, tx *applyTx) error {
	r, err := loadRecord(home)
	if err != nil {
		return err
	}
	// Fold a prior injection of the same harness into this ledger: whole-file
	// ownership survives only while the file has not diverged, else it
	// downgrades to a surgical revert; the prior injection's restore info is
	// carried through so removal reverts to the user's bytes, not ours.
	if prev, ok := r.Targets[t.Harness]; ok {
		carryOwnership(t.Ledger, prev.Ledger, tx)
	}
	// prowl-legacy is the current id for the harness formerly recorded under
	// `prowl`; absorb ANY ownership left under the old id - created or merged -
	// so both never claim the same file and neither apply nor remove strands
	// the other.
	if t.Harness == prowlLegacyHarness {
		if old, ok := r.Targets[prowlAliasHarness]; ok {
			carryOwnership(t.Ledger, old.Ledger, tx)
			delete(r.Targets, prowlAliasHarness)
		}
	}
	// Record the exact bytes of any file we own so Remove can delete it only
	// while it still matches. Reading here - after the carry above - captures
	// the bytes this apply just wrote, whether the file was created now or
	// re-authored over one we created earlier.
	for i := range t.Ledger {
		if !t.Ledger[i].CreatedFile {
			continue
		}
		if authored, err := os.ReadFile(t.Ledger[i].Path); err == nil {
			t.Ledger[i].CreatedContent = string(authored)
		}
	}
	r.Targets[t.Harness] = t
	return publishRecord(home, r)
}

// carryOwnership folds a prior injection's ownership (prev) into the ledger
// this apply just produced (led), for entries on the same file:
//   - Whole-file ownership is CARRIED only while the file has not diverged
//     from what we last wrote; once the user has edited a created file it
//     DOWNGRADES to the surgical block/key ownership this apply derived, so
//     removal excises our addition and leaves their edits intact.
//   - The prior injection's restore info - the user's ORIGINAL shadowed value,
//     a non-object container it replaced, a container it created, a pre-existing
//     env file - is carried through, because that prior injection (not the
//     user's real config) is what sits under our key now; without this,
//     removal would "restore" our own earlier value instead of the user's.
func carryOwnership(led []writtenEntry, prev []writtenEntry, tx *applyTx) {
	for pi := range prev {
		pe := prev[pi]
		for li := range led {
			ne := &led[li]
			if ne.Path != pe.Path {
				continue
			}
			if pe.CreatedFile && isFileLevel(*ne) && !divergedFromPrior(tx, pe) {
				ne.CreatedFile = true
			}
			carryRestoreInfo(ne, pe)
		}
	}
}

// isFileLevel reports whether an entry describes ownership of a file (a
// container, block or table), as opposed to a lone prepended top-level key.
func isFileLevel(e writtenEntry) bool {
	return e.Container != "" || e.YamlBlock != "" || e.TomlTable != "" || e.CreatedFile
}

func carryRestoreInfo(ne *writtenEntry, pe writtenEntry) {
	if pe.EnvFile && ne.EnvFile {
		ne.EnvCreated = pe.EnvCreated
		ne.EnvPrior = pe.EnvPrior
		return
	}
	// A YAML provider block or TOML table this apply replaced belonged to the
	// prior injection, not the user: the block/table currently under our key is
	// our own earlier one. Carry the prior injection's recorded original (empty
	// when it was a fresh insert or a whole-file creation) so removal restores
	// the user's bytes, never our own earlier block.
	if ne.YamlBlock != "" && (pe.YamlBlock != "" || pe.CreatedFile) {
		ne.YamlPrior = pe.YamlPrior
	}
	if ne.TomlTable != "" && (pe.TomlTable != "" || pe.CreatedFile) {
		ne.TomlPrior = pe.TomlPrior
	}
	if pe.Container == "" || ne.Container != pe.Container {
		return
	}
	if pe.ContainerReplaced != "" {
		ne.ContainerReplaced = pe.ContainerReplaced
	}
	if pe.ContainerWasNew {
		ne.ContainerWasNew = true
	}
	// The value under each key this apply wrote was the prior injection's, not
	// the user's. Restore what the prior injection recorded: the user's true
	// original (NestedPrior) if it shadowed one, else nothing (delete on
	// removal, since the user had no value there).
	for k := range ne.Nested {
		if prior, ok := pe.NestedPrior[k]; ok {
			if ne.NestedPrior == nil {
				ne.NestedPrior = map[string]string{}
			}
			ne.NestedPrior[k] = prior
		} else if _, injected := pe.Nested[k]; injected {
			delete(ne.NestedPrior, k)
		}
	}
}

func forgetRecord(home, harness string) error {
	r, err := loadRecord(home)
	if err != nil {
		return err
	}
	if _, ok := r.Targets[harness]; !ok {
		return nil
	}
	delete(r.Targets, harness)
	return publishRecord(home, r)
}

// publishRecord writes the ledger atomically: a tmp file renamed into place so
// a crash mid-write can never leave a half-written (malformed) ledger that a
// later load would then reject.
func publishRecord(home string, r Record) error {
	blob, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(recordPath(home)), 0o700); err != nil {
		return err
	}
	return writeFile(recordPath(home), blob, 0o600)
}

// Targets reports what has been injected. A malformed ledger yields nothing
// for this display-only path; the write paths reject it loudly instead.
func Targets(home string) []Target {
	r, err := loadRecord(home)
	if err != nil {
		return nil
	}
	out := make([]Target, 0, len(r.Targets))
	for _, t := range r.Targets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Harness < out[j].Harness })
	return out
}

type writer interface {
	apply(Options) (Target, error)
	remove(home string, harness string) (Target, error)
}

func writerFor(harness string) (writer, error) {
	switch harness {
	case "omp":
		return ompWriter{}, nil
	case "pi":
		return piWriter{}, nil
	case "claude":
		return claudeWriter{}, nil
	case "codex":
		return codexWriter{}, nil
	case "opencode":
		return opencodeWriter{}, nil
	case "hermes":
		return hermesWriter{}, nil
	case "openclaw":
		return openclawWriter{}, nil
	case prowlLegacyHarness:
		return prowlLegacyWriter{}, nil
	case prowlAliasHarness:
		return prowlAliasWriter{}, nil
	}
	return nil, fmt.Errorf("no provider config writer for %q (supported: %s)",
		harness, strings.Join(Supported(), ", "))
}
