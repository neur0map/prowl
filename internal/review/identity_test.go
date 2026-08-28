package review

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestFramedFieldsV1Vector is the externally-specified normative vector for the
// FramedFieldsV1 primitive. Every other identity schema composes Frame, so this
// vector anchors the entire encoding.
func TestFramedFieldsV1Vector(t *testing.T) {
	got := Frame(Field{Name: "a", Value: []byte{0x01}}, Field{Name: "bb", Value: []byte("x")})
	wantHex := "00016100000000000000010100026262000000000000000178"
	if hex.EncodeToString(got) != wantHex {
		t.Fatalf("%x", got)
	}
}

// TestFramedListVector locks the FrameList count/length framing.
func TestFramedListVector(t *testing.T) {
	got := FrameList([]byte{0xaa}, []byte{0xbb, 0xcc})
	wantHex := "00000000000000020000000000000001aa0000000000000002bbcc"
	if hex.EncodeToString(got) != wantHex {
		t.Fatalf("%x", got)
	}
}

func TestFramedFieldsV1RejectsNonASCIIName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("non-ASCII field name accepted")
		}
	}()
	_ = Frame(Field{Name: "\u00e9", Value: []byte{0x00}})
}

func TestFramedFieldsV1RejectsEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("empty field name accepted")
		}
	}()
	_ = Frame(Field{Name: "", Value: []byte{0x00}})
}

func TestPublicIDPrefixWidth(t *testing.T) {
	var full Digest
	for i := range full {
		full[i] = byte(i)
	}
	got := PublicID("p_", full, PublicIDContentBytesV1)
	if got.Public != "p_000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("public=%s", got.Public)
	}
	if got.Full != full {
		t.Fatal("full digest not preserved")
	}
	rid := PublicID("rvw_", full, PublicIDReviewBytesV1)
	if rid.Public != "rvw_000102030405060708090a0b0c0d0e0f10111213" {
		t.Fatalf("review public=%s", rid.Public)
	}
}

func TestHunkIDIncludesOwningPath(t *testing.T) {
	scope := sha256.Sum256([]byte("scope"))
	hunk := RawHunk{Ordinal: 0, OldStart: 1, NewStart: 1, Payload: []byte("+x\n")}
	a := HunkID(scope, sha256.Sum256([]byte("a.go")), hunk)
	b := HunkID(scope, sha256.Sum256([]byte("b.go")), hunk)
	if a.Public == b.Public {
		t.Fatal("same edit in different files collided")
	}
}

// identityFixture builds the fixed inputs behind every normative identity
// vector below. Keeping one canonical fixture makes the locked digests and
// public IDs auditable against a single set of inputs.
type identityFixture struct {
	scope       Digest
	sha1side    SideIdentity
	sha256side  SideIdentity
	wtree       []WorkspaceTreeRecord
	rec         RawPathRecord
	recWithHunk RawPathRecord
	hunk        RawHunk
	cpd         Digest
	pathDigest  Digest
	hunkless    StableID
	hid         StableID
	unit        StableID
	cohort      StableID
	layer       StableID
	target      StableID
	plan        PlanIdentity
}

func newIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	d := func(s string) Digest { return sha256.Sum256([]byte(s)) }

	var f identityFixture
	f.scope = d("scope")

	sha1oid, err := hex.DecodeString("da39a3ee5e6b4b0d3255bfef95601890afd80709")
	if err != nil {
		t.Fatalf("decode sha1 oid: %v", err)
	}
	f.sha1side = SideIdentity{Kind: SideGitOID, Value: sha1oid}
	sha256val := d("head-256")
	f.sha256side = SideIdentity{Kind: SideWorkspaceSHA256, Value: sha256val[:]}

	c1 := d("content-1")
	f.wtree = []WorkspaceTreeRecord{
		{Path: "b/second.go", Mode: 0o100644, Kind: "regular", Content: &c1},
		{Path: "a/first.go", Mode: 0o120000, Kind: "symlink", Content: nil},
	}

	oldc := d("old")
	newc := d("new")
	f.rec = RawPathRecord{
		Kind: "tracked", Status: "M", OldPath: "pkg/a.go", NewPath: "pkg/a.go",
		OldMode: 0o100644, NewMode: 0o100644,
		OldSide:   SideIdentity{Kind: SideGitOID, Value: sha1oid},
		NewSide:   SideIdentity{Kind: SideGitOID, Value: sha1oid},
		TextClass: "text", Additions: 3, Deletions: 1,
		OldContentDigest: &oldc, NewContentDigest: &newc,
	}
	f.hunk = RawHunk{Ordinal: 0, OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 3, Payload: []byte("+x\n")}
	f.recWithHunk = f.rec
	f.recWithHunk.Hunks = []RawHunk{f.hunk}
	f.cpd = CanonicalPatchDigest([]RawPathRecord{f.recWithHunk})

	f.hunkless = PathID(f.scope, f.rec)
	f.pathDigest = d("pkg/a.go")
	f.hid = HunkID(f.scope, f.pathDigest, f.hunk)
	f.unit = UnitID("behavior", []StableID{f.hid, f.hunkless})
	f.cohort = CohortID(f.scope, "core", []StableID{f.unit})
	f.layer = LayerID(f.cohort, 0, []StableID{f.unit})
	sig := d("signature")
	f.target = TargetID(RawTarget{
		Kind: "removed_symbol", PathID: f.pathDigest, Side: "base",
		SymbolKind: "func", SymbolName: "OldFunc", Start: 10, End: 20, SignatureDigest: &sig,
	})

	trusted := d("trusted")
	untrusted := d("untrusted")
	f.plan = PlanIdentity{
		PlannerVersion: "1", ScopeDigest: f.scope,
		IndexSchema: "index.v1", IndexVersion: "7",
		HeadIndexSignature: []byte{0xde, 0xad, 0xbe, 0xef},
		Paths:              []PlanPathEntry{{PathID: f.hunkless, ReviewClass: "full", Coverage: "full", RoleID: "role_core"}},
		Hunks:              []PlanHunkEntry{{HunkID: f.hid, Reviewability: "reviewable"}},
		Units:              []PlanUnitEntry{{UnitID: f.unit, Kind: "behavior", HunkIDs: []StableID{f.hid}, AttentionSignalIDs: []string{"sig_changed_signature"}}},
		Cohorts:            []PlanCohortEntry{{CohortID: f.cohort, LayerID: f.layer, UnitIDs: []StableID{f.unit}}},
		Audits:             []PlanAuditEntry{{AuditID: AuditRemovedBehaviorV1, TargetIDs: []StableID{f.target}}},
		TrustedEvidence:    []Digest{trusted},
		UntrustedEvidence:  []Digest{untrusted},
		Constants: PlanConstants{
			StructuredThreshold: StructuredThresholdV1, UnitLineCap: MaxUnitChangedLinesV1,
			MandatoryJSONCap: MaxUnitMandatoryJSONBytesV1, TextClassifierVersion: "text.v1", ContextRankerVersion: "rank.v1",
		},
	}
	return f
}

// TestIdentityNormativeVectors locks the byte, digest, and public-ID output of
// every v1 identity schema. A change to any encoding must update these vectors
// deliberately; an accidental drift breaks review IDs and fails here.
func TestIdentityNormativeVectors(t *testing.T) {
	f := newIdentityFixture(t)
	hx := hex.EncodeToString

	cases := []struct {
		name string
		got  string
		want string
	}{
		// Byte-level side identities (SHA-1 and SHA-256 object formats).
		{"side_sha1_bytes", hx(f.sha1side.Frame()),
			"00046b696e6400000000000000076769745f6f6964000576616c75650000000000000014da39a3ee5e6b4b0d3255bfef95601890afd80709"},
		{"side_sha256_bytes", hx(f.sha256side.Frame()),
			"00046b696e640000000000000010776f726b73706163655f736861323536000576616c75650000000000000020cbda520a024a318658ff6b09846e69eface74f1a3fc6319918a7d283c3024da5"},

		// WorkspaceTreeV1 head identity digest.
		{"workspace_tree_v1_head", hx(WorkspaceHeadIdentity(f.wtree).Value),
			"910fd4c4146803ac2a826a94854d4ba9c01e5f0df992a54032421fb6e99dba26"},

		// CanonicalPatchV1 digest.
		{"canonical_patch_v1_digest", hx(f.cpd[:]),
			"ddd86d4b44d44383fe2033f64231d3e65f2b3b34eb283e448521fbdc56d2f13c"},

		// ScopeIdentityV1 digest.
		{"scope_identity_v1_digest", func() string {
			sd := ScopeDigest("sha1", f.sha1side, f.sha256side, ScopeWorkspace, f.cpd)
			return hx(sd[:])
		}(),
			"f9cf4d3a2416392c8ddfe030f6ad91bb38de9164f388dffab41ffea99573afe3"},

		// Hunkless path ID.
		{"path_id_hunkless_public", f.hunkless.Public, "p_522588bfda4f00f8268c5963a7f1006a"},

		// Path-qualified hunk ID.
		{"hunk_id_public", f.hid.Public, "h_469ca12ff16ed26004edb547ce8cd096"},

		// Unit, cohort, layer, semantic target public IDs.
		{"unit_id_public", f.unit.Public, "u_f29df3146ad6f722262284d746f2f571"},
		{"cohort_id_public", f.cohort.Public, "c_462a1b1d2909a6b580903d2fe9c1a6db"},
		{"layer_id_public", f.layer.Public, "l_d5e3fadeed73dd4518309651110cba29"},
		{"semantic_target_id_public", f.target.Public, "t_aa8aee5a74f8470a5a28c4db2a9a2aef"},

		// ReviewPlanIdentityV1 digest and public review ID.
		{"review_plan_v1_digest", func() string {
			pd := ReviewPlanDigest(f.plan)
			return hx(pd[:])
		}(),
			"2f4ebfc21389083824eba72f49ebf43ee007997562b2f147f70317dda81937f0"},
		{"review_id_public", ReviewID(f.plan).Public, "rvw_2f4ebfc21389083824eba72f49ebf43ee0079975"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got=%s\nwant=%s", c.name, c.got, c.want)
		}
	}
}

// TestIdentityPublicIDDerivesFromFull proves the public string is exactly the
// prefixed hex of the leading digest bytes at the schema's width.
func TestIdentityPublicIDDerivesFromFull(t *testing.T) {
	f := newIdentityFixture(t)
	if want := PathIDPrefixV1 + hex.EncodeToString(f.hunkless.Full[:PublicIDContentBytesV1]); f.hunkless.Public != want {
		t.Fatalf("path public %s != %s", f.hunkless.Public, want)
	}
	pd := ReviewPlanDigest(f.plan)
	if want := ReviewIDPrefixV1 + hex.EncodeToString(pd[:PublicIDReviewBytesV1]); ReviewID(f.plan).Public != want {
		t.Fatalf("review public %s != %s", ReviewID(f.plan).Public, want)
	}
}

// TestIdentityCanonicalPatchV1SortsRecords proves canonical ordering is
// independent of input order.
func TestIdentityCanonicalPatchV1SortsRecords(t *testing.T) {
	a := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "a.go"}
	b := RawPathRecord{Kind: "tracked", Status: "A", NewPath: "b.go"}
	forward := CanonicalPatchDigest([]RawPathRecord{a, b})
	reverse := CanonicalPatchDigest([]RawPathRecord{b, a})
	if forward != reverse {
		t.Fatal("canonical patch digest depends on input order")
	}
}

// TestIdentityWorkspaceHeadIsSHA256 proves the workspace head side is tagged
// workspace_sha256 with a 32-byte value.
func TestIdentityWorkspaceHeadIsSHA256(t *testing.T) {
	f := newIdentityFixture(t)
	head := WorkspaceHeadIdentity(f.wtree)
	if head.Kind != SideWorkspaceSHA256 {
		t.Fatalf("workspace head kind %q", head.Kind)
	}
	if len(head.Value) != 32 {
		t.Fatalf("workspace head value length %d", len(head.Value))
	}
}
