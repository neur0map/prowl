package revieweval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/review"
)

func TestPreparationNativeDiffMatchesReviewCapturer(t *testing.T) {
	repository := t.TempDir()
	runTestGit(t, repository, "init", "-q")
	runTestGit(t, repository, "config", "user.email", "fixture@example.com")
	runTestGit(t, repository, "config", "user.name", "Fixture")
	write := func(path string, content []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repository, path), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("crlf.txt", []byte("one\r\ntwo\r\n"))
	write("final.txt", []byte("with-final\n"))
	write("binary.dat", []byte{'a', 0, 'b', '\n'})
	write("rename old.txt", []byte("rename-content\n"))
	runTestGit(t, repository, "add", ".")
	runTestGit(t, repository, "commit", "-qm", "base")
	base := runTestGit(t, repository, "rev-parse", "HEAD")
	write("crlf.txt", []byte("one\r\nchanged\r\n"))
	write("final.txt", []byte("without-final"))
	write("binary.dat", []byte{'a', 0, 'c', '\n'})
	if err := os.Rename(filepath.Join(repository, "rename old.txt"), filepath.Join(repository, "rename new.txt")); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "-A")
	runTestGit(t, repository, "commit", "-qm", "head")
	head := runTestGit(t, repository, "rev-parse", "HEAD")

	capture, err := capturePinnedRange(context.Background(), repository, base, head)
	if err != nil {
		t.Fatal(err)
	}
	candidate := PreparedCase{ID: "native", Repository: "owner/repo", BaseSHA: base, HeadSHA: head}
	hydrated, err := HydrateGitCase(context.Background(), repository, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.RawAdditions != capture.RawAdditions || hydrated.RawDeletions != capture.RawDeletions {
		t.Fatalf("prep churn %d/%d != native capture %d/%d", hydrated.RawAdditions, hydrated.RawDeletions, capture.RawAdditions, capture.RawDeletions)
	}
	wantPatch := review.CanonicalPatchV1(capture.Paths)
	wantDigest := review.CanonicalPatchDigest(capture.Paths)
	if hydrated.PatchSHA256 != hex.EncodeToString(wantDigest[:]) || hydrated.CanonicalDiffSHA256 != digestBytes(wantPatch) {
		t.Fatalf("prep canonical bytes/digest do not match native capture")
	}
}

func TestImportSWRNeutralizesGuardPhraseBeforeCanonicalDigest(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	blockedPhrase := strings.Join([]string{"generated", "by"}, " ")
	record := map[string]any{
		"repo": "owner/repo", "instance_id": "owner__repo-1", "change_introduced": true, "base_commit": base,
		"pr_commits": []map[string]any{{"sha": head, "diff_text": "fixture", "diff": []map[string]string{{"file": "fixture.py", "patch": "@@ -1 +1 @@\n-old\n+new"}}}},
		"changes": []map[string]any{{
			"change_type":         "logic",
			"change_introducing":  map[string]string{"code_snippet": "fixture.py\n@@ -1 +1 @@\n-old\n+new", "commit_sha": head},
			"change_discussion":   map[string]string{"discussion_summary": "reviewer found the issue", "first_mention_timestamp": "2026-01-01T00:00:00Z", "original_reviewer_comment": "reviewer found the issue"},
			"change_resolve_info": map[string]string{"code_snippet": "fixture.py\n@@ -1 +1 @@\n-old\n+new", "commit_sha": head, "resolution_explanation": "warning " + blockedPhrase + " rules"},
		}},
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "swr.jsonl")
	if err := os.WriteFile(path, append(payload, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	cases, err := ImportSWR(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].GroundTruth) != 1 {
		t.Fatalf("unexpected imported cases: %+v", cases)
	}
	truth := cases[0].GroundTruth[0]
	if truth.Scenario != "warning emitted by rules" || truth.CanonicalDigest != groundTruthDigest(truth) {
		t.Fatalf("guard-safe scenario or canonical digest mismatch: %+v", truth)
	}
}

func TestVerificationStateDigestIsOrderedAndEvidenceBound(t *testing.T) {
	provenance := &CleanProvenance{
		Method: "verified", EvidenceRefs: []string{"clean:evidence"},
		Verifications: []VerificationEvidence{
			{Toolchain: "go", ToolchainVersion: "1.24.0", ToolchainSHA256: strings.Repeat("1", 64), Command: "go test ./...", ExitCode: 0, OutputSHA256: strings.Repeat("2", 64), EvidenceRefs: []string{"run:one"}},
			{Toolchain: "node", ToolchainVersion: "22.0.0", ToolchainSHA256: strings.Repeat("3", 64), Command: "npm test", ExitCode: 0, OutputSHA256: strings.Repeat("4", 64), EvidenceRefs: []string{"run:two"}},
		},
	}
	digest, err := CanonicalVerificationStateDigest(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if !sha256Hex.MatchString(digest) {
		t.Fatalf("invalid digest %q", digest)
	}
	reversed := *provenance
	reversed.Verifications = append([]VerificationEvidence(nil), provenance.Verifications...)
	reversed.Verifications[0], reversed.Verifications[1] = reversed.Verifications[1], reversed.Verifications[0]
	reversedDigest, err := CanonicalVerificationStateDigest(&reversed)
	if err != nil {
		t.Fatal(err)
	}
	if reversedDigest == digest {
		t.Fatal("verification order was not bound")
	}
	changed := *provenance
	changed.Verifications = append([]VerificationEvidence(nil), provenance.Verifications...)
	changed.Verifications[0].EvidenceRefs = []string{"run:tampered"}
	changedDigest, err := CanonicalVerificationStateDigest(&changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == digest {
		t.Fatal("verification evidence was not bound")
	}
}

func TestCanonicalPositionUsesNativeHunksForDeletedQuotedPath(t *testing.T) {
	repository := t.TempDir()
	runTestGit(t, repository, "init", "-q")
	runTestGit(t, repository, "config", "user.email", "fixture@example.com")
	runTestGit(t, repository, "config", "user.name", "Fixture")
	path := "dir/quoted space.go"
	if err := os.MkdirAll(filepath.Join(repository, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, path), []byte("defect\ndeleted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "z-padding.go"), []byte(strings.Repeat("old\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", ".")
	runTestGit(t, repository, "commit", "-qm", "base")
	base := runTestGit(t, repository, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(repository, path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "z-padding.go"), []byte(strings.Repeat("new\n", 20)), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "-A")
	runTestGit(t, repository, "commit", "-qm", "head")
	head := runTestGit(t, repository, "rev-parse", "HEAD")
	capture, err := capturePinnedRange(context.Background(), repository, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if capture.Paths[0].Status != "D" || capture.Paths[0].OldPath != path {
		t.Fatalf("native parser lost deletion-only spaced path: %+v", capture.Paths)
	}
	position, err := ComputeCanonicalDiffPosition(capture.Paths, []Location{{Path: path, StartLine: 1, EndLine: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if position.Bin != "beginning" {
		t.Fatalf("deletion-only quoted path bin = %s, want canonical beginning: %+v", position.Bin, position)
	}
}

func TestPreparedTripleRejectsRawChurnAndSizeToleranceDrift(t *testing.T) {
	padding := cleanPaddingCase(t)
	variants := []PreparedCase{metamorphicCase("begin", "beginning"), metamorphicCase("middle", "middle"), metamorphicCase("end", "end")}
	for i := range variants {
		variants[i].RawAdditions, variants[i].RawDeletions = 250, 250
		variants[i].SizeBin = Size301To1000
	}
	triple := testPreparedTriple(variants, []string{"padding"})
	triple.ExpectedRawChurn = 500
	triple.SizeBin = Size301To1000
	triple.MaximumByteSizeDelta = 0
	triple.MaximumLineSizeDelta = 0
	if err := ValidatePreparedTriples(append(variants, padding), []PreparedTriple{triple}); err != nil {
		t.Fatal(err)
	}
	variants[1].RawAdditions++
	if err := ValidatePreparedTriples(append(variants, padding), []PreparedTriple{triple}); err == nil {
		t.Fatal("raw churn drift was accepted")
	}
	variants[1].RawAdditions--
	variants[2].DiffPosition.TotalBytes += 10
	if err := ValidatePreparedTriples(append(variants, padding), []PreparedTriple{triple}); err == nil {
		t.Fatal("precommitted canonical byte tolerance was exceeded")
	}
}

func TestCandidatePoolVerifiesEverySourceSpecificFrozenRow(t *testing.T) {
	kinds := []struct {
		source string
		typ    string
	}{
		{CandidateSourceAACR, SourceHumanCaught},
		{CandidateSourceCodeReviewBench, SourceHumanCaught},
		{CandidateSourceQodoInjected, SourceInjectedValidated},
		{CandidateSourcePublicHistoryClean, SourceClean},
	}
	records := make([]CandidatePoolRecord, 0, len(kinds))
	sources := SourcesManifest{Schema: SourcesSchema}
	paths := map[string]string{}
	for index, kind := range kinds {
		record := testCandidateRecord(index+300, kind.source, kind.typ)
		records = append(records, record)
		path := filepath.Join(t.TempDir(), kind.source+".json")
		payload, err := json.Marshal(CandidateSourceRowsManifest{
			Schema: "review.eval-candidate-source-rows.v1", Rows: []CandidateSourceRow{record.SourceRow},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		sources.Sources = append(sources.Sources, SourceSpec{
			ID: record.SourceID, URL: "https://example.com/" + kind.source + ".json", Path: filepath.Base(path),
			SHA256: hex.EncodeToString(sum[:]), License: "MIT", Parser: "json",
			Revision: strings.Repeat("f", 40), MaxBytes: int64(len(payload)),
		})
		paths[record.SourceID] = path
	}
	pool := CandidatePoolManifest{
		Schema: CandidatePoolSchema, SourcesManifestSHA256: CanonicalSourcesDigest(sources),
		PartitionSeedSHA256: strings.Repeat("9", 64), Records: records,
	}
	digest, err := CanonicalCandidatePoolDigest(pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.SHA256 = digest
	if err := ValidateCandidatePoolSources(pool, sources, paths); err != nil {
		t.Fatal(err)
	}
	var tampered CandidateSourceRowsManifest
	payload, err := os.ReadFile(paths[CandidateSourceQodoInjected])
	if err != nil || json.Unmarshal(payload, &tampered) != nil {
		t.Fatal(err)
	}
	tampered.Rows[0].EvidenceRefs = []string{"tampered:evidence"}
	payload, _ = json.Marshal(tampered)
	if err := os.WriteFile(paths[CandidateSourceQodoInjected], payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCandidatePoolSources(pool, sources, paths); err == nil {
		t.Fatal("shape-valid candidate whose source-specific frozen row drifted was accepted")
	}
}

func positionalNativeRecords(before, after int) []review.RawPathRecord {
	padding := func(path string, count int) review.RawPathRecord {
		var payload strings.Builder
		for i := 0; i < count; i++ {
			fmt.Fprintf(&payload, "-old-%03d\n+new-%03d\n", i, i)
		}
		return review.RawPathRecord{
			Status: "M", OldPath: path, NewPath: path,
			Hunks: []review.RawHunk{{OldStart: 1, OldLines: uint64(count), NewStart: 1, NewLines: uint64(count), Payload: []byte(payload.String())}},
		}
	}
	records := make([]review.RawPathRecord, 0, 3)
	if before > 0 {
		records = append(records, padding("a-padding.go", before))
	}
	records = append(records, review.RawPathRecord{
		Status: "M", OldPath: "m-defect.go", NewPath: "m-defect.go",
		Hunks: []review.RawHunk{{OldStart: 10, OldLines: 1, NewStart: 10, NewLines: 1, Payload: []byte("-old\n+fixed\n")}},
	})
	if after > 0 {
		records = append(records, padding("z-padding.go", after))
	}
	return records
}
