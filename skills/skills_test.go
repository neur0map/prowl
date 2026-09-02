package skills

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/capability"
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

// The body routes structural work through the prowl-agent CLI and never presents
// MCP as a transport choice: an agent should not have to pick between CLI and
// MCP, and the retired MCP tool names must be gone.
func TestCodeSearchBodyRoutesThroughTheCLINotMCP(t *testing.T) {
	skill, ok := findSkill("code-search")
	if !ok {
		t.Fatal("code-search skill is not embedded")
	}
	if !strings.Contains(skill.Content, "prowl-agent ") {
		t.Error("code-search body invokes no prowl-agent CLI command")
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
			"prowl-agent review plan --base <base> --head <head>",
			"prowl-agent review unit <review-id>/<unit-id>",
			"prowl-agent review check --review <review-id> --report <report.json>",
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
		"raw_additions + raw_deletions > 300",
		"at or below that threshold",
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
	for _, competing := range []string{
		"raw text additions plus removals",
		"greater than 300",
		"300 or fewer",
	} {
		if strings.Contains(compact, competing) {
			t.Errorf("review skill retains competing threshold wording %q", competing)
		}
	}
	for _, command := range []string{
		"`prowl-agent review plan [--base ref --head ref | --commit ref] [--structured]`",
		"`prowl-agent review unit <review-id>/<unit-id> [--budget-tokens n --budget-bytes n]`",
		"`prowl-agent review check --review <id> --report <regular-file|->`",
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

// Generated routing stays compact because this portable skill owns the detailed
// unit, receipt, and four-audit protocol for every host.
func TestReviewRoutingPortableSkillOwnsDetailedProtocol(t *testing.T) {
	skill, ok := findSkill("prowl-pr-review")
	if !ok {
		t.Fatal("prowl-pr-review skill is not embedded")
	}
	compact := strings.Join(strings.Fields(strings.ToLower(skill.Content)), " ")
	for _, detail := range []string{
		"prowl-agent review unit",
		"record its primary receipt",
		"audit_removed_behavior_v1",
		"audit_contract_migration_v1",
		"audit_test_matrix_v1",
		"audit_integration_gap_v1",
		"acknowledged_primary_hunk_ids",
		"acknowledged_audit_target_ids",
	} {
		if !strings.Contains(compact, detail) {
			t.Errorf("portable review skill does not own protocol detail %q", detail)
		}
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
		"Bash(prowl-agent:*), Read, Grep, Glob"; got != want {
		t.Errorf("review command allowed-tools = %q, want %q", got, want)
	}
	for _, duplicated := range []string{
		"prowl-agent review plan",
		"prowl-agent review unit",
		"prowl-agent review check",
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
// PreToolUse advisory hook) and omp's scout agent plus a routing extension.
// Every consumer -- the installer and its tests -- reads the same files in the
// same order, so Native(client) must return them sorted by relative path, with
// unique paths, a stamped client, and non-empty content.
func TestNativeAssetsAreOrderedAndUnique(t *testing.T) {
	for _, client := range []string{"claude", "omp"} {
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
// the broad-search tools. The advisory must be exactly the prowl-agent binary
// call, with no jq, Python, shell interpolation of tool input, or network hop.
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
			if h.Type == "command" && h.Command == "prowl-agent _search-advisory" {
				found = true
			}
		}
	}
	if !found {
		t.Error(`hooks.json lacks a PreToolUse "Grep|Glob|Bash" hook running "prowl-agent _search-advisory"`)
	}
	lower := strings.ToLower(hooks.Content)
	for _, banned := range []string{"jq", "python", "$(", "${", "curl", "http"} {
		if strings.Contains(lower, banned) {
			t.Errorf("hooks.json advisory depends on %q; it must call the prowl-agent binary directly", banned)
		}
	}
}

// The explicit /search command must delegate, not restate a routing table that
// can drift from the canonical one: it keeps the $ARGUMENTS placeholder,
// instructs Claude to load and follow the bundled skill by its plugin-namespaced
// name prowl:code-search (an unscoped "code-search" will not resolve to the
// plugin's copy), holds the grep/glob boundary, and enumerates none of the
// canonical CLI subcommands itself.
func TestNativeSearchCommandDelegatesToCanonicalTable(t *testing.T) {
	cmd := nativeAsset(t, "claude", "commands/search.md")
	content := cmd.Content
	lower := strings.ToLower(content)
	if !strings.Contains(content, "$ARGUMENTS") {
		t.Error("search command drops the $ARGUMENTS placeholder")
	}
	if !strings.Contains(lower, "prowl-agent") {
		t.Error("search command does not route through the prowl-agent CLI")
	}
	if !strings.Contains(lower, "prowl:code-search") {
		t.Error("search command does not name the plugin-namespaced skill prowl:code-search; an unscoped code-search may not load the plugin's copy")
	}
	if !strings.Contains(lower, "follow") && !strings.Contains(lower, "load") {
		t.Error("search command does not instruct loading/following the code-search skill before choosing a command")
	}
	// Delegation means it must not carry its own copy of the routing table.
	for _, dup := range []string{"prowl-agent find", "prowl-agent def", "prowl-agent outline", "prowl-agent references", "prowl-agent impact", "prowl-agent overview"} {
		if strings.Contains(content, dup) {
			t.Errorf("search command duplicates the canonical routing table (names %q); it must delegate to code-search", dup)
		}
	}
	if !strings.Contains(lower, "grep") || !strings.Contains(lower, "glob") {
		t.Error("search command drops the grep/glob boundary")
	}
}

// Both native scouts inherit the existing scout's contract: read-only (no write
// or edit tool), and structural discovery leads with the prowl-agent CLI before
// any grep fallback.
func TestNativeScoutsAreReadOnlyAndCLIFirst(t *testing.T) {
	for _, client := range []string{"claude", "omp"} {
		scout := nativeAsset(t, client, "agents/code-scout.md")
		content := scout.Content
		lower := strings.ToLower(content)
		if !strings.Contains(content, "prowl-agent") {
			t.Errorf("%s code-scout never runs the prowl-agent CLI", client)
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
		p := strings.Index(body, "prowl-agent")
		if g := strings.Index(body, "grep"); g >= 0 && p > g {
			t.Errorf("%s code-scout reaches for grep before prowl-agent", client)
		}
	}
}

// The omp routing extension is advisory only. Without standing up a Node/Bun
// runtime (CONTRIBUTING forbids making Go tests require an interpreter; live OMP
// smoke comes later), this asserts the static contract of the source: it
// registers only a tool_result observer, classifies the lowercase grep/glob
// tools and search-shell bash commands, returns early on errors, and patches
// the result by appending exactly one text chunk after the spread of existing
// content -- with no block, input, details, or isError override.
func TestNativeOMPExtensionIsNonBlockingAppendOnly(t *testing.T) {
	ext := nativeAsset(t, "omp", "extensions/prowl-routing.ts")
	content := ext.Content

	// (a) Only tool_result is registered -- no tool_call, so it cannot block or
	// rewrite tool input.
	if got := strings.Count(content, "pi.on("); got != 1 {
		t.Errorf("routing extension registers %d handlers; it must register exactly one (tool_result)", got)
	}
	if !strings.Contains(content, `pi.on("tool_result"`) {
		t.Error("routing extension does not observe tool_result")
	}
	if strings.Contains(content, `pi.on("tool_call"`) {
		t.Error("routing extension intercepts tool_call; it must not rewrite input or block")
	}

	// (b) Lowercase grep/glob are classified as broad searches.
	for _, positive := range []string{`toolName === "grep"`, `toolName === "glob"`} {
		if !strings.Contains(content, positive) {
			t.Errorf("routing extension does not classify %s as a broad search", positive)
		}
	}

	// Native grep/glob and shell searches must be narrowed by path/operands
	// before the advisory fires; exact or file-bounded controls remain native.
	for _, classifier := range []string{"repoWidePath(", "bashIsRepoWide("} {
		if !strings.Contains(content, classifier) {
			t.Errorf("routing extension omits bounded-search classifier %q", classifier)
		}
	}
	if strings.Contains(content, `if (toolName === "grep" || toolName === "glob") return true`) {
		t.Error("routing extension treats every native grep/glob as repository-wide")
	}
	if !strings.Contains(content, "words.slice(1).every") {
		t.Error("routing extension does not recognize explicit repository-root grep operands")
	}

	// (c) A bash call is classified by extracting its command and testing it
	// against the TREE_SEARCH utility set.
	if !strings.Contains(content, `toolName === "bash"`) {
		t.Error("routing extension does not classify bash calls")
	}
	if !strings.Contains(content, "input?.command") {
		t.Error("routing extension does not extract the bash command from tool input")
	}
	if !strings.Contains(content, "TREE_SEARCH.test(") {
		t.Error("routing extension does not test bash commands against TREE_SEARCH")
	}
	treeSearch := lineContaining(content, "const TREE_SEARCH")
	for _, util := range []string{"rg", "grep", "egrep", "fgrep", "ag", "ack", "find", "fd"} {
		if !strings.Contains(treeSearch, util) {
			t.Errorf("TREE_SEARCH omits the search utility %q: %q", util, treeSearch)
		}
	}

	// (d) A non-search tool call (and a non-matching bash command) falls through
	// to an explicit negative.
	if !strings.Contains(content, "return false") {
		t.Error("routing extension has no explicit non-matching fallthrough (return false)")
	}

	// (e) Error results return early, before any reminder is appended.
	if !strings.Contains(content, "if (event.isError) return") {
		t.Error("routing extension does not return early on error results")
	}

	// (f)/(g) The single object return is a content patch: it spreads the
	// existing content and appends exactly one text chunk, with no other keys.
	ret := returnObject(content)
	if ret == "" {
		t.Fatal("routing extension has no object return to inspect")
	}
	if !strings.Contains(ret, "...event.content") {
		t.Error("returned patch does not spread the existing result content; it must be append-only")
	}
	if got := strings.Count(ret, `type: "text"`); got != 1 {
		t.Errorf("returned patch appends %d text chunks; it must append exactly one", got)
	}
	if !strings.Contains(ret, "content:") {
		t.Error("returned patch does not set content")
	}
	for _, forbidden := range []string{"block", "input", "details", "isError"} {
		if strings.Contains(ret, forbidden) {
			t.Errorf("returned patch carries a %q field; it must only patch content", forbidden)
		}
	}
}

// lineContaining returns the first line of s that contains sub, or "".
func lineContaining(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// returnObject returns the text of the first object-literal return in s -- from
// "return {" through its terminating "};" -- so a test can inspect only the
// fields the handler actually returns.
func returnObject(s string) string {
	start := strings.Index(s, "return {")
	if start < 0 {
		return ""
	}
	rest := s[start:]
	if end := strings.Index(rest, "};"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// The CLI-first cutover removes MCP as a surface: no native asset may name an
// MCP transport or a retired MCP tool, or an agent could route back to it.
func TestNativeAssetsNameNoMCPTool(t *testing.T) {
	for _, client := range []string{"claude", "omp"} {
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
