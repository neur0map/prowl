package inject

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Active reports whether the harness currently selects Prowl's canonical auto
// route. The checks read the same keys Apply writes and do not trust the ledger:
// a user can change the active model after injection.
func Active(home, harness string) (bool, string) {
	switch harness {
	case "claude":
		path := filepath.Join(home, ".claude", "settings.json")
		obj := readJSONObject(path)
		env := objectMember(obj, "env")
		return jsonString(env, "ANTHROPIC_BASE_URL") != "" && jsonString(env, "ANTHROPIC_AUTH_TOKEN") != "" && jsonString(env, "ANTHROPIC_MODEL") == "auto", ""
	case "codex":
		text := readText(filepath.Join(home, ".codex", "config.toml"))
		return tomlTopValue(text, "model_provider") == `"prowl"` && tomlTopValue(text, "model") == `"auto"`, ""
	case "opencode":
		return jsonString(readJSONObject(filepath.Join(home, ".config", "opencode", "opencode.json")), "model") == "prowl/auto", ""
	case "hermes":
		text := readText(hermesConfigPath(home))
		return yamlNestedValue(text, "model", "provider") == "prowl" && yamlNestedValue(text, "model", "default") == "auto", ""
	case "omp":
		text := readText(ompConfigPath(home))
		return yamlNestedValue(text, "modelRoles", "default") == "prowl/auto", ""
	case "pi":
		return false, "Select prowl/auto as Pi's default model manually."
	case "openclaw":
		return false, "Select prowl/auto as OpenClaw's default model manually."
	default:
		return false, "This harness has no verified default-model activation key."
	}
}

func applyActivation(o Options, harness string) ([]writtenEntry, []string, string, error) {
	switch harness {
	case "claude":
		// Claude Code's settings schema uses ANTHROPIC_BASE_URL and
		// ANTHROPIC_MODEL together; claudeWriter already writes both on every
		// injection, so activation requires no second mutation.
		return nil, nil, "claude is active on Prowl auto; restart claude to pick up settings", nil
	case "codex":
		// Codex's installed ~/.codex/config.toml schema uses the top-level
		// model_provider and model keys for its persistent default.
		path := filepath.Join(o.Home, ".codex", "config.toml")
		entries, err := activateTOML(o, path, []kv{{"model_provider", json.RawMessage(`"prowl"`)}, {"model", json.RawMessage(`"auto"`)}})
		return entries, []string{path}, "codex is active on Prowl auto", err
	case "opencode":
		// https://opencode.ai/docs/models/ defines top-level model as the
		// provider/model selector used for the default model.
		path := filepath.Join(o.Home, ".config", "opencode", "opencode.json")
		entries, err := activateJSON(o, path, "model", "prowl/auto")
		return entries, []string{path}, "opencode is active on Prowl auto", err
	case "hermes":
		// Installed Hermes source acp_adapter/session.py:607-615 reads the
		// top-level model mapping's provider and default fields. Its provider
		// registry supplies the matching endpoint and credential.
		path := hermesConfigPath(o.Home)
		entries, err := activateYAML(o, path, "model", []kv{{"provider", json.RawMessage("prowl")}, {"default", json.RawMessage("auto")}})
		return entries, []string{path}, "hermes is active on Prowl auto", err
	case "omp":
		// OMP docs models.md and settings.md define modelRoles.default in
		// config.yml (or existing config.yaml) as the persistent default.
		path := ompConfigPath(o.Home)
		entries, err := activateYAML(o, path, "modelRoles", []kv{{"default", json.RawMessage("prowl/auto")}})
		return entries, []string{path}, "omp is active on Prowl auto", err
	case "pi", "openclaw":
		_, note := Active(o.Home, harness)
		return nil, nil, note, nil
	default:
		return nil, nil, "This harness has no verified default-model activation key.", nil
	}
}

func activateJSON(o Options, path, key, value string) ([]writtenEntry, error) {
	var entry writtenEntry
	changed := false
	created, err := mergeJSONFile(o.tx, path, func(obj *object) error {
		want := json.RawMessage(jsonStr(value))
		if cur, ok := obj.get(key); ok && jsonEq(cur, want) {
			if priorActivation(o.Home, filepath.Clean(path), "", key) {
				entry = writtenEntry{Path: path, Activation: true, Single: map[string]string{key: string(want)}}
			}
			return nil
		}
		entry = writtenEntry{Path: path, Activation: true, Single: map[string]string{key: string(want)}}
		if prior, ok := obj.get(key); ok {
			entry.SinglePrior = map[string]string{key: string(prior)}
		}
		obj.setRaw(key, want)
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created && changed {
		entry.CreatedFile = true
	}
	if len(entry.Single) == 0 {
		return nil, nil
	}
	return []writtenEntry{entry}, nil
}

func activateTOML(o Options, path string, values []kv) ([]writtenEntry, error) {
	if _, err := writeBackup(o.tx, path); err != nil {
		return nil, err
	}
	text := readText(path)
	entries := make([]writtenEntry, 0, len(values))
	for _, pair := range values {
		want := string(pair.Val)
		var entry writtenEntry
		var changed bool
		text, entry, changed = setTomlTopValue(text, pair.Key, want)
		entry.Path = path
		entry.Activation = true
		if changed || priorActivation(o.Home, filepath.Clean(path), "", pair.Key) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
		return nil, err
	}
	return entries, nil
}

func setTomlTopValue(text, key, want string) (string, writtenEntry, bool) {
	entry := writtenEntry{TomlTop: key, TomlVal: want}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body := strings.TrimSpace(line)
		if strings.HasPrefix(body, "[") {
			break
		}
		k, v, ok := strings.Cut(body, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		if strings.TrimSpace(v) == want {
			return text, entry, false
		}
		entry.TomlTopPrior = line
		lines[i] = key + " = " + want
		return strings.Join(lines, "\n"), entry, true
	}
	return key + " = " + want + "\n" + text, entry, true
}

func activateYAML(o Options, path, parent string, values []kv) ([]writtenEntry, error) {
	created, err := writeBackup(o.tx, path)
	if err != nil {
		return nil, err
	}
	text := readText(path)
	entries := make([]writtenEntry, 0, len(values))
	for _, pair := range values {
		var entry writtenEntry
		var changed bool
		text, entry, changed, err = setYAMLNestedValue(text, parent, pair.Key, string(pair.Val))
		if err != nil {
			return nil, err
		}
		entry.Path = path
		entry.Activation = true
		if changed || priorActivation(o.Home, filepath.Clean(path), parent, pair.Key) {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if created {
		entries[0].CreatedFile = true
	}
	if err := writeFile(path, []byte(text), credentialMode(path)); err != nil {
		return nil, err
	}
	return entries, nil
}

func priorActivation(home, path, parent, key string) bool {
	record, err := loadRecord(home)
	if err != nil {
		return false
	}
	for _, target := range record.Targets {
		for _, entry := range target.Ledger {
			if !entry.Activation || filepath.Clean(entry.Path) != path {
				continue
			}
			if entry.TomlTop == key || entry.YamlParent == parent && entry.YamlKey == key || entry.Single[key] != "" {
				return true
			}
		}
	}
	return false
}

func removeActivation(entries []writtenEntry) error {
	removedPaths := map[string]bool{}
	for _, entry := range entries {
		if !entry.Activation || !entry.CreatedFile || entry.CreatedContent == "" {
			continue
		}
		raw, err := os.ReadFile(entry.Path)
		if os.IsNotExist(err) {
			removedPaths[entry.Path] = true
			continue
		}
		if err != nil {
			return err
		}
		if createdFileUnchanged(raw, entry.CreatedContent) {
			if err := os.Remove(entry.Path); err != nil {
				return err
			}
			removedPaths[entry.Path] = true
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.Activation || removedPaths[entry.Path] {
			continue
		}
		if entry.YamlParent != "" {
			if err := removeYAMLActivation(entry); err != nil {
				return err
			}
			continue
		}
		if _, _, err := removeWritten([]writtenEntry{entry}); err != nil {
			return err
		}
	}
	return nil
}

func createdTargetIntact(entries []writtenEntry) bool {
	if len(entries) == 0 || entries[0].Activation || !entries[0].CreatedFile || entries[0].CreatedContent == "" {
		return false
	}
	raw, err := os.ReadFile(entries[0].Path)
	if err != nil || !createdFileUnchanged(raw, entries[0].CreatedContent) {
		return false
	}
	for _, entry := range entries {
		if entry.Activation && entry.Path != entries[0].Path {
			return false
		}
	}
	return true
}

func setYAMLNestedValue(text, parent, key, value string) (string, writtenEntry, bool, error) {
	entry := writtenEntry{YamlParent: parent, YamlKey: key, YamlValue: value}
	parentStart, parentLineEnd, parentEnd, ok := yamlParentRange(text, parent)
	line := "  " + key + ": " + value + "\n"
	if !ok {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		entry.YamlParentWasNew = true
		return text + parent + ":\n" + line, entry, true, nil
	}
	parentLine := strings.TrimSpace(text[parentStart:parentLineEnd])
	_, inline, _ := strings.Cut(parentLine, ":")
	inline = strings.TrimSpace(strings.SplitN(inline, "#", 2)[0])
	if inline != "" && inline != "{}" && inline != "null" && inline != "[]" {
		return text, entry, false, fmt.Errorf("cannot activate %s.%s: parent is not a YAML mapping", parent, key)
	}
	if inline != "" {
		entry.YamlParentPrior = text[parentStart:parentLineEnd]
		text = text[:parentStart] + parent + ":\n" + text[parentLineEnd:]
		parentLineEnd = parentStart + len(parent) + 2
		parentEnd = yamlSectionEnd(text, parentLineEnd)
	}
	if !strings.HasSuffix(text[parentStart:parentLineEnd], "\n") {
		text = text[:parentLineEnd] + "\n" + text[parentLineEnd:]
		parentLineEnd++
		parentEnd++
	}
	start, end, found := yamlChildRange(text, parentLineEnd, parentEnd, key)
	if found {
		prior := text[start:end]
		if yamlNestedValue(text, parent, key) == value && strings.TrimSpace(prior) == strings.TrimSpace(line) {
			return text, entry, false, nil
		}
		entry.YamlPrior = prior
		return text[:start] + line + text[end:], entry, true, nil
	}
	return text[:parentEnd] + line + text[parentEnd:], entry, true, nil
}

func removeYAMLActivation(entry writtenEntry) error {
	raw, err := os.ReadFile(entry.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	text := string(raw)
	_, parentLineEnd, parentEnd, ok := yamlParentRange(text, entry.YamlParent)
	if !ok {
		return nil
	}
	start, end, found := yamlChildRange(text, parentLineEnd, parentEnd, entry.YamlKey)
	if !found || yamlNestedValue(text, entry.YamlParent, entry.YamlKey) != entry.YamlValue {
		return nil
	}
	if entry.YamlPrior != "" {
		text = text[:start] + entry.YamlPrior + text[end:]
	} else {
		text = text[:start] + text[end:]
	}
	if entry.YamlParentWasNew || entry.YamlParentPrior != "" {
		ps, pe, pend, present := yamlParentRange(text, entry.YamlParent)
		if present && !yamlSectionHasValues(text[pe:pend]) {
			if entry.YamlParentPrior != "" {
				text = text[:ps] + entry.YamlParentPrior + text[pend:]
			} else {
				text = text[:ps] + text[pend:]
			}
		}
	}
	return writeFile(entry.Path, []byte(text), credentialMode(entry.Path))
}

func yamlParentRange(text, parent string) (start, lineEnd, end int, ok bool) {
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		body := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		key, _, found := strings.Cut(body, ":")
		if indent == 0 && found && strings.TrimSpace(key) == parent {
			lineEnd = offset + len(line)
			return offset, lineEnd, yamlSectionEnd(text, lineEnd), true
		}
		offset += len(line)
	}
	return 0, 0, 0, false
}

func yamlSectionEnd(text string, from int) int {
	offset := from
	for _, line := range strings.SplitAfter(text[from:], "\n") {
		body := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if body != "" && !strings.HasPrefix(body, "#") && indent == 0 {
			return offset
		}
		offset += len(line)
	}
	return len(text)
}

func yamlChildRange(text string, from, sectionEnd int, key string) (start, end int, ok bool) {
	offset := from
	for _, line := range strings.SplitAfter(text[from:sectionEnd], "\n") {
		body := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		child, _, found := strings.Cut(body, ":")
		if indent == 2 && found && strings.TrimSpace(child) == key {
			start = offset
			end = offset + len(line)
			for _, following := range strings.SplitAfter(text[end:sectionEnd], "\n") {
				fbody := strings.TrimSpace(following)
				findent := len(following) - len(strings.TrimLeft(following, " \t"))
				if fbody != "" && !strings.HasPrefix(fbody, "#") && findent <= 2 {
					break
				}
				end += len(following)
			}
			return start, end, true
		}
		offset += len(line)
	}
	return 0, 0, false
}

func yamlNestedValue(text, parent, key string) string {
	_, lineEnd, sectionEnd, ok := yamlParentRange(text, parent)
	if !ok {
		return ""
	}
	start, end, ok := yamlChildRange(text, lineEnd, sectionEnd, key)
	if !ok {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(text[start:end], "\n", 2)[0])
	_, value, _ := strings.Cut(line, ":")
	value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
	return strings.Trim(value, `"'`)
}

func yamlSectionHasValues(section string) bool {
	for _, line := range strings.Split(section, "\n") {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

func tomlTopValue(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		body := strings.TrimSpace(line)
		if strings.HasPrefix(body, "[") {
			break
		}
		k, value, ok := strings.Cut(body, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
		}
	}
	return ""
}

func readJSONObject(path string) *object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return &object{}
	}
	obj, err := parseObject(raw)
	if err != nil {
		return &object{}
	}
	return obj
}

func ompConfigPath(home string) string {
	yml := filepath.Join(home, ".omp", "agent", "config.yml")
	if exists(yml) {
		return yml
	}
	yaml := filepath.Join(home, ".omp", "agent", "config.yaml")
	if exists(yaml) {
		return yaml
	}
	return yml
}

func objectMember(obj *object, key string) *object {
	raw, ok := obj.get(key)
	if !ok {
		return &object{}
	}
	out, err := parseObject(raw)
	if err != nil {
		return &object{}
	}
	return out
}

func jsonString(obj *object, key string) string {
	raw, ok := obj.get(key)
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func readText(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

func appendUnique(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst)+len(values))
	for _, value := range dst {
		seen[value] = true
	}
	for _, value := range values {
		if value != "" && !seen[value] {
			dst = append(dst, value)
			seen[value] = true
		}
	}
	return dst
}
