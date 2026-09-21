package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/setup"
)

// The Prowl map lives in its own marked region inside AGENTS.md, separate from
// setup's static guidance block, so it can be refreshed on each index without
// disturbing the guidance and survives a setup re-run.
const (
	agentsMapMarker    = setup.AgentsMapMarker
	agentsMapEndMarker = setup.AgentsMapEndMarker
)

// projectMapBlock renders the always-on repository map embedded in AGENTS.md: a
// token-lean summary of the repo's shape (size, languages, subsystems,
// entrypoints, hotspots, guides) that a coding agent reasons from on every turn.
// Passive, always-present context like this makes agents retrieval-led rather
// than pretraining-led -- the single change shown to most improve real-world
// accuracy -- and Prowl already computes every field, refreshing it on each
// index so it never goes stale.
func projectMapBlock(ov query.Overview) string {
	var b strings.Builder
	b.WriteString(agentsMapMarker + "\n")
	b.WriteString("## Prowl project map\n\n")
	b.WriteString("Auto-generated from the Prowl index, refreshed on each `overview`/`init`. " +
		"Prefer retrieving from Prowl (and reading the cited files) over grepping or relying on training memory; this is the current shape of the repo.\n\n")

	c := ov.Counts
	fmt.Fprintf(&b, "- size: %d files, %d symbols, %d edges (resolved %d, external deps %d, unresolved %d)\n",
		c.Files, c.Symbols, c.Edges, c.Resolved, c.External, c.Unresolved)
	if langs := topLangs(c.Langs, 8); langs != "" {
		fmt.Fprintf(&b, "- languages: %s\n", langs)
	}
	if len(ov.Clusters) > 0 {
		parts := make([]string, 0, len(ov.Clusters))
		for _, cl := range ov.Clusters {
			parts = append(parts, fmt.Sprintf("%s(%d,%s)", cl.Label, cl.Files, cl.Lang))
		}
		fmt.Fprintf(&b, "- subsystems: %s\n", strings.Join(parts, " · "))
	}
	if eps := usefulEntrypoints(ov.Entrypoints, 8); len(eps) > 0 {
		line := strings.Join(eps, " · ")
		if extra := ov.EntrypointCount - len(eps); extra > 0 {
			line += fmt.Sprintf(" · (+%d more)", extra)
		}
		fmt.Fprintf(&b, "- entrypoints: %s\n", line)
	}
	if len(ov.Hotspots) > 0 {
		parts := make([]string, 0, len(ov.Hotspots))
		for _, h := range ov.Hotspots {
			parts = append(parts, h.File)
		}
		fmt.Fprintf(&b, "- central files (most depended-on): %s\n", strings.Join(parts, " · "))
	}
	if len(ov.Docs) > 0 {
		fmt.Fprintf(&b, "- read these guides first: %s\n", strings.Join(ov.Docs, " · "))
	}
	b.WriteString("\nDepth on demand: `prowl find|def|outline|references <name>`, `search <text>`, `context search \"<question>\"`, `sketch <ui>`.\n")
	b.WriteString(agentsMapEndMarker)
	return b.String()
}

// topLangs formats the n most-populous languages as "lang:count" pairs.
func topLangs(langs map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	xs := make([]kv, 0, len(langs))
	for k, v := range langs {
		xs = append(xs, kv{k, v})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].v != xs[j].v {
			return xs[i].v > xs[j].v
		}
		return xs[i].k < xs[j].k
	})
	if len(xs) > n {
		xs = xs[:n]
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = x.k + ":" + strconv.Itoa(x.v)
	}
	return strings.Join(parts, " ")
}

// usefulEntrypoints drops test files (never a useful orientation target) and
// caps the list, so the map points an agent at real roots, not test leaves.
func usefulEntrypoints(eps []string, n int) []string {
	out := make([]string, 0, n)
	for _, e := range eps {
		if strings.Contains(e, "_test.") || strings.Contains(e, ".test.") || strings.HasSuffix(e, ".spec.ts") {
			continue
		}
		out = append(out, e)
		if len(out) == n {
			break
		}
	}
	return out
}

// refreshAgentsMap updates the Prowl map block in <root>/AGENTS.md, but only when
// the repo has opted into Prowl (its setup guidance block is present). It never
// creates AGENTS.md and never writes when the content is unchanged, so it is a
// safe side effect of overview/init. Failures are non-fatal to the caller.
func refreshAgentsMap(root string, ov query.Overview) error {
	path := filepath.Join(root, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // no AGENTS.md: nothing to refresh, respect the user's setup choice
	}
	content := string(data)
	if !strings.Contains(content, setup.AgentsMarker) {
		return nil // Prowl is not set up in this AGENTS.md
	}
	block := projectMapBlock(ov)

	if start := strings.Index(content, agentsMapMarker); start >= 0 {
		end := start + len(agentsMapMarker)
		if off := strings.Index(content[start:], agentsMapEndMarker); off >= 0 {
			end = start + off + len(agentsMapEndMarker)
		}
		updated := content[:start] + block + content[end:]
		if updated == content {
			return nil
		}
		return os.WriteFile(path, []byte(updated), 0o644)
	}
	// First map: insert right after the guidance block when present, else append.
	if endIdx := strings.Index(content, setup.AgentsEndMarker); endIdx >= 0 {
		at := endIdx + len(setup.AgentsEndMarker)
		updated := content[:at] + "\n\n" + block + content[at:]
		return os.WriteFile(path, []byte(updated), 0o644)
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content+"\n"+block+"\n"), 0o644)
}
