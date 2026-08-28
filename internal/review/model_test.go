package review

import "testing"

const (
	testReviewID   = "rvw_0123456789abcdef0123456789abcdef01234567"
	testPlanDigest = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
)

func testGitSide() SideIdentity { return SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)} }
func testWSSide() SideIdentity {
	return SideIdentity{Kind: SideWorkspaceSHA256, Value: make([]byte, 32)}
}

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

// validFinding builds a fully-formed confirmed finding.
func validFinding() Finding {
	return Finding{
		ID:         "f_1",
		Causes:     []Cause{{Kind: "primary_unit", ID: "u_abc"}},
		Category:   "functional_correctness",
		Severity:   "minor",
		Confidence: "high",
		Summary:    "off-by-one in bounds check",
		Detail:     "loop uses <= instead of < so the final index overruns the slice",
		Scenario:   "index len(xs) triggers an out-of-range panic",
		Locations: []Location{{
			Kind:        "range",
			Path:        "internal/x/y.go",
			Side:        "head",
			ContentHash: testPlanDigest,
			Start:       10,
			End:         12,
		}},
		Verifier: "confirmed",
	}
}

// validRejectedFinding builds a rejected finding with the evidence the contract
// requires before it may be excluded from recommendation severity.
func validRejectedFinding() Finding {
	f := validFinding()
	f.Verifier = "rejected"
	f.RejectionReason = "pre_existing"
	f.VerifierEvidence = []Citation{{Kind: "code", Path: "internal/x/y.go", Note: "identical code exists on base"}}
	f.Citations = []Citation{{Kind: "hunk", ID: "h_1"}}
	return f
}

func validReport(findings ...Finding) Report {
	return Report{
		Schema:         ReportSchemaV1,
		ReviewID:       testReviewID,
		PlanDigest:     testPlanDigest,
		Base:           testGitSide(),
		Head:           testGitSide(),
		Recommendation: "comment",
		Findings:       findings,
	}
}

func TestReportValidateAcceptsCleanReport(t *testing.T) {
	if err := validReport(validFinding()).Validate(); err != nil {
		t.Fatalf("clean report rejected: %v", err)
	}
}

func TestReportValidateRejectsCriticalApprove(t *testing.T) {
	// Unrejected critical finding forbids approve.
	f := validFinding()
	f.Severity = "critical"
	r := validReport(f)
	r.Recommendation = "approve"
	if err := r.Validate(); err == nil {
		t.Fatal("approve accepted with an unrejected critical finding")
	}

	// An evidence-free rejected critical finding is itself invalid: it may not
	// silently unlock approve.
	ef := validFinding()
	ef.Severity = "critical"
	ef.Verifier = "rejected"
	ef.RejectionReason = "pre_existing"
	er := validReport(ef)
	er.Recommendation = "approve"
	if err := er.Validate(); err == nil {
		t.Fatal("evidence-free rejected critical finding accepted with approve")
	}

	// A fully-evidenced rejected critical finding no longer constrains severity.
	rf := validRejectedFinding()
	rf.Severity = "critical"
	rr := validReport(rf)
	rr.Recommendation = "approve"
	if err := rr.Validate(); err != nil {
		t.Fatalf("evidenced rejected critical finding still constrained recommendation: %v", err)
	}
}

func TestReportValidateRejectsDuplicateFindingID(t *testing.T) {
	if err := validReport(validFinding(), validFinding()).Validate(); err == nil {
		t.Fatal("duplicate finding id accepted")
	}
}

func TestReportValidateChecksIdentityAndDigest(t *testing.T) {
	r := validReport(validFinding())
	r.Base = SideIdentity{}
	if err := r.Validate(); err == nil {
		t.Fatal("report with zero-value base identity accepted")
	}
	r = validReport(validFinding())
	r.ReviewID = "rvw_1"
	if err := r.Validate(); err == nil {
		t.Fatal("report with malformed review id accepted")
	}
	r = validReport(validFinding())
	r.PlanDigest = "abc"
	if err := r.Validate(); err == nil {
		t.Fatal("report with non-canonical plan digest accepted")
	}
}

func TestFindingValidateRequiresDetailAndRejectionEvidence(t *testing.T) {
	f := validFinding()
	f.Detail = ""
	if err := f.Validate(); err == nil {
		t.Fatal("finding without detail accepted")
	}
	// Rejected without verifier evidence / supporting citations is invalid.
	f = validFinding()
	f.Verifier = "rejected"
	f.RejectionReason = "duplicate"
	if err := f.Validate(); err == nil {
		t.Fatal("rejected finding without verifier evidence accepted")
	}
	f = validRejectedFinding()
	f.VerifierEvidence = nil
	if err := f.Validate(); err == nil {
		t.Fatal("rejected finding without verifier evidence accepted")
	}
	f = validRejectedFinding()
	f.Citations = nil
	if err := f.Validate(); err == nil {
		t.Fatal("rejected finding without supporting citations accepted")
	}
	if err := validRejectedFinding().Validate(); err != nil {
		t.Fatalf("fully-evidenced rejected finding rejected: %v", err)
	}
}

func TestLocationValidateProofVariants(t *testing.T) {
	// Content-backed range.
	ok := Location{Kind: "range", Path: "a.go", Side: "head", ContentHash: testPlanDigest, Start: 1, End: 2}
	if err := ok.Validate(); err != nil {
		t.Fatalf("content-backed range rejected: %v", err)
	}
	// Non-content path proof.
	np := Location{Kind: "path", Path: "sub", Side: "base", Identity: &SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}, Mode: 0o160000, EntryType: "gitlink"}
	if err := np.Validate(); err != nil {
		t.Fatalf("non-content path proof rejected: %v", err)
	}
	bad := []Location{
		{Kind: "range", Path: "a.go", Side: "head", ContentHash: "deadbeef", Start: 1, End: 2},                                               // short content hash
		{Kind: "path", Path: "a.go", Side: "head", ContentHash: testPlanDigest, Identity: &SideIdentity{Kind: SideAbsent}},                   // two proof forms
		{Kind: "path", Path: "sub", Side: "base", Identity: &SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}},                        // non-content missing entry type
		{Kind: "range", Path: "sub", Side: "base", Identity: &SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}, EntryType: "gitlink"}, // range must be content-backed
		{Kind: "range", Path: "a.go", Side: "head", ContentHash: testPlanDigest, Start: 0, End: 2},                                           // bad range bounds
		{Kind: "path", Path: "", Side: "head", ContentHash: testPlanDigest},                                                                  // missing path
		{Kind: "sideways", Path: "a.go", Side: "head", ContentHash: testPlanDigest},                                                          // bad kind
	}
	for i, l := range bad {
		if err := l.Validate(); err == nil {
			t.Fatalf("bad location %d accepted", i)
		}
	}
}

func TestPlanRequestValidate(t *testing.T) {
	good := []PlanRequest{{}, {Base: "main"}, {Base: "main", Head: "HEAD"}, {Commit: "abc123"}}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Fatalf("valid request rejected %+v: %v", r, err)
		}
	}
	bad := []PlanRequest{{Commit: "-x"}, {Base: "-main"}, {Head: "-HEAD"}, {Commit: "abc", Base: "main"}, {Head: "HEAD"}}
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

func TestRequiredAuditsV1Immutable(t *testing.T) {
	a := RequiredAuditsV1()
	if len(a) != 4 {
		t.Fatalf("want 4 required audits, got %d", len(a))
	}
	a[0] = "mutated"
	b := RequiredAuditsV1()
	if b[0] == "mutated" {
		t.Fatal("RequiredAuditsV1 exposes shared mutable state")
	}
	if b[0] != AuditRemovedBehaviorV1 {
		t.Fatalf("unexpected first required audit %q", b[0])
	}
}

func validUnit() Unit {
	return Unit{
		Schema:       UnitSchemaV1,
		ReviewID:     testReviewID,
		UnitID:       "u_1",
		CohortID:     "c_1",
		LayerID:      "l_1",
		ScopeKind:    ScopeWorkspace,
		ObjectFormat: "sha1",
		Base:         testGitSide(),
		Head:         testWSSide(),
		Hunks: []UnitHunk{{
			PathID: "p_1", NewPath: "a.go", Status: "M", Ordinal: 0,
			OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 2, PatchBase64: "K3gK",
		}},
	}
}

func TestUnitValidate(t *testing.T) {
	if err := validUnit().Validate(); err != nil {
		t.Fatalf("valid unit rejected: %v", err)
	}
	u := validUnit()
	u.UnitID = ""
	if err := u.Validate(); err == nil {
		t.Fatal("unit without unit_id accepted")
	}
	u = validUnit()
	u.Hunks = nil
	if err := u.Validate(); err == nil {
		t.Fatal("unit without hunks accepted")
	}
	u = validUnit()
	u.Hunks[0].PatchBase64 = ""
	if err := u.Validate(); err == nil {
		t.Fatal("unit hunk without patch_base64 accepted")
	}
	u = validUnit()
	u.Schema = "review.unit.v2"
	if err := u.Validate(); err == nil {
		t.Fatal("unit with wrong schema accepted")
	}
}

func allRequiredAudits() []PlanAudit {
	out := make([]PlanAudit, 0, 4)
	for _, id := range RequiredAuditsV1() {
		out = append(out, PlanAudit{AuditID: id, TargetIDs: []string{}})
	}
	return out
}

func validPlan() Plan {
	return Plan{
		Schema:             PlanSchemaV1,
		ReviewID:           testReviewID,
		PlanDigest:         testPlanDigest,
		Mode:               ModeDirect,
		StructuredRequired: false,
		Scope:              Scope{Kind: ScopeWorkspace, ObjectFormat: "sha1", Base: testGitSide(), Head: testWSSide()},
		Stats:              PlanStats{RawAdditions: 1, RawChurn: 1, ChangedPaths: 1},
		ChangedPaths: []PlanPath{{
			PathID: "p_1", NewPath: "a.go", Status: "M",
			ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"},
		}},
		NextCommands: []NextCommand{{Label: "check", Command: "prowl-agent review check --review rvw_x --report r.json"}},
	}
}

func validStructuredPlan() Plan {
	p := validPlan()
	p.Mode = ModeStructured
	p.StructuredRequired = true
	p.PrimaryUnits = []Unit{validUnit()}
	p.Cohorts = []PlanCohort{{
		CohortID: "c_1", Label: "core",
		Layers:  []PlanLayer{{LayerID: "l_1", Ordinal: 0, UnitIDs: []string{"u_1"}}},
		UnitIDs: []string{"u_1"},
	}}
	p.RequiredAudits = allRequiredAudits()
	return p
}

func TestPlanValidate(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("valid direct plan rejected: %v", err)
	}
	if err := validStructuredPlan().Validate(); err != nil {
		t.Fatalf("valid structured plan rejected: %v", err)
	}
	// direct plan must not carry structured collections
	p := validPlan()
	p.Cohorts = []PlanCohort{{CohortID: "c_1"}}
	if err := p.Validate(); err == nil {
		t.Fatal("direct plan carrying cohorts accepted")
	}
	// structured plan missing a required audit
	sp := validStructuredPlan()
	sp.RequiredAudits = sp.RequiredAudits[:3]
	if err := sp.Validate(); err == nil {
		t.Fatal("structured plan missing a required audit accepted")
	}
	// structured plan without cohorts
	sc := validStructuredPlan()
	sc.Cohorts = nil
	if err := sc.Validate(); err == nil {
		t.Fatal("structured plan without cohorts accepted")
	}
	// plan without changed paths
	e := validPlan()
	e.ChangedPaths = nil
	if err := e.Validate(); err == nil {
		t.Fatal("plan without changed paths accepted")
	}
	// plan without next commands
	n := validPlan()
	n.NextCommands = nil
	if err := n.Validate(); err == nil {
		t.Fatal("plan without next commands accepted")
	}
	// direct plan marked structured_required
	d := validPlan()
	d.StructuredRequired = true
	if err := d.Validate(); err == nil {
		t.Fatal("direct plan marked structured_required accepted")
	}
}

func TestCheckResultValidate(t *testing.T) {
	directComplete := CheckResult{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"}
	if err := directComplete.Validate(); err != nil {
		t.Fatalf("valid direct complete rejected: %v", err)
	}
	structuredComplete := CheckResult{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckComplete, Coverage: CoverageComplete, Recommendation: "approve"}
	if err := structuredComplete.Validate(); err != nil {
		t.Fatalf("valid structured complete rejected: %v", err)
	}
	incomplete := CheckResult{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckIncomplete, Coverage: CoverageIncomplete, Recommendation: "incomplete", Problems: []string{"missing receipt u_1"}}
	if err := incomplete.Validate(); err != nil {
		t.Fatalf("valid incomplete rejected: %v", err)
	}
	if !structuredComplete.Complete() || incomplete.Complete() {
		t.Fatal("Complete() derived wrongly from status")
	}

	bad := []CheckResult{
		{Schema: "review.check.v2", ReviewID: testReviewID, Mode: ModeDirect, Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"},
		{Schema: CheckSchemaV1, ReviewID: "rvw_1", Mode: ModeDirect, Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"},
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: "sideways", Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"},
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: "unknown", Coverage: CoverageNotRequiredDirect, Recommendation: "comment"},
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: CheckComplete, Coverage: CoverageComplete, Recommendation: "comment"},                                  // direct must be not_required_direct
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "comment"},                     // structured must not be not_required_direct
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckComplete, Coverage: CoverageIncomplete, Recommendation: "comment"},                            // complete needs complete coverage
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: CheckComplete, Coverage: CoverageNotRequiredDirect, Recommendation: "incomplete"},                      // complete cannot recommend incomplete
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckComplete, Coverage: CoverageComplete, Recommendation: "approve", Problems: []string{"x"}},     // complete has no problems
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckIncomplete, Coverage: CoverageIncomplete, Recommendation: "incomplete"},                       // incomplete needs problems
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeStructured, Status: CheckIncomplete, Coverage: CoverageIncomplete, Recommendation: "comment", Problems: []string{"x"}}, // incomplete needs incomplete recommendation
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: CheckStale, Coverage: CoverageNotRequiredDirect, Recommendation: "comment", Problems: []string{"x"}},   // stale needs incomplete recommendation
		{Schema: CheckSchemaV1, ReviewID: testReviewID, Mode: ModeDirect, Status: CheckStale, Coverage: CoverageNotRequiredDirect, Recommendation: "incomplete"},                         // stale needs problems
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("bad check result %d accepted", i)
		}
	}
}
