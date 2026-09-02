package revieweval

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
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
	artifacts, report, check := validStructuredReviewArtifacts(t, base, head)
	plan := artifacts.Plan
	ctx := context.Background()
	store, err := review.OpenPlanStore(ctx, review.ExecGit{Binary: "git", Timeout: time.Second}, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close plan store: %v", err)
		}
	})
	lease, err := store.NewSnapshotLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Errorf("release snapshot lease: %v", err)
		}
	})
	result, err := store.Save(ctx, artifacts, lease)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewID != plan.ReviewID || result.Reused {
		t.Fatalf("save result = %#v", result)
	}
	if result.PruneWarning != nil {
		t.Fatalf("save prune warning: %v", result.PruneWarning)
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

func TestEvalReportBindingRequiresCanonicalVerifierEvidence(t *testing.T) {
	native := review.Finding{
		ID: "f_native", Category: "functional_correctness", Severity: "major", Confidence: "high",
		Summary: "wrong guard", Scenario: "empty input reaches storage", Verifier: "confirmed",
		Locations:        []review.Location{{Kind: string(review.LocationRange), Path: "a.go", Start: 9, End: 9}},
		VerifierEvidence: []review.Citation{{Kind: "hunk", ID: "h_1", Note: "guard remains reachable"}},
	}
	evidence, err := canonicalVerifierEvidence(native.VerifierEvidence)
	if err != nil {
		t.Fatal(err)
	}
	output := EvalOutput{Recommendation: "comment", Findings: []Finding{{
		HostID: native.ID, Category: native.Category, Severity: native.Severity, Confidence: native.Confidence,
		Summary: native.Summary, Scenario: native.Scenario, VerifierDisposition: native.Verifier,
		VerifierEvidence: evidence, Locations: []Location{{Path: "a.go", StartLine: 9, EndLine: 9}},
	}}}
	report := review.Report{Recommendation: "comment", Findings: []review.Finding{native}}
	if err := validateEvalReportBinding(output, mustJSON(t, report)); err != nil {
		t.Fatalf("canonical verifier evidence rejected: %v", err)
	}
	output.Findings[0].VerifierEvidence = "invented evidence"
	if err := validateEvalReportBinding(output, mustJSON(t, report)); err == nil {
		t.Fatal("invented verifier evidence accepted")
	}
}

func validStructuredReviewArtifacts(t *testing.T, baseSHA, headSHA string) (review.PlanArtifacts, review.Report, review.CheckResult) {
	t.Helper()
	baseBytes, err := hex.DecodeString(baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	headBytes, err := hex.DecodeString(headSHA)
	if err != nil {
		t.Fatal(err)
	}
	base := review.SideIdentity{Kind: review.SideGitOID, Value: baseBytes}
	head := review.SideIdentity{Kind: review.SideGitOID, Value: headBytes}

	const (
		pathName       = "a.go"
		cohortLabel    = "code"
		unitKind       = "normal"
		indexSignature = "review-eval-fixture-index-v1"
	)
	baseContent := []byte("package a\n")
	var headContent, patch strings.Builder
	headContent.Write(baseContent)
	patch.WriteString(" package a\n")
	for index := range 301 {
		fmt.Fprintf(&headContent, "var Value%03d = %d\n", index, index)
		fmt.Fprintf(&patch, "+var Value%03d = %d\n", index, index)
	}
	oldContentDigest := review.Digest(sha256.Sum256(baseContent))
	newContentDigest := review.Digest(sha256.Sum256([]byte(headContent.String())))
	rawHunk := review.RawHunk{
		Ordinal: 0, OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 302,
		Payload: []byte(patch.String()),
	}
	rawPath := review.RawPathRecord{
		Kind: "tracked", Status: "M", OldPath: pathName, NewPath: pathName,
		OldMode: 0o100644, NewMode: 0o100644, OldSide: base, NewSide: head,
		TextClass: string(review.TextClassText), Additions: 301,
		OldContentDigest: &oldContentDigest, NewContentDigest: &newContentDigest,
		Hunks: []review.RawHunk{rawHunk},
	}
	scopeDigest := review.ScopeDigest("sha1", base, head, review.ScopeRange, review.CanonicalPatchDigest([]review.RawPathRecord{rawPath}))

	pathCanonical := review.Frame(
		review.Field{Name: "scope", Value: scopeDigest[:]},
		review.Field{Name: "record", Value: rawPath.Frame()},
	)
	pathID := review.PathID(scopeDigest, rawPath)
	pathRecord := fixtureIDRecord(review.PathIDPrefixV1, pathID, pathCanonical)

	hunkCanonical := review.Frame(
		review.Field{Name: "scope", Value: scopeDigest[:]},
		review.Field{Name: "path", Value: pathID.Full[:]},
		review.Field{Name: "ordinal", Value: fixtureUint64(rawHunk.Ordinal)},
		review.Field{Name: "hunk", Value: rawHunk.Frame()},
	)
	hunkID := review.HunkID(scopeDigest, pathID.Full, rawHunk)
	hunkRecord := fixtureIDRecord(review.HunkIDPrefixV1, hunkID, hunkCanonical)

	unitIDs := []review.StableID{hunkID}
	unitCanonical := review.Frame(
		review.Field{Name: "kind", Value: []byte(unitKind)},
		review.Field{Name: "hunks", Value: fixtureStableIDList(unitIDs)},
	)
	unitID := review.UnitID(unitKind, unitIDs)
	unitRecord := fixtureIDRecord(review.UnitIDPrefixV1, unitID, unitCanonical)

	cohortUnits := []review.StableID{unitID}
	cohortCanonical := review.Frame(
		review.Field{Name: "scope", Value: scopeDigest[:]},
		review.Field{Name: "label", Value: []byte(cohortLabel)},
		review.Field{Name: "units", Value: fixtureStableIDList(cohortUnits)},
	)
	cohortID := review.CohortID(scopeDigest, cohortLabel, cohortUnits)
	cohortRecord := fixtureIDRecord(review.CohortIDPrefixV1, cohortID, cohortCanonical)

	layerCanonical := review.Frame(
		review.Field{Name: "cohort", Value: cohortID.Full[:]},
		review.Field{Name: "ordinal", Value: fixtureUint64(0)},
		review.Field{Name: "units", Value: fixtureStableIDList(cohortUnits)},
	)
	layerID := review.LayerID(cohortID, 0, cohortUnits)
	layerRecord := fixtureIDRecord(review.LayerIDPrefixV1, layerID, layerCanonical)

	identity := review.PlanIdentity{
		PlannerVersion: "1", ScopeDigest: scopeDigest, IndexSchema: "prowl.index.v1", IndexVersion: "1",
		HeadIndexSignature: []byte(indexSignature),
		Paths:              []review.PlanPathEntry{{PathID: pathID, ReviewClass: string(review.ReviewClassFull), Coverage: string(review.PathCoverageFull), RoleIDs: []string{review.RoleImplementation}}},
		Hunks:              []review.PlanHunkEntry{{HunkID: hunkID, Reviewability: string(review.HunkReviewable)}},
		Units:              []review.PlanUnitEntry{{UnitID: unitID, Kind: unitKind, HunkIDs: unitIDs}},
		Cohorts:            []review.PlanCohortEntry{{CohortID: cohortID, LayerID: layerID, UnitIDs: cohortUnits}},
		Constants: review.PlanConstants{
			StructuredThreshold:   review.StructuredThresholdV1,
			UnitLineCap:           uint64(review.MaxUnitChangedLinesV1),
			MandatoryJSONCap:      uint64(review.MaxUnitMandatoryJSONBytesV1),
			TextClassifierVersion: "text.v1",
			ContextRankerVersion:  "rank.v1",
		},
	}
	for _, auditID := range review.RequiredAuditsV1() {
		identity.Audits = append(identity.Audits, review.PlanAuditEntry{AuditID: auditID})
	}
	identityBytes := review.ReviewPlanIdentityV1(identity)
	planDigest := review.ReviewPlanDigest(identity)
	reviewID := review.ReviewID(identity).Public

	unit := review.Unit{
		Schema: review.UnitSchemaV1, ReviewID: reviewID, UnitID: unitID.Public, CohortID: cohortID.Public, LayerID: layerID.Public,
		ScopeKind: review.ScopeRange, ObjectFormat: "sha1", Base: base, Head: head,
		Hunks: []review.UnitHunk{{
			PathID: pathID.Public, OldPath: pathName, NewPath: pathName, Status: "M",
			Ordinal: 0, OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 302,
			PatchBase64: base64.StdEncoding.EncodeToString(rawHunk.Payload),
		}},
	}
	mandatoryUnit, err := unit.CanonicalMandatoryJSON()
	if err != nil {
		t.Fatal(err)
	}
	plan := review.Plan{
		Schema: review.PlanSchemaV1, ReviewID: reviewID, PlanDigest: hex.EncodeToString(planDigest[:]), Mode: review.ModeStructured, StructuredRequired: true,
		Scope:        review.Scope{Kind: review.ScopeRange, ObjectFormat: "sha1", Base: base, Head: head},
		Stats:        review.PlanStats{RawAdditions: 301, RawChurn: 301, ReviewableChurn: 301, ChangedPaths: 1},
		ChangedPaths: []review.PlanPath{{PathID: pathID.Public, OldPath: pathName, NewPath: pathName, Status: "M", ReviewClass: string(review.ReviewClassFull), Coverage: string(review.PathCoverageFull), Roles: []string{review.RoleImplementation}}},
		Cohorts:      []review.PlanCohort{{CohortID: cohortID.Public, Label: cohortLabel, Layers: []review.PlanLayer{{LayerID: layerID.Public, Ordinal: 0, UnitIDs: []string{unitID.Public}}}, UnitIDs: []string{unitID.Public}}},
		PrimaryUnits: []review.Unit{unit},
		NextCommands: []review.NextCommand{{Label: "check", Command: "prowl-agent review check"}},
	}
	report := review.Report{
		Schema: review.ReportSchemaV1, ReviewID: reviewID, PlanDigest: plan.PlanDigest, Base: base, Head: head, Recommendation: "approve",
		PrimaryReceipts: []review.PrimaryReceipt{{UnitID: unitID.Public, AcknowledgedPrimaryHunkIDs: []string{hunkID.Public}, Reviewer: "agent"}},
	}
	for _, auditID := range review.RequiredAuditsV1() {
		plan.RequiredAudits = append(plan.RequiredAudits, review.PlanAudit{AuditID: auditID})
		report.AuditReceipts = append(report.AuditReceipts, review.AuditReceipt{AuditID: auditID, Reviewer: "agent"})
	}
	check := review.CheckResult{Schema: review.CheckSchemaV1, ReviewID: reviewID, Mode: review.ModeStructured, Status: review.CheckComplete, Coverage: review.CoverageComplete, Recommendation: "approve"}
	return review.PlanArtifacts{
		Plan:           plan,
		UnitCandidates: map[string][]contextpacket.Candidate{unitID.Public: nil},
		MandatoryUnits: map[string][]byte{unitID.Public: mandatoryUnit},
		Citations: map[string]review.CitationProof{
			hunkID.Public: {ID: hunkID.Public, Side: review.SideHead, Path: pathName, ContentHash: hex.EncodeToString(newContentDigest[:]), Start: 1, End: 302},
		},
		PublishedIndexSignature:     indexSignature,
		PlanIdentityBytes:           identityBytes,
		PlanIdentity:                identity,
		IDRecords:                   []review.IDRecord{pathRecord, hunkRecord, unitRecord, cohortRecord, layerRecord},
		WorkspaceFingerprint:        "",
		WorkspaceCaptureFingerprint: "",
	}, report, check
}

func fixtureIDRecord(kind string, id review.StableID, canonical []byte) review.IDRecord {
	return review.IDRecord{Kind: kind, Public: id.Public, Full: id.Full, Canonical: canonical}
}

func fixtureUint64(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func fixtureStableIDList(ids []review.StableID) []byte {
	items := make([][]byte, len(ids))
	for index := range ids {
		items[index] = ids[index].Full[:]
	}
	return review.FrameList(items...)
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
	truth := func() []GroundTruth { return []GroundTruth{{ID: "bug"}} }
	cases := []Case{{ID: "begin", Metamorphic: true, GroundTruth: truth()}, {ID: "middle", Metamorphic: true, GroundTruth: truth()}, {ID: "end", Metamorphic: true, GroundTruth: truth()}}
	if err := ValidateMetamorphicTriples(cases, []MetamorphicTriple{triple}); err != nil {
		t.Fatal(err)
	}
	scores := []TrialScore{
		{CaseID: "begin", Condition: ConditionTreatment, Client: "c", Completed: true, MatchedGroundTruth: []string{groundTruthKey("begin", "bug")}},
		{CaseID: "middle", Condition: ConditionTreatment, Client: "c", Completed: false, MatchedGroundTruth: []string{groundTruthKey("middle", "bug")}},
		{CaseID: "end", Condition: ConditionTreatment, Client: "c", Completed: true, MatchedGroundTruth: []string{groundTruthKey("end", "bug")}},
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
