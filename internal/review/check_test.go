package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type cancelAfterErrContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (c *cancelAfterErrContext) Err() error {
	c.checks++
	if c.cancelAt > 0 && c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func checkReportFor(artifacts PlanArtifacts) Report {
	return Report{
		Schema:         ReportSchemaV1,
		ReviewID:       artifacts.Plan.ReviewID,
		PlanDigest:     artifacts.Plan.PlanDigest,
		Base:           artifacts.Plan.Scope.Base,
		Head:           artifacts.Plan.Scope.Head,
		Recommendation: string(RecommendApprove),
		Findings:       []Finding{},
	}
}

func checkServiceFor(artifacts PlanArtifacts) *Service {
	svc := NewService(ServiceOptions{Root: "."})
	svc.store = &serviceTestStore{artifacts: artifacts}
	return svc
}

func TestCheckDirectNoReceipts(t *testing.T) {
	artifacts := makeArtifacts("check-direct")
	report := checkReportFor(artifacts)

	result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := CheckResult{
		Schema: CheckSchemaV1, ReviewID: artifacts.Plan.ReviewID, Mode: ModeDirect,
		Status: CheckComplete, Coverage: CoverageNotRequiredDirect,
		Recommendation: string(RecommendApprove),
	}
	if result.Schema != want.Schema || result.ReviewID != want.ReviewID || result.Mode != want.Mode ||
		result.Status != want.Status || result.Coverage != want.Coverage || result.Recommendation != want.Recommendation || len(result.Problems) != 0 {
		t.Fatalf("result = %#v, want %#v", result, want)
	}
}

func TestCheckInvalidReportReturnsTypedNonCompleteError(t *testing.T) {
	artifacts := makeArtifacts("check-invalid")
	report := checkReportFor(artifacts)
	report.Schema = "review.report.v0"

	result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
	if result.Status != CheckInvalid || result.Recommendation != string(RecommendIncomplete) {
		t.Fatalf("result = %#v", result)
	}
	var checkErr *CheckError
	if !errors.As(err, &checkErr) {
		t.Fatalf("error = %T %v, want *CheckError", err, err)
	}
	if checkErr.Status != CheckInvalid || checkErr.Reason != "report_shape" {
		t.Fatalf("typed error = %#v", checkErr)
	}
	if len(result.Problems) != 1 || result.Problems[0] != "report_shape: review: report schema \"review.report.v0\" is not review.report.v1" {
		t.Fatalf("problems = %#v", result.Problems)
	}
}

func committedCheckArtifacts(t *testing.T, structured bool) (*Service, PlanArtifacts, string, string) {
	t.Helper()
	fixture := newGitFixture(t)
	fixture.write(t, "changed.go", "package p\n\nfunc Changed() int { return 1 }\n")
	fixture.write(t, "outside.go", "package p\n\nfunc Outside() int { return Changed() }\n")
	fixture.commit(t, "base")
	fixture.write(t, "changed.go", "package p\n\nfunc Changed() int { return 2 }\n")
	fixture.commit(t, "head")

	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD", ForceStructured: structured})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	store, err := OpenPlanStore(context.Background(), execRunner(t), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, loadErr := store.Load(context.Background(), plan.ReviewID)
	closeErr := store.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", loadErr, closeErr)
	}
	hunkID := ""
	if len(artifacts.PlanIdentity.Units) != 0 && len(artifacts.PlanIdentity.Units[0].HunkIDs) != 0 {
		hunkID = artifacts.PlanIdentity.Units[0].HunkIDs[0].Public
	}
	return svc, artifacts, hunkID, fixture.root
}

func rangeCheckArtifacts(t *testing.T) (*Service, PlanArtifacts) {
	t.Helper()
	fixture := newGitFixture(t)
	fixture.write(t, "changed.go", "package p\n\nfunc Changed() int { return 1 }\n")
	fixture.commit(t, "base")
	fixture.write(t, "changed.go", "package p\n\nfunc Changed() int { return 2 }\n")
	fixture.commit(t, "head")
	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	plan, err := svc.Plan(context.Background(), PlanRequest{Base: "HEAD~1", Head: "HEAD"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	store, err := OpenPlanStore(context.Background(), execRunner(t), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, loadErr := store.Load(context.Background(), plan.ReviewID)
	closeErr := store.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", loadErr, closeErr)
	}
	return svc, artifacts
}

func contentLocation(path, side, body string, start, end int) Location {
	sum := sha256.Sum256([]byte(body))
	return Location{
		Kind: string(LocationRange), Path: path, Side: side,
		ContentHash: hex.EncodeToString(sum[:]), Start: start, End: end,
	}
}

func checkFinding(id string, location Location, cause Cause, citation Citation) Finding {
	return Finding{
		ID: id, Causes: []Cause{cause},
		Category: string(CategoryFunctionalCorrectness), Severity: string(SeverityMinor),
		Confidence: string(ConfidenceHigh), Summary: "wrong result", Detail: "the changed return value breaks callers",
		Scenario: "calling Changed returns the wrong value", Locations: []Location{location},
		Citations: []Citation{citation}, IntroducedByChange: true,
		Verifier: string(DispositionConfirmed), VerifierEvidence: []Citation{citation},
	}
}

func assertCheckFailure(t *testing.T, result CheckResult, err error, status CheckStatus, reason, problem string) {
	t.Helper()
	var checkErr *CheckError
	if !errors.As(err, &checkErr) {
		t.Fatalf("error = %T %v, want *CheckError", err, err)
	}
	if result.Status != status || result.Recommendation != string(RecommendIncomplete) ||
		checkErr.Status != status || checkErr.Reason != reason {
		t.Fatalf("result=%#v error=%#v", result, checkErr)
	}
	if len(result.Problems) != 1 || result.Problems[0] != problem {
		t.Fatalf("problems = %#v, want %q", result.Problems, problem)
	}
}

func TestCheckDirectIdentityAndWorkspaceFreshness(t *testing.T) {
	t.Run("full plan digest", func(t *testing.T) {
		artifacts := makeArtifacts("check-plan-digest")
		report := checkReportFor(artifacts)
		report.PlanDigest = strings.Repeat("f", 64)
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		assertCheckFailure(t, result, err, CheckInvalid, "plan_digest",
			"plan_digest: report "+report.PlanDigest+" does not match persisted "+artifacts.Plan.PlanDigest)
	})

	t.Run("tagged base", func(t *testing.T) {
		artifacts := makeArtifacts("check-base")
		report := checkReportFor(artifacts)
		report.Base.Value = append([]byte(nil), report.Base.Value...)
		report.Base.Value[0] = 1
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		assertCheckFailure(t, result, err, CheckInvalid, "base_identity",
			"base_identity: report does not match persisted plan")
	})

	t.Run("fresh workspace", func(t *testing.T) {
		artifacts := makeWorkspaceArtifacts("check-workspace-fresh")
		capture := Capture{Scope: artifacts.Plan.Scope, CanonicalPatch: Digest{1}, Paths: []RawPathRecord{}}
		artifacts.WorkspaceFingerprint = hex.EncodeToString(capture.Scope.Head.Value)
		artifacts.WorkspaceCaptureFingerprint = canonicalCaptureFingerprint(capture)
		svc := checkServiceFor(artifacts)
		svc.captureOnce = func(context.Context, Scope) (Capture, error) { return capture, nil }
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, checkReportFor(artifacts))
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("stale workspace", func(t *testing.T) {
		artifacts := makeWorkspaceArtifacts("check-workspace-stale")
		accepted := Capture{Scope: artifacts.Plan.Scope, CanonicalPatch: Digest{1}}
		current := Capture{Scope: artifacts.Plan.Scope, CanonicalPatch: Digest{2}}
		artifacts.WorkspaceFingerprint = hex.EncodeToString(accepted.Scope.Head.Value)
		artifacts.WorkspaceCaptureFingerprint = canonicalCaptureFingerprint(accepted)
		svc := checkServiceFor(artifacts)
		svc.captureOnce = func(context.Context, Scope) (Capture, error) { return current, nil }
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, checkReportFor(artifacts))
		assertCheckFailure(t, result, err, CheckStale, "workspace_stale",
			"workspace_stale: canonical workspace capture changed")
		if !errors.Is(err, ErrCheckStale) {
			t.Fatalf("error %v does not wrap ErrCheckStale", err)
		}
	})

	t.Run("workspace HEAD movement is stale", func(t *testing.T) {
		artifacts := makeWorkspaceArtifacts("check-workspace-head-moved")
		accepted := Capture{Scope: artifacts.Plan.Scope, CanonicalPatch: Digest{1}}
		current := accepted
		current.Scope.Base.Value = append([]byte(nil), current.Scope.Base.Value...)
		current.Scope.Base.Value[0] = 7
		artifacts.WorkspaceFingerprint = hex.EncodeToString(accepted.Scope.Head.Value)
		current.Scope.Digest[0] ^= 1
		artifacts.WorkspaceCaptureFingerprint = canonicalCaptureFingerprint(accepted)
		svc := checkServiceFor(artifacts)
		svc.captureOnce = func(context.Context, Scope) (Capture, error) { return current, nil }
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, checkReportFor(artifacts))
		assertCheckFailure(t, result, err, CheckStale, "workspace_stale",
			"workspace_stale: canonical workspace capture changed")
	})
}

func TestCheckDirectLocationsSupportAndDuplicates(t *testing.T) {
	svc, artifacts, hunkID, _ := committedCheckArtifacts(t, false)
	report := checkReportFor(artifacts)
	citation := Citation{Kind: "hunk", ID: hunkID}
	cause := Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID}
	report.Findings = []Finding{checkFinding(
		"F-1",
		contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
		cause,
		citation,
	)}
	report.Recommendation = string(RecommendComment)

	t.Run("changed range resolves", func(t *testing.T) {
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("base range resolves inside changed hunk", func(t *testing.T) {
		candidate := report
		pathCitation := Citation{Kind: "path", ID: artifacts.Plan.ChangedPaths[0].PathID}
		candidate.Findings = []Finding{checkFinding(
			"F-base",
			contentLocation("changed.go", string(SideBase), "package p\n\nfunc Changed() int { return 1 }\n", 3, 3),
			cause,
			pathCitation,
		)}
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, candidate)
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("outside diff requires causal citation", func(t *testing.T) {
		candidate := report
		candidate.Findings = append([]Finding(nil), report.Findings...)
		candidate.Findings[0] = checkFinding(
			"F-outside",
			contentLocation("outside.go", string(SideHead), "package p\n\nfunc Outside() int { return Changed() }\n", 3, 3),
			cause,
			citation,
		)
		candidate.Findings[0].Citations = []Citation{{Kind: "path", ID: artifacts.Plan.ChangedPaths[0].PathID}}
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, candidate)
		assertCheckFailure(t, result, err, CheckInvalid, "outside_diff_support",
			"outside_diff_support: finding F-outside location outside.go:3-3 has no plan-owned causal citation")
	})

	t.Run("outside diff with causal citation", func(t *testing.T) {
		candidate := report
		candidate.Findings = []Finding{checkFinding(
			"F-outside",
			contentLocation("outside.go", string(SideHead), "package p\n\nfunc Outside() int { return Changed() }\n", 3, 3),
			cause,
			citation,
		)}
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, candidate)
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("range is bounded", func(t *testing.T) {
		candidate := report
		candidate.Findings = append([]Finding(nil), report.Findings...)
		candidate.Findings[0].Locations = []Location{
			contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 20),
		}
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, candidate)
		assertCheckFailure(t, result, err, CheckInvalid, "location",
			"location: finding F-1 location 0 range 3-20 exceeds changed.go head line count 3")
	})

	t.Run("duplicate locations", func(t *testing.T) {
		candidate := report
		candidate.Findings = append([]Finding(nil), report.Findings...)
		loc := candidate.Findings[0].Locations[0]
		candidate.Findings[0].Locations = []Location{loc, loc}
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, candidate)
		assertCheckFailure(t, result, err, CheckInvalid, "duplicate_location",
			"duplicate_location: finding F-1 repeats range:head:changed.go:3:3")
	})
}

func TestCheckDirectRecommendationThresholds(t *testing.T) {
	svc, artifacts, hunkID, _ := committedCheckArtifacts(t, false)
	citation := Citation{Kind: "hunk", ID: hunkID}
	finding := checkFinding(
		"F-severity",
		contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
		Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID},
		citation,
	)
	tests := []struct {
		name           string
		severity       Severity
		verifier       Disposition
		introduced     bool
		recommendation Recommendation
		wantStatus     CheckStatus
	}{
		{"confirmed introduced major may comment", SeverityMajor, DispositionConfirmed, true, RecommendComment, CheckComplete},
		{"confirmed introduced major may request changes", SeverityMajor, DispositionConfirmed, true, RecommendRequestChanges, CheckComplete},
		{"introduced major cannot approve", SeverityMajor, DispositionConfirmed, true, RecommendApprove, CheckInvalid},
		{"plausible critical requests changes", SeverityCritical, DispositionPlausible, true, RecommendRequestChanges, CheckComplete},
		{"pre-existing critical requests changes", SeverityCritical, DispositionConfirmed, false, RecommendRequestChanges, CheckComplete},
		{"pre-existing critical cannot comment", SeverityCritical, DispositionConfirmed, false, RecommendComment, CheckInvalid},
		{"minor may request changes", SeverityMinor, DispositionConfirmed, true, RecommendRequestChanges, CheckComplete},
		{"pre-existing major may comment", SeverityMajor, DispositionConfirmed, false, RecommendComment, CheckComplete},
		{"unverified is incomplete", SeverityMinor, DispositionUnverified, true, RecommendIncomplete, CheckIncomplete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := finding
			candidate.Severity = string(tc.severity)
			candidate.Verifier = string(tc.verifier)
			candidate.IntroducedByChange = tc.introduced
			report := checkReportFor(artifacts)
			report.Findings = []Finding{candidate}
			report.Recommendation = string(tc.recommendation)
			result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
			if tc.wantStatus == CheckComplete {
				if err != nil || result.Status != CheckComplete {
					t.Fatalf("result=%#v error=%v", result, err)
				}
				return
			}
			var checkErr *CheckError
			if !errors.As(err, &checkErr) || result.Status != tc.wantStatus || checkErr.Status != tc.wantStatus {
				t.Fatalf("result=%#v error=%v", result, err)
			}
		})
	}
}

func TestCheckFindingShape(t *testing.T) {
	svc, artifacts, hunkID, _ := committedCheckArtifacts(t, false)
	citation := Citation{Kind: "hunk", ID: hunkID}
	valid := checkFinding(
		"F-shape",
		contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
		Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID},
		citation,
	)
	tests := []struct {
		name   string
		mutate func(*Report)
		reason string
	}{
		{"duplicate finding id", func(report *Report) { report.Findings = append(report.Findings, report.Findings[0]) }, "report_shape"},
		{"missing category", func(report *Report) { report.Findings[0].Category = "" }, "report_shape"},
		{"missing severity", func(report *Report) { report.Findings[0].Severity = "" }, "report_shape"},
		{"missing confidence", func(report *Report) { report.Findings[0].Confidence = "" }, "report_shape"},
		{"missing scenario", func(report *Report) { report.Findings[0].Scenario = "" }, "report_shape"},
		{"missing detail", func(report *Report) { report.Findings[0].Detail = "" }, "report_shape"},
		{"missing support", func(report *Report) { report.Findings[0].Citations = nil }, "citation"},
		{"missing verifier evidence", func(report *Report) { report.Findings[0].VerifierEvidence = nil }, "verifier_evidence"},
		{"foreign cause", func(report *Report) { report.Findings[0].Causes[0].ID = "u_00000000000000000000000000000000" }, "cause"},
		{"rejection needs typed reason", func(report *Report) { report.Findings[0].Verifier = string(DispositionRejected) }, "report_shape"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := checkReportFor(artifacts)
			report.Findings = []Finding{valid}
			report.Recommendation = string(RecommendComment)
			tc.mutate(&report)
			result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
			var checkErr *CheckError
			if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != tc.reason {
				t.Fatalf("result=%#v error=%#v", result, checkErr)
			}
		})
	}
}

func completeStructuredReport(artifacts PlanArtifacts) Report {
	report := checkReportFor(artifacts)
	for _, unit := range artifacts.PlanIdentity.Units {
		hunks := make([]string, len(unit.HunkIDs))
		for i, hunk := range unit.HunkIDs {
			hunks[i] = hunk.Public
		}
		report.PrimaryReceipts = append(report.PrimaryReceipts, PrimaryReceipt{
			UnitID: unit.UnitID.Public, AcknowledgedPrimaryHunkIDs: hunks, Reviewer: "primary-reviewer",
		})
	}
	for _, audit := range artifacts.Plan.RequiredAudits {
		report.AuditReceipts = append(report.AuditReceipts, AuditReceipt{
			AuditID: audit.AuditID, AcknowledgedAuditTargetIDs: append([]string(nil), audit.TargetIDs...), Reviewer: "audit-reviewer",
		})
	}
	return report
}

func TestCheckStructuredExactCoverageAndEmptyAudits(t *testing.T) {
	artifacts := makeStructuredArtifacts("check-structured-complete")
	report := completeStructuredReport(artifacts)
	result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete || result.Coverage != CoverageComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	empty := 0
	for _, receipt := range report.AuditReceipts {
		if len(receipt.AcknowledgedAuditTargetIDs) == 0 {
			empty++
		}
	}
	if empty != 3 {
		t.Fatalf("explicit empty audit receipts=%d, want 3", empty)
	}
}

func TestCheckStructuredReceiptSetFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Report)
		status  CheckStatus
		reason  string
		problem string
	}{
		{
			name: "missing primary", status: CheckIncomplete, reason: "primary_coverage",
			mutate:  func(report *Report) { report.PrimaryReceipts = nil },
			problem: "primary_coverage: missing receipt for unit PLACEHOLDER",
		},
		{
			name: "foreign primary", status: CheckInvalid, reason: "primary_receipt",
			mutate:  func(report *Report) { report.PrimaryReceipts[0].UnitID = "u_00000000000000000000000000000000" },
			problem: "primary_receipt: foreign unit u_00000000000000000000000000000000",
		},
		{
			name: "duplicate primary", status: CheckInvalid, reason: "primary_receipt",
			mutate: func(report *Report) {
				report.PrimaryReceipts = append(report.PrimaryReceipts, report.PrimaryReceipts[0])
			},
			problem: "primary_receipt: duplicate receipt for unit PLACEHOLDER",
		},
		{
			name: "primary missing hunk", status: CheckIncomplete, reason: "primary_coverage",
			mutate:  func(report *Report) { report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs = nil },
			problem: "primary_coverage: unit PLACEHOLDER missing hunk HUNK",
		},
		{
			name: "primary foreign hunk", status: CheckInvalid, reason: "primary_coverage",
			mutate: func(report *Report) {
				report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs = append(report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs, "h_00000000000000000000000000000000")
			},
			problem: "primary_coverage: unit PLACEHOLDER acknowledges foreign hunk h_00000000000000000000000000000000",
		},
		{
			name: "primary duplicate hunk", status: CheckInvalid, reason: "primary_coverage",
			mutate: func(report *Report) {
				report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs = append(report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs, report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs[0])
			},
			problem: "primary_coverage: unit PLACEHOLDER repeats hunk HUNK",
		},
		{
			name: "missing audit", status: CheckIncomplete, reason: "audit_coverage",
			mutate:  func(report *Report) { report.AuditReceipts = report.AuditReceipts[:3] },
			problem: "audit_coverage: missing receipt for audit " + AuditIntegrationGapV1,
		},
		{
			name: "duplicate audit", status: CheckInvalid, reason: "audit_receipt",
			mutate:  func(report *Report) { report.AuditReceipts = append(report.AuditReceipts, report.AuditReceipts[0]) },
			problem: "audit_receipt: duplicate receipt for audit " + AuditRemovedBehaviorV1,
		},
		{
			name: "audit missing target", status: CheckIncomplete, reason: "audit_coverage",
			mutate:  func(report *Report) { report.AuditReceipts[0].AcknowledgedAuditTargetIDs = nil },
			problem: "audit_coverage: audit " + AuditRemovedBehaviorV1 + " missing target TARGET",
		},
		{
			name: "audit foreign target", status: CheckInvalid, reason: "audit_coverage",
			mutate: func(report *Report) {
				report.AuditReceipts[0].AcknowledgedAuditTargetIDs = append(report.AuditReceipts[0].AcknowledgedAuditTargetIDs, "t_00000000000000000000000000000000")
			},
			problem: "audit_coverage: audit " + AuditRemovedBehaviorV1 + " acknowledges foreign target t_00000000000000000000000000000000",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifacts := makeStructuredArtifacts("check-structured-" + strings.ReplaceAll(tc.name, " ", "-"))
			report := completeStructuredReport(artifacts)
			tc.mutate(&report)
			unitID := artifacts.Plan.PrimaryUnits[0].UnitID
			hunkID := artifacts.PlanIdentity.Units[0].HunkIDs[0].Public
			targetID := artifacts.Plan.RequiredAudits[0].TargetIDs[0]
			want := strings.NewReplacer("PLACEHOLDER", unitID, "HUNK", hunkID, "TARGET", targetID).Replace(tc.problem)
			result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
			assertCheckFailure(t, result, err, tc.status, tc.reason, want)
			if result.Coverage != CoverageIncomplete {
				t.Fatalf("coverage=%s, want incomplete", result.Coverage)
			}
		})
	}

	t.Run("primary and audit domains are independent", func(t *testing.T) {
		artifacts := makeStructuredArtifacts("check-domain-independence")
		report := completeStructuredReport(artifacts)
		report.PrimaryReceipts[0].AcknowledgedPrimaryHunkIDs = []string{artifacts.Plan.RequiredAudits[0].TargetIDs[0]}
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "primary_coverage" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
	})

	t.Run("duplicate audit target", func(t *testing.T) {
		artifacts := makeStructuredArtifacts("check-duplicate-audit-target")
		report := completeStructuredReport(artifacts)
		report.AuditReceipts[0].AcknowledgedAuditTargetIDs = append(
			report.AuditReceipts[0].AcknowledgedAuditTargetIDs,
			report.AuditReceipts[0].AcknowledgedAuditTargetIDs[0],
		)
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "audit_coverage" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
	})
}

func rebindCheckArtifacts(artifacts *PlanArtifacts) {
	artifacts.PlanIdentityBytes, artifacts.Plan.ReviewID, artifacts.Plan.PlanDigest = identityFrom(artifacts.PlanIdentity)
	for i := range artifacts.Plan.PrimaryUnits {
		artifacts.Plan.PrimaryUnits[i].ReviewID = artifacts.Plan.ReviewID
		unitID := artifacts.Plan.PrimaryUnits[i].UnitID
		artifacts.MandatoryUnits[unitID] = mustCanonicalUnit(artifacts.Plan.PrimaryUnits[i])
	}
}

func TestCheckStructuredUncertaintyAndExplicitTargets(t *testing.T) {
	artifacts := makeStructuredArtifacts("check-structured-uncertainty")

	t.Run("blocking uncertainty is incomplete", func(t *testing.T) {
		report := completeStructuredReport(artifacts)
		report.PrimaryReceipts[0].Uncertainties = []Uncertainty{{Blocking: true, Summary: "generated file could not be reviewed"}}
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		assertCheckFailure(t, result, err, CheckIncomplete, "blocking_uncertainty",
			"blocking_uncertainty: primary unit "+report.PrimaryReceipts[0].UnitID+": generated file could not be reviewed")
	})

	t.Run("non-blocking uncertainty remains complete", func(t *testing.T) {
		report := completeStructuredReport(artifacts)
		report.AuditReceipts[0].Uncertainties = []Uncertainty{{Summary: "binary format was inspected structurally"}}
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("partial and unreviewable target stays explicit", func(t *testing.T) {
		candidate := makeStructuredArtifacts("check-structured-unreviewable")
		candidate.Plan.ChangedPaths[0].ReviewClass = string(ReviewClassUnreviewable)
		candidate.Plan.ChangedPaths[0].Coverage = string(PathCoverageNone)
		candidate.Plan.ChangedPaths[0].Reason = "special path"
		candidate.PlanIdentity.Paths[0].ReviewClass = string(ReviewClassUnreviewable)
		candidate.PlanIdentity.Paths[0].Coverage = string(PathCoverageNone)
		candidate.PlanIdentity.Paths[0].Reason = "special path"
		pathID := candidate.PlanIdentity.Paths[0].PathID
		candidate.Plan.RequiredAudits[3].TargetIDs = []string{pathID.Public}
		candidate.PlanIdentity.Audits[3].TargetIDs = []StableID{pathID}
		rebindCheckArtifacts(&candidate)
		report := completeStructuredReport(candidate)
		if got := report.AuditReceipts[3].AcknowledgedAuditTargetIDs; len(got) != 1 || got[0] != pathID.Public {
			t.Fatalf("explicit unreviewable target=%v, want %s", got, pathID.Public)
		}
		result, err := checkServiceFor(candidate).Check(context.Background(), candidate.Plan.ReviewID, report)
		if err != nil || result.Status != CheckComplete {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})
}
func TestCheckStructuredReceiptFindingAndForeignAudit(t *testing.T) {
	t.Run("foreign finding id", func(t *testing.T) {
		artifacts := makeStructuredArtifacts("check-receipt-finding")
		report := completeStructuredReport(artifacts)
		report.PrimaryReceipts[0].FindingIDs = []string{"F-foreign"}
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "receipt_finding" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
	})

	t.Run("foreign audit id", func(t *testing.T) {
		artifacts := makeStructuredArtifacts("check-foreign-audit")
		report := completeStructuredReport(artifacts)
		report.AuditReceipts[0].AuditID = "audit_foreign_v1"
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, report)
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "report_shape" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
	})
}

func cloneCitationProofMap(in map[string]CitationProof) map[string]CitationProof {
	out := make(map[string]CitationProof, len(in))
	for id, proof := range in {
		out[id] = proof
	}
	return out
}

func TestCheckCitationProofFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*PlanArtifacts, *Report, string)
		reason string
	}{
		{
			name: "invented citation",
			mutate: func(_ *PlanArtifacts, report *Report, _ string) {
				report.Findings[0].Citations[0].ID = "h_00000000000000000000000000000000"
			},
			reason: "citation",
		},
		{
			name: "missing persisted proof",
			mutate: func(artifacts *PlanArtifacts, _ *Report, hunkID string) {
				delete(artifacts.Citations, hunkID)
			},
			reason: "citation",
		},
		{
			name: "foreign plan citation",
			mutate: func(_ *PlanArtifacts, report *Report, _ string) {
				report.Findings[0].Citations[0].ID = "h_11111111111111111111111111111111"
			},
			reason: "citation",
		},
		{
			name: "wrong side",
			mutate: func(artifacts *PlanArtifacts, _ *Report, hunkID string) {
				proof := artifacts.Citations[hunkID]
				proof.Side = SideBase
				artifacts.Citations[hunkID] = proof
			},
			reason: "citation_proof",
		},
		{
			name: "wrong range",
			mutate: func(artifacts *PlanArtifacts, _ *Report, hunkID string) {
				proof := artifacts.Citations[hunkID]
				proof.End++
				artifacts.Citations[hunkID] = proof
			},
			reason: "citation_proof",
		},
		{
			name: "stale content",
			mutate: func(artifacts *PlanArtifacts, _ *Report, hunkID string) {
				proof := artifacts.Citations[hunkID]
				proof.ContentHash = strings.Repeat("f", 64)
				artifacts.Citations[hunkID] = proof
			},
			reason: "citation_proof",
		},
		{
			name: "duplicate citation",
			mutate: func(_ *PlanArtifacts, report *Report, _ string) {
				report.Findings[0].Citations = append(report.Findings[0].Citations, report.Findings[0].Citations[0])
			},
			reason: "duplicate_citation",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, artifacts, hunkID, _ := committedCheckArtifacts(t, true)
			artifacts.Citations = cloneCitationProofMap(artifacts.Citations)
			report := completeStructuredReport(artifacts)
			citation := Citation{Kind: "hunk", ID: hunkID}
			report.Findings = []Finding{checkFinding(
				"F-proof",
				contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
				Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID},
				citation,
			)}
			report.Recommendation = string(RecommendComment)
			tc.mutate(&artifacts, &report, hunkID)
			svc.store = &serviceTestStore{artifacts: artifacts}
			result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
			var checkErr *CheckError
			if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != tc.reason {
				t.Fatalf("result=%#v error=%#v", result, checkErr)
			}
		})
	}
}

func TestCheckStructuredReceiptContextAndRejectionEvidence(t *testing.T) {
	svc, artifacts, hunkID, _ := committedCheckArtifacts(t, true)
	report := completeStructuredReport(artifacts)
	citation := Citation{Kind: "hunk", ID: hunkID}
	finding := checkFinding(
		"F-rejected",
		contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
		Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID},
		citation,
	)
	finding.Verifier = string(DispositionRejected)
	finding.RejectionReason = string(RejectionContradictedByCode)
	report.Findings = []Finding{finding}
	report.PrimaryReceipts[0].ContextCitations = []Citation{citation}
	report.PrimaryReceipts[0].FindingIDs = []string{finding.ID}
	report.Recommendation = string(RecommendComment)

	result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}

	report.PrimaryReceipts[0].ContextCitations[0].ID = "h_00000000000000000000000000000000"
	result, err = svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	var checkErr *CheckError
	if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "citation" {
		t.Fatalf("result=%#v error=%#v", result, checkErr)
	}
}

func TestCheckDirectHunklessOutsideDiffSupport(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.write(t, "mode.go", "package p\n\nfunc Mode() {}\n")
	fixture.write(t, "outside.go", "package p\n\nfunc OutsideMode() { Mode() }\n")
	fixture.commit(t, "base")
	if err := os.Chmod(filepath.Join(fixture.root, "mode.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.commit(t, "mode change")

	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenPlanStore(context.Background(), execRunner(t), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.Load(context.Background(), plan.ReviewID)
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", err, closeErr)
	}
	if len(artifacts.Plan.PrimaryUnits) != 0 {
		t.Fatalf("mode-only plan has %d primary units, want zero", len(artifacts.Plan.PrimaryUnits))
	}
	pathID := artifacts.Plan.ChangedPaths[0].PathID
	body := "package p\n\nfunc OutsideMode() { Mode() }\n"
	sum := sha256.Sum256([]byte(body))
	artifacts.Citations[pathID] = CitationProof{
		ID: pathID, Side: SideHead, Path: "outside.go", ContentHash: hex.EncodeToString(sum[:]), Start: 1, End: 3,
	}
	svc.store = &serviceTestStore{artifacts: artifacts}
	citation := Citation{Kind: "changed_path", ID: pathID}
	report := checkReportFor(artifacts)
	report.Findings = []Finding{checkFinding(
		"F-hunkless", contentLocation("outside.go", string(SideHead), body, 3, 3),
		Cause{Kind: string(CauseChangedPath), ID: pathID}, citation,
	)}
	report.Recommendation = string(RecommendComment)
	result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestCheckGitlinkLocation(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.commitFile(t, "base.go", "package p\n")
	oid := fixture.initNestedGitlink(t, "dep", "nested\n")
	fixture.commit(t, "add gitlink")
	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenPlanStore(context.Background(), execRunner(t), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.Load(context.Background(), plan.ReviewID)
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", err, closeErr)
	}
	pathID := artifacts.Plan.ChangedPaths[0].PathID
	empty := sha256.Sum256(nil)
	artifacts.Citations[pathID] = CitationProof{
		ID: pathID, Side: SideHead, Path: "dep", ContentHash: hex.EncodeToString(empty[:]), Start: 1, End: 1,
	}
	svc.store = &serviceTestStore{artifacts: artifacts}
	identityBytes, err := hex.DecodeString(oid)
	if err != nil {
		t.Fatal(err)
	}
	location := Location{
		Kind: string(LocationPath), Path: "dep", Side: string(SideHead),
		Identity: &SideIdentity{Kind: SideGitOID, Value: identityBytes}, Mode: modeGitlink, EntryType: "gitlink",
	}
	citation := Citation{Kind: "changed_path", ID: pathID}
	report := checkReportFor(artifacts)
	report.Findings = []Finding{checkFinding("F-gitlink", location, Cause{Kind: string(CauseChangedPath), ID: pathID}, citation)}
	report.Recommendation = string(RecommendComment)
	result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestCheckTamperedDigestBoundsAndCancellation(t *testing.T) {
	t.Run("tampered canonical digest", func(t *testing.T) {
		artifacts := makeArtifacts("check-tampered")
		artifacts.PlanIdentityBytes = append([]byte(nil), artifacts.PlanIdentityBytes...)
		artifacts.PlanIdentityBytes[len(artifacts.PlanIdentityBytes)-1] ^= 1
		result, err := checkServiceFor(artifacts).Check(context.Background(), artifacts.Plan.ReviewID, checkReportFor(artifacts))
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "plan_identity" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
	})

	t.Run("bounded before store load", func(t *testing.T) {
		artifacts := makeArtifacts("check-bounds")
		report := checkReportFor(artifacts)
		report.Findings = make([]Finding, maxCheckFindingsV1+1)
		svc := checkServiceFor(artifacts)
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
		var checkErr *CheckError
		if !errors.As(err, &checkErr) || result.Status != CheckInvalid || checkErr.Reason != "report_bounds" {
			t.Fatalf("result=%#v error=%#v", result, checkErr)
		}
		if svc.store.(*serviceTestStore).loads != 0 {
			t.Fatal("oversized report reached persistence")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		artifacts := makeArtifacts("check-canceled")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := checkServiceFor(artifacts).Check(ctx, artifacts.Plan.ReviewID, checkReportFor(artifacts))
		if !errors.Is(err, context.Canceled) || result.Schema != "" || result.Status != "" {
			t.Fatalf("result=%#v error=%v", result, err)
		}
	})

	t.Run("canceled during validator indexing", func(t *testing.T) {
		artifacts := makeStructuredArtifacts("check-index-canceled")
		ctx := &cancelAfterErrContext{Context: context.Background(), cancelAt: 2}
		if _, err := newReportValidator(ctx, checkServiceFor(artifacts), artifacts); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context.Canceled", err)
		}
	})

	t.Run("empty immutable report canceled before success", func(t *testing.T) {
		for _, kind := range []ScopeKind{ScopeCommit, ScopeRange} {
			t.Run(string(kind), func(t *testing.T) {
				var svc *Service
				var artifacts PlanArtifacts
				if kind == ScopeRange {
					svc, artifacts = rangeCheckArtifacts(t)
				} else {
					artifacts = makeArtifacts("check-final-canceled-" + string(kind))
					svc = checkServiceFor(artifacts)
				}
				report := checkReportFor(artifacts)
				probe := &cancelAfterErrContext{Context: context.Background()}
				result, err := svc.Check(probe, artifacts.Plan.ReviewID, report)
				if err != nil || result.Status != CheckComplete || probe.checks < 2 {
					t.Fatalf("probe checks=%d result=%#v error=%v", probe.checks, result, err)
				}
				ctx := &cancelAfterErrContext{Context: context.Background(), cancelAt: probe.checks}
				result, err = svc.Check(ctx, artifacts.Plan.ReviewID, report)
				if !errors.Is(err, context.Canceled) || result.Schema != "" || result.Status != "" {
					t.Fatalf("checks=%d result=%#v error=%v", ctx.checks, result, err)
				}
			})
		}
	})
}

func TestCheckSpecialLocation(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.write(t, "base.go", "package p\n")
	fixture.write(t, "event.pipe", "regular before\n")
	fixture.commit(t, "base")
	if err := os.Remove(filepath.Join(fixture.root, "event.pipe")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(fixture.root, "event.pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := captureWorkspace(t, fixture)
	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	svc.graph = fakeGraphQueries{}
	width, ok := oidWidthFor(capture.Scope.ObjectFormat)
	if !ok {
		t.Fatal("fixture has unknown object format")
	}
	view := &HeadView{Sources: newSourceResolver(HeadViewOptions{
		Runner: execRunner(t), RepoRoot: fixture.root, ObjectFormat: capture.Scope.ObjectFormat,
		BaseTreeish: hex.EncodeToString(capture.Scope.Base.Value),
	}, width, true)}
	artifacts, err := svc.buildArtifacts(context.Background(), capture, view, "", false)
	if err != nil {
		t.Fatal(err)
	}
	pathID := artifacts.Plan.ChangedPaths[0].PathID
	empty := sha256.Sum256(nil)
	artifacts.Citations[pathID] = CitationProof{
		ID: pathID, Side: SideHead, Path: "event.pipe", ContentHash: hex.EncodeToString(empty[:]), Start: 1, End: 1,
	}
	svc.store = &serviceTestStore{artifacts: artifacts}
	svc.captureOnce = func(context.Context, Scope) (Capture, error) { return capture, nil }
	location := Location{
		Kind: string(LocationPath), Path: "event.pipe", Side: string(SideHead),
		Identity: &SideIdentity{Kind: SideAbsent}, EntryType: "special",
	}
	citation := Citation{Kind: "changed_path", ID: pathID}
	report := checkReportFor(artifacts)
	report.Findings = []Finding{checkFinding("F-special", location, Cause{Kind: string(CauseChangedPath), ID: pathID}, citation)}
	report.Recommendation = string(RecommendComment)
	result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestCheckAuditTargetOutsideDiffSupport(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.write(t, "changed.go", "package p\n\nfunc Changed() int { return 1 }\n")
	fixture.write(t, "other.go", "package p\n\nfunc Other() int { return 1 }\n")
	fixture.write(t, "outside.go", "package p\n\nfunc Outside() int { return Changed() }\n")
	fixture.commit(t, "base")
	fixture.write(t, "changed.go", "package p\n\nfunc Changed(value int) int { return value }\n")
	fixture.write(t, "other.go", "package p\n\nfunc Other() int { return 2 }\n")
	fixture.commit(t, "signature")
	svc := NewService(ServiceOptions{Root: fixture.root, Git: execRunner(t)})
	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD", ForceStructured: true})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenPlanStore(context.Background(), execRunner(t), fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := store.Load(context.Background(), plan.ReviewID)
	if closeErr := store.Close(); err != nil || closeErr != nil {
		t.Fatalf("load=%v close=%v", err, closeErr)
	}

	paths := map[string]StableID{}
	for _, path := range artifacts.Plan.ChangedPaths {
		for _, identity := range artifacts.PlanIdentity.Paths {
			if path.PathID == identity.PathID.Public {
				paths[path.NewPath] = identity.PathID
			}
		}
	}
	units := map[string]PlanUnitEntry{}
	for _, unit := range artifacts.PlanIdentity.Units {
		units[unit.UnitID.Public] = unit
	}
	hunkForPath := func(pathID string) (StableID, StableID) {
		t.Helper()
		for _, unit := range artifacts.Plan.PrimaryUnits {
			identity := units[unit.UnitID]
			for i, hunk := range unit.Hunks {
				if hunk.PathID == pathID && i < len(identity.HunkIDs) {
					return identity.HunkIDs[i], identity.UnitID
				}
			}
		}
		t.Fatalf("no primary hunk for path %s", pathID)
		return StableID{}, StableID{}
	}
	changedPath := paths["changed.go"]
	otherPath := paths["other.go"]
	changedHunk, changedUnit := hunkForPath(changedPath.Public)
	otherHunk, _ := hunkForPath(otherPath.Public)
	for _, unit := range artifacts.Plan.PrimaryUnits {
		identity := units[unit.UnitID]
		for i, hunk := range unit.Hunks {
			if i >= len(identity.HunkIDs) || identity.HunkIDs[i].Public != otherHunk.Public {
				continue
			}
			checked, checkErr := checkedHunkFrom(unit.UnitID, otherHunk.Public, hunk)
			if checkErr != nil {
				t.Fatal(checkErr)
			}
			artifacts.Citations[otherHunk.Public] = CitationProof{
				ID: otherHunk.Public, Side: checked.side, Path: checked.path,
				ContentHash: checked.patchSum, Start: checked.start, End: checked.end,
			}
		}
	}
	var changedCohort StableID
	for _, cohort := range artifacts.PlanIdentity.Cohorts {
		for _, unit := range cohort.UnitIDs {
			if unit.Public == changedUnit.Public {
				changedCohort = cohort.CohortID
			}
		}
	}
	var semanticTarget StableID
	for _, audit := range artifacts.PlanIdentity.Audits {
		for _, target := range audit.TargetIDs {
			if strings.HasPrefix(target.Public, TargetIDPrefixV1) {
				semanticTarget = target
			}
		}
	}
	if changedPath.Public == "" || otherPath.Public == "" || changedCohort.Public == "" || semanticTarget.Public == "" {
		t.Fatalf("missing production target ids: changed=%s other=%s cohort=%s target=%s", changedPath.Public, otherPath.Public, changedCohort.Public, semanticTarget.Public)
	}

	targets := []StableID{changedHunk, changedPath, changedUnit, changedCohort, semanticTarget}
	artifacts.Plan.RequiredAudits[0].TargetIDs = nil
	artifacts.PlanIdentity.Audits[0].TargetIDs = nil
	for _, target := range targets {
		artifacts.Plan.RequiredAudits[0].TargetIDs = append(artifacts.Plan.RequiredAudits[0].TargetIDs, target.Public)
		artifacts.PlanIdentity.Audits[0].TargetIDs = append(artifacts.PlanIdentity.Audits[0].TargetIDs, target)
	}
	rebindCheckArtifacts(&artifacts)
	svc.store = &serviceTestStore{artifacts: artifacts}

	location := contentLocation("outside.go", string(SideHead), "package p\n\nfunc Outside() int { return Changed() }\n", 3, 3)
	causalCitation := Citation{Kind: "hunk", ID: changedHunk.Public}
	for _, target := range targets {
		t.Run(target.Public[:2], func(t *testing.T) {
			report := completeStructuredReport(artifacts)
			report.Findings = []Finding{checkFinding(
				"F-audit-"+target.Public[:1],
				location,
				Cause{Kind: string(CauseAuditTarget), ID: target.Public},
				causalCitation,
			)}
			report.Recommendation = string(RecommendComment)
			result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
			if err != nil || result.Status != CheckComplete {
				t.Fatalf("target=%s result=%#v error=%v", target.Public, result, err)
			}
		})
	}

	t.Run("noncausal hunk", func(t *testing.T) {
		report := completeStructuredReport(artifacts)
		report.Findings = []Finding{checkFinding(
			"F-audit-noncausal",
			location,
			Cause{Kind: string(CauseAuditTarget), ID: changedPath.Public},
			Citation{Kind: "hunk", ID: otherHunk.Public},
		)}
		report.Recommendation = string(RecommendComment)
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
		assertCheckFailure(t, result, err, CheckInvalid, "outside_diff_support",
			"outside_diff_support: finding F-audit-noncausal location outside.go:3-3 has no plan-owned causal citation")
	})

	t.Run("foreign target", func(t *testing.T) {
		report := completeStructuredReport(artifacts)
		report.Findings = []Finding{checkFinding(
			"F-audit-foreign",
			location,
			Cause{Kind: string(CauseAuditTarget), ID: "t_00000000000000000000000000000000"},
			causalCitation,
		)}
		report.Recommendation = string(RecommendComment)
		result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
		assertCheckFailure(t, result, err, CheckInvalid, "cause",
			"cause: finding F-audit-foreign references foreign audit_target t_00000000000000000000000000000000")
	})
}

func TestCheckCommittedScopeRemainsBoundToImmutableOIDs(t *testing.T) {
	svc, artifacts, hunkID, root := committedCheckArtifacts(t, false)
	citation := Citation{Kind: "hunk", ID: hunkID}
	report := checkReportFor(artifacts)
	report.Findings = []Finding{checkFinding(
		"F-immutable",
		contentLocation("changed.go", string(SideHead), "package p\n\nfunc Changed() int { return 2 }\n", 3, 3),
		Cause{Kind: string(CausePrimaryUnit), ID: artifacts.Plan.PrimaryUnits[0].UnitID},
		citation,
	)}
	report.Recommendation = string(RecommendComment)
	if err := os.WriteFile(filepath.Join(root, "later.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rawGit(t, root, "add", "later.go")
	rawGit(t, root, "commit", "-qm", "move checked out branch")

	result, err := svc.Check(context.Background(), artifacts.Plan.ReviewID, report)
	if err != nil || result.Status != CheckComplete {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}
