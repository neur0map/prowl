package agenteval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromptManifestIsBlindAndComplete(t *testing.T) {
	manifest, err := LoadManifest(filepath.Join("..", "..", "testdata", "agent-adoption", "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Tuning) == 0 || len(manifest.HeldOut) < 8 {
		t.Fatalf("prompt bank too small: tuning=%d held=%d", len(manifest.Tuning), len(manifest.HeldOut))
	}
	for _, c := range append(append([]Case{}, manifest.Tuning...), manifest.HeldOut...) {
		if forbiddenPrompt.MatchString(c.Prompt) {
			t.Errorf("prompt %s leaks routing/evaluation language: %q", c.ID, c.Prompt)
		}
	}
}

func TestParseClaudeStreamExtractsEvidence(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"prowl-agent find Register --format human"}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"internal/cli/cli.go","offset":1,"limit":30}}]}}`,
		`{"type":"result","duration_ms":25,"result":"Register is in internal/cli/cli.go:10"}`,
	}, "\n")
	parsed := ParseStream("claude", []byte(stream), nil, 10*time.Millisecond)
	if len(parsed.Tools) != 2 || !parsed.Tools[0].Prowl || parsed.Tools[1].Broad {
		t.Fatalf("tools: %+v", parsed.Tools)
	}
	if parsed.FirstDiscovery != "Bash" || !strings.Contains(parsed.FinalAnswer, "Register") {
		t.Fatalf("parsed: %+v", parsed)
	}
	if parsed.ElapsedMS != 25 || parsed.ToolCount != 2 || len(parsed.Citations) == 0 {
		t.Fatalf("totals/citations: %+v", parsed)
	}
}

func TestParseOMPAndMalformedEvidence(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_end","toolCall":{"type":"toolCall","id":"call-1","name":"grep","arguments":{"pattern":"Register","path":"."}}}}`,
		`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"grep","args":{"pattern":"Register","path":"."}}`,
		`not-json`,
		`{"type":"agent_end","message":{"role":"assistant","content":"Register lives in internal/cli/cli.go:10"}}`,
	}, "\n")
	parsed := ParseStream("omp", []byte(stream), []byte("warning"), 5*time.Millisecond)
	if len(parsed.Tools) != 1 || !parsed.Tools[0].Broad {
		t.Fatalf("tools: %+v", parsed.Tools)
	}
	if len(parsed.Malformed) != 1 || !strings.Contains(parsed.RawStdout, "not-json") || parsed.RawStderr != "warning" {
		t.Fatalf("raw/malformed lost: %+v", parsed)
	}
	caseDef := Case{ID: "x", Class: "structural", ExpectedPaths: []string{"internal/cli/cli.go"}, ExpectedSymbols: []string{"Register"}}
	if trial := Score(caseDef, "omp", "treatment", 1, parsed, nil); trial.Correct {
		t.Fatal("malformed stream counted as correct")
	}
}

func TestParseOMPUsesCompletedArgumentsForDuplicateCall(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"message_start","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{}}]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_end","toolCall":{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"prowl-agent search harness"}}}}`,
		`{"type":"agent_end","message":{"role":"assistant","content":"internal/setup/harness.go DetectInstalledHarnesses"}}`,
	}, "\n")
	parsed := ParseStream("omp", []byte(stream), nil, time.Millisecond)
	if len(parsed.Tools) != 1 || !parsed.Tools[0].Prowl {
		t.Fatalf("completed tool arguments not retained: %+v", parsed.Tools)
	}
}

func TestClassifiersKeepBoundedAndControlOperationsNative(t *testing.T) {
	calls := []ToolCall{
		{Name: "Grep", Input: json.RawMessage(`{"pattern":"x","path":"internal/cli"}`)},
		{Name: "Glob", Input: json.RawMessage(`{"pattern":"*.go","path":"internal/cli"}`)},
		{Name: "Read", Input: json.RawMessage(`{"file_path":"internal/cli/cli.go","offset":1,"limit":20}`)},
		{Name: "Bash", Input: json.RawMessage(`{"command":"rg x internal/cli/cli.go"}`)},
		{Name: "Bash", Input: json.RawMessage(`{"command":"printf prowl-agent"}`)},
	}
	for i := range calls {
		classifyTool(&calls[i])
		if calls[i].Broad || calls[i].Prowl {
			t.Errorf("bounded/control call misclassified: %+v", calls[i])
		}
	}
	broad := ToolCall{Name: "Glob", Input: json.RawMessage(`{"pattern":"**/*.go"}`)}
	classifyTool(&broad)
	if !broad.Broad {
		t.Fatal("repository-wide glob not classified broad")
	}

	flagFiltered := ToolCall{Name: "Bash", Input: json.RawMessage(`{"command":"rg -g '*.go' Register"}`)}
	classifyTool(&flagFiltered)
	if !flagFiltered.Broad {
		t.Fatal("repository-wide rg with a glob filter not classified broad")
	}
}

func TestScoreRequiresGroundTruthAndProwlBeforeBroadSearch(t *testing.T) {
	caseDef := Case{ID: "s", Class: "structural", ExpectedPaths: []string{"internal/cli/cli.go"}, ExpectedSymbols: []string{"Register"}}
	parsed := ParsedTrial{FinalAnswer: "Register internal/cli/cli.go:10", Tools: []ToolCall{{Index: 0, Name: "Bash", Prowl: true}, {Index: 1, Name: "Grep", Broad: true}}}
	trial := Score(caseDef, "claude", "treatment", 1, parsed, nil)
	if !trial.Correct || !trial.CitationQuality || !trial.ProwlFirst {
		t.Fatalf("score: %+v", trial)
	}
	controlCase := Case{ID: "c", Class: "known_file", ExpectedPaths: []string{"internal/cli/cli.go"}}
	control := Score(controlCase, "claude", "treatment", 1, ParsedTrial{FinalAnswer: "internal/cli/cli.go", Tools: []ToolCall{{Index: 0, Name: "Read"}}}, nil)
	if !control.NativeControl {
		t.Fatalf("native control failed: %+v", control)
	}
}

func TestMedianAndGateBoundaries(t *testing.T) {
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Fatalf("even median=%v", got)
	}
	if got := median([]float64{4, 1, 3}); got != 3 {
		t.Fatalf("odd median=%v", got)
	}
	trials := gateFixture(4, 1)
	report := BuildReport(trials)
	if report.Gates.ProwlFirstRate != .8 || report.Gates.BroadSearchReduction != .5 || !report.Gates.Passed {
		t.Fatalf("boundary gates: %+v", report.Gates)
	}
	belowRate := BuildReport(gateFixture(3, 1))
	if belowRate.Gates.Passed {
		t.Fatalf("below 80%% passed: %+v", belowRate.Gates)
	}
	belowReduction := BuildReport(gateFixture(4, 2))
	if belowReduction.Gates.Passed {
		t.Fatalf("below 50%% reduction passed: %+v", belowReduction.Gates)
	}
}

func gateFixture(prowlFirst, treatmentBroad int) []Trial {
	var trials []Trial
	for i := range 5 {
		controlTools := []ToolCall{{Broad: true}, {Broad: true}}
		treatmentTools := make([]ToolCall, treatmentBroad)
		for j := range treatmentTools {
			treatmentTools[j].Broad = true
		}
		trials = append(trials,
			Trial{CaseID: "s", Class: "structural", Condition: "control", Correct: true, Parsed: ParsedTrial{Tools: controlTools}},
			Trial{CaseID: "s", Class: "structural", Condition: "treatment", Correct: true, ProwlFirst: i < prowlFirst, Parsed: ParsedTrial{Tools: treatmentTools}},
		)
	}
	for _, class := range []string{"literal", "filename", "known_file"} {
		trials = append(trials,
			Trial{CaseID: class, Class: class, Condition: "control", Correct: true, NativeControl: true},
			Trial{CaseID: class, Class: class, Condition: "treatment", Correct: true, NativeControl: true},
		)
	}
	return trials
}

func TestFixtureCopyAndRoutingRemoval(t *testing.T) {
	source := t.TempDir()
	for _, rel := range []string{"keep.go", "AGENTS.md", ".mcp.json", ".claude/skill", ".omp/rule", ".agents/a", ".prowl/index.db", ".git/config"} {
		path := filepath.Join(source, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "copy")
	if err := copyFixture(source, destination, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "keep.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".git")); !os.IsNotExist(err) {
		t.Fatal("copied .git")
	}
	removeRouting(destination)
	for _, rel := range []string{"AGENTS.md", ".mcp.json", ".claude", ".omp", ".agents"} {
		if _, err := os.Stat(filepath.Join(destination, rel)); !os.IsNotExist(err) {
			t.Fatalf("routing survived: %s", rel)
		}
	}
}

func TestRunRejectsNonLocalOutputBeforeLaunching(t *testing.T) {
	_, err := Run(t.Context(), Config{OutputDir: filepath.Join(string(filepath.Separator), "tmp", "outside")}, Manifest{})
	if err == nil {
		t.Fatal("absolute output accepted")
	}
}

func TestManifestRejectsUnsafeArtifactCaseID(t *testing.T) {
	manifest, err := LoadManifest(filepath.Join("..", "..", "testdata", "agent-adoption", "prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest.Tuning[0].ID = "../escape"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("path-traversing case ID accepted")
	}
}

func TestClientAndSourceValidation(t *testing.T) {
	if err := validateClients([]string{"claude", "../outside"}); err == nil {
		t.Fatal("unknown/path-traversing client accepted")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "setup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := caseWorkDir(root, "../outside"); err == nil {
		t.Fatal("source escape accepted")
	}
	got, err := caseWorkDir(root, "internal/setup")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "internal", "setup") {
		t.Fatalf("source dir=%q", got)
	}
}

func TestPreparedFixtureCopiesAreIndependent(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, ".prowl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "source.go"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".prowl", "index.db"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if err := copyPreparedFixture(source, first); err != nil {
		t.Fatal(err)
	}
	if err := copyPreparedFixture(source, second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "source.go"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(second, "source.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("second trial contaminated: %q", data)
	}
}

func TestProwlClassifierAcceptsWindowsExecutable(t *testing.T) {
	for _, command := range []string{`"C:\Tools\prowl-agent.exe" find Register`, `"C:\Program Files\prowl-agent.exe" find Register`} {
		input, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		call := ToolCall{Name: "Bash", Input: input}
		classifyTool(&call)
		if !call.Prowl {
			t.Errorf("Windows Prowl invocation not classified: %s", command)
		}
	}
}

func TestResolveFixtureUsesManifestUnlessOverridden(t *testing.T) {
	if got := resolveFixture("", "manifest-repo"); got != "manifest-repo" {
		t.Fatalf("manifest fixture ignored: %q", got)
	}
	if got := resolveFixture("override", "manifest-repo"); got != "override" {
		t.Fatalf("fixture override ignored: %q", got)
	}
}

func TestClaudeArgsPreapproveReadOnlyDiscoveryTools(t *testing.T) {
	args := claudeArgs(Config{Model: "model"}, "control", "/home/test", "question")
	joined := strings.Join(args, " ")
	for _, required := range []string{"--allowedTools Bash,Read,Grep,Glob,Agent,Skill", "--disallowedTools Edit,Write,Notebook", "--no-session-persistence"} {
		if !strings.Contains(joined, required) {
			t.Errorf("Claude args omit %q: %v", required, args)
		}
	}
	for _, forbidden := range []string{"--system-prompt", "--append-system-prompt"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("Claude args contain prompt override %q: %v", forbidden, args)
		}
	}
}

func TestRunClientPreservesOMPStreamAndArguments(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	binary := filepath.Join(t.TempDir(), "omp")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > \"$AGENTEVAL_ARGS\"\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"ok\"}'\nprintf 'warning' >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTEVAL_ARGS", argsFile)
	stdout, stderr, _, err := runClient(context.Background(), Config{Model: "fixed", OMPBinary: binary, Timeout: time.Second}, t.TempDir(), t.TempDir(), "omp", "control", "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(stdout), "{\"type\":\"result\",\"result\":\"ok\"}\n"; got != want {
		t.Fatalf("stdout = %q", got)
	}
	if got, want := string(stderr), "warning"; got != want {
		t.Fatalf("stderr = %q", got)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "-p --mode json --no-session --no-title --tools read,bash,grep,glob,lsp --model fixed --no-skills --no-extensions --no-rules prompt"
	if string(args) != wantArgs {
		t.Fatalf("args = %q", args)
	}
}
