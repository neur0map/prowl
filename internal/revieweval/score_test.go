package revieweval

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/agenttrial"
)

func TestMatchUsesLexicographicMaximumAndDuplicatesBecomeFP(t *testing.T) {
	findings := []Finding{{ID: "a", VerifierDisposition: "confirmed"}, {ID: "b", VerifierDisposition: "plausible"}, {ID: "c", VerifierDisposition: "confirmed"}}
	truth := []GroundTruth{{ID: "x"}, {ID: "y"}}
	edges := []EligibilityEdge{{FindingID: "a", GroundTruthID: "x"}, {FindingID: "a", GroundTruthID: "y"}, {FindingID: "b", GroundTruthID: "x"}, {FindingID: "c", GroundTruthID: "x"}}
	got := Match(findings, truth, edges)
	want := []MatchPair{{FindingID: "a", GroundTruthID: "y"}, {FindingID: "b", GroundTruthID: "x"}}
	if !reflect.DeepEqual(got.Pairs, want) {
		t.Fatalf("pairs = %#v", got.Pairs)
	}
	if got.TP != 2 || got.FP != 1 || got.FN != 0 {
		t.Fatalf("counts = %d/%d/%d", got.TP, got.FP, got.FN)
	}
}

func TestBlindAdjudicationInputOmitsClientConditionAndHostIDs(t *testing.T) {
	input := BuildBlindAdjudicationInput(
		[]TrialRecord{{CaseID: "case", Client: "secret-client", Condition: ConditionTreatment, Output: &EvalOutput{Findings: []Finding{{ID: "canonical", HostID: "host", Condition: ConditionTreatment, Summary: "summary", VerifierDisposition: "confirmed"}}}}},
		[]Case{{ID: "case", GroundTruth: []GroundTruth{{ID: "truth"}}}},
	)
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"secret-client", "treatment", "host"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("blind input leaked %q: %s", forbidden, text)
		}
	}
	if got := input.FindingCases["canonical"]; len(got) != 1 || got[0] != "case" {
		t.Fatalf("finding cases = %#v", got)
	}
	if got := input.GroundTruthCases["case:truth"]; len(got) != 1 || got[0] != "case" {
		t.Fatalf("case-qualified ground-truth cases = %#v", got)
	}
	digest, err := jsonDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAdjudication(Adjudication{Schema: AdjudicationSchema, CandidateDigest: digest, Frozen: true, Edges: []EligibilityEdge{{FindingID: "foreign", GroundTruthID: "case:truth"}}}, input); err == nil {
		t.Fatal("foreign adjudication edge accepted")
	}
	crossCase := input
	crossCase.GroundTruthCases = map[string][]string{"case:truth": {"other"}}
	crossDigest, _ := jsonDigest(crossCase)
	if err := ValidateAdjudication(Adjudication{Schema: AdjudicationSchema, CandidateDigest: crossDigest, Frozen: true, Edges: []EligibilityEdge{{FindingID: "canonical", GroundTruthID: "case:truth"}}}, crossCase); err == nil {
		t.Fatal("cross-case adjudication edge accepted")
	}
}

func TestCaseQualifiedGroundTruthKeysKeepSharedIdentitiesDistinct(t *testing.T) {
	cases := []Case{
		{ID: "begin", GroundTruth: []GroundTruth{{ID: "bug"}}},
		{ID: "middle", GroundTruth: []GroundTruth{{ID: "bug"}}},
	}
	input := BuildBlindAdjudicationInput(nil, cases)
	if len(input.GroundTruth) != 2 || input.GroundTruth[0].ID != "begin:bug" || input.GroundTruth[1].ID != "middle:bug" {
		t.Fatalf("qualified ground truth = %#v", input.GroundTruth)
	}
	record := TrialRecord{
		CaseID: "begin", Condition: ConditionTreatment, Status: StatusCompleted,
		Output: &EvalOutput{Status: StatusCompleted, Findings: []Finding{{ID: "finding", VerifierDisposition: "confirmed"}}},
	}
	score := ScoreTrial(record, cases[0].GroundTruth, []EligibilityEdge{{FindingID: "finding", GroundTruthID: "begin:bug"}})
	if score.TP != 1 || score.FP != 0 || score.FN != 0 || !reflect.DeepEqual(score.MatchedGroundTruth, []string{"begin:bug"}) {
		t.Fatalf("qualified score = %#v", score)
	}
}

func TestFailureAndCleanFailureSemantics(t *testing.T) {
	defective := ScoreTrial(TrialRecord{CaseID: "bug", Condition: ConditionTreatment, Status: StatusFailed}, []GroundTruth{{ID: "x"}}, nil)
	if defective.TP != 0 || defective.FP != 0 || defective.FN != 1 || defective.Completed {
		t.Fatalf("defective failure = %#v", defective)
	}
	clean := ScoreTrial(TrialRecord{CaseID: "clean", Condition: ConditionTreatment, Status: StatusFailed}, nil, nil)
	if clean.TP != 0 || clean.FP != 0 || clean.FN != 0 || clean.Completed {
		t.Fatalf("clean failure = %#v", clean)
	}
	failedApproval := ScoreTrial(TrialRecord{CaseID: "clean", Condition: ConditionTreatment, Status: StatusFailed, Output: &EvalOutput{Status: StatusFailed, Recommendation: "approve"}}, nil, nil)
	if !failedApproval.IncompleteApproval || failedApproval.FP != 0 {
		t.Fatalf("failed approval = %#v", failedApproval)
	}
}

func TestAggregateComputesExactPooledAndBreakdowns(t *testing.T) {
	scores := []TrialScore{
		{CaseID: "a", Client: "c1", Condition: ConditionControl, DefectClass: "local", TP: 1, FP: 1, FN: 1, CriticalTP: 1, CriticalTotal: 1, CrossFileTP: 1, CrossFileTotal: 2, DeletionTP: 0, DeletionTotal: 1, SemanticTP: 1, LocationRecords: 2, ResolvedLocations: 1, Completed: true, ElapsedMS: 10, Usage: agenttrial.Usage{ModelTokens: 10, ToolCalls: 2}},
		{CaseID: "a", Client: "c1", Condition: ConditionTreatment, DefectClass: "local", TP: 2, FP: 0, FN: 0, CriticalTP: 1, CriticalTotal: 1, CrossFileTP: 2, CrossFileTotal: 2, DeletionTP: 1, DeletionTotal: 1, SemanticTP: 2, LocationRecords: 2, ResolvedLocations: 2, Completed: true, PrimaryRangesCovered: 2, PrimaryRangesTotal: 2, AuditTargetsCovered: 1, AuditTargetsTotal: 1, ElapsedMS: 8, Usage: agenttrial.Usage{ModelTokens: 10, ToolCalls: 3, Subagents: 1}},
	}
	report := Aggregate(scores)
	control := report.Conditions[ConditionControl]
	if control.Precision != .5 || control.Recall != .5 || control.F1 != .5 {
		t.Fatalf("control = %#v", control)
	}
	treatment := report.Conditions[ConditionTreatment]
	if treatment.Precision != 1 || treatment.Recall != 1 || treatment.F1 != 1 || treatment.ReceiptCoverage != 1 || treatment.LocationCoverage != 1 {
		t.Fatalf("treatment = %#v", treatment)
	}
	if report.ByClient["c1"][ConditionTreatment].ToolCalls != 3 {
		t.Fatal("missing client breakdown")
	}
	if report.ByDefectClass["local"][ConditionControl].F1 != .5 {
		t.Fatal("missing class breakdown")
	}
}

func TestRepetitionVarianceAndEveryShippingGate(t *testing.T) {
	variance := aggregateMetric([]TrialScore{{CaseID: "a", Client: "c", Condition: ConditionControl, TP: 1}, {CaseID: "a", Client: "c", Condition: ConditionControl, FN: 1}})
	if variance.RepetitionVariance != .25 {
		t.Fatalf("variance = %v", variance.RepetitionVariance)
	}
	report := AggregateReport{
		Conditions: map[string]Metric{
			ConditionControl:   {Precision: .8, Recall: .5, F1: .6},
			ConditionTreatment: {Precision: .76, Recall: .8, F1: .71},
		},
		ByClient: map[string]map[string]Metric{"client": {ConditionControl: {F1: .6}, ConditionTreatment: {F1: .61}}},
	}
	scores := []TrialScore{
		{Condition: ConditionControl, Completed: true, Usage: agenttrial.Usage{ModelTokens: 100}},
		{Condition: ConditionTreatment, Completed: true, PrimaryRangesCovered: 2, PrimaryRangesTotal: 2, AuditTargetsCovered: 1, AuditTargetsTotal: 1, LocationRecords: 2, ResolvedLocations: 2, Usage: agenttrial.Usage{ModelTokens: 105}},
	}
	positional := map[string]MetamorphicMetric{ConditionControl: {Sensitivity: .1}, ConditionTreatment: {Sensitivity: .1}}
	cfg := ScoringConfig{MinimumF1Delta: .1, MaximumPrecisionDrop: .05, MaximumPositionalSensitivity: .1, MaximumTokenRatio: 1.05}
	gates := EvaluateShippingGates(report, scores, positional, BootstrapInterval{Lower: .01}, cfg)
	if !gates.Passed {
		t.Fatalf("gates = %#v", gates)
	}

	scores[1].IncompleteApproval = true
	if got := EvaluateShippingGates(report, scores, positional, BootstrapInterval{Lower: .01}, cfg); got.NoInvalidApprovals || got.Passed {
		t.Fatalf("invalid approval gate = %#v", got)
	}
	scores[1].IncompleteApproval = false
	scores[1].ResolvedLocations = 1
	if got := EvaluateShippingGates(report, scores, positional, BootstrapInterval{Lower: .01}, cfg); got.LocationCoverage || got.Passed {
		t.Fatalf("location gate = %#v", got)
	}
}
