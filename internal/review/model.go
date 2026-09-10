package review

import (
	"bytes"
	"encoding/json"
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
	CheckSchemaV1      = "review.check.v1"

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

// RequiredAuditsV1 returns a fresh copy of the four mandatory structured-mode
// audit identifiers in order. It never exposes shared mutable state.
func RequiredAuditsV1() []string {
	return []string{
		AuditRemovedBehaviorV1,
		AuditContractMigrationV1,
		AuditTestMatrixV1,
		AuditIntegrationGapV1,
	}
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

// Validate checks the resolved scope's kind and object format and binds each
// tagged identity to them: commit/range require base and head git_oids at the
// object-format width (20 bytes for sha1, 32 for sha256); workspace requires a
// base git_oid at that width and a 32-byte workspace_sha256 head. Absent or
// mismatched identities are rejected.
func (s Scope) Validate() error {
	return validateScopeSides(s.Kind, s.ObjectFormat, s.Base, s.Head, "scope")
}

// oidWidthFor returns the object-id byte width for a Git object format.
func oidWidthFor(objectFormat string) (int, bool) {
	switch objectFormat {
	case "sha1":
		return 20, true
	case "sha256":
		return 32, true
	}
	return 0, false
}

// requireGitOID enforces that side is a git_oid of exactly width bytes.
func requireGitOID(side SideIdentity, width int, label string) error {
	if side.Kind != SideGitOID {
		return fmt.Errorf("review: %s must be a git_oid, got %q", label, side.Kind)
	}
	if len(side.Value) != width {
		return fmt.Errorf("review: %s git_oid must be %d bytes, got %d", label, width, len(side.Value))
	}
	return nil
}

// validateScopeSides binds base/head identities to the scope kind and object
// format. It is shared by Scope and Unit, which both carry these fields.
func validateScopeSides(kind ScopeKind, objectFormat string, base, head SideIdentity, ctx string) error {
	width, ok := oidWidthFor(objectFormat)
	if !ok {
		return fmt.Errorf("review: %s has invalid object_format %q", ctx, objectFormat)
	}
	switch kind {
	case ScopeCommit, ScopeRange:
		if err := requireGitOID(base, width, ctx+" base"); err != nil {
			return err
		}
		if err := requireGitOID(head, width, ctx+" head"); err != nil {
			return err
		}
	case ScopeWorkspace:
		if err := requireGitOID(base, width, ctx+" base"); err != nil {
			return err
		}
		if head.Kind != SideWorkspaceSHA256 {
			return fmt.Errorf("review: %s head must be workspace_sha256, got %q", ctx, head.Kind)
		}
		if len(head.Value) != 32 {
			return fmt.Errorf("review: %s head workspace_sha256 must be 32 bytes, got %d", ctx, len(head.Value))
		}
	default:
		return fmt.Errorf("review: %s has invalid kind %q", ctx, kind)
	}
	return nil
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

// ReviewClass is a changed path's review class.
type ReviewClass string

const (
	ReviewClassFull         ReviewClass = "full"
	ReviewClassMechanical   ReviewClass = "mechanical"
	ReviewClassUnreviewable ReviewClass = "unreviewable"
)

// PathCoverage is a changed path's aggregate reviewable-coverage state.
type PathCoverage string

const (
	PathCoverageFull    PathCoverage = "full"
	PathCoveragePartial PathCoverage = "partial"
	PathCoverageNone    PathCoverage = "none"
)

// HunkReviewability is a single hunk's reviewability.
type HunkReviewability string

const (
	HunkReviewable            HunkReviewability = "reviewable"
	HunkUnreviewableLargeText HunkReviewability = "unreviewable_large_text"
)

// PlanStats holds the deterministic churn accounting for a plan.
type PlanStats struct {
	RawAdditions    int `json:"raw_additions"`
	RawDeletions    int `json:"raw_deletions"`
	RawChurn        int `json:"raw_churn"`
	ReviewableChurn int `json:"reviewable_churn"`
	ChangedPaths    int `json:"changed_paths"`
}

// PlanPath is a plan-level changed-path record. Every tracked and untracked
// changed path appears here with its class, coverage, roles, and reason.
type PlanPath struct {
	PathID      string   `json:"path_id"`
	OldPath     string   `json:"old_path,omitempty"`
	NewPath     string   `json:"new_path"`
	Status      string   `json:"status"`
	ReviewClass string   `json:"review_class"`
	Coverage    string   `json:"coverage"`
	Roles       []string `json:"roles"`
	Reason      string   `json:"reason,omitempty"`
}

// Validate checks a plan changed-path record's shape.
func (p PlanPath) Validate() error {
	if p.PathID == "" {
		return errors.New("review: plan path missing path_id")
	}
	if p.NewPath == "" && p.OldPath == "" {
		return fmt.Errorf("review: plan path %s missing old and new path", p.PathID)
	}
	if p.Status == "" {
		return fmt.Errorf("review: plan path %s missing status", p.PathID)
	}
	if !validReviewClass(p.ReviewClass) {
		return fmt.Errorf("review: plan path %s has invalid review class %q", p.PathID, p.ReviewClass)
	}
	if !validPathCoverage(p.Coverage) {
		return fmt.Errorf("review: plan path %s has invalid coverage %q", p.PathID, p.Coverage)
	}
	return nil
}

// PlanLayer is a dependency-ordered review layer within a cohort.
type PlanLayer struct {
	LayerID string   `json:"layer_id"`
	Ordinal int      `json:"ordinal"`
	UnitIDs []string `json:"unit_ids"`
}

// PlanCohort is a logically related group of changed files and its ordered
// layers.
type PlanCohort struct {
	CohortID string      `json:"cohort_id"`
	Label    string      `json:"label"`
	Layers   []PlanLayer `json:"layers"`
	UnitIDs  []string    `json:"unit_ids"`
}

// Validate checks a cohort's shape and layer ordering.
func (c PlanCohort) Validate() error {
	if c.CohortID == "" {
		return errors.New("review: cohort missing cohort_id")
	}
	if len(c.Layers) == 0 {
		return fmt.Errorf("review: cohort %s has no layers", c.CohortID)
	}
	for _, l := range c.Layers {
		if l.LayerID == "" {
			return fmt.Errorf("review: cohort %s has a layer without layer_id", c.CohortID)
		}
	}
	return nil
}

// PlanAudit is a required cross-cutting audit and its exact target set.
type PlanAudit struct {
	AuditID   string   `json:"audit_id"`
	TargetIDs []string `json:"target_ids"`
}

// AttentionSignal is a transparent, deterministically-derived attention marker.
type AttentionSignal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Fact string `json:"fact"`
}

// NextCommand is one exact progressive-disclosure command the plan ends with.
type NextCommand struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

// Plan is the persisted review plan: scope, mode, statistics, the complete
// changed-path table, and - in structured mode - cohorts/layers, primary units,
// required audits with exact target sets, and the ordered next commands.
type Plan struct {
	Schema             string            `json:"schema"`
	ReviewID           string            `json:"review_id"`
	PlanDigest         string            `json:"plan_digest"`
	Mode               Mode              `json:"mode"`
	StructuredRequired bool              `json:"structured_required"`
	Scope              Scope             `json:"scope"`
	Stats              PlanStats         `json:"stats"`
	ChangedPaths       []PlanPath        `json:"changed_paths"`
	AttentionSignals   []AttentionSignal `json:"attention_signals,omitempty"`
	Cohorts            []PlanCohort      `json:"cohorts,omitempty"`
	PrimaryUnits       []Unit            `json:"primary_units,omitempty"`
	RequiredAudits     []PlanAudit       `json:"required_audits,omitempty"`
	NextCommands       []NextCommand     `json:"next_commands"`
}

// Validate checks the plan's structural contract: schema/identity, mode
// consistency, a non-empty changed-path table, exact next commands, and - in
// structured mode - cohorts plus all four required audits with well-formed
// primary units. Direct plans must not carry structured collections.
func (p Plan) Validate() error {
	if p.Schema != PlanSchemaV1 {
		return fmt.Errorf("review: plan schema %q is not %s", p.Schema, PlanSchemaV1)
	}
	if !validReviewID(p.ReviewID) {
		return fmt.Errorf("review: plan has malformed review id %q", p.ReviewID)
	}
	if !isCanonicalDigestHex(p.PlanDigest) {
		return fmt.Errorf("review: plan has non-canonical plan digest %q", p.PlanDigest)
	}
	switch p.Mode {
	case ModeDirect, ModeStructured:
	default:
		return fmt.Errorf("review: plan has invalid mode %q", p.Mode)
	}
	if p.Mode == ModeDirect && p.StructuredRequired {
		return errors.New("review: direct plan cannot be structured_required")
	}
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if len(p.ChangedPaths) == 0 {
		return errors.New("review: plan has no changed paths")
	}
	for _, cp := range p.ChangedPaths {
		if err := cp.Validate(); err != nil {
			return err
		}
	}
	if len(p.NextCommands) == 0 {
		return errors.New("review: plan missing next commands")
	}
	if p.Mode == ModeStructured {
		if len(p.Cohorts) == 0 {
			return errors.New("review: structured plan has no cohorts")
		}
		for _, c := range p.Cohorts {
			if err := c.Validate(); err != nil {
				return err
			}
		}
		present := make(map[string]struct{}, len(p.RequiredAudits))
		for _, a := range p.RequiredAudits {
			if !validAuditID(a.AuditID) {
				return fmt.Errorf("review: plan has unknown required audit %q", a.AuditID)
			}
			present[a.AuditID] = struct{}{}
		}
		for _, id := range RequiredAuditsV1() {
			if _, ok := present[id]; !ok {
				return fmt.Errorf("review: structured plan missing required audit %s", id)
			}
		}
	} else if len(p.Cohorts) != 0 || len(p.RequiredAudits) != 0 {
		// Direct plans may carry bounded primary units, but never the
		// structured-only cohort/layer and required-audit collections.
		return errors.New("review: direct plan must not carry cohorts or required audits")
	}
	// Primary units are validated in both modes: a direct plan may return a
	// single bounded unit, or partition-respecting units when one partition
	// does not fit.
	for _, u := range p.PrimaryUnits {
		if err := u.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// UnitHunk is one owned hunk in a review.unit.v1 mandatory object. HunkID is
// the stable public ID reviewers acknowledge in primary coverage receipts.
type UnitHunk struct {
	HunkID            string `json:"hunk_id"`
	PathID            string `json:"path_id"`
	OldPath           string `json:"old_path"`
	NewPath           string `json:"new_path"`
	Status            string `json:"status"`
	Ordinal           int    `json:"ordinal"`
	OldStart          int    `json:"old_start"`
	OldCount          int    `json:"old_count"`
	NewStart          int    `json:"new_start"`
	NewCount          int    `json:"new_count"`
	NoFinalNewlineOld bool   `json:"old_no_final_newline"`
	NoFinalNewlineNew bool   `json:"new_no_final_newline"`
	PatchBase64       string `json:"patch_base64"`
}

// Unit is the review.unit.v1 mandatory object: the smallest bounded review
// territory a host agent acknowledges. It carries the fixed identity/scope
// fields and its ordered owned hunks with exact base64 patch bytes. Plan-derived
// metadata (kind, roles, signals, symbols, context) is excluded from this object.
type Unit struct {
	Schema       string       `json:"schema"`
	ReviewID     string       `json:"review_id"`
	UnitID       string       `json:"unit_id"`
	CohortID     string       `json:"cohort_id"`
	LayerID      string       `json:"layer_id"`
	ScopeKind    ScopeKind    `json:"scope_kind"`
	ObjectFormat string       `json:"object_format"`
	Base         SideIdentity `json:"base"`
	Head         SideIdentity `json:"head"`
	Hunks        []UnitHunk   `json:"hunks"`
}

// Validate checks the mandatory unit object's shape.
func (u Unit) Validate() error {
	if u.Schema != UnitSchemaV1 {
		return fmt.Errorf("review: unit schema %q is not %s", u.Schema, UnitSchemaV1)
	}
	if !validReviewID(u.ReviewID) {
		return fmt.Errorf("review: unit has malformed review id %q", u.ReviewID)
	}
	if u.UnitID == "" {
		return errors.New("review: unit missing unit_id")
	}
	if u.CohortID == "" {
		return fmt.Errorf("review: unit %s missing cohort_id", u.UnitID)
	}
	if u.LayerID == "" {
		return fmt.Errorf("review: unit %s missing layer_id", u.UnitID)
	}
	if err := validateScopeSides(u.ScopeKind, u.ObjectFormat, u.Base, u.Head, "unit "+u.UnitID); err != nil {
		return err
	}
	if len(u.Hunks) == 0 {
		return fmt.Errorf("review: unit %s owns no hunks", u.UnitID)
	}
	for i, h := range u.Hunks {
		if h.HunkID == "" {
			return fmt.Errorf("review: unit %s hunk %d missing hunk_id", u.UnitID, i)
		}
		if h.PathID == "" {
			return fmt.Errorf("review: unit %s hunk %d missing path_id", u.UnitID, i)
		}
		if h.NewPath == "" && h.OldPath == "" {
			return fmt.Errorf("review: unit %s hunk %d missing old and new path", u.UnitID, i)
		}
		if h.Status == "" {
			return fmt.Errorf("review: unit %s hunk %d missing status", u.UnitID, i)
		}
		if h.PatchBase64 == "" {
			return fmt.Errorf("review: unit %s hunk %d missing patch_base64", u.UnitID, i)
		}
	}
	return nil
}

// CanonicalMandatoryJSON serializes the unit as CanonicalUnitMandatoryJSONV1:
// compact UTF-8 JSON emitted from the fixed-field struct in declaration order,
// with HTML escaping disabled, standard padded base64 for tagged identity
// values, no optional context, and exactly one trailing LF. Because every
// identity field is fixed-length, replacing zero-filled placeholder IDs with
// real IDs of the same length never changes the serialized size.
func (u Unit) CanonicalMandatoryJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(u); err != nil {
		return nil, err
	}
	// json.Encoder.Encode writes exactly one trailing newline.
	return buf.Bytes(), nil
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

// Validate checks the location's shape and its canonical side proof. Exactly
// one proof form is allowed: a content-backed location carries a canonical
// 32-byte content hash (64 lowercase hex), while a non-content location
// (gitlink/special) carries a tagged side identity plus an entry type. Range
// locations must be content-backed with 1 <= start <= end. It cannot resolve
// the location against a tree; that is the checker's job in a later task.
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
	isRange := LocationKind(l.Kind) == LocationRange
	contentBacked := l.ContentHash != ""
	if contentBacked {
		// Content-backed proof: reject every non-content proof field.
		if l.Identity != nil {
			return errors.New("content-backed location must not carry a tagged side identity")
		}
		if l.Mode != 0 {
			return errors.New("content-backed location must not carry a mode")
		}
		if l.EntryType != "" {
			return errors.New("content-backed location must not carry an entry type")
		}
		if !isCanonicalDigestHex(l.ContentHash) {
			return fmt.Errorf("location content hash %q is not a canonical 32-byte digest", l.ContentHash)
		}
		if isRange {
			if l.Start < 1 || l.End < l.Start {
				return fmt.Errorf("range location requires 1 <= start <= end, got start=%d end=%d", l.Start, l.End)
			}
		} else if l.Start != 0 || l.End != 0 {
			return errors.New("content-backed path location must not carry range coordinates")
		}
	} else {
		// Non-content proof: reject the content hash and range coordinates.
		if isRange {
			return errors.New("range location must be text and content-backed")
		}
		if l.Identity == nil {
			return errors.New("non-content location needs a tagged side identity proof")
		}
		if err := l.Identity.Validate(); err != nil {
			return fmt.Errorf("location identity: %w", err)
		}
		if l.EntryType == "" {
			return errors.New("non-content location needs an entry type")
		}
		if l.Start != 0 || l.End != 0 {
			return errors.New("non-content location must not carry range coordinates")
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

// Validate checks that a citation names a kind and references a concrete
// target: an id (e.g. a hunk or path id) or a repository path.
func (c Citation) Validate() error {
	if c.Kind == "" {
		return errors.New("review: citation missing kind")
	}
	if c.ID == "" && c.Path == "" {
		return errors.New("review: citation must reference an id or path")
	}
	return nil
}

// validateCitations validates each citation in a slice, labeling failures.
func validateCitations(cites []Citation, label string) error {
	for i, c := range cites {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("review: %s citation %d: %w", label, i, err)
		}
	}
	return nil
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
	if err := validateCitations(f.Citations, fmt.Sprintf("finding %s supporting", f.ID)); err != nil {
		return err
	}
	if err := validateCitations(f.VerifierEvidence, fmt.Sprintf("finding %s verifier", f.ID)); err != nil {
		return err
	}
	if f.Summary == "" {
		return fmt.Errorf("review: finding %s missing summary", f.ID)
	}
	if f.Scenario == "" {
		return fmt.Errorf("review: finding %s missing a concrete failure scenario", f.ID)
	}
	if f.Detail == "" {
		return fmt.Errorf("review: finding %s missing a detailed explanation", f.ID)
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
		if len(f.VerifierEvidence) == 0 {
			return fmt.Errorf("review: rejected finding %s needs verifier evidence citations", f.ID)
		}
		if len(f.Citations) == 0 {
			return fmt.Errorf("review: rejected finding %s needs supporting citations", f.ID)
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
	if err := validateCitations(r.ContextCitations, fmt.Sprintf("primary receipt %s context", r.UnitID)); err != nil {
		return err
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
	if err := validateCitations(r.ContextCitations, fmt.Sprintf("audit receipt %s context", r.AuditID)); err != nil {
		return err
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
	if !validReviewID(r.ReviewID) {
		return fmt.Errorf("review: report has malformed review id %q", r.ReviewID)
	}
	if !isCanonicalDigestHex(r.PlanDigest) {
		return fmt.Errorf("review: report has non-canonical plan digest %q", r.PlanDigest)
	}
	if err := r.Base.Validate(); err != nil {
		return fmt.Errorf("review: report base: %w", err)
	}
	if err := r.Head.Validate(); err != nil {
		return fmt.Errorf("review: report head: %w", err)
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

// CheckStatus is the checker's overall verdict. Non-complete states exit
// non-zero at the CLI boundary.
type CheckStatus string

const (
	CheckComplete   CheckStatus = "complete"
	CheckIncomplete CheckStatus = "incomplete"
	CheckStale      CheckStatus = "stale"
	CheckInvalid    CheckStatus = "invalid"
)

// CheckResult is the status review check emits after validating a report.
type CheckResult struct {
	Schema         string      `json:"schema"`
	ReviewID       string      `json:"review_id"`
	Mode           Mode        `json:"mode"`
	Status         CheckStatus `json:"status"`
	Coverage       Coverage    `json:"coverage"`
	Recommendation string      `json:"recommendation"`
	Problems       []string    `json:"problems,omitempty"`
}

// Complete reports whether the check verdict is a clean pass.
func (c CheckResult) Complete() bool { return c.Status == CheckComplete }

// Validate enforces the exact mode/status/coverage/recommendation/problem
// combinations: direct checks carry not_required_direct coverage and structured
// checks carry complete/incomplete coverage; a complete verdict has complete
// coverage (structured), no problems, and never recommends incomplete; and
// incomplete, stale, and invalid verdicts must list problems and recommend
// incomplete.
func (c CheckResult) Validate() error {
	if c.Schema != CheckSchemaV1 {
		return fmt.Errorf("review: check schema %q is not %s", c.Schema, CheckSchemaV1)
	}
	if !validReviewID(c.ReviewID) {
		return fmt.Errorf("review: check has malformed review id %q", c.ReviewID)
	}
	switch c.Mode {
	case ModeDirect, ModeStructured:
	default:
		return fmt.Errorf("review: check result has invalid mode %q", c.Mode)
	}
	if !validCheckStatus(c.Status) {
		return fmt.Errorf("review: check result has invalid status %q", c.Status)
	}
	switch c.Coverage {
	case CoverageNotRequiredDirect, CoverageComplete, CoverageIncomplete:
	default:
		return fmt.Errorf("review: check result has invalid coverage %q", c.Coverage)
	}
	if !validRecommendation(c.Recommendation) {
		return fmt.Errorf("review: check result has invalid recommendation %q", c.Recommendation)
	}
	// Mode fixes the coverage domain.
	if c.Mode == ModeDirect && c.Coverage != CoverageNotRequiredDirect {
		return fmt.Errorf("review: direct check must record coverage %s", CoverageNotRequiredDirect)
	}
	if c.Mode == ModeStructured && c.Coverage == CoverageNotRequiredDirect {
		return errors.New("review: structured check must record complete or incomplete coverage")
	}
	// Status governs recommendation and problems.
	switch c.Status {
	case CheckComplete:
		if len(c.Problems) != 0 {
			return errors.New("review: complete check must report no problems")
		}
		if c.Recommendation == string(RecommendIncomplete) {
			return errors.New("review: complete check cannot recommend incomplete")
		}
		if c.Mode == ModeStructured && c.Coverage != CoverageComplete {
			return errors.New("review: complete structured check requires complete coverage")
		}
	case CheckIncomplete, CheckStale, CheckInvalid:
		if len(c.Problems) == 0 {
			return fmt.Errorf("review: %s check must list problems", c.Status)
		}
		if c.Recommendation != string(RecommendIncomplete) {
			return fmt.Errorf("review: %s check requires recommendation incomplete", c.Status)
		}
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
	switch s {
	case AuditRemovedBehaviorV1, AuditContractMigrationV1, AuditTestMatrixV1, AuditIntegrationGapV1:
		return true
	}
	return false
}

func validObjectFormat(s string) bool { return s == "sha1" || s == "sha256" }

func validReviewClass(s string) bool {
	switch ReviewClass(s) {
	case ReviewClassFull, ReviewClassMechanical, ReviewClassUnreviewable:
		return true
	}
	return false
}

func validPathCoverage(s string) bool {
	switch PathCoverage(s) {
	case PathCoverageFull, PathCoveragePartial, PathCoverageNone:
		return true
	}
	return false
}

func validCheckStatus(s CheckStatus) bool {
	switch s {
	case CheckComplete, CheckIncomplete, CheckStale, CheckInvalid:
		return true
	}
	return false
}

// isLowerHex reports whether s is exactly n lowercase hex characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// isCanonicalDigestHex reports whether s is a canonical 32-byte digest rendered
// as 64 lowercase hex characters.
func isCanonicalDigestHex(s string) bool { return isLowerHex(s, 64) }

// validReviewID reports whether s is a canonical public review ID: the rvw_
// prefix plus 40 lowercase hex characters (the first 20 digest bytes).
func validReviewID(s string) bool {
	if !strings.HasPrefix(s, ReviewIDPrefixV1) {
		return false
	}
	return isLowerHex(s[len(ReviewIDPrefixV1):], 40)
}
