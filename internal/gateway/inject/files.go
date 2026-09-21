package inject

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func lookPath(bin string) (string, error) { return exec.LookPath(bin) }

// ── ordered JSON editing ─────────────────────────────────────────────────────
//
// The harness settings files we touch (claude, opencode, prowl) are hand-edited
// by users. Re-shipping one through map[string]any would alphabetise every key
// and reorder the whole file on injection - technically valid JSON, and rude
// enough that a user diff looks like we rewrote their config. These helpers
// preserve key order and leave untouched values byte-identical.

type member struct {
	Key   string
	Raw   json.RawMessage
	dirty bool // we wrote/replaced it: only then does rendering re-indent it
}

type object struct {
	members []member
}

func parseObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	obj := &object{}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := k.(string)
		if !ok {
			return nil, fmt.Errorf("non-string object key")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		obj.members = append(obj.members, member{Key: key, Raw: value})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, err
	}
	return obj, nil
}

// get returns the raw value for a key.
func (o *object) get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.Key == key {
			return m.Raw, true
		}
	}
	return nil, false
}

// set replaces or appends a key, keeping position when it already existed.
func (o *object) set(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	o.setRaw(key, raw)
	return nil
}

// setRaw stores verbatim JSON for a key. Keeping raw bytes (instead of
// round-tripping through `any`) is what preserves the USER's key order in
// subtrees we touched but did not author: a map re-marshals alphabetised, and
// reordering someone's config on an unrelated edit is exactly the rude
// rewrite this package exists to avoid.
func (o *object) setRaw(key string, raw json.RawMessage) {
	for i, m := range o.members {
		if m.Key == key {
			o.members[i].Raw = raw
			o.members[i].dirty = true
			return
		}
	}
	o.members = append(o.members, member{Key: key, Raw: raw, dirty: true})
}

// setString appends or replaces a string key.
func (o *object) setString(key, value string) error { return o.set(key, value) }

// del removes a key, reporting whether it existed.
func (o *object) del(key string) bool {
	for i, m := range o.members {
		if m.Key == key {
			o.members = append(o.members[:i], o.members[i+1:]...)
			return true
		}
	}
	return false
}

func (o *object) marshal() []byte {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, m := range o.members {
		kb, _ := json.Marshal(m.Key)
		buf.WriteString("  " + string(kb) + ": ")
		if m.dirty {
			buf.Write(indentJSON(m.Raw, "  "))
		} else {
			buf.Write(m.Raw) // verbatim: byte-identical to the user's file
		}
		if i < len(o.members)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes()
}

// indentJSON pretty-prints a nested raw value at the given prefix depth, so
// merged subtrees read like the rest of a hand-written settings file.
func indentJSON(raw json.RawMessage, prefix string) json.RawMessage {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, prefix, "  ")
	if err != nil {
		return raw
	}
	return out
}

// ── file helpers ─────────────────────────────────────────────────────────────

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// createdFileUnchanged reports whether a file this package created still holds
// exactly the bytes it authored. A ledger entry without recorded content
// predates this check, so it is treated as unchanged to preserve the prior
// delete-on-removal behaviour.
func createdFileUnchanged(current []byte, authored string) bool {
	return authored == "" || string(current) == authored
}

// authoredBlockIntact reports whether the block/table currently on disk is
// still the one this package authored, ignoring only trailing whitespace: the
// YAML/TOML range functions absorb blank lines a user adds as a separator
// before a following section, and that separator is a surrounding edit, not an
// edit to our block. An empty authored value predates the guard and is treated
// as intact, preserving the historical unconditional revert.
func authoredBlockIntact(current, authored string) bool {
	return authored == "" ||
		strings.TrimRight(current, " \t\r\n") == strings.TrimRight(authored, " \t\r\n")
}

// ── apply transaction ─────────────────────────────────────────────────────────
//
// A single Apply touches one or several files (Codex touches three). Each is
// snapshotted, exactly once, at the moment BEFORE this apply first writes it,
// so a failure - a later step's write error, or an unpersistable ledger -
// rolls every file back to the bytes it held when this apply began. The old
// design rolled back from the durable .bak, which is only the FIRST
// injection's backup: a failed re-apply would then wipe the prior injection
// (and any user edits since) instead of the change actually in flight.

type fileState struct {
	existed bool
	data    []byte
	mode    os.FileMode
}

type applyTx struct {
	order []string
	snaps map[string]fileState
}

func newApplyTx() *applyTx { return &applyTx{snaps: map[string]fileState{}} }

// snapshot captures path's current bytes once per transaction. Repeated calls
// for the same path keep the FIRST snapshot - the true pre-apply state - so a
// writer that rewrites a file several times still rolls back to where it began.
func (tx *applyTx) snapshot(path string) error {
	if tx == nil {
		return nil
	}
	if _, ok := tx.snaps[path]; ok {
		return nil
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		tx.order = append(tx.order, path)
		tx.snaps[path] = fileState{existed: false}
		return nil
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tx.order = append(tx.order, path)
	tx.snaps[path] = fileState{existed: true, data: data, mode: info.Mode().Perm()}
	return nil
}

// rollback restores every snapshotted file to its pre-apply state, in reverse
// order of first touch. Best-effort: Apply already carries the failure that
// triggered it.
func (tx *applyTx) rollback() {
	if tx == nil {
		return
	}
	for i := len(tx.order) - 1; i >= 0; i-- {
		path := tx.order[i]
		st := tx.snaps[path]
		if st.existed {
			_ = writeFile(path, st.data, st.mode)
		} else {
			_ = os.Remove(path)
		}
	}
}

// refuseSymlink guards against editing a config the user has symlinked
// elsewhere: writing through it (or replacing it via rename) would either
// clobber the link target or silently swap their link for a regular file.
// Neither is a safe, reversible edit, so we refuse before touching anything.
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil // absent or unstat-able: creation is handled by the caller
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to edit %s: it is a symlink; resolve it to a regular file first", path)
	}
	return nil
}

// divergedFromPrior reports whether the file a prior apply created was edited
// by the user before THIS apply ran, judged from the immediate pre-apply
// snapshot rather than the already-rewritten current bytes. A record without
// recorded content predates the check and is treated as unchanged, preserving
// the prior whole-file ownership.
func divergedFromPrior(tx *applyTx, pe writtenEntry) bool {
	if tx == nil || pe.CreatedContent == "" {
		return false
	}
	st, ok := tx.snaps[pe.Path]
	if !ok || !st.existed {
		return false
	}
	return string(st.data) != pe.CreatedContent
}

// removeEnvFile reverts a Codex env file this package wrote: while it still
// holds exactly what we wrote, one we created is deleted and one that
// pre-existed is restored to the user's original bytes - never blindly
// unlinking a file the user already had.
func removeEnvFile(e writtenEntry) error {
	if !exists(e.Path) {
		return nil
	}
	cur, err := os.ReadFile(e.Path)
	if err != nil {
		return err
	}
	if e.CreatedContent != "" && string(cur) != e.CreatedContent {
		return nil // the user rewrote it: leave their version
	}
	if e.EnvCreated {
		return os.Remove(e.Path)
	}
	return writeFile(e.Path, []byte(e.EnvPrior), 0o600)
}

// backupSuffix names the single pre-rewrite backup this package keeps beside a
// file it edits.
const backupSuffix = ".prowl-agent-bak"

// writeBackup copies a pre-existing file aside before its first rewrite, one
// .bak kept (a timestamped pile trains users to ignore them). It refuses a
// symlinked config outright (writing through it would clobber the link target
// or silently replace the link) and snapshots the file's immediate pre-apply
// state into tx so a failed apply rolls it back to exactly here. Returns true
// when the file did not exist and is being created by this apply. The backup
// holds the same credentials as the config, so it is clamped to owner-only.
func writeBackup(tx *applyTx, path string) (bool, error) {
	if err := refuseSymlink(path); err != nil {
		return false, err
	}
	bak := path + backupSuffix
	if err := tx.snapshot(path); err != nil {
		return false, err
	}
	if err := tx.snapshot(bak); err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !exists(bak) {
		blob, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		mode := info.Mode().Perm() &^ 0o077
		if err := os.WriteFile(bak, blob, mode); err != nil {
			return false, err
		}
		if err := os.Chmod(bak, mode); err != nil {
			return false, err
		}
	}
	return false, nil
}

func writeFile(path string, blob []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, mode); err != nil {
		return err
	}
	// Enforce the exact mode: os.WriteFile is subject to umask, and a leftover
	// tmp keeps its old perms. A credential file must land at the mode asked
	// for, not a looser one.
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// credentialMode is the mode to write a credential-bearing config with: 0600
// for a file we are creating, or the existing mode clamped to owner-only for
// one we are merging into. A token must never be group- or world-readable OR
// writable - a group-writable config is a credential another local user can
// replace - however permissive the user left the file.
func credentialMode(path string) os.FileMode {
	info, err := os.Stat(path)
	if err != nil {
		return 0o600
	}
	return info.Mode().Perm() &^ 0o077
}

// mergeJSONFile applies fn over the ordered top-level object of a JSON file,
// creating it (as `{}`) when absent.
func mergeJSONFile(tx *applyTx, path string, fn func(*object) error) (bool, error) {
	created, err := writeBackup(tx, path)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return created, err
	}
	obj := &object{}
	if len(bytes.TrimSpace(raw)) > 0 {
		obj, err = parseObject(raw)
		if err != nil {
			return created, fmt.Errorf("%s is not a JSON object I can edit safely: %w", path, err)
		}
	}
	if err := fn(obj); err != nil {
		return created, err
	}
	return created, writeFile(path, obj.marshal(), credentialMode(path))
}

// jsonEq compares two raw JSON values canonically (key order normalised), so
// whitespace and key ordering in a user's file don't read as a change.
func jsonEq(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ab, err1 := json.Marshal(av)
	bb, err2 := json.Marshal(bv)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

// writtenEntry is one key we set and its exact value, recorded so removal can
// verify before reverting.
type writtenEntry struct {
	// Path is the file (absolute).
	Path string `json:"path"`
	// CreatedFile records that the whole file was ours to begin with; removal
	// deletes it - but only while its bytes still match CreatedContent, so a
	// file the user has since edited is reverted surgically, never destroyed.
	CreatedFile bool `json:"created_file,omitempty"`
	// CreatedContent is the exact bytes Apply wrote when it created (or last
	// re-authored) the file. Same value-match discipline as the nested/single
	// entries below: removal acts only while what is on disk still equals what
	// we recorded.
	CreatedContent string `json:"created_content,omitempty"`
	// YamlBlock names a managed YAML provider block (omp's models.yml, hermes'
	// config.yaml). Removal excises a block we added, or restores YamlPrior
	// when this apply replaced a block that pre-existed.
	YamlBlock string `json:"yaml_block,omitempty"`
	// YamlAuthored is the exact block bytes this apply wrote and YamlPrior the
	// exact bytes that were there before (empty when we merely inserted).
	// Removal acts only while the block on disk still equals YamlAuthored: a
	// user edit since apply is a conflict left intact, never overwritten.
	YamlAuthored string `json:"yaml_authored,omitempty"`
	YamlPrior    string `json:"yaml_prior,omitempty"`
	// TomlTable names a managed TOML table (codex's config.toml). Removal
	// excises a table we added, or restores TomlPrior when this apply replaced
	// a table that pre-existed.
	TomlTable string `json:"toml_table,omitempty"`
	// TomlAuthored / TomlPrior mirror the Yaml pair above for the TOML table:
	// the exact authored table and the exact prior bytes it replaced, with the
	// same restore-while-intact, else-conflict discipline.
	TomlAuthored string `json:"toml_authored,omitempty"`
	TomlPrior    string `json:"toml_prior,omitempty"`
	// TomlTop names a top-level TOML key we prepended (codex model_provider)
	// and the raw TOML value we wrote for it; removal deletes the line only
	// while its value is still ours.
	TomlTop string `json:"toml_top,omitempty"`
	TomlVal string `json:"toml_val,omitempty"`
	// Container is the top-level map key holding nested (when set).
	Container string `json:"container,omitempty"`
	// ContainerWasNew records that Apply created the container mapping;
	// removal then deletes the emptied key instead of leaving `{} behind.
	ContainerWasNew bool              `json:"container_was_new,omitempty"`
	Nested          map[string]string `json:"nested,omitempty"` // nested key -> JSON value we wrote
	// NestedPrior records the pre-existing JSON value of a nested key we
	// overwrote (keyed like Nested). Removal restores it - while our written
	// value is still in place - instead of deleting the key, so an injection
	// that shadowed a user's own entry gives it back.
	NestedPrior map[string]string `json:"nested_prior,omitempty"`
	// ContainerReplaced holds the raw pre-existing value of Container when it
	// was NOT an object we could merge into (a string/array/scalar). Apply
	// replaced it wholesale; removal restores it verbatim while our injected
	// object is still there.
	ContainerReplaced string            `json:"container_replaced,omitempty"`
	Single            map[string]string `json:"single,omitempty"` // top key -> JSON value
	// EnvFile marks a sourceable credential env file this apply wrote (Codex).
	// CreatedContent holds exactly what we wrote; on removal, while the file
	// still holds that, we delete it when EnvCreated (we authored it from
	// scratch) or restore EnvPrior when it pre-existed.
	EnvFile    bool   `json:"env_file,omitempty"`
	EnvCreated bool   `json:"env_created,omitempty"`
	EnvPrior   string `json:"env_prior,omitempty"`
}

// kv is one ordered key/value pair to merge into a container. A slice (not a
// map) keeps the written key order stable, so re-applying does not reshuffle a
// user's file.
type kv struct {
	Key string
	Val json.RawMessage
}

// mergeContainer merges ordered key/value pairs into a named container map of
// the top-level object and returns the ledger entry that reverses it: whether
// the container was created, the raw value of a non-object container it
// replaced, each value it wrote, and the pre-existing value of any key it
// overwrote. legacyKeys are dropped from the container (renamed-away ids).
func mergeContainer(obj *object, container string, values []kv, legacyKeys ...string) writtenEntry {
	e := writtenEntry{Container: container, Nested: map[string]string{}}
	sub := &object{}
	if raw, ok := obj.get(container); ok {
		if parsed, err := parseObject(raw); err == nil {
			sub = parsed
		} else {
			// A non-object container: record it so removal restores it verbatim.
			e.ContainerReplaced = string(raw)
		}
	} else {
		e.ContainerWasNew = true
	}
	for _, lk := range legacyKeys {
		sub.del(lk)
	}
	for _, p := range values {
		if prior, ok := sub.get(p.Key); ok {
			if e.NestedPrior == nil {
				e.NestedPrior = map[string]string{}
			}
			e.NestedPrior[p.Key] = string(prior)
		}
		sub.setRaw(p.Key, p.Val)
		e.Nested[p.Key] = string(p.Val)
	}
	obj.setRaw(container, bytes.TrimSuffix(sub.marshal(), []byte("\n")))
	return e
}

// objectMatchesNested reports whether raw parses as an object whose keys still
// carry exactly the values we recorded - the marker that our injection is
// intact and safe to reverse.
func objectMatchesNested(raw json.RawMessage, nested map[string]string) bool {
	so, err := parseObject(raw)
	if err != nil {
		return false
	}
	for k, want := range nested {
		cur, ok := so.get(k)
		if !ok || !jsonEq(cur, json.RawMessage(want)) {
			return false
		}
	}
	return true
}

// removeWritten undoes exactly what was recorded: a key is deleted only while
// its current value still equals what we wrote. Anything the user has since
// changed is left alone - a removal that reverts someone's later edit is not
// a removal, it is a rollback of their work.
func removeWritten(entries []writtenEntry) (undone, conflicts []string, err error) {
	for _, e := range entries {
		if !exists(e.Path) {
			continue
		}
		// TOML entries skip the JSON object path entirely - the file is not
		// JSON and parsing it as one is a guaranteed false error.
		if e.TomlTable != "" || e.TomlTop != "" {
			if e.TomlTable != "" {
				raw, rerr := os.ReadFile(e.Path)
				if rerr != nil {
					return undone, conflicts, rerr
				}
				if start, end, ok := tomlTableRange(string(raw), e.TomlTable); ok {
					current := string(raw[start:end])
					// Act only while our authored table is still on disk; a user
					// edit is a conflict left intact. An empty TomlAuthored
					// predates the guard (unconditional revert).
					intact := authoredBlockIntact(current, e.TomlAuthored)
					switch {
					case !intact:
						// The user edited our table: preserve their file.
						conflicts = append(conflicts, filepath.Base(e.Path)+":"+e.TomlTable)
					case e.TomlPrior != "":
						// We replaced a pre-existing table: restore it verbatim.
						out := append(append([]byte{}, raw[:start]...), e.TomlPrior...)
						out = append(out, raw[end:]...)
						if werr := writeFile(e.Path, out, credentialMode(e.Path)); werr != nil {
							return undone, conflicts, werr
						}
						undone = append(undone, filepath.Base(e.Path)+":"+e.TomlTable+" (restored)")
					default:
						// A table we added: excise it.
						excised := append(append([]byte{}, raw[:start]...), raw[end:]...)
						if werr := writeFile(e.Path, excised, credentialMode(e.Path)); werr != nil {
							return undone, conflicts, werr
						}
						undone = append(undone, filepath.Base(e.Path)+":"+e.TomlTable)
					}
				}
			}
			if e.TomlTop != "" {
				raw, rerr := os.ReadFile(e.Path)
				if rerr != nil {
					return undone, conflicts, rerr
				}
				if out, removed := removeTomlTopLine(string(raw), e.TomlTop, e.TomlVal); removed {
					if werr := writeFile(e.Path, out, credentialMode(e.Path)); werr != nil {
						return undone, conflicts, werr
					}
					undone = append(undone, filepath.Base(e.Path)+":"+e.TomlTop)
				}
			}
			continue
		}
		raw, rerr := os.ReadFile(e.Path)
		if rerr != nil {
			return undone, conflicts, rerr
		}
		obj, perr := parseObject(raw)
		if perr != nil {
			return undone, conflicts, fmt.Errorf("cannot edit %s during removal: %w", e.Path, perr)
		}
		touched := false
		switch {
		case e.ContainerReplaced != "":
			// Apply overwrote a non-object container wholesale. Restore the
			// prior value verbatim, but only while our injected object is still
			// what is there - if the user rebuilt it, leave their version.
			if cur, ok := obj.get(e.Container); ok && objectMatchesNested(cur, e.Nested) {
				obj.setRaw(e.Container, json.RawMessage(e.ContainerReplaced))
				undone = append(undone, filepath.Base(e.Path)+":"+e.Container)
				touched = true
			}
		case e.Container != "":
			if sub, ok := obj.get(e.Container); ok {
				if so, serr := parseObject(sub); serr == nil {
					for k, want := range e.Nested {
						cur, ok := so.get(k)
						if !ok || !jsonEq(cur, json.RawMessage(want)) {
							continue
						}
						if prior, has := e.NestedPrior[k]; has {
							// The key shadowed a user value: give it back.
							so.setRaw(k, json.RawMessage(prior))
							undone = append(undone, filepath.Base(e.Path)+":"+e.Container+"."+k+" (restored)")
						} else {
							so.del(k)
							undone = append(undone, filepath.Base(e.Path)+":"+e.Container+"."+k)
						}
						touched = true
					}
					if touched {
						if len(so.members) == 0 && e.ContainerWasNew {
							obj.del(e.Container)
						} else {
							obj.setRaw(e.Container, bytes.TrimSuffix(so.marshal(), []byte("\n")))
						}
					}
				}
			}
		}
		for k, want := range e.Single {
			if cur, ok := obj.get(k); ok && jsonEq(cur, json.RawMessage(want)) {
				obj.del(k)
				undone = append(undone, filepath.Base(e.Path)+":"+k)
				touched = true
			}
		}
		if touched {
			if werr := writeFile(e.Path, obj.marshal(), credentialMode(e.Path)); werr != nil {
				return undone, conflicts, werr
			}
		}
	}
	sort.Strings(undone)
	sort.Strings(conflicts)
	return undone, conflicts, nil
}

// removeTomlTopLine drops one top-level `key = value` line, and only while the
// value still matches what we wrote - a key the user repointed stays.
func removeTomlTopLine(text, key, want string) ([]byte, bool) {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	removed := false
	inTable := false
	for _, ln := range lines {
		body := strings.TrimSpace(ln)
		if strings.HasPrefix(body, "[") {
			inTable = true
		}
		if !removed && !inTable {
			if k, v, found := strings.Cut(body, "="); found && strings.TrimSpace(k) == key {
				if strings.TrimSpace(v) == want {
					removed = true
					continue
				}
			}
		}
		out = append(out, ln)
	}
	if !removed {
		return nil, false
	}
	return []byte(strings.Join(out, "\n")), true
}
