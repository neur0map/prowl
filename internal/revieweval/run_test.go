package revieweval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prowl-agent/prowl-agent/internal/agenttrial"
	"github.com/prowl-agent/prowl-agent/internal/review"
)

func TestBuildOrderIsCompleteReproducibleAndBalanced(t *testing.T) {
	cases := []Case{{ID: "a"}, {ID: "b"}}
	first, err := BuildOrder(cases, []string{"claude", "omp"}, 3, 77)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildOrder(cases, []string{"claude", "omp"}, 3, 77)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced different order")
	}
	if len(first) != 24 {
		t.Fatalf("matrix length = %d", len(first))
	}
	seen := map[string]int{}
	for _, spec := range first {
		seen[spec.CaseID+"/"+spec.Client+"/"+spec.Condition]++
	}
	for key, count := range seen {
		if count != 3 {
			t.Fatalf("%s count = %d", key, count)
		}
	}
}

func TestCollectLoadsPrivateClaudePluginAndScoreDoesNotRerunTrials(t *testing.T) {
	prepared := t.TempDir()
	repository := filepath.Join(prepared, "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	baseSHA, headSHA := gitPair(t, repository, 301)
	skill := filepath.Join(t.TempDir(), "skill")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("review"), 0o644); err != nil {
		t.Fatal(err)
	}
	prowl := fakeVersionExecutable(t, "prowl 1")
	claude := fakeVersionExecutable(t, "claude 1")
	outputParent, err := os.MkdirTemp(".", "run-artifacts-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outputParent)
	output := filepath.Base(outputParent)
	caseInfo := Case{ID: "case", Repository: "repo", BaseSHA: baseSHA, HeadSHA: headSHA, RawAdditions: 301, DefectClass: "clean"}
	manifest := Manifest{Schema: ManifestSchema, Tuning: []Case{caseInfo}}
	scoring := tuningScoring(caseInfo.ID, []string{"claude"})
	seenRoots := map[string]bool{}
	launcherCalls := 0
	launcher := func(_ context.Context, work, _ string, config agenttrial.ClientConfig) (agenttrial.Result, error) {
		launcherCalls++
		if seenRoots[work] {
			t.Fatalf("worktree reused: %s", work)
		}
		seenRoots[work] = true
		treatment := len(config.PluginDirs) != 0
		eval := EvalOutput{Schema: EvalOutputSchema, Status: StatusCompleted, Recommendation: "approve", Findings: []Finding{}, Usage: agenttrial.Usage{ModelTokens: 5}}
		if treatment {
			if len(config.PluginDirs) != 1 || len(config.Skills) != 0 {
				t.Fatalf("Claude treatment config = plugins %v skills %v", config.PluginDirs, config.Skills)
			}
			plugin := config.PluginDirs[0]
			if !strings.HasPrefix(plugin, config.ConfigDir+string(filepath.Separator)) {
				t.Fatalf("plugin not private: %s outside %s", plugin, config.ConfigDir)
			}
			for _, path := range []string{filepath.Join(plugin, ".claude-plugin", "plugin.json"), filepath.Join(plugin, "skills", "prowl-pr-review", "SKILL.md")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("missing Claude plugin artifact %s: %v", path, err)
				}
			}
			if err := os.MkdirAll(filepath.Join(work, "artifacts"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"plan.json", "report.json", "check.json"} {
				if err := os.WriteFile(filepath.Join(work, "artifacts", name), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			eval.Checker = &CheckerArtifacts{ReviewID: "rvw_0123456789abcdef0123456789abcdef01234567", PlanPath: "artifacts/plan.json", ReportPath: "artifacts/report.json", CheckPath: "artifacts/check.json"}
		} else {
			if len(config.PluginDirs) != 0 || len(config.Skills) != 0 {
				t.Fatalf("Claude control received treatment assets: plugins %v skills %v", config.PluginDirs, config.Skills)
			}
			guard, err := os.ReadFile(filepath.Join(config.ConfigDir, "bin", "prowl-agent"))
			if err != nil || !strings.Contains(string(guard), "unavailable in the control condition") {
				t.Fatalf("control guard missing: %v %q", err, guard)
			}
		}
		data, err := json.Marshal(eval)
		if err != nil {
			t.Fatal(err)
		}
		return agenttrial.Result{Stdout: append(data, '\n'), Usage: eval.Usage, UsageReported: true, Elapsed: time.Millisecond}, nil
	}
	cfg := RunConfig{Clients: []string{"claude"}, Repetitions: 1, Set: "tuning", OutputDir: output, PreparedRoot: prepared, ProwlBinary: prowl, ClaudeBinary: claude, ReviewSkill: skill, Launcher: launcher}
	collection, err := Collect(context.Background(), cfg, manifest, scoring)
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Trials) != 2 || len(seenRoots) != 2 || launcherCalls != 2 {
		t.Fatalf("trials=%d roots=%d launches=%d", len(collection.Trials), len(seenRoots), launcherCalls)
	}
	var controlCompleted, forgedTreatmentRejected bool
	for _, record := range collection.Trials {
		if record.Condition == ConditionControl && record.Status == StatusCompleted {
			controlCompleted = true
		}
		if record.Condition == ConditionTreatment && record.Status == StatusFailed && record.Error != "" {
			forgedTreatmentRejected = true
		}
	}
	if !controlCompleted || !forgedTreatmentRejected {
		t.Fatalf("strict treatment evidence: control=%v forged-rejected=%v trials=%#v", controlCompleted, forgedTreatmentRejected, collection.Trials)
	}
	if _, err := os.Stat(filepath.Join(output, "collection.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "adjudication-input.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "report.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collection phase scored early: %v", err)
	}
	adjudication := Adjudication{Schema: AdjudicationSchema, CandidateDigest: collection.CandidateDigest, Frozen: true}
	tampered := collection
	tampered.Trials = append([]TrialRecord(nil), collection.Trials...)
	tampered.Trials[0].Condition = "tampered"
	if _, err := ScoreCollection(cfg, manifest, scoring, tampered, adjudication); err == nil {
		t.Fatal("scoring accepted modified retained trial records")
	}
	cfg.Launcher = func(context.Context, string, string, agenttrial.ClientConfig) (agenttrial.Result, error) {
		t.Fatal("scoring reran an agent")
		return agenttrial.Result{}, nil
	}
	if _, err := ScoreCollection(cfg, manifest, scoring, collection, adjudication); err != nil {
		t.Fatal(err)
	}
	if launcherCalls != 2 {
		t.Fatalf("scoring changed launch count to %d", launcherCalls)
	}
	if _, err := os.Stat(filepath.Join(output, "report.json")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareEvaluationRejectsRuntimeClientReordering(t *testing.T) {
	caseInfo := Case{ID: "case", Repository: "repo", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), RawAdditions: 301, DefectClass: "clean"}
	manifest := Manifest{Schema: ManifestSchema, Tuning: []Case{caseInfo}}
	scoring := tuningScoring(caseInfo.ID, []string{"claude", "omp"})
	_, _, _, err := prepareEvaluation(RunConfig{Clients: []string{"omp", "claude"}, Set: "tuning", OutputDir: "artifacts"}, manifest, scoring)
	if err == nil {
		t.Fatal("runtime client permutation accepted")
	}
}

func TestPreparedRepoPreflightRejectsDirtyWrongHeadAndChurnMismatch(t *testing.T) {
	prepared := t.TempDir()
	repository := filepath.Join(prepared, "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	base, head := gitPair(t, repository, 301)
	c := Case{ID: "case", Repository: "repo", BaseSHA: base, HeadSHA: head, RawAdditions: 301}
	if err := verifyPreparedCase(context.Background(), prepared, c); err != nil {
		t.Fatalf("clean pinned repository rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPreparedCase(context.Background(), prepared, c); err == nil {
		t.Fatal("untracked file accepted")
	}
	if err := os.Remove(filepath.Join(repository, "untracked.txt")); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "checkout", "-q", base)
	if err := verifyPreparedCase(context.Background(), prepared, c); err == nil {
		t.Fatal("wrong checked-out HEAD accepted")
	}
	gitCommand(t, repository, "checkout", "-q", head)
	mismatched := c
	mismatched.RawAdditions++
	if err := verifyPreparedCase(context.Background(), prepared, mismatched); err == nil {
		t.Fatal("manifest churn mismatch accepted")
	}
}

func TestPreparedRepoPreflightRejectsReviewChurnBoundary(t *testing.T) {
	prepared := t.TempDir()
	repository := filepath.Join(prepared, "repo")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	base, head := gitPair(t, repository, 300)
	c := Case{ID: "case", Repository: "repo", BaseSHA: base, HeadSHA: head, RawAdditions: 300}
	if err := verifyPreparedCase(context.Background(), prepared, c); err == nil {
		t.Fatal("native raw churn boundary accepted")
	}
}

func TestResolveExecutableUsesPathAndRecordsVersionAndHash(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "prowl-agent")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho 'prowl deterministic'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	identity, err := resolveExecutable(context.Background(), "prowl", "prowl-agent")
	if err != nil {
		t.Fatal(err)
	}
	wantPath, _ := filepath.Abs(executable)
	if identity.Path != wantPath || identity.Version != "prowl deterministic" || !sha256Hex.MatchString(identity.SHA256) {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestProductionToolchainPreflightRejectsVersionOrHashMismatch(t *testing.T) {
	claude := fakeVersionExecutable(t, "claude 1")
	omp := fakeVersionExecutable(t, "omp 1")
	prowl := fakeVersionExecutable(t, "prowl 1")
	cfg := RunConfig{Production: true, ClaudeBinary: claude, OMPBinary: omp, ProwlBinary: prowl}
	scoring := ScoringConfig{Clients: []string{"claude", "omp"}}
	for name, item := range map[string]struct {
		path string
		pin  *ExecutablePin
	}{
		"claude": {claude, &scoring.Executables.Claude},
		"omp":    {omp, &scoring.Executables.OMP},
		"prowl":  {prowl, &scoring.Executables.Prowl},
	} {
		identity, err := resolveExecutable(context.Background(), name, item.path)
		if err != nil {
			t.Fatal(err)
		}
		*item.pin = ExecutablePin{Version: identity.Version, SHA256: identity.SHA256}
	}
	scoring.Executables.Prowl.SHA256 = strings.Repeat("0", 64)
	if _, err := preflightToolchain(context.Background(), &cfg, scoring); err == nil {
		t.Fatal("production accepted mismatched Prowl executable hash")
	}
}

func TestArtifactPathsDoNotFollowSymlinksAndLineBoundsAreExact(t *testing.T) {
	external := t.TempDir()
	name := filepath.Join(".", "output-link-test")
	_ = os.Remove(name)
	if err := os.Symlink(external, name); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(name)
	if _, err := prepareOutputRoot(filepath.Join("output-link-test", "child")); err == nil {
		t.Fatal("output symlink accepted")
	}

	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "check.json")
	if err := os.WriteFile(outside, []byte(`{"status":"complete"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "check.json")); err != nil {
		t.Fatal(err)
	}
	if data := readRelativeArtifact(root, "check.json", 1024); data != nil {
		t.Fatal("checker artifact symlink followed")
	}

	file := filepath.Join(root, "lines.txt")
	if err := os.WriteFile(file, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(handle, 1) {
		t.Fatal("existing first line did not resolve")
	}
	_ = handle.Close()
	handle, err = os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	if hasLine(handle, 2) {
		t.Fatal("phantom line after terminal newline resolved")
	}
	_ = handle.Close()
}

func TestCheckerArtifactsRequireStrictBoundProwlEvidence(t *testing.T) {
	root := t.TempDir()
	gitCommand(t, root, "init", "-q")
	artifactRoot := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifactRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	plan, report, check, identityBytes := validStructuredReviewArtifacts(t, base, head)
	store, err := review.OpenPlanStore(context.Background(), review.ExecGit{Binary: "git", Timeout: time.Second}, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), review.PlanArtifacts{Plan: plan, PlanIdentityBytes: identityBytes}, ""); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"plan.json": plan, "report.json": report, "check.json": check} {
		if err := writeJSON(filepath.Join(artifactRoot, name), value); err != nil {
			t.Fatal(err)
		}
	}
	prowl := filepath.Join(t.TempDir(), "prowl-agent")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'prowl 1'; exit 0; fi\ncat artifacts/check.json\n"
	if err := os.WriteFile(prowl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prowlSHA, err := fileSHA256(prowl)
	if err != nil {
		t.Fatal(err)
	}
	refs := CheckerArtifacts{ReviewID: plan.ReviewID, PlanPath: "artifacts/plan.json", ReportPath: "artifacts/report.json", CheckPath: "artifacts/check.json"}
	evidence, _, _, _, err := validateCheckerArtifacts(root, refs, Case{BaseSHA: base, HeadSHA: head, RawAdditions: 301}, 1<<20, prowl, prowlSHA)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "complete" || evidence.PrimaryRangesCovered != 1 || evidence.PrimaryRangesTotal != 1 || evidence.AuditTargetsCovered != 0 || evidence.AuditTargetsTotal != 0 {
		t.Fatalf("derived checker evidence = %#v", evidence)
	}
	if err := validateEvalReportBinding(EvalOutput{Recommendation: "comment"}, mustJSON(t, report)); err == nil {
		t.Fatal("eval output recommendation diverged from canonical report")
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "check.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := validateCheckerArtifacts(root, refs, Case{BaseSHA: base, HeadSHA: head, RawAdditions: 301}, 1<<20, prowl, prowlSHA); err == nil {
		t.Fatal("minimal forged checker result accepted")
	}
}

func validStructuredReviewArtifacts(t *testing.T, baseSHA, headSHA string) (review.Plan, review.Report, review.CheckResult, []byte) {
	t.Helper()
	baseBytes, err := hex.DecodeString(baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	headBytes, err := hex.DecodeString(headSHA)
	if err != nil {
		t.Fatal(err)
	}
	identityBytes := []byte("review-eval-persisted-plan-fixture")
	sum := sha256.Sum256(identityBytes)
	digest := hex.EncodeToString(sum[:])
	reviewID := "rvw_" + hex.EncodeToString(sum[:20])
	base := review.SideIdentity{Kind: review.SideGitOID, Value: baseBytes}
	head := review.SideIdentity{Kind: review.SideGitOID, Value: headBytes}
	unit := review.Unit{
		Schema: review.UnitSchemaV1, ReviewID: reviewID, UnitID: "u_1", CohortID: "c_1", LayerID: "l_1",
		ScopeKind: review.ScopeRange, ObjectFormat: "sha1", Base: base, Head: head,
		Hunks: []review.UnitHunk{{PathID: "p_1", OldPath: "a.go", NewPath: "a.go", Status: "M", Ordinal: 1, OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 302, PatchBase64: "eA=="}},
	}
	plan := review.Plan{
		Schema: review.PlanSchemaV1, ReviewID: reviewID, PlanDigest: digest, Mode: review.ModeStructured, StructuredRequired: true,
		Scope:        review.Scope{Kind: review.ScopeRange, ObjectFormat: "sha1", Base: base, Head: head},
		Stats:        review.PlanStats{RawAdditions: 301, RawChurn: 301, ReviewableChurn: 301, ChangedPaths: 1},
		ChangedPaths: []review.PlanPath{{PathID: "p_1", OldPath: "a.go", NewPath: "a.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		Cohorts:      []review.PlanCohort{{CohortID: "c_1", Label: "code", Layers: []review.PlanLayer{{LayerID: "l_1", Ordinal: 1, UnitIDs: []string{"u_1"}}}, UnitIDs: []string{"u_1"}}},
		PrimaryUnits: []review.Unit{unit},
		NextCommands: []review.NextCommand{{Label: "check", Command: "prowl-agent review check"}},
	}
	report := review.Report{
		Schema: review.ReportSchemaV1, ReviewID: reviewID, PlanDigest: digest, Base: base, Head: head, Recommendation: "approve",
		PrimaryReceipts: []review.PrimaryReceipt{{UnitID: "u_1", AcknowledgedPrimaryHunkIDs: []string{"h_1"}, Reviewer: "agent"}},
	}
	for _, auditID := range review.RequiredAuditsV1() {
		plan.RequiredAudits = append(plan.RequiredAudits, review.PlanAudit{AuditID: auditID})
		report.AuditReceipts = append(report.AuditReceipts, review.AuditReceipt{AuditID: auditID, Reviewer: "agent"})
	}
	check := review.CheckResult{Schema: review.CheckSchemaV1, ReviewID: reviewID, Mode: review.ModeStructured, Status: review.CheckComplete, Coverage: review.CoverageComplete, Recommendation: "approve"}
	return plan, report, check, identityBytes
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBootstrapIsExactAndProductionRequiresTenThousand(t *testing.T) {
	scores := []TrialScore{
		{CaseID: "a", Condition: ConditionControl, TP: 0, FN: 1}, {CaseID: "a", Condition: ConditionTreatment, TP: 1},
		{CaseID: "b", Condition: ConditionControl, TP: 1}, {CaseID: "b", Condition: ConditionTreatment, TP: 1},
	}
	one, err := Bootstrap(scores, 99, 64, false)
	if err != nil {
		t.Fatal(err)
	}
	if one.Lower != 0 || one.Upper != 1 || one.Replicates != 64 || one.Seed != 99 {
		t.Fatalf("bootstrap vector = %#v", one)
	}
	two, err := Bootstrap(scores, 99, 64, false)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("bootstrap not deterministic: %#v %#v", one, two)
	}
	blocked := []TrialScore{
		{CaseID: "only", Condition: ConditionControl, Repetition: 1, TP: 1},
		{CaseID: "only", Condition: ConditionControl, Repetition: 2, FN: 1},
		{CaseID: "only", Condition: ConditionTreatment, Repetition: 1, TP: 1},
		{CaseID: "only", Condition: ConditionTreatment, Repetition: 2, TP: 1},
	}
	blockInterval, err := Bootstrap(blocked, 1, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0.33333333333333337; blockInterval.Lower != want || blockInterval.Upper != want {
		t.Fatalf("repetitions left case block: %#v", blockInterval)
	}
	if _, err := Bootstrap(scores, 99, 9999, true); err == nil {
		t.Fatal("production accepted non-frozen replicate count")
	}
}

func TestMetamorphicValidationAndFailedVariantScoring(t *testing.T) {
	triple := MetamorphicTriple{ID: "triple", GroundTruthID: "bug", BeginningCaseID: "begin", MiddleCaseID: "middle", EndCaseID: "end"}
	truth := func(caseID string) []GroundTruth { return []GroundTruth{{ID: tripleGroundTruthID(caseID, "bug")}} }
	cases := []Case{{ID: "begin", Metamorphic: true, GroundTruth: truth("begin")}, {ID: "middle", Metamorphic: true, GroundTruth: truth("middle")}, {ID: "end", Metamorphic: true, GroundTruth: truth("end")}}
	if err := ValidateMetamorphicTriples(cases, []MetamorphicTriple{triple}); err != nil {
		t.Fatal(err)
	}
	scores := []TrialScore{
		{CaseID: "begin", Condition: ConditionTreatment, Client: "c", Completed: true, MatchedGroundTruth: []string{tripleGroundTruthID("begin", "bug")}},
		{CaseID: "middle", Condition: ConditionTreatment, Client: "c", Completed: false, MatchedGroundTruth: []string{tripleGroundTruthID("middle", "bug")}},
		{CaseID: "end", Condition: ConditionTreatment, Client: "c", Completed: true, MatchedGroundTruth: []string{tripleGroundTruthID("end", "bug")}},
	}
	got := ScoreMetamorphic(scores, []MetamorphicTriple{triple})
	if got[ConditionTreatment].Beginning != 1 || got[ConditionTreatment].Middle != 0 || got[ConditionTreatment].End != 1 || got[ConditionTreatment].Sensitivity != 1 {
		t.Fatalf("score = %#v", got)
	}
}

func gitPair(t *testing.T, repository string, additions int) (string, string) {
	t.Helper()
	gitCommand(t, repository, "init", "-q")
	if err := os.WriteFile(filepath.Join(repository, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "-c", "user.name=Review Eval", "-c", "user.email=review@example.invalid", "commit", "-q", "-m", "base")
	base := gitCommand(t, repository, "rev-parse", "HEAD")
	var content strings.Builder
	content.WriteString("package a\n")
	for index := range additions {
		fmt.Fprintf(&content, "var Value%03d = %d\n", index, index)
	}
	if err := os.WriteFile(filepath.Join(repository, "a.go"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "-c", "user.name=Review Eval", "-c", "user.email=review@example.invalid", "commit", "-q", "-m", "head")
	return base, gitCommand(t, repository, "rev-parse", "HEAD")
}

func gitCommand(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func fakeVersionExecutable(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client")
	body := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '%s\\n' " + fmt.Sprintf("%q", version) + "; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func tuningScoring(caseID string, clients []string) ScoringConfig {
	return ScoringConfig{
		Schema: ScoringConfigSchema, BaseCaseIDs: []string{caseID}, Clients: clients, Model: "fake",
		Budget:         agenttrial.Budget{MaxModelTokens: 100, MaxToolCalls: 10, MaxSubagents: 2, Timeout: time.Second},
		MaxOutputBytes: 4096, Repetitions: 1, BootstrapReplicates: 8,
		MatchingCriteria: "frozen", BlindAdjudication: "blind", AcceptedDispositions: []string{"confirmed", "plausible"}, FailureScoring: "frozen", Seed: 42,
	}
}
