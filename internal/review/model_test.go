package review

import "testing"

func TestModeForChurnBoundary(t *testing.T) {
	tests := []struct {
		churn    int
		force    bool
		mode     Mode
		required bool
	}{
		{300, false, ModeDirect, false},
		{301, false, ModeStructured, true},
		{12, true, ModeStructured, false},
	}
	for _, tt := range tests {
		mode, required := ModeForChurn(tt.churn, tt.force)
		if mode != tt.mode || required != tt.required {
			t.Fatalf("churn=%d force=%v: (%q,%v)", tt.churn, tt.force, mode, required)
		}
	}
}

func TestReportFindingRequiresCauseAndLocation(t *testing.T) {
	finding := Finding{ID: "f_1", Summary: "broken contract"}
	if err := finding.Validate(); err == nil {
		t.Fatal("finding without cause/location accepted")
	}
}

// validFinding builds a fully-formed finding for report-level assertions.
func validFinding() Finding {
	return Finding{
		ID:         "f_1",
		Causes:     []Cause{{Kind: "primary_unit", ID: "u_abc"}},
		Category:   "functional_correctness",
		Severity:   "minor",
		Confidence: "high",
		Summary:    "off-by-one in bounds check",
		Detail:     "loop uses <= instead of <",
		Scenario:   "index len(xs) triggers an out-of-range panic",
		Locations: []Location{{
			Kind:        "range",
			Path:        "internal/x/y.go",
			Side:        "head",
			ContentHash: "deadbeef",
			Start:       10,
			End:         12,
		}},
		Verifier: "confirmed",
	}
}

func TestReportValidateAcceptsCleanReport(t *testing.T) {
	r := Report{
		Schema:         ReportSchemaV1,
		ReviewID:       "rvw_1",
		PlanDigest:     "abc",
		Recommendation: "comment",
		Findings:       []Finding{validFinding()},
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("clean report rejected: %v", err)
	}
}

func TestReportValidateRejectsCriticalApprove(t *testing.T) {
	f := validFinding()
	f.Severity = "critical"
	r := Report{
		Schema:         ReportSchemaV1,
		ReviewID:       "rvw_1",
		PlanDigest:     "abc",
		Recommendation: "approve",
		Findings:       []Finding{f},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("approve accepted with an unrejected critical finding")
	}
	// A rejected critical finding no longer constrains the recommendation.
	f.Verifier = "rejected"
	f.RejectionReason = "pre_existing"
	r.Findings = []Finding{f}
	if err := r.Validate(); err != nil {
		t.Fatalf("rejected critical finding still constrained recommendation: %v", err)
	}
}

func TestReportValidateRejectsDuplicateFindingID(t *testing.T) {
	r := Report{
		Schema:         ReportSchemaV1,
		ReviewID:       "rvw_1",
		PlanDigest:     "abc",
		Recommendation: "comment",
		Findings:       []Finding{validFinding(), validFinding()},
	}
	if err := r.Validate(); err == nil {
		t.Fatal("duplicate finding id accepted")
	}
}

func TestPlanRequestValidate(t *testing.T) {
	good := []PlanRequest{
		{},
		{Base: "main"},
		{Base: "main", Head: "HEAD"},
		{Commit: "abc123"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Fatalf("valid request rejected %+v: %v", r, err)
		}
	}
	bad := []PlanRequest{
		{Commit: "-x"},
		{Base: "-main"},
		{Head: "-HEAD"},
		{Commit: "abc", Base: "main"},
		{Head: "HEAD"},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Fatalf("invalid request accepted: %+v", r)
		}
	}
}

func TestPlanRequestKind(t *testing.T) {
	cases := []struct {
		req  PlanRequest
		kind ScopeKind
	}{
		{PlanRequest{}, ScopeWorkspace},
		{PlanRequest{Base: "main"}, ScopeRange},
		{PlanRequest{Commit: "abc"}, ScopeCommit},
	}
	for _, tc := range cases {
		if got := tc.req.Kind(); got != tc.kind {
			t.Fatalf("%+v kind=%q want %q", tc.req, got, tc.kind)
		}
	}
}

func TestPrimaryReceiptValidate(t *testing.T) {
	ok := PrimaryReceipt{UnitID: "u_1", Reviewer: "agent-a", AcknowledgedPrimaryHunkIDs: []string{"h_1"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid primary receipt rejected: %v", err)
	}
	if err := (PrimaryReceipt{Reviewer: "x"}).Validate(); err == nil {
		t.Fatal("primary receipt without unit id accepted")
	}
	if err := (PrimaryReceipt{UnitID: "u_1"}).Validate(); err == nil {
		t.Fatal("primary receipt without reviewer accepted")
	}
}

func TestAuditReceiptValidate(t *testing.T) {
	// An empty target set is a valid explicit receipt.
	ok := AuditReceipt{AuditID: AuditRemovedBehaviorV1, Reviewer: "agent-a", AcknowledgedAuditTargetIDs: []string{}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid empty audit receipt rejected: %v", err)
	}
	if err := (AuditReceipt{AuditID: "audit_bogus", Reviewer: "x"}).Validate(); err == nil {
		t.Fatal("unknown audit id accepted")
	}
	if err := (AuditReceipt{AuditID: AuditTestMatrixV1}).Validate(); err == nil {
		t.Fatal("audit receipt without reviewer accepted")
	}
}

func TestCheckResultValidate(t *testing.T) {
	if err := (CheckResult{Mode: ModeDirect, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"}).Validate(); err != nil {
		t.Fatalf("valid direct check rejected: %v", err)
	}
	if err := (CheckResult{Mode: ModeDirect, Coverage: CoverageComplete, Recommendation: "comment"}).Validate(); err == nil {
		t.Fatal("direct check with non-direct coverage accepted")
	}
	if err := (CheckResult{Mode: ModeStructured, Coverage: CoverageIncomplete, Recommendation: "incomplete", OK: true}).Validate(); err == nil {
		t.Fatal("ok check recommending incomplete accepted")
	}
	if err := (CheckResult{Mode: "sideways", Coverage: CoverageComplete, Recommendation: "comment"}).Validate(); err == nil {
		t.Fatal("invalid mode accepted")
	}
}
