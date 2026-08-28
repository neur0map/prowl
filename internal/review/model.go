package review

import (
	"errors"
	"fmt"
	"strings"
)

// Stable v1 schema identifiers and numeric limits. These are part of the
// machine contract; later tasks and external agents depend on the exact
// values, so they never change without a schema version bump.
const (
	PlanSchemaV1       = "review.plan.v1"
	UnitSchemaV1       = "review.unit.v1"
	ReportSchemaV1     = "review.report.v1"
	EvalOutputSchemaV1 = "review.eval-output.v1"
	ScopeSchemaV1      = "review.scope.v1"

	// StructuredThresholdV1 is the strict raw-churn threshold: structured
	// review is mandatory when raw churn exceeds this value.
	StructuredThresholdV1 = 300
	// MaxUnitChangedLinesV1 bounds the changed lines in a single review unit.
	MaxUnitChangedLinesV1 = 400
	// MaxUnitMandatoryJSONBytesV1 bounds a unit's mandatory JSON payload.
	MaxUnitMandatoryJSONBytesV1 = 16 << 20
	// MaxCanonicalPatchBytesV1 bounds total canonical raw hunk payload.
	MaxCanonicalPatchBytesV1 = 128 << 20
	// MaxChangedPathsV1 bounds the number of changed paths in a plan.
	MaxChangedPathsV1 = 100_000
	// MaxUntrackedFileBytesV1 bounds exact accounting of one untracked file.
	MaxUntrackedFileBytesV1 = 64 << 20
	// MaxUntrackedTotalBytesV1 bounds exact untracked accounting per plan.
	MaxUntrackedTotalBytesV1 = 512 << 20
)

// The four mandatory structured-mode audit identifiers.
const (
	AuditRemovedBehaviorV1   = "audit_removed_behavior_v1"
	AuditContractMigrationV1 = "audit_contract_migration_v1"
	AuditTestMatrixV1        = "audit_test_matrix_v1"
	AuditIntegrationGapV1    = "audit_integration_gap_v1"
)

// RequiredAuditsV1 lists the four mandatory structured-mode audits in order.
var RequiredAuditsV1 = [...]string{
	AuditRemovedBehaviorV1,
	AuditContractMigrationV1,
	AuditTestMatrixV1,
	AuditIntegrationGapV1,
}

// Mode is the review execution mode.
type Mode string

const (
	ModeDirect     Mode = "direct"
	ModeStructured Mode = "structured"
)

// ModeForChurn resolves the review mode and whether structured review is
// mandatory. Structured review is required exactly when raw churn exceeds the
// v1 threshold; forceStructured may raise the mode to structured below the
// threshold without making it required. No input can force direct mode when
// structured review is required.
func ModeForChurn(churn int, forceStructured bool) (Mode, bool) {
	required := churn > StructuredThresholdV1
	if required || forceStructured {
		return ModeStructured, required
	}
	return ModeDirect, false
}

// ScopeKind is the kind of change a review targets.
type ScopeKind string

const (
	ScopeWorkspace ScopeKind = "workspace"
	ScopeCommit    ScopeKind = "commit"
	ScopeRange     ScopeKind = "range"
)

// PlanRequest is the raw, unresolved request to build a review plan.
type PlanRequest struct {
	Commit          string
	Base            string
	Head            string
	ForceStructured bool
}

// Kind reports the scope kind implied by the request fields.
func (r PlanRequest) Kind() ScopeKind {
	switch {
	case r.Commit != "":
		return ScopeCommit
	case r.Base != "":
		return ScopeRange
	default:
		return ScopeWorkspace
	}
}

// Validate rejects malformed requests before any Git command runs. Ref
// arguments may not begin with '-', a commit scope cannot be combined with a
// base/head range, and a head override requires an explicit base.
func (r PlanRequest) Validate() error {
	for _, ref := range []struct{ name, value string }{
		{"commit", r.Commit},
		{"base", r.Base},
		{"head", r.Head},
	} {
		if strings.HasPrefix(ref.value, "-") {
			return fmt.Errorf("review: %s ref %q may not begin with '-'", ref.name, ref.value)
		}
	}
	if r.Commit != "" && (r.Base != "" || r.Head != "") {
		return errors.New("review: commit scope cannot be combined with a base/head range")
	}
	if r.Base == "" && r.Head != "" {
		return errors.New("review: --head requires --base to define a range scope")
	}
	return nil
}

// Scope is a resolved review scope with tagged base/head identities.
type Scope struct {
	Kind         ScopeKind    `json:"kind"`
	ObjectFormat string       `json:"object_format"`
	Base         SideIdentity `json:"base"`
	Head         SideIdentity `json:"head"`
	BaseRef      string       `json:"base_ref,omitempty"`
	HeadRef      string       `json:"head_ref,omitempty"`
	Digest       Digest       `json:"-"`
}

// Capture is the deterministic raw change capture that precedes planning.
type Capture struct {
	Scope           Scope           `json:"scope"`
	Paths           []RawPathRecord `json:"-"`
	RawAdditions    int             `json:"raw_additions"`
	RawDeletions    int             `json:"raw_deletions"`
	RawChurn        int             `json:"raw_churn"`
	ReviewableChurn int             `json:"reviewable_churn"`
	ChangedPaths    int             `json:"changed_paths"`
	CanonicalPatch  Digest          `json:"-"`
}

// Plan is the persisted review plan: scope, mode, statistics, and the ordered
// review structure a host agent consumes. Later tasks populate the structured
// collections.
type Plan struct {
	Schema             string   `json:"schema"`
	ReviewID           string   `json:"review_id"`
	PlanDigest         string   `json:"plan_digest"`
	Mode               Mode     `json:"mode"`
	StructuredRequired bool     `json:"structured_required"`
	Scope              Scope    `json:"scope"`
	RawAdditions       int      `json:"raw_additions"`
	RawDeletions       int      `json:"raw_deletions"`
	RawChurn           int      `json:"raw_churn"`
	ReviewableChurn    int      `json:"reviewable_churn"`
	ChangedPaths       int      `json:"changed_paths"`
	Units              []Unit   `json:"units,omitempty"`
	PrimaryUnitIDs     []string `json:"primary_unit_ids,omitempty"`
	RequiredAuditIDs   []string `json:"required_audit_ids,omitempty"`
}

// Unit is the smallest bounded review territory a host agent acknowledges.
type Unit struct {
	Schema       string   `json:"schema"`
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	HunkIDs      []string `json:"hunk_ids"`
	ChangedLines int      `json:"changed_lines"`
}

// Recommendation is the reviewer's overall recommendation.
type Recommendation string

const (
	RecommendApprove        Recommendation = "approve"
	RecommendComment        Recommendation = "comment"
	RecommendRequestChanges Recommendation = "request_changes"
	RecommendIncomplete     Recommendation = "incomplete"
)

// Category is a finding's category.
type Category string

const (
	CategoryFunctionalCorrectness    Category = "functional_correctness"
	CategorySecurityPrivacy          Category = "security_privacy"
	CategoryStabilityAvailability    Category = "stability_availability"
	CategoryDataIntegrityIntegration Category = "data_integrity_integration"
	CategoryPerformanceScalability   Category = "performance_scalability"
	CategoryMaintainabilityQuality   Category = "maintainability_quality"
)

// Severity is a finding's severity.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
)

// Confidence is a finding's qualitative confidence. It is never a numeric
// probability.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Disposition is the host verifier's disposition of a finding.
type Disposition string

const (
	DispositionConfirmed  Disposition = "confirmed"
	DispositionPlausible  Disposition = "plausible"
	DispositionRejected   Disposition = "rejected"
	DispositionUnverified Disposition = "unverified"
)

// RejectionReason is a typed reason a verifier rejected a finding.
type RejectionReason string

const (
	RejectionContradictedByCode     RejectionReason = "contradicted_by_code"
	RejectionUnreachableByInvariant RejectionReason = "unreachable_by_invariant"
	RejectionPreExisting            RejectionReason = "pre_existing"
	RejectionDuplicate              RejectionReason = "duplicate"
	RejectionNotIntroduced          RejectionReason = "not_introduced"
)

// CauseKind is the kind of a finding's causal reference.
type CauseKind string

const (
	CausePrimaryUnit CauseKind = "primary_unit"
	CauseChangedPath CauseKind = "changed_path"
	CauseAuditTarget CauseKind = "audit_target"
)

// LocationKind is the kind of a finding location.
type LocationKind string

const (
	LocationRange LocationKind = "range"
	LocationPath  LocationKind = "path"
)

// Side names one revision side of a change.
type Side string

const (
	SideBase Side = "base"
	SideHead Side = "head"
)

// Coverage is the coverage state recorded by review check.
type Coverage string

const (
	CoverageNotRequiredDirect Coverage = "not_required_direct"
	CoverageComplete          Coverage = "complete"
	CoverageIncomplete        Coverage = "incomplete"
)

// Cause is a finding's causal reference to a plan element.
type Cause struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Location is a concrete finding location with a canonical side proof.
type Location struct {
	Kind        string        `json:"kind"`
	Path        string        `json:"path"`
	Side        string        `json:"side"`
	ContentHash string        `json:"content_hash,omitempty"`
	Identity    *SideIdentity `json:"identity,omitempty"`
	Mode        uint32        `json:"mode,omitempty"`
	EntryType   string        `json:"entry_type,omitempty"`
	Start       int           `json:"start,omitempty"`
	End         int           `json:"end,omitempty"`
}

// Validate checks the location's shape and side proof. It cannot resolve the
// location against a tree; that is the checker's job in a later task.
func (l Location) Validate() error {
	switch LocationKind(l.Kind) {
	case LocationRange, LocationPath:
	default:
		return fmt.Errorf("location has invalid kind %q", l.Kind)
	}
	if l.Path == "" {
		return errors.New("location missing path")
	}
	switch Side(l.Side) {
	case SideBase, SideHead:
	default:
		return fmt.Errorf("location has invalid side %q", l.Side)
	}
	if l.ContentHash == "" && l.Identity == nil {
		return errors.New("location missing a content hash or tagged side identity proof")
	}
	if LocationKind(l.Kind) == LocationRange {
		if l.Start < 1 || l.End < l.Start {
			return fmt.Errorf("range location requires 1 <= start <= end, got start=%d end=%d", l.Start, l.End)
		}
	}
	return nil
}

// Citation is a reference to supporting or verifying evidence.
type Citation struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	Note string `json:"note,omitempty"`
}

// Uncertainty is a structured statement of remaining reviewer uncertainty.
type Uncertainty struct {
	Blocking bool   `json:"blocking"`
	Summary  string `json:"summary"`
}

// Finding is one immutable finding in a review report.
type Finding struct {
	ID                 string     `json:"id"`
	Causes             []Cause    `json:"causes"`
	Category           string     `json:"category"`
	Severity           string     `json:"severity"`
	Confidence         string     `json:"confidence"`
	Summary            string     `json:"summary"`
	Detail             string     `json:"detail"`
	Scenario           string     `json:"scenario"`
	Locations          []Location `json:"locations"`
	Citations          []Citation `json:"citations,omitempty"`
	IntroducedByChange bool       `json:"introduced_by_change"`
	Verifier           string     `json:"verifier"`
	VerifierEvidence   []Citation `json:"verifier_evidence,omitempty"`
	RejectionReason    string     `json:"rejection_reason,omitempty"`
	Remediation        string     `json:"remediation,omitempty"`
}

// Validate checks a finding's shape: it must carry an ID, at least one valid
// causal reference, at least one valid location, a summary, a concrete failure
// scenario, valid enums, and a typed rejection reason exactly when rejected.
func (f Finding) Validate() error {
	if f.ID == "" {
		return errors.New("review: finding missing id")
	}
	if len(f.Causes) == 0 {
		return fmt.Errorf("review: finding %s has no causal reference", f.ID)
	}
	for _, c := range f.Causes {
		if !validCauseKind(c.Kind) {
			return fmt.Errorf("review: finding %s has invalid cause kind %q", f.ID, c.Kind)
		}
		if c.ID == "" {
			return fmt.Errorf("review: finding %s has an empty cause id", f.ID)
		}
	}
	if len(f.Locations) == 0 {
		return fmt.Errorf("review: finding %s has no location", f.ID)
	}
	for _, loc := range f.Locations {
		if err := loc.Validate(); err != nil {
			return fmt.Errorf("review: finding %s: %w", f.ID, err)
		}
	}
	if f.Summary == "" {
		return fmt.Errorf("review: finding %s missing summary", f.ID)
	}
	if f.Scenario == "" {
		return fmt.Errorf("review: finding %s missing a concrete failure scenario", f.ID)
	}
	if !validCategory(f.Category) {
		return fmt.Errorf("review: finding %s has invalid category %q", f.ID, f.Category)
	}
	if !validSeverity(f.Severity) {
		return fmt.Errorf("review: finding %s has invalid severity %q", f.ID, f.Severity)
	}
	if !validConfidence(f.Confidence) {
		return fmt.Errorf("review: finding %s has invalid confidence %q", f.ID, f.Confidence)
	}
	if !validDisposition(f.Verifier) {
		return fmt.Errorf("review: finding %s has invalid verifier disposition %q", f.ID, f.Verifier)
	}
	if f.Verifier == string(DispositionRejected) {
		if !validRejectionReason(f.RejectionReason) {
			return fmt.Errorf("review: rejected finding %s needs a typed rejection reason", f.ID)
		}
	} else if f.RejectionReason != "" {
		return fmt.Errorf("review: finding %s has a rejection reason without a rejected disposition", f.ID)
	}
	return nil
}

// PrimaryReceipt is a host agent's declaration that it examined a review unit.
type PrimaryReceipt struct {
	UnitID                     string        `json:"unit_id"`
	AcknowledgedPrimaryHunkIDs []string      `json:"acknowledged_primary_hunk_ids"`
	ContextCitations           []Citation    `json:"context_citations,omitempty"`
	FindingIDs                 []string      `json:"finding_ids,omitempty"`
	Uncertainties              []Uncertainty `json:"uncertainties,omitempty"`
	Reviewer                   string        `json:"reviewer"`
}

// Validate checks the receipt's shape. Exact hunk-set equality against the
// unit is enforced by the checker in a later task.
func (r PrimaryReceipt) Validate() error {
	if r.UnitID == "" {
		return errors.New("review: primary receipt missing unit id")
	}
	if r.Reviewer == "" {
		return fmt.Errorf("review: primary receipt for %s missing reviewer identity", r.UnitID)
	}
	for _, u := range r.Uncertainties {
		if u.Summary == "" {
			return fmt.Errorf("review: primary receipt for %s has an empty uncertainty summary", r.UnitID)
		}
	}
	return nil
}

// AuditReceipt is a host agent's declaration that it performed a required
// cross-cutting audit.
type AuditReceipt struct {
	AuditID                    string        `json:"audit_id"`
	AcknowledgedAuditTargetIDs []string      `json:"acknowledged_audit_target_ids"`
	ContextCitations           []Citation    `json:"context_citations,omitempty"`
	FindingIDs                 []string      `json:"finding_ids,omitempty"`
	Uncertainties              []Uncertainty `json:"uncertainties,omitempty"`
	Reviewer                   string        `json:"reviewer"`
}

// Validate checks the receipt's shape. An empty acknowledged-target set is a
// valid explicit receipt; exact target-set equality is enforced by the checker.
func (r AuditReceipt) Validate() error {
	if !validAuditID(r.AuditID) {
		return fmt.Errorf("review: audit receipt has unknown audit id %q", r.AuditID)
	}
	if r.Reviewer == "" {
		return fmt.Errorf("review: audit receipt for %s missing reviewer identity", r.AuditID)
	}
	for _, u := range r.Uncertainties {
		if u.Summary == "" {
			return fmt.Errorf("review: audit receipt for %s has an empty uncertainty summary", r.AuditID)
		}
	}
	return nil
}

// Report is the canonical review.report.v1 object a host agent submits.
type Report struct {
	Schema          string           `json:"schema"`
	ReviewID        string           `json:"review_id"`
	PlanDigest      string           `json:"plan_digest"`
	Base            SideIdentity     `json:"base"`
	Head            SideIdentity     `json:"head"`
	Recommendation  string           `json:"recommendation"`
	Findings        []Finding        `json:"findings"`
	PrimaryReceipts []PrimaryReceipt `json:"primary_receipts,omitempty"`
	AuditReceipts   []AuditReceipt   `json:"audit_receipts,omitempty"`
	Notes           string           `json:"notes,omitempty"`
}

// Validate checks report shape: schema, identifiers, recommendation enum,
// unique well-formed findings, well-formed receipts, and recommendation
// consistency with unrejected findings. Coverage, identity resolution, and
// freshness are enforced by the checker in a later task.
func (r Report) Validate() error {
	if r.Schema != ReportSchemaV1 {
		return fmt.Errorf("review: report schema %q is not %s", r.Schema, ReportSchemaV1)
	}
	if r.ReviewID == "" {
		return errors.New("review: report missing review id")
	}
	if r.PlanDigest == "" {
		return errors.New("review: report missing plan digest")
	}
	if !validRecommendation(r.Recommendation) {
		return fmt.Errorf("review: report has invalid recommendation %q", r.Recommendation)
	}
	seen := make(map[string]struct{}, len(r.Findings))
	for _, f := range r.Findings {
		if _, dup := seen[f.ID]; dup {
			return fmt.Errorf("review: duplicate finding id %q", f.ID)
		}
		seen[f.ID] = struct{}{}
		if err := f.Validate(); err != nil {
			return err
		}
	}
	for _, rc := range r.PrimaryReceipts {
		if err := rc.Validate(); err != nil {
			return err
		}
	}
	for _, rc := range r.AuditReceipts {
		if err := rc.Validate(); err != nil {
			return err
		}
	}
	return r.checkRecommendationConsistency()
}

// checkRecommendationConsistency enforces the severity/recommendation rules:
// an unrejected critical finding permits only request_changes or incomplete,
// and an unrejected major finding forbids approve.
func (r Report) checkRecommendationConsistency() error {
	for _, f := range r.Findings {
		if f.Verifier == string(DispositionRejected) {
			continue
		}
		switch Severity(f.Severity) {
		case SeverityCritical:
			if r.Recommendation != string(RecommendRequestChanges) && r.Recommendation != string(RecommendIncomplete) {
				return fmt.Errorf("review: unrejected critical finding %s forbids recommendation %q", f.ID, r.Recommendation)
			}
		case SeverityMajor:
			if r.Recommendation == string(RecommendApprove) {
				return fmt.Errorf("review: unrejected major finding %s forbids approve", f.ID)
			}
		}
	}
	return nil
}

// CheckResult is the status review check emits after validating a report.
type CheckResult struct {
	Schema         string   `json:"schema"`
	ReviewID       string   `json:"review_id"`
	Mode           Mode     `json:"mode"`
	Coverage       Coverage `json:"coverage"`
	Recommendation string   `json:"recommendation"`
	OK             bool     `json:"ok"`
	Problems       []string `json:"problems,omitempty"`
}

// Validate checks the internal consistency of a check status: valid enums, a
// direct check records not_required_direct coverage, an ok check cannot
// recommend incomplete, and an ok check carries no problems.
func (c CheckResult) Validate() error {
	switch c.Mode {
	case ModeDirect, ModeStructured:
	default:
		return fmt.Errorf("review: check result has invalid mode %q", c.Mode)
	}
	switch c.Coverage {
	case CoverageNotRequiredDirect, CoverageComplete, CoverageIncomplete:
	default:
		return fmt.Errorf("review: check result has invalid coverage %q", c.Coverage)
	}
	if !validRecommendation(c.Recommendation) {
		return fmt.Errorf("review: check result has invalid recommendation %q", c.Recommendation)
	}
	if c.Mode == ModeDirect && c.Coverage != CoverageNotRequiredDirect {
		return fmt.Errorf("review: direct check must record coverage %s", CoverageNotRequiredDirect)
	}
	if c.OK && c.Recommendation == string(RecommendIncomplete) {
		return errors.New("review: check cannot be ok while recommending incomplete")
	}
	if c.OK && len(c.Problems) > 0 {
		return errors.New("review: an ok check must report no problems")
	}
	return nil
}

func validRecommendation(s string) bool {
	switch Recommendation(s) {
	case RecommendApprove, RecommendComment, RecommendRequestChanges, RecommendIncomplete:
		return true
	}
	return false
}

func validCategory(s string) bool {
	switch Category(s) {
	case CategoryFunctionalCorrectness, CategorySecurityPrivacy, CategoryStabilityAvailability,
		CategoryDataIntegrityIntegration, CategoryPerformanceScalability, CategoryMaintainabilityQuality:
		return true
	}
	return false
}

func validSeverity(s string) bool {
	switch Severity(s) {
	case SeverityCritical, SeverityMajor, SeverityMinor:
		return true
	}
	return false
}

func validConfidence(s string) bool {
	switch Confidence(s) {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	}
	return false
}

func validDisposition(s string) bool {
	switch Disposition(s) {
	case DispositionConfirmed, DispositionPlausible, DispositionRejected, DispositionUnverified:
		return true
	}
	return false
}

func validRejectionReason(s string) bool {
	switch RejectionReason(s) {
	case RejectionContradictedByCode, RejectionUnreachableByInvariant, RejectionPreExisting,
		RejectionDuplicate, RejectionNotIntroduced:
		return true
	}
	return false
}

func validCauseKind(s string) bool {
	switch CauseKind(s) {
	case CausePrimaryUnit, CauseChangedPath, CauseAuditTarget:
		return true
	}
	return false
}

func validAuditID(s string) bool {
	for _, id := range RequiredAuditsV1 {
		if s == id {
			return true
		}
	}
	return false
}
