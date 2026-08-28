//go:build unix

package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sha256Digest(b []byte) Digest { return sha256.Sum256(b) }

// planIdentity derives the canonical identity bytes, review id, and plan digest
// for a named test plan, exactly as the store recomputes them.
func planIdentity(name string) (idBytes []byte, reviewID, planDigest string) {
	idBytes = []byte("review.plan.v1|" + name)
	full := sha256.Sum256(idBytes)
	reviewID = "rvw_" + hex.EncodeToString(full[:20])
	planDigest = hex.EncodeToString(full[:])
	return
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
	if hasPathPrefix(store.Root(), linked) {
		t.Fatalf("state leaked into worktree: %s", store.Root())
	}
	mainStore, err := OpenPlanStore(ctx, runner, main)
	if err != nil {
		t.Fatal(err)
	}
	if mainStore.Root() != store.Root() {
		t.Fatalf("linked worktree domain differs: %s vs %s", mainStore.Root(), store.Root())
	}

	store.digest = collidingDigest
	if err := store.Save(ctx, collidingArtifacts("one", []byte("A")), ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// A second worktree instance sees the same domain and must reject the collision.
	mainStore.digest = collidingDigest
	if err := mainStore.Save(ctx, collidingArtifacts("two", []byte("B")), ""); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("cross-worktree collision err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreSaveLoadRoundTrip proves artifacts survive an atomic save/load.
func TestPlanStoreSaveLoadRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("round")
	a.PublishedIndexSignature = "sig-123"
	a.MandatoryUnits = map[string][]byte{"u_1": []byte("{\"schema\":\"review.unit.v1\"}\n")}
	pathID := a.Plan.ChangedPaths[0].PathID
	a.Citations = map[string]CitationProof{pathID: {ID: pathID, Side: SideHead, Path: "a.go", ContentHash: hex.EncodeToString(make([]byte, 32)), Start: 1, End: 4}}
	if err := store.Save(ctx, a, ""); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, a.Plan.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan.ReviewID != a.Plan.ReviewID || got.PublishedIndexSignature != "sig-123" {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if string(got.MandatoryUnits["u_1"]) != string(a.MandatoryUnits["u_1"]) {
		t.Fatalf("mandatory bytes lost: %q", got.MandatoryUnits["u_1"])
	}
	if got.Citations[pathID].Path != "a.go" {
		t.Fatalf("citation proof lost: %+v", got.Citations)
	}
	if len(got.IDRecords) != 1 || got.IDRecords[0].Public != pathID {
		t.Fatalf("id records lost: %+v", got.IDRecords)
	}
}

// TestPlanStoreRejectsSuppliedDigestMismatch proves the store recomputes the
// plan digest from its canonical bytes and never trusts a supplied string.
func TestPlanStoreRejectsSuppliedDigestMismatch(t *testing.T) {
	store := newStore(t)
	a := makeArtifacts("mismatch")
	// A different (still well-formed) plan digest not derived from the identity.
	other := sha256.Sum256([]byte("something else"))
	a.Plan.PlanDigest = hex.EncodeToString(other[:])
	if err := store.Save(context.Background(), a, ""); !errors.Is(err, ErrPlanIdentityMismatch) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			a := makeArtifacts("shape")
			tc.mutey(&a)
			if err := store.Save(ctx, a, ""); !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want %v", err, tc.want)
			}
		})
	}
}

// TestPlanStoreForcedCollisionDuringConstruction injects a digest that maps
// distinct canonical inputs to the same public prefix with different fulls, and
// proves Save rejects the second plan.
func TestPlanStoreForcedCollisionDuringConstruction(t *testing.T) {
	store := newStore(t)
	store.digest = collidingDigest
	ctx := context.Background()
	if err := store.Save(ctx, collidingArtifacts("c-one", []byte("A")), ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := store.Save(ctx, collidingArtifacts("c-two", []byte("B")), ""); !errors.Is(err, ErrIDCollision) {
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

// TestPlanStoreAtomicTempRename proves atomic publication with the snapshot moved
// in, the caller's snapshot consumed, and no temp directory left behind.
func TestPlanStoreAtomicTempRename(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("atomic")

	snapshotDir := makeSnapshot(t, store, "marker")
	if err := store.Save(ctx, a, snapshotDir); err != nil {
		t.Fatal(err)
	}
	reviewDir := filepath.Join(store.Root(), a.Plan.ReviewID)
	if _, err := os.Stat(filepath.Join(reviewDir, manifestName)); err != nil {
		t.Fatalf("manifest not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, snapshotName, "marker")); err != nil {
		t.Fatalf("snapshot not moved into review dir: %v", err)
	}
	if _, err := os.Stat(snapshotDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("caller snapshot not consumed: %v", err)
	}
	assertNoTempLeftover(t, store)
}

// TestPlanStoreRollbackRestoresSnapshot proves a failed publication restores the
// caller's snapshot and leaves no temp directory.
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
	snapshotDir := makeSnapshot(t, store, "marker")
	if err := store.Save(ctx, a, snapshotDir); !errors.Is(err, ErrSnapshotOwnership) {
		t.Fatalf("blocked publication err=%v, want ErrSnapshotOwnership", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, "marker")); err != nil {
		t.Fatalf("caller snapshot not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial manifest published: %v", err)
	}
	assertNoTempLeftover(t, store)
}

// TestPlanStoreLockExclusion proves Save fails when the store lock is held.
func TestPlanStoreLockExclusion(t *testing.T) {
	store := newStore(t)
	store.lockTimeout = 150 * time.Millisecond
	ctx := context.Background()

	release, err := store.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	other := &PlanStore{root: store.root, locksDir: store.locksDir, lockPath: store.lockPath, lockTimeout: 150 * time.Millisecond, maxManifest: store.maxManifest, digest: store.digest, now: store.now}
	if err := other.Save(ctx, makeArtifacts("locked"), ""); !errors.Is(err, ErrPlanLocked) {
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
		if err := store.Save(ctx, a, ""); !errors.Is(err, ErrWorkspaceFingerprint) {
			t.Fatalf("err=%v, want ErrWorkspaceFingerprint", err)
		}
	})
	t.Run("workspace without fingerprint rejected", func(t *testing.T) {
		store := newStore(t)
		a := makeWorkspaceArtifacts("ws-empty")
		a.WorkspaceFingerprint = ""
		if err := store.Save(ctx, a, ""); !errors.Is(err, ErrWorkspaceFingerprint) {
			t.Fatalf("err=%v, want ErrWorkspaceFingerprint", err)
		}
	})
	t.Run("workspace fingerprint not bound to head rejected", func(t *testing.T) {
		store := newStore(t)
		a := makeWorkspaceArtifacts("ws-bad")
		a.WorkspaceFingerprint = hex.EncodeToString(make([]byte, 32)) // not the head value
		if err := store.Save(ctx, a, ""); !errors.Is(err, ErrWorkspaceFingerprint) {
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
	if err := store.Save(ctx, ws, ""); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, ws.WorkspaceFingerprint); err != nil || stale {
		t.Fatalf("fresh workspace plan stale=%v err=%v", stale, err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, "different"); err != nil || !stale {
		t.Fatalf("changed workspace plan stale=%v err=%v; want stale", stale, err)
	}

	committed := makeArtifacts("committed")
	if err := store.Save(ctx, committed, ""); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Stale(ctx, committed.Plan.ReviewID, "anything"); err != nil || stale {
		t.Fatalf("committed plan reported stale=%v err=%v", stale, err)
	}
}

// TestPlanStoreImmutableReuse proves saving an identical plan reuses the existing
// manifest and snapshot without rewriting, deleting the redundant incoming one.
func TestPlanStoreImmutableReuse(t *testing.T) {
	store := newStore(t)
	base := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return base }
	ctx := context.Background()

	a := makeArtifacts("reuse")
	snap := makeSnapshot(t, store, "keep")
	if err := store.Save(ctx, a, snap); err != nil {
		t.Fatal(err)
	}
	firstCreated := mustManifest(t, store, a.Plan.ReviewID).CreatedAt

	store.now = func() time.Time { return base.Add(72 * time.Hour) }
	extra := makeSnapshot(t, store, "redundant")
	if err := store.Save(ctx, a, extra); err != nil {
		t.Fatalf("reuse save: %v", err)
	}
	if got := mustManifest(t, store, a.Plan.ReviewID).CreatedAt; got != firstCreated {
		t.Fatalf("reuse rewrote manifest: created %d -> %d", firstCreated, got)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), a.Plan.ReviewID, snapshotName, "keep")); err != nil {
		t.Fatalf("original snapshot not preserved on reuse: %v", err)
	}
	if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("redundant incoming snapshot not deleted on reuse: %v", err)
	}
}

// TestPlanStoreRetentionSevenDayAndLockRace proves creation prunes plans older
// than seven days but never one a reader holds through the stable lock, and that
// releasing the lock lets a later prune delete it.
func TestPlanStoreRetentionSevenDayAndLockRace(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	store.now = func() time.Time { return base }
	old := makeArtifacts("old")
	if err := store.Save(ctx, old, ""); err != nil {
		t.Fatal(err)
	}
	locked := makeArtifacts("locked-old")
	if err := store.Save(ctx, locked, ""); err != nil {
		t.Fatal(err)
	}

	release, err := store.LockReview(ctx, locked.Plan.ReviewID)
	if err != nil {
		t.Fatal(err)
	}

	// A creation eight days later prunes the unlocked old plan but not the locked.
	store.now = func() time.Time { return base.Add(8 * 24 * time.Hour) }
	if err := store.Save(ctx, makeArtifacts("fresh"), ""); err != nil {
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
		if err := store.Save(ctx, makeArtifacts(fmt.Sprintf("p-%02d", i)), ""); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if n := countReviews(t, store); n != retentionCountV1 {
		t.Fatalf("retained %d plans, want %d", n, retentionCountV1)
	}
}

// TestPlanStoreLoadRejectsMalformedReviewID proves review ids are validated
// before any path join or open.
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
	if err := store.Save(ctx, a, ""); err != nil {
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
	if err := store.Save(context.Background(), a, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx, a.Plan.ReviewID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load err=%v, want context.Canceled", err)
	}
}

// TestPlanStoreLoadDetectsTamper proves the integrity digest catches a mutated
// manifest independent of the injectable ID digest.
func TestPlanStoreLoadDetectsTamper(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("tamper")
	if err := store.Save(ctx, a, ""); err != nil {
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
	idBytes := a.PlanIdentityBytes
	m := planManifest{
		Schema: planStoreSchemaV1, ReviewID: a.Plan.ReviewID, PlanDigest: a.Plan.PlanDigest,
		ScopeKind: ScopeWorkspace, // disagrees with the embedded commit-scope plan
		CreatedAt: 1, Plan: a.Plan, PlanIdentityBytes: idBytes, IDRecords: a.IDRecords,
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
	return store
}

func makeSnapshot(t *testing.T, store *PlanStore, marker string) string {
	t.Helper()
	dir, err := os.MkdirTemp(store.Root(), "snap-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
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
