package skills

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/capability"
)

// A skill's directory name is its installed identity: setup writes each skill to
// <client>/skills/<dir>/SKILL.md, and agents match the frontmatter name. If the
// two disagree the skill installs under one name and announces another, so
// nothing reliably resolves it.
func TestEachSkillNameMatchesItsDirectory(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("no skills embedded")
	}
	for _, skill := range all {
		declared := frontmatterValue(skill.Content, "name:")
		if declared != skill.Name {
			t.Errorf("skill %s declares name %q; the two must match", skill.Name, declared)
		}
	}
}

// The description is the only thing an agent reads when deciding whether to
// reach for a skill at all, so an empty or truncated one silently disables it.
// Length bounds are the convention agent harnesses share: long enough to list
// real triggering symptoms, short enough to stay in a tool listing.
func TestEachSkillDescribesWhenToUseIt(t *testing.T) {
	for _, skill := range All() {
		description := Description(skill.Content)
		switch {
		case description == "":
			t.Errorf("skill %s has no description; agents cannot route to it", skill.Name)
		case len(description) < 80:
			t.Errorf("skill %s description is %d chars, too short to state when it applies: %q",
				skill.Name, len(description), description)
		case len(description) > 700:
			t.Errorf("skill %s description is %d chars; trim it to stay readable in a skill listing",
				skill.Name, len(description))
		}
		if !strings.Contains(strings.ToLower(description), "use ") {
			t.Errorf("skill %s description does not state a triggering condition (\"Use when ...\"): %q",
				skill.Name, description)
		}
	}
}

// The CLI-first cutover replaces prowl-repo-exploration with a single canonical
// code-search skill. The active registry must expose code-search and must never
// surface the retired skill or the legacy ownership tree, which exists only so
// setup can recognize and remove old installed copies.
func TestCodeSearchIsTheOnlyActiveSearchSkill(t *testing.T) {
	names := Names()
	if !hasName(names, "code-search") {
		t.Errorf("All() must expose code-search; got %v", names)
	}
	if hasName(names, "prowl-repo-exploration") {
		t.Errorf("retired skill prowl-repo-exploration is still active: %v", names)
	}
	if hasName(names, "legacy") {
		t.Errorf("the legacy ownership tree leaked into All(): %v", names)
	}
}

// The description is selected from metadata alone, so it must name the tasks that
// should route to Prowl: repository/code search, symbols, callers, structure,
// architecture, and change impact.
func TestCodeSearchDescriptionNamesItsTriggers(t *testing.T) {
	skill, ok := findSkill("code-search")
	if !ok {
		t.Fatal("code-search skill is not embedded")
	}
	if name := frontmatterValue(skill.Content, "name:"); name != "code-search" {
		t.Errorf("code-search frontmatter name = %q, want it to match the directory", name)
	}
	description := strings.ToLower(Description(skill.Content))
	if !strings.HasPrefix(description, "must use as the first") {
		t.Errorf("code-search description does not make first-step routing mandatory: %q", description)
	}
	for _, trigger := range []string{"search", "symbol", "caller", "structure", "architecture", "impact"} {
		if !strings.Contains(description, trigger) {
			t.Errorf("code-search description omits the %q trigger: %q", trigger, description)
		}
	}
	if !strings.Contains(description, "repositor") && !strings.Contains(description, "code search") {
		t.Errorf("code-search description does not name repository/code search: %q", description)
	}
}

// The body routes structural work through the prowl CLI and never presents
// MCP as a transport choice: an agent should not have to pick between CLI and
// MCP, and the retired MCP tool names must be gone.
func TestCodeSearchBodyRoutesThroughTheCLINotMCP(t *testing.T) {
	skill, ok := findSkill("code-search")
	if !ok {
		t.Fatal("code-search skill is not embedded")
	}
	if !strings.Contains(skill.Content, "prowl ") {
		t.Error("code-search body invokes no prowl CLI command")
	}
	lower := strings.ToLower(skill.Content)
	compact := strings.Join(strings.Fields(lower), " ")
	if strings.Contains(lower, "mcp") {
		t.Error("code-search body still presents MCP as a transport choice")
	}
	for _, banned := range []string{"search_context", "read_symbol", "mcp__"} {
		if strings.Contains(lower, banned) {
			t.Errorf("code-search body still names the MCP tool %q", banned)
		}
	}
	for _, required := range []string{"do not launch an exploration agent", "do not repeat", "repository-relative paths"} {
		if !strings.Contains(compact, required) {
			t.Errorf("code-search body omits evidence-driven guard %q", required)
		}
	}
}

// The routing boundary must lead the skill body, before any command catalog, so
// a model reads it first: grep is for exact literal/regex text, glob is for
// filename patterns, and everything structural goes to Prowl.
func TestCodeSearchOpensWithTheGrepGlobBoundary(t *testing.T) {
	skill, ok := findSkill("code-search")
	if !ok {
		t.Fatal("code-search skill is not embedded")
	}
	opening := strings.ToLower(openingSection(skill.Content))
	if !strings.Contains(opening, "grep") || (!strings.Contains(opening, "literal") && !strings.Contains(opening, "regex")) {
		t.Errorf("opening does not reserve grep for exact literal/regex work: %q", opening)
	}
	if !strings.Contains(opening, "glob") || !strings.Contains(opening, "filename") {
		t.Errorf("opening does not reserve glob for filename patterns: %q", opening)
	}
}

func TestReviewCapabilityManifest(t *testing.T) {
	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := catalog.Get("review-large-change")
	if !ok {
		t.Fatal("review-large-change capability is not embedded")
	}
	want := capability.Manifest{
		Name:        "review-large-change",
		Title:       "Review a large change with complete coverage",
		Description: "Partition a workspace, commit, or PR range into bounded graph-aware review units and verify review coverage.",
		Triggers:    []string{"review pull request", "review commit", "review large diff", "review agent generated change"},
		Requires:    []string{"workspace index", "local git repository"},
		Outputs:     []string{"review plan", "bounded units", "coverage check"},
		Privacy:     "local-only",
		ReadOnly:    true,
		Version:     "1",
		Prompts:     []string{"review-change"},
		Resources:   []string{},
		Tools:       []string{},
		Commands: []string{
			"prowl review plan --base <base> --head <head>",
			"prowl review unit <review-id>/<unit-id>",
			"prowl review check --review <review-id> --report <report.json>",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("review-large-change manifest mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestReviewSkillFrontmatterAndProtocol(t *testing.T) {
	skill, ok := findSkill("prowl-pr-review")
	if !ok {
		t.Fatal("prowl-pr-review skill is not embedded")
	}
	if name := frontmatterValue(skill.Content, "name:"); name != skill.Name {
		t.Errorf("review skill frontmatter name = %q, want %q", name, skill.Name)
	}
	description := strings.ToLower(Description(skill.Content))
	for _, trigger := range []string{"pull request", "commit", "large", "agent-authored"} {
		if !strings.Contains(description, trigger) {
			t.Errorf("review skill description omits trigger %q: %q", trigger, description)
		}
	}

	opening := strings.ToLower(openingSection(skill.Content))
	if !strings.Contains(opening, "repository bytes") || !strings.Contains(opening, "untrusted") {
		t.Errorf("review skill opening does not label reviewed repository bytes untrusted: %q", opening)
	}

	compact := strings.Join(strings.Fields(strings.ToLower(skill.Content)), " ")
	for _, clause := range []string{
		"raw text additions plus removals greater than 300",
		"300 or fewer",
		"binary payload bytes are never counted",
		"one bounded unit at a time",
		"record its primary receipt",
		"removed behavior",
		"audit_removed_behavior_v1",
		"contract migration",
		"audit_contract_migration_v1",
		"test matrix",
		"audit_test_matrix_v1",
		"integration and gap",
		"audit_integration_gap_v1",
		"acknowledged_primary_hunk_ids",
		"acknowledged_audit_target_ids",
		"separate verification pass",
		"never invent ids or citations",
		"never summarize omitted units away",
	} {
		if !strings.Contains(compact, clause) {
			t.Errorf("review skill omits required protocol clause %q", clause)
		}
	}
	for _, command := range []string{
		"`prowl review plan [--base ref --head ref | --commit ref] [--structured]`",
		"`prowl review unit <review-id>/<unit-id> [--budget-tokens n --budget-bytes n]`",
		"`prowl review check --review <id> --report <regular-file|->`",
	} {
		if !strings.Contains(compact, command) {
			t.Errorf("review skill omits exact command contract %q", command)
		}
	}
	for _, state := range []string{"incomplete", "stale", "invalid"} {
		if !strings.Contains(compact, "no approval") || !strings.Contains(compact, state) {
			t.Errorf("review skill does not forbid approval for %s checks", state)
		}
	}
	if strings.Contains(compact, "mcp") {
		t.Error("review skill presents MCP as an alternative")
	}
}

func TestReviewClaudeCommandDelegatesToPortableSkill(t *testing.T) {
	command := nativeAsset(t, "claude", "commands/review.md")
	if !strings.Contains(command.Content, "$ARGUMENTS") {
		t.Error("review command drops the $ARGUMENTS placeholder")
	}
	lower := strings.ToLower(command.Content)
	if !strings.Contains(lower, "load and follow") || !strings.Contains(lower, "prowl:prowl-pr-review") {
		t.Error("review command does not delegate to the plugin-namespaced portable skill")
	}
	if got, want := frontmatterValue(command.Content, "allowed-tools:"),
		"Bash(prowl:*), Read, Grep, Glob"; got != want {
		t.Errorf("review command allowed-tools = %q, want %q", got, want)
	}
	for _, duplicated := range []string{
		"prowl review plan",
		"prowl review unit",
		"prowl review check",
	} {
		if strings.Contains(lower, duplicated) {
			t.Errorf("review command duplicates portable protocol command %q", duplicated)
		}
	}

	manifest := nativeAsset(t, "claude", ".claude-plugin/plugin.json")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(manifest.Content), &parsed); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	description, _ := parsed["description"].(string)
	if !strings.Contains(strings.ToLower(description), "large-change review") {
		t.Errorf("plugin manifest does not advertise large-change review: %q", description)
	}
}

// frontmatterValue reads one scalar key from the leading YAML frontmatter.
func frontmatterValue(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" && strings.HasPrefix(content, "---") {
			continue
		}
		if strings.HasPrefix(trimmed, key) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		}
	}
	return ""
}

// hasName reports whether names contains want.
func hasName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// findSkill returns the embedded active skill with the given name.
func findSkill(name string) (Skill, bool) {
	for _, skill := range All() {
		if skill.Name == name {
			return skill, true
		}
	}
	return Skill{}, false
}

// openingSection returns the skill body before its first level-2 heading -- the
// title and intro a model reads before any command catalog. Frontmatter is
// stripped so the boundary is asserted against real body prose, not metadata.
func openingSection(content string) string {
	body := content
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---"); end >= 0 {
			body = body[4+end+len("\n---"):]
		}
	}
	if i := strings.Index(body, "\n## "); i >= 0 {
		return body[:i]
	}
	return body
}

// Native assets are the harness-native integration files prowl installs beside
// the portable skills: a Claude plugin (manifest, command, scout agent, and a
// PreToolUse advisory hook), omp's scout agent plus a routing extension, and
// prowl's own routing skill. Every consumer -- the installer and its tests --
// reads the same files in the same order, so Native(client) must return them
// sorted by relative path, with unique paths, a stamped client, and non-empty
// content.
func TestNativeAssetsAreOrderedAndUnique(t *testing.T) {
	for _, client := range []string{"claude", "omp", "prowl"} {
		assets := Native(client)
		if len(assets) == 0 {
			t.Fatalf("Native(%q) returned no assets", client)
		}
		seen := make(map[string]bool, len(assets))
		for i, asset := range assets {
			if asset.Client != client {
				t.Errorf("Native(%q)[%d] is stamped client %q", client, i, asset.Client)
			}
			if asset.Path == "" {
				t.Errorf("Native(%q)[%d] has an empty relative path", client, i)
			}
			if asset.Content == "" {
				t.Errorf("Native(%q) asset %q has empty content", client, asset.Path)
			}
			if asset.Executable {
				t.Errorf("Native(%q) asset %q is marked executable; the bundle ships none", client, asset.Path)
			}
			if seen[asset.Path] {
				t.Errorf("Native(%q) repeats relative path %q", client, asset.Path)
			}
			seen[asset.Path] = true
			if i > 0 && assets[i-1].Path >= asset.Path {
				t.Errorf("Native(%q) is not sorted: %q precedes %q", client, assets[i-1].Path, asset.Path)
			}
		}
	}
}

// Native must resolve only immediate, supported clients. A nested pseudo-client
// ("claude/agents"), a traversal-like name, an unsupported harness, or an empty
// string would otherwise let a caller walk an arbitrary embedded subtree or a
// partial slice of one client's assets; every such name must return nil.
func TestNativeRejectsUnknownAndNestedClients(t *testing.T) {
	for _, client := range []string{
		"", "unknown", "Claude", "OMP", "claude/agents", "omp/extensions",
		"claude/", "/claude", "../legacy", "native", "claude/../omp", ".",
	} {
		if assets := Native(client); assets != nil {
			t.Errorf("Native(%q) returned %d assets; unsupported clients must return nil", client, len(assets))
		}
	}
}

// The Claude plugin manifest is the entry point Claude reads to load the
// integration: it must be valid JSON, be named prowl, and carry the {{VERSION}}
// template token so the installer stamps the shipping release instead of a
// baked-in stale one.
func TestNativeClaudeManifestParsesAndIsVersioned(t *testing.T) {
	manifest := nativeAsset(t, "claude", ".claude-plugin/plugin.json")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(manifest.Content), &parsed); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if parsed["name"] != "prowl" {
		t.Errorf("plugin.json name = %v, want prowl", parsed["name"])
	}
	version, _ := parsed["version"].(string)
	if version != "{{VERSION}}" {
		t.Errorf("plugin.json version = %q, want the exact template token {{VERSION}}", version)
	}
	// The template token is the manifest's alone; if it leaked into another
	// asset the installer would rewrite a file that must ship verbatim.
	for _, client := range []string{"claude", "omp"} {
		for _, asset := range Native(client) {
			if asset.Path == ".claude-plugin/plugin.json" {
				continue
			}
			if strings.Contains(asset.Content, "{{VERSION}}") {
				t.Errorf("native %s asset %q contains the {{VERSION}} token; only the manifest may", client, asset.Path)
			}
		}
	}
}

// Claude discovers the integration by its plugin layout: a slash command, a
// scout subagent, and a hooks file whose PreToolUse rule fires the advisory on
// broad-search tools. The advisory must be exactly the prowl binary call, with
// no jq, Python, shell interpolation of tool input, or network hop.
func TestNativeClaudeExposesCommandAgentAndHook(t *testing.T) {
	nativeAsset(t, "claude", "commands/search.md")
	nativeAsset(t, "claude", "agents/code-scout.md")
	hooks := nativeAsset(t, "claude", "hooks/hooks.json")

	var parsed struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(hooks.Content), &parsed); err != nil {
		t.Fatalf("hooks.json is not valid JSON: %v", err)
	}
	pre := parsed.Hooks["PreToolUse"]
	if len(pre) == 0 {
		t.Fatal("hooks.json defines no PreToolUse hook")
	}
	var found bool
	for _, matcher := range pre {
		if matcher.Matcher != "Grep|Glob|Bash" {
			continue
		}
		for _, h := range matcher.Hooks {
			if h.Type == "command" && h.Command == "prowl _search-advisory" {
				found = true
			}
		}
	}
	if !found {
		t.Error(`hooks.json lacks a PreToolUse "Grep|Glob|Bash" hook running "prowl _search-advisory"`)
	}
	lower := strings.ToLower(hooks.Content)
	for _, banned := range []string{"jq", "python", "$(", "${", "curl", "http"} {
		if strings.Contains(lower, banned) {
			t.Errorf("hooks.json advisory depends on %q; it must call the prowl binary directly", banned)
		}
	}
}

// Both native scouts inherit the existing scout's contract: read-only (no write
// or edit tool), and structural discovery leads with the prowl CLI before any
// grep fallback.
func TestNativeScoutsAreReadOnlyAndCLIFirst(t *testing.T) {
	for _, client := range []string{"claude", "omp"} {
		scout := nativeAsset(t, client, "agents/code-scout.md")
		content := scout.Content
		lower := strings.ToLower(content)
		if !strings.Contains(content, "prowl ") {
			t.Errorf("%s code-scout never runs the prowl CLI", client)
		}
		tools := strings.ToLower(frontmatterValue(content, "tools:"))
		if tools == "" {
			t.Errorf("%s code-scout declares no tools frontmatter", client)
		}
		for _, w := range []string{"write", "edit"} {
			if strings.Contains(tools, w) {
				t.Errorf("%s code-scout grants the state-changing tool %q: %q", client, w, tools)
			}
		}
		if !strings.Contains(lower, "read-only") && !strings.Contains(lower, "read only") {
			t.Errorf("%s code-scout does not declare a read-only contract", client)
		}
		body := strings.ToLower(openingSection(content))
		p := strings.Index(body, "prowl ")
		if g := strings.Index(body, "grep"); g >= 0 && p > g {
			t.Errorf("%s code-scout reaches for grep before prowl", client)
		}
	}
}

// The CLI-first cutover removes MCP as a surface: no native asset may name an
// MCP transport or a retired MCP tool, or an agent could route back to it.
func TestNativeAssetsNameNoMCPTool(t *testing.T) {
	for _, client := range []string{"claude", "omp", "prowl"} {
		for _, asset := range Native(client) {
			lower := strings.ToLower(asset.Content)
			for _, banned := range []string{"mcp", "search_context", "read_symbol"} {
				if strings.Contains(lower, banned) {
					t.Errorf("native %s asset %q names the MCP token %q", client, asset.Path, banned)
				}
			}
		}
	}
}

// nativeAsset returns the native asset for client at the given relative path, or
// fails the test when the bundle omits it.
func nativeAsset(t *testing.T, client, path string) Asset {
	t.Helper()
	for _, asset := range Native(client) {
		if asset.Path == path {
			return asset
		}
	}
	t.Fatalf("native %s bundle is missing %q", client, path)
	return Asset{}
}

// The prowl client ships a single native routing skill: prowl auto-discovers a
// skill at <root>/<name>/SKILL.md, so the asset must be exactly SKILL.md, carry
// frontmatter naming it prowl (matching the install directory), and lead with
// the grep/glob-vs-Prowl boundary that routes structural work through the
// read-only prowl CLI.
func TestNativeProwlRoutingSkill(t *testing.T) {
	assets := Native("prowl")
	if len(assets) != 1 {
		t.Fatalf("Native(prowl) returned %d assets, want exactly the routing SKILL.md", len(assets))
	}
	skill := nativeAsset(t, "prowl", "SKILL.md")
	if name := frontmatterValue(skill.Content, "name:"); name != "prowl" {
		t.Errorf("prowl routing skill frontmatter name = %q, want prowl", name)
	}
	if desc := frontmatterValue(skill.Content, "description:"); desc == "" {
		t.Error("prowl routing skill has no frontmatter description")
	}
	body := strings.ToLower(skill.Content)
	for _, want := range []string{"prowl", "grep", "glob"} {
		if !strings.Contains(body, want) {
			t.Errorf("prowl routing skill body omits %q", want)
		}
	}
	for _, banned := range []string{"mcp", "search_context", "read_symbol"} {
		if strings.Contains(body, banned) {
			t.Errorf("prowl routing skill names the MCP token %q", banned)
		}
	}
}
