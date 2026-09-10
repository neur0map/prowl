// Package review defines the transport-independent domain for Prowl's native
// large-change review subsystem: the stable v1 data models, the churn/mode
// threshold rules, and the content-qualified identity primitives that every
// later task consumes.
//
// This file owns the FramedFieldsV1 canonical encoding and every identity
// schema derived from it. All encodings are deterministic and locked by the
// normative vectors in identity_test.go. Changing any byte layout here changes
// review IDs and must be a deliberate, vector-updating edit.
package review

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

// Identity prefixes and public-ID widths (in raw digest bytes) for v1.
const (
	PathIDPrefixV1   = "p_"
	HunkIDPrefixV1   = "h_"
	UnitIDPrefixV1   = "u_"
	CohortIDPrefixV1 = "c_"
	LayerIDPrefixV1  = "l_"
	TargetIDPrefixV1 = "t_"
	ReviewIDPrefixV1 = "rvw_"

	// Content-derived IDs expose the first 16 digest bytes (32 hex chars).
	PublicIDContentBytesV1 = 16
	// The review ID exposes the first 20 digest bytes (40 hex chars).
	PublicIDReviewBytesV1 = 20
)

// Digest is a raw SHA-256 hash used throughout the review identity schemas.
type Digest [32]byte

// Field is one named value in a FramedFieldsV1 record. Names must be ASCII.
type Field struct {
	Name  string
	Value []byte
}

// StableID is a content-derived identifier: a human-facing public prefix+hex
// string and the full SHA-256 digest it was truncated from.
type StableID struct {
	Public string
	Full   Digest
}

// Frame encodes fields under the FramedFieldsV1 rule:
//
//	field := uint16be(name_length) | ASCII_name |
//	         uint64be(value_length) | value_bytes
//
// Field names must be non-empty ASCII no longer than 65535 bytes. A violation
// is a programming error and panics, because field names are fixed schema
// constants, never runtime data.
func Frame(fields ...Field) []byte {
	var out []byte
	for _, f := range fields {
		name := []byte(f.Name)
		if len(name) == 0 || len(name) > 0xFFFF {
			panic(fmt.Sprintf("review: field name length %d out of range", len(name)))
		}
		for _, b := range name {
			if b >= 0x80 {
				panic(fmt.Sprintf("review: non-ASCII field name %q", f.Name))
			}
		}
		out = binary.BigEndian.AppendUint16(out, uint16(len(name)))
		out = append(out, name...)
		out = binary.BigEndian.AppendUint64(out, uint64(len(f.Value)))
		out = append(out, f.Value...)
	}
	return out
}

// FrameList encodes an ordered list under the FramedFieldsV1 rule:
//
//	list := uint64be(item_count) |
//	        repeated(uint64be(item_length) | item_bytes)
func FrameList(items ...[]byte) []byte {
	out := binary.BigEndian.AppendUint64(nil, uint64(len(items)))
	for _, item := range items {
		out = binary.BigEndian.AppendUint64(out, uint64(len(item)))
		out = append(out, item...)
	}
	return out
}

// PublicID truncates full to publicBytes and renders prefix + lowercase hex.
func PublicID(prefix string, full Digest, publicBytes int) StableID {
	if publicBytes < 0 || publicBytes > len(full) {
		panic(fmt.Sprintf("review: public id width %d out of range", publicBytes))
	}
	return StableID{
		Public: prefix + hex.EncodeToString(full[:publicBytes]),
		Full:   full,
	}
}

// u64 encodes v as unsigned big-endian 64-bit, the default integer width for
// values inside framed records.
func u64(v uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, v)
}

// boolByte encodes a flag as a single 0/1 byte.
func boolByte(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

// optDigest returns the raw digest bytes when present, or an empty value when
// absent. A present digest is always 32 bytes, so length disambiguates.
func optDigest(d *Digest) []byte {
	if d == nil {
		return nil
	}
	out := make([]byte, len(d))
	copy(out, d[:])
	return out
}

// SideKind tags one side of a change identity.
type SideKind string

const (
	SideGitOID          SideKind = "git_oid"
	SideWorkspaceSHA256 SideKind = "workspace_sha256"
	SideAbsent          SideKind = "absent"
)

// SideIdentity is a framed (kind, value) pair identifying one revision side.
// Value holds raw bytes: a Git OID for git_oid, a SHA-256 for
// workspace_sha256, and is empty for absent.
type SideIdentity struct {
	Kind  SideKind `json:"kind"`
	Value []byte   `json:"value,omitempty"`
}

// Frame encodes the side as a FramedFieldsV1 (kind, value) record.
func (s SideIdentity) Frame() []byte {
	return Frame(
		Field{Name: "kind", Value: []byte(s.Kind)},
		Field{Name: "value", Value: s.Value},
	)
}

// AbsentSide is the identity of a missing revision side.
func AbsentSide() SideIdentity { return SideIdentity{Kind: SideAbsent} }

// Validate enforces the allowed kinds and their exact value widths: a git_oid
// is a 20-byte (sha1) or 32-byte (sha256) object id, a workspace_sha256 is a
// 32-byte digest, and an absent side carries no value. A zero-value identity
// (empty kind) is invalid.
func (s SideIdentity) Validate() error {
	switch s.Kind {
	case SideGitOID:
		if len(s.Value) != 20 && len(s.Value) != 32 {
			return fmt.Errorf("review: git_oid identity must be 20 or 32 bytes, got %d", len(s.Value))
		}
	case SideWorkspaceSHA256:
		if len(s.Value) != 32 {
			return fmt.Errorf("review: workspace_sha256 identity must be 32 bytes, got %d", len(s.Value))
		}
	case SideAbsent:
		if len(s.Value) != 0 {
			return fmt.Errorf("review: absent identity must carry no value, got %d bytes", len(s.Value))
		}
	default:
		return fmt.Errorf("review: side identity has invalid kind %q", s.Kind)
	}
	return nil
}

// RawHunk is a canonical raw hunk record within an owning path.
type RawHunk struct {
	Ordinal           uint64
	OldStart          uint64
	OldLines          uint64
	NewStart          uint64
	NewLines          uint64
	NoFinalNewlineOld bool
	NoFinalNewlineNew bool
	Payload           []byte
}

// Frame encodes the hunk in canonical field order, binding the exact payload
// bytes plus their SHA-256.
func (h RawHunk) Frame() []byte {
	sum := sha256.Sum256(h.Payload)
	return Frame(
		Field{Name: "ordinal", Value: u64(h.Ordinal)},
		Field{Name: "old_start", Value: u64(h.OldStart)},
		Field{Name: "old_lines", Value: u64(h.OldLines)},
		Field{Name: "new_start", Value: u64(h.NewStart)},
		Field{Name: "new_lines", Value: u64(h.NewLines)},
		Field{Name: "no_final_newline_old", Value: boolByte(h.NoFinalNewlineOld)},
		Field{Name: "no_final_newline_new", Value: boolByte(h.NoFinalNewlineNew)},
		Field{Name: "payload", Value: h.Payload},
		Field{Name: "payload_sha256", Value: sum[:]},
	)
}

// RawPathRecord is a canonical tracked/untracked path record.
type RawPathRecord struct {
	Kind             string
	Status           string
	OldPath          string
	NewPath          string
	OldMode          uint32
	NewMode          uint32
	OldSide          SideIdentity
	NewSide          SideIdentity
	TextClass        string
	Additions        uint64
	Deletions        uint64
	OldContentDigest *Digest
	NewContentDigest *Digest
	Hunks            []RawHunk
}

// Frame encodes the record in canonical field order: identifying fields,
// classification and counts, content digests, then the framed hunk list.
func (r RawPathRecord) Frame() []byte {
	hunkItems := make([][]byte, len(r.Hunks))
	for i := range r.Hunks {
		hunkItems[i] = r.Hunks[i].Frame()
	}
	return Frame(
		Field{Name: "kind", Value: []byte(r.Kind)},
		Field{Name: "status", Value: []byte(r.Status)},
		Field{Name: "old_path", Value: []byte(r.OldPath)},
		Field{Name: "new_path", Value: []byte(r.NewPath)},
		Field{Name: "old_mode", Value: u64(uint64(r.OldMode))},
		Field{Name: "new_mode", Value: u64(uint64(r.NewMode))},
		Field{Name: "old_side", Value: r.OldSide.Frame()},
		Field{Name: "new_side", Value: r.NewSide.Frame()},
		Field{Name: "text_class", Value: []byte(r.TextClass)},
		Field{Name: "additions", Value: u64(r.Additions)},
		Field{Name: "deletions", Value: u64(r.Deletions)},
		Field{Name: "old_content", Value: optDigest(r.OldContentDigest)},
		Field{Name: "new_content", Value: optDigest(r.NewContentDigest)},
		Field{Name: "hunks", Value: FrameList(hunkItems...)},
	)
}

// sortedPathRecords returns records in canonical order: by new path, then old
// path, then status.
func sortedPathRecords(records []RawPathRecord) []RawPathRecord {
	sorted := make([]RawPathRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.NewPath != b.NewPath {
			return a.NewPath < b.NewPath
		}
		if a.OldPath != b.OldPath {
			return a.OldPath < b.OldPath
		}
		return a.Status < b.Status
	})
	return sorted
}

// CanonicalPatchV1 frames the path records as an ordered list in canonical
// order (new path, old path, status).
func CanonicalPatchV1(records []RawPathRecord) []byte {
	sorted := sortedPathRecords(records)
	items := make([][]byte, len(sorted))
	for i := range sorted {
		items[i] = sorted[i].Frame()
	}
	return FrameList(items...)
}

// CanonicalPatchDigest is the full SHA-256 of CanonicalPatchV1.
func CanonicalPatchDigest(records []RawPathRecord) Digest {
	return sha256.Sum256(CanonicalPatchV1(records))
}

// PathID identifies a changed path: full scope digest plus the canonical raw
// path record.
func PathID(scope Digest, record RawPathRecord) StableID {
	b := Frame(
		Field{Name: "scope", Value: scope[:]},
		Field{Name: "record", Value: record.Frame()},
	)
	return PublicID(PathIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// HunkID identifies a hunk within its owning path: full scope digest, full
// path digest, hunk ordinal, and the raw hunk record. Including the path digest
// prevents identical edits in different files from colliding.
func HunkID(scope Digest, path Digest, hunk RawHunk) StableID {
	b := Frame(
		Field{Name: "scope", Value: scope[:]},
		Field{Name: "path", Value: path[:]},
		Field{Name: "ordinal", Value: u64(hunk.Ordinal)},
		Field{Name: "hunk", Value: hunk.Frame()},
	)
	return PublicID(HunkIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// UnitID identifies a review unit: ordered full path-qualified hunk digests
// plus the unit kind.
func UnitID(kind string, hunks []StableID) StableID {
	b := Frame(
		Field{Name: "kind", Value: []byte(kind)},
		Field{Name: "hunks", Value: fullDigestList(hunks)},
	)
	return PublicID(UnitIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// CohortID identifies a cohort: full scope digest, stable cohort label, and
// ordered member unit IDs.
func CohortID(scope Digest, label string, units []StableID) StableID {
	b := Frame(
		Field{Name: "scope", Value: scope[:]},
		Field{Name: "label", Value: []byte(label)},
		Field{Name: "units", Value: fullDigestList(units)},
	)
	return PublicID(CohortIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// LayerID identifies a layer: owning cohort ID, topological ordinal, and
// ordered member unit IDs.
func LayerID(cohort StableID, ordinal uint64, units []StableID) StableID {
	b := Frame(
		Field{Name: "cohort", Value: cohort.Full[:]},
		Field{Name: "ordinal", Value: u64(ordinal)},
		Field{Name: "units", Value: fullDigestList(units)},
	)
	return PublicID(LayerIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// RawTarget is a semantic audit target descriptor.
type RawTarget struct {
	Kind            string
	PathID          Digest
	Side            string
	SymbolKind      string
	SymbolName      string
	Start           uint64
	End             uint64
	SignatureDigest *Digest
}

// TargetID identifies a semantic audit target: kind, owning path digest, side,
// symbol kind/name, source coordinates, and signature/content digest.
func TargetID(t RawTarget) StableID {
	b := Frame(
		Field{Name: "kind", Value: []byte(t.Kind)},
		Field{Name: "path", Value: t.PathID[:]},
		Field{Name: "side", Value: []byte(t.Side)},
		Field{Name: "symbol_kind", Value: []byte(t.SymbolKind)},
		Field{Name: "symbol_name", Value: []byte(t.SymbolName)},
		Field{Name: "start", Value: u64(t.Start)},
		Field{Name: "end", Value: u64(t.End)},
		Field{Name: "signature", Value: optDigest(t.SignatureDigest)},
	)
	return PublicID(TargetIDPrefixV1, sha256.Sum256(b), PublicIDContentBytesV1)
}

// WorkspaceTreeRecord is one final path in the workspace head tree.
type WorkspaceTreeRecord struct {
	Path    string
	Mode    uint32
	Kind    string
	Content *Digest
}

// WorkspaceTreeV1 frames the sorted final path records that define workspace
// head identity. Records are sorted by raw UTF-8 path bytes.
func WorkspaceTreeV1(records []WorkspaceTreeRecord) []byte {
	sorted := make([]WorkspaceTreeRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})
	items := make([][]byte, len(sorted))
	for i := range sorted {
		items[i] = Frame(
			Field{Name: "path", Value: []byte(sorted[i].Path)},
			Field{Name: "mode", Value: u64(uint64(sorted[i].Mode))},
			Field{Name: "kind", Value: []byte(sorted[i].Kind)},
			Field{Name: "content", Value: optDigest(sorted[i].Content)},
		)
	}
	return FrameList(items...)
}

// WorkspaceHeadIdentity derives the workspace_sha256 side identity from the
// workspace tree encoding.
func WorkspaceHeadIdentity(records []WorkspaceTreeRecord) SideIdentity {
	sum := sha256.Sum256(WorkspaceTreeV1(records))
	value := make([]byte, len(sum))
	copy(value, sum[:])
	return SideIdentity{Kind: SideWorkspaceSHA256, Value: value}
}

// ScopeIdentityV1 encodes the scope identity schema in canonical field order:
// schema, object format, tagged base identity, tagged head identity, scope
// mode, and the 32-byte canonical-patch digest.
func ScopeIdentityV1(objectFormat string, base, head SideIdentity, mode ScopeKind, canonicalPatch Digest) []byte {
	return Frame(
		Field{Name: "schema", Value: []byte(ScopeSchemaV1)},
		Field{Name: "object_format", Value: []byte(objectFormat)},
		Field{Name: "base", Value: base.Frame()},
		Field{Name: "head", Value: head.Frame()},
		Field{Name: "mode", Value: []byte(mode)},
		Field{Name: "canonical_patch", Value: canonicalPatch[:]},
	)
}

// ScopeDigest is the full SHA-256 of ScopeIdentityV1.
func ScopeDigest(objectFormat string, base, head SideIdentity, mode ScopeKind, canonicalPatch Digest) Digest {
	return sha256.Sum256(ScopeIdentityV1(objectFormat, base, head, mode, canonicalPatch))
}

// PlanPathEntry is one ordered path row in the plan identity.
type PlanPathEntry struct {
	PathID      StableID
	ReviewClass string
	Coverage    string
	Reason      string
	RoleIDs     []string
}

// PlanHunkEntry is one ordered path-qualified hunk row in the plan identity.
type PlanHunkEntry struct {
	HunkID        StableID
	Reviewability string
	Reason        string
}

// PlanUnitEntry is one ordered primary unit row in the plan identity.
type PlanUnitEntry struct {
	UnitID             StableID
	Kind               string
	HunkIDs            []StableID
	AttentionSignalIDs []string
}

// PlanCohortEntry is one ordered cohort/layer row in the plan identity.
type PlanCohortEntry struct {
	CohortID StableID
	LayerID  StableID
	UnitIDs  []StableID
}

// PlanAuditEntry is one ordered required-audit row in the plan identity.
type PlanAuditEntry struct {
	AuditID   string
	TargetIDs []StableID
}

// PlanConstants pins the planner constants that influence a plan's identity.
type PlanConstants struct {
	StructuredThreshold   uint64
	UnitLineCap           uint64
	MandatoryJSONCap      uint64
	TextClassifierVersion string
	ContextRankerVersion  string
}

// PlanIdentity holds every already-computed component that feeds the plan
// identity encoding. Capture and planning logic in later tasks populate it.
type PlanIdentity struct {
	PlannerVersion     string
	ScopeDigest        Digest
	IndexSchema        string
	IndexVersion       string
	HeadIndexSignature []byte
	Paths              []PlanPathEntry
	Hunks              []PlanHunkEntry
	Units              []PlanUnitEntry
	Cohorts            []PlanCohortEntry
	Audits             []PlanAuditEntry
	TrustedEvidence    []Digest
	UntrustedEvidence  []Digest
	Constants          PlanConstants
}

// ReviewPlanIdentityV1 encodes the plan identity in canonical field order.
// Any planning-relevant output or constant that changes alters one of these
// encoded fields; formatting-only changes that produce the same plan do not.
func ReviewPlanIdentityV1(p PlanIdentity) []byte {
	pathItems := make([][]byte, len(p.Paths))
	for i, e := range p.Paths {
		pathItems[i] = Frame(
			Field{Name: "path_id", Value: e.PathID.Full[:]},
			Field{Name: "review_class", Value: []byte(e.ReviewClass)},
			Field{Name: "coverage", Value: []byte(e.Coverage)},
			Field{Name: "reason", Value: []byte(e.Reason)},
			Field{Name: "role_ids", Value: stringList(e.RoleIDs)},
		)
	}
	hunkItems := make([][]byte, len(p.Hunks))
	for i, e := range p.Hunks {
		hunkItems[i] = Frame(
			Field{Name: "hunk_id", Value: e.HunkID.Full[:]},
			Field{Name: "reviewability", Value: []byte(e.Reviewability)},
			Field{Name: "reason", Value: []byte(e.Reason)},
		)
	}
	unitItems := make([][]byte, len(p.Units))
	for i, e := range p.Units {
		unitItems[i] = Frame(
			Field{Name: "unit_id", Value: e.UnitID.Full[:]},
			Field{Name: "kind", Value: []byte(e.Kind)},
			Field{Name: "hunk_ids", Value: fullDigestList(e.HunkIDs)},
			Field{Name: "attention_signal_ids", Value: stringList(e.AttentionSignalIDs)},
		)
	}
	cohortItems := make([][]byte, len(p.Cohorts))
	for i, e := range p.Cohorts {
		cohortItems[i] = Frame(
			Field{Name: "cohort_id", Value: e.CohortID.Full[:]},
			Field{Name: "layer_id", Value: e.LayerID.Full[:]},
			Field{Name: "unit_ids", Value: fullDigestList(e.UnitIDs)},
		)
	}
	auditItems := make([][]byte, len(p.Audits))
	for i, e := range p.Audits {
		auditItems[i] = Frame(
			Field{Name: "audit_id", Value: []byte(e.AuditID)},
			Field{Name: "target_ids", Value: fullDigestList(e.TargetIDs)},
		)
	}
	constants := Frame(
		Field{Name: "structured_threshold", Value: u64(p.Constants.StructuredThreshold)},
		Field{Name: "unit_line_cap", Value: u64(p.Constants.UnitLineCap)},
		Field{Name: "mandatory_json_cap", Value: u64(p.Constants.MandatoryJSONCap)},
		Field{Name: "text_classifier_version", Value: []byte(p.Constants.TextClassifierVersion)},
		Field{Name: "context_ranker_version", Value: []byte(p.Constants.ContextRankerVersion)},
	)
	return Frame(
		Field{Name: "schema", Value: []byte(PlanSchemaV1)},
		Field{Name: "planner_version", Value: []byte(p.PlannerVersion)},
		Field{Name: "scope", Value: p.ScopeDigest[:]},
		Field{Name: "index_schema", Value: []byte(p.IndexSchema)},
		Field{Name: "index_version", Value: []byte(p.IndexVersion)},
		Field{Name: "head_index_signature", Value: p.HeadIndexSignature},
		Field{Name: "paths", Value: FrameList(pathItems...)},
		Field{Name: "hunks", Value: FrameList(hunkItems...)},
		Field{Name: "units", Value: FrameList(unitItems...)},
		Field{Name: "cohorts", Value: FrameList(cohortItems...)},
		Field{Name: "audits", Value: FrameList(auditItems...)},
		Field{Name: "trusted_evidence", Value: digestList(p.TrustedEvidence)},
		Field{Name: "untrusted_evidence", Value: digestList(p.UntrustedEvidence)},
		Field{Name: "constants", Value: constants},
	)
}

// ReviewPlanDigest is the full SHA-256 of ReviewPlanIdentityV1.
func ReviewPlanDigest(p PlanIdentity) Digest {
	return sha256.Sum256(ReviewPlanIdentityV1(p))
}

// ReviewID derives the public review identifier from the plan identity digest.
func ReviewID(p PlanIdentity) StableID {
	return PublicID(ReviewIDPrefixV1, ReviewPlanDigest(p), PublicIDReviewBytesV1)
}

// fullDigestList frames an ordered list of stable-ID full digests.
func fullDigestList(ids []StableID) []byte {
	items := make([][]byte, len(ids))
	for i := range ids {
		items[i] = ids[i].Full[:]
	}
	return FrameList(items...)
}

// stringList frames an ordered list of UTF-8 strings.
func stringList(ss []string) []byte {
	items := make([][]byte, len(ss))
	for i := range ss {
		items[i] = []byte(ss[i])
	}
	return FrameList(items...)
}

// digestList frames an ordered list of raw digests.
func digestList(ds []Digest) []byte {
	items := make([][]byte, len(ds))
	for i := range ds {
		items[i] = ds[i][:]
	}
	return FrameList(items...)
}
