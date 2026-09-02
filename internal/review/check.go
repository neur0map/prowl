package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	maxCheckFindingsV1        = 100_000
	maxCheckReceiptsV1        = 100_000
	maxCheckItemsPerFindingV1 = 100_000
	maxCheckItemsPerReceiptV1 = 100_000
	maxCheckPlanItemsV1       = 500_000
	maxCheckTotalReferencesV1 = 1_000_000
)

var (
	ErrCheckIncomplete = errors.New("review: check is incomplete")
	ErrCheckInvalid    = errors.New("review: check is invalid")
	ErrCheckStale      = errors.New("review: check is stale")
)

// CheckError is returned with every non-complete CheckResult. Reason is a stable
// machine-oriented code; the result carries the deterministic, actionable
// problem list.
type CheckError struct {
	Status   CheckStatus
	Reason   string
	Problems []string
}

func (e *CheckError) Error() string {
	if e == nil {
		return "review: check failed"
	}
	if len(e.Problems) != 0 {
		return fmt.Sprintf("review: check %s (%s): %s", e.Status, e.Reason, e.Problems[0])
	}
	return fmt.Sprintf("review: check %s (%s)", e.Status, e.Reason)
}

func (e *CheckError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Status {
	case CheckIncomplete:
		return ErrCheckIncomplete
	case CheckInvalid:
		return ErrCheckInvalid
	case CheckStale:
		return ErrCheckStale
	default:
		return nil
	}
}

// Check validates a canonical report against its persisted review plan. It
// checks deterministic evidence and coverage only; it never claims that a
// finding, severity, disposition, or recommendation is semantically true.
func (s *Service) Check(ctx context.Context, reviewID string, report Report) (result CheckResult, err error) {
	if err := ctx.Err(); err != nil {
		return CheckResult{}, err
	}
	if s == nil {
		return CheckResult{}, errors.New("review: nil service")
	}
	if strings.TrimSpace(s.root) == "" || s.git == nil || s.captureOnce == nil {
		return CheckResult{}, errors.New("review: service is incompletely configured for check")
	}
	if err := preflightReportBounds(ctx, report); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return bareInvalidResult(reviewID, report, "report_bounds", err)
	}
	if !validReviewID(reviewID) {
		return bareInvalidResult(report.ReviewID, report, "review_id", fmt.Errorf("review: malformed requested review id %q", reviewID))
	}

	planStore, closeStore, err := s.acquireStore(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	release, err := planStore.LockReview(ctx, reviewID)
	if err != nil {
		return CheckResult{}, err
	}
	defer release()
	artifacts, err := planStore.Load(ctx, reviewID)
	if err != nil {
		return CheckResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, err
	}

	result = baseCheckResult(artifacts.Plan, report.Recommendation)
	if err := preflightArtifactBounds(ctx, artifacts); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, CheckInvalid, "plan_identity", "plan_identity: "+err.Error())
	}
	if artifacts.Plan.ReviewID != reviewID {
		return nonComplete(result, CheckInvalid, "review_id", fmt.Sprintf("persisted_review_id: got %s want %s", artifacts.Plan.ReviewID, reviewID))
	}
	if err := verifyLoadedCheckArtifacts(ctx, artifacts); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, CheckInvalid, "plan_identity", "plan_identity: "+err.Error())
	}
	if err := validateReportShapeForCheck(ctx, report); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, CheckInvalid, "report_shape", "report_shape: "+err.Error())
	}
	if report.ReviewID != reviewID {
		return nonComplete(result, CheckInvalid, "review_id", fmt.Sprintf("review_id: report %s does not match requested %s", report.ReviewID, reviewID))
	}
	if report.PlanDigest != artifacts.Plan.PlanDigest {
		return nonComplete(result, CheckInvalid, "plan_digest", fmt.Sprintf("plan_digest: report %s does not match persisted %s", report.PlanDigest, artifacts.Plan.PlanDigest))
	}
	if !sameSide(report.Base, artifacts.Plan.Scope.Base) {
		return nonComplete(result, CheckInvalid, "base_identity", "base_identity: report does not match persisted plan")
	}
	if !sameSide(report.Head, artifacts.Plan.Scope.Head) {
		return nonComplete(result, CheckInvalid, "head_identity", "head_identity: report does not match persisted plan")
	}

	if staleReason, freshnessErr := s.checkWorkspaceFreshness(ctx, artifacts); freshnessErr != nil {
		return CheckResult{}, freshnessErr
	} else if staleReason != "" {
		return nonComplete(result, CheckStale, "workspace_stale", "workspace_stale: "+staleReason)
	}

	validator, err := newReportValidator(ctx, s, artifacts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, CheckInvalid, "plan_identity", "plan_identity: "+err.Error())
	}
	if issue := validator.validateFindings(ctx, report.Findings); issue != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, issue.status, issue.reason, issue.problem)
	}
	if artifacts.Plan.Mode == ModeStructured {
		if issue := validator.validateStructuredCoverage(ctx, report); issue != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CheckResult{}, ctxErr
			}
			return nonComplete(result, issue.status, issue.reason, issue.problem)
		}
	}
	if issue := validateRecommendationForCheck(ctx, report); issue != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResult{}, ctxErr
		}
		return nonComplete(result, issue.status, issue.reason, issue.problem)
	}
	if staleReason, freshnessErr := s.checkWorkspaceFreshness(ctx, artifacts); freshnessErr != nil {
		return CheckResult{}, freshnessErr
	} else if staleReason != "" {
		return nonComplete(result, CheckStale, "workspace_stale", "workspace_stale: "+staleReason)
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, err
	}

	result.Status = CheckComplete
	result.Problems = nil
	if result.Mode == ModeStructured {
		result.Coverage = CoverageComplete
	}
	return result, nil
}

func (s *Service) checkWorkspaceFreshness(ctx context.Context, artifacts PlanArtifacts) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if artifacts.Plan.Scope.Kind != ScopeWorkspace {
		return "", nil
	}
	capture, err := s.captureOnce(ctx, artifacts.Plan.Scope)
	if err != nil {
		if isCaptureMismatch(err) {
			return err.Error(), nil
		}
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if canonicalCaptureFingerprint(capture) != artifacts.WorkspaceCaptureFingerprint {
		return "canonical workspace capture changed", nil
	}
	if hex.EncodeToString(capture.Scope.Head.Value) != artifacts.WorkspaceFingerprint {
		return "workspace tree fingerprint changed", nil
	}
	return "", ctx.Err()
}

func baseCheckResult(plan Plan, recommendation string) CheckResult {
	coverage := CoverageNotRequiredDirect
	if plan.Mode == ModeStructured {
		coverage = CoverageIncomplete
	}
	return CheckResult{
		Schema: CheckSchemaV1, ReviewID: plan.ReviewID, Mode: plan.Mode,
		Status: CheckInvalid, Coverage: coverage, Recommendation: recommendation,
	}
}

func bareInvalidResult(reviewID string, report Report, reason string, cause error) (CheckResult, error) {
	if !validReviewID(reviewID) {
		reviewID = report.ReviewID
	}
	result := CheckResult{
		Schema: CheckSchemaV1, ReviewID: reviewID, Mode: ModeDirect,
		Status: CheckInvalid, Coverage: CoverageNotRequiredDirect,
		Recommendation: string(RecommendIncomplete), Problems: []string{reason + ": " + cause.Error()},
	}
	return result, &CheckError{Status: result.Status, Reason: reason, Problems: append([]string(nil), result.Problems...)}
}

func nonComplete(result CheckResult, status CheckStatus, reason, problem string) (CheckResult, error) {
	result.Status = status
	result.Recommendation = string(RecommendIncomplete)
	if result.Mode == ModeStructured {
		result.Coverage = CoverageIncomplete
	} else {
		result.Coverage = CoverageNotRequiredDirect
	}
	result.Problems = []string{problem}
	return result, &CheckError{Status: status, Reason: reason, Problems: append([]string(nil), result.Problems...)}
}

func verifyLoadedCheckArtifacts(ctx context.Context, artifacts PlanArtifacts) error {
	if err := artifacts.Plan.Validate(); err != nil {
		return err
	}
	if err := validatePlanAuditManifest(ctx, artifacts.Plan); err != nil {
		return err
	}
	canonical := ReviewPlanIdentityV1(artifacts.PlanIdentity)
	if !bytes.Equal(canonical, artifacts.PlanIdentityBytes) {
		return errors.New("canonical plan identity bytes differ")
	}
	digest := sha256.Sum256(canonical)
	digestHex := hex.EncodeToString(digest[:])
	if artifacts.Plan.PlanDigest != digestHex {
		return fmt.Errorf("full plan digest mismatch: got %s want %s", artifacts.Plan.PlanDigest, digestHex)
	}
	wantReviewID := "rvw_" + hex.EncodeToString(digest[:20])
	if artifacts.Plan.ReviewID != wantReviewID {
		return fmt.Errorf("review id mismatch: got %s want %s", artifacts.Plan.ReviewID, wantReviewID)
	}
	if err := verifyArtifactIntegrity(
		artifacts.Plan,
		artifacts.IDRecords,
		artifacts.PlanIdentity,
		artifacts.PlanIdentityBytes,
		artifacts.PublishedIndexSignature,
		artifacts.MandatoryUnits,
	); err != nil {
		return err
	}
	var reviewFull Digest
	decoded, err := hex.DecodeString(artifacts.Plan.PlanDigest)
	if err != nil || len(decoded) != len(reviewFull) {
		return errors.New("malformed full plan digest")
	}
	copy(reviewFull[:], decoded)
	if _, err := addPlanToRegistry(
		ctx,
		map[string]Digest{},
		func(data []byte) Digest { return Digest(sha256.Sum256(data)) },
		artifacts.Plan,
		artifacts.Citations,
		artifacts.UnitCandidates,
		artifacts.MandatoryUnits,
		artifacts.IDRecords,
		reviewFull,
	); err != nil {
		return err
	}
	return nil
}

type checkIssue struct {
	status  CheckStatus
	reason  string
	problem string
}

func canceledCheckIssue(err error) *checkIssue {
	return &checkIssue{status: CheckInvalid, reason: "canceled", problem: "canceled: " + err.Error()}
}

type checkedHunk struct {
	id       string
	unitID   string
	pathID   string
	side     Side
	path     string
	start    int
	end      int
	patchSum string
	oldPath  string
	oldStart int
	oldCount int
	newPath  string
	newStart int
	newCount int
}

type checkLineRange struct {
	start int
	end   int
}

type causalOwnership struct {
	hunks map[string]struct{}
	paths map[string]struct{}
	units map[string]struct{}
}

type reportValidator struct {
	artifacts      PlanArtifacts
	sources        SourceResolver
	paths          map[string]PlanPath
	units          map[string]map[string]struct{}
	auditTargets   map[string]struct{}
	auditOwnership map[string]causalOwnership
	targetPaths    map[string]string
	planIDs        map[string]struct{}
	hunks          map[string]checkedHunk
	hunkProofs     map[string]struct{}
	proofsByPath   map[string][]CitationProof
	pathHasHunk    map[string]bool
	pathSides      map[string]SideIdentity
	changedSides   map[string]struct{}
	diffRanges     map[string][]checkLineRange
}

func validateReportShapeForCheck(ctx context.Context, report Report) error {
	if report.Schema != ReportSchemaV1 {
		return fmt.Errorf("review: report schema %q is not %s", report.Schema, ReportSchemaV1)
	}
	if !validReviewID(report.ReviewID) {
		return fmt.Errorf("review: report has malformed review id %q", report.ReviewID)
	}
	if !isCanonicalDigestHex(report.PlanDigest) {
		return fmt.Errorf("review: report has non-canonical plan digest %q", report.PlanDigest)
	}
	if err := report.Base.Validate(); err != nil {
		return fmt.Errorf("review: report base: %w", err)
	}
	if err := report.Head.Validate(); err != nil {
		return fmt.Errorf("review: report head: %w", err)
	}
	if !validRecommendation(report.Recommendation) {
		return fmt.Errorf("review: report has invalid recommendation %q", report.Recommendation)
	}
	seen := make(map[string]struct{}, len(report.Findings))
	for _, finding := range report.Findings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, duplicate := seen[finding.ID]; duplicate {
			return fmt.Errorf("review: duplicate finding id %q", finding.ID)
		}
		seen[finding.ID] = struct{}{}
		if err := finding.Validate(); err != nil {
			return err
		}
	}
	for _, receipt := range report.PrimaryReceipts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receipt.Validate(); err != nil {
			return err
		}
	}
	for _, receipt := range report.AuditReceipts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receipt.Validate(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func newReportValidator(ctx context.Context, s *Service, artifacts PlanArtifacts) (*reportValidator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	width, ok := oidWidthFor(artifacts.Plan.Scope.ObjectFormat)
	if !ok {
		return nil, fmt.Errorf("invalid object format %q", artifacts.Plan.Scope.ObjectFormat)
	}
	options := HeadViewOptions{
		Runner: s.git, RepoRoot: s.root, ObjectFormat: artifacts.Plan.Scope.ObjectFormat,
		BaseTreeish: hex.EncodeToString(artifacts.Plan.Scope.Base.Value),
	}
	workspace := artifacts.Plan.Scope.Kind == ScopeWorkspace
	if !workspace {
		options.HeadTreeish = hex.EncodeToString(artifacts.Plan.Scope.Head.Value)
	}
	v := &reportValidator{
		artifacts: artifacts, sources: newSourceResolver(options, width, workspace),
		paths:          make(map[string]PlanPath, len(artifacts.Plan.ChangedPaths)),
		units:          make(map[string]map[string]struct{}, len(artifacts.Plan.PrimaryUnits)),
		auditTargets:   map[string]struct{}{},
		auditOwnership: map[string]causalOwnership{},
		targetPaths:    map[string]string{},
		planIDs:        map[string]struct{}{},
		hunks:          map[string]checkedHunk{},
		hunkProofs:     map[string]struct{}{},
		proofsByPath:   map[string][]CitationProof{},
		pathHasHunk:    map[string]bool{},
		pathSides:      map[string]SideIdentity{},
		changedSides:   map[string]struct{}{},
		diffRanges:     map[string][]checkLineRange{},
	}
	for _, record := range artifacts.IDRecords {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v.planIDs[record.Public] = struct{}{}
	}
	for _, path := range artifacts.Plan.ChangedPaths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, duplicate := v.paths[path.PathID]; duplicate {
			return nil, fmt.Errorf("duplicate changed path id %s", path.PathID)
		}
		v.paths[path.PathID] = path
		v.planIDs[path.PathID] = struct{}{}
		if path.OldPath != "" {
			v.changedSides[sourceKey(SideBase, path.OldPath)] = struct{}{}
		}
		if path.NewPath != "" {
			v.changedSides[sourceKey(SideHead, path.NewPath)] = struct{}{}
		}
	}
	for _, proof := range artifacts.Citations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v.proofsByPath[proof.Path] = append(v.proofsByPath[proof.Path], proof)
	}
	fullPaths := make(map[string]string, len(artifacts.PlanIdentity.Paths))
	for _, path := range artifacts.PlanIdentity.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fullPaths[hex.EncodeToString(path.PathID.Full[:])] = path.PathID.Public
	}
	for _, record := range artifacts.IDRecords {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch record.Kind {
		case PathIDPrefixV1:
			if err := v.indexPathSides(ctx, record); err != nil {
				return nil, err
			}
		case HunkIDPrefixV1:
			hunk, err := checkedHunkFromRecord(record, fullPaths, v.paths)
			if err != nil {
				return nil, err
			}
			v.indexHunk(hunk)
		case TargetIDPrefixV1:
			fields, err := decodeFramedFields(record.Canonical)
			if err != nil {
				return nil, fmt.Errorf("target %s canonical: %w", record.Public, err)
			}
			if pathID, ok := fullPaths[hex.EncodeToString(fields["path"])]; ok {
				v.targetPaths[record.Public] = pathID
			}
		}
	}
	unitIdentity := make(map[string]PlanUnitEntry, len(artifacts.PlanIdentity.Units))
	for _, unit := range artifacts.PlanIdentity.Units {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unitIdentity[unit.UnitID.Public] = unit
		v.planIDs[unit.UnitID.Public] = struct{}{}
		for _, hunkID := range unit.HunkIDs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			v.planIDs[hunkID.Public] = struct{}{}
		}
	}
	for _, entry := range artifacts.PlanIdentity.Hunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v.planIDs[entry.HunkID.Public] = struct{}{}
	}
	for _, audit := range artifacts.Plan.RequiredAudits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, target := range audit.TargetIDs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			v.auditTargets[target] = struct{}{}
			v.planIDs[target] = struct{}{}
		}
	}
	for _, unit := range artifacts.Plan.PrimaryUnits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owned := map[string]struct{}{}
		identity := unitIdentity[unit.UnitID]
		for i, hunkID := range identity.HunkIDs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			owned[hunkID.Public] = struct{}{}
			if i >= len(unit.Hunks) {
				continue
			}
			hunk, err := checkedHunkFrom(unit.UnitID, hunkID.Public, unit.Hunks[i])
			if err != nil {
				return nil, err
			}
			v.indexHunk(hunk)
		}
		v.units[unit.UnitID] = owned
		v.planIDs[unit.UnitID] = struct{}{}
	}
	cohortUnits := make(map[string]map[string]struct{}, len(artifacts.PlanIdentity.Cohorts))
	for _, cohort := range artifacts.PlanIdentity.Cohorts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owned := cohortUnits[cohort.CohortID.Public]
		if owned == nil {
			owned = map[string]struct{}{}
			cohortUnits[cohort.CohortID.Public] = owned
		}
		for _, unit := range cohort.UnitIDs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			owned[unit.Public] = struct{}{}
		}
	}
	if err := v.normalizeDiffRanges(ctx); err != nil {
		return nil, err
	}
	if err := v.indexAuditOwnership(ctx, cohortUnits); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *reportValidator) indexAuditOwnership(ctx context.Context, cohortUnits map[string]map[string]struct{}) error {
	for target := range v.auditTargets {
		if err := ctx.Err(); err != nil {
			return err
		}
		ownership := causalOwnership{
			hunks: map[string]struct{}{},
			paths: map[string]struct{}{},
			units: map[string]struct{}{},
		}
		switch {
		case strings.HasPrefix(target, HunkIDPrefixV1):
			if _, ok := v.hunks[target]; !ok {
				return fmt.Errorf("audit target %s has no owned hunk", target)
			}
			ownership.hunks[target] = struct{}{}
		case strings.HasPrefix(target, PathIDPrefixV1):
			if _, ok := v.paths[target]; !ok {
				return fmt.Errorf("audit target %s has no owned path", target)
			}
			ownership.paths[target] = struct{}{}
		case strings.HasPrefix(target, UnitIDPrefixV1):
			if _, ok := v.units[target]; !ok {
				return fmt.Errorf("audit target %s has no owned unit", target)
			}
			ownership.units[target] = struct{}{}
		case strings.HasPrefix(target, CohortIDPrefixV1):
			units, ok := cohortUnits[target]
			if !ok {
				return fmt.Errorf("audit target %s has no owned cohort", target)
			}
			ownership.units = units
		case strings.HasPrefix(target, TargetIDPrefixV1):
			pathID, ok := v.targetPaths[target]
			if !ok {
				return fmt.Errorf("audit target %s has no owned path", target)
			}
			ownership.paths[pathID] = struct{}{}
		default:
			return fmt.Errorf("audit target %s has unsupported kind", target)
		}
		v.auditOwnership[target] = ownership
	}
	return ctx.Err()
}

func checkedHunkFrom(unitID, hunkID string, hunk UnitHunk) (checkedHunk, error) {
	payload, err := base64.StdEncoding.DecodeString(hunk.PatchBase64)
	if err != nil {
		return checkedHunk{}, fmt.Errorf("unit %s hunk %s patch: %w", unitID, hunkID, err)
	}
	side, path, start, count := SideHead, hunk.NewPath, hunk.NewStart, hunk.NewCount
	if count == 0 {
		side, path, start, count = SideBase, hunk.OldPath, hunk.OldStart, hunk.OldCount
	}
	if count < 1 {
		count = 1
	}
	sum := sha256.Sum256(payload)
	return checkedHunk{
		id: hunkID, unitID: unitID, pathID: hunk.PathID, side: side, path: path,
		start: start, end: start + count - 1, patchSum: hex.EncodeToString(sum[:]),
		oldPath: hunk.OldPath, oldStart: hunk.OldStart, oldCount: hunk.OldCount,
		newPath: hunk.NewPath, newStart: hunk.NewStart, newCount: hunk.NewCount,
	}, nil
}

func sourceKey(side Side, path string) string {
	return string(side) + "\x00" + path
}

func proofKey(side Side, path string, start, end int, contentHash string) string {
	return strings.Join([]string{string(side), path, strconv.Itoa(start), strconv.Itoa(end), contentHash}, "\x00")
}

func (v *reportValidator) indexPathSides(ctx context.Context, record IDRecord) error {
	path, ok := v.paths[record.Public]
	if !ok {
		return fmt.Errorf("path record %s has no plan path", record.Public)
	}
	outer, err := decodeFramedFields(record.Canonical)
	if err != nil {
		return fmt.Errorf("path %s canonical: %w", record.Public, err)
	}
	raw, err := decodeFramedFields(outer["record"])
	if err != nil {
		return fmt.Errorf("path %s record: %w", record.Public, err)
	}
	for _, side := range []struct {
		name string
		path string
		side Side
	}{
		{name: "old_side", path: path.OldPath, side: SideBase},
		{name: "new_side", path: path.NewPath, side: SideHead},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if side.path == "" {
			continue
		}
		identity, err := decodeCheckSideIdentity(raw[side.name])
		if err != nil {
			return fmt.Errorf("path %s %s: %w", record.Public, side.name, err)
		}
		v.pathSides[sourceKey(side.side, side.path)] = identity
	}
	return ctx.Err()
}

func (v *reportValidator) indexHunk(hunk checkedHunk) {
	v.hunks[hunk.id] = hunk
	v.pathHasHunk[hunk.pathID] = true
	v.hunkProofs[proofKey(hunk.side, hunk.path, hunk.start, hunk.end, hunk.patchSum)] = struct{}{}
	if hunk.oldCount > 0 && hunk.oldPath != "" {
		key := sourceKey(SideBase, hunk.oldPath)
		v.diffRanges[key] = append(v.diffRanges[key], checkLineRange{start: hunk.oldStart, end: hunk.oldStart + hunk.oldCount - 1})
	}
	if hunk.newCount > 0 && hunk.newPath != "" {
		key := sourceKey(SideHead, hunk.newPath)
		v.diffRanges[key] = append(v.diffRanges[key], checkLineRange{start: hunk.newStart, end: hunk.newStart + hunk.newCount - 1})
	}
}

func (v *reportValidator) normalizeDiffRanges(ctx context.Context) error {
	for key, ranges := range v.diffRanges {
		if err := ctx.Err(); err != nil {
			return err
		}
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].start != ranges[j].start {
				return ranges[i].start < ranges[j].start
			}
			return ranges[i].end < ranges[j].end
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		merged := ranges[:0]
		for _, current := range ranges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(merged) == 0 || current.start > merged[len(merged)-1].end+1 {
				merged = append(merged, current)
				continue
			}
			if current.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = current.end
			}
		}
		v.diffRanges[key] = merged
	}
	return ctx.Err()
}

func checkedHunkFromRecord(record IDRecord, fullPaths map[string]string, paths map[string]PlanPath) (checkedHunk, error) {
	fields, err := decodeFramedFields(record.Canonical)
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s canonical: %w", record.Public, err)
	}
	pathID, ok := fullPaths[hex.EncodeToString(fields["path"])]
	if !ok {
		return checkedHunk{}, fmt.Errorf("hunk %s has unknown full path digest", record.Public)
	}
	hunkFields, err := decodeFramedFields(fields["hunk"])
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s record: %w", record.Public, err)
	}
	oldStart, err := checkUint64(hunkFields["old_start"])
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s old_start: %w", record.Public, err)
	}
	oldLines, err := checkUint64(hunkFields["old_lines"])
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s old_lines: %w", record.Public, err)
	}
	newStart, err := checkUint64(hunkFields["new_start"])
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s new_start: %w", record.Public, err)
	}
	newLines, err := checkUint64(hunkFields["new_lines"])
	if err != nil {
		return checkedHunk{}, fmt.Errorf("hunk %s new_lines: %w", record.Public, err)
	}
	if oldStart > uint64(^uint(0)>>1) || oldLines > uint64(^uint(0)>>1) ||
		newStart > uint64(^uint(0)>>1) || newLines > uint64(^uint(0)>>1) {
		return checkedHunk{}, fmt.Errorf("hunk %s coordinates overflow int", record.Public)
	}
	planPath := paths[pathID]
	side, sourcePath, start, count := SideHead, planPath.NewPath, int(newStart), int(newLines)
	if count == 0 {
		side, sourcePath, start, count = SideBase, planPath.OldPath, int(oldStart), int(oldLines)
	}
	if count < 1 {
		count = 1
	}
	sum := sha256.Sum256(hunkFields["payload"])
	return checkedHunk{
		id: record.Public, pathID: pathID, side: side, path: sourcePath,
		start: start, end: start + count - 1, patchSum: hex.EncodeToString(sum[:]),
		oldPath: planPath.OldPath, oldStart: int(oldStart), oldCount: int(oldLines),
		newPath: planPath.NewPath, newStart: int(newStart), newCount: int(newLines),
	}, nil
}

func checkUint64(data []byte) (uint64, error) {
	if len(data) != 8 {
		return 0, fmt.Errorf("want 8 bytes, got %d", len(data))
	}
	return binary.BigEndian.Uint64(data), nil
}

func (v *reportValidator) validateFindings(ctx context.Context, findings []Finding) *checkIssue {
	seenLocations := map[string]struct{}{}
	for _, finding := range findings {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if len(finding.Citations) == 0 {
			return &checkIssue{status: CheckInvalid, reason: "citation", problem: fmt.Sprintf("citation: finding %s has no supporting citation", finding.ID)}
		}
		if len(finding.VerifierEvidence) == 0 {
			return &checkIssue{status: CheckInvalid, reason: "verifier_evidence", problem: fmt.Sprintf("verifier_evidence: finding %s has no verifier evidence citation", finding.ID)}
		}
		causes := map[string]struct{}{}
		for _, cause := range finding.Causes {
			if err := ctx.Err(); err != nil {
				return canceledCheckIssue(err)
			}
			key := cause.Kind + ":" + cause.ID
			if _, duplicate := causes[key]; duplicate {
				return &checkIssue{status: CheckInvalid, reason: "duplicate_cause", problem: fmt.Sprintf("duplicate_cause: finding %s repeats %s", finding.ID, key)}
			}
			if !v.validCause(cause) {
				return &checkIssue{status: CheckInvalid, reason: "cause", problem: fmt.Sprintf("cause: finding %s references foreign %s %s", finding.ID, cause.Kind, cause.ID)}
			}
			causes[key] = struct{}{}
		}
		if issue := v.validateCitationSet(ctx, "finding "+finding.ID+" supporting", finding.Citations); issue != nil {
			return issue
		}
		if issue := v.validateCitationSet(ctx, "finding "+finding.ID+" verifier", finding.VerifierEvidence); issue != nil {
			return issue
		}
		for index, location := range finding.Locations {
			if err := ctx.Err(); err != nil {
				return canceledCheckIssue(err)
			}
			key := locationKey(location)
			if _, duplicate := seenLocations[key]; duplicate {
				return &checkIssue{status: CheckInvalid, reason: "duplicate_location", problem: fmt.Sprintf("duplicate_location: finding %s repeats %s", finding.ID, key)}
			}
			seenLocations[key] = struct{}{}
			inside, issue := v.validateLocation(ctx, finding.ID, index, location)
			if issue != nil {
				return issue
			}
			if !inside {
				for _, cause := range finding.Causes {
					if err := ctx.Err(); err != nil {
						return canceledCheckIssue(err)
					}
					supported, err := v.citationsSupportCause(ctx, cause, finding.Citations)
					if err != nil {
						return canceledCheckIssue(err)
					}
					if !supported {
						return &checkIssue{
							status: CheckInvalid, reason: "outside_diff_support",
							problem: fmt.Sprintf("outside_diff_support: finding %s location %s has no plan-owned causal citation", finding.ID, displayLocation(location)),
						}
					}
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	return nil
}

func (v *reportValidator) validCause(cause Cause) bool {
	switch CauseKind(cause.Kind) {
	case CausePrimaryUnit:
		_, ok := v.units[cause.ID]
		return ok
	case CauseChangedPath:
		_, ok := v.paths[cause.ID]
		return ok
	case CauseAuditTarget:
		_, ok := v.auditTargets[cause.ID]
		return ok
	default:
		return false
	}
}

func (v *reportValidator) citationsSupportCause(ctx context.Context, cause Cause, citations []Citation) (bool, error) {
	for _, citation := range citations {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		switch CauseKind(cause.Kind) {
		case CausePrimaryUnit:
			if _, ok := v.units[cause.ID][citation.ID]; ok {
				return true, nil
			}
		case CauseChangedPath:
			if citation.ID == cause.ID && !v.pathHasHunk[cause.ID] {
				return true, nil
			}
			if hunk, ok := v.hunks[citation.ID]; ok && hunk.pathID == cause.ID {
				return true, nil
			}
		case CauseAuditTarget:
			ownership := v.auditOwnership[cause.ID]
			if _, ok := ownership.hunks[citation.ID]; ok {
				return true, nil
			}
			if _, ok := ownership.paths[citation.ID]; ok && !v.pathHasHunk[citation.ID] {
				return true, nil
			}
			if hunk, ok := v.hunks[citation.ID]; ok {
				if _, ok := ownership.paths[hunk.pathID]; ok {
					return true, nil
				}
				if _, ok := ownership.units[hunk.unitID]; ok {
					return true, nil
				}
			}
		}
	}
	return false, ctx.Err()
}

func (v *reportValidator) validateCitationSet(ctx context.Context, label string, citations []Citation) *checkIssue {
	seen := make(map[string]struct{}, len(citations))
	for _, citation := range citations {
		if err := ctx.Err(); err != nil {
			return &checkIssue{status: CheckInvalid, reason: "canceled", problem: "canceled: " + err.Error()}
		}
		if citation.ID != "" && citation.Path != "" {
			return &checkIssue{status: CheckInvalid, reason: "citation", problem: fmt.Sprintf("citation: %s citation must not mix id and path", label)}
		}
		key := citation.ID
		if key == "" {
			key = "path:" + citation.Path
		}
		if _, duplicate := seen[key]; duplicate {
			return &checkIssue{status: CheckInvalid, reason: "duplicate_citation", problem: fmt.Sprintf("duplicate_citation: %s repeats %s", label, key)}
		}
		seen[key] = struct{}{}
		proof, ok := v.lookupCitationProof(citation)
		if !ok {
			return &checkIssue{status: CheckInvalid, reason: "citation", problem: fmt.Sprintf("citation: %s references evidence %s absent from persisted plan", label, key)}
		}
		if issue := v.validateCitationProof(ctx, key, proof); issue != nil {
			return issue
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	return nil
}

func (v *reportValidator) lookupCitationProof(citation Citation) (CitationProof, bool) {
	if citation.ID != "" {
		if _, ok := v.planIDs[citation.ID]; !ok {
			return CitationProof{}, false
		}
		proof, ok := v.artifacts.Citations[citation.ID]
		return proof, ok && proof.ID == citation.ID
	}
	proofs := v.proofsByPath[citation.Path]
	if len(proofs) != 1 {
		return CitationProof{}, false
	}
	return proofs[0], true
}

func (v *reportValidator) validateCitationProof(ctx context.Context, key string, proof CitationProof) *checkIssue {
	if proof.Side != SideBase && proof.Side != SideHead {
		return proofIssue(key, fmt.Sprintf("wrong side %q", proof.Side))
	}
	if proof.Path == "" || !isCanonicalDigestHex(proof.ContentHash) || proof.Start < 1 || proof.End < proof.Start {
		return proofIssue(key, "malformed path, digest, or range")
	}
	if hunk, ok := v.hunks[proof.ID]; ok {
		if !proofMatchesHunk(proof, hunk) {
			return proofIssue(key, "wrong side, path, range, or content for owned hunk")
		}
		return nil
	}
	if _, ok := v.hunkProofs[proofKey(proof.Side, proof.Path, proof.Start, proof.End, proof.ContentHash)]; ok {
		return nil
	}
	entry, err := v.sources.Read(ctx, proof.Side, proof.Path, maxEvidenceBytes)
	if err != nil {
		if ctx.Err() != nil {
			return &checkIssue{status: CheckInvalid, reason: "canceled", problem: "canceled: " + ctx.Err().Error()}
		}
		return proofIssue(key, err.Error())
	}
	if !entry.Present {
		return proofIssue(key, "path is absent")
	}
	sum := sha256.Sum256(entry.Bytes)
	lines := lineCount(entry.Bytes)
	if proof.ContentHash != hex.EncodeToString(sum[:]) {
		return proofIssue(key, "content digest is stale")
	}
	if proof.Start != 1 || proof.End != lines {
		return proofIssue(key, fmt.Sprintf("wrong range %d-%d; canonical content range is 1-%d", proof.Start, proof.End, lines))
	}
	return nil
}

func proofMatchesHunk(proof CitationProof, hunk checkedHunk) bool {
	return proof.Side == hunk.side && proof.Path == hunk.path && proof.Start == hunk.start &&
		proof.End == hunk.end && proof.ContentHash == hunk.patchSum
}

func proofIssue(key, detail string) *checkIssue {
	return &checkIssue{status: CheckInvalid, reason: "citation_proof", problem: fmt.Sprintf("citation_proof: %s %s", key, detail)}
}

func (v *reportValidator) validateLocation(ctx context.Context, findingID string, index int, location Location) (bool, *checkIssue) {
	entry, err := v.sources.Read(ctx, Side(location.Side), location.Path, maxEvidenceBytes)
	if err != nil {
		if ctx.Err() != nil {
			return false, &checkIssue{status: CheckInvalid, reason: "canceled", problem: "canceled: " + ctx.Err().Error()}
		}
		return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d cannot resolve: %v", findingID, index, err)}
	}
	if !entry.Present {
		return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d path %s is absent on %s", findingID, index, location.Path, location.Side)}
	}
	if location.ContentHash != "" {
		sum := sha256.Sum256(entry.Bytes)
		if location.ContentHash != hex.EncodeToString(sum[:]) {
			return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d content digest does not match %s %s", findingID, index, location.Path, location.Side)}
		}
		if LocationKind(location.Kind) == LocationRange {
			lines := lineCount(entry.Bytes)
			if entry.TextClass != TextClassText {
				return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d range requires text", findingID, index)}
			}
			if location.End > lines {
				return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d range %d-%d exceeds %s %s line count %d", findingID, index, location.Start, location.End, location.Path, location.Side, lines)}
			}
		}
	} else {
		if entry.Kind != location.EntryType || entry.Mode != location.Mode {
			return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d non-content type/mode does not match", findingID, index)}
		}
		expected, ok := v.locationSideIdentity(location, entry)
		if !ok || location.Identity == nil || !sameSide(*location.Identity, expected) {
			return false, &checkIssue{status: CheckInvalid, reason: "location", problem: fmt.Sprintf("location: finding %s location %d non-content identity does not match", findingID, index)}
		}
	}
	return v.locationInsideDiff(location), nil
}

func (v *reportValidator) locationSideIdentity(location Location, entry SourceEntry) (SideIdentity, bool) {
	if entry.OID != "" {
		value, err := hex.DecodeString(entry.OID)
		if err != nil {
			return SideIdentity{}, false
		}
		return SideIdentity{Kind: SideGitOID, Value: value}, true
	}
	expected, ok := v.pathSides[sourceKey(Side(location.Side), location.Path)]
	return expected, ok
}

func decodeCheckSideIdentity(data []byte) (SideIdentity, error) {
	fields, err := decodeFramedFields(data)
	if err != nil {
		return SideIdentity{}, err
	}
	side := SideIdentity{Kind: SideKind(string(fields["kind"])), Value: append([]byte(nil), fields["value"]...)}
	return side, side.Validate()
}

func (v *reportValidator) locationInsideDiff(location Location) bool {
	key := sourceKey(Side(location.Side), location.Path)
	if LocationKind(location.Kind) == LocationPath {
		_, ok := v.changedSides[key]
		return ok
	}
	ranges := v.diffRanges[key]
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i].end >= location.Start })
	return index < len(ranges) && ranges[index].start <= location.End
}

func locationKey(location Location) string {
	return strings.Join([]string{location.Kind, location.Side, location.Path, strconv.Itoa(location.Start), strconv.Itoa(location.End)}, ":")
}

func displayLocation(location Location) string {
	if LocationKind(location.Kind) == LocationRange {
		return fmt.Sprintf("%s:%d-%d", location.Path, location.Start, location.End)
	}
	return location.Path
}

func (v *reportValidator) validateStructuredCoverage(ctx context.Context, report Report) *checkIssue {
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	findings := make(map[string]struct{}, len(report.Findings))
	for _, finding := range report.Findings {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		findings[finding.ID] = struct{}{}
	}

	primary := make(map[string]PrimaryReceipt, len(report.PrimaryReceipts))
	for _, receipt := range report.PrimaryReceipts {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if _, ok := v.units[receipt.UnitID]; !ok {
			return &checkIssue{status: CheckInvalid, reason: "primary_receipt", problem: "primary_receipt: foreign unit " + receipt.UnitID}
		}
		if _, duplicate := primary[receipt.UnitID]; duplicate {
			return &checkIssue{status: CheckInvalid, reason: "primary_receipt", problem: "primary_receipt: duplicate receipt for unit " + receipt.UnitID}
		}
		primary[receipt.UnitID] = receipt
	}
	for _, unit := range v.artifacts.Plan.PrimaryUnits {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		receipt, ok := primary[unit.UnitID]
		if !ok {
			return &checkIssue{status: CheckIncomplete, reason: "primary_coverage", problem: "primary_coverage: missing receipt for unit " + unit.UnitID}
		}
		if issue := exactAcknowledgement(
			ctx, "primary_coverage", "unit "+unit.UnitID, "hunk",
			v.units[unit.UnitID], receipt.AcknowledgedPrimaryHunkIDs,
		); issue != nil {
			return issue
		}
		if issue := validateReceiptFindingIDs(ctx, "primary receipt "+unit.UnitID, receipt.FindingIDs, findings); issue != nil {
			return issue
		}
		if issue := v.validateCitationSet(ctx, "primary receipt "+unit.UnitID+" context", receipt.ContextCitations); issue != nil {
			return issue
		}
		for _, uncertainty := range receipt.Uncertainties {
			if err := ctx.Err(); err != nil {
				return canceledCheckIssue(err)
			}
			if uncertainty.Blocking {
				return &checkIssue{
					status: CheckIncomplete, reason: "blocking_uncertainty",
					problem: "blocking_uncertainty: primary unit " + unit.UnitID + ": " + uncertainty.Summary,
				}
			}
		}
	}

	expectedAudits := make(map[string]map[string]struct{}, len(v.artifacts.Plan.RequiredAudits))
	for _, audit := range v.artifacts.Plan.RequiredAudits {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		targets := make(map[string]struct{}, len(audit.TargetIDs))
		for _, target := range audit.TargetIDs {
			if err := ctx.Err(); err != nil {
				return canceledCheckIssue(err)
			}
			targets[target] = struct{}{}
		}
		expectedAudits[audit.AuditID] = targets
	}
	audits := make(map[string]AuditReceipt, len(report.AuditReceipts))
	for _, receipt := range report.AuditReceipts {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if _, ok := expectedAudits[receipt.AuditID]; !ok {
			return &checkIssue{status: CheckInvalid, reason: "audit_receipt", problem: "audit_receipt: foreign audit " + receipt.AuditID}
		}
		if _, duplicate := audits[receipt.AuditID]; duplicate {
			return &checkIssue{status: CheckInvalid, reason: "audit_receipt", problem: "audit_receipt: duplicate receipt for audit " + receipt.AuditID}
		}
		audits[receipt.AuditID] = receipt
	}
	for _, audit := range v.artifacts.Plan.RequiredAudits {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		receipt, ok := audits[audit.AuditID]
		if !ok {
			return &checkIssue{status: CheckIncomplete, reason: "audit_coverage", problem: "audit_coverage: missing receipt for audit " + audit.AuditID}
		}
		if issue := exactAcknowledgement(
			ctx, "audit_coverage", "audit "+audit.AuditID, "target",
			expectedAudits[audit.AuditID], receipt.AcknowledgedAuditTargetIDs,
		); issue != nil {
			return issue
		}
		if issue := validateReceiptFindingIDs(ctx, "audit receipt "+audit.AuditID, receipt.FindingIDs, findings); issue != nil {
			return issue
		}
		if issue := v.validateCitationSet(ctx, "audit receipt "+audit.AuditID+" context", receipt.ContextCitations); issue != nil {
			return issue
		}
		for _, uncertainty := range receipt.Uncertainties {
			if err := ctx.Err(); err != nil {
				return canceledCheckIssue(err)
			}
			if uncertainty.Blocking {
				return &checkIssue{
					status: CheckIncomplete, reason: "blocking_uncertainty",
					problem: "blocking_uncertainty: audit " + audit.AuditID + ": " + uncertainty.Summary,
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	return nil
}

func exactAcknowledgement(ctx context.Context, reason, owner, item string, expected map[string]struct{}, acknowledged []string) *checkIssue {
	seen := make(map[string]struct{}, len(acknowledged))
	for _, id := range acknowledged {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if _, duplicate := seen[id]; duplicate {
			return &checkIssue{status: CheckInvalid, reason: reason, problem: fmt.Sprintf("%s: %s repeats %s %s", reason, owner, item, id)}
		}
		seen[id] = struct{}{}
		if _, ok := expected[id]; !ok {
			return &checkIssue{status: CheckInvalid, reason: reason, problem: fmt.Sprintf("%s: %s acknowledges foreign %s %s", reason, owner, item, id)}
		}
	}
	missing := make([]string, 0, len(expected))
	for id := range expected {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if _, ok := seen[id]; !ok {
			missing = append(missing, id)
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		return &checkIssue{status: CheckIncomplete, reason: reason, problem: fmt.Sprintf("%s: %s missing %s %s", reason, owner, item, missing[0])}
	}
	return nil
}

func validateReceiptFindingIDs(ctx context.Context, owner string, ids []string, findings map[string]struct{}) *checkIssue {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if _, duplicate := seen[id]; duplicate {
			return &checkIssue{status: CheckInvalid, reason: "receipt_finding", problem: "receipt_finding: " + owner + " repeats finding " + id}
		}
		seen[id] = struct{}{}
		if _, ok := findings[id]; !ok {
			return &checkIssue{status: CheckInvalid, reason: "receipt_finding", problem: "receipt_finding: " + owner + " references foreign finding " + id}
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	return nil
}

func validateRecommendationForCheck(ctx context.Context, report Report) *checkIssue {
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	if report.Recommendation == string(RecommendIncomplete) {
		return &checkIssue{status: CheckIncomplete, reason: "recommendation_incomplete", problem: "recommendation_incomplete: report declares incomplete review"}
	}
	critical := ""
	unverified := ""
	major := ""
	for _, finding := range report.Findings {
		if err := ctx.Err(); err != nil {
			return canceledCheckIssue(err)
		}
		if finding.Verifier == string(DispositionUnverified) {
			unverified = finding.ID
			continue
		}
		if finding.Verifier != string(DispositionConfirmed) && finding.Verifier != string(DispositionPlausible) {
			continue
		}
		switch Severity(finding.Severity) {
		case SeverityCritical:
			critical = finding.ID
		case SeverityMajor:
			major = finding.ID
		}
	}
	if err := ctx.Err(); err != nil {
		return canceledCheckIssue(err)
	}
	if unverified != "" {
		return &checkIssue{status: CheckIncomplete, reason: "unverified_finding", problem: "unverified_finding: finding " + unverified + " remains unverified"}
	}
	if critical != "" && report.Recommendation != string(RecommendRequestChanges) {
		return &checkIssue{status: CheckInvalid, reason: "recommendation", problem: "recommendation: finding " + critical + " requires request_changes"}
	}
	if report.Recommendation == string(RecommendApprove) && major != "" {
		return &checkIssue{status: CheckInvalid, reason: "recommendation", problem: "recommendation: approve forbidden by unrejected critical/major finding " + major}
	}
	return nil
}

func preflightArtifactBounds(ctx context.Context, artifacts PlanArtifacts) error {
	collections := []struct {
		name string
		size int
	}{
		{"changed paths", len(artifacts.Plan.ChangedPaths)},
		{"primary units", len(artifacts.Plan.PrimaryUnits)},
		{"required audits", len(artifacts.Plan.RequiredAudits)},
		{"identity paths", len(artifacts.PlanIdentity.Paths)},
		{"identity hunks", len(artifacts.PlanIdentity.Hunks)},
		{"identity units", len(artifacts.PlanIdentity.Units)},
		{"identity cohorts", len(artifacts.PlanIdentity.Cohorts)},
		{"identity audits", len(artifacts.PlanIdentity.Audits)},
		{"identity records", len(artifacts.IDRecords)},
		{"citation proofs", len(artifacts.Citations)},
		{"unit candidates", len(artifacts.UnitCandidates)},
		{"mandatory units", len(artifacts.MandatoryUnits)},
	}
	total := 0
	charge := func(name string, size int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size > maxCheckPlanItemsV1 {
			return fmt.Errorf("review: plan %s count %d exceeds %d", name, size, maxCheckPlanItemsV1)
		}
		if total > maxCheckTotalReferencesV1-size {
			return fmt.Errorf("review: plan references exceed %d", maxCheckTotalReferencesV1)
		}
		total += size
		return nil
	}
	for _, collection := range collections {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := charge(collection.name, collection.size); err != nil {
			return err
		}
	}
	for _, unit := range artifacts.Plan.PrimaryUnits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := charge("unit hunks", len(unit.Hunks)); err != nil {
			return err
		}
	}
	for _, unit := range artifacts.PlanIdentity.Units {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := charge("identity unit hunks", len(unit.HunkIDs)); err != nil {
			return err
		}
	}
	for _, audit := range artifacts.Plan.RequiredAudits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := charge("audit targets", len(audit.TargetIDs)); err != nil {
			return err
		}
	}
	for _, candidates := range artifacts.UnitCandidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := charge("unit candidate entries", len(candidates)); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func validatePlanAuditManifest(ctx context.Context, plan Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.Mode != ModeStructured {
		return nil
	}
	required := RequiredAuditsV1()
	if len(plan.RequiredAudits) != len(required) {
		return fmt.Errorf("review: structured plan must contain exactly %d required audits", len(required))
	}
	want := make(map[string]struct{}, len(required))
	for _, id := range required {
		if err := ctx.Err(); err != nil {
			return err
		}
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(plan.RequiredAudits))
	for _, audit := range plan.RequiredAudits {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := want[audit.AuditID]; !ok {
			return fmt.Errorf("review: structured plan has foreign audit %s", audit.AuditID)
		}
		if _, duplicate := seen[audit.AuditID]; duplicate {
			return fmt.Errorf("review: structured plan repeats audit %s", audit.AuditID)
		}
		seen[audit.AuditID] = struct{}{}
		targets := make(map[string]struct{}, len(audit.TargetIDs))
		for _, target := range audit.TargetIDs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, duplicate := targets[target]; duplicate {
				return fmt.Errorf("review: audit %s repeats target %s", audit.AuditID, target)
			}
			targets[target] = struct{}{}
		}
	}
	return ctx.Err()
}

func preflightReportBounds(ctx context.Context, report Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(report.Findings) > maxCheckFindingsV1 {
		return fmt.Errorf("review: findings count %d exceeds %d", len(report.Findings), maxCheckFindingsV1)
	}
	if len(report.PrimaryReceipts) > maxCheckReceiptsV1 {
		return fmt.Errorf("review: primary receipt count %d exceeds %d", len(report.PrimaryReceipts), maxCheckReceiptsV1)
	}
	if len(report.AuditReceipts) > maxCheckReceiptsV1 {
		return fmt.Errorf("review: audit receipt count %d exceeds %d", len(report.AuditReceipts), maxCheckReceiptsV1)
	}
	total := 0
	charge := func(n int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n > maxCheckItemsPerFindingV1 && n > maxCheckItemsPerReceiptV1 {
			return fmt.Errorf("review: nested collection count %d exceeds %d", n, maxCheckItemsPerFindingV1)
		}
		if total > maxCheckTotalReferencesV1-n {
			return fmt.Errorf("review: report references exceed %d", maxCheckTotalReferencesV1)
		}
		total += n
		return nil
	}
	for _, finding := range report.Findings {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, n := range []int{len(finding.Causes), len(finding.Locations), len(finding.Citations), len(finding.VerifierEvidence)} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n > maxCheckItemsPerFindingV1 {
				return fmt.Errorf("review: finding %s collection count %d exceeds %d", finding.ID, n, maxCheckItemsPerFindingV1)
			}
			if err := charge(n); err != nil {
				return err
			}
		}
	}
	for _, receipt := range report.PrimaryReceipts {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, n := range []int{len(receipt.AcknowledgedPrimaryHunkIDs), len(receipt.ContextCitations), len(receipt.FindingIDs), len(receipt.Uncertainties)} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n > maxCheckItemsPerReceiptV1 {
				return fmt.Errorf("review: primary receipt %s collection count %d exceeds %d", receipt.UnitID, n, maxCheckItemsPerReceiptV1)
			}
			if err := charge(n); err != nil {
				return err
			}
		}
	}
	for _, receipt := range report.AuditReceipts {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, n := range []int{len(receipt.AcknowledgedAuditTargetIDs), len(receipt.ContextCitations), len(receipt.FindingIDs), len(receipt.Uncertainties)} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n > maxCheckItemsPerReceiptV1 {
				return fmt.Errorf("review: audit receipt %s collection count %d exceeds %d", receipt.AuditID, n, maxCheckItemsPerReceiptV1)
			}
			if err := charge(n); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
