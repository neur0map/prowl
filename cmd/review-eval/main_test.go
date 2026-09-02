package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prowl-agent/prowl-agent/internal/agenttrial"
	"github.com/prowl-agent/prowl-agent/internal/revieweval"
)

func TestHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Usage of review-eval:")) {
		t.Fatalf("help = %q", stderr.String())
	}
}

func TestParseFlagsRequiresPhaseInputs(t *testing.T) {
	for _, args := range [][]string{
		{"--phase", "collect"},
		{"--phase", "score", "--output", "artifacts"},
		{"--phase", "score", "--output", "artifacts", "--collection", "collection.json"},
		{"--phase", "score", "--output", "artifacts", "--adjudication", "edges.json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args %v exit = %d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestParseFlagsRejectsProductionRepetitionOverrideBeforeIO(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--output", "artifacts", "--set", "held_out", "--repetitions", "2"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
}

func TestScorePhaseConsumesRetainedCollectionWithoutExecutables(t *testing.T) {
	caseInfo := revieweval.Case{ID: "case", Repository: "repo", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), RawAdditions: 301, DefectClass: "clean"}
	manifest := revieweval.Manifest{Schema: revieweval.ManifestSchema, Tuning: []revieweval.Case{caseInfo}}
	scoring := revieweval.ScoringConfig{
		Schema: revieweval.ScoringConfigSchema, BaseCaseIDs: []string{"case"}, Clients: []string{"claude"}, Model: "fake",
		Budget:         agenttrial.Budget{MaxModelTokens: 1, MaxToolCalls: 1, MaxSubagents: 1, Timeout: time.Second},
		MaxOutputBytes: 1024, Repetitions: 1, BootstrapReplicates: 8,
		MatchingCriteria: "frozen", BlindAdjudication: "blind", AcceptedDispositions: []string{"confirmed", "plausible"}, FailureScoring: "frozen", Seed: 7,
	}
	order, err := revieweval.BuildOrder(manifest.Tuning, scoring.Clients, scoring.Repetitions, scoring.Seed)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]revieweval.TrialRecord, len(order))
	for index, spec := range order {
		records[index] = revieweval.TrialRecord{Ordinal: spec.Ordinal, CaseID: spec.CaseID, Client: spec.Client, Condition: spec.Condition, Repetition: spec.Repetition, Status: revieweval.StatusFailed}
	}
	blind := revieweval.BuildBlindAdjudicationInput(records, manifest.Tuning)
	collection := revieweval.TrialCollection{
		Schema: revieweval.CollectionSchema, ManifestDigest: testJSONDigest(t, manifest), ScoringConfigDigest: testJSONDigest(t, scoring),
		CandidateDigest: testJSONDigest(t, blind), Set: "tuning", Order: order, Trials: records, BlindInput: blind,
	}
	adjudication := revieweval.Adjudication{Schema: revieweval.AdjudicationSchema, CandidateDigest: collection.CandidateDigest, Frozen: true}
	inputRoot := t.TempDir()
	manifestPath, scoringPath := filepath.Join(inputRoot, "manifest.json"), filepath.Join(inputRoot, "scoring.json")
	collectionPath, adjudicationPath := filepath.Join(inputRoot, "collection.json"), filepath.Join(inputRoot, "adjudication.json")
	for path, value := range map[string]any{manifestPath: manifest, scoringPath: scoring, collectionPath: collection, adjudicationPath: adjudication} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outputParent, err := os.MkdirTemp(".", "score-artifacts-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outputParent)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--phase", "score", "--manifest", manifestPath, "--scoring-config", scoringPath, "--collection", collectionPath, "--adjudication", adjudicationPath, "--output", filepath.Base(outputParent)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(outputParent, "report.json")); err != nil {
		t.Fatal(err)
	}
}

func testJSONDigest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
