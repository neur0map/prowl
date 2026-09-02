package revieweval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchSourcePinsCacheAndDetectsTamper(t *testing.T) {
	payload := []byte(`{"ok":true}`)
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	source := SourceSpec{ID: "fixture", URL: server.URL + "/data.json", Path: "fixture.json", SHA256: hex.EncodeToString(digest[:]), License: "MIT", Parser: "json", Revision: strings.Repeat("a", 40), MaxBytes: int64(len(payload))}
	cache := t.TempDir()
	path, err := fetchSourceWithClient(context.Background(), source, cache, false, server.Client(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(payload) {
		t.Fatalf("cached payload = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchSourceWithClient(context.Background(), source, cache, true, server.Client(), false); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("offline tamper error = %v", err)
	}
}

func TestFetchSourceRejectsBoundsAndUnsupportedRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("oversized"))
	}))
	defer server.Close()
	base := SourceSpec{ID: "fixture", Path: "fixture.json", SHA256: strings.Repeat("0", 64), License: "MIT", Parser: "json", Revision: strings.Repeat("a", 40), MaxBytes: 4}
	base.URL = server.URL + "/data"
	if _, err := fetchSourceWithClient(context.Background(), base, t.TempDir(), false, server.Client(), false); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("bound error = %v", err)
	}
	base.URL = server.URL + "/redirect"
	client := server.Client()
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return validateSourceURL(base, req.URL)
	}
	if _, err := fetchSourceWithClient(context.Background(), base, t.TempDir(), false, client, false); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestValidateSourcesRequiresImmutableCommitsAndSafePayloads(t *testing.T) {
	valid := SourcesManifest{Schema: SourcesSchema, Sources: []SourceSpec{{ID: "a", URL: "https://example.com/a.json", Path: "a.json", SHA256: strings.Repeat("a", 64), License: "MIT", Parser: "json", Revision: strings.Repeat("b", 40), MaxBytes: 100}}}
	if err := ValidateSources(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Sources = append([]SourceSpec(nil), valid.Sources...)
	invalid.Sources[0].Revision = "main"
	if err := ValidateSources(invalid); err == nil {
		t.Fatal("mutable revision accepted")
	}
	invalid = valid
	invalid.Sources = append([]SourceSpec(nil), valid.Sources...)
	invalid.Sources[0].Path = "../a.json"
	if err := ValidateSources(invalid); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestRawChurnMatchesNativeLineSemantics(t *testing.T) {
	cases := []struct {
		name       string
		base, head []byte
		add, del   int
	}{
		{"empty-add", nil, nil, 0, 0},
		{"final-no-newline", nil, []byte("a\nb"), 2, 0},
		{"crlf", []byte("a\r\n"), []byte("b\r\n"), 1, 1},
		{"deletion", []byte("a\nb\n"), []byte("a\n"), 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			add, del, err := RawTextChurn(tc.base, tc.head)
			if err != nil {
				t.Fatal(err)
			}
			if add != tc.add || del != tc.del {
				t.Fatalf("churn = %d/%d, want %d/%d", add, del, tc.add, tc.del)
			}
		})
	}
}

func TestComputeGitChurnUsesImmutableRawBlobs(t *testing.T) {
	repository := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q")
	run("config", "user.name", "Review Eval Test")
	run("config", "user.email", "review-eval@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "binary.bin"), []byte{0, 'a', '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "fixture.go", "binary.bin")
	run("commit", "-qm", "base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("a\nc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repository, "binary.bin")); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "head")
	head := run("rev-parse", "HEAD")
	additions, deletions, ranges, err := ComputeGitChurn(context.Background(), repository, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if additions != 1 || deletions != 1 || len(ranges) != 1 || ranges[0] != (ChangedRange{Path: "fixture.go", StartLine: 2, EndLine: 2}) {
		t.Fatalf("git churn = %d/%d %#v", additions, deletions, ranges)
	}
}

func TestCorpusValidationRejectsCoverageDuplicatesAndLocations(t *testing.T) {
	cases := completeLargeCases()
	if err := ValidateLargeCorpus(cases, 8, false); err != nil {
		t.Fatal(err)
	}
	duplicate := append([]PreparedCase(nil), cases...)
	duplicate[1].Repository, duplicate[1].BaseSHA, duplicate[1].HeadSHA = duplicate[0].Repository, duplicate[0].BaseSHA, duplicate[0].HeadSHA
	duplicate[1].BaseFetch, duplicate[1].HeadFetch = duplicate[0].BaseFetch, duplicate[0].HeadFetch
	if err := ValidateLargeCorpus(duplicate, 8, false); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("duplicate error = %v", err)
	}
	bad := append([]PreparedCase(nil), cases...)
	bad[0].GroundTruth[0].Locations[0].StartLine = 100
	bad[0].GroundTruth[0].Locations[0].EndLine = 100
	bad[0].ChangedRanges[0].EndLine = 20
	if err := ValidateLargeCorpus(bad, 8, false); err == nil || !strings.Contains(err.Error(), "changed range") {
		t.Fatalf("location error = %v", err)
	}
}

func TestSelectionIsDeterministicAndScoreIndependent(t *testing.T) {
	cases := completeLargeCases()
	first, err := SelectCases(cases, len(cases))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		cases[i].SourceRecordID = "changed-non-identity-metadata"
	}
	second, err := SelectCases(cases, len(cases))
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("selection changed at %d: %s != %s", i, first[i].ID, second[i].ID)
		}
	}
}

func TestSmallCorpusSeparationBoundaryAndDirectness(t *testing.T) {
	small := make([]PreparedCase, 0, 40)
	for i := 0; i < 40; i++ {
		label := SmallCleanPR
		category := "clean"
		sourceType := SourceClean
		truth := []GroundTruth(nil)
		clean := &CleanProvenance{Method: "official_swr_clean_label", EvidenceRefs: []string{"swr:test-fixture"}}
		if i < 20 {
			label, category, sourceType, clean = SmallChangePR, "local_logic", SourceHumanCaught, nil
			truth = []GroundTruth{canonicalGroundTruth(caseID(i)+"-truth", caseID(i))}
		}
		base, head := shaFor(i), shaFor(i+100)
		small = append(small, PreparedCase{
			ID: caseID(i), SourceID: "swr", SourceRecordID: caseID(i), Repository: "owner/repo",
			BaseSHA: base, BaseFetch: testFetch("owner/repo", base), HeadSHA: head, HeadFetch: testFetch("owner/repo", head),
			RawAdditions: 300, Language: "Python", SizeBin: SizeSmall, DefectCategory: category, SourceType: sourceType,
			Direct: true, SmallLabel: label, GroundTruth: truth, ChangedRanges: []ChangedRange{{Path: "x.py", StartLine: 1, EndLine: 1}},
			CleanProvenance: clean, PatchSHA256: strings.Repeat("a", 64), BuildStateSHA256: strings.Repeat("b", 64),
		})
	}
	if err := ValidateSmallCorpus(small, nil); err != nil {
		t.Fatal(err)
	}
	small[0].RawAdditions = 301
	if err := ValidateSmallCorpus(small, nil); err == nil {
		t.Fatal("301-line small case accepted")
	}
	small[0].RawAdditions = 300
	small[0].Direct = false
	if err := ValidateSmallCorpus(small, nil); err == nil {
		t.Fatal("structured small case accepted")
	}
}

func TestMetamorphicTriplesPreserveCausalState(t *testing.T) {
	variants := []PreparedCase{
		metamorphicCase("begin", "beginning"),
		metamorphicCase("middle", "middle"),
		metamorphicCase("end", "end"),
	}
	padding := cleanPaddingCase()
	triple := testPreparedTriple(variants, []string{"padding"})
	if err := ValidatePreparedTriples(append(variants, padding), []PreparedTriple{triple}); err != nil {
		t.Fatal(err)
	}
	variants[1].CausalPatchSHA256 = strings.Repeat("c", 64)
	if err := ValidatePreparedTriples(append(variants, padding), []PreparedTriple{triple}); err == nil {
		t.Fatal("causal-patch drift accepted")
	}
}

func TestScoringFreezeContainsEveryGateAndSmallPolicy(t *testing.T) {
	freeze, tuning, heldOut, small := validScoringFixture()
	if err := ValidateScoringFreeze(freeze, tuning, heldOut, small); err != nil {
		t.Fatal(err)
	}
	freeze.Small.MaximumAbsolutePointDrop = 0.021
	if err := ValidateScoringFreeze(freeze, tuning, heldOut, small); err == nil {
		t.Fatal("weakened small gate accepted")
	}
	freeze, tuning, heldOut, small = validScoringFixture()
	freeze.Large.HeldOutBaseCaseIDs = append(freeze.Large.HeldOutBaseCaseIDs, freeze.Large.HeldOutBaseCaseIDs[0])
	if err := ValidateScoringFreeze(freeze, tuning, heldOut, small); err == nil {
		t.Fatal("duplicate held-out case id accepted")
	}
	freeze, tuning, heldOut, small = validScoringFixture()
	freeze.Prompts[0].SHA256 = strings.Repeat("0", 63)
	if err := ValidateScoringFreeze(freeze, tuning, heldOut, small); err == nil {
		t.Fatal("unpinned prompt content accepted")
	}
}

func TestFrozenCorpusFailsClosedOnUnresolvedAudit(t *testing.T) {
	corpus := FrozenCorpus{Schema: CorpusSchema, Set: "held_out", Cases: completeLargeCases(), AuditQueue: []AuditQueueItem{{CaseID: "case-00", Reasons: []string{"second independent review pending"}}}}
	if err := ValidateFrozenCorpus(corpus, 8); err == nil || !strings.Contains(err.Error(), "audit queue") {
		t.Fatalf("audit error = %v", err)
	}
}

func TestFrozenFilesDecodeStrictly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.json")
	if err := os.WriteFile(path, []byte(`{"schema":"review.eval-sources.v1","sources":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSources(path); err == nil {
		t.Fatal("unknown JSON field accepted")
	}
}

func completeLargeCases() []PreparedCase {
	categories := RequiredDefectCategories()
	languages := RequiredLanguages()
	bins := []string{Size301To1000, Size1001To3000, SizeOver3000}
	cases := make([]PreparedCase, 0, len(categories))
	for i, category := range categories {
		truth := []GroundTruth(nil)
		clean := (*CleanProvenance)(nil)
		sourceType := SourceHumanCaught
		if i%2 == 1 {
			sourceType = SourceInjectedValidated
		}
		if category == "clean" {
			sourceType = SourceClean
			clean = &CleanProvenance{Method: "merged_without_defect_followup", EvidenceRefs: []string{"https://example.com/history"}}
		} else {
			truth = []GroundTruth{{ID: caseID(i) + "-truth", DefectClass: category, Summary: "source-verified defect", Scenario: "the changed behavior fails", Locations: []Location{{Path: "x.go", StartLine: 10, EndLine: 10}}}}
		}
		bin := bins[i%len(bins)]
		churn := map[string]int{Size301To1000: 500, Size1001To3000: 1500, SizeOver3000: 3500}[bin]
		repository := "owner/repo" + caseID(i)
		base, head := shaFor(i), shaFor(i+100)
		cases = append(cases, PreparedCase{
			ID: caseID(i), SourceID: "source", SourceRecordID: caseID(i), Repository: repository,
			BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
			RawAdditions: churn, Language: languages[i%len(languages)], SizeBin: bin, DefectCategory: category,
			SourceType: sourceType, StructuredRequired: true, GroundTruth: truth,
			ChangedRanges: []ChangedRange{{Path: "x.go", StartLine: 1, EndLine: 20}}, CleanProvenance: clean,
			PatchSHA256: strings.Repeat("a", 64), BuildStateSHA256: strings.Repeat("b", 64),
		})
	}
	return cases
}

func metamorphicCase(id, position string) PreparedCase {
	truth := GroundTruth{
		ID:          id + ":family-truth",
		DefectClass: "local_logic",
		Severity:    "major",
		Summary:     "same defect",
		Scenario:    "same failure",
		Verifier:    "fixture-verifier",
		Provenance: &GroundTruthProvenance{
			SourceID:       "source",
			SourceRecordID: id,
			SourceDigest:   strings.Repeat("c", 64),
			EvidenceRefs:   []string{"fixture:evidence"},
		},
		Locations: []Location{{Path: "defect.go", StartLine: 10, EndLine: 10}},
	}
	truth.CanonicalDigest = groundTruthDigest(truth)
	repository := "owner/repo"
	base, head := strings.Repeat("a", 40), shaFor(len(id))
	return PreparedCase{
		ID: id, SourceID: "source", SourceRecordID: id, Repository: repository,
		BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
		RawAdditions: 500, Language: "Go", SizeBin: Size301To1000, DefectCategory: "local_logic",
		SourceType: SourceInjectedValidated, StructuredRequired: true,
		GroundTruth: []GroundTruth{truth}, ChangedRanges: []ChangedRange{{Path: "defect.go", StartLine: 9, EndLine: 11}},
		MetamorphicFamily: "triple", PaddingPosition: position, PaddingSourceCaseIDs: []string{"padding"},
		PaddingApplications: []PaddingApplication{{SourceCaseID: "padding", PatchSHA256: strings.Repeat("e", 64)}},
		CausalDigest:        causalTruthDigest(truth), CausalPatchSHA256: strings.Repeat("6", 64),
		VerificationStateSHA256: testVerificationState(), BuildStateSHA256: digestBytes([]byte("tree:" + id)),
		PatchSHA256: digestBytes([]byte("patch:" + id)), CanonicalDiffSHA256: digestBytes([]byte("diff:" + id)),
		DiffPosition: CanonicalDiffPosition{
			ByteStart: map[string]int{"beginning": 10, "middle": 40, "end": 70}[position],
			ByteEnd:   map[string]int{"beginning": 20, "middle": 50, "end": 80}[position], TotalBytes: 90,
			LineStart: map[string]int{"beginning": 1, "middle": 4, "end": 7}[position],
			LineEnd:   map[string]int{"beginning": 2, "middle": 5, "end": 8}[position], TotalLines: 9, Bin: position,
		},
	}
}

func canonicalGroundTruth(id, sourceRecordID string) GroundTruth {
	truth := GroundTruth{
		ID: id, DefectClass: "local_logic", Severity: "major",
		Summary: "reviewer-confirmed issue", Scenario: "the changed behavior is incorrect",
		Verifier: "official_swr_reviewer_confirmed",
		Provenance: &GroundTruthProvenance{
			SourceID: "swr", SourceRecordID: sourceRecordID, SourceDigest: strings.Repeat("c", 64),
			EvidenceRefs: []string{"fixture:evidence"},
		},
		Locations: []Location{{Path: "x.py", StartLine: 1, EndLine: 1}},
	}
	truth.CanonicalDigest = groundTruthDigest(truth)
	return truth
}

func approvedAudit() AuditProvenance {
	return AuditProvenance{Decisions: []AuditDecision{
		{
			Slot: "reviewer_1", ReviewerID: "reviewer-one", Toolchain: "review-console", ToolchainVersion: "1.2.3",
			ToolchainSHA256: strings.Repeat("c", 64), Decision: "approved", EvidenceRefs: []string{"fixture:one"},
		},
		{
			Slot: "reviewer_2", ReviewerID: "reviewer-two", Toolchain: "review-console", ToolchainVersion: "1.2.3",
			ToolchainSHA256: strings.Repeat("c", 64), Decision: "approved", EvidenceRefs: []string{"fixture:two"},
		},
	}}
}

func cleanPaddingCase() PreparedCase {
	repository := "owner/repo"
	base, head := shaFor(60), shaFor(61)
	provenance := &CleanProvenance{
		Method: "history", EvidenceRefs: []string{"history:evidence"}, Verifications: []VerificationEvidence{validVerificationEvidence()},
	}
	digest, err := CanonicalVerificationStateDigest(provenance)
	if err != nil {
		panic(err)
	}
	return PreparedCase{
		ID: "padding", SourceID: "source", SourceRecordID: "padding", Repository: repository,
		BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
		RawAdditions: 10, Language: "Go", SizeBin: SizeSmall, DefectCategory: "clean", SourceType: SourceClean, Direct: true,
		CleanProvenance: provenance, Audit: approvedAudit(),
		PatchSHA256: strings.Repeat("e", 64), BuildStateSHA256: strings.Repeat("b", 64), VerificationStateSHA256: digest,
	}
}

func validScoringFixture() (ScoringFreeze, FrozenCorpus, FrozenCorpus, FrozenCorpus) {
	sourceDigest, poolDigest := strings.Repeat("d", 64), strings.Repeat("e", 64)
	tuning := FrozenCorpus{Schema: CorpusSchema, Set: "tuning", SourcesManifestSHA256: sourceDigest, CandidatePoolSHA256: poolDigest}
	heldOut := FrozenCorpus{Schema: CorpusSchema, Set: "held_out", SourcesManifestSHA256: sourceDigest, CandidatePoolSHA256: poolDigest}
	small := FrozenCorpus{Schema: SmallCorpusSchema, Set: "small_non_regression", SourcesManifestSHA256: sourceDigest}
	for i := 0; i < 12; i++ {
		tuning.Cases = append(tuning.Cases, PreparedCase{ID: "tuning-" + caseID(i)})
	}
	for i := 0; i < 30; i++ {
		heldOut.Cases = append(heldOut.Cases, PreparedCase{ID: "held-" + caseID(i)})
	}
	for _, variant := range []string{"begin", "middle", "end"} {
		heldOut.Cases = append(heldOut.Cases, PreparedCase{ID: variant, MetamorphicFamily: "triple"})
	}
	heldOut.Triples = []PreparedTriple{{ID: "triple", BeginningCaseID: "begin", MiddleCaseID: "middle", EndCaseID: "end"}}
	small.Cases = []PreparedCase{{ID: "small"}}
	pin := func(id string, character byte) ContentPin {
		return ContentPin{ID: id, Version: "1", SHA256: strings.Repeat(string(character), 64)}
	}
	freeze := ScoringFreeze{
		Schema: ScoringFreezeSchema, PolicyVersion: EvalPolicyVersion, ArtifactSchemaVersion: EvalArtifactSchemaVersion,
		SourcesManifestSHA256: sourceDigest, CandidatePoolSHA256: poolDigest,
		Clients: []FrozenClient{
			{ID: "claude", Model: "claude-opus-4-1", ExecutableVersion: "1.0.0", ExecutableSHA256: strings.Repeat("a", 64)},
			{ID: "omp", Model: "openai-codex/gpt-5.6", ExecutableVersion: "1.0.0", ExecutableSHA256: strings.Repeat("b", 64)},
		},
		Toolchain:    FrozenToolchain{ProwlVersion: "1.0.0", ProwlSHA256: strings.Repeat("c", 64), PolicyID: EvalPolicyVersion, SchemaID: EvalOutputSchema},
		Prompts:      []ContentPin{pin("review-prompts", 'a')},
		SystemPolicy: pin("system-policy", 'b'),
		Formulas:     []ContentPin{pin("micro-f1", 'c')},
		MetricWeights: []MetricWeight{
			{ID: "micro-f1-weight", Version: "1", SHA256: strings.Repeat("d", 64), Weight: 1},
		},
		Toolchains: []ContentPin{pin("go", 'e'), pin("git", 'f')},
		Schemas:    []ContentPin{pin("eval-output", 'a'), pin("eval-manifest", 'b')},
		Large: FrozenLargeProtocol{
			TuningBaseCaseIDs: primaryCaseIDs(tuning.Cases), HeldOutBaseCaseIDs: primaryCaseIDs(heldOut.Cases),
			TripleIDs: []string{"triple"}, Repetitions: 3, MaxModelTokens: 100000, MaxToolCalls: 200,
			MaxSubagents: 16, WallTimeSeconds: 1800, MaxOutputBytes: 1048576, AggregateCaps: true,
			AcceptedFinding:     "parseable confirmed-or-plausible findings only",
			Matching:            "frozen causal-and-location eligibility with maximum-cardinality one-to-one matching and lexicographic tie-break",
			Adjudication:        "blind independent adjudication before condition labels and aggregate scores",
			FailureScoring:      "TP=0 FP=0 FN=ground-truth count coverage=0",
			BootstrapReplicates: ProductionBootstrapReplicates, ExecutionSeed: 20260902, BootstrapSeed: 1200260902,
			Gates: RequiredLargeGates(),
		},
		Small: FrozenSmallProtocol{CaseIDs: allCaseIDs(small.Cases), Repetitions: 3, TreatmentCompletion: 1, DirectOnly: true, MaximumAbsolutePointDrop: .02},
	}
	return freeze, tuning, heldOut, small
}

func caseID(i int) string { return "case-" + string(rune('a'+i)) }

func shaFor(i int) string {
	sum := sha256.Sum256([]byte{byte(i)})
	return hex.EncodeToString(sum[:20])
}

func gitFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repository := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q")
	run("config", "user.name", "Review Eval Test")
	run("config", "user.email", "review-eval@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "fixture.go")
	run("commit", "-qm", "base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("one\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("commit", "-qam", "head")
	return repository, base, run("rev-parse", "HEAD")
}

func TestPreparedManifestJSONRoundTrip(t *testing.T) {
	corpus := FrozenCorpus{Schema: CorpusSchema, Set: "tuning", Cases: completeLargeCases()}
	data, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var decoded FrozenCorpus
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Cases) != len(corpus.Cases) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestAuditApprovalRequiresTwoDecisionsAndEvidencedAdjudication(t *testing.T) {
	approved := approvedAudit()
	if !auditApproved(approved) {
		t.Fatal("two evidenced approvals were rejected")
	}
	rejected := approved
	rejected.Decisions = append([]AuditDecision(nil), approved.Decisions...)
	rejected.Decisions[0].Decision = "rejected"
	rejected.Decisions[1].Decision = "rejected"
	rejected.Adjudication = approvedAdjudication()
	if auditApproved(rejected) {
		t.Fatal("unanimous rejection was overridden")
	}
	disagreement := approved
	disagreement.Decisions = append([]AuditDecision(nil), approved.Decisions...)
	disagreement.Decisions[1].Decision = "rejected"
	if auditApproved(disagreement) {
		t.Fatal("unevidenced disagreement was approved")
	}
	disagreement.Adjudication = approvedAdjudication()
	if !auditApproved(disagreement) {
		t.Fatal("evidenced approved adjudication did not resolve disagreement")
	}
	unanimous := approved
	unanimous.Adjudication = approvedAdjudication()
	if auditApproved(unanimous) {
		t.Fatal("unanimous decisions accepted an unnecessary adjudication")
	}
}

func TestVerifyPreparedCaseGitRejectsEveryFrozenGitClaim(t *testing.T) {
	repository, base, head := gitFixture(t)
	candidate := PreparedCase{ID: "git-case", SourceID: "fixture", SourceRecordID: "fixture", Repository: "owner/repo", BaseSHA: base, HeadSHA: head}
	hydrated, err := HydrateGitCase(context.Background(), repository, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPreparedCaseGit(context.Background(), repository, hydrated); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PreparedCase){
		"additions":   func(c *PreparedCase) { c.RawAdditions++ },
		"deletions":   func(c *PreparedCase) { c.RawDeletions++ },
		"ranges":      func(c *PreparedCase) { c.ChangedRanges[0].EndLine++ },
		"size_bin":    func(c *PreparedCase) { c.SizeBin = Size301To1000 },
		"routing":     func(c *PreparedCase) { c.StructuredRequired = !c.StructuredRequired },
		"patch":       func(c *PreparedCase) { c.PatchSHA256 = strings.Repeat("0", 64) },
		"build_state": func(c *PreparedCase) { c.BuildStateSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := hydrated
			tampered.ChangedRanges = append([]ChangedRange(nil), hydrated.ChangedRanges...)
			mutate(&tampered)
			if err := VerifyPreparedCaseGit(context.Background(), repository, tampered); err == nil {
				t.Fatal("tampered frozen Git fact was accepted")
			}
		})
	}
}

func TestCorpusPartitionUsesFrozenPoolAndRejectsGlobalOverlap(t *testing.T) {
	records := make([]CandidatePoolRecord, 0, 45)
	for i := 0; i < 45; i++ {
		records = append(records, testCandidateRecord(i, CandidateSourceAACR, SourceHumanCaught))
	}
	digest, err := CanonicalCandidatePoolDigest(records)
	if err != nil {
		t.Fatal(err)
	}
	pool := CandidatePoolManifest{Schema: CandidatePoolSchema, SourcesManifestSHA256: strings.Repeat("a", 64), SHA256: digest, PartitionSeedSHA256: digest, Records: records}
	tuningRecords, heldRecords, err := PartitionCandidatePool(pool, 12, 30)
	if err != nil {
		t.Fatal(err)
	}
	tuning := FrozenCorpus{Schema: CorpusSchema, Set: "tuning", CandidatePoolSHA256: digest, AuditQueue: queueFromCandidateRecords(tuningRecords)}
	held := FrozenCorpus{Schema: CorpusSchema, Set: "held_out", CandidatePoolSHA256: digest, AuditQueue: queueFromCandidateRecords(heldRecords)}
	small := FrozenCorpus{Schema: SmallCorpusSchema, Set: "small_non_regression"}
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err != nil {
		t.Fatal(err)
	}
	held.AuditQueue[0] = tuning.AuditQueue[0]
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err == nil {
		t.Fatal("cross-set immutable identity overlap was accepted")
	}
}

func TestLargeMinimumCountsOnlyPrimaryBaseCases(t *testing.T) {
	cases := append([]PreparedCase(nil), completeLargeCases()...)
	for len(cases) < 11 {
		next := cases[len(cases)%len(cases)]
		next.ID = caseID(len(cases) + 20)
		next.Repository = "owner/" + next.ID
		next.BaseSHA, next.HeadSHA = shaFor(len(cases)+20), shaFor(len(cases)+120)
		next.BaseFetch, next.HeadFetch = testFetch(next.Repository, next.BaseSHA), testFetch(next.Repository, next.HeadSHA)
		next.SelectionKey = ""
		for i := range next.GroundTruth {
			next.GroundTruth[i].ID = next.ID + "-truth"
		}
		cases = append(cases, next)
	}
	for _, position := range []string{"beginning", "middle", "end"} {
		cases = append(cases, metamorphicCase("variant-"+position, position))
	}
	if err := ValidateLargeCorpus(cases, 12, false); err == nil || !strings.Contains(err.Error(), "primary base") {
		t.Fatalf("variants satisfied base minimum: %v", err)
	}
}

func TestSmallSourceBindingRejectsCanonicalGroundTruthMutation(t *testing.T) {
	source := PreparedCase{ID: "swr-source", SourceID: "swr", SourceRecordID: "record", Repository: "owner/repo", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), SmallLabel: SmallChangePR, SourceType: SourceHumanCaught, DefectCategory: "local_logic", GroundTruth: []GroundTruth{canonicalGroundTruth("truth", "record")}}
	frozen := source
	frozen.GroundTruth = append([]GroundTruth(nil), source.GroundTruth...)
	if err := validateSmallAgainstSWR([]PreparedCase{frozen}, []PreparedCase{source}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*GroundTruth){
		"severity":   func(g *GroundTruth) { g.Severity = "critical" },
		"summary":    func(g *GroundTruth) { g.Summary += " changed" },
		"verifier":   func(g *GroundTruth) { g.Verifier = "other" },
		"provenance": func(g *GroundTruth) { g.Provenance.SourceDigest = strings.Repeat("0", 64) },
		"digest":     func(g *GroundTruth) { g.CanonicalDigest = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := frozen
			tampered.GroundTruth = append([]GroundTruth(nil), frozen.GroundTruth...)
			provenance := *tampered.GroundTruth[0].Provenance
			tampered.GroundTruth[0].Provenance = &provenance
			mutate(&tampered.GroundTruth[0])
			if err := validateSmallAgainstSWR([]PreparedCase{tampered}, []PreparedCase{source}); err == nil {
				t.Fatal("mutated canonical SWR ground truth was accepted")
			}
		})
	}
}

func TestMetamorphicTriplesRequireAuditedCleanPaddingAndExactCoverage(t *testing.T) {
	padding := cleanPaddingCase()
	variants := []PreparedCase{metamorphicCase("begin", "beginning"), metamorphicCase("middle", "middle"), metamorphicCase("end", "end")}
	triple := testPreparedTriple(variants, []string{"padding"})
	cases := append(variants, padding)
	if err := ValidatePreparedTriples(cases, []PreparedTriple{triple}); err != nil {
		t.Fatal(err)
	}
	cases[3].Audit.Decisions[0].Decision = "rejected"
	if err := ValidatePreparedTriples(cases, []PreparedTriple{triple}); err == nil {
		t.Fatal("unaudited clean padding was accepted")
	}
	cases = append([]PreparedCase(nil), variants...)
	cases[0].PaddingSourceCaseIDs = nil
	cases = append(cases, padding)
	if err := ValidatePreparedTriples(cases, []PreparedTriple{triple}); err == nil {
		t.Fatal("variant padding provenance drift was accepted")
	}
	extra := metamorphicCase("extra", "beginning")
	if err := ValidatePreparedTriples(append(append([]PreparedCase(nil), variants...), padding, extra), []PreparedTriple{triple}); err == nil {
		t.Fatal("uncovered metamorphic case was accepted")
	}
}

func TestDownloaderNetworkPolicyRejectsUnsafeSchemesOriginsAndIPs(t *testing.T) {
	source := SourceSpec{URL: "https://example.com/data.json", AllowedOrigins: []string{"https://cdn.example.com"}}
	if err := validateSourceURL(source, mustParseURL(t, "https://example.com/next")); err != nil {
		t.Fatal(err)
	}
	if err := validateSourceURL(source, mustParseURL(t, "https://cdn.example.com/next")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"http://example.com/data.json",
		"https://other.example/data.json",
		"https://127.0.0.1/data.json",
		"https://169.254.169.254/latest/meta-data",
	} {
		if err := validateSourceURL(source, mustParseURL(t, target)); err == nil {
			t.Fatalf("unsafe target %q accepted", target)
		}
	}
	for _, address := range []string{
		"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1",
		"192.31.196.1", "192.52.193.1", "192.175.48.1", "::1", "64:ff9b:1::1",
		"100::1", "fc00::1", "fe80::1", "2001:20::1", "2001:db8::1", "3fff::1", "5f00::1",
	} {
		if isPublicIP(net.ParseIP(address)) {
			t.Fatalf("non-public IP %s accepted", address)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IP rejected")
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestCorpusPartitionsAcceptCanonicalMultiSourcePool(t *testing.T) {
	kinds := []string{
		CandidateSourceAACR,
		CandidateSourceCodeReviewBench,
		CandidateSourceQodoInjected,
		CandidateSourcePublicHistoryClean,
	}
	records := make([]CandidatePoolRecord, 0, 45)
	for i := 0; i < 45; i++ {
		kind := kinds[i%len(kinds)]
		sourceType := SourceHumanCaught
		if kind == CandidateSourceQodoInjected {
			sourceType = SourceInjectedValidated
		} else if kind == CandidateSourcePublicHistoryClean {
			sourceType = SourceClean
		}
		records = append(records, testCandidateRecord(i, kind, sourceType))
	}
	digest, err := CanonicalCandidatePoolDigest(records)
	if err != nil {
		t.Fatal(err)
	}
	pool := CandidatePoolManifest{Schema: CandidatePoolSchema, SourcesManifestSHA256: strings.Repeat("a", 64), SHA256: digest, PartitionSeedSHA256: digest, Records: records}
	tuningRecords, heldRecords, err := PartitionCandidatePool(pool, 12, 30)
	if err != nil {
		t.Fatal(err)
	}
	toCases := func(input []CandidatePoolRecord) []PreparedCase {
		result := make([]PreparedCase, 0, len(input))
		for _, record := range input {
			result = append(result, PreparedCase{
				ID: record.ID, CandidateSource: record.CandidateSource, SourceID: record.SourceID, SourceRecordID: record.SourceRecordID,
				SourceRowSHA256: record.SourceRowSHA256, SourceType: record.SourceType, Repository: record.Repository,
				BaseSHA: record.BaseSHA, BaseFetch: record.BaseFetch, HeadSHA: record.HeadSHA, HeadFetch: record.HeadFetch,
				SelectionKey: record.SelectionKey, Language: record.Language,
			})
		}
		return result
	}
	tuning := FrozenCorpus{Schema: CorpusSchema, Set: "tuning", CandidatePoolSHA256: digest, Cases: toCases(tuningRecords)}
	held := FrozenCorpus{Schema: CorpusSchema, Set: "held_out", CandidatePoolSHA256: digest, Cases: toCases(heldRecords)}
	small := FrozenCorpus{Schema: SmallCorpusSchema, Set: "small_non_regression"}
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err != nil {
		t.Fatal(err)
	}
	tuning.Cases[0].SourceID = "tampered-source"
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err == nil {
		t.Fatal("primary case with noncanonical source binding was accepted")
	}
}

func TestMetamorphicPaddingUsesPortablePatchesNotEqualTrees(t *testing.T) {
	before, after := cleanPaddingCase(), cleanPaddingCase()
	before.ID, before.SourceRecordID, before.BuildStateSHA256, before.PatchSHA256 = "padding-before", "padding-before", strings.Repeat("e", 64), strings.Repeat("1", 64)
	after.ID, after.SourceRecordID, after.BuildStateSHA256, after.PatchSHA256 = "padding-after", "padding-after", strings.Repeat("f", 64), strings.Repeat("2", 64)
	for _, padding := range []*PreparedCase{&before, &after} {
		padding.VerificationStateSHA256 = testVerificationState()
	}
	variants := []PreparedCase{
		positionalVariant("begin", "beginning", []PreparedCase{after}),
		positionalVariant("middle", "middle", []PreparedCase{before, after}),
		positionalVariant("end", "end", []PreparedCase{before}),
	}
	triple := testPreparedTriple(variants, []string{"padding-before", "padding-after"})
	if err := ValidatePreparedTriples(append(variants, before, after), []PreparedTriple{triple}); err != nil {
		t.Fatal(err)
	}
	variants[1].PaddingApplications[0].PatchSHA256 = strings.Repeat("0", 64)
	if err := ValidatePreparedTriples(append(variants, before, after), []PreparedTriple{triple}); err == nil {
		t.Fatal("noncanonical padding patch binding was accepted")
	}
}

func TestCanonicalDiffPositionUsesWholeDiffOffsetsNotSourceLines(t *testing.T) {
	location := []Location{{Path: "m-defect.go", StartLine: 10, EndLine: 10}}
	tests := []struct {
		name   string
		before int
		after  int
		bin    string
	}{
		{name: "beginning", before: 0, after: 60, bin: "beginning"},
		{name: "middle", before: 30, after: 30, bin: "middle"},
		{name: "end", before: 60, after: 0, bin: "end"},
	}
	seen := map[string]bool{}
	for _, test := range tests {
		position, err := ComputeCanonicalDiffPosition(positionalNativeRecords(test.before, test.after), location)
		if err != nil {
			t.Fatal(err)
		}
		if position.Bin != test.bin || seen[position.Bin] {
			t.Fatalf("%s position = %#v", test.name, position)
		}
		seen[position.Bin] = true
	}
}

func TestVerifyPreparedTripleGitReproducesPortablePaddingTrees(t *testing.T) {
	repositoryPath := t.TempDir()
	runTestGit(t, repositoryPath, "init", "-q")
	runTestGit(t, repositoryPath, "config", "user.email", "fixture@example.com")
	runTestGit(t, repositoryPath, "config", "user.name", "Fixture")
	paddingText := func(prefix string) string {
		var builder strings.Builder
		for i := 0; i < 120; i++ {
			fmt.Fprintf(&builder, "%s-%03d\n", prefix, i)
		}
		return builder.String()
	}
	writeFixture := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repositoryPath, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		runTestGit(t, repositoryPath, "add", ".")
		runTestGit(t, repositoryPath, "commit", "-qm", message)
		return runTestGit(t, repositoryPath, "rev-parse", "HEAD")
	}
	writeFixture("a-padding.txt", paddingText("a-old"))
	writeFixture("m-defect.go", "package fixture\n\nvar value = 1\n")
	writeFixture("z-padding.txt", paddingText("z-old"))
	base := commit("base")
	writeFixture("m-defect.go", "package fixture\n\nvar value = 2\n")
	causalHead := commit("causal")

	runTestGit(t, repositoryPath, "checkout", "-qb", "padding-before", base)
	writeFixture("a-padding.txt", paddingText("a-new"))
	beforeHead := commit("padding before")
	runTestGit(t, repositoryPath, "checkout", "-qb", "padding-after", base)
	writeFixture("z-padding.txt", paddingText("z-new"))
	afterHead := commit("padding after")
	halfPaddingText := func(oldPrefix, newPrefix string) string {
		var builder strings.Builder
		for i := 0; i < 120; i++ {
			prefix := oldPrefix
			if i < 60 {
				prefix = newPrefix
			}
			fmt.Fprintf(&builder, "%s-%03d\n", prefix, i)
		}
		return builder.String()
	}
	runTestGit(t, repositoryPath, "checkout", "-qb", "padding-before-half", base)
	writeFixture("a-padding.txt", halfPaddingText("a-old", "a-new"))
	beforeHalfHead := commit("padding before half")
	runTestGit(t, repositoryPath, "checkout", "-qb", "padding-after-half", base)
	writeFixture("z-padding.txt", halfPaddingText("z-old", "z-new"))
	afterHalfHead := commit("padding after half")

	makeVariantCommit := func(branch, beforeContent, afterContent string) string {
		t.Helper()
		runTestGit(t, repositoryPath, "checkout", "-qb", branch, causalHead)
		if beforeContent != "" {
			writeFixture("a-padding.txt", beforeContent)
		}
		if afterContent != "" {
			writeFixture("z-padding.txt", afterContent)
		}
		return commit(branch)
	}
	beginHead := makeVariantCommit("variant-begin", "", paddingText("z-new"))
	middleHead := makeVariantCommit("variant-middle", halfPaddingText("a-old", "a-new"), halfPaddingText("z-old", "z-new"))
	endHead := makeVariantCommit("variant-end", paddingText("a-new"), "")

	repository := "owner/repo"
	verificationProvenance := &CleanProvenance{
		Method: "verified fixture", EvidenceRefs: []string{"fixture:portable-padding"}, Verifications: []VerificationEvidence{validVerificationEvidence()},
	}
	verificationState, err := CanonicalVerificationStateDigest(verificationProvenance)
	if err != nil {
		t.Fatal(err)
	}
	makePadding := func(id, head string) PreparedCase {
		candidate := PreparedCase{
			ID: id, SourceID: "fixture", SourceRecordID: id, Repository: repository,
			BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
		}
		hydrated, err := HydrateGitCase(context.Background(), repositoryPath, candidate)
		if err != nil {
			t.Fatal(err)
		}
		hydrated.SourceType, hydrated.DefectCategory, hydrated.Direct = SourceClean, "clean", true
		provenanceCopy := *verificationProvenance
		hydrated.CleanProvenance = &provenanceCopy
		hydrated.Audit = approvedAudit()
		hydrated.VerificationStateSHA256 = verificationState
		return hydrated
	}
	beforePadding, afterPadding := makePadding("padding-before", beforeHead), makePadding("padding-after", afterHead)
	beforeHalfPadding := makePadding("padding-before-half", beforeHalfHead)
	afterHalfPadding := makePadding("padding-after-half", afterHalfHead)
	causalPatch, err := canonicalGitPatch(context.Background(), repositoryPath, base, causalHead)
	if err != nil {
		t.Fatal(err)
	}
	causalPatchDigest := digestBytes(causalPatch)
	makeVariant := func(id, position, head string, padding []PreparedCase) PreparedCase {
		truth := GroundTruth{
			ID: id + ":family-truth", DefectClass: "local_logic", Severity: "major",
			Summary: "same defect", Scenario: "same failure", Verifier: "fixture-verifier",
			Provenance: &GroundTruthProvenance{
				SourceID: "fixture", SourceRecordID: id, SourceDigest: strings.Repeat("c", 64), EvidenceRefs: []string{"fixture:causal"},
			},
			Locations: []Location{{Path: "m-defect.go", StartLine: 3, EndLine: 3}},
		}
		truth.CanonicalDigest = groundTruthDigest(truth)
		candidate := PreparedCase{
			ID: id, SourceID: "fixture", SourceRecordID: id, Repository: repository,
			BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
			MetamorphicFamily: "triple", PaddingPosition: position, GroundTruth: []GroundTruth{truth},
			CausalDigest: causalTruthDigest(truth), CausalPatchSHA256: causalPatchDigest,
			VerificationStateSHA256: verificationState,
		}
		for _, source := range padding {
			candidate.PaddingSourceCaseIDs = append(candidate.PaddingSourceCaseIDs, source.ID)
			candidate.PaddingApplications = append(candidate.PaddingApplications, PaddingApplication{SourceCaseID: source.ID, PatchSHA256: source.PatchSHA256})
		}
		hydrated, hydrateErr := HydrateGitCase(context.Background(), repositoryPath, candidate)
		if hydrateErr != nil {
			t.Fatal(hydrateErr)
		}
		return hydrated
	}
	variants := []PreparedCase{
		makeVariant("begin", "beginning", beginHead, []PreparedCase{afterPadding}),
		makeVariant("middle", "middle", middleHead, []PreparedCase{beforeHalfPadding, afterHalfPadding}),
		makeVariant("end", "end", endHead, []PreparedCase{beforePadding}),
	}
	allPadding := []PreparedCase{beforePadding, beforeHalfPadding, afterHalfPadding, afterPadding}
	triple := testPreparedTriple(variants, []string{"padding-before", "padding-before-half", "padding-after-half", "padding-after"})
	triple.CausalHeadSHA, triple.CausalHeadFetch = causalHead, testFetch(repository, causalHead)
	if err := VerifyPreparedTripleGit(context.Background(), repositoryPath, append(variants, allPadding...), triple); err != nil {
		t.Fatal(err)
	}
}

func TestGitCommandsAreBoundedSanitizedAndDeadlineAware(t *testing.T) {
	repository, _, head := gitFixture(t)
	if err := os.WriteFile(filepath.Join(repository, "large.txt"), []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "large.txt")
	runTestGit(t, repository, "commit", "-qm", "large")
	head = runTestGit(t, repository, "rev-parse", "HEAD")
	if _, err := runGitBounded(context.Background(), repository, 128, "show", head+":large.txt"); !errors.Is(err, ErrGitOutputLimit) {
		t.Fatalf("output limit error = %v", err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[alias]\npoison = !exit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	if _, err := runGitBounded(context.Background(), repository, 1024, "poison"); err == nil {
		t.Fatal("ambient global Git alias was honored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runGitBounded(ctx, repository, 1024, "-c", "alias.wait=!sleep 5", "wait"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Git process group survived cancellation for %s", elapsed)
	}
}

func TestGitFetchMetadataRequiresImmutableGitHubOIDBinding(t *testing.T) {
	valid := GitFetchSpec{RemoteURL: "https://github.com/owner/repo.git", Ref: "refs/pull/42/head", OID: strings.Repeat("a", 40)}
	if err := ValidateGitFetchSpec(valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []GitFetchSpec{
		{RemoteURL: "http://github.com/owner/repo.git", Ref: valid.Ref, OID: valid.OID},
		{RemoteURL: valid.RemoteURL, Ref: "main", OID: valid.OID},
		{RemoteURL: valid.RemoteURL, Ref: valid.Ref, OID: "deadbeef"},
	} {
		if err := ValidateGitFetchSpec(invalid); err == nil {
			t.Fatalf("unsafe Git fetch metadata accepted: %#v", invalid)
		}
	}
}

func TestAuditRequiresImmutableDistinctReviewerAndAdjudicatorIdentities(t *testing.T) {
	audit := approvedAudit()
	if !auditApproved(audit) {
		t.Fatal("valid independent audit rejected")
	}
	audit.Decisions[1].ReviewerID = audit.Decisions[0].ReviewerID
	if auditApproved(audit) {
		t.Fatal("duplicate reviewer identity accepted")
	}
	audit = approvedAudit()
	audit.Decisions[1].Decision = "rejected"
	audit.Adjudication = &AuditAdjudication{
		Decision: "approved", EvidenceRefs: []string{"adjudication:evidence"},
		AdjudicatorID: "reviewer-three", Toolchain: "review-console", ToolchainVersion: "1.2.3",
		ToolchainSHA256: strings.Repeat("d", 64),
	}
	if !auditApproved(audit) {
		t.Fatal("valid distinct adjudicator rejected")
	}
	audit.Adjudication.AdjudicatorID = audit.Decisions[0].ReviewerID
	if auditApproved(audit) {
		t.Fatal("reviewer was allowed to adjudicate their own disagreement")
	}
}

func validVerificationEvidence() VerificationEvidence {
	return VerificationEvidence{
		Toolchain: "go", ToolchainVersion: "1.24.0", ToolchainSHA256: strings.Repeat("7", 64),
		Command: "go test ./...", ExitCode: 0, OutputSHA256: strings.Repeat("8", 64), EvidenceRefs: []string{"fixture:verification"},
	}
}

func testVerificationState() string {
	digest, err := CanonicalVerificationStateDigest(&CleanProvenance{
		Method: "history", EvidenceRefs: []string{"history:evidence"}, Verifications: []VerificationEvidence{validVerificationEvidence()},
	})
	if err != nil {
		panic(err)
	}
	return digest
}

func positionalVariant(id, position string, padding []PreparedCase) PreparedCase {
	c := metamorphicCase(id, position)
	c.BuildStateSHA256 = digestBytes([]byte("tree:" + id))
	c.VerificationStateSHA256 = testVerificationState()
	c.CausalPatchSHA256 = strings.Repeat("6", 64)
	c.PaddingSourceCaseIDs = nil
	c.PaddingApplications = nil
	for _, source := range padding {
		c.PaddingSourceCaseIDs = append(c.PaddingSourceCaseIDs, source.ID)
		c.PaddingApplications = append(c.PaddingApplications, PaddingApplication{SourceCaseID: source.ID, PatchSHA256: source.PatchSHA256})
	}
	c.DiffPosition = CanonicalDiffPosition{
		ByteStart:  map[string]int{"beginning": 10, "middle": 40, "end": 70}[position],
		ByteEnd:    map[string]int{"beginning": 20, "middle": 50, "end": 80}[position],
		TotalBytes: 90,
		LineStart:  map[string]int{"beginning": 1, "middle": 4, "end": 7}[position],
		LineEnd:    map[string]int{"beginning": 2, "middle": 5, "end": 8}[position],
		TotalLines: 9,
		Bin:        position,
	}
	c.CanonicalDiffSHA256 = strings.Repeat("5", 64)
	c.GroundTruth[0].Locations = []Location{{Path: "defect.go", StartLine: 10, EndLine: 10}}
	c.GroundTruth[0].CanonicalDigest = groundTruthDigest(c.GroundTruth[0])
	return c
}

func testCandidateRecord(index int, source, sourceType string) CandidatePoolRecord {
	repository := fmt.Sprintf("owner/repo-%02d", index)
	base, head := shaFor(index), shaFor(index+100)
	sourceID, recordID := source, fmt.Sprintf("record-%02d", index)
	evidence := []string{fmt.Sprintf("source:%02d", index)}
	pullRequestURL := fmt.Sprintf("https://github.com/%s/pull/%d", repository, index+1)
	if source == CandidateSourcePublicHistoryClean {
		pullRequestURL = ""
	}
	row := CandidateSourceRow{
		CandidateSource: source, SourceID: sourceID, SourceRecordID: recordID, Repository: repository,
		PullRequestURL: pullRequestURL,
		BaseSHA:        base, HeadSHA: head, License: "MIT", Provenance: strings.Repeat("f", 40), Language: "Go",
		EvidenceRefs: evidence, RawRecordSHA256: digestBytes([]byte(fmt.Sprintf("raw:%d", index))),
	}
	rowDigest, err := CanonicalCandidateSourceRowDigest(row)
	if err != nil {
		panic(err)
	}
	return CandidatePoolRecord{
		ID: fmt.Sprintf("candidate-%02d", index), CandidateSource: source,
		SourceID: sourceID, SourceRecordID: recordID, SourceType: sourceType,
		Repository: repository, BaseSHA: base, HeadSHA: head,
		SelectionKey: digestBytes([]byte(repository + "\x00" + base + "\x00" + head)),
		Language:     "Go", EvidenceRefs: evidence, SourceRow: row, SourceRowSHA256: rowDigest,
		BaseFetch: GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: base, OID: base},
		HeadFetch: GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: fmt.Sprintf("refs/pull/%d/head", index+1), OID: head},
	}
}

func queueFromCandidateRecords(records []CandidatePoolRecord) []AuditQueueItem {
	queue := make([]AuditQueueItem, 0, len(records))
	for _, record := range records {
		queue = append(queue, AuditQueueItem{
			CaseID: record.ID, CandidateSource: record.CandidateSource, SourceID: record.SourceID,
			SourceRecordID: record.SourceRecordID, SourceType: record.SourceType, Repository: record.Repository,
			BaseSHA: record.BaseSHA, HeadSHA: record.HeadSHA, SelectionKey: record.SelectionKey, Language: record.Language,
			SourceReportedChurn: record.SourceReportedChurn, SourceRowSHA256: record.SourceRowSHA256,
			EvidenceRefs: append([]string(nil), record.EvidenceRefs...),
			BaseFetch:    record.BaseFetch, HeadFetch: record.HeadFetch,
		})
	}
	return queue
}

func runTestGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repository
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func testFetch(repository, oid string) GitFetchSpec {
	return GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: oid, OID: oid}
}

func approvedAdjudication() *AuditAdjudication {
	return &AuditAdjudication{
		AdjudicatorID: "reviewer-three", Toolchain: "review-console", ToolchainVersion: "1.2.3",
		ToolchainSHA256: strings.Repeat("d", 64), Decision: "approved", EvidenceRefs: []string{"adjudication:evidence"},
	}
}

func testPreparedTriple(variants []PreparedCase, paddingIDs []string) PreparedTriple {
	repository := variants[0].Repository
	causalHead := shaFor(200)
	return PreparedTriple{
		ID: "triple", GroundTruthID: "family-truth", CausalDigest: variants[0].CausalDigest,
		CausalPatchSHA256: variants[0].CausalPatchSHA256,
		CausalBaseSHA:     variants[0].BaseSHA, CausalBaseFetch: testFetch(repository, variants[0].BaseSHA),
		CausalHeadSHA: causalHead, CausalHeadFetch: testFetch(repository, causalHead),
		ExpectedRawChurn: variants[0].RawAdditions + variants[0].RawDeletions,
		SizeBin:          variants[0].SizeBin,
		MaximumByteSizeDelta: max(variants[0].DiffPosition.TotalBytes, variants[1].DiffPosition.TotalBytes, variants[2].DiffPosition.TotalBytes) -
			min(variants[0].DiffPosition.TotalBytes, variants[1].DiffPosition.TotalBytes, variants[2].DiffPosition.TotalBytes),
		MaximumLineSizeDelta: max(variants[0].DiffPosition.TotalLines, variants[1].DiffPosition.TotalLines, variants[2].DiffPosition.TotalLines) -
			min(variants[0].DiffPosition.TotalLines, variants[1].DiffPosition.TotalLines, variants[2].DiffPosition.TotalLines),
		VerificationStateSHA256: variants[0].VerificationStateSHA256,
		BeginningCaseID:         variants[0].ID, MiddleCaseID: variants[1].ID, EndCaseID: variants[2].ID,
		PaddingSourceCaseIDs: append([]string(nil), paddingIDs...),
	}
}
