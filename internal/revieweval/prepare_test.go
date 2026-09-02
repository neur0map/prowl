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
	bad[0].GroundTruth[0].CanonicalDigest = groundTruthDigest(bad[0].GroundTruth[0])
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
	padding := cleanPaddingCase(t)
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
func TestScoringIntegrityRejectsTamperDespiteReadinessBlockers(t *testing.T) {
	freeze, tuning, heldOut, _ := validScoringFixture()
	tuning.Cases = tuning.Cases[:1]
	heldOut.Cases = heldOut.Cases[:1]
	currentPoolDigest := freeze.CandidatePoolSHA256
	freeze.FreezeBlockers = []string{"held-out deficit"}
	freeze.CandidatePoolSHA256 = strings.Repeat("f", 64)
	if err := ValidateScoringFreezeIntegrity(freeze, tuning.SourcesManifestSHA256, currentPoolDigest); err == nil ||
		!strings.Contains(err.Error(), "exact candidate pool") {
		t.Fatalf("readiness blocker masked stale candidate-pool binding: %v", err)
	}

	freeze, _, _, _ = validScoringFixture()
	freeze.FreezeBlockers = []string{"held-out deficit"}
	if err := ValidateScoringFreezeIntegrity(freeze, freeze.SourcesManifestSHA256, freeze.CandidatePoolSHA256); err != nil {
		t.Fatalf("honest readiness blocker failed immutable validation: %v", err)
	}
	freeze.Clients[0].Model = ""
	if err := ValidateScoringFreezeReadiness(freeze); err == nil ||
		!strings.Contains(err.Error(), "held-out deficit") ||
		!strings.Contains(err.Error(), "frozen client claude lacks exact model or executable pins") {
		t.Fatalf("readiness blockers and toolchain pins were not aggregated: %v", err)
	}

	freeze.Large.Gates = freeze.Large.Gates[:len(freeze.Large.Gates)-1]
	if err := ValidateScoringFreezeIntegrity(freeze, freeze.SourcesManifestSHA256, freeze.CandidatePoolSHA256); err == nil ||
		!strings.Contains(err.Error(), "every large shipping gate") {
		t.Fatalf("readiness blocker masked weakened gate set: %v", err)
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
			groundTruth := GroundTruth{
				ID: caseID(i) + "-truth", DefectClass: category, Severity: "major",
				Summary: "source-verified defect", Scenario: "the changed behavior fails", Verifier: "fixture-verifier",
				Provenance: &GroundTruthProvenance{
					SourceID: "source", SourceRecordID: caseID(i), SourceDigest: strings.Repeat("c", 64),
					EvidenceRefs: []string{"fixture:evidence"},
				},
				Locations: []Location{{Path: "x.go", StartLine: 10, EndLine: 10}},
			}
			groundTruth.CanonicalDigest = groundTruthDigest(groundTruth)
			truth = []GroundTruth{groundTruth}
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

type auditFixtureConfig struct {
	ReviewerIDs            [2]string
	Decisions              [2]string
	AdjudicatorID          string
	AdjudicatorDecision    string
	MutateRecord           func(int, *AuditEvidenceRecord)
	MutateOutput           func(int, *AuditEvidenceOutput)
	MutateRecordPayload    func(int, []byte) []byte
	MutateOutputPayload    func(int, []byte) []byte
	DeleteOutput           int
	CorruptOutputAfterHash int
	ExtraOutput            bool
}

func approvedAuditConfig() auditFixtureConfig {
	return auditFixtureConfig{
		ReviewerIDs: [2]string{"reviewer-one", "reviewer-two"},
		Decisions:   [2]string{"approved", "approved"},
	}
}

func approvedCase(t *testing.T, c PreparedCase) PreparedCase {
	t.Helper()
	loaded, err := persistAuditCase(t, c, approvedAuditConfig())
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func persistAuditCase(t *testing.T, c PreparedCase, config auditFixtureConfig) (PreparedCase, error) {
	t.Helper()
	dir := t.TempDir()
	outputs := []AuditEvidenceOutput{
		auditEvidenceOutput(c, config.ReviewerIDs[0], "reviewer", config.Decisions[0]),
		auditEvidenceOutput(c, config.ReviewerIDs[1], "reviewer", config.Decisions[1]),
	}
	c.Audit = AuditProvenance{Decisions: []AuditDecision{
		{Slot: "reviewer_1", EvidenceID: "evidence-1"},
		{Slot: "reviewer_2", EvidenceID: "evidence-2"},
	}}
	if config.AdjudicatorID != "" {
		outputs = append(outputs, auditEvidenceOutput(c, config.AdjudicatorID, "adjudicator", config.AdjudicatorDecision))
		c.Audit.Adjudication = &AuditAdjudication{EvidenceID: "evidence-3"}
	}
	if config.ExtraOutput {
		outputs = append(outputs, auditEvidenceOutput(c, "unused-reviewer", "reviewer", "approved"))
	}
	specs := make([]AuditEvidenceSpec, 0, len(outputs))
	for index := range outputs {
		id := fmt.Sprintf("evidence-%d", index+1)
		if config.MutateOutput != nil {
			config.MutateOutput(index, &outputs[index])
		}
		outputName := id + ".output.json"
		outputPath := filepath.Join(dir, outputName)
		if err := writeCanonicalJSON(outputPath, outputs[index]); err != nil {
			return PreparedCase{}, err
		}
		outputPayload, err := os.ReadFile(outputPath)
		if err != nil {
			return PreparedCase{}, err
		}
		if config.MutateOutputPayload != nil {
			outputPayload = config.MutateOutputPayload(index, outputPayload)
			if err := os.WriteFile(outputPath, outputPayload, 0o600); err != nil {
				return PreparedCase{}, err
			}
		}
		record := auditEvidenceRecord(c, outputs[index], digestBytes(outputPayload))
		if config.MutateRecord != nil {
			config.MutateRecord(index, &record)
		}
		recordName := id + ".record.json"
		recordPath := filepath.Join(dir, recordName)
		if err := writeCanonicalJSON(recordPath, record); err != nil {
			return PreparedCase{}, err
		}
		recordPayload, err := os.ReadFile(recordPath)
		if err != nil {
			return PreparedCase{}, err
		}
		if config.MutateRecordPayload != nil {
			recordPayload = config.MutateRecordPayload(index, recordPayload)
			if err := os.WriteFile(recordPath, recordPayload, 0o600); err != nil {
				return PreparedCase{}, err
			}
		}
		specs = append(specs, AuditEvidenceSpec{
			ID: id, RecordPath: recordName, RecordSHA256: digestBytes(recordPayload),
			OutputPath: outputName, OutputSHA256: digestBytes(outputPayload),
		})
	}
	if config.CorruptOutputAfterHash > 0 && config.CorruptOutputAfterHash <= len(specs) {
		path := filepath.Join(dir, specs[config.CorruptOutputAfterHash-1].OutputPath)
		payload, err := os.ReadFile(path)
		if err != nil {
			return PreparedCase{}, err
		}
		if err := os.WriteFile(path, append(payload, ' '), 0o600); err != nil {
			return PreparedCase{}, err
		}
	}
	corpus := FrozenCorpus{
		Schema: CorpusSchema, Set: "tuning", SourcesManifestSHA256: strings.Repeat("a", 64),
		Cases: []PreparedCase{c}, AuditQueue: []AuditQueueItem{}, AuditEvidence: specs,
	}
	if config.DeleteOutput > 0 && config.DeleteOutput <= len(specs) {
		if err := os.Remove(filepath.Join(dir, specs[config.DeleteOutput-1].OutputPath)); err != nil {
			return PreparedCase{}, err
		}
	}
	corpusPath := filepath.Join(dir, "corpus.json")
	if err := writeCanonicalJSON(corpusPath, corpus); err != nil {
		return PreparedCase{}, err
	}
	loaded, err := LoadFrozenCorpus(corpusPath)
	if err != nil {
		return PreparedCase{}, err
	}
	return loaded.Cases[0], nil
}

func auditEvidenceOutput(c PreparedCase, actorID, role, decision string) AuditEvidenceOutput {
	return AuditEvidenceOutput{
		Schema: AuditEvidenceOutputSchema, CaseID: c.ID, ActorID: actorID, ActorRole: role, Decision: decision,
		Findings: c.GroundTruth, SourceRowSHA256: c.SourceRowSHA256,
		PatchSHA256: c.PatchSHA256, BuildStateSHA256: c.BuildStateSHA256,
	}
}

func auditEvidenceRecord(c PreparedCase, output AuditEvidenceOutput, outputSHA256 string) AuditEvidenceRecord {
	return AuditEvidenceRecord{
		Schema: AuditEvidenceRecordSchema, CaseID: output.CaseID, ActorID: output.ActorID,
		ActorRole: output.ActorRole, Decision: output.Decision, Findings: output.Findings,
		Toolchain: "review-console", ToolchainVersion: "1.2.3", ToolchainSHA256: strings.Repeat("c", 64),
		SourceRowSHA256: c.SourceRowSHA256, PatchSHA256: c.PatchSHA256, BuildStateSHA256: c.BuildStateSHA256,
		OutputSHA256: outputSHA256,
	}
}

func cleanPaddingCase(t *testing.T) PreparedCase {
	repository := "owner/repo"
	base, head := shaFor(60), shaFor(61)
	provenance := &CleanProvenance{
		Method: "history", EvidenceRefs: []string{"history:evidence"}, Verifications: []VerificationEvidence{validVerificationEvidence()},
	}
	digest, err := CanonicalVerificationStateDigest(provenance)
	if err != nil {
		panic(err)
	}
	c := PreparedCase{
		ID: "padding", SourceID: "source", SourceRecordID: "padding", Repository: repository,
		BaseSHA: base, BaseFetch: testFetch(repository, base), HeadSHA: head, HeadFetch: testFetch(repository, head),
		RawAdditions: 10, Language: "Go", SizeBin: SizeSmall, DefectCategory: "clean", SourceType: SourceClean, Direct: true,
		CleanProvenance: provenance, SourceRowSHA256: strings.Repeat("a", 64),
		PatchSHA256: strings.Repeat("e", 64), BuildStateSHA256: strings.Repeat("b", 64), VerificationStateSHA256: digest,
	}
	return approvedCase(t, c)
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

func TestNativeChangedRangesPreserveClaimSide(t *testing.T) {
	repository := t.TempDir()
	runTestGit(t, repository, "init", "-q")
	runTestGit(t, repository, "config", "user.name", "Review Eval Test")
	runTestGit(t, repository, "config", "user.email", "review-eval@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("removed\nkeep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "fixture.go")
	runTestGit(t, repository, "commit", "-qm", "base")
	base := runTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, "fixture.go"), []byte("keep\nadded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "commit", "-qam", "head")
	capture, err := capturePinnedRange(context.Background(), repository, base, runTestGit(t, repository, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	left := changedRangesFromNativeSide(capture.Paths, "left")
	right := changedRangesFromNativeSide(capture.Paths, "right")
	if len(left) != 1 || left[0] != (ChangedRange{Path: "fixture.go", StartLine: 1, EndLine: 1}) {
		t.Fatalf("left ranges = %#v", left)
	}
	if len(right) != 1 || right[0] != (ChangedRange{Path: "fixture.go", StartLine: 2, EndLine: 2}) {
		t.Fatalf("right ranges = %#v", right)
	}
}

func TestAACRRangeSemanticsReproduceUniqueMergeBase(t *testing.T) {
	repository, base, head := gitFixture(t)
	record := CandidatePoolRecord{
		ID: "candidate", CandidateSource: CandidateSourceAACR,
		SourceBaseSHA: base, BaseSHA: base, HeadSHA: head,
	}
	if err := verifyCandidateRangeSemanticsGit(context.Background(), repository, record); err != nil {
		t.Fatal(err)
	}
	claimDigest := strings.Repeat("a", 64)
	record.SourceRow.Claims = []CandidateClaim{{
		RawRecordSHA256: claimDigest, Path: "fixture.go", Side: "right", FromLine: 2, ToLine: 2,
	}}
	record.EligibleClaimSHA256s = []string{claimDigest}
	capture, err := capturePinnedRange(context.Background(), repository, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCandidateClaimsGit(context.Background(), repository, record, capture); err != nil {
		t.Fatal(err)
	}
	record.EligibleClaimSHA256s = nil
	if err := verifyCandidateClaimsGit(context.Background(), repository, record, capture); err == nil {
		t.Fatal("accepted eligible AACR claim digests that did not reproduce from native Git")
	}
	record.BaseSHA = head
	if err := verifyCandidateRangeSemanticsGit(context.Background(), repository, record); err == nil {
		t.Fatal("accepted a native base that was not the source-base/head merge base")
	}
}

func TestClaimCoordinatesRequireNativeSideBinding(t *testing.T) {
	sourceBase, mergeBase := shaFor(1), shaFor(2)
	if !claimCoordinatesMatchNativeRange("right", sourceBase, mergeBase) {
		t.Fatal("right-side head coordinates were rejected")
	}
	if claimCoordinatesMatchNativeRange("left", sourceBase, mergeBase) {
		t.Fatal("left-side source-base coordinates were treated as native merge-base coordinates")
	}
	if !claimCoordinatesMatchNativeRange("left", sourceBase, sourceBase) {
		t.Fatal("matching left-side merge-base coordinates were rejected")
	}
}

func TestCandidateAncestryIsCleanWarmOfflineDeterministicAndRecoversStaleMarkers(t *testing.T) {
	repository, base, head := gitFixture(t)
	row := CandidateSourceRow{Repository: "owner/repo", BaseSHA: base, HeadSHA: head}
	cacheRoot := t.TempDir()
	ctx := context.Background()
	ancestry, err := openCandidateAncestryRepository(ctx, cacheRoot, filepath.Join(repository, ".git"), row, false)
	if err != nil {
		t.Fatal(err)
	}
	initialState, err := candidateAncestryStateDigest(ctx, ancestry, base, head)
	if err != nil {
		t.Fatal(err)
	}
	stale := candidateFailureMarker{
		Schema: "review.eval-candidate-failure.v1", Algorithm: candidateAncestryAlgorithm, Stage: "merge-base",
		IdentitySHA256: digestBytes([]byte(base + "\x00" + head)), ObjectStateSHA256: strings.Repeat("0", 64),
		Failure: "bounded_exhausted", Deepen: 168,
	}
	if err := writeCanonicalJSON(filepath.Join(ancestry, "failure.json"), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMergeBase(ctx, ancestry, repository, base, head, true); err == nil {
		t.Fatal("stale marker allowed offline merge-base resolution")
	} else if _, ok := boundedAncestryDiscoveryBlocker(err); ok {
		t.Fatalf("stale marker was converted into a deterministic quarantine: %v", err)
	}
	stale.ObjectStateSHA256 = initialState
	stale.Failure = "arbitrary_blocker"
	if err := writeCanonicalJSON(filepath.Join(ancestry, "failure.json"), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMergeBase(ctx, ancestry, repository, base, head, true); err == nil {
		t.Fatal("non-derived marker allowed offline merge-base resolution")
	} else if _, ok := boundedAncestryDiscoveryBlocker(err); ok {
		t.Fatalf("non-derived marker was converted into a deterministic quarantine: %v", err)
	}
	if err := writeCandidateFailureMarker(ctx, ancestry, base, head, digestBytes([]byte(base+"\x00"+head)), "bounded_exhausted", 168); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMergeBase(ctx, ancestry, repository, base, head, true); err == nil {
		t.Fatal("current-state bounded marker was ignored")
	} else if blocker, ok := boundedAncestryDiscoveryBlocker(err); !ok || blocker.Code != "merge_base_bounded" {
		t.Fatalf("current-state bounded marker was not reproduced: %v", err)
	}
	ancestry, err = openCandidateAncestryRepository(ctx, cacheRoot, filepath.Join(repository, ".git"), row, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestry, "shallow"), []byte(base+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanBase, err := resolveMergeBase(ctx, ancestry, repository, base, head, true)
	if err != nil || cleanBase != base {
		t.Fatalf("clean isolated merge base = %s, %v", cleanBase, err)
	}
	ancestry, err = openCandidateAncestryRepository(ctx, cacheRoot, filepath.Join(repository, ".git"), row, false)
	if err != nil {
		t.Fatal(err)
	}
	warmResetState, err := candidateAncestryStateDigest(ctx, ancestry, base, head)
	if err != nil || warmResetState != initialState {
		t.Fatalf("warm online cache did not reconstruct the exact shallow boundary: %s, %v", warmResetState, err)
	}
	if err := os.WriteFile(filepath.Join(ancestry, "shallow"), []byte(base+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	warmBase, err := resolveMergeBase(ctx, ancestry, repository, base, head, true)
	if err != nil || warmBase != cleanBase {
		t.Fatalf("warm isolated merge base = %s, %v; clean = %s", warmBase, err, cleanBase)
	}
	offlineAncestry, err := openCandidateAncestryRepository(ctx, cacheRoot, filepath.Join(repository, ".git"), row, true)
	if err != nil {
		t.Fatal(err)
	}
	offlineBase, err := resolveMergeBase(ctx, offlineAncestry, repository, base, head, true)
	if err != nil || offlineBase != cleanBase {
		t.Fatalf("offline isolated merge base = %s, %v; clean = %s", offlineBase, err, cleanBase)
	}
}

func TestDiscoveryFailureClassificationRejectsOperationalErrors(t *testing.T) {
	operational := []error{
		context.Canceled,
		context.DeadlineExceeded,
		os.ErrNotExist,
		os.ErrPermission,
		ErrGitOutputLimit,
		errors.New("offline cache lacks hydration proof"),
		errors.New("Git capture failed"),
		errors.New("RPC failed; HTTP 502"),
	}
	for _, err := range operational {
		if blocker, ok := pinnedDiscoveryBlocker(err, "source_oid_unavailable", "source_hydration"); ok {
			t.Fatalf("%v classified as pinned quarantine blocker %#v", err, blocker)
		}
		if blocker, ok := boundedAncestryDiscoveryBlocker(err); ok {
			t.Fatalf("%v classified as bounded-ancestry blocker %#v", err, blocker)
		}
	}

	remote := pinnedRemoteVerificationError{err: errors.New("fatal: remote error: upload-pack: not our ref")}
	if blocker, ok := pinnedDiscoveryBlocker(remote, "source_oid_unavailable", "source_hydration"); !ok ||
		blocker != (MechanicalBlocker{Code: "source_oid_unavailable", Stage: "source_hydration"}) {
		t.Fatalf("proven remote rejection classification = %#v, %v", blocker, ok)
	}
	bounded := candidateMechanicalError{blocker: MechanicalBlocker{Code: "merge_base_bounded", Stage: "merge_base", Deepen: 168}}
	if blocker, ok := boundedAncestryDiscoveryBlocker(bounded); !ok || blocker != bounded.blocker {
		t.Fatalf("bounded ancestry classification = %#v, %v", blocker, ok)
	}
}

func TestGitPathExistsDistinguishesVerifiedAbsenceFromFailure(t *testing.T) {
	repository, base, _ := gitFixture(t)
	if exists, err := gitPathExists(context.Background(), repository, base, "fixture.go"); err != nil || !exists {
		t.Fatalf("existing path = %v, %v", exists, err)
	}
	if exists, err := gitPathExists(context.Background(), repository, base, "missing.go"); err != nil || exists {
		t.Fatalf("absent path = %v, %v", exists, err)
	}
	if _, err := gitPathExists(context.Background(), repository, base, "bad\x00path"); err == nil {
		t.Fatal("invalid path was treated as verified absence")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gitPathExists(cancelled, repository, base, "fixture.go"); err == nil {
		t.Fatal("context cancellation was treated as verified absence")
	}
	if _, err := gitPathExists(context.Background(), filepath.Join(repository, "missing"), base, "fixture.go"); err == nil {
		t.Fatal("Git/filesystem failure was treated as verified absence")
	}
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

func TestImportAACRSourceRowsGroupsEveryAcceptedClaim(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	source := func(path, note string, line, label int) map[string]any {
		return map[string]any{
			"project_main_language": "TypeScript",
			"pr_url":                "https://github.com/owner/repo/pull/17",
			"pr_source_commit":      base,
			"pr_target_commit":      head,
			"pr_change_line_count":  450,
			"label":                 label,
			"path":                  path,
			"side":                  "right",
			"from_line":             line,
			"to_line":               line,
			"category":              "Code Defect",
			"context":               "File Level",
			"note":                  note,
			"is_ai_comment":         false,
		}
	}
	rawRows := []map[string]any{
		source("first.ts", "first accepted claim", 11, 1),
		source("second.ts", "second accepted claim", 22, 1),
		source("ignored.ts", "rejected claim", 33, 0),
	}
	payload, err := json.Marshal(rawRows)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "aacr.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ImportAACRSourceRows(path, "Apache-2.0", strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Claims) != 2 || len(rows[0].RawRecordSHA256s) != 2 {
		t.Fatalf("grouped rows = %#v", rows)
	}
	if rows[0].BaseSHA != base || rows[0].HeadSHA != head {
		t.Fatalf("AACR source/target direction = %s..%s, want %s..%s", rows[0].BaseSHA, rows[0].HeadSHA, base, head)
	}
	rejections := CandidateRejectionsManifest{Rejections: []CandidateRejection{{
		CaseID: aacrCaseID(rows[0]), Repository: rows[0].Repository,
		SourceBaseSHA: base, SourceHeadSHA: head, RawSourceRecordSHA256: rows[0].RawRecordSHA256s[0],
	}}}
	if err := ValidateCandidateRejectionsAgainstAACR(rejections, rows); err != nil {
		t.Fatal(err)
	}
	rejections.Rejections[0].SourceBaseSHA, rejections.Rejections[0].SourceHeadSHA = head, base
	if err := ValidateCandidateRejectionsAgainstAACR(rejections, rows); err == nil {
		t.Fatal("reversed AACR source identity was accepted")
	}
	rejections.Rejections[0].SourceBaseSHA, rejections.Rejections[0].SourceHeadSHA = base, head
	rejections.Rejections[0].RawSourceRecordSHA256 = strings.Repeat("f", 64)
	if err := ValidateCandidateRejectionsAgainstAACR(rejections, rows); err == nil {
		t.Fatal("unbound AACR raw source digest was accepted")
	}
	notes := map[string]bool{}
	for index, claim := range rows[0].Claims {
		notes[claim.Note] = true
		if claim.RawRecordSHA256 != rows[0].RawRecordSHA256s[index] {
			t.Fatal("claim is not bound to its raw source-row digest")
		}
	}
	if !notes["first accepted claim"] || !notes["second accepted claim"] {
		t.Fatalf("accepted AACR claims were overwritten: %#v", rows[0].Claims)
	}
	if _, err := CanonicalCandidateSourceRowDigest(rows[0]); err != nil {
		t.Fatal(err)
	}
}

func TestAACRCaseCannotLeaveAuditQueueWithoutNativeAndClaimValidation(t *testing.T) {
	record := testCandidateRecord(91, CandidateSourceAACR, SourceHumanCaught)
	claim := record.SourceRow.Claims[0]
	truth := GroundTruth{
		ID: "aacr-truth", DefectClass: "local_logic", Severity: "major",
		Summary: "reviewer-confirmed issue", Scenario: "the changed behavior violates the reviewed contract",
		Verifier: "independent_semantic_review",
		Provenance: &GroundTruthProvenance{
			SourceID: record.SourceID, SourceRecordID: record.SourceRecordID,
			SourceDigest: claim.RawRecordSHA256, EvidenceRefs: []string{"fixture:semantic-review"},
		},
		Locations: []Location{{Path: claim.Path, StartLine: claim.FromLine, EndLine: claim.ToLine}},
	}
	truth.CanonicalDigest = groundTruthDigest(truth)
	candidate := PreparedCase{
		ID: record.ID, SourceRowSHA256: record.SourceRowSHA256, CandidateSource: record.CandidateSource,
		SourceID: record.SourceID, SourceRecordID: record.SourceRecordID, Repository: record.Repository,
		BaseSHA: record.BaseSHA, BaseFetch: record.BaseFetch, HeadSHA: record.HeadSHA, HeadFetch: record.HeadFetch,
		RawAdditions: record.RawAdditions, RawDeletions: record.RawDeletions, Language: record.Language,
		SizeBin: record.SizeBin, DefectCategory: "local_logic", SourceType: record.SourceType,
		StructuredRequired: true, GroundTruth: []GroundTruth{truth}, ChangedRanges: record.ChangedRanges,
		SelectionKey: record.SelectionKey, PatchSHA256: record.PatchSHA256,
		BuildStateSHA256: record.BuildStateSHA256,
	}
	candidate = approvedCase(t, candidate)
	if !preparedCaseMatchesRecord(candidate, record) {
		t.Fatal("fully hydrated and independently audited AACR case was rejected")
	}
	unhydrated := candidate
	unhydrated.RawAdditions = 0
	if preparedCaseMatchesRecord(unhydrated, record) {
		t.Fatal("AACR case without exact native churn was approved")
	}
	wrongClaim := candidate
	wrongClaim.GroundTruth = append([]GroundTruth(nil), candidate.GroundTruth...)
	wrongClaim.GroundTruth[0].Locations = []Location{{Path: "other.go", StartLine: 10, EndLine: 10}}
	wrongClaim.GroundTruth[0].CanonicalDigest = groundTruthDigest(wrongClaim.GroundTruth[0])
	if preparedCaseMatchesRecord(wrongClaim, record) {
		t.Fatal("AACR case with a relocated source claim was approved")
	}
	ineligibleRecord := record
	ineligibleClaim := claim
	ineligibleClaim.RawRecordSHA256 = strings.Repeat("c", 64)
	ineligibleRecord.SourceRow.Claims = append(ineligibleRecord.SourceRow.Claims, ineligibleClaim)
	ineligible := candidate
	ineligible.GroundTruth = append([]GroundTruth(nil), candidate.GroundTruth...)
	ineligible.GroundTruth[0].Provenance = &GroundTruthProvenance{
		SourceID: record.SourceID, SourceRecordID: record.SourceRecordID,
		SourceDigest: ineligibleClaim.RawRecordSHA256, EvidenceRefs: []string{"fixture:semantic-review"},
	}
	if validateSourceClaimBinding(ineligible, ineligibleRecord) == nil {
		t.Fatal("AACR approval accepted a human claim that failed native mechanical qualification")
	}
	unaudited := candidate
	unaudited.Audit = AuditProvenance{}
	if preparedCaseMatchesRecord(unaudited, record) {
		t.Fatal("AACR case without semantic audit was approved")
	}
}

func TestCandidatePoolRejectsUnhydratedLargeRecord(t *testing.T) {
	record := testCandidateRecord(93, CandidateSourceAACR, SourceHumanCaught)
	record.RawAdditions = 0
	record.ChangedRanges = nil
	record.PatchSHA256 = ""
	record.BuildStateSHA256 = ""
	if _, err := CanonicalCandidatePoolDigest(candidatePoolEnvelope([]CandidatePoolRecord{record})); err == nil {
		t.Fatal("unhydrated large candidate entered the canonical pool")
	}
	record = testCandidateRecord(93, CandidateSourceAACR, SourceHumanCaught)
	record.SourceBaseFetch = GitFetchSpec{}
	if _, err := CanonicalCandidatePoolDigest(candidatePoolEnvelope([]CandidatePoolRecord{record})); err == nil {
		t.Fatal("AACR candidate without immutable source-base fetch metadata entered the canonical pool")
	}
	record = testCandidateRecord(93, CandidateSourceAACR, SourceHumanCaught)
	record.EligibleClaimSHA256s = nil
	if _, err := CanonicalCandidatePoolDigest(candidatePoolEnvelope([]CandidatePoolRecord{record})); err == nil {
		t.Fatal("AACR candidate without a mechanically eligible human claim entered the canonical pool")
	}
}

func TestRejectedAACRIdentityCannotReenterCandidatePool(t *testing.T) {
	record := testCandidateRecord(92, CandidateSourceAACR, SourceHumanCaught)
	rejections := CandidateRejectionsManifest{Rejections: []CandidateRejection{{
		CaseID: record.ID, Repository: record.Repository,
		SourceBaseSHA: record.SourceRow.BaseSHA, SourceHeadSHA: record.SourceRow.HeadSHA,
	}}}
	pool := CandidatePoolManifest{Records: []CandidatePoolRecord{record}}
	if err := ValidateCandidatePoolRejections(pool, rejections); err == nil {
		t.Fatal("rejected immutable AACR identity silently re-entered candidate pool")
	}
	record.ID = "relabeled-candidate"
	pool.Records[0] = record
	if err := ValidateCandidatePoolRejections(pool, rejections); err == nil {
		t.Fatal("relabeled rejected immutable AACR identity silently re-entered candidate pool")
	}
}
func TestCandidatePoolDigestCommitsCanonicalEnvelope(t *testing.T) {
	pool := candidatePoolEnvelope([]CandidatePoolRecord{testCandidateRecord(94, CandidateSourceAACR, SourceHumanCaught)})
	digest, err := CanonicalCandidatePoolDigest(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.SHA256 = digest
	if err := ValidateCandidatePool(pool); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CandidatePoolManifest){
		"schema":         func(value *CandidatePoolManifest) { value.Schema = "review.eval-candidate-pool.v0" },
		"sources_digest": func(value *CandidatePoolManifest) { value.SourcesManifestSHA256 = strings.Repeat("b", 64) },
		"partition_seed": func(value *CandidatePoolManifest) { value.PartitionSeedSHA256 = strings.Repeat("8", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := pool
			mutate(&tampered)
			if err := ValidateCandidatePool(tampered); err == nil {
				t.Fatal("candidate pool accepted envelope mutation under the old digest")
			}
		})
	}
}

func TestFrozenRejectionsContainAllIndependentlyRejectedAACRCases(t *testing.T) {
	manifest, err := LoadCandidateRejections(filepath.Join("..", "..", "testdata", "review-eval", "candidate_rejections.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"aacr-langflow-ai-langflow-6652",
		"aacr-bluewave-labs-checkmate-2883",
		"aacr-comfyanonymous-comfyui-7952",
		"aacr-elastic-elasticsearch-119759",
		"aacr-facebook-react-32499",
		"aacr-freecad-freecad-20825",
		"aacr-clickhouse-clickhouse-74070",
		"aacr-langflow-ai-langflow-6044",
		"aacr-immich-app-immich-17535",
		"aacr-elastic-elasticsearch-118183",
		"aacr-wavetermdev-waveterm-1998",
		"aacr-n8n-io-n8n-20210",
		"aacr-freecad-freecad-21257",
		"aacr-cherryhq-cherry-studio-5540",
		"aacr-gofr-dev-gofr-1395",
		"aacr-sveltejs-svelte-16232",
		"aacr-cherryhq-cherry-studio-5637",
		"aacr-google-gemini-gemini-cli-4828",
		"aacr-valkey-io-valkey-1485",
		"aacr-microsoft-typescript-go-831",
		"aacr-freecad-freecad-19426",
		"aacr-elastic-elasticsearch-126376",
		"aacr-clickhouse-clickhouse-82441",
		"aacr-laravel-framework-54226",
		"aacr-google-gemini-gemini-cli-8154",
		"aacr-immich-app-immich-14777",
		"aacr-kestra-io-kestra-7191",
		"aacr-gofr-dev-gofr-1355",
	}
	got := map[string]bool{}
	legacyDigests, rawDigests := 0, 0
	for _, rejection := range manifest.Rejections {
		got[rejection.CaseID] = true
		if rejection.LegacySourceRowSHA256 != "" {
			legacyDigests++
		}
		if rejection.RawSourceRecordSHA256 != "" {
			rawDigests++
		}
	}
	wantEvidence := map[string][2]string{
		"a1": {"2bd13e61617c9d895271a57cf6e74d77f053c0ccec3df55704718ecb85200259", "1b524c92eeff1cd719217af566d7f2e74f802c31d94426a36e9c3d545c91c6da"},
		"a2": {"d480f6074996ef7fbd1464e7036595def31d94de945eae25f494c1f9aa4eefac", "8d70db488ef30e481d80ca6f11dc5b75265b7805a44eaba48e7f87ad32eec867"},
	}
	if legacyDigests != 14 || rawDigests != 14 || len(manifest.Evidence) != 2 {
		t.Fatalf("rejection evidence bindings: legacy=%d raw=%d evidence=%d", legacyDigests, rawDigests, len(manifest.Evidence))
	}
	for _, spec := range manifest.Evidence {
		expected, ok := wantEvidence[spec.ID]
		var evidence CandidateRejectionEvidence
		var original []CandidateRejectionSourceRecord
		originalPath := filepath.Join("..", "..", "testdata", "review-eval", "rejection_evidence", "prowl-corpus-"+spec.ID+".json")
		originalPayload, readErr := os.ReadFile(originalPath)
		if !ok || spec.SHA256 != expected[0] || spec.SourceSHA256 != expected[1] ||
			spec.SourcePath != filepath.ToSlash(filepath.Join("rejection_evidence", "prowl-corpus-"+spec.ID+".json")) ||
			readErr != nil || digestBytes(originalPayload) != expected[1] ||
			decodeStrictFile(originalPath, &original) != nil || len(original) == 0 ||
			decodeStrictFile(filepath.Join("..", "..", "testdata", "review-eval", spec.Path), &evidence) != nil ||
			evidence.SourcePayloadSHA256 != expected[1] {
			t.Fatalf("rejection evidence %s does not preserve and strictly decode its exact source payload", spec.ID)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("frozen rejection count = %d, want %d", len(got), len(want))
	}
	for _, caseID := range want {
		if !got[caseID] {
			t.Fatalf("independently rejected case %s is not frozen", caseID)
		}
	}
	tampered := manifest
	tampered.Rejections = append([]CandidateRejection(nil), manifest.Rejections...)
	tampered.Rejections[0].Blocker = "digest merely listed without its exact evidence record"
	if err := ValidateCandidateRejections(tampered); err == nil {
		t.Fatal("rejection decision fields were accepted without exact evidence-record equality")
	}
}

func TestOriginalRejectionEvidenceRejectsHashAndStrictDecodeDrift(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "review-eval")
	manifest, err := LoadCandidateRejections(filepath.Join(root, "candidate_rejections.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec := manifest.Evidence[0]
	var evidence CandidateRejectionEvidence
	if err := decodeStrictFile(filepath.Join(root, spec.Path), &evidence); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, spec.SourcePath)
	payload, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "tampered.json")
	if err := os.WriteFile(tamperedPath, append(append([]byte(nil), payload...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateCandidateRejectionSource(tamperedPath, spec, evidence); err == nil {
		t.Fatal("source payload byte drift escaped its manifest hash")
	}

	var records []map[string]any
	if err := json.Unmarshal(payload, &records); err != nil {
		t.Fatal(err)
	}
	records[0]["unexpected"] = true
	unknown, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	unknown = append(unknown, '\n')
	if err := os.WriteFile(tamperedPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	spec.SourceSHA256 = digestBytes(unknown)
	evidence.SourcePayloadSHA256 = spec.SourceSHA256
	if err := validateCandidateRejectionSource(tamperedPath, spec, evidence); err == nil {
		t.Fatal("unknown original evidence field escaped strict decoding")
	}
}

func TestFrozenCandidateArtifactsExcludeRejectedIdentities(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "review-eval")
	rejections, err := LoadCandidateRejections(filepath.Join(root, "candidate_rejections.json"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := LoadCandidatePool(filepath.Join(root, "candidate_pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCandidatePoolRejections(pool, rejections); err != nil {
		t.Fatal(err)
	}
	freeze, err := LoadScoringFreeze(filepath.Join(root, "scoring.json"))
	if err != nil {
		t.Fatal(err)
	}
	if freeze.CandidatePoolSHA256 != pool.SHA256 {
		t.Fatal("scoring freeze does not bind the canonical candidate-pool envelope digest")
	}
	rejectedIDs := map[string]bool{}
	for _, rejection := range rejections.Rejections {
		rejectedIDs[rejection.CaseID] = true
	}
	for _, caseID := range append(append([]string(nil), freeze.Large.TuningBaseCaseIDs...), freeze.Large.HeldOutBaseCaseIDs...) {
		if rejectedIDs[caseID] {
			t.Fatalf("scoring freeze still references rejected case %s", caseID)
		}
	}
}
func TestAuditPacketsAreCanonicalAndFullyBoundToCurrentArtifacts(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "review-eval")
	packets, err := LoadAuditPackets(filepath.Join(root, "audit_packets.json"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := LoadCandidatePool(filepath.Join(root, "candidate_pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	freeze, err := LoadScoringFreeze(filepath.Join(root, "scoring.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(freeze.FreezeBlockers) == 0 {
		t.Fatal("checked scoring fixture must retain honest readiness blockers")
	}
	if err := ValidateScoringFreezeIntegrity(freeze, pool.SourcesManifestSHA256, pool.SHA256); err != nil {
		t.Fatalf("honest readiness blockers masked scoring integrity: %v", err)
	}
	rejections, err := LoadCandidateRejections(filepath.Join(root, "candidate_rejections.json"))
	if err != nil {
		t.Fatal(err)
	}
	tuning, err := LoadFrozenCorpus(filepath.Join(root, "tuning.json"))
	if err != nil {
		t.Fatal(err)
	}
	heldOut, err := LoadFrozenCorpus(filepath.Join(root, "held_out.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]CandidateSourceRow, 0, len(packets.Packets))
	for _, packet := range packets.Packets {
		rows = append(rows, packet.SourceRow)
	}
	if err := ValidateAuditPackets(packets, rows, rejections, pool, tuning, heldOut); err != nil {
		t.Fatal(err)
	}
	tampered := packets
	tampered.Packets = append([]CandidateAuditPacket(nil), packets.Packets...)
	tampered.Packets[0].Blockers = []MechanicalBlocker{{Code: "tampered", Stage: "tampered"}}
	tampered.CanonicalSHA256, err = CanonicalAuditPacketsDigest(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuditPackets(tampered, rows, rejections, pool, tuning, heldOut); err == nil {
		t.Fatal("self-consistent packet mutation escaped cross-artifact validation")
	}
	compact, err := json.Marshal(packets)
	if err != nil {
		t.Fatal(err)
	}
	noncanonicalPath := filepath.Join(t.TempDir(), "audit_packets.json")
	if err := os.WriteFile(noncanonicalPath, compact, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuditPackets(noncanonicalPath); err == nil {
		t.Fatal("noncanonical checked-in packet bytes were accepted")
	}
}
func TestRegeneratedCandidateArtifactsRejectEveryMechanicalDrift(t *testing.T) {
	record := testCandidateRecord(91, CandidateSourceAACR, SourceHumanCaught)
	expectedPool := candidatePoolEnvelope([]CandidatePoolRecord{record})
	poolDigest, err := CanonicalCandidatePoolDigest(expectedPool)
	if err != nil {
		t.Fatal(err)
	}
	expectedPool.SHA256 = poolDigest
	freeze, _, _, _ := validScoringFixture()
	freeze.SourcesManifestSHA256 = expectedPool.SourcesManifestSHA256
	freeze.CandidatePoolSHA256 = expectedPool.SHA256
	freeze.FreezeBlockers = []string{"held-out deficit"}
	if err := ValidateScoringFreezeIntegrity(freeze, expectedPool.SourcesManifestSHA256, expectedPool.SHA256); err != nil {
		t.Fatalf("honest readiness blocker failed scoring integrity: %v", err)
	}
	expectedPacket := CandidateAuditPacket{
		CaseID: record.ID, Disposition: "audit_required", NativeBaseSHA: record.BaseSHA,
		Claims: []ClaimAudit{{
			CandidateClaim: CandidateClaim{RawRecordSHA256: strings.Repeat("1", 64)},
			PathExists:     true, InChangedRange: true, SemanticDecision: "unresolved",
		}},
		Blockers: []MechanicalBlocker{{Code: "semantic_audit_required", Stage: "semantic_audit", Detail: "human review"}},
	}
	expectedPackets := AuditPacketsManifest{Packets: []CandidateAuditPacket{expectedPacket}}
	clonePackets := func() AuditPacketsManifest {
		clone := expectedPackets
		clone.Packets = append([]CandidateAuditPacket(nil), expectedPackets.Packets...)
		clone.Packets[0].Claims = append([]ClaimAudit(nil), expectedPackets.Packets[0].Claims...)
		clone.Packets[0].Blockers = append([]MechanicalBlocker(nil), expectedPackets.Packets[0].Blockers...)
		return clone
	}

	t.Run("removed qualified row", func(t *testing.T) {
		actualPool := expectedPool
		actualPool.Records = nil
		if err := compareRegeneratedCandidateArtifacts(expectedPool, actualPool, expectedPackets, expectedPackets); err == nil {
			t.Fatal("removed mechanically qualified row escaped regeneration comparison")
		}
	})
	t.Run("forged quarantine", func(t *testing.T) {
		actual := clonePackets()
		actual.Packets[0].Disposition = "quarantined"
		actual.Packets[0].QueueItem = nil
		actual.Packets[0].Blockers = []MechanicalBlocker{{Code: "no_human_claim", Stage: "source_claims"}}
		if err := compareRegeneratedCandidateArtifacts(expectedPool, expectedPool, expectedPackets, actual); err == nil {
			t.Fatal("forged quarantine escaped regeneration comparison")
		}
	})
	t.Run("claim fact drift", func(t *testing.T) {
		actual := clonePackets()
		actual.Packets[0].Claims[0].PathExists = false
		if err := compareRegeneratedCandidateArtifacts(expectedPool, expectedPool, expectedPackets, actual); err == nil {
			t.Fatal("claim boolean drift escaped regeneration comparison")
		}
	})
	t.Run("claim blocker drift", func(t *testing.T) {
		actual := clonePackets()
		actual.Packets[0].Claims[0].Blockers = []MechanicalBlocker{{Code: "claim_path_absent", Stage: "claim_binding"}}
		if err := compareRegeneratedCandidateArtifacts(expectedPool, expectedPool, expectedPackets, actual); err == nil {
			t.Fatal("claim blocker drift escaped regeneration comparison")
		}
	})
}

func TestAuditApprovalRequiresPersistedCanonicalEvidenceArtifacts(t *testing.T) {
	base := auditTestCase()
	approved := approvedCase(t, base)
	if !auditApproved(approved) {
		t.Fatal("two persisted exact reviewer outputs were rejected")
	}
	if evidence, ok := approved.auditEvidence["evidence-1"]; !ok ||
		evidence.Output.Schema != AuditEvidenceOutputSchema ||
		evidence.Output.ActorID != "reviewer-one" ||
		evidence.Output.Decision != "approved" {
		t.Fatal("loaded case discarded or altered persisted reviewer output")
	}
	inMemory := base
	inMemory.Audit = AuditProvenance{Decisions: []AuditDecision{
		{Slot: "reviewer_1", EvidenceID: "evidence-1"},
		{Slot: "reviewer_2", EvidenceID: "evidence-2"},
	}}
	if auditApproved(inMemory) {
		t.Fatal("in-memory audit references bypassed persisted evidence loading")
	}
	disagreement := approvedAuditConfig()
	disagreement.Decisions[1] = "rejected"
	if _, err := persistAuditCase(t, base, disagreement); err == nil {
		t.Fatal("reviewer disagreement without persisted adjudication was accepted")
	}
	disagreement.AdjudicatorID, disagreement.AdjudicatorDecision = "reviewer-three", "approved"
	if resolved, err := persistAuditCase(t, base, disagreement); err != nil || !auditApproved(resolved) {
		t.Fatalf("persisted independent adjudication was rejected: %v", err)
	}
	unnecessary := approvedAuditConfig()
	unnecessary.AdjudicatorID, unnecessary.AdjudicatorDecision = "reviewer-three", "approved"
	if _, err := persistAuditCase(t, base, unnecessary); err == nil {
		t.Fatal("unnecessary adjudication output was accepted")
	}
	boundBase := base
	boundBase.GroundTruth = []GroundTruth{canonicalGroundTruth("audit-finding", "audit-source")}
	for name, mutate := range map[string]func(*AuditEvidenceRecord){
		"case":     func(record *AuditEvidenceRecord) { record.CaseID = "other-case" },
		"source":   func(record *AuditEvidenceRecord) { record.SourceRowSHA256 = strings.Repeat("f", 64) },
		"patch":    func(record *AuditEvidenceRecord) { record.PatchSHA256 = strings.Repeat("f", 64) },
		"build":    func(record *AuditEvidenceRecord) { record.BuildStateSHA256 = strings.Repeat("f", 64) },
		"actor":    func(record *AuditEvidenceRecord) { record.ActorID = "other-reviewer" },
		"decision": func(record *AuditEvidenceRecord) { record.Decision = "rejected" },
		"findings": func(record *AuditEvidenceRecord) { record.Findings = nil },
	} {
		t.Run("output metadata "+name, func(t *testing.T) {
			mismatched := approvedAuditConfig()
			mismatched.MutateRecord = func(index int, record *AuditEvidenceRecord) {
				if index == 0 {
					mutate(record)
				}
			}
			if _, err := persistAuditCase(t, boundBase, mismatched); err == nil {
				t.Fatalf("%s mismatch between output and metadata was accepted", name)
			}
		})
	}
	missing := approvedAuditConfig()
	missing.DeleteOutput = 1
	if _, err := persistAuditCase(t, base, missing); err == nil {
		t.Fatal("missing reviewer output was accepted")
	}
	corrupt := approvedAuditConfig()
	corrupt.CorruptOutputAfterHash = 1
	if _, err := persistAuditCase(t, base, corrupt); err == nil {
		t.Fatal("reviewer output with a mismatched byte hash was accepted")
	}
	noncanonical := approvedAuditConfig()
	noncanonical.MutateOutputPayload = func(index int, payload []byte) []byte {
		if index == 0 {
			return append(payload, '\n')
		}
		return payload
	}
	if _, err := persistAuditCase(t, base, noncanonical); err == nil {
		t.Fatal("noncanonical reviewer output bytes were accepted")
	}
	unknownField := approvedAuditConfig()
	unknownField.MutateOutputPayload = func(index int, payload []byte) []byte {
		if index != 0 {
			return payload
		}
		var record map[string]any
		if err := json.Unmarshal(payload, &record); err != nil {
			t.Fatal(err)
		}
		record["unexpected"] = true
		encoded, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(encoded, '\n')
	}
	if _, err := persistAuditCase(t, base, unknownField); err == nil {
		t.Fatal("reviewer output with unknown fields was accepted")
	}
	extra := approvedAuditConfig()
	extra.ExtraOutput = true
	if _, err := persistAuditCase(t, base, extra); err == nil {
		t.Fatal("unconsumed audit output was accepted")
	}
}

func TestVerifyPreparedCaseGitRejectsEveryFrozenGitClaim(t *testing.T) {
	repository, base, head := gitFixture(t)
	candidate := PreparedCase{ID: "git-case", SourceID: "fixture", SourceRecordID: "fixture", Repository: "owner/repo", BaseSHA: base, HeadSHA: head}
	hydrated, err := HydrateGitCase(context.Background(), repository, candidate)
	if err != nil {
		t.Fatal(err)
	}
	freeze, _, _, _ := validScoringFixture()
	freeze.FreezeBlockers = []string{"held-out deficit"}
	if err := ValidateScoringFreezeIntegrity(freeze, freeze.SourcesManifestSHA256, freeze.CandidatePoolSHA256); err != nil {
		t.Fatalf("honest readiness blocker failed scoring integrity: %v", err)
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
	pool := candidatePoolEnvelope(records)
	digest, err := CanonicalCandidatePoolDigest(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.SHA256 = digest
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
	padding := cleanPaddingCase(t)
	variants := []PreparedCase{metamorphicCase("begin", "beginning"), metamorphicCase("middle", "middle"), metamorphicCase("end", "end")}
	triple := testPreparedTriple(variants, []string{"padding"})
	cases := append(variants, padding)
	if err := ValidatePreparedTriples(cases, []PreparedTriple{triple}); err != nil {
		t.Fatal(err)
	}
	cases[3].Audit.Decisions[0].EvidenceID = "missing-review-output"
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
	pool := candidatePoolEnvelope(records)
	digest, err := CanonicalCandidatePoolDigest(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.SHA256 = digest
	tuningRecords, heldRecords, err := PartitionCandidatePool(pool, 12, 30)
	if err != nil {
		t.Fatal(err)
	}
	tuning := FrozenCorpus{
		Schema: CorpusSchema, Set: "tuning", CandidatePoolSHA256: digest,
		AuditQueue: queueFromCandidateRecords(tuningRecords),
	}
	held := FrozenCorpus{
		Schema: CorpusSchema, Set: "held_out", CandidatePoolSHA256: digest,
		AuditQueue: queueFromCandidateRecords(heldRecords),
	}
	small := FrozenCorpus{Schema: SmallCorpusSchema, Set: "small_non_regression"}
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err != nil {
		t.Fatal(err)
	}
	tuning.AuditQueue[0].SourceID = "tampered-source"
	if err := ValidateCorpusPartitions(tuning, held, small, pool); err == nil {
		t.Fatal("queue case with noncanonical source binding was accepted")
	}
}

func TestMetamorphicPaddingUsesPortablePatchesNotEqualTrees(t *testing.T) {
	before, after := cleanPaddingCase(t), cleanPaddingCase(t)
	before.ID, before.SourceRecordID, before.BuildStateSHA256, before.PatchSHA256 = "padding-before", "padding-before", strings.Repeat("e", 64), strings.Repeat("1", 64)
	after.ID, after.SourceRecordID, after.BuildStateSHA256, after.PatchSHA256 = "padding-after", "padding-after", strings.Repeat("f", 64), strings.Repeat("2", 64)
	for _, padding := range []*PreparedCase{&before, &after} {
		padding.VerificationStateSHA256 = testVerificationState()
		*padding = approvedCase(t, *padding)
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
		hydrated.SourceRowSHA256 = digestBytes([]byte("source-row:" + id))
		hydrated = approvedCase(t, hydrated)
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
		{RemoteURL: valid.RemoteURL, Ref: strings.Repeat("b", 40), OID: valid.OID},
	} {
		if err := ValidateGitFetchSpec(invalid); err == nil {
			t.Fatalf("unsafe Git fetch metadata accepted: %#v", invalid)
		}
	}
}

func TestAuditRequiresImmutableDistinctReviewerAndAdjudicatorIdentities(t *testing.T) {
	base := auditTestCase()
	if approved := approvedCase(t, base); !auditApproved(approved) {
		t.Fatal("valid independent audit rejected")
	}
	duplicate := approvedAuditConfig()
	duplicate.ReviewerIDs[1] = duplicate.ReviewerIDs[0]
	if _, err := persistAuditCase(t, base, duplicate); err == nil {
		t.Fatal("duplicate reviewer identity accepted")
	}
	selfAdjudicated := approvedAuditConfig()
	selfAdjudicated.Decisions[1] = "rejected"
	selfAdjudicated.AdjudicatorID = selfAdjudicated.ReviewerIDs[0]
	selfAdjudicated.AdjudicatorDecision = "approved"
	if _, err := persistAuditCase(t, base, selfAdjudicated); err == nil {
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
	rawDigest := digestBytes([]byte(fmt.Sprintf("raw:%d", index)))
	row := CandidateSourceRow{
		CandidateSource: source, SourceID: sourceID, SourceRecordID: recordID, Repository: repository,
		PullRequestURL: pullRequestURL,
		BaseSHA:        base, HeadSHA: head, License: "MIT", Provenance: strings.Repeat("f", 40), Language: "Go",
		EvidenceRefs: evidence, RawRecordSHA256: rawDigest,
	}
	if source == CandidateSourceAACR {
		row.RawRecordSHA256 = ""
		row.RawRecordSHA256s = []string{rawDigest}
		row.Claims = []CandidateClaim{{
			RawRecordSHA256: rawDigest, SourceOrdinal: 1, Path: "defect.go", Side: "right", FromLine: 10, ToLine: 10,
			Category: "Code Defect", Context: "File Level", Note: "fixture claim",
		}}
	}
	rowDigest, err := CanonicalCandidateSourceRowDigest(row)
	if err != nil {
		panic(err)
	}
	record := CandidatePoolRecord{
		ID: fmt.Sprintf("candidate-%02d", index), CandidateSource: source,
		SourceID: sourceID, SourceRecordID: recordID, SourceType: sourceType,
		Repository: repository, BaseSHA: base, HeadSHA: head,
		SelectionKey: digestBytes([]byte(repository + "\x00" + base + "\x00" + head)),
		Language:     "Go", RawAdditions: 350, SizeBin: Size301To1000,
		ChangedRanges: []ChangedRange{{Path: "defect.go", StartLine: 10, EndLine: 359}},
		PatchSHA256:   strings.Repeat("1", 64), BuildStateSHA256: strings.Repeat("2", 64),
		EvidenceRefs: evidence, SourceRow: row, SourceRowSHA256: rowDigest,
		BaseFetch: GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: base, OID: base},
		HeadFetch: GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: fmt.Sprintf("refs/pull/%d/head", index+1), OID: head},
	}
	if source == CandidateSourceAACR {
		record.SourceBaseSHA = base
		record.SourceBaseFetch = GitFetchSpec{RemoteURL: "https://github.com/" + repository + ".git", Ref: base, OID: base}
		record.EligibleClaimSHA256s = []string{row.Claims[0].RawRecordSHA256}
		record.RangeSemantics = "github_merge_base_to_head"
	}
	return record
}
func candidatePoolEnvelope(records []CandidatePoolRecord) CandidatePoolManifest {
	return CandidatePoolManifest{
		Schema: CandidatePoolSchema, SourcesManifestSHA256: strings.Repeat("a", 64),
		PartitionSeedSHA256: strings.Repeat("9", 64), Records: records,
	}
}

func queueFromCandidateRecords(records []CandidatePoolRecord) []AuditQueueItem {
	queue := make([]AuditQueueItem, 0, len(records))
	for _, record := range records {
		queue = append(queue, AuditQueueItem{
			CaseID: record.ID, CandidateSource: record.CandidateSource, SourceID: record.SourceID,
			SourceRecordID: record.SourceRecordID, SourceType: record.SourceType, Repository: record.Repository,
			SourceBaseSHA: record.SourceBaseSHA, SourceBaseFetch: record.SourceBaseFetch,
			EligibleClaimSHA256s: append([]string(nil), record.EligibleClaimSHA256s...),
			BaseSHA:              record.BaseSHA, HeadSHA: record.HeadSHA,
			RangeSemantics: record.RangeSemantics, SelectionKey: record.SelectionKey, Language: record.Language,
			SourceReportedChurn: record.SourceReportedChurn, RawAdditions: record.RawAdditions, RawDeletions: record.RawDeletions,
			SizeBin: record.SizeBin, ChangedRanges: append([]ChangedRange(nil), record.ChangedRanges...),
			PatchSHA256: record.PatchSHA256, BuildStateSHA256: record.BuildStateSHA256,
			SourceRowSHA256: record.SourceRowSHA256, EvidenceRefs: append([]string(nil), record.EvidenceRefs...),
			BaseFetch: record.BaseFetch, HeadFetch: record.HeadFetch,
		})
	}
	return queue
}

func TestPinnedHydrationProofCoversCleanWarmDeletedRefAndManualSeed(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	cache := filepath.Join(root, "cache.git")
	manual := filepath.Join(root, "manual.git")
	for _, repository := range []string{remote, cache, manual} {
		if err := os.MkdirAll(repository, 0o700); err != nil {
			t.Fatal(err)
		}
		runTestGit(t, repository, "init", "--bare", ".")
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, work, "init", ".")
	runTestGit(t, work, "config", "user.name", "Review Eval")
	runTestGit(t, work, "config", "user.email", "review-eval@example.invalid")
	if err := os.WriteFile(filepath.Join(work, "fixture.txt"), []byte("pinned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, work, "add", "fixture.txt")
	runTestGit(t, work, "commit", "-m", "pinned")
	oid := runTestGit(t, work, "rev-parse", "HEAD")
	runTestGit(t, work, "remote", "add", "origin", remote)
	runTestGit(t, work, "push", "origin", "HEAD:refs/heads/pinned")
	spec := GitFetchSpec{RemoteURL: remote, Ref: "refs/heads/pinned", OID: oid}
	ctx := context.Background()
	localFetch := func(ctx context.Context, repository string, spec GitFetchSpec) error {
		command := exec.CommandContext(ctx, "git", "fetch", "--no-tags", "--force", "--depth=1", spec.RemoteURL, spec.Ref)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("local fixture fetch: %w: %s", err, output)
		}
		return nil
	}

	if err := hydratePinnedRepositorySpecWithFetcher(ctx, cache, "owner/repo", spec, false, localFetch); err != nil {
		t.Fatalf("clean online hydration: %v", err)
	}
	if err := hydratePinnedRepositorySpec(ctx, cache, "owner/repo", spec, true); err != nil {
		t.Fatalf("offline proof after clean hydration: %v", err)
	}
	if err := hydratePinnedRepositorySpecWithFetcher(ctx, cache, "owner/repo", spec, false, localFetch); err != nil {
		t.Fatalf("warm online hydration did not reverify the declared ref: %v", err)
	}

	runTestGit(t, manual, "fetch", remote, "refs/heads/pinned")
	if err := hydratePinnedRepositorySpec(ctx, manual, "owner/repo", spec, true); err == nil ||
		!strings.Contains(err.Error(), "lacks hydration proof") {
		t.Fatalf("manually seeded object bypassed offline proof: %v", err)
	}

	runTestGit(t, remote, "update-ref", "-d", "refs/heads/pinned")
	if err := hydratePinnedRepositorySpecWithFetcher(ctx, cache, "owner/repo", spec, false, localFetch); err == nil {
		t.Fatal("warm object and proof bypassed verification of a deleted declared ref")
	}
	if err := hydratePinnedRepositorySpec(ctx, cache, "owner/repo", spec, true); err != nil {
		t.Fatalf("valid persisted proof should remain usable offline: %v", err)
	}
	transient := hydratePinnedRepositorySpecWithFetcher(ctx, cache, "owner/repo", spec, false,
		func(context.Context, string, GitFetchSpec) error { return errors.New("RPC failed; HTTP 502") })
	if transient == nil || isPinnedRemoteVerificationError(transient) {
		t.Fatal("transient transport failure was converted into a deterministic remote rejection")
	}
	rejected := hydratePinnedRepositorySpecWithFetcher(ctx, cache, "owner/repo", spec, false,
		func(context.Context, string, GitFetchSpec) error {
			return errors.New("fatal: remote error: upload-pack: not our ref")
		})
	if rejected == nil || !isPinnedRemoteVerificationError(rejected) {
		t.Fatal("explicit remote OID rejection was not classified as deterministic")
	}
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

func auditTestCase() PreparedCase {
	return PreparedCase{
		ID: "audit-case", SourceRowSHA256: strings.Repeat("a", 64),
		PatchSHA256: strings.Repeat("b", 64), BuildStateSHA256: strings.Repeat("d", 64),
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
