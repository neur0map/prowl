package review

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	UnitKindNormal          = "normal"
	UnitKindOversizedAtomic = "oversized_atomic"
	plannerVersionV1        = "1"
	textClassifierVersionV1 = "text.v1"
	contextRankerVersionV1  = "rank.v1"
)

// Evidence binds the plan to its indexed head and ordered local/policy evidence.
// Digest order is evidence order and therefore identity-bearing.
type Evidence struct {
	IndexSchema        string
	IndexVersion       string
	HeadIndexSignature []byte
	TrustedLocal       []Digest
	UntrustedPolicy    []Digest
	ForceStructured    bool
}

// PlanningEvidence is a descriptive alias retained for callers that prefer the
// phase-qualified name.
type PlanningEvidence = Evidence

// PlanErrorKind is a stable fatal planner error class.
type PlanErrorKind string

const (
	PlanErrorInvalidCapture PlanErrorKind = "invalid_capture"
	PlanErrorCanceled       PlanErrorKind = "canceled"
	PlanErrorIdentity       PlanErrorKind = "identity"
	PlanErrorSource         PlanErrorKind = "source"
	PlanErrorUnitSize       PlanErrorKind = "unit_size"
	PlanErrorInvariant      PlanErrorKind = "invariant"
)

// PlanError is returned only for failures that prevent an exact plan. Graph
// enrichment failures are omissions and never become PlanError.
type PlanError struct {
	Kind PlanErrorKind
	Path string
	Err  error
}

func (e *PlanError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("review: plan %s for %s: %v", e.Kind, e.Path, e.Err)
	}
	return fmt.Sprintf("review: plan %s: %v", e.Kind, e.Err)
}
func (e *PlanError) Unwrap() error { return e.Err }

// IDCollisionError is the typed form of ErrIDCollision. It names the colliding
// public identifier while retaining both full digests for programmatic checks.
type IDCollisionError struct {
	PublicID string
	Existing Digest
	Incoming Digest
}

func (e *IDCollisionError) Error() string {
	return fmt.Sprintf("%v: %s", ErrIDCollision, e.PublicID)
}

func (e *IDCollisionError) Unwrap() error { return ErrIDCollision }

type checkedIDSet struct {
	byPublic map[string]StableID
	probes   map[string]StableID
	probe    func(StableID) StableID
}

func newCheckedIDSet(probe func(StableID) StableID) *checkedIDSet {
	return &checkedIDSet{byPublic: map[string]StableID{}, probes: map[string]StableID{}, probe: probe}
}

func (s *checkedIDSet) Insert(id StableID) error {
	if existing, ok := s.byPublic[id.Public]; ok && existing.Full != id.Full {
		return &IDCollisionError{PublicID: id.Public, Existing: existing.Full, Incoming: id.Full}
	}
	candidate := id
	if s.probe != nil {
		candidate = s.probe(id)
	}
	if existing, ok := s.probes[candidate.Public]; ok && existing.Full != candidate.Full {
		return &IDCollisionError{PublicID: candidate.Public, Existing: existing.Full, Incoming: candidate.Full}
	}
	if _, ok := s.byPublic[id.Public]; !ok {
		s.byPublic[id.Public] = id
	}
	if _, ok := s.probes[candidate.Public]; !ok {
		s.probes[candidate.Public] = candidate
	}
	return nil
}

func (s *checkedIDSet) Lookup(public string) (StableID, error) {
	id, ok := s.byPublic[public]
	if !ok {
		return StableID{}, fmt.Errorf("review: identity %s was not registered", public)
	}
	return id, nil
}

// Planner builds the final deterministic plan. Zero caps/versions select the v1
// constants; lower caps are supported only as focused test seams.
type Planner struct {
	Graph             GraphQueries
	SymbolMapper      SymbolMapper
	ForceStructured   bool
	PlannerVersion    string
	MaxChangedLines   int
	MaxMandatoryBytes int
	// IDCollisionProbe is a test seam that changes only collision checking,
	// never emitted identities or their canonical full digests.
	IDCollisionProbe func(StableID) StableID
}

func (p *Planner) lineCap() int {
	if p != nil && p.MaxChangedLines > 0 {
		return p.MaxChangedLines
	}
	return MaxUnitChangedLinesV1
}
func (p *Planner) mandatoryCap() int {
	if p != nil && p.MaxMandatoryBytes > 0 {
		return p.MaxMandatoryBytes
	}
	return MaxUnitMandatoryJSONBytesV1
}
func (p *Planner) version() string {
	if p != nil && p.PlannerVersion != "" {
		return p.PlannerVersion
	}
	return plannerVersionV1
}

// PackHunk is one already-identified hunk entering a cohort/layer partition.
// GroupKey preserves symbol coalescing metadata but never makes siblings atomic.
type PackHunk struct {
	Path     RawPathRecord
	PathID   string
	Hunk     RawHunk
	HunkID   StableID
	GroupKey string
	Signals  []string
}

// PackedUnit is a pre-plan unit plus its identity inputs.
type PackedUnit struct {
	Kind       string
	Unit       Unit
	UnitStable StableID
	HunkIDs    []StableID
	Hunks      []RawHunk
	PathIDs    []string
	SignalIDs  []string
}

// MandatoryUnitFits applies the inclusive mandatory JSON cap.
func MandatoryUnitFits(unit Unit, capBytes int) (bool, int, error) {
	encoded, err := unit.CanonicalMandatoryJSON()
	if err != nil {
		return false, 0, err
	}
	return len(encoded) <= capBytes, len(encoded), nil
}

// PackReviewPartition greedily packs one real cohort/layer partition. Existing
// whole-hunk boundaries remain legal split points even within a symbol group.
// Only an individual hunk may become oversized or unreviewable.
func PackReviewPartition(scope Scope, defaultPathID, cohortID, layerID string, hunks []PackHunk, lineCap, mandatoryCap int) ([]PackedUnit, []RawHunk, error) {
	if lineCap <= 0 || mandatoryCap <= 0 {
		return nil, nil, errors.New("review: non-positive packing cap")
	}
	groups := packGroups(hunks)
	var packed []PackedUnit
	var large []RawHunk
	var current []PackHunk
	flush := func(kind string) error {
		if len(current) == 0 {
			return nil
		}
		unit, err := makePackedUnit(scope, defaultPathID, cohortID, layerID, kind, current)
		if err != nil {
			return err
		}
		packed = append(packed, unit)
		current = nil
		return nil
	}
	for _, group := range groups {
		groupLines := packChangedLines(group)
		candidateUnit, err := makePackedUnit(scope, defaultPathID, cohortID, layerID, UnitKindNormal, group)
		if err != nil {
			return nil, nil, err
		}
		fits, _, err := MandatoryUnitFits(candidateUnit.Unit, mandatoryCap)
		if err != nil {
			return nil, nil, err
		}
		if !fits {
			if err := flush(UnitKindNormal); err != nil {
				return nil, nil, err
			}
			for _, hunk := range group {
				large = append(large, hunk.Hunk)
			}
			continue
		}
		if groupLines > lineCap {
			if err := flush(UnitKindNormal); err != nil {
				return nil, nil, err
			}
			current = append(current, group...)
			if err := flush(UnitKindOversizedAtomic); err != nil {
				return nil, nil, err
			}
			continue
		}
		candidate := append(append([]PackHunk(nil), current...), group...)
		if len(current) > 0 {
			combined, err := makePackedUnit(scope, defaultPathID, cohortID, layerID, UnitKindNormal, candidate)
			if err != nil {
				return nil, nil, err
			}
			combinedFits, _, err := MandatoryUnitFits(combined.Unit, mandatoryCap)
			if err != nil {
				return nil, nil, err
			}
			if packChangedLines(candidate) > lineCap || !combinedFits {
				if err := flush(UnitKindNormal); err != nil {
					return nil, nil, err
				}
				candidate = append([]PackHunk(nil), group...)
			}
		}
		current = candidate
	}
	if err := flush(UnitKindNormal); err != nil {
		return nil, nil, err
	}
	return packed, large, nil
}

func packGroups(hunks []PackHunk) [][]PackHunk {
	groups := make([][]PackHunk, len(hunks))
	for i, hunk := range hunks {
		groups[i] = []PackHunk{hunk}
	}
	return groups
}

func packChangedLines(hunks []PackHunk) int {
	total := 0
	for _, hunk := range hunks {
		additions, deletions := hunkPayloadChurn(hunk.Hunk.Payload)
		total += additions + deletions
	}
	return total
}

func makePackedUnit(scope Scope, defaultPathID, cohortID, layerID, kind string, hunks []PackHunk) (PackedUnit, error) {
	unit := Unit{Schema: UnitSchemaV1, ReviewID: placeholderID("rvw_", 40), UnitID: placeholderID("u_", 32), CohortID: fixedOrPlaceholder(cohortID, "c_", 32), LayerID: fixedOrPlaceholder(layerID, "l_", 32), ScopeKind: scope.Kind, ObjectFormat: scope.ObjectFormat, Base: scope.Base, Head: scope.Head}
	out := PackedUnit{Kind: kind}
	for _, item := range hunks {
		pathID := item.PathID
		if pathID == "" {
			pathID = defaultPathID
		}
		if pathID == "" {
			pathID = placeholderID("p_", 32)
		}
		hunkID := item.HunkID
		if hunkID.Public == "" {
			full := sha256.Sum256(Frame(Field{Name: "path", Value: []byte(pathID)}, Field{Name: "hunk", Value: item.Hunk.Frame()}))
			hunkID = PublicID(HunkIDPrefixV1, full, PublicIDContentBytesV1)
		}
		unit.Hunks = append(unit.Hunks, UnitHunk{HunkID: hunkID.Public, PathID: pathID, OldPath: item.Path.OldPath, NewPath: item.Path.NewPath, Status: item.Path.Status, Ordinal: int(item.Hunk.Ordinal), OldStart: int(item.Hunk.OldStart), OldCount: int(item.Hunk.OldLines), NewStart: int(item.Hunk.NewStart), NewCount: int(item.Hunk.NewLines), NoFinalNewlineOld: item.Hunk.NoFinalNewlineOld, NoFinalNewlineNew: item.Hunk.NoFinalNewlineNew, PatchBase64: base64.StdEncoding.EncodeToString(item.Hunk.Payload)})
		out.HunkIDs = append(out.HunkIDs, hunkID)
		out.Hunks = append(out.Hunks, item.Hunk)
		out.PathIDs = append(out.PathIDs, pathID)
		out.SignalIDs = append(out.SignalIDs, item.Signals...)
	}
	out.PathIDs = sortedUniqueStrings(out.PathIDs)
	out.SignalIDs = sortedUniqueStrings(out.SignalIDs)
	out.UnitStable = UnitID(kind, out.HunkIDs)
	unit.UnitID = out.UnitStable.Public
	out.Unit = unit
	return out, nil
}

func placeholderID(prefix string, hexBytes int) string { return prefix + strings.Repeat("0", hexBytes) }
func fixedOrPlaceholder(value, prefix string, n int) string {
	if len(value) == len(prefix)+n {
		return value
	}
	return placeholderID(prefix, n)
}

// Build constructs the complete plan. Graph failures become stable omissions;
// only malformed capture/identity/size invariants return a typed error.
func (p *Planner) Build(ctx context.Context, capture Capture, view *HeadView, evidence Evidence) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, &PlanError{Kind: PlanErrorCanceled, Err: err}
	}
	if err := validatePlannerCapture(capture); err != nil {
		return Plan{}, &PlanError{Kind: PlanErrorInvalidCapture, Err: err}
	}
	scope := capture.Scope
	if scope.Digest == (Digest{}) {
		scope.Digest = ScopeDigest(scope.ObjectFormat, scope.Base, scope.Head, scope.Kind, capture.CanonicalPatch)
	}
	records := sortedPathRecords(capture.Paths)
	paths := make([]string, 0, len(records))
	for _, record := range records {
		paths = append(paths, recordPath(record))
	}
	graph := GraphQueries(nil)
	if p != nil {
		graph = p.Graph
	}
	if graph == nil && view != nil {
		graph = view.Query
	}
	enrichment := EnrichGraph(ctx, paths, graph)
	cohorts := BuildFileCohorts(paths, enrichment)

	planned := make(map[string]*plannedPath, len(records))
	var collisionProbe func(StableID) StableID
	if p != nil {
		collisionProbe = p.IDCollisionProbe
	}
	identityIDs := newCheckedIDSet(collisionProbe)
	attention := []AttentionSignal{}
	graphSignalsByPath := make(map[string][]AttentionSignal)
	graphReasonsByPath := make(map[string][]string)
	for _, omission := range enrichment.Omissions {
		signal := newAttentionSignal("graph_omission", omission.Path, omission.Error())
		attention = append(attention, signal)
		owners := []string{omission.Path}
		if omission.Path == "" {
			owners = paths
		}
		for _, owner := range owners {
			graphSignalsByPath[owner] = append(graphSignalsByPath[owner], signal)
			graphReasonsByPath[owner] = append(graphReasonsByPath[owner], omission.Error())
		}
	}
	for _, record := range records {
		pathName := recordPath(record)
		pathID := PathID(scope.Digest, record)
		if err := identityIDs.Insert(pathID); err != nil {
			return Plan{}, &PlanError{Kind: PlanErrorIdentity, Path: pathName, Err: err}
		}
		facts := enrichment.Facts[pathName]
		pp := &plannedPath{record: record, path: pathName, pathID: pathID, roles: append([]string(nil), facts.Roles...), hunkIDs: map[uint64]StableID{}, reviewability: map[uint64]HunkReviewability{}, hunkReasons: map[uint64]string{}}
		if len(pp.roles) == 0 {
			pp.roles = []string{RoleUnknown}
		}
		for _, hunk := range record.Hunks {
			hunkID := HunkID(scope.Digest, pathID.Full, hunk)
			pp.hunkIDs[hunk.Ordinal] = hunkID
			pp.reviewability[hunk.Ordinal] = HunkReviewable
			if err := identityIDs.Insert(hunkID); err != nil {
				return Plan{}, &PlanError{Kind: PlanErrorIdentity, Path: pathName, Err: err}
			}
		}
		var base, head []byte
		if record.TextClass == string(TextClassText) && len(record.Hunks) > 0 {
			var err error
			base, head, err = readPlanningSources(ctx, view, record)
			if err != nil {
				return Plan{}, &PlanError{Kind: PlanErrorSource, Path: pathName, Err: err}
			}
		}
		mapper := SymbolMapper{}
		if p != nil {
			mapper = p.SymbolMapper
		}
		pp.mappings, pp.symbolOmissions = mapper.Map(record, base, head)
		pp.groups = GroupHunkMappings(pp.mappings)
		pp.semantic = semanticTargets(pathID, pp.mappings)
		for _, target := range pp.semantic {
			if err := identityIDs.Insert(target.ID); err != nil {
				return Plan{}, &PlanError{Kind: PlanErrorIdentity, Path: pathName, Err: err}
			}
		}
		pp.class, pp.coverage, pp.reason = classifyPlanPath(record, pp.roles)
		pp.reason = appendPlanReasons(pp.reason, graphReasonsByPath[pathName])
		pp.signals = append(pp.signals, facts.Signals...)
		pp.signals = append(pp.signals, graphSignalsByPath[pathName]...)
		pp.signals = append(pp.signals, contentSignals(pp)...)
		for _, omission := range pp.symbolOmissions {
			signal := newAttentionSignal("symbol_mapping_omission", omission.Path, omission.Class+":"+string(omission.Side))
			pp.signals = append(pp.signals, signal)
			pp.reason = appendPlanReasons(pp.reason, []string{signal.Fact})
		}
		pp.signals = canonicalSignals(pp.signals)
		attention = append(attention, pp.signals...)
		planned[pathName] = pp
	}

	packedByCohort := make([]map[int][]PackedUnit, len(cohorts))
	for i := range packedByCohort {
		packedByCohort[i] = map[int][]PackedUnit{}
	}
	for cohortIndex, cohort := range cohorts {
		for _, layer := range cohort.Layers {
			items := partitionPackHunks(layer.Paths, planned)
			packed, large, err := PackReviewPartition(scope, "", placeholderID("c_", 32), placeholderID("l_", 32), items, p.lineCap(), p.mandatoryCap())
			if err != nil {
				return Plan{}, &PlanError{Kind: PlanErrorUnitSize, Err: err}
			}
			packedByCohort[cohortIndex][layer.Ordinal] = packed
			owned := make(map[string]bool)
			for _, unit := range packed {
				for _, hunkID := range unit.HunkIDs {
					owned[hunkID.Public] = true
				}
			}
			largeCount := 0
			for _, item := range items {
				if owned[item.HunkID.Public] {
					continue
				}
				owner := planned[recordPath(item.Path)]
				owner.reviewability[item.Hunk.Ordinal] = HunkUnreviewableLargeText
				owner.hunkReasons[item.Hunk.Ordinal] = string(HunkUnreviewableLargeText)
				signal := newAttentionSignal("unreviewable_large_text", owner.path, fmt.Sprintf("hunk %d mandatory JSON exceeds %d bytes", item.Hunk.Ordinal, p.mandatoryCap()))
				owner.signals = canonicalSignals(append(owner.signals, signal))
				attention = append(attention, signal)
				largeCount++
			}
			if largeCount != len(large) {
				return Plan{}, &PlanError{Kind: PlanErrorInvariant, Err: errors.New("large hunk ownership count mismatch")}
			}
		}
	}

	cohortStable := make([]StableID, len(cohorts))
	layerStable := make([]map[int]StableID, len(cohorts))
	for i, cohort := range cohorts {
		var unitIDs []StableID
		for _, layer := range cohort.Layers {
			for _, unit := range packedByCohort[i][layer.Ordinal] {
				if err := identityIDs.Insert(unit.UnitStable); err != nil {
					return Plan{}, &PlanError{Kind: PlanErrorIdentity, Err: err}
				}
				unitIDs = append(unitIDs, unit.UnitStable)
			}
		}
		cohortStable[i] = CohortID(scope.Digest, cohort.Key, unitIDs)
		if err := identityIDs.Insert(cohortStable[i]); err != nil {
			return Plan{}, &PlanError{Kind: PlanErrorIdentity, Err: err}
		}
		layerStable[i] = map[int]StableID{}
		for _, layer := range cohort.Layers {
			ids := make([]StableID, 0, len(packedByCohort[i][layer.Ordinal]))
			for _, unit := range packedByCohort[i][layer.Ordinal] {
				ids = append(ids, unit.UnitStable)
			}
			layerID := LayerID(cohortStable[i], uint64(layer.Ordinal), ids)
			layerStable[i][layer.Ordinal] = layerID
			if err := identityIDs.Insert(layerID); err != nil {
				return Plan{}, &PlanError{Kind: PlanErrorIdentity, Err: err}
			}
		}
	}

	var packedUnits []PackedUnit
	for i, cohort := range cohorts {
		for _, layer := range cohort.Layers {
			units := packedByCohort[i][layer.Ordinal]
			for j := range units {
				units[j].Unit.CohortID = cohortStable[i].Public
				units[j].Unit.LayerID = layerStable[i][layer.Ordinal].Public
				units[j].SignalIDs = sortedUniqueStrings(append(units[j].SignalIDs, signalIDsForPaths(planned, units[j].PathIDs)...))
				packedUnits = append(packedUnits, units[j])
			}
		}
	}
	updateCoverageAndReviewableChurn(planned)

	mode, required := ModeForChurn(capture.RawChurn, (p != nil && p.ForceStructured) || evidence.ForceStructured)
	audits := buildAudits(mode, cohorts, cohortStable, planned, packedUnits)
	identity, err := buildPlanIdentity(p, evidence, scope.Digest, records, planned, packedUnits, cohorts, cohortStable, layerStable, audits, identityIDs)
	if err != nil {
		return Plan{}, &PlanError{Kind: PlanErrorIdentity, Err: err}
	}
	reviewStable := ReviewID(identity)
	if err := identityIDs.Insert(reviewStable); err != nil {
		return Plan{}, &PlanError{Kind: PlanErrorIdentity, Err: err}
	}
	planDigest := ReviewPlanDigest(identity)
	for i := range packedUnits {
		before, err := packedUnits[i].Unit.CanonicalMandatoryJSON()
		if err != nil {
			return Plan{}, &PlanError{Kind: PlanErrorUnitSize, Err: err}
		}
		packedUnits[i].Unit.ReviewID = reviewStable.Public
		after, err := packedUnits[i].Unit.CanonicalMandatoryJSON()
		if err != nil {
			return Plan{}, &PlanError{Kind: PlanErrorUnitSize, Err: err}
		}
		if len(before) != len(after) {
			return Plan{}, &PlanError{Kind: PlanErrorInvariant, Err: errors.New("fixed-length ID substitution changed mandatory JSON size")}
		}
		if len(after) > p.mandatoryCap() {
			return Plan{}, &PlanError{Kind: PlanErrorUnitSize, Err: fmt.Errorf("final mandatory JSON is %d bytes", len(after))}
		}
	}

	plan := Plan{Schema: PlanSchemaV1, ReviewID: reviewStable.Public, PlanDigest: hex.EncodeToString(planDigest[:]), Mode: mode, StructuredRequired: required, Scope: scope, Stats: PlanStats{RawAdditions: capture.RawAdditions, RawDeletions: capture.RawDeletions, RawChurn: capture.RawChurn, ReviewableChurn: reviewableChurn(planned), ChangedPaths: len(records)}, AttentionSignals: canonicalSignals(attention)}
	for _, record := range records {
		pp := planned[recordPath(record)]
		plan.ChangedPaths = append(plan.ChangedPaths, PlanPath{PathID: pp.pathID.Public, OldPath: record.OldPath, NewPath: record.NewPath, Status: record.Status, ReviewClass: string(pp.class), Coverage: string(pp.coverage), Roles: append([]string(nil), pp.roles...), Reason: pp.reason})
	}
	for _, unit := range packedUnits {
		plan.PrimaryUnits = append(plan.PrimaryUnits, unit.Unit)
	}
	if mode == ModeStructured {
		plan.Cohorts = materializePlanCohorts(cohorts, cohortStable, layerStable, packedByCohort)
		plan.RequiredAudits = audits
	}
	plan.NextCommands = nextCommands(plan)
	if err := plan.Validate(); err != nil {
		return Plan{}, &PlanError{Kind: PlanErrorInvariant, Err: err}
	}
	return plan, nil
}

type plannedPath struct {
	record          RawPathRecord
	path            string
	pathID          StableID
	roles           []string
	class           ReviewClass
	coverage        PathCoverage
	reason          string
	mappings        []HunkSymbolMapping
	groups          []HunkGroup
	semantic        []SemanticTarget
	symbolOmissions []SymbolMappingOmission
	hunkIDs         map[uint64]StableID
	reviewability   map[uint64]HunkReviewability
	hunkReasons     map[uint64]string
	signals         []AttentionSignal
}

func validatePlannerCapture(capture Capture) error {
	if err := capture.Scope.Validate(); err != nil {
		return err
	}
	if len(capture.Paths) == 0 {
		return errors.New("capture has no changed paths")
	}
	if len(capture.Paths) > MaxChangedPathsV1 {
		return fmt.Errorf("capture has %d changed paths", len(capture.Paths))
	}
	seen := map[string]bool{}
	for _, record := range capture.Paths {
		p := recordPath(record)
		if p == "" {
			return errors.New("capture path has no old or new name")
		}
		key := record.NewPath + "\x00" + record.OldPath + "\x00" + record.Status
		if seen[key] {
			return fmt.Errorf("duplicate canonical path record %q", p)
		}
		seen[key] = true
	}
	return nil
}

func recordPath(record RawPathRecord) string {
	if record.NewPath != "" {
		return record.NewPath
	}
	return record.OldPath
}

func readPlanningSources(ctx context.Context, view *HeadView, record RawPathRecord) ([]byte, []byte, error) {
	if view == nil || view.Sources == nil {
		return nil, nil, errors.New("source resolver unavailable")
	}
	var base, head []byte
	if record.OldPath != "" {
		entry, err := view.Sources.Read(ctx, SideBase, record.OldPath, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("read base source: %w", err)
		}
		if !entry.Present {
			return nil, nil, errors.New("base source is absent")
		}
		base = entry.Bytes
	}
	if record.NewPath != "" {
		entry, err := view.Sources.Read(ctx, SideHead, record.NewPath, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("read head source: %w", err)
		}
		if !entry.Present {
			return nil, nil, errors.New("head source is absent")
		}
		head = entry.Bytes
	}
	return base, head, nil
}

func classifyPlanPath(record RawPathRecord, roles []string) (ReviewClass, PathCoverage, string) {
	if record.TextClass != string(TextClassText) {
		return ReviewClassUnreviewable, PathCoverageNone, "non-text content"
	}
	class := ReviewClassFull
	if containsString(roles, RoleMechanical) {
		class = ReviewClassMechanical
	}
	if len(record.Hunks) == 0 {
		return class, PathCoverageFull, "no primary-owned text hunk"
	}
	return class, PathCoverageFull, ""
}

func contentSignals(pp *plannedPath) []AttentionSignal {
	var out []AttentionSignal
	add := func(kind, fact string) { out = append(out, newAttentionSignal(kind, pp.path, fact)) }
	for _, target := range pp.semantic {
		switch target.Kind {
		case TargetRemovedSymbol:
			add("removed_symbol", target.Symbol.Kind+" "+target.Symbol.Name)
		case TargetChangedSignature:
			add("changed_signature", target.Symbol.Kind+" "+target.Symbol.Name)
		case TargetAddedFieldOption:
			add("added_field_option", target.Symbol.Kind+" "+target.Symbol.Name)
		}
	}
	for _, hunk := range pp.record.Hunks {
		a, d := hunkPayloadChurn(hunk.Payload)
		if d > 0 && d >= a*2 {
			add("deletion_heavy", fmt.Sprintf("hunk %d has %d deletions and %d additions", hunk.Ordinal, d, a))
		}
		if a > 0 && d > 0 && min(a, d)*2 >= max(a, d) {
			add("replacement_heavy", fmt.Sprintf("hunk %d replaces %d/%d lines", hunk.Ordinal, d, a))
		}
		if a+d > MaxUnitChangedLinesV1 {
			add("large_rewrite", fmt.Sprintf("hunk %d changes %d raw lines", hunk.Ordinal, a+d))
		}
	}
	if pp.class == ReviewClassUnreviewable {
		add("unreviewable_content", pp.reason)
	}
	return canonicalSignals(out)
}

func partitionPackHunks(paths []string, planned map[string]*plannedPath) []PackHunk {
	var items []PackHunk
	for _, p := range paths {
		pp := planned[p]
		signalIDs := make([]string, 0, len(pp.signals))
		for _, signal := range pp.signals {
			signalIDs = append(signalIDs, signal.ID)
		}
		for _, group := range pp.groups {
			groupKey := pp.pathID.Public + ":" + fmt.Sprint(group.Hunks[0].Ordinal)
			for _, hunk := range group.Hunks {
				items = append(items, PackHunk{Path: pp.record, PathID: pp.pathID.Public, Hunk: hunk, HunkID: pp.hunkIDs[hunk.Ordinal], GroupKey: groupKey, Signals: signalIDs})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		return cmp.Or(strings.Compare(recordPath(a.Path), recordPath(b.Path)), cmp.Compare(a.Hunk.OldStart, b.Hunk.OldStart), cmp.Compare(a.Hunk.NewStart, b.Hunk.NewStart), cmp.Compare(a.Hunk.Ordinal, b.Hunk.Ordinal), strings.Compare(a.HunkID.Public, b.HunkID.Public)) < 0
	})
	return items
}

func updateCoverageAndReviewableChurn(planned map[string]*plannedPath) {
	for _, pp := range planned {
		if pp.record.TextClass != string(TextClassText) || len(pp.record.Hunks) == 0 {
			continue
		}
		large := 0
		for _, state := range pp.reviewability {
			if state == HunkUnreviewableLargeText {
				large++
			}
		}
		if large == 0 {
			pp.coverage = PathCoverageFull
		} else {
			pp.coverage = PathCoveragePartial
		}
		if large > 0 {
			pp.reason = string(HunkUnreviewableLargeText)
		}
	}
}

func reviewableChurn(planned map[string]*plannedPath) int {
	total := 0
	for _, pp := range planned {
		for _, hunk := range pp.record.Hunks {
			if pp.reviewability[hunk.Ordinal] == HunkReviewable {
				a, d := hunkPayloadChurn(hunk.Payload)
				total += a + d
			}
		}
	}
	return total
}

func buildAudits(mode Mode, cohorts []FileCohort, cohortIDs []StableID, planned map[string]*plannedPath, units []PackedUnit) []PlanAudit {
	if mode != ModeStructured {
		return nil
	}
	sets := map[string]map[string]bool{}
	for _, id := range RequiredAuditsV1() {
		sets[id] = map[string]bool{}
	}
	for _, pp := range planned {
		hasPrimary := false
		for _, hunk := range pp.record.Hunks {
			hunkID := pp.hunkIDs[hunk.Ordinal]
			if pp.reviewability[hunk.Ordinal] == HunkReviewable {
				hasPrimary = true
			}
			_, deletions := hunkPayloadChurn(hunk.Payload)
			if deletions > 0 {
				sets[AuditRemovedBehaviorV1][hunkID.Public] = true
			}
			if pp.reviewability[hunk.Ordinal] == HunkUnreviewableLargeText {
				sets[AuditIntegrationGapV1][hunkID.Public] = true
			}
		}
		for _, target := range pp.semantic {
			switch target.Kind {
			case TargetRemovedSymbol:
				sets[AuditRemovedBehaviorV1][target.ID.Public] = true
				sets[AuditContractMigrationV1][target.ID.Public] = true
			case TargetChangedSignature, TargetAddedFieldOption:
				sets[AuditContractMigrationV1][target.ID.Public] = true
			}
		}
		if pp.class == ReviewClassMechanical || pp.class == ReviewClassUnreviewable || !hasPrimary {
			sets[AuditIntegrationGapV1][pp.pathID.Public] = true
		}
	}
	for _, cohortID := range cohortIDs {
		sets[AuditIntegrationGapV1][cohortID.Public] = true
	}
	for _, unit := range units {
		qualifies := false
		for _, pathID := range unit.PathIDs {
			for _, pp := range planned {
				if pp.pathID.Public == pathID && unitNeedsTestAudit(pp.roles) {
					qualifies = true
					break
				}
			}
		}
		if qualifies {
			sets[AuditTestMatrixV1][unit.UnitStable.Public] = true
		}
	}
	out := make([]PlanAudit, 0, 4)
	for _, id := range RequiredAuditsV1() {
		targets := make([]string, 0, len(sets[id]))
		for target := range sets[id] {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		out = append(out, PlanAudit{AuditID: id, TargetIDs: targets})
	}
	return out
}

func unitNeedsTestAudit(roles []string) bool {
	if containsString(roles, RoleTest) || containsString(roles, RoleMechanical) {
		return false
	}
	for _, role := range []string{RoleImplementation, RoleContract, RoleOperations, RoleConsumer} {
		if containsString(roles, role) {
			return true
		}
	}
	return false
}

func buildPlanIdentity(p *Planner, evidence Evidence, scope Digest, records []RawPathRecord, planned map[string]*plannedPath, units []PackedUnit, cohorts []FileCohort, cohortIDs []StableID, layerIDs []map[int]StableID, audits []PlanAudit, stable *checkedIDSet) (PlanIdentity, error) {
	identity := PlanIdentity{PlannerVersion: p.version(), ScopeDigest: scope, IndexSchema: cmp.Or(evidence.IndexSchema, "index.v1"), IndexVersion: cmp.Or(evidence.IndexVersion, "unknown"), HeadIndexSignature: append([]byte(nil), evidence.HeadIndexSignature...), TrustedEvidence: append([]Digest(nil), evidence.TrustedLocal...), UntrustedEvidence: append([]Digest(nil), evidence.UntrustedPolicy...), Constants: PlanConstants{StructuredThreshold: StructuredThresholdV1, UnitLineCap: uint64(p.lineCap()), MandatoryJSONCap: uint64(p.mandatoryCap()), TextClassifierVersion: textClassifierVersionV1, ContextRankerVersion: contextRankerVersionV1}}
	for _, record := range records {
		pp := planned[recordPath(record)]
		identity.Paths = append(identity.Paths, PlanPathEntry{PathID: pp.pathID, ReviewClass: string(pp.class), Coverage: string(pp.coverage), Reason: pp.reason, RoleIDs: append([]string(nil), pp.roles...)})
		for _, hunk := range record.Hunks {
			identity.Hunks = append(identity.Hunks, PlanHunkEntry{HunkID: pp.hunkIDs[hunk.Ordinal], Reviewability: string(pp.reviewability[hunk.Ordinal]), Reason: pp.hunkReasons[hunk.Ordinal]})
		}
	}
	for _, unit := range units {
		identity.Units = append(identity.Units, PlanUnitEntry{UnitID: unit.UnitStable, Kind: unit.Kind, HunkIDs: append([]StableID(nil), unit.HunkIDs...), AttentionSignalIDs: append([]string(nil), unit.SignalIDs...)})
	}
	for i, cohort := range cohorts {
		for _, layer := range cohort.Layers {
			var ids []StableID
			for _, unit := range units {
				if unit.Unit.CohortID == cohortIDs[i].Public && unit.Unit.LayerID == layerIDs[i][layer.Ordinal].Public {
					ids = append(ids, unit.UnitStable)
				}
			}
			identity.Cohorts = append(identity.Cohorts, PlanCohortEntry{CohortID: cohortIDs[i], LayerID: layerIDs[i][layer.Ordinal], UnitIDs: ids})
		}
	}
	for _, audit := range audits {
		ids := make([]StableID, 0, len(audit.TargetIDs))
		for _, target := range audit.TargetIDs {
			id, err := stable.Lookup(target)
			if err != nil {
				return PlanIdentity{}, err
			}
			ids = append(ids, id)
		}
		identity.Audits = append(identity.Audits, PlanAuditEntry{AuditID: audit.AuditID, TargetIDs: ids})
	}
	return identity, nil
}

func materializePlanCohorts(cohorts []FileCohort, cohortIDs []StableID, layerIDs []map[int]StableID, packed []map[int][]PackedUnit) []PlanCohort {
	out := make([]PlanCohort, 0, len(cohorts))
	for i, cohort := range cohorts {
		pc := PlanCohort{CohortID: cohortIDs[i].Public, Label: cohort.Label}
		for _, layer := range cohort.Layers {
			pl := PlanLayer{LayerID: layerIDs[i][layer.Ordinal].Public, Ordinal: layer.Ordinal}
			for _, unit := range packed[i][layer.Ordinal] {
				pl.UnitIDs = append(pl.UnitIDs, unit.UnitStable.Public)
				pc.UnitIDs = append(pc.UnitIDs, unit.UnitStable.Public)
			}
			pc.Layers = append(pc.Layers, pl)
		}
		out = append(out, pc)
	}
	return out
}

func canonicalSignals(in []AttentionSignal) []AttentionSignal {
	sort.Slice(in, func(i, j int) bool {
		return cmp.Or(strings.Compare(in[i].ID, in[j].ID), strings.Compare(in[i].Kind, in[j].Kind), strings.Compare(in[i].Fact, in[j].Fact)) < 0
	})
	out := in[:0]
	for _, signal := range in {
		if len(out) == 0 || out[len(out)-1].ID != signal.ID {
			out = append(out, signal)
		}
	}
	return out
}

func appendPlanReasons(current string, added []string) string {
	reasons := append([]string(nil), added...)
	if current != "" {
		reasons = append(reasons, current)
	}
	reasons = sortedUniqueStrings(reasons)
	return strings.Join(reasons, "; ")
}

func signalIDsForPaths(planned map[string]*plannedPath, pathIDs []string) []string {
	wanted := make(map[string]bool, len(pathIDs))
	for _, pathID := range pathIDs {
		wanted[pathID] = true
	}
	var ids []string
	for _, pp := range planned {
		if !wanted[pp.pathID.Public] {
			continue
		}
		for _, signal := range pp.signals {
			ids = append(ids, signal.ID)
		}
	}
	return sortedUniqueStrings(ids)
}

func nextCommands(plan Plan) []NextCommand {
	commands := make([]NextCommand, 0, len(plan.PrimaryUnits)+1)
	for _, unit := range plan.PrimaryUnits {
		commands = append(commands, NextCommand{Label: "review unit " + unit.UnitID, Command: "prowl-agent review unit " + plan.ReviewID + "/" + unit.UnitID})
	}
	if plan.Mode == ModeStructured {
		commands = append(commands, NextCommand{Label: "check review", Command: "prowl-agent review check --review " + plan.ReviewID + " --report review-results.json"})
	}
	if len(commands) == 0 {
		commands = append(commands, NextCommand{Label: "inspect review gaps", Command: "prowl-agent review plan"})
	}
	return commands
}
