package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/review"
	"github.com/neur0map/prowl/internal/workspace"
	"github.com/spf13/cobra"
)

func TestReviewCommandsResolveAndValidateBeforeProjectOpen(t *testing.T) {
	root := newReviewTestRoot()
	paths := commandPaths(root)
	for path, flags := range map[string][]string{
		"review plan":  {"base", "head", "commit", "structured"},
		"review unit":  {"budget-tokens", "budget-bytes"},
		"review check": {"review", "report"},
	} {
		command := paths[path]
		if command == nil {
			t.Fatalf("command %q is not registered", path)
		}
		for _, flag := range flags {
			if command.Flags().Lookup(flag) == nil {
				t.Errorf("command %q missing --%s", path, flag)
			}
		}
	}

	t.Chdir(t.TempDir()) // no project: validation must win over project opening
	for name, args := range map[string][]string{
		"head without base":     {"review", "plan", "--head", "HEAD"},
		"commit with range":     {"review", "plan", "--commit", "HEAD", "--base", "main", "--head", "HEAD"},
		"malformed compound id": {"review", "unit", "bad/u_0123456789abcdef0123456789abcdef"},
		"zero token budget":     {"review", "unit", "rvw_0123456789abcdef0123456789abcdef01234567/u_0123456789abcdef0123456789abcdef", "--budget-tokens", "0"},
		"negative byte budget":  {"review", "unit", "rvw_0123456789abcdef0123456789abcdef01234567/u_0123456789abcdef0123456789abcdef", "--budget-bytes", "-1"},
		"missing check review":  {"review", "check", "--report", "-"},
		"missing check report":  {"review", "check", "--review", "rvw_0123456789abcdef0123456789abcdef01234567"},
	} {
		t.Run(name, func(t *testing.T) {
			command := newReviewTestRoot()
			command.SetArgs(args)
			command.SetIn(strings.NewReader("{}"))
			err := command.Execute()
			if err == nil {
				t.Fatalf("%v succeeded", args)
			}
			if strings.Contains(err.Error(), "workspace") {
				t.Fatalf("%v opened project before validation: %v", args, err)
			}
		})
	}
}
func TestReviewCheckValidatesFormatBeforeReadingReport(t *testing.T) {
	for name, flags := range map[string][]string{
		"unknown format":    {"--format", "invalid"},
		"conflicting flags": {"--json", "--format", "toon"},
	} {
		t.Run(name, func(t *testing.T) {
			input := &countingErrorReader{}
			command := newReviewTestRoot()
			command.SetIn(input)
			command.SetArgs(append([]string{
				"review", "check",
				"--review", "rvw_0123456789abcdef0123456789abcdef01234567",
				"--report", "-",
			}, flags...))
			err := command.Execute()
			if err == nil {
				t.Fatal("review check accepted invalid output format")
			}
			if input.reads != 0 {
				t.Fatalf("invalid format consumed stdin with %d read(s): %v", input.reads, err)
			}
			if strings.Contains(err.Error(), "stdin consumed") || strings.Contains(err.Error(), "decode report") {
				t.Fatalf("report error beat invalid format: %v", err)
			}
		})
	}
}

type countingErrorReader struct{ reads int }

func (reader *countingErrorReader) Read([]byte) (int, error) {
	reader.reads++
	return 0, errors.New("stdin consumed")
}

func TestReviewPlanUnitAndCheckCommands(t *testing.T) {
	root := reviewCLIFixture(t, 12)
	t.Chdir(root)

	planOutput, err := executeReview(t, nil, "review", "plan", "--structured", "--format", "json")
	if err != nil {
		t.Fatalf("review plan: %v", err)
	}
	var plan review.Plan
	if err := json.Unmarshal(planOutput, &plan); err != nil {
		t.Fatalf("decode plan: %v\n%s", err, planOutput)
	}
	if plan.Mode != review.ModeStructured || plan.StructuredRequired {
		t.Fatalf("plan mode/required = %s/%v", plan.Mode, plan.StructuredRequired)
	}
	if len(plan.PrimaryUnits) == 0 {
		t.Fatal("plan has no primary units")
	}
	wantCommands := append([]review.NextCommand(nil), plan.NextCommands...)
	assertReviewCommandsResolve(t, plan.NextCommands)

	unitID := plan.PrimaryUnits[0].UnitID
	unitOutput, err := executeReview(t, nil, "review", "unit", plan.ReviewID+"/"+unitID, "--budget-tokens", "256", "--budget-bytes", "4096", "--format", "json")
	if err != nil {
		t.Fatalf("review unit: %v", err)
	}
	var packet review.UnitPacket
	if err := json.Unmarshal(unitOutput, &packet); err != nil {
		t.Fatalf("decode unit packet: %v\n%s", err, unitOutput)
	}
	unit := packet.Mandatory
	if unit.ReviewID != plan.ReviewID || unit.UnitID != unitID || len(unit.Hunks) == 0 {
		t.Fatalf("unit identity/content = %+v", unit)
	}
	for _, hunk := range unit.Hunks {
		if _, err := base64.StdEncoding.DecodeString(hunk.PatchBase64); err != nil {
			t.Fatalf("unit hunk %s has invalid base64 patch: %v", hunk.PathID, err)
		}
	}
	assertReviewCommandsResolve(t, packet.NextCommands)

	report := review.Report{
		Schema: review.ReportSchemaV1, ReviewID: plan.ReviewID, PlanDigest: plan.PlanDigest,
		Base: plan.Scope.Base, Head: plan.Scope.Head,
		Recommendation: string(review.RecommendIncomplete), Findings: []review.Finding{},
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	checkOutput, err := executeReview(t, bytes.NewReader(reportJSON), "review", "check", "--review", plan.ReviewID, "--report", "-", "--format", "json")
	var checkErr *review.CheckError
	if !errors.As(err, &checkErr) {
		t.Fatalf("review check error = %v, want *review.CheckError", err)
	}
	var result review.CheckResult
	if decodeErr := json.Unmarshal(checkOutput, &result); decodeErr != nil {
		t.Fatalf("decode check result: %v\n%s", decodeErr, checkOutput)
	}
	if result.Status != review.CheckIncomplete || result.Complete() {
		t.Fatalf("check result = %+v", result)
	}

	// Formatting must never reorder or omit progressive-disclosure commands.
	for _, format := range []string{"toon", "human", "markdown"} {
		output, err := executeReview(t, nil, "review", "plan", "--structured", "--format", format)
		if err != nil {
			t.Fatalf("review plan --format %s: %v", format, err)
		}
		cursor := 0
		for _, next := range wantCommands {
			position := bytes.Index(output[cursor:], []byte(next.Command))
			if position < 0 {
				t.Fatalf("%s output omits next command %q\n%s", format, next.Command, output)
			}
			cursor += position + len(next.Command)
		}
	}
}

func TestReviewPlanCommitAndRangeScopes(t *testing.T) {
	root := reviewCLIFixture(t, 4)
	t.Chdir(root)
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("add", "sample.go")
	git("commit", "-qm", "change")

	baseOnlyRequest := review.PlanRequest{Base: "HEAD~1"}
	if err := baseOnlyRequest.Validate(); err != nil || baseOnlyRequest.Kind() != review.ScopeRange {
		t.Fatalf("base-only request = %s, %v; want valid range", baseOnlyRequest.Kind(), err)
	}

	cases := []struct {
		name string
		kind review.ScopeKind
		args []string
	}{
		{name: "commit", kind: review.ScopeCommit, args: []string{"review", "plan", "--commit", "HEAD", "--format", "json"}},
		{name: "range", kind: review.ScopeRange, args: []string{"review", "plan", "--base", "HEAD~1", "--head", "HEAD", "--format", "json"}},
		{name: "base-only", kind: review.ScopeRange, args: []string{"review", "plan", "--base", "HEAD~1", "--format", "json"}},
	}
	plans := make(map[string]review.Plan, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output, err := executeReview(t, nil, testCase.args...)
			if err != nil {
				t.Fatalf("%s plan: %v", testCase.name, err)
			}
			var plan review.Plan
			if err := json.Unmarshal(output, &plan); err != nil {
				t.Fatalf("decode %s plan: %v\n%s", testCase.name, err, output)
			}
			if plan.Scope.Kind != testCase.kind {
				t.Fatalf("%s scope kind = %s", testCase.name, plan.Scope.Kind)
			}
			plans[testCase.name] = plan
		})
	}
	explicit, baseOnly := plans["range"], plans["base-only"]
	if baseOnly.ReviewID != explicit.ReviewID || baseOnly.Scope.Head.Kind != explicit.Scope.Head.Kind ||
		!bytes.Equal(baseOnly.Scope.Head.Value, explicit.Scope.Head.Value) {
		t.Fatalf("base-only head = %+v, want explicit HEAD %+v", baseOnly.Scope.Head, explicit.Scope.Head)
	}
}

func TestReviewReportInputIsBoundedStrictAndCancellable(t *testing.T) {
	ctx := context.Background()
	valid := []byte(`{"schema":"review.report.v1"}`)
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := readReviewReport(ctx, path, nil)
	if err != nil || report.Schema != review.ReportSchemaV1 {
		t.Fatalf("read regular report = %+v, %v", report, err)
	}

	for name, contents := range map[string][]byte{
		"trailing object": append(append([]byte(nil), valid...), valid...),
		"unknown field":   []byte(`{"schema":"review.report.v1","unexpected":true}`),
		"null":            []byte(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeReviewReport(ctx, bytes.NewReader(contents)); err == nil {
				t.Fatal("invalid report input accepted")
			}
		})
	}

	over := bytes.Repeat([]byte{' '}, maxReviewReportBytes+1)
	if _, err := decodeReviewReport(ctx, bytes.NewReader(over)); !errors.Is(err, errReviewReportTooLarge) {
		t.Fatalf("oversized input error = %v, want errReviewReportTooLarge", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := decodeReviewReport(canceled, bytes.NewReader(valid)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled input error = %v", err)
	}

	symlink := filepath.Join(filepath.Dir(path), "report-link.json")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewReport(ctx, symlink, nil); err == nil {
		t.Fatal("symlink report input accepted")
	}
	directory := filepath.Join(filepath.Dir(path), "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewReport(ctx, directory, nil); err == nil {
		t.Fatal("directory report input accepted")
	}
	fifo := filepath.Join(filepath.Dir(path), "report.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReviewReport(ctx, fifo, nil); err == nil {
		t.Fatal("FIFO report input accepted")
	}
	if _, err := readReviewReport(ctx, "/dev/null", nil); err == nil {
		t.Fatal("device report input accepted")
	}
}

func assertReviewCommandsResolve(t *testing.T, next []review.NextCommand) {
	t.Helper()
	root := newReviewTestRoot()
	commands := commandPaths(root)
	for _, advertised := range next {
		invocation := strings.TrimSpace(strings.TrimPrefix(advertised.Command, "prowl "))
		path, remainder, ok := resolveInvocation(commands, invocation)
		if !ok {
			t.Errorf("advertised command does not resolve: %q", advertised.Command)
			continue
		}
		command := commands[path]
		for _, flag := range citedFlags(remainder) {
			name := strings.TrimPrefix(flag, "--")
			if command.Flags().Lookup(name) == nil && command.InheritedFlags().Lookup(name) == nil &&
				root.PersistentFlags().Lookup(name) == nil {
				t.Errorf("advertised command %q has unknown flag %s", advertised.Command, flag)
			}
		}
	}
}

func newReviewTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "prowl", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("format", "", "")
	root.PersistentFlags().Bool("json", false, "")
	root.AddCommand(newReviewCmd())
	return root
}

func executeReview(t *testing.T, input *bytes.Reader, args ...string) ([]byte, error) {
	t.Helper()
	root := newReviewTestRoot()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	if input != nil {
		root.SetIn(input)
	}
	root.SetArgs(args)
	err := root.Execute()
	return output.Bytes(), err
}

func reviewCLIFixture(t *testing.T, changedLines int) string {
	t.Helper()
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".prowl/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\n\nfunc Original() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	git("init", "-q")
	git("config", "user.email", "review@example.com")
	git("config", "user.name", "Review Test")
	git("add", ".gitignore", "sample.go")
	git("commit", "-qm", "base")

	var source strings.Builder
	source.WriteString("package sample\n\nfunc Original() int { return 1 }\n")
	for index := 0; index < changedLines; index++ {
		source.WriteString("// review line\n")
	}
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(source.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}
