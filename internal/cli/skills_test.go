package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/neur0map/prowl/internal/setup"
)

// skillsOpts builds a user-install target confined to temporary directories so a
// runSkills exercise never touches the real user's config roots. Clients are
// explicit (production supplies DetectInstalledHarnesses) so planning is
// deterministic regardless of what harnesses the test host happens to have.
func skillsOpts(t *testing.T, clients ...string) setup.UserInstallOptions {
	t.Helper()
	if len(clients) == 0 {
		clients = []string{"claude"}
	}
	return setup.UserInstallOptions{
		Home:     t.TempDir(),
		StateDir: t.TempDir(),
		Version:  "9.9.9",
		Clients:  clients,
	}
}

// manifestPresent reports whether an apply committed the ownership manifest. Its
// presence is the single reliable signal that a write actually happened: no
// apply, no manifest.
func manifestPresent(opts setup.UserInstallOptions) bool {
	_, err := os.Stat(filepath.Join(opts.StateDir, "prowl-agent", "agent-assets.json"))
	return err == nil
}

// TestSkillsNonInteractivePreviewWritesNothing proves a non-interactive run is a
// pure preview: it renders the plan and commits nothing, so an agent shelling
// out to `prowl skills` in a pipe can never mutate the user's config.
func TestSkillsNonInteractivePreviewWritesNothing(t *testing.T) {
	opts := skillsOpts(t)
	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader(""), &out, false, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "nothing was written") {
		t.Errorf("non-interactive run did not report a no-write preview:\n%s", out.String())
	}
	if manifestPresent(opts) {
		t.Error("non-interactive run committed the ownership manifest")
	}
	if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")); err == nil {
		t.Error("non-interactive run installed an asset")
	}
}

// TestSkillsDefaultNoKeepsFilesystemUntouched proves that in interactive mode the
// default answer is No: both an empty line and an explicit "n" decline, and
// neither writes anything.
func TestSkillsDefaultNoKeepsFilesystemUntouched(t *testing.T) {
	for _, answer := range []string{"", "\n", "n\n", "N\n", "no\n"} {
		opts := skillsOpts(t)
		var out bytes.Buffer
		if err := runSkills(opts, strings.NewReader(answer), &out, true, false); err != nil {
			t.Fatalf("runSkills(%q): %v", answer, err)
		}
		if manifestPresent(opts) {
			t.Errorf("answer %q applied changes despite a No default", answer)
		}
		if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "skills", "prowl")); err == nil {
			t.Errorf("answer %q installed assets despite a No default", answer)
		}
	}
}

// TestSkillsYesAppliesPlanOnce proves an explicit "y" (and "yes") applies the
// reviewed plan: assets land under the client root and the ownership manifest is
// committed exactly once.
func TestSkillsYesAppliesPlanOnce(t *testing.T) {
	for _, answer := range []string{"y\n", "yes\n", "Y\n", "YES\n"} {
		opts := skillsOpts(t)
		var out bytes.Buffer
		if err := runSkills(opts, strings.NewReader(answer), &out, true, false); err != nil {
			t.Fatalf("runSkills(%q): %v", answer, err)
		}
		if !manifestPresent(opts) {
			t.Errorf("answer %q did not commit the ownership manifest", answer)
		}
		asset := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
		if _, err := os.Stat(asset); err != nil {
			t.Errorf("answer %q did not install %s: %v", answer, asset, err)
		}
		if !strings.Contains(strings.ToLower(out.String()), "installed") {
			t.Errorf("answer %q did not report the apply:\n%s", answer, out.String())
		}
	}
}

// TestSkillsConflictsPreservedAndPrinted proves a destination Prowl does not own
// is surfaced as a conflict, printed clearly, and left byte-for-byte untouched by
// an approved apply, while the other assets still install.
func TestSkillsConflictsPreservedAndPrinted(t *testing.T) {
	opts := skillsOpts(t)
	foreign := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	const keep = "someone else's command\n"
	if err := os.WriteFile(foreign, []byte(keep), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader("y\n"), &out, true, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}

	rendered := out.String()
	if !strings.Contains(strings.ToLower(rendered), "conflict") {
		t.Errorf("conflict was not printed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "commands/search.md") {
		t.Errorf("conflict destination was not named:\n%s", rendered)
	}
	if got, _ := os.ReadFile(foreign); string(got) != keep {
		t.Errorf("apply overwrote the unowned file: %q", got)
	}
	// A non-conflicting asset still installs alongside the preserved conflict.
	if _, err := os.Stat(filepath.Join(opts.Home, ".claude", "skills", "prowl", "hooks", "hooks.json")); err != nil {
		t.Errorf("apply skipped a clean asset because of an unrelated conflict: %v", err)
	}
}

// TestSkillsConflictOnlyPlanReportsUnresolved proves a plan with no writable
// actions but unresolved conflicts is never reported as "up to date": when every
// destination is a foreign, unowned file nothing installs, and the presenter
// tells the user conflicts remain to resolve instead of falsely claiming a
// clean, current install.
func TestSkillsConflictOnlyPlanReportsUnresolved(t *testing.T) {
	opts := skillsOpts(t)
	// Plant a foreign, unowned file at every destination the install would
	// target, so each candidate is a conflict and the plan has zero writes.
	plan, err := setup.PlanUserSkills(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) == 0 {
		t.Fatal("baseline plan had no install actions to convert to conflicts")
	}
	for _, action := range plan.Actions {
		dest := filepath.Join(opts.Home, filepath.FromSlash(action.Destination))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte("foreign file, not prowl-owned\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader(""), &out, false, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	got := out.String()
	if strings.Contains(strings.ToLower(got), "up to date") {
		t.Errorf("conflict-only plan falsely reported up to date:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), "unresolved conflict") {
		t.Errorf("conflict-only plan did not report unresolved conflicts:\n%s", got)
	}
	if manifestPresent(opts) {
		t.Error("conflict-only preview committed the ownership manifest")
	}
}

// TestSkillsOutputNamesRestartReload proves a successful install tells the user to
// reload each detected client, so the freshly installed skills actually take
// effect.
func TestSkillsOutputNamesRestartReload(t *testing.T) {
	opts := skillsOpts(t, "claude", "omp")
	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader("y\n"), &out, true, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	lower := strings.ToLower(out.String())
	for _, want := range []string{"restart", "claude", "reload", "omp"} {
		if !strings.Contains(lower, want) {
			t.Errorf("apply output did not name the %q step:\n%s", want, out.String())
		}
	}
}

// TestSkillsOutputNamesProwlLegacyReload proves a successful prowl-legacy install
// tells the user to reload Prowl Legacy so the freshly installed prowl agent
// skills take effect.
func TestSkillsOutputNamesProwlLegacyReload(t *testing.T) {
	opts := skillsOpts(t, setup.IntegrationProwl)
	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader("y\n"), &out, true, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	lower := strings.ToLower(out.String())
	for _, want := range []string{"reload prowl legacy"} {
		if !strings.Contains(lower, want) {
			t.Errorf("apply output did not name the %q step:\n%s", want, out.String())
		}
	}
}

// TestSkillsCommandArgsAndFlags proves the public command is an argument-free,
// subcommand-free installer that exposes exactly the two documented flags: --yes
// (the sanctioned non-interactive apply) and --clients (client selection). Any
// other flag would be an undocumented surface.
func TestSkillsCommandArgsAndFlags(t *testing.T) {
	cmd := newSkillsCmd("test")
	if cmd.Args == nil {
		t.Fatal("skills command accepts arbitrary positional arguments")
	}
	if err := cmd.Args(cmd, []string{"extra"}); err == nil {
		t.Error("skills command accepted a positional argument")
	}
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("skills command rejected a bare invocation: %v", err)
	}
	if subs := cmd.Commands(); len(subs) != 0 {
		t.Errorf("skills command exposes subcommands: %v", subs)
	}
	got := map[string]bool{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) { got[f.Name] = true })
	want := map[string]bool{"yes": true, "clients": true}
	if len(got) != len(want) {
		t.Errorf("skills command flags = %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("skills command missing the %q flag", name)
		}
	}
}

// TestSearchAdvisoryClassification pins the conservative advisory classifier: only
// repository-wide native searches and shell rg/grep/find earn an advisory, while
// bounded, named, prowl, unknown, and malformed inputs stay silent. False
// negatives are acceptable; the hook is advisory, never policy.
func TestSearchAdvisoryClassification(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		advise  bool
	}{
		{"repo-wide grep", `{"hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"TODO"}}`, true},
		{"repo-wide grep dot path", `{"tool_name":"Grep","tool_input":{"pattern":"TODO","path":"."}}`, true},
		{"repo-wide glob", `{"tool_name":"Glob","tool_input":{"pattern":"**/*.go"}}`, true},
		{"shell rg", `{"tool_name":"Bash","tool_input":{"command":"rg TODO"}}`, true},
		{"shell grep recursive", `{"tool_name":"Bash","tool_input":{"command":"grep -rn TODO ."}}`, true},
		{"shell find", `{"tool_name":"Bash","tool_input":{"command":"find . -name '*.go'"}}`, true},
		{"shell grep in pipeline", `{"tool_name":"Bash","tool_input":{"command":"cat notes | grep TODO"}}`, true},
		{"bounded grep", `{"tool_name":"Grep","tool_input":{"pattern":"TODO","path":"internal/cli"}}`, false},
		{"bounded glob", `{"tool_name":"Glob","tool_input":{"pattern":"*.go","path":"internal/cli"}}`, false},
		{"prowl bash", `{"tool_name":"Bash","tool_input":{"command":"prowl search TODO"}}`, false},
		{"named read", `{"tool_name":"Read","tool_input":{"file_path":"/repo/main.go"}}`, false},
		{"unknown tool", `{"tool_name":"Edit","tool_input":{"file_path":"/repo/main.go"}}`, false},
		{"non-search bash", `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`, false},
		{"malformed json", `{not valid`, false},
		{"empty input", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runSearchAdvisory(strings.NewReader(tc.payload), &out); err != nil {
				t.Fatalf("runSearchAdvisory returned error (must fail open): %v", err)
			}
			emitted := strings.TrimSpace(out.String()) != ""
			if emitted != tc.advise {
				t.Errorf("advise=%v, want %v (output=%q)", emitted, tc.advise, out.String())
			}
		})
	}
}

// TestSearchAdvisoryOutputShape proves an emitted advisory is exactly Claude's
// additionalContext response for PreToolUse and carries no permission decision,
// so the hook can inform but never approve, ask, defer, or deny.
func TestSearchAdvisoryOutputShape(t *testing.T) {
	var out bytes.Buffer
	if err := runSearchAdvisory(strings.NewReader(`{"tool_name":"Grep","tool_input":{"pattern":"TODO"}}`), &out); err != nil {
		t.Fatalf("runSearchAdvisory: %v", err)
	}
	if strings.Contains(out.String(), "permissionDecision") {
		t.Fatalf("advisory carried a permission decision:\n%s", out.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("advisory is not valid JSON: %v\n%s", err, out.String())
	}
	raw, ok := payload["hookSpecificOutput"]
	if !ok {
		t.Fatalf("advisory missing hookSpecificOutput: %s", out.String())
	}
	var hook struct {
		HookEventName      string `json:"hookEventName"`
		AdditionalContext  string `json:"additionalContext"`
		PermissionDecision string `json:"permissionDecision"`
	}
	if err := json.Unmarshal(raw, &hook); err != nil {
		t.Fatal(err)
	}
	if hook.HookEventName != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", hook.HookEventName)
	}
	if strings.TrimSpace(hook.AdditionalContext) == "" {
		t.Error("advisory carried an empty additionalContext")
	}
	if hook.PermissionDecision != "" {
		t.Errorf("advisory carried a permission decision: %q", hook.PermissionDecision)
	}
}

// TestSearchAdvisoryNeverEchoesInput proves the advisory is constant: it never
// copies the caller's tool input (a pattern, path, or command) into its output,
// so a hook can never be used to reflect attacker-controlled text back to Claude.
func TestSearchAdvisoryNeverEchoesInput(t *testing.T) {
	const secret = "ZZ_SUPER_SECRET_PATTERN_ZZ"
	inputs := []string{
		`{"tool_name":"Grep","tool_input":{"pattern":"` + secret + `"}}`,
		`{"tool_name":"Glob","tool_input":{"pattern":"**/` + secret + `"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"grep -r ` + secret + ` ."}}`,
	}
	for _, in := range inputs {
		var out bytes.Buffer
		if err := runSearchAdvisory(strings.NewReader(in), &out); err != nil {
			t.Fatalf("runSearchAdvisory: %v", err)
		}
		if strings.TrimSpace(out.String()) == "" {
			t.Fatalf("expected an advisory for %q", in)
		}
		if strings.Contains(out.String(), secret) {
			t.Errorf("advisory echoed caller input:\n%s", out.String())
		}
	}
}

// TestStreamsInteractive proves interactivity is judged by a real terminal
// predicate on the exact streams runSkills is handed, not process-global stdio
// and not a char-device heuristic: buffers, pipes, regular files, and /dev/null
// are all non-interactive. /dev/null is the key case -- it is a character device
// but not a terminal, so a TTY-in + /dev/null-out combination must never accept a
// blind write. A live positive is covered by the controller's supervised PTY
// smoke, not reproducible in a unit test.
func TestStreamsInteractive(t *testing.T) {
	if streamsInteractive(&bytes.Buffer{}, &bytes.Buffer{}) {
		t.Error("byte buffers classified as an interactive terminal")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if streamsInteractive(r, w) {
		t.Error("an os.Pipe classified as interactive")
	}

	reg, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if streamsInteractive(reg, reg) {
		t.Error("a regular file classified as interactive")
	}

	dev, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	if streamsInteractive(dev, dev) {
		t.Error("/dev/null (a char device that is not a terminal) classified as interactive")
	}
}

// TestSkillsPropagatesVerifyError proves a post-apply verification failure is a
// hard error: runSkills returns it and prints no success/restart output, rather
// than inventing a verified-and-done state after an unverifiable apply.
func TestSkillsPropagatesVerifyError(t *testing.T) {
	opts := skillsOpts(t)
	verify := func(setup.UserInstallOptions) (setup.UserHealth, error) {
		return setup.UserHealth{}, errors.New("verify boom")
	}

	var out bytes.Buffer
	err := runSkillsWithVerifier(opts, strings.NewReader("y\n"), &out, true, false, verify)
	if err == nil {
		t.Fatal("runSkills swallowed a verification failure")
	}
	if !strings.Contains(err.Error(), "verify boom") {
		t.Errorf("error did not propagate verify failure: %v", err)
	}
	if strings.Contains(strings.ToLower(out.String()), "restart") || strings.Contains(strings.ToLower(out.String()), "reload") {
		t.Errorf("printed restart guidance after an unverifiable apply:\n%s", out.String())
	}
}

// TestSkillsPreviewRendersUnchanged proves the preview lists every planned action,
// including unchanged destinations, not only the mutating ones -- so a re-run
// shows the full owned set rather than an empty plan.
func TestSkillsPreviewRendersUnchanged(t *testing.T) {
	opts := skillsOpts(t)
	if _, err := setup.ApplyUserSkills(opts, mustPlanForSkills(t, opts), true); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	var out bytes.Buffer
	if err := runSkills(opts, strings.NewReader(""), &out, false, false); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, string(setup.UserActionUnchanged)) {
		t.Errorf("preview omitted unchanged destinations:\n%s", rendered)
	}
	if !strings.Contains(rendered, "commands/search.md") {
		t.Errorf("preview omitted an owned destination:\n%s", rendered)
	}
}

func mustPlanForSkills(t *testing.T, opts setup.UserInstallOptions) setup.UserPlan {
	t.Helper()
	plan, err := setup.PlanUserSkills(opts)
	if err != nil {
		t.Fatalf("PlanUserSkills: %v", err)
	}
	return plan
}

// TestSearchAdvisoryConservativeShellClassification pins the quote-aware,
// bounds-checked shell classifier: quoted separators are inert, a file- or
// directory-bounded search is silent, only the command word (never the search
// text) is treated as prowl, and a broad search is not suppressed by an adjacent
// Prowl invocation.
func TestSearchAdvisoryConservativeShellClassification(t *testing.T) {
	cases := []struct {
		name    string
		command string
		advise  bool
	}{
		{"quoted separator is inert", "printf 'x | grep TODO'", false},
		{"file-bounded grep", "grep TODO internal/cli/skills.go", false},
		{"dir-bounded find", "find internal/cli", false},
		{"prowl as search text still advises", "rg prowl .", true},
		{"broad search after prowl invocation", "prowl status && grep -r TODO .", true},
		{"prowl invocation alone stays silent", "prowl search TODO", false},
		{"repo-wide grep still advises", "grep -rn TODO .", true},
		{"pipeline grep still advises", "cat notes | grep TODO", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"tool_name":"Bash","tool_input":{"command":` + mustJSON(t, tc.command) + `}}`
			var out bytes.Buffer
			if err := runSearchAdvisory(strings.NewReader(payload), &out); err != nil {
				t.Fatalf("runSearchAdvisory: %v", err)
			}
			emitted := strings.TrimSpace(out.String()) != ""
			if emitted != tc.advise {
				t.Errorf("advise=%v, want %v (output=%q)", emitted, tc.advise, out.String())
			}
		})
	}
}

// TestSearchAdvisoryShellEdgeCases pins two conservative refinements: a command
// with unmatched quotes or a dangling escape is unparseable and stays silent
// (fail closed), and a search whose pattern is attached to a flag is still
// recognized as file/dir-bounded (no false-positive advisory).
func TestSearchAdvisoryShellEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		command string
		advise  bool
	}{
		{"unmatched single quote fails closed", "rg 'TODO", false},
		{"unmatched double quote fails closed", "grep -r \"TODO .", false},
		{"dangling escape fails closed", "rg TODO \\", false},
		{"attached short pattern is bounded", "grep -eTODO internal/cli/skills.go", false},
		{"attached long pattern is bounded", "rg --regexp=TODO internal/cli/skills.go", false},
		{"attached pattern repo-wide still advises", "grep -eTODO .", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"tool_name":"Bash","tool_input":{"command":` + mustJSON(t, tc.command) + `}}`
			var out bytes.Buffer
			if err := runSearchAdvisory(strings.NewReader(payload), &out); err != nil {
				t.Fatalf("runSearchAdvisory: %v", err)
			}
			emitted := strings.TrimSpace(out.String()) != ""
			if emitted != tc.advise {
				t.Errorf("advise=%v, want %v (output=%q)", emitted, tc.advise, out.String())
			}
		})
	}
}

// TestSearchAdvisoryFindTraversalOptions proves find's leading traversal options
// are skipped before start-path classification, so a bounded search behind
// -H/-L/-P (and value-bearing -D/-O) stays silent while a repo-root one advises.
func TestSearchAdvisoryFindTraversalOptions(t *testing.T) {
	cases := []struct {
		name    string
		command string
		advise  bool
	}{
		{"logical follow bounded", "find -L internal/cli -name '*.go'", false},
		{"logical follow repo-wide", "find -L . -name '*.go'", true},
		{"stacked traversal flags bounded", "find -H -P internal/cli", false},
		{"debug option value bounded", "find -D tree internal/cli", false},
		{"optimize attached bounded", "find -O3 internal/cli", false},
		{"double dash then bounded path", "find -- internal/cli", false},
		{"traversal then repo root advises", "find -P . -type f", true},
	}
	runShellAdviseCases(t, cases)
}

// TestSearchAdvisoryShellComments proves an unquoted token-boundary '#' starts a
// comment through newline, so a search hidden behind a comment never runs, while
// quoted/embedded hashes stay data and a real search before a trailing comment
// still advises.
func TestSearchAdvisoryShellComments(t *testing.T) {
	cases := []struct {
		name    string
		command string
		advise  bool
	}{
		{"commented grep is inert", "printf done # ; grep TODO .", false},
		{"comment before newline then real search", "echo hi #note\ngrep -r TODO .", true},
		{"real search then trailing comment", "grep -r TODO . # done", true},
		{"quoted hash is data and bounds", "grep '#' internal/cli/skills.go", false},
		{"embedded hash is data", "grep foo#bar .", true},
	}
	runShellAdviseCases(t, cases)
}

func runShellAdviseCases(t *testing.T, cases []struct {
	name    string
	command string
	advise  bool
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"tool_name":"Bash","tool_input":{"command":` + mustJSON(t, tc.command) + `}}`
			var out bytes.Buffer
			if err := runSearchAdvisory(strings.NewReader(payload), &out); err != nil {
				t.Fatalf("runSearchAdvisory: %v", err)
			}
			emitted := strings.TrimSpace(out.String()) != ""
			if emitted != tc.advise {
				t.Errorf("advise=%v, want %v (output=%q)", emitted, tc.advise, out.String())
			}
		})
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSkillsYesAppliesNonInteractively proves --yes (assumeYes) applies the
// reviewed plan without a prompt even when no terminal is attached: the exact
// path an agent or a provisioning script uses. The manifest is committed and the
// assets land, unlike a plain non-interactive preview.
func TestSkillsYesAppliesNonInteractively(t *testing.T) {
	opts := skillsOpts(t)
	var out bytes.Buffer
	// interactive=false, assumeYes=true, and an empty reader: no prompt is read.
	if err := runSkills(opts, strings.NewReader(""), &out, false, true); err != nil {
		t.Fatalf("runSkills: %v", err)
	}
	if !manifestPresent(opts) {
		t.Error("--yes non-interactive run did not commit the ownership manifest")
	}
	asset := filepath.Join(opts.Home, ".claude", "skills", "prowl", "commands", "search.md")
	if _, err := os.Stat(asset); err != nil {
		t.Errorf("--yes non-interactive run did not install %s: %v", asset, err)
	}
	if !strings.Contains(strings.ToLower(out.String()), "installed") {
		t.Errorf("--yes run did not report the apply:\n%s", out.String())
	}
	if strings.Contains(strings.ToLower(out.String()), "preview only") {
		t.Errorf("--yes run fell through to the preview path:\n%s", out.String())
	}
}

// TestSplitClients proves the --clients tokenizer trims whitespace and drops
// empty entries, leaving normalization (unknown-name filtering, dedup) to the
// planner.
func TestSplitClients(t *testing.T) {
	got := splitClients(" claude , omp,hermes ,, ")
	want := []string{"claude", "omp", "hermes"}
	if len(got) != len(want) {
		t.Fatalf("splitClients = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitClients = %v, want %v", got, want)
		}
	}
}

// TestRenderRestartNamesEveryUserClient proves the reload/restart guidance covers
// every supported skill client -- Claude, OMP, Pi, Hermes, OpenClaw, and the
// retired Prowl Legacy harness -- so a detected client is never left without an
// actionable next step (Pi and OpenClaw previously produced no line at all).
func TestRenderRestartNamesEveryUserClient(t *testing.T) {
	var out bytes.Buffer
	renderRestart(&out, []string{
		setup.IntegrationClaude, setup.IntegrationOMP, setup.IntegrationPi,
		setup.IntegrationHermes, setup.IntegrationOpenClaw, setup.IntegrationProwl,
	})
	got := out.String()
	for client, want := range map[string]string{
		setup.IntegrationClaude:   "Restart Claude Code",
		setup.IntegrationOMP:      "Reload OMP",
		setup.IntegrationPi:       "Reload Pi",
		setup.IntegrationHermes:   "Reload Hermes",
		setup.IntegrationOpenClaw: "Reload OpenClaw",
		setup.IntegrationProwl:    "Reload Prowl Legacy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderRestart omitted guidance for %s (want %q):\n%s", client, want, got)
		}
	}
}
