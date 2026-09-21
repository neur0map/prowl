package revieweval

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/agenttrial"
)

func TestValidateManifestRejectsTraversalOverlapAndBoundary(t *testing.T) {
	valid := Case{ID: "case-a", Repository: "repos/a", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), RawAdditions: 301, DefectClass: "local_logic", GroundTruth: []GroundTruth{{ID: "bug-a", DefectClass: "local_logic", Summary: "guard accepts empty input", Scenario: "empty input reaches storage", Locations: []Location{{Path: "pkg/a.go", StartLine: 4, EndLine: 7}}}}}
	for name, mutate := range map[string]func(*Manifest){
		"traversal":    func(m *Manifest) { m.HeldOut[0].Repository = "../a" },
		"boundary":     func(m *Manifest) { m.HeldOut[0].RawAdditions = 300 },
		"overlap":      func(m *Manifest) { m.Tuning = []Case{m.HeldOut[0]} },
		"short sha":    func(m *Manifest) { m.HeldOut[0].HeadSHA = "abc123" },
		"bad location": func(m *Manifest) { m.HeldOut[0].GroundTruth[0].Locations[0].Path = "/etc/passwd" },
	} {
		t.Run(name, func(t *testing.T) {
			m := Manifest{Schema: ManifestSchema, HeldOut: []Case{valid}}
			mutate(&m)
			if err := ValidateManifest(m); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateManifestRejectsGloballyDuplicateGroundTruthIDs(t *testing.T) {
	first := Case{ID: "case-a", Repository: "repos/a", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), RawAdditions: 301, DefectClass: "local_logic", GroundTruth: []GroundTruth{{ID: "duplicate", DefectClass: "local_logic", Summary: "first defect", Scenario: "first failure", Locations: []Location{{Path: "a.go", StartLine: 1, EndLine: 1}}}}}
	second := Case{ID: "case-b", Repository: "repos/b", BaseSHA: strings.Repeat("c", 40), HeadSHA: strings.Repeat("d", 40), RawAdditions: 301, DefectClass: "local_logic", GroundTruth: []GroundTruth{{ID: "duplicate", DefectClass: "local_logic", Summary: "first defect", Scenario: "first failure", Locations: []Location{{Path: "a.go", StartLine: 1, EndLine: 1}}}}}
	if err := ValidateManifest(Manifest{Schema: ManifestSchema, Tuning: []Case{first, second}}); err == nil {
		t.Fatal("manifest-wide duplicate ground-truth ID accepted")
	}
}

func TestValidateManifestRejectsConflictingTripleGroundTruthContent(t *testing.T) {
	manifest, _ := productionValidationFixture()
	manifest.HeldOut[len(manifest.HeldOut)-1].GroundTruth[0].Summary = "different defect"
	if err := ValidateManifest(manifest); err == nil {
		t.Fatal("metamorphic triple accepted conflicting ground-truth content")
	}
}

func TestProductionTripleSetContainsOnlyHeldOutTriples(t *testing.T) {
	manifest, cfg := productionValidationFixture()
	if err := ValidateManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if err := ValidateScoringConfig(cfg, manifest, true); err != nil {
		t.Fatalf("held-out triple set rejected: %v", err)
	}
	cfg.MetamorphicTripleIDs = append(cfg.MetamorphicTripleIDs, "tuning-triple")
	if err := ValidateScoringConfig(cfg, manifest, true); err == nil {
		t.Fatal("production accepted a tuning triple")
	}
}

func productionValidationFixture() (Manifest, ScoringConfig) {
	manifest := Manifest{Schema: ManifestSchema}
	for index := range 30 {
		id := fmt.Sprintf("base-%02d", index)
		manifest.HeldOut = append(manifest.HeldOut, Case{ID: id, Repository: "repos/" + id, BaseSHA: strings.Repeat("a", 40), HeadSHA: fmt.Sprintf("%040x", index+1), RawAdditions: 301, DefectClass: "clean"})
	}
	addTriple := func(set *[]Case, prefix, repository, base string) MetamorphicTriple {
		ids := []string{prefix + "-begin", prefix + "-middle", prefix + "-end"}
		truthID := prefix + "-bug"
		for index, id := range ids {
			*set = append(*set, Case{
				ID: id, Repository: repository, BaseSHA: base, HeadSHA: fmt.Sprintf("%040x", index+100),
				RawAdditions: 301, DefectClass: "local_logic", Metamorphic: true,
				GroundTruth: []GroundTruth{{ID: truthID, DefectClass: "local_logic", Summary: "guard failure", Scenario: "bad input passes", Locations: []Location{{Path: "a.go", StartLine: 1, EndLine: 1}}}},
			})
		}
		return MetamorphicTriple{ID: prefix + "-triple", GroundTruthID: truthID, BeginningCaseID: ids[0], MiddleCaseID: ids[1], EndCaseID: ids[2]}
	}
	tuningTriple := addTriple(&manifest.Tuning, "tuning", "repos/tuning-meta", strings.Repeat("b", 40))
	heldTriple := addTriple(&manifest.HeldOut, "held", "repos/held-meta", strings.Repeat("c", 40))
	manifest.MetamorphicTriples = []MetamorphicTriple{tuningTriple, heldTriple}
	cfg := ScoringConfig{
		Schema: ScoringConfigSchema, PolicyVersion: EvalPolicyVersion, ArtifactSchemaVersion: EvalArtifactSchemaVersion,
		Executables: ToolchainPins{
			Claude: ExecutablePin{Version: "claude 1", SHA256: strings.Repeat("1", 64)},
			OMP:    ExecutablePin{Version: "omp 1", SHA256: strings.Repeat("2", 64)},
			Prowl:  ExecutablePin{Version: "prowl 1", SHA256: strings.Repeat("3", 64)},
		},
		Clients: []string{"claude", "omp"}, Model: "frozen", Repetitions: 3, BootstrapReplicates: ProductionBootstrapReplicates,
		Budget: agenttrial.Budget{MaxModelTokens: 1, MaxToolCalls: 1, MaxSubagents: 1, Timeout: time.Second}, MaxOutputBytes: 1,
		MatchingCriteria: "frozen", BlindAdjudication: "frozen", AcceptedDispositions: []string{"confirmed", "plausible"}, FailureScoring: "frozen",
		MinimumF1Delta: .10, MaximumPrecisionDrop: .05, MaximumPositionalSensitivity: .10, MaximumTokenRatio: 1.05,
		MetamorphicTripleIDs: []string{heldTriple.ID},
	}
	for _, c := range manifest.HeldOut {
		if !c.Metamorphic {
			cfg.BaseCaseIDs = append(cfg.BaseCaseIDs, c.ID)
		}
	}
	return manifest, cfg
}

func TestProductionScoringRequiresPolicySchemaAndExecutablePins(t *testing.T) {
	manifest, cfg := productionValidationFixture()
	cfg.PolicyVersion = ""
	if err := ValidateScoringConfig(cfg, manifest, true); err == nil {
		t.Fatal("production accepted an unpinned policy version")
	}
	_, cfg = productionValidationFixture()
	cfg.Executables.Prowl.SHA256 = ""
	if err := ValidateScoringConfig(cfg, manifest, true); err == nil {
		t.Fatal("production accepted a missing Prowl hash pin")
	}
}

func TestEvalOutputStrictValidationAndCanonicalIDs(t *testing.T) {
	output := EvalOutput{Schema: EvalOutputSchema, Status: StatusCompleted, Recommendation: "request_changes", Findings: []Finding{
		{HostID: "host-one", Category: "functional_correctness", Severity: "major", Confidence: "high", Summary: "wrong guard", Scenario: "empty input", Locations: []Location{{Path: "a.go", StartLine: 9, EndLine: 9}}, VerifierDisposition: "confirmed", VerifierEvidence: "branch is reachable"},
		{HostID: "host-two", Category: "functional_correctness", Severity: "major", Confidence: "high", Summary: "wrong guard", Scenario: "empty input", Locations: []Location{{Path: "a.go", StartLine: 9, EndLine: 9}}, VerifierDisposition: "confirmed", VerifierEvidence: "branch is reachable"},
	}}
	if err := ValidateEvalOutput(output, false); err != nil {
		t.Fatal(err)
	}
	CanonicalizeFindingIDs(&output)
	if output.Findings[0].ID == output.Findings[1].ID {
		t.Fatal("ordinal must distinguish duplicate findings")
	}
	if got, want := output.Findings[0].ID, "2ec774a69d0e9ef759cdb284cb94aa8b23d885a466f921f614f6060907e26248"; got != want {
		t.Fatalf("canonical vector = %s", got)
	}
	first := output.Findings[0].ID
	output.Findings[0].HostID, output.Findings[0].Condition = "changed", "treatment"
	CanonicalizeFindingIDs(&output)
	if output.Findings[0].ID != first {
		t.Fatal("host IDs and condition labels must be ignored")
	}

	output.Findings[0].Condition = ""

	data, _ := json.Marshal(output)
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := ParseEvalOutput(data, false); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := ValidateEvalOutput(output, true); err == nil {
		t.Fatal("treatment without checker record accepted")
	}
	output.Checker = &CheckerArtifacts{ReviewID: "rvw_0123456789abcdef0123456789abcdef01234567", PlanPath: "artifacts/plan.json", ReportPath: "artifacts/report.json", CheckPath: "artifacts/check.json"}
	if err := ValidateEvalOutput(output, true); err != nil {
		t.Fatal(err)
	}
}

func TestFailedTreatmentOutputIsValidWithoutChecker(t *testing.T) {
	output := EvalOutput{Schema: EvalOutputSchema, Status: StatusFailed, Recommendation: "incomplete", Findings: []Finding{}}
	if err := ValidateEvalOutput(output, true); err != nil {
		t.Fatal(err)
	}
}
