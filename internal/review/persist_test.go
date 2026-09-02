//go:build unix

package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
)

func sha256Digest(b []byte) Digest { return sha256.Sum256(b) }

// planIdentity derives production-valid canonical identity bytes (the real
// ReviewPlanIdentityV1 encoding, made distinct per name through PlannerVersion),
// its review id, and plan digest, exactly as the store recomputes them.
func planIdentity(name string) (idBytes []byte, reviewID, planDigest string) {
	pi := PlanIdentity{PlannerVersion: name, IndexSchema: "prowl.index.v1", IndexVersion: "1"}
	idBytes = ReviewPlanIdentityV1(pi)
	full := sha256.Sum256(idBytes)
	reviewID = PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public
	planDigest = hex.EncodeToString(full[:])
	return
}

// contentRecord builds a content-derived StableID plus its identity record from
// the exact canonical bytes the corresponding ID function hashes.
func contentRecord(kind string, canonical []byte) (StableID, IDRecord) {
	full := sha256.Sum256(canonical)
	id := PublicID(kind, full, PublicIDContentBytesV1)
	return id, IDRecord{Kind: kind, Public: id.Public, Full: full, Canonical: canonical}
}

// pathRecord builds a real p_ StableID plus its identity record for a changed
// path, using the given content digest so collision fixtures can inject one.
func pathRecord(digest func([]byte) Digest) (public string, record IDRecord) {
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	scope := ScopeDigest("sha1", oid20, oid20, ScopeCommit, Digest{})
	rec := RawPathRecord{
		Kind: "tracked", Status: "M", OldPath: "a.go", NewPath: "a.go",
		OldMode: 0o100644, NewMode: 0o100644, OldSide: oid20, NewSide: oid20,
		TextClass: "text", Additions: 1,
	}
	canonical := Frame(Field{Name: "scope", Value: scope[:]}, Field{Name: "record", Value: rec.Frame()})
	full := digest(canonical)
	public = PublicID(PathIDPrefixV1, full, PublicIDContentBytesV1).Public
	return public, IDRecord{Kind: PathIDPrefixV1, Public: public, Full: full, Canonical: canonical}
}

// makeArtifacts builds a minimal valid direct-mode plan with a real StableID for
// its single changed path and a complete identity registry.
func makeArtifacts(name string) PlanArtifacts {
	return makeArtifactsWith(name, sha256Digest)
}

func makeArtifactsWith(name string, digest func([]byte) Digest) PlanArtifacts {
	idBytes, reviewID, planDigest := planIdentity(name)
	public, record := pathRecord(digest)
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	plan := Plan{
		Schema:       PlanSchemaV1,
		ReviewID:     reviewID,
		PlanDigest:   planDigest,
		Mode:         ModeDirect,
		Scope:        Scope{Kind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20},
		Stats:        PlanStats{ChangedPaths: 1},
		ChangedPaths: []PlanPath{{PathID: public, NewPath: "a.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		NextCommands: []NextCommand{{Label: "unit", Command: "prowl-agent review unit " + reviewID}},
	}
	return PlanArtifacts{Plan: plan, PlanIdentityBytes: idBytes, IDRecords: []IDRecord{record}}
}

// makeStructuredArtifacts builds a production-valid structured plan that exercises
// every content-ID kind (p/h/u/c/l/t) with real StableIDs, real
// ReviewPlanIdentityV1 identity bytes, and a complete registry whose enumerated
// set (plan, cohorts, units, audits, citations, unit candidates, mandatory
// units) exactly matches the identity records.
func makeStructuredArtifacts(name string) PlanArtifacts {
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	scope := ScopeDigest("sha1", oid20, oid20, ScopeCommit, Digest{})

	rec := RawPathRecord{
		Kind: "tracked", Status: "M", OldPath: "svc.go", NewPath: "svc.go",
		OldMode: 0o100644, NewMode: 0o100644, OldSide: oid20, NewSide: oid20,
		TextClass: "text", Additions: 3, Deletions: 1,
	}
	pCanon := Frame(Field{Name: "scope", Value: scope[:]}, Field{Name: "record", Value: rec.Frame()})
	pathID, pRec := contentRecord(PathIDPrefixV1, pCanon)

	hunk := RawHunk{Ordinal: 0, OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 3, Payload: []byte("@@ body @@\n")}
	hCanon := Frame(
		Field{Name: "scope", Value: scope[:]},
		Field{Name: "path", Value: pathID.Full[:]},
		Field{Name: "ordinal", Value: u64(hunk.Ordinal)},
		Field{Name: "hunk", Value: hunk.Frame()},
	)
	hunkID, hRec := contentRecord(HunkIDPrefixV1, hCanon)

	uCanon := Frame(Field{Name: "kind", Value: []byte("primary")}, Field{Name: "hunks", Value: fullDigestList([]StableID{hunkID})})
	unitID, uRec := contentRecord(UnitIDPrefixV1, uCanon)

	cCanon := Frame(
		Field{Name: "scope", Value: scope[:]},
		Field{Name: "label", Value: []byte("core")},
		Field{Name: "units", Value: fullDigestList([]StableID{unitID})},
	)
	cohortID, cRec := contentRecord(CohortIDPrefixV1, cCanon)

	lCanon := Frame(
		Field{Name: "cohort", Value: cohortID.Full[:]},
		Field{Name: "ordinal", Value: u64(0)},
		Field{Name: "units", Value: fullDigestList([]StableID{unitID})},
	)
	layerID, lRec := contentRecord(LayerIDPrefixV1, lCanon)

	target := RawTarget{Kind: "symbol", PathID: pathID.Full, Side: "head", SymbolKind: "func", SymbolName: "Svc", Start: 1, End: 3}
	tCanon := Frame(
		Field{Name: "kind", Value: []byte(target.Kind)},
		Field{Name: "path", Value: target.PathID[:]},
		Field{Name: "side", Value: []byte(target.Side)},
		Field{Name: "symbol_kind", Value: []byte(target.SymbolKind)},
		Field{Name: "symbol_name", Value: []byte(target.SymbolName)},
		Field{Name: "start", Value: u64(target.Start)},
		Field{Name: "end", Value: u64(target.End)},
		Field{Name: "signature", Value: optDigest(target.SignatureDigest)},
	)
	targetID, tRec := contentRecord(TargetIDPrefixV1, tCanon)

	pi := PlanIdentity{
		PlannerVersion: name,
		ScopeDigest:    scope,
		IndexSchema:    "prowl.index.v1",
		IndexVersion:   "1",
		Paths:          []PlanPathEntry{{PathID: pathID, ReviewClass: "full", Coverage: "full", RoleIDs: []string{"implementation"}}},
		Hunks:          []PlanHunkEntry{{HunkID: hunkID, Reviewability: "reviewable"}},
		Units:          []PlanUnitEntry{{UnitID: unitID, Kind: "primary", HunkIDs: []StableID{hunkID}}},
		Cohorts:        []PlanCohortEntry{{CohortID: cohortID, LayerID: layerID, UnitIDs: []StableID{unitID}}},
		Audits: []PlanAuditEntry{
			{AuditID: AuditRemovedBehaviorV1, TargetIDs: []StableID{targetID}},
			{AuditID: AuditContractMigrationV1},
			{AuditID: AuditTestMatrixV1},
			{AuditID: AuditIntegrationGapV1},
		},
	}
	idBytes := ReviewPlanIdentityV1(pi)
	full := sha256.Sum256(idBytes)
	reviewID := PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public
	planDigest := hex.EncodeToString(full[:])

	plan := Plan{
		Schema:             PlanSchemaV1,
		ReviewID:           reviewID,
		PlanDigest:         planDigest,
		Mode:               ModeStructured,
		StructuredRequired: true,
		Scope:              Scope{Kind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20},
		Stats:              PlanStats{RawAdditions: 3, RawDeletions: 1, RawChurn: 4, ChangedPaths: 1},
		ChangedPaths:       []PlanPath{{PathID: pathID.Public, NewPath: "svc.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		Cohorts: []PlanCohort{{
			CohortID: cohortID.Public, Label: "core",
			Layers:  []PlanLayer{{LayerID: layerID.Public, Ordinal: 0, UnitIDs: []string{unitID.Public}}},
			UnitIDs: []string{unitID.Public},
		}},
		PrimaryUnits: []Unit{{
			Schema: UnitSchemaV1, ReviewID: reviewID, UnitID: unitID.Public,
			CohortID: cohortID.Public, LayerID: layerID.Public,
			ScopeKind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20,
			Hunks: []UnitHunk{{PathID: pathID.Public, OldPath: "svc.go", NewPath: "svc.go", Status: "M", Ordinal: 0, OldStart: 1, OldCount: 1, NewStart: 1, NewCount: 3, PatchBase64: "QGJvZHlA"}},
		}},
		RequiredAudits: []PlanAudit{
			{AuditID: AuditRemovedBehaviorV1, TargetIDs: []string{targetID.Public}},
			{AuditID: AuditContractMigrationV1},
			{AuditID: AuditTestMatrixV1},
			{AuditID: AuditIntegrationGapV1},
		},
		NextCommands: []NextCommand{{Label: "unit", Command: "prowl-agent review unit " + reviewID}},
	}
	citations := map[string]CitationProof{
		hunkID.Public: {ID: hunkID.Public, Side: SideHead, Path: "svc.go", ContentHash: hex.EncodeToString(make([]byte, 32)), Start: 1, End: 3},
	}
	return PlanArtifacts{
		Plan:              plan,
		PlanIdentityBytes: idBytes,
		IDRecords:         []IDRecord{pRec, hRec, uRec, cRec, lRec, tRec},
		Citations:         citations,
		UnitCandidates:    map[string][]contextpacket.Candidate{unitID.Public: nil},
		MandatoryUnits:    map[string][]byte{unitID.Public: []byte("{\"schema\":\"review.unit.v1\"}\n")},
	}
}

// makeWorkspaceArtifacts builds a workspace-scope plan with a fingerprint bound
// to its workspace head.
func makeWorkspaceArtifacts(name string) PlanArtifacts {
	a := makeArtifacts(name)
	head := make([]byte, 32)
	head[0] = 0x5a
	a.Plan.Scope.Kind = ScopeWorkspace
	a.Plan.Scope.Base = SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	a.Plan.Scope.Head = SideIdentity{Kind: SideWorkspaceSHA256, Value: head}
	a.WorkspaceFingerprint = hex.EncodeToString(head)
	return a
}

// collidingDigest maps every input to a digest whose first 16 bytes are zero
// (identical public prefix) but whose 17th byte carries the input's first byte
// (distinct full digest), forcing a same-kind prefix collision.
func collidingDigest(b []byte) Digest {
	var d Digest
	if len(b) > 0 {
		d[16] = b[0]
	}
	return d
}

// collidingArtifacts builds a plan whose single p_ record hashes (under the
// colliding digest) to the shared prefix but a canonical-specific full digest.
func collidingArtifacts(name string, canonical []byte) PlanArtifacts {
	idBytes, reviewID, planDigest := planIdentity(name)
	full := collidingDigest(canonical)
	public := PublicID(PathIDPrefixV1, full, PublicIDContentBytesV1).Public
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	plan := Plan{
		Schema:       PlanSchemaV1,
		ReviewID:     reviewID,
		PlanDigest:   planDigest,
		Mode:         ModeDirect,
		Scope:        Scope{Kind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20},
		Stats:        PlanStats{ChangedPaths: 1},
		ChangedPaths: []PlanPath{{PathID: public, NewPath: "a.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		NextCommands: []NextCommand{{Label: "unit", Command: "prowl-agent review unit " + reviewID}},
	}
	return PlanArtifacts{Plan: plan, PlanIdentityBytes: idBytes, IDRecords: []IDRecord{{Kind: PathIDPrefixV1, Public: public, Full: full, Canonical: canonical}}}
}

// collidingManifest builds a persisted manifest for a colliding plan, so a
// cross-plan collision can be planted directly into the domain.
func collidingManifest(name string, canonical []byte) planManifest {
	a := collidingArtifacts(name, canonical)
	return planManifest{
		Schema: planStoreSchemaV1, ReviewID: a.Plan.ReviewID, PlanDigest: a.Plan.PlanDigest,
		ScopeKind: a.Plan.Scope.Kind, CreatedAt: 1, Plan: a.Plan,
		PlanIdentityBytes: a.PlanIdentityBytes, IDRecords: a.IDRecords,
	}
}

// linkedWorktree builds a repository plus a detached linked worktree.
func linkedWorktree(t *testing.T) (main, linked string, runner GitRunner) {
	t.Helper()
	f := newGitFixture(t)
	f.commitFile(t, "seed.txt", "x\n")
	linked = filepath.Join(t.TempDir(), "linked")
	rawGit(t, f.root, "worktree", "add", "-q", "--detach", linked)
	return f.root, linked, execRunner(t)
}

// TestPlanStoreLivesOutsideWorktreeAndDetectsCollision proves review state lives
// under the shared Git common dir (outside a linked worktree, one domain) and
// that a same-kind public prefix mapping to a different full digest across the
// domain is a collision.
func TestPlanStoreLivesOutsideWorktreeAndDetectsCollision(t *testing.T) {
	main, linked, runner := linkedWorktree(t)
	ctx := context.Background()

	store, err := OpenPlanStore(ctx, runner, linked)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if hasPathPrefix(store.Root(), linked) {
		t.Fatalf("state leaked into worktree: %s", store.Root())
	}
	mainStore, err := OpenPlanStore(ctx, runner, main)
	if err != nil {
		t.Fatal(err)
	}
	defer mainStore.Close()
	if mainStore.Root() != store.Root() {
		t.Fatalf("linked worktree domain differs: %s vs %s", mainStore.Root(), store.Root())
	}

	store.digest = collidingDigest
	if _, err := store.Save(ctx, collidingArtifacts("one", []byte("A")), nil); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A second worktree instance sees the same domain and must reject the collision.
	mainStore.digest = collidingDigest
	if _, err := mainStore.Save(ctx, collidingArtifacts("two", []byte("B")), nil); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("cross-worktree collision err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreSaveLoadRoundTrip proves artifacts survive an atomic save/load.
func TestPlanStoreSaveLoadRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("round")
	a.PublishedIndexSignature = "sig-123"
	pathID := a.Plan.ChangedPaths[0].PathID
	a.Citations = map[string]CitationProof{pathID: {ID: pathID, Side: SideHead, Path: "a.go", ContentHash: hex.EncodeToString(make([]byte, 32)), Start: 1, End: 4}}
	if _, err := store.Save(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, a.Plan.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan.ReviewID != a.Plan.ReviewID || got.PublishedIndexSignature != "sig-123" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if got.Citations[pathID].Path != "a.go" {
		t.Fatalf("citation proof lost: %+v", got.Citations)
	}
	if len(got.IDRecords) != 1 || got.IDRecords[0].Public != pathID {
		t.Fatalf("id records lost: %+v", got.IDRecords)
	}
}

// TestPlanStoreSaveLoadStructuredAllKinds proves a production-valid structured
// plan exercising every content-ID kind, plus its unit candidates and mandatory
// units, survives an atomic save/load with the full registry accepted.
func TestPlanStoreSaveLoadStructuredAllKinds(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeStructuredArtifacts("all-kinds")
	lease := makeLease(t, store, "snap-marker")
	res, err := store.Save(ctx, a, lease)
	if err != nil {
		t.Fatalf("save structured: %v", err)
	}
	got, err := store.Load(ctx, res.ReviewID)
	if err != nil {
		t.Fatalf("load structured: %v", err)
	}
	kinds := map[string]bool{}
	for _, r := range got.IDRecords {
		kinds[r.Kind] = true
	}
	for _, k := range []string{PathIDPrefixV1, HunkIDPrefixV1, UnitIDPrefixV1, CohortIDPrefixV1, LayerIDPrefixV1, TargetIDPrefixV1} {
		if !kinds[k] {
			t.Fatalf("id kind %s missing after round trip: %+v", k, got.IDRecords)
		}
	}
	uid := a.Plan.PrimaryUnits[0].UnitID
	if _, ok := got.UnitCandidates[uid]; !ok {
		t.Fatalf("unit candidates lost: %+v", got.UnitCandidates)
	}
	if string(got.MandatoryUnits[uid]) != string(a.MandatoryUnits[uid]) {
		t.Fatalf("mandatory bytes lost: %q", got.MandatoryUnits[uid])
	}
	if _, err := os.Stat(filepath.Join(store.Root(), res.ReviewID, snapshotName, "snap-marker")); err != nil {
		t.Fatalf("leased snapshot not persisted: %v", err)
	}
}

// TestPlanStoreRejectsSuppliedDigestMismatch proves the store recomputes the
// plan digest from its canonical bytes and never trusts a supplied string.
func TestPlanStoreRejectsSuppliedDigestMismatch(t *testing.T) {
	store := newStore(t)
	a := makeArtifacts("mismatch")
	other := sha256.Sum256([]byte("something else"))
	a.Plan.PlanDigest = hex.EncodeToString(other[:])
	if _, err := store.Save(context.Background(), a, nil); !errors.Is(err, ErrPlanIdentityMismatch) {
		t.Fatalf("supplied-digest mismatch err=%v, want ErrPlanIdentityMismatch", err)
	}
}

// TestPlanStoreRegistryRejectsShapeViolations proves omission, foreign, mapping,
// and duplicate identity-record violations are rejected.
func TestPlanStoreRegistryRejectsShapeViolations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		mutey func(a *PlanArtifacts)
		want  error
	}{
		{"omission: plan id without record", func(a *PlanArtifacts) { a.IDRecords = nil }, ErrIDRegistry},
		{"foreign: record not in plan", func(a *PlanArtifacts) {
			_, extra := pathRecord(func(b []byte) Digest { return sha256.Sum256(append([]byte("x"), b...)) })
			a.IDRecords = append(a.IDRecords, extra)
		}, ErrIDRegistry},
		{"mapping: full does not match canonical", func(a *PlanArtifacts) {
			a.IDRecords[0].Full = sha256.Sum256([]byte("wrong"))
		}, ErrIDRegistry},
		{"mapping: public does not match full", func(a *PlanArtifacts) {
			a.IDRecords[0].Public = PathIDPrefixV1 + hex.EncodeToString(make([]byte, 16))
		}, ErrIDRegistry},
		{"unknown kind", func(a *PlanArtifacts) {
			a.IDRecords[0].Kind = "z_"
		}, ErrIDRegistry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			a := makeArtifacts("shape")
			tc.mutey(&a)
			if _, err := store.Save(ctx, a, nil); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestPlanStoreRegistryEnumeratesCandidatesAndMandatory proves a unit-candidate
// or mandatory-unit key that is a content ID with no identity record is rejected.
func TestPlanStoreRegistryEnumeratesCandidatesAndMandatory(t *testing.T) {
	ctx := context.Background()
	orphan := UnitIDPrefixV1 + hex.EncodeToString(make([]byte, 16))

	t.Run("unit candidate key without record", func(t *testing.T) {
		store := newStore(t)
		a := makeArtifacts("cand")
		a.UnitCandidates = map[string][]contextpacket.Candidate{orphan: nil}
		if _, err := store.Save(ctx, a, nil); !errors.Is(err, ErrIDRegistry) {
			t.Fatalf("err=%v, want ErrIDRegistry", err)
		}
	})
	t.Run("mandatory unit key without record", func(t *testing.T) {
		store := newStore(t)
		a := makeArtifacts("mand")
		a.MandatoryUnits = map[string][]byte{orphan: []byte("{}")}
		if _, err := store.Save(ctx, a, nil); !errors.Is(err, ErrIDRegistry) {
			t.Fatalf("err=%v, want ErrIDRegistry", err)
		}
	})
}

// TestPlanStoreForcedCollisionDuringConstruction injects a digest that maps
// distinct canonical inputs to the same public prefix with different fulls, and
// proves Save rejects the second plan.
func TestPlanStoreForcedCollisionDuringConstruction(t *testing.T) {
	store := newStore(t)
	store.digest = collidingDigest
	ctx := context.Background()
	if _, err := store.Save(ctx, collidingArtifacts("c-one", []byte("A")), nil); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := store.Save(ctx, collidingArtifacts("c-two", []byte("B")), nil); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("colliding construction err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreForcedCollisionDuringLoad writes a manifest whose two records
// collide under the loader's injected digest and proves Load rejects it.
func TestPlanStoreForcedCollisionDuringLoad(t *testing.T) {
	store := newStore(t)
	store.digest = collidingDigest
	ctx := context.Background()

	idBytes, reviewID, planDigest := planIdentity("load-collide")
	fullA := collidingDigest([]byte("A"))
	fullB := collidingDigest([]byte("B"))
	public := PublicID(PathIDPrefixV1, fullA, PublicIDContentBytesV1).Public
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	plan := Plan{
		Schema: PlanSchemaV1, ReviewID: reviewID, PlanDigest: planDigest, Mode: ModeDirect,
		Scope:        Scope{Kind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20},
		Stats:        PlanStats{ChangedPaths: 1},
		ChangedPaths: []PlanPath{{PathID: public, NewPath: "a.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		NextCommands: []NextCommand{{Label: "unit", Command: "prowl-agent review unit " + reviewID}},
	}
	m := planManifest{
		Schema: planStoreSchemaV1, ReviewID: reviewID, PlanDigest: planDigest, ScopeKind: ScopeCommit,
		CreatedAt: 1, Plan: plan, PlanIdentityBytes: idBytes,
		IDRecords: []IDRecord{
			{Kind: PathIDPrefixV1, Public: public, Full: fullA, Canonical: []byte("A")},
			{Kind: PathIDPrefixV1, Public: public, Full: fullB, Canonical: []byte("B")},
		},
	}
	writeManifestDirect(t, store, m)
	if _, err := store.Load(ctx, reviewID); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("colliding load err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreForcedCollisionAcrossDomainOnLoad plants two valid manifests that
// collide across plans and proves loading either detects the cross-domain
// collision (not merely the manifest's own records).
func TestPlanStoreForcedCollisionAcrossDomainOnLoad(t *testing.T) {
	store := newStore(t)
	store.digest = collidingDigest
	ctx := context.Background()

	writeManifestDirect(t, store, collidingManifest("dom-a", []byte("A")))
	writeManifestDirect(t, store, collidingManifest("dom-b", []byte("B")))

	_, reviewA, _ := planIdentity("dom-a")
	if _, err := store.Load(ctx, reviewA); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("cross-domain load collision err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreAtomicTempRename proves atomic publication with the leased
// snapshot moved in, the lease consumed, and no temp directory left behind.
func TestPlanStoreAtomicTempRename(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("atomic")

	lease := makeLease(t, store, "marker")
	if _, err := store.Save(ctx, a, lease); err != nil {
		t.Fatal(err)
	}
	reviewDir := filepath.Join(store.Root(), a.Plan.ReviewID)
	if _, err := os.Stat(filepath.Join(reviewDir, manifestName)); err != nil {
		t.Fatalf("manifest not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, snapshotName, "marker")); err != nil {
		t.Fatalf("snapshot not moved into review dir: %v", err)
	}
	if _, err := os.Stat(lease.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease not consumed: %v", err)
	}
	assertNoTempLeftover(t, store)
}

// TestPlanStoreRollbackRestoresSnapshot proves a failed publication restores the
// caller's lease and leaves no temp directory.
func TestPlanStoreRollbackRestoresSnapshot(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("rollback")

	// Pre-create a non-empty review directory so the final rename fails.
	reviewDir := filepath.Join(store.Root(), a.Plan.ReviewID)
	if err := os.MkdirAll(reviewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reviewDir, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease := makeLease(t, store, "marker")
	if _, err := store.Save(ctx, a, lease); !errors.Is(err, ErrSnapshotOwnership) {
		t.Fatalf("blocked publication err=%v, want ErrSnapshotOwnership", err)
	}
	if _, err := os.Stat(filepath.Join(lease.Dir, "marker")); err != nil {
		t.Fatalf("caller lease not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial manifest published: %v", err)
	}
	assertNoTempLeftover(t, store)
}

// TestPlanStoreLeaseWrongParentRejected proves Save refuses a lease that is not
// under the store's snapshots root, leaving it caller-owned.
func TestPlanStoreLeaseWrongParentRejected(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "marker"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease := &SnapshotLease{Dir: outside, store: store}
	if _, err := store.Save(ctx, makeArtifacts("wrong-parent"), lease); !errors.Is(err, ErrSnapshotLease) {
		t.Fatalf("wrong-parent lease err=%v, want ErrSnapshotLease", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); err != nil {
		t.Fatalf("wrong-parent lease was consumed: %v", err)
	}
}

// TestPlanStoreLeaseSymlinkRejected proves Save refuses a lease reached through a
// symlink, without following it.
func TestPlanStoreLeaseSymlinkRejected(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "victim"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(store.snapshotsDir, "snap-symlink")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lease := &SnapshotLease{Dir: link, store: store}
	if _, err := store.Save(ctx, makeArtifacts("symlink"), lease); !errors.Is(err, ErrSnapshotLease) {
		t.Fatalf("symlink lease err=%v, want ErrSnapshotLease", err)
	}
	if _, err := os.Stat(filepath.Join(target, "victim")); err != nil {
		t.Fatalf("symlink target was consumed: %v", err)
	}
}

// TestPlanStoreLeaseCrossDeviceRejected proves Save refuses a lease on a
// different device (so the move would not be atomic), leaving it caller-owned.
func TestPlanStoreLeaseCrossDeviceRejected(t *testing.T) {
	store := newStore(t)
	store.sameDevice = func(_, _ string) (bool, error) { return false, nil }
	ctx := context.Background()
	a := makeArtifacts("xdev")
	lease := makeLease(t, store, "marker")
	if _, err := store.Save(ctx, a, lease); !errors.Is(err, ErrSnapshotLease) {
		t.Fatalf("cross-device lease err=%v, want ErrSnapshotLease", err)
	}
	if _, err := os.Stat(filepath.Join(lease.Dir, "marker")); err != nil {
		t.Fatalf("cross-device lease not preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), a.Plan.ReviewID, manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest published despite cross-device rejection")
	}
}

// TestPlanStoreSaveReturnsPruneWarning proves a post-publication retention error
// is surfaced as a warning, never a Save failure, and the plan is persisted.
func TestPlanStoreSaveReturnsPruneWarning(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	boom := errors.New("prune boom")
	store.pruneOverride = func(context.Context, time.Time) error { return boom }
	res, err := store.Save(ctx, makeArtifacts("warn"), nil)
	if err != nil {
		t.Fatalf("save failed on a prune warning: %v", err)
	}
	if !errors.Is(res.PruneWarning, boom) {
		t.Fatalf("PruneWarning=%v, want the injected error", res.PruneWarning)
	}
	if _, err := store.Load(ctx, res.ReviewID); err != nil {
		t.Fatalf("plan not persisted despite prune warning: %v", err)
	}
}

// TestPlanStoreLockExclusion proves Save fails when the store lock is held.
func TestPlanStoreLockExclusion(t *testing.T) {
	f := newGitFixture(t)
	f.commitFile(t, "seed.txt", "x\n")
	runner := execRunner(t)
	ctx := context.Background()

	store, err := OpenPlanStore(ctx, runner, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.lockTimeout = 150 * time.Millisecond
	release, err := store.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	other, err := OpenPlanStore(ctx, runner, f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.lockTimeout = 150 * time.Millisecond
	if _, err := other.Save(ctx, makeArtifacts("locked"), nil); !errors.Is(err, ErrPlanLocked) {
		t.Fatalf("save under held lock err=%v, want ErrPlanLocked", err)
	}
}

// TestPlanStoreWorkspaceFingerprintInvariants proves the fingerprint is required
// and bound for workspace scope and forbidden for committed scope.
func TestPlanStoreWorkspaceFingerprintInvariants(t *testing.T) {
	ctx := context.Background()

	t.Run("committed with fingerprint rejected", func(t *testing.T) {
		store := newStore(t)
		a := makeArtifacts("commit-fp")
		a.WorkspaceFingerprint = "deadbeef"
		if _, err := store.Save(ctx, a, nil); !errors.Is(err, ErrWorkspaceFingerprint) {
			t.Fatalf("err=%v, want ErrWorkspaceFingerprint", err)
		}
	})
	t.Run("workspace without fingerprint rejected", func(t *testing.T) {
		store := newStore(t)
		a := makeWorkspaceArtifacts("ws-empty")
		a.WorkspaceFingerprint = ""
		if _, err := store.Save(ctx, a, nil); !errors.Is(err, ErrWorkspaceFingerprint) {
			t.Fatalf("err=%v, want ErrWorkspaceFingerprint", err)
		}
	})
	t.Run("workspace fingerprint not bound to head rejected", func(t *testing.T) {
		store := newStore(t)
		a := makeWorkspaceArtifacts("ws-bad")
		a.WorkspaceFingerprint = hex.EncodeToString(make([]byte, 32))
		if _, err := store.Save(ctx, a, nil); !errors.Is(err, ErrWorkspaceFingerprint) {
			t.Fatalf("err=%v, want ErrWorkspaceFingerprint", err)
		}
	})
}

// TestPlanStoreStaleWorkspaceFingerprint proves a workspace plan is stale when
// the workspace scope changes, while a committed plan is never stale.
func TestPlanStoreStaleWorkspaceFingerprint(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	ws := makeWorkspaceArtifacts("workspace")
	if _, err := store.Save(ctx, ws, nil); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, ws.WorkspaceFingerprint); err != nil || stale {
		t.Fatalf("fresh workspace plan stale=%v err=%v", stale, err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, "different"); err != nil || !stale {
		t.Fatalf("changed workspace plan stale=%v err=%v; want stale", stale, err)
	}

	committed := makeArtifacts("committed")
	if _, err := store.Save(ctx, committed, nil); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Stale(ctx, committed.Plan.ReviewID, "anything"); err != nil || stale {
		t.Fatalf("committed plan reported stale=%v err=%v", stale, err)
	}
}

// TestPlanStoreImmutableReuse proves saving an identical plan reuses the existing
// manifest and snapshot without rewriting, deleting the redundant incoming lease.
func TestPlanStoreImmutableReuse(t *testing.T) {
	store := newStore(t)
	base := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return base }
	ctx := context.Background()

	a := makeArtifacts("reuse")
	snap := makeLease(t, store, "keep")
	res, err := store.Save(ctx, a, snap)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reused {
		t.Fatalf("first save reported reuse")
	}
	firstCreated := mustManifest(t, store, a.Plan.ReviewID).CreatedAt

	store.now = func() time.Time { return base.Add(72 * time.Hour) }
	extra := makeLease(t, store, "redundant")
	res, err = store.Save(ctx, a, extra)
	if err != nil {
		t.Fatalf("reuse save: %v", err)
	}
	if !res.Reused {
		t.Fatalf("identical save not reported as reuse")
	}
	if got := mustManifest(t, store, a.Plan.ReviewID).CreatedAt; got != firstCreated {
		t.Fatalf("reuse rewrote manifest: created %d -> %d", firstCreated, got)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), a.Plan.ReviewID, snapshotName, "keep")); err != nil {
		t.Fatalf("original snapshot not preserved on reuse: %v", err)
	}
	if _, err := os.Stat(extra.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redundant incoming lease not deleted on reuse: %v", err)
	}
}

// TestPlanStoreRetentionSevenDayAndLockRace proves creation prunes plans older
// than seven days but never one a reader holds through the striped lock, and that
// releasing the lock lets a later prune delete it. The old and locked plans are
// chosen on distinct stripes so the lock never blocks the unrelated deletion.
func TestPlanStoreRetentionSevenDayAndLockRace(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	names := distinctStripeNames(store, 3)

	store.now = func() time.Time { return base }
	old := makeArtifacts(names[0])
	if _, err := store.Save(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	locked := makeArtifacts(names[1])
	if _, err := store.Save(ctx, locked, nil); err != nil {
		t.Fatal(err)
	}

	release, err := store.LockReview(ctx, locked.Plan.ReviewID)
	if err != nil {
		t.Fatal(err)
	}

	// A creation eight days later prunes the unlocked old plan but not the locked.
	store.now = func() time.Time { return base.Add(8 * 24 * time.Hour) }
	if _, err := store.Save(ctx, makeArtifacts(names[2]), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, old.Plan.ReviewID); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("stale plan not pruned: err=%v", err)
	}
	if _, err := store.Load(ctx, locked.Plan.ReviewID); err != nil {
		t.Fatalf("locked plan was pruned: %v", err)
	}

	// After release, an explicit prune deletes the now-unlocked old plan.
	release()
	if err := store.Prune(ctx, base.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, locked.Plan.ReviewID); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("released plan not pruned: err=%v", err)
	}
}

// TestPlanStoreRetentionTwentyPlans proves creation keeps at most twenty plans.
func TestPlanStoreRetentionTwentyPlans(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	for i := range 25 {
		store.now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		if _, err := store.Save(ctx, makeArtifacts(fmt.Sprintf("p-%02d", i)), nil); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if n := countReviews(t, store); n != retentionCountV1 {
		t.Fatalf("retained %d plans, want %d", n, retentionCountV1)
	}
}

// TestPlanStoreLockStripesBounded proves the per-review lock files are drawn from
// a fixed, bounded stripe set even across far more review ids than stripes.
func TestPlanStoreLockStripesBounded(t *testing.T) {
	store := newStore(t)
	paths := map[string]bool{}
	for i := range 1000 {
		_, reviewID, _ := planIdentity(fmt.Sprintf("stripe-%d", i))
		paths[store.reviewLockPath(reviewID)] = true
	}
	if len(paths) > lockStripeCountV1 {
		t.Fatalf("distinct lock files=%d exceed stripe cap %d", len(paths), lockStripeCountV1)
	}
	if len(paths) < lockStripeCountV1/2 {
		t.Fatalf("striping collapsed: only %d distinct stripes for 1000 ids", len(paths))
	}
}

// TestPlanStoreLoadRejectsMalformedReviewID proves review ids are validated
// before any path resolution or open.
func TestPlanStoreLoadRejectsMalformedReviewID(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	for _, id := range []string{"../escape", "rvw_short", "rvw_" + "zz", "notareview"} {
		if _, err := store.Load(ctx, id); err == nil {
			t.Fatalf("Load(%q) accepted a malformed review id", id)
		}
		if _, err := store.LockReview(ctx, id); err == nil {
			t.Fatalf("LockReview(%q) accepted a malformed review id", id)
		}
	}
}

// TestPlanStoreLoadRejectsHugeManifest proves the bounded read fails closed.
func TestPlanStoreLoadRejectsHugeManifest(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("huge")
	if _, err := store.Save(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	store.maxManifest = 40 // smaller than any real manifest
	if _, err := store.Load(ctx, a.Plan.ReviewID); !errors.Is(err, ErrManifestCorrupt) {
		t.Fatalf("huge manifest err=%v, want ErrManifestCorrupt", err)
	}
}

// TestPlanStoreLoadHonorsCancellation proves a canceled context surfaces.
func TestPlanStoreLoadHonorsCancellation(t *testing.T) {
	store := newStore(t)
	a := makeArtifacts("cancel")
	if _, err := store.Save(context.Background(), a, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx, a.Plan.ReviewID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load err=%v, want context.Canceled", err)
	}
}

// TestPlanStoreDecodeManifestMidDecodeCancel proves the context-aware streaming
// decoder surfaces a cancellation that fires mid-decode.
func TestPlanStoreDecodeManifestMidDecodeCancel(t *testing.T) {
	a := makeArtifacts("mid-decode")
	m := planManifest{
		Schema: planStoreSchemaV1, ReviewID: a.Plan.ReviewID, PlanDigest: a.Plan.PlanDigest,
		ScopeKind: a.Plan.Scope.Kind, CreatedAt: 1, Plan: a.Plan,
		PlanIdentityBytes: a.PlanIdentityBytes, IDRecords: a.IDRecords,
	}
	payload, err := marshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < 32 {
		t.Fatalf("manifest too small to exercise mid-decode cancel: %d bytes", len(payload))
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelMidRead{data: payload, chunk: 8, cancel: cancel}
	if _, err := decodeManifest(ctx, r, int64(len(payload))*4); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-decode cancel err=%v, want context.Canceled", err)
	}
}

// TestPlanStoreLoadThroughPinnedRootSurvivesRootSwap proves manifest reads go
// through a pinned reviews-root descriptor, so swapping the reviews path cannot
// redirect a read to an attacker-controlled directory.
func TestPlanStoreLoadThroughPinnedRootSurvivesRootSwap(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("pinned")
	a.PublishedIndexSignature = "original-sig"
	if _, err := store.Save(ctx, a, nil); err != nil {
		t.Fatal(err)
	}

	// Move the real reviews directory aside and drop an empty decoy in its place.
	// A path-resolving store would now miss the plan; the pinned descriptor still
	// reads the original inode.
	aside := store.Root() + ".orig"
	if err := os.Rename(store.Root(), aside); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Root(), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(ctx, a.Plan.ReviewID)
	if err != nil {
		t.Fatalf("load through pinned root failed after swap: %v", err)
	}
	if got.PublishedIndexSignature != "original-sig" {
		t.Fatalf("pinned root read the swapped path: sig=%q", got.PublishedIndexSignature)
	}
}

// TestPlanStoreLoadDetectsTamper proves the integrity digest catches a mutated
// manifest independent of the injectable ID digest.
func TestPlanStoreLoadDetectsTamper(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("tamper")
	if _, err := store.Save(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), a.Plan.ReviewID, manifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m planManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.PublishedIndexSignature = "tampered-after-the-fact"
	tampered, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, a.Plan.ReviewID); !errors.Is(err, ErrManifestCorrupt) {
		t.Fatalf("tampered load err=%v, want ErrManifestCorrupt", err)
	}
}

// TestPlanStoreLoadDetectsScopeHeaderMismatch proves the manifest header must
// agree with the embedded plan.
func TestPlanStoreLoadDetectsScopeHeaderMismatch(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("scope-header")
	m := planManifest{
		Schema: planStoreSchemaV1, ReviewID: a.Plan.ReviewID, PlanDigest: a.Plan.PlanDigest,
		ScopeKind: ScopeWorkspace, // disagrees with the embedded commit-scope plan
		CreatedAt: 1, Plan: a.Plan, PlanIdentityBytes: a.PlanIdentityBytes, IDRecords: a.IDRecords,
	}
	writeManifestDirect(t, store, m)
	if _, err := store.Load(ctx, a.Plan.ReviewID); !errors.Is(err, ErrManifestCorrupt) {
		t.Fatalf("scope-header mismatch err=%v, want ErrManifestCorrupt", err)
	}
}

// ---- helpers --------------------------------------------------------------

func newStore(t *testing.T) *PlanStore {
	t.Helper()
	f := newGitFixture(t)
	f.commitFile(t, "seed.txt", "x\n")
	store, err := OpenPlanStore(context.Background(), execRunner(t), f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// makeLease mints a store-owned snapshot lease and writes a marker file into it.
func makeLease(t *testing.T, store *PlanStore, marker string) *SnapshotLease {
	t.Helper()
	lease, err := store.NewSnapshotLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Dir, marker), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return lease
}

// distinctStripeNames returns n plan names whose review ids map to distinct lock
// stripes, so a per-review lock never blocks an unrelated deletion in a test.
func distinctStripeNames(store *PlanStore, n int) []string {
	seen := map[string]bool{}
	var names []string
	for i := 0; len(names) < n; i++ {
		name := fmt.Sprintf("rl-%d", i)
		_, reviewID, _ := planIdentity(name)
		p := store.reviewLockPath(reviewID)
		if seen[p] {
			continue
		}
		seen[p] = true
		names = append(names, name)
	}
	return names
}

func writeManifestDirect(t *testing.T, store *PlanStore, m planManifest) {
	t.Helper()
	payload, err := marshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Root(), m.ReviewID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustManifest(t *testing.T, store *PlanStore, reviewID string) planManifest {
	t.Helper()
	m, err := store.loadManifest(context.Background(), reviewID)
	if err != nil {
		t.Fatalf("loadManifest %s: %v", reviewID, err)
	}
	return m
}

func countReviews(t *testing.T, store *PlanStore) int {
	t.Helper()
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && validReviewID(e.Name()) {
			n++
		}
	}
	return n
}

func assertNoTempLeftover(t *testing.T, store *PlanStore) {
	t.Helper()
	entries, _ := os.ReadDir(store.Root())
	for _, e := range entries {
		if len(e.Name()) >= 5 && e.Name()[:5] == ".tmp-" {
			t.Fatalf("temp directory leaked: %s", e.Name())
		}
	}
}

func hasPathPrefix(path, dir string) bool {
	return path == dir || (len(path) > len(dir) && path[:len(dir)] == dir && path[len(dir)] == filepath.Separator)
}

// cancelMidRead delivers chunk bytes per read and cancels the context after the
// first delivery, so a streaming decoder observes cancellation mid-decode.
type cancelMidRead struct {
	data   []byte
	off    int
	chunk  int
	cancel context.CancelFunc
}

func (c *cancelMidRead) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := c.chunk
	if n > len(p) {
		n = len(p)
	}
	if c.off+n > len(c.data) {
		n = len(c.data) - c.off
	}
	copy(p, c.data[c.off:c.off+n])
	c.off += n
	c.cancel()
	return n, nil
}
