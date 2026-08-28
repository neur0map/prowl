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

// makeArtifacts builds a minimal valid direct-mode plan whose review id is the
// canonical truncation of its plan digest, so the store's identity checks pass.
func makeArtifacts(name string) PlanArtifacts {
	full := sha256.Sum256([]byte("plan:" + name))
	return artifactsFromDigest(full)
}

func artifactsFromDigest(full [32]byte) PlanArtifacts {
	reviewID := "rvw_" + hex.EncodeToString(full[:20])
	planDigest := hex.EncodeToString(full[:])
	oid20 := SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	plan := Plan{
		Schema:       PlanSchemaV1,
		ReviewID:     reviewID,
		PlanDigest:   planDigest,
		Mode:         ModeDirect,
		Scope:        Scope{Kind: ScopeCommit, ObjectFormat: "sha1", Base: oid20, Head: oid20},
		Stats:        PlanStats{ChangedPaths: 1},
		ChangedPaths: []PlanPath{{PathID: "p_x", NewPath: "a.go", Status: "M", ReviewClass: "full", Coverage: "full", Roles: []string{"implementation"}}},
		NextCommands: []NextCommand{{Label: "unit", Command: "prowl-agent review unit " + reviewID}},
	}
	return PlanArtifacts{Plan: plan}
}

// linkedWorktree builds a repository plus a detached linked worktree, returning
// both roots and a sanitized runner.
func linkedWorktree(t *testing.T) (main, linked string, runner GitRunner) {
	t.Helper()
	f := newGitFixture(t)
	f.commitFile(t, "seed.txt", "x\n")
	linked = filepath.Join(t.TempDir(), "linked")
	rawGit(t, f.root, "worktree", "add", "-q", "--detach", linked)
	return f.root, linked, execRunner(t)
}

// TestPlanStoreLivesOutsideWorktreeAndDetectsCollision proves review state lives
// under the shared Git common dir (outside a linked worktree) and that reusing a
// public review id with a different full digest is a collision.
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
	// Linked worktrees share one collision/retention domain.
	mainStore, err := OpenPlanStore(ctx, runner, main)
	if err != nil {
		t.Fatal(err)
	}
	if mainStore.Root() != store.Root() {
		t.Fatalf("linked worktree domain differs: %s vs %s", mainStore.Root(), store.Root())
	}

	a := makeArtifacts("collide")
	if err := store.Save(ctx, a, ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// Same public review id, different full plan digest (flip a trailing byte so
	// the first 20 bytes - and thus the review id - are unchanged).
	full := sha256.Sum256([]byte("plan:collide"))
	full[31] ^= 0xff
	b := artifactsFromDigest(full)
	if b.Plan.ReviewID != a.Plan.ReviewID {
		t.Fatalf("test setup: review ids differ %s vs %s", b.Plan.ReviewID, a.Plan.ReviewID)
	}
	if err := store.Save(ctx, b, ""); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("collision save err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreSaveLoadRoundTrip proves artifacts survive an atomic save/load.
func TestPlanStoreSaveLoadRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("round")
	a.PublishedIndexSignature = "sig-123"
	a.MandatoryUnits = map[string][]byte{"u_1": []byte("{\"schema\":\"review.unit.v1\"}\n")}
	a.Citations = map[string]CitationProof{"c_1": {ID: "c_1", Side: SideHead, Path: "a.go", ContentHash: hex.EncodeToString(make([]byte, 32)), Start: 1, End: 4}}
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
	if got.Citations["c_1"].Path != "a.go" {
		t.Fatalf("citation proof lost: %+v", got.Citations)
	}
}

// TestPlanStoreAtomicTempRename proves the review directory is published by an
// atomic rename with the snapshot moved in and no temp directory left behind.
func TestPlanStoreAtomicTempRename(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	a := makeArtifacts("atomic")

	snapshotDir, err := os.MkdirTemp(store.Root(), "snap-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("original snapshot dir still present: %v", err)
	}
	// No .tmp-* work directory left behind.
	entries, _ := os.ReadDir(store.Root())
	for _, e := range entries {
		if len(e.Name()) >= 5 && e.Name()[:5] == ".tmp-" {
			t.Fatalf("temp directory leaked: %s", e.Name())
		}
	}
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

	// A second store instance over the same root cannot acquire the lock.
	other := &PlanStore{root: store.root, lockPath: store.lockPath, lockTimeout: 150 * time.Millisecond, digest: store.digest, now: store.now}
	if err := other.Save(ctx, makeArtifacts("locked"), ""); !errors.Is(err, ErrPlanLocked) {
		t.Fatalf("save under held lock err=%v, want ErrPlanLocked", err)
	}
}

// TestPlanStoreStaleWorkspaceFingerprint proves a workspace plan is stale when
// the workspace scope changes, while a committed plan is never stale.
func TestPlanStoreStaleWorkspaceFingerprint(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	ws := makeArtifacts("workspace")
	ws.Plan.Scope.Kind = ScopeWorkspace
	ws.Plan.Scope.Base = SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)}
	ws.Plan.Scope.Head = SideIdentity{Kind: SideWorkspaceSHA256, Value: make([]byte, 32)}
	ws.WorkspaceFingerprint = "fp-A"
	if err := store.Save(ctx, ws, ""); err != nil {
		t.Fatal(err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, "fp-A"); err != nil || stale {
		t.Fatalf("fresh workspace plan stale=%v err=%v", stale, err)
	}
	if stale, err := store.Stale(ctx, ws.Plan.ReviewID, "fp-B"); err != nil || !stale {
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
// immutable manifest and snapshot without rewriting.
func TestPlanStoreImmutableReuse(t *testing.T) {
	store := newStore(t)
	base := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return base }
	ctx := context.Background()

	a := makeArtifacts("reuse")
	snap, _ := os.MkdirTemp(store.Root(), "snap-")
	_ = os.WriteFile(filepath.Join(snap, "keep"), []byte("1"), 0o600)
	if err := store.Save(ctx, a, snap); err != nil {
		t.Fatal(err)
	}
	firstCreated := mustManifest(t, store, a.Plan.ReviewID).CreatedAt

	// A later, identical save must be a no-op reuse (created time unchanged).
	store.now = func() time.Time { return base.Add(72 * time.Hour) }
	extra, _ := os.MkdirTemp(store.Root(), "snap2-")
	if err := store.Save(ctx, a, extra); err != nil {
		t.Fatalf("reuse save: %v", err)
	}
	if got := mustManifest(t, store, a.Plan.ReviewID).CreatedAt; got != firstCreated {
		t.Fatalf("reuse rewrote manifest: created %d -> %d", firstCreated, got)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), a.Plan.ReviewID, snapshotName, "keep")); err != nil {
		t.Fatalf("original snapshot not preserved on reuse: %v", err)
	}
}

// TestPlanStoreRetentionSevenDayAndLock proves creation prunes plans older than
// seven days but never a plan held by a per-review lock.
func TestPlanStoreRetentionSevenDayAndLock(t *testing.T) {
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
	if _, err := store.Load(ctx, old.Plan.ReviewID); err != nil {
		t.Fatalf("old plan not saved: %v", err)
	}

	// Hold the locked plan so retention cannot delete it.
	release, err := store.LockReview(ctx, locked.Plan.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// A creation eight days later prunes the unlocked old plan.
	store.now = func() time.Time { return base.Add(8 * 24 * time.Hour) }
	fresh := makeArtifacts("fresh")
	if err := store.Save(ctx, fresh, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, old.Plan.ReviewID); !errors.Is(err, ErrReviewNotFound) {
		t.Fatalf("stale plan not pruned: err=%v", err)
	}
	if _, err := store.Load(ctx, locked.Plan.ReviewID); err != nil {
		t.Fatalf("locked plan was pruned: %v", err)
	}
	if _, err := store.Load(ctx, fresh.Plan.ReviewID); err != nil {
		t.Fatalf("fresh plan missing: %v", err)
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

// TestPlanStoreForcedCollisionDuringConstruction injects a digest that maps
// distinct identity inputs to the same public prefix with different fulls, and
// proves Save rejects the second plan.
func TestPlanStoreForcedCollisionDuringConstruction(t *testing.T) {
	store := newStore(t)
	store.digest = collidingDigest
	ctx := context.Background()

	first := makeArtifacts("c-one")
	first.IDInputs = []IDInput{{Kind: PathIDPrefixV1, Bytes: []byte("A")}}
	if err := store.Save(ctx, first, ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	second := makeArtifacts("c-two")
	second.IDInputs = []IDInput{{Kind: PathIDPrefixV1, Bytes: []byte("B")}}
	if err := store.Save(ctx, second, ""); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("colliding construction err=%v, want ErrIDCollision", err)
	}
}

// TestPlanStoreForcedCollisionDuringLoad saves two non-colliding inputs with a
// registry rebuilt during manifest loading rejects the collision.
func TestPlanStoreForcedCollisionDuringLoad(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	a := makeArtifacts("load-collide")
	a.IDInputs = []IDInput{
		{Kind: PathIDPrefixV1, Bytes: []byte("A")},
		{Kind: PathIDPrefixV1, Bytes: []byte("B")},
	}
	if err := store.Save(ctx, a, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A fresh store over the same root whose digest collides the two inputs.
	loader := &PlanStore{root: store.root, lockPath: store.lockPath, lockTimeout: store.lockTimeout, digest: collidingDigest, now: store.now}
	if _, err := loader.Load(ctx, a.Plan.ReviewID); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("colliding load err=%v, want ErrIDCollision", err)
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
	// Mutate a field value while keeping the stored content digest, so loading
	// recomputes a digest that no longer matches.
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

// ---- helpers --------------------------------------------------------------

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

func mustManifest(t *testing.T, store *PlanStore, reviewID string) planManifest {
	t.Helper()
	m, err := store.loadManifest(reviewID)
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

func hasPathPrefix(path, dir string) bool {
	return path == dir || (len(path) > len(dir) && path[:len(dir)] == dir && path[len(dir)] == filepath.Separator)
}
