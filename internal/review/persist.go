package review

// persist.go stores review plans immutably below Git common state so review
// artifacts can never enter a worktree's tracked status, even across linked
// worktrees, which share one collision and retention domain. The store keeps an
// open, pinned reviews-root descriptor and reads every manifest relative to it
// (no-follow, bounded, context-aware decode) so the reviews path cannot be
// swapped underneath a reader. Snapshots are leased by the store under its own
// <common>/prowl/snapshots root - never an arbitrary caller path - and consumed
// through a locked temp-dir-and-rename that validates the lease (no-follow
// directory on the same device) and restores it on any pre-publication failure.
// Identity is verified by recomputing the plan digest from its canonical bytes;
// the registry enumerates every content ID the plan, its unit candidates,
// mandatory units, and citations reference, verifies each record's full/public
// mapping, and rejects a same-kind prefix that maps to a different full digest
// across the shared domain on both save and load. Workspace plans record a
// bound staleness fingerprint; an identical plan reuses the immutable manifest
// and deletes the redundant incoming lease; and creation prunes plans older than
// seven days beyond the twenty most recent while never deleting a plan a reader
// holds through a fixed, hash-striped set of lock files whose count is bounded
// for the store's lifetime.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/prowl-agent/prowl-agent/internal/boundedio"
	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
)

const (
	planStoreSchemaV1 = "review.manifest.v1"
	reviewsSubdir     = "reviews"
	locksSubdir       = "locks"
	snapshotsSubdir   = "snapshots"
	manifestName      = "manifest.json"
	snapshotName      = "snapshot"
	storeLockName     = ".lock"
	leasePrefix       = "snap-"

	// retentionAgeV1 removes review directories older than seven days.
	retentionAgeV1 = 7 * 24 * time.Hour
	// retentionCountV1 keeps at most the twenty most recent plans.
	retentionCountV1 = 20

	// lockStripeCountV1 is the fixed number of per-review lock files. A review's
	// lock is chosen by hashing its id into this range, so the store's lifetime
	// lock-file count is bounded regardless of how many review ids it ever sees,
	// and a reader and the pruner of the same review share one stable inode.
	lockStripeCountV1 = 256

	// defaultLockTimeout bounds how long Save/Prune/LockReview wait for a lock.
	defaultLockTimeout = 5 * time.Second

	// maxManifestBytesV1 bounds a manifest read into memory. Manifests carry the
	// mandatory unit bytes (16 MiB/unit) and candidates, so the ceiling is
	// generous but finite; an overlong manifest fails closed.
	maxManifestBytesV1 int64 = 512 << 20
)

var (
	// ErrIDCollision reports that a content-derived public ID prefix maps to two
	// different full digests of the same kind, across the shared review domain.
	ErrIDCollision = errors.New("review: public ID prefix collision with a different full digest")
	// ErrIDRegistry reports a malformed identity registry: a record whose full or
	// public does not match its canonical input, a duplicate record, a plan ID
	// with no record, or a record for an ID absent from the plan.
	ErrIDRegistry = errors.New("review: identity registry does not match the plan")
	// ErrPlanLocked reports that a mutation or per-review lock is held elsewhere.
	ErrPlanLocked = errors.New("review: plan store is locked")
	// ErrManifestCorrupt reports that a manifest failed its integrity digest, was
	// too large, or is otherwise malformed.
	ErrManifestCorrupt = errors.New("review: review manifest is corrupt")
	// ErrReviewNotFound reports that no persisted plan has the requested id.
	ErrReviewNotFound = errors.New("review: no persisted plan for review id")
	// ErrPlanIdentityMismatch reports that a plan digest or review id is not the
	// canonical hash/truncation of the plan's canonical identity bytes.
	ErrPlanIdentityMismatch = errors.New("review: plan identity is inconsistent")
	// ErrWorkspaceFingerprint reports a workspace plan without a bound fingerprint
	// or a committed/range plan carrying one.
	ErrWorkspaceFingerprint = errors.New("review: workspace fingerprint invariant violated")
	// ErrSnapshotOwnership reports that a leased snapshot could not be persisted
	// atomically (for example a cross-filesystem move); the caller's lease is left
	// intact.
	ErrSnapshotOwnership = errors.New("review: snapshot could not be persisted atomically")
	// ErrSnapshotLease reports that a snapshot lease is not owned by this store,
	// is not a real directory reached without a symlink, or is on a different
	// device than the review root.
	ErrSnapshotLease = errors.New("review: snapshot lease is not a valid store-owned directory")
	// errManifestTooLarge is the internal sentinel the bounded manifest reader
	// returns when the ceiling is exceeded mid-read.
	errManifestTooLarge = errors.New("review: manifest exceeds its byte ceiling")
)

// contentIDKinds are the content-derived public ID kinds the registry tracks.
// The review id (rvw_) is registered separately from the plan digest it carries.
var contentIDKinds = map[string]bool{
	PathIDPrefixV1:   true,
	HunkIDPrefixV1:   true,
	UnitIDPrefixV1:   true,
	CohortIDPrefixV1: true,
	LayerIDPrefixV1:  true,
	TargetIDPrefixV1: true,
}

// CitationProof is a persisted, content-qualified proof of one citation's exact
// source location and bytes.
type CitationProof struct {
	ID          string     `json:"id"`
	Side        ReviewSide `json:"side"`
	Path        string     `json:"path"`
	ContentHash string     `json:"content_hash"`
	Start       int        `json:"start"`
	End         int        `json:"end"`
}

// IDRecord binds one content-derived public ID to its full digest and the exact
// canonical FramedFieldsV1 bytes it was hashed from, so the store can rehash and
// verify the full/public mapping and detect prefix collisions on save and load.
type IDRecord struct {
	Kind      string `json:"kind"`
	Public    string `json:"public"`
	Full      Digest `json:"full"`
	Canonical []byte `json:"canonical"`
}

// PlanArtifacts is everything persisted atomically with a plan: the plan itself,
// per-unit retrieval candidates, mandatory unit bytes, citation proofs, the
// published head-index signature, the canonical plan identity bytes, the identity
// records for every content ID, and (for workspace plans) the bound capture
// fingerprint.
type PlanArtifacts struct {
	Plan                    Plan
	UnitCandidates          map[string][]contextpacket.Candidate
	MandatoryUnits          map[string][]byte
	Citations               map[string]CitationProof
	PublishedIndexSignature string
	// PlanIdentityBytes is the canonical ReviewPlanIdentityV1 encoding; the store
	// hashes it with a fixed SHA-256 and requires the result to match the plan
	// digest and review id rather than trusting the supplied strings.
	PlanIdentityBytes []byte
	// IDRecords covers every content-derived public ID in the plan/artifacts.
	IDRecords []IDRecord
	// WorkspaceFingerprint is the workspace head fingerprint for a workspace
	// plan; it is empty for an immutable committed or range plan.
	WorkspaceFingerprint string
}

// SaveResult reports the outcome of a Save. PruneWarning carries a
// post-publication retention error that never fails the save: the plan is
// durably persisted regardless.
type SaveResult struct {
	ReviewID     string
	Reused       bool
	PruneWarning error
}

// planManifest is the on-disk representation of a persisted plan.
type planManifest struct {
	Schema                  string                               `json:"schema"`
	ReviewID                string                               `json:"review_id"`
	PlanDigest              string                               `json:"plan_digest"`
	ScopeKind               ScopeKind                            `json:"scope_kind"`
	WorkspaceFingerprint    string                               `json:"workspace_fingerprint,omitempty"`
	CreatedAt               int64                                `json:"created_at"`
	Plan                    Plan                                 `json:"plan"`
	UnitCandidates          map[string][]contextpacket.Candidate `json:"unit_candidates,omitempty"`
	MandatoryUnits          map[string][]byte                    `json:"mandatory_units,omitempty"`
	Citations               map[string]CitationProof             `json:"citations,omitempty"`
	PublishedIndexSignature string                               `json:"published_index_signature,omitempty"`
	PlanIdentityBytes       []byte                               `json:"plan_identity_bytes"`
	IDRecords               []IDRecord                           `json:"id_records,omitempty"`
	ContentDigest           string                               `json:"content_digest"`
}

// SnapshotLease is a store-owned directory beneath <common>/prowl/snapshots that
// a caller materializes into and then hands to Save. Only the store can mint a
// lease, and Save validates it (no-follow directory, same device, store-owned)
// before consuming it, so a caller can never direct Save at an arbitrary
// destructive path. An unconsumed lease is the caller's to Release.
type SnapshotLease struct {
	Dir      string
	store    *PlanStore
	consumed bool
}

// Release deletes an unconsumed lease directory. It is a no-op once the lease has
// been consumed by a successful Save.
func (l *SnapshotLease) Release() error {
	if l == nil || l.consumed {
		return nil
	}
	l.consumed = true
	return os.RemoveAll(l.Dir)
}

// PlanStore persists plans beneath the Git common directory. It is safe to open
// from any linked worktree; every instance shares one collision and retention
// domain.
type PlanStore struct {
	root         string
	rootDir      *os.Root
	locksDir     string
	snapshotsDir string
	lockPath     string
	lockTimeout  time.Duration
	maxManifest  int64
	// digest derives content-ID full digests; it is injectable so a test can
	// force prefix collisions during construction and manifest loading. Plan
	// identity and manifest integrity always use a fixed SHA-256 independent of
	// this seam.
	digest func([]byte) Digest
	// now supplies the creation clock; injectable so retention is deterministic.
	now func() time.Time
	// sameDevice reports whether two paths share a filesystem; injectable so a
	// cross-device lease rejection is testable without a second mount.
	sameDevice func(a, b string) (bool, error)
	// pruneOverride, when set, replaces the post-publication retention pass so a
	// test can observe a surfaced prune warning deterministically.
	pruneOverride func(context.Context, time.Time) error
}

// OpenPlanStore resolves the Git common directory, roots the review state beneath
// it (never inside a worktree), pins an open descriptor to that root, and
// prepares the store-wide lock, per-review lock, and snapshot directories.
func OpenPlanStore(ctx context.Context, runner GitRunner, repoRoot string) (*PlanStore, error) {
	if runner == nil {
		return nil, errors.New("review: OpenPlanStore requires a runner")
	}
	common, err := gitCommonDir(ctx, runner, repoRoot)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(common, "prowl", reviewsSubdir)
	locks := filepath.Join(root, locksSubdir)
	snapshots := filepath.Join(common, "prowl", snapshotsSubdir)
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snapshots, 0o700); err != nil {
		return nil, err
	}
	rootDir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &PlanStore{
		root:         root,
		rootDir:      rootDir,
		locksDir:     locks,
		snapshotsDir: snapshots,
		lockPath:     filepath.Join(root, storeLockName),
		lockTimeout:  defaultLockTimeout,
		maxManifest:  maxManifestBytesV1,
		digest:       func(b []byte) Digest { return sha256.Sum256(b) },
		now:          time.Now,
		sameDevice:   sameDevice,
	}, nil
}

// Root returns the review-state root directory.
func (s *PlanStore) Root() string { return s.root }

// Close releases the pinned reviews-root descriptor.
func (s *PlanStore) Close() error {
	if s.rootDir == nil {
		return nil
	}
	err := s.rootDir.Close()
	s.rootDir = nil
	return err
}

// NewSnapshotLease mints a fresh store-owned snapshot directory under the store's
// snapshots root. The caller materializes into lease.Dir and then hands the lease
// to Save (or Release()s it on an abandoned attempt).
func (s *PlanStore) NewSnapshotLease(ctx context.Context) (*SnapshotLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.snapshotsDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(s.snapshotsDir, leasePrefix)
	if err != nil {
		return nil, err
	}
	return &SnapshotLease{Dir: dir, store: s}, nil
}

// Save persists artifacts atomically under the plan's review id. It recomputes
// the plan digest from the canonical identity bytes (never trusting the supplied
// strings), enforces the identity registry and workspace-fingerprint invariants,
// consumes the caller's store-owned lease on success, reuses an identical
// existing plan (deleting the redundant incoming lease), and restores the lease
// on any pre-publication failure. Post-publication pruning is best-effort and is
// returned as SaveResult.PruneWarning, never as a Save failure.
func (s *PlanStore) Save(ctx context.Context, artifacts PlanArtifacts, lease *SnapshotLease) (SaveResult, error) {
	if err := artifacts.Plan.Validate(); err != nil {
		return SaveResult{}, err
	}
	full, err := s.verifyPlanIdentity(artifacts.Plan, artifacts.PlanIdentityBytes)
	if err != nil {
		return SaveResult{}, err
	}
	if err := verifyWorkspaceFingerprint(artifacts.Plan, artifacts.WorkspaceFingerprint); err != nil {
		return SaveResult{}, err
	}
	reviewID := artifacts.Plan.ReviewID

	release, err := s.lock(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	defer release()

	reviewDir := filepath.Join(s.root, reviewID)
	if existing, err := s.loadManifest(ctx, reviewID); err == nil {
		existingFull := sha256.Sum256(existing.PlanIdentityBytes)
		if existingFull == full {
			// Identical immutable plan: reuse, deleting the redundant incoming
			// lease so the caller does not leak a temp tree.
			_ = lease.Release()
			return SaveResult{ReviewID: reviewID, Reused: true}, nil
		}
		return SaveResult{}, fmt.Errorf("%w: review %s already persisted with a different identity", ErrIDCollision, reviewID)
	} else if !errors.Is(err, ErrReviewNotFound) {
		return SaveResult{}, err
	}

	registry, err := s.buildDomainRegistry(ctx, reviewID)
	if err != nil {
		return SaveResult{}, err
	}
	if _, err := addPlanToRegistry(ctx, registry, s.digest, artifacts.Plan, artifacts.Citations, artifacts.UnitCandidates, artifacts.MandatoryUnits, artifacts.IDRecords, full); err != nil {
		return SaveResult{}, err
	}

	manifest := planManifest{
		Schema:                  planStoreSchemaV1,
		ReviewID:                reviewID,
		PlanDigest:              artifacts.Plan.PlanDigest,
		ScopeKind:               artifacts.Plan.Scope.Kind,
		WorkspaceFingerprint:    artifacts.WorkspaceFingerprint,
		CreatedAt:               s.now().Unix(),
		Plan:                    artifacts.Plan,
		UnitCandidates:          artifacts.UnitCandidates,
		MandatoryUnits:          artifacts.MandatoryUnits,
		Citations:               artifacts.Citations,
		PublishedIndexSignature: artifacts.PublishedIndexSignature,
		PlanIdentityBytes:       artifacts.PlanIdentityBytes,
		IDRecords:               artifacts.IDRecords,
	}
	payload, err := marshalManifest(manifest)
	if err != nil {
		return SaveResult{}, err
	}
	if err := s.publish(reviewDir, payload, lease); err != nil {
		return SaveResult{}, err
	}

	// Retention runs after publication and never fails the save; a failure is
	// surfaced as a warning on the result.
	res := SaveResult{ReviewID: reviewID}
	if s.pruneOverride != nil {
		res.PruneWarning = s.pruneOverride(ctx, s.now())
	} else {
		res.PruneWarning = s.pruneLocked(ctx, s.now())
	}
	return res, nil
}

// publish writes the manifest into a temp directory, validates and moves the
// caller's leased snapshot into it, then atomically renames it to the review
// directory. A pre-publication failure leaves the caller's lease intact
// (restoring it if it was already moved) and no review or temp directory behind.
func (s *PlanStore) publish(reviewDir string, payload []byte, lease *SnapshotLease) error {
	tmp, err := os.MkdirTemp(s.root, ".tmp-save-")
	if err != nil {
		return err
	}
	committed := false
	moved := false
	defer func() {
		if committed {
			return
		}
		if moved && lease != nil {
			// Restore the caller's lease before the temp directory is removed.
			if rerr := os.Rename(filepath.Join(tmp, snapshotName), lease.Dir); rerr == nil {
				lease.consumed = false
			}
		}
		_ = os.RemoveAll(tmp)
	}()
	if err := writeRootedFile(tmp, manifestName, payload); err != nil {
		return err
	}
	if lease != nil {
		if err := s.consumeLease(lease, filepath.Join(tmp, snapshotName)); err != nil {
			return err
		}
		moved = true
	}
	if err := os.Rename(tmp, reviewDir); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotOwnership, err)
	}
	committed = true
	return nil
}

// consumeLease validates that a lease is a genuine store-owned directory - minted
// by this store, a direct child of the snapshots root reached without a symlink,
// a real directory, and on the same device as the review root - and then moves it
// to dest, marking it consumed. Any validation failure leaves the lease intact.
func (s *PlanStore) consumeLease(lease *SnapshotLease, dest string) error {
	if lease.store != s {
		return fmt.Errorf("%w: lease was not issued by this store", ErrSnapshotLease)
	}
	if lease.consumed {
		return fmt.Errorf("%w: lease already consumed", ErrSnapshotLease)
	}
	if filepath.Dir(lease.Dir) != s.snapshotsDir {
		return fmt.Errorf("%w: %q is not under the store snapshots root", ErrSnapshotLease, lease.Dir)
	}
	base := filepath.Base(lease.Dir)
	sroot, err := os.OpenRoot(s.snapshotsDir)
	if err != nil {
		return err
	}
	defer sroot.Close()
	info, err := sroot.Lstat(base)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotLease, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a real directory", ErrSnapshotLease, lease.Dir)
	}
	same, err := s.sameDevice(s.root, lease.Dir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotLease, err)
	}
	if !same {
		return fmt.Errorf("%w: %q is on a different device", ErrSnapshotLease, lease.Dir)
	}
	if err := os.Rename(lease.Dir, dest); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotOwnership, err)
	}
	lease.consumed = true
	return nil
}

// Load reads and fully verifies a persisted plan: bounded context-aware manifest
// read through the pinned reviews root, integrity digest, plan-identity
// recomputation, workspace-fingerprint invariant, and the complete identity
// registry (mapping and cross-domain collisions).
func (s *PlanStore) Load(ctx context.Context, reviewID string) (PlanArtifacts, error) {
	manifest, err := s.loadManifest(ctx, reviewID)
	if err != nil {
		return PlanArtifacts{}, err
	}
	if err := s.verifyManifest(ctx, manifest); err != nil {
		return PlanArtifacts{}, err
	}
	return PlanArtifacts{
		Plan:                    manifest.Plan,
		UnitCandidates:          manifest.UnitCandidates,
		MandatoryUnits:          manifest.MandatoryUnits,
		Citations:               manifest.Citations,
		PublishedIndexSignature: manifest.PublishedIndexSignature,
		PlanIdentityBytes:       manifest.PlanIdentityBytes,
		IDRecords:               manifest.IDRecords,
		WorkspaceFingerprint:    manifest.WorkspaceFingerprint,
	}, nil
}

// Stale reports whether a workspace plan no longer matches the current workspace
// scope. It runs the same full verification as Load. Committed and range plans
// apply to an immutable resolved head and are never stale.
func (s *PlanStore) Stale(ctx context.Context, reviewID, currentFingerprint string) (bool, error) {
	loaded, err := s.Load(ctx, reviewID)
	if err != nil {
		return false, err
	}
	if loaded.Plan.Scope.Kind != ScopeWorkspace {
		return false, nil
	}
	return loaded.WorkspaceFingerprint != currentFingerprint, nil
}

// LockReview acquires the review's hash-striped lock (kept outside the deletable
// review directory) that a caller holds while a plan is in use, so retention
// pruning will not delete it. The returned release closes the lock. The wait is
// bounded so a busy stripe never hangs the caller.
func (s *PlanStore) LockReview(ctx context.Context, reviewID string) (func(), error) {
	if !validReviewID(reviewID) {
		return nil, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	// Acquire the stable lock before checking existence so a reader and pruner
	// can never both believe they own the plan.
	fl := flock.New(s.reviewLockPath(reviewID))
	lockCtx, cancel := context.WithTimeout(ctx, s.lockTimeout)
	defer cancel()
	locked, err := fl.TryLockContext(lockCtx, 25*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrPlanLocked
		}
		return nil, err
	}
	if !locked {
		return nil, ErrPlanLocked
	}
	if _, err := s.rootDir.Stat(reviewID); err != nil {
		_ = fl.Unlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return nil, err
	}
	return func() { _ = fl.Unlock() }, nil
}

// Prune removes review directories older than seven days and keeps at most the
// twenty most recent, never deleting a plan currently held by a per-review lock.
func (s *PlanStore) Prune(ctx context.Context, now time.Time) error {
	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()
	return s.pruneLocked(ctx, now)
}

// ---- identity and registry ------------------------------------------------

// verifyPlanIdentity recomputes the full plan digest from the canonical identity
// bytes with a fixed SHA-256 and requires it to match the plan digest and review
// id, never trusting the supplied strings.
func (s *PlanStore) verifyPlanIdentity(plan Plan, identityBytes []byte) (Digest, error) {
	if len(identityBytes) == 0 {
		return Digest{}, fmt.Errorf("%w: missing canonical plan identity bytes", ErrPlanIdentityMismatch)
	}
	full := sha256.Sum256(identityBytes)
	if hex.EncodeToString(full[:]) != plan.PlanDigest {
		return Digest{}, fmt.Errorf("%w: recomputed digest does not match plan digest", ErrPlanIdentityMismatch)
	}
	if PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public != plan.ReviewID {
		return Digest{}, fmt.Errorf("%w: review id %q does not match plan digest", ErrPlanIdentityMismatch, plan.ReviewID)
	}
	return full, nil
}

// verifyWorkspaceFingerprint enforces that a workspace plan carries a fingerprint
// bound to its workspace head, and a committed/range plan carries none.
func verifyWorkspaceFingerprint(plan Plan, fingerprint string) error {
	if plan.Scope.Kind == ScopeWorkspace {
		if fingerprint == "" {
			return fmt.Errorf("%w: workspace plan missing fingerprint", ErrWorkspaceFingerprint)
		}
		want := hex.EncodeToString(plan.Scope.Head.Value)
		if plan.Scope.Head.Kind != SideWorkspaceSHA256 || fingerprint != want {
			return fmt.Errorf("%w: fingerprint not bound to the workspace head", ErrWorkspaceFingerprint)
		}
		return nil
	}
	if fingerprint != "" {
		return fmt.Errorf("%w: committed/range plan must not carry a workspace fingerprint", ErrWorkspaceFingerprint)
	}
	return nil
}

// verifyManifest re-runs every consistency check a load must pass, including the
// cross-domain collision registry (not merely the manifest's own records).
func (s *PlanStore) verifyManifest(ctx context.Context, m planManifest) error {
	if m.Schema != planStoreSchemaV1 {
		return fmt.Errorf("%w: schema %q", ErrManifestCorrupt, m.Schema)
	}
	if m.ReviewID != m.Plan.ReviewID || m.PlanDigest != m.Plan.PlanDigest || m.ScopeKind != m.Plan.Scope.Kind {
		return fmt.Errorf("%w: manifest header disagrees with embedded plan", ErrManifestCorrupt)
	}
	if err := m.Plan.Validate(); err != nil {
		return err
	}
	full, err := s.verifyPlanIdentity(m.Plan, m.PlanIdentityBytes)
	if err != nil {
		return err
	}
	if err := verifyWorkspaceFingerprint(m.Plan, m.WorkspaceFingerprint); err != nil {
		return err
	}
	registry, err := s.buildDomainRegistry(ctx, m.ReviewID)
	if err != nil {
		return err
	}
	if _, err := addPlanToRegistry(ctx, registry, s.digest, m.Plan, m.Citations, m.UnitCandidates, m.MandatoryUnits, m.IDRecords, full); err != nil {
		return err
	}
	return ctx.Err()
}

// buildDomainRegistry rebuilds the collision registry from every persisted
// manifest except the one being written or verified. A sibling that cannot be
// read is skipped (it will be pruned), never allowed to fail an unrelated load.
func (s *PlanStore) buildDomainRegistry(ctx context.Context, exclude string) (map[string]Digest, error) {
	registry := map[string]Digest{}
	ids, err := s.reviewIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if id == exclude {
			continue
		}
		m, err := s.loadManifest(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		full := sha256.Sum256(m.PlanIdentityBytes)
		if _, err := addPlanToRegistry(ctx, registry, s.digest, m.Plan, m.Citations, m.UnitCandidates, m.MandatoryUnits, m.IDRecords, full); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// addPlanToRegistry verifies a plan's identity records against the IDs the plan,
// its unit candidates, mandatory units, and citations actually reference, then
// merges them (and the review id) into the registry, rejecting a same-kind prefix
// that maps to a different full digest.
func addPlanToRegistry(ctx context.Context, registry map[string]Digest, digest func([]byte) Digest, plan Plan, citations map[string]CitationProof, unitCandidates map[string][]contextpacket.Candidate, mandatoryUnits map[string][]byte, records []IDRecord, reviewFull Digest) (map[string]Digest, error) {
	byPublic, err := verifyRecords(ctx, digest, records)
	if err != nil {
		return nil, err
	}
	enumerated := enumeratePlanIDs(plan, citations, unitCandidates, mandatoryUnits)
	for id := range enumerated {
		if _, ok := byPublic[id]; !ok {
			return nil, fmt.Errorf("%w: plan references %s with no identity record", ErrIDRegistry, id)
		}
	}
	for id := range byPublic {
		if _, ok := enumerated[id]; !ok {
			return nil, fmt.Errorf("%w: identity record %s is not referenced by the plan", ErrIDRegistry, id)
		}
	}
	if err := registerID(registry, plan.ReviewID, reviewFull); err != nil {
		return nil, err
	}
	for public, full := range byPublic {
		if err := registerID(registry, public, full); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// verifyRecords rehashes each record's canonical input, verifies its full/public
// mapping, and rejects an unknown kind or a duplicate public within the set.
func verifyRecords(ctx context.Context, digest func([]byte) Digest, records []IDRecord) (map[string]Digest, error) {
	byPublic := make(map[string]Digest, len(records))
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !contentIDKinds[r.Kind] {
			return nil, fmt.Errorf("%w: unknown identity kind %q", ErrIDRegistry, r.Kind)
		}
		got := digest(r.Canonical)
		if got != r.Full {
			return nil, fmt.Errorf("%w: record %s full digest does not match its canonical input", ErrIDRegistry, r.Public)
		}
		if PublicID(r.Kind, r.Full, PublicIDContentBytesV1).Public != r.Public {
			return nil, fmt.Errorf("%w: record %s public prefix does not match its full digest", ErrIDRegistry, r.Public)
		}
		if existing, ok := byPublic[r.Public]; ok {
			if existing != r.Full {
				return nil, fmt.Errorf("%w: %s", ErrIDCollision, r.Public)
			}
			return nil, fmt.Errorf("%w: duplicate identity record %s", ErrIDRegistry, r.Public)
		}
		byPublic[r.Public] = r.Full
	}
	return byPublic, nil
}

// registerID adds one public->full mapping, rejecting a differing full digest
// for a public prefix already present.
func registerID(registry map[string]Digest, public string, full Digest) error {
	if existing, ok := registry[public]; ok {
		if existing != full {
			return fmt.Errorf("%w: %s", ErrIDCollision, public)
		}
		return nil
	}
	registry[public] = full
	return nil
}

// enumeratePlanIDs collects every syntactically valid content-derived public ID
// the plan, its unit candidates, mandatory units, and citations reference.
func enumeratePlanIDs(plan Plan, citations map[string]CitationProof, unitCandidates map[string][]contextpacket.Candidate, mandatoryUnits map[string][]byte) map[string]struct{} {
	set := map[string]struct{}{}
	add := func(id string) {
		if _, ok := contentKindOf(id); ok {
			set[id] = struct{}{}
		}
	}
	for _, p := range plan.ChangedPaths {
		add(p.PathID)
	}
	for _, c := range plan.Cohorts {
		add(c.CohortID)
		for _, u := range c.UnitIDs {
			add(u)
		}
		for _, l := range c.Layers {
			add(l.LayerID)
			for _, u := range l.UnitIDs {
				add(u)
			}
		}
	}
	for _, u := range plan.PrimaryUnits {
		add(u.UnitID)
		add(u.CohortID)
		add(u.LayerID)
		for _, h := range u.Hunks {
			add(h.PathID)
		}
	}
	for _, a := range plan.RequiredAudits {
		for _, target := range a.TargetIDs {
			add(target)
		}
	}
	for key, c := range citations {
		add(key)
		add(c.ID)
	}
	for key := range unitCandidates {
		add(key)
	}
	for key := range mandatoryUnits {
		add(key)
	}
	return set
}

// contentKindOf returns the content-ID kind of a well-formed public ID (prefix
// plus exactly 32 lowercase hex characters), or ok=false.
func contentKindOf(id string) (string, bool) {
	for kind := range contentIDKinds {
		if strings.HasPrefix(id, kind) && isLowerHex(id[len(kind):], 2*PublicIDContentBytesV1) {
			return kind, true
		}
	}
	return "", false
}

// ---- manifest I/O and integrity -------------------------------------------

// reviewIDs enumerates the valid review directory names through the pinned root,
// so the enumeration cannot be redirected by a swap of the reviews path.
func (s *PlanStore) reviewIDs() ([]string, error) {
	d, err := s.rootDir.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && validReviewID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// loadManifest reads a review manifest through the pinned reviews root with a
// confined, bounded, context-aware streaming decode and checks its integrity
// digest. The review id is validated before any path resolution or open.
func (s *PlanStore) loadManifest(ctx context.Context, reviewID string) (planManifest, error) {
	if !validReviewID(reviewID) {
		return planManifest{}, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	sub, err := s.rootDir.OpenRoot(reviewID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planManifest{}, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return planManifest{}, err
	}
	defer sub.Close()
	f, err := boundedio.OpenRegularNoFollow(sub, manifestName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planManifest{}, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return planManifest{}, err
	}
	defer f.Close()
	m, err := decodeManifest(ctx, f, s.maxManifest)
	if err != nil {
		return planManifest{}, err
	}
	stored := m.ContentDigest
	recomputed, err := manifestIntegrityDigest(m)
	if err != nil {
		return planManifest{}, err
	}
	if stored != recomputed {
		return planManifest{}, fmt.Errorf("%w: integrity digest mismatch", ErrManifestCorrupt)
	}
	return m, nil
}

// decodeManifest streams a manifest through a context-aware, byte-bounded reader
// into a JSON decoder, so cancellation is honored mid-read and mid-decode and an
// oversized manifest fails closed without buffering past the ceiling.
func decodeManifest(ctx context.Context, r io.Reader, max int64) (planManifest, error) {
	cr := &ctxLimitReader{ctx: ctx, r: r, remaining: max + 1}
	dec := json.NewDecoder(cr)
	var m planManifest
	if err := dec.Decode(&m); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return planManifest{}, ctxErr
		}
		if cr.exceeded || errors.Is(err, errManifestTooLarge) {
			return planManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrManifestCorrupt, max)
		}
		return planManifest{}, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
	}
	// Reject trailing bytes after the single manifest object, matching the strict
	// whole-document decode the store relied on before it streamed.
	if dec.More() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return planManifest{}, ctxErr
		}
		return planManifest{}, fmt.Errorf("%w: trailing data after manifest", ErrManifestCorrupt)
	}
	return m, nil
}

// ctxLimitReader wraps a reader with a per-read context check and a hard byte
// ceiling. It never reads past remaining bytes and reports an overrun through
// exceeded so the caller can distinguish a too-large manifest from a parse error.
type ctxLimitReader struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (c *ctxLimitReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if c.remaining <= 0 {
		c.exceeded = true
		return 0, errManifestTooLarge
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

// marshalManifest computes the integrity digest and returns the serialized bytes.
func marshalManifest(m planManifest) ([]byte, error) {
	digest, err := manifestIntegrityDigest(m)
	if err != nil {
		return nil, err
	}
	m.ContentDigest = digest
	return json.Marshal(m)
}

// manifestIntegrityDigest is the fixed SHA-256 of the manifest with an empty
// content-digest field. It is independent of the injectable ID digest so a
// test-injected digest cannot mask a tamper.
func manifestIntegrityDigest(m planManifest) (string, error) {
	m.ContentDigest = ""
	payload, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// writeRootedFile writes name under dir through a confined root.
func writeRootedFile(dir, name string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(name, data, 0o600)
}

// ---- locking and retention ------------------------------------------------

// lock acquires the store-wide mutation lock, failing with ErrPlanLocked when it
// is held elsewhere past the store's lock timeout.
func (s *PlanStore) lock(ctx context.Context) (func(), error) {
	fl := flock.New(s.lockPath)
	lockCtx, cancel := context.WithTimeout(ctx, s.lockTimeout)
	defer cancel()
	locked, err := fl.TryLockContext(lockCtx, 25*time.Millisecond)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrPlanLocked
		}
		return nil, err
	}
	if !locked {
		return nil, ErrPlanLocked
	}
	return func() { _ = fl.Unlock() }, nil
}

// reviewLockPath is the fixed hash-striped per-review lock file, kept outside the
// deletable review directory so a held lock survives the directory's removal
// window. A review maps deterministically to one of lockStripeCountV1 files, so
// a reader and the pruner of the same review share one stable inode and the
// store's lifetime lock-file count is bounded.
func (s *PlanStore) reviewLockPath(reviewID string) string {
	sum := sha256.Sum256([]byte(reviewID))
	stripe := binary.BigEndian.Uint32(sum[:4]) % lockStripeCountV1
	return filepath.Join(s.locksDir, fmt.Sprintf("%03d.lock", stripe))
}

// pruneLocked applies retention. The caller must hold the store lock.
func (s *PlanStore) pruneLocked(ctx context.Context, now time.Time) error {
	ids, err := s.reviewIDs()
	if err != nil {
		return err
	}
	type plan struct {
		id      string
		created int64
	}
	plans := make([]plan, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := s.loadManifest(ctx, id)
		if err != nil {
			// A corrupt or unreadable review is left in place; pruning never
			// destroys evidence it cannot understand.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		plans = append(plans, plan{id: id, created: m.CreatedAt})
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].created != plans[j].created {
			return plans[i].created > plans[j].created // newest first
		}
		return plans[i].id < plans[j].id
	})
	cutoff := now.Add(-retentionAgeV1).Unix()
	for i, p := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.created >= cutoff && i < retentionCountV1 {
			continue
		}
		if err := s.deleteReview(p.id); err != nil {
			return err
		}
	}
	return nil
}

// deleteReview removes a review directory while holding its striped lock through
// the removal, so a reader that already holds the lock is never deleted out from
// under. A lock held elsewhere (including by a reader of another review in the
// same stripe) leaves the plan in place until a later prune.
func (s *PlanStore) deleteReview(reviewID string) error {
	fl := flock.New(s.reviewLockPath(reviewID))
	locked, err := fl.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return nil // in use; never delete a locked plan
	}
	defer fl.Unlock()
	if err := os.RemoveAll(filepath.Join(s.root, reviewID)); err != nil {
		return err
	}
	// The striped lock file itself is retained (bounded by the stripe count) so
	// an in-flight reader's fd stays valid.
	return nil
}
