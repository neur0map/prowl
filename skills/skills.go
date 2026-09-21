// Package skills embeds the agent skills Prowl installs into a project so
// coding agents learn when to reach for Prowl. Each skill is a SKILL.md with
// YAML frontmatter (name, description) whose description states the triggering
// conditions, following the convention agents use to decide when a skill applies.
package skills

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed */SKILL.md
//go:embed all:legacy
//go:embed all:native
var files embed.FS

// legacyDir holds retired skill bodies. They exist only so setup can recognize
// and remove an exact old installed copy during migration; they are never active
// and never appear in All().
const legacyDir = "legacy"

// nativeDir holds harness-native integration assets (Claude plugin files, omp
// agent and extension files, and prowl's routing skill) exposed through Native.
// They are not portable skills, so they never appear in All().
const nativeDir = "native"

// Skill is one installable agent skill: a directory name and its SKILL.md body.
type Skill struct {
	Name    string
	Content string
}

// All returns every embedded skill, sorted by name.
func All() []Skill {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	out := make([]Skill, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == legacyDir || entry.Name() == nativeDir {
			continue
		}
		data, err := files.ReadFile(entry.Name() + "/SKILL.md")
		if err != nil {
			continue
		}
		out = append(out, Skill{Name: entry.Name(), Content: string(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Legacy returns the retired skill body once shipped under name, for
// ownership-safe migration. The bytes are the exact historical SKILL.md, so
// setup can confirm a prior Prowl install before removing it. Legacy skills are
// never active and never appear in All().
func Legacy(name string) (Skill, bool) {
	data, err := files.ReadFile(legacyDir + "/" + name + "/SKILL.md")
	if err != nil {
		return Skill{}, false
	}
	return Skill{Name: name, Content: string(data)}, true
}

// Asset is one embedded native integration file for a coding-agent client.
// Client is the client name ("claude", "omp", or "prowl"); Path is the install path
// relative to that client's native root (e.g. ".claude-plugin/plugin.json");
// Content is the file body; Executable marks files the installer must write
// with the execute bit. Native assets are concrete embedded files, not a plugin
// framework: the installer copies each Asset verbatim to its Path.
type Asset struct {
	Client     string
	Path       string
	Content    string
	Executable bool
}

// Native returns the embedded native integration assets for client, sorted by
// relative path so every consumer sees the same files in the same order. Only
// the immediate supported clients "claude", "omp", and "prowl" resolve; any
// other name -- unknown, nested (e.g. "claude/agents"), or traversal-like --
// returns nil, so a caller can never walk an arbitrary embedded subtree.
func Native(client string) []Asset {
	switch client {
	case "claude", "omp", "prowl":
	default:
		return nil
	}
	root := nativeDir + "/" + client
	var out []Asset
	fs.WalkDir(files, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		out = append(out, Asset{
			Client:     client,
			Path:       rel,
			Content:    string(data),
			Executable: strings.HasSuffix(rel, ".sh"),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Names returns the skill names, sorted.
func Names() []string {
	skills := All()
	names := make([]string, len(skills))
	for i, skill := range skills {
		names[i] = skill.Name
	}
	return names
}

// Description extracts the frontmatter description line from a SKILL.md body,
// for callers that want to preview a skill without shipping the whole file.
func Description(content string) string {
	const key = "description:"
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		}
	}
	return ""
}
