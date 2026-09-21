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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/neur0map/prowl/internal/boundedio"
	contextpacket "github.com/neur0map/prowl/internal/context"
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
	// PlanIdentity is the full structured identity whose ReviewPlanIdentityV1
	// encoding must equal PlanIdentityBytes byte-for-byte and whose plan-bearing
	// fields the store binds to the persisted Plan and ID records.
	PlanIdentity PlanIdentity
	// IDRecords covers every content-derived public ID in the plan/artifacts.
	IDRecords []IDRecord
	// WorkspaceFingerprint is the workspace head fingerprint for a workspace
	// plan; it is empty for an immutable committed or range plan.
	WorkspaceFingerprint string
	// WorkspaceCaptureFingerprint binds the complete accepted canonical capture,
	// including deletion-only path identity. It is empty for immutable scopes.
	WorkspaceCaptureFingerprint string
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
	Schema                      string                               `json:"schema"`
	ReviewID                    string                               `json:"review_id"`
	PlanDigest                  string                               `json:"plan_digest"`
	ScopeKind                   ScopeKind                            `json:"scope_kind"`
	WorkspaceFingerprint        string                               `json:"workspace_fingerprint,omitempty"`
	WorkspaceCaptureFingerprint string                               `json:"workspace_capture_fingerprint,omitempty"`
	CreatedAt                   int64                                `json:"created_at"`
	Plan                        Plan                                 `json:"plan"`
	UnitCandidates              map[string][]contextpacket.Candidate `json:"unit_candidates,omitempty"`
	MandatoryUnits              map[string][]byte                    `json:"mandatory_units,omitempty"`
	Citations                   map[string]CitationProof             `json:"citations,omitempty"`
	PublishedIndexSignature     string                               `json:"published_index_signature,omitempty"`
	PlanIdentityBytes           []byte                               `json:"plan_identity_bytes"`
	PlanIdentity                PlanIdentity                         `json:"plan_identity"`
	IDRecords                   []IDRecord                           `json:"id_records,omitempty"`
	ContentDigest               string                               `json:"content_digest"`
}

// SnapshotLease is a store-owned directory beneath <common>/prowl/snapshots that
// a caller materializes into and then hands to Save. Only the store can mint a
// lease; its backing path is immutable from outside (exposed read-only via Dir)
// so it can never be redirected into an arbitrary destructive path. Save and
// Release both revalidate the lease (store-owned, direct child of the snapshots
// root, no-follow real directory) before consuming or deleting it. An unconsumed
// lease is the caller's to Release.
type SnapshotLease struct {
	dir      string
	store    *PlanStore
	consumed bool
}

// Dir is the directory the caller materializes its snapshot into. It is
// read-only: the lease's backing path cannot be reassigned by a caller.
func (l *SnapshotLease) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Release validates and deletes an unconsumed lease directory through the
// store's confined snapshots root (no-follow). It is a no-op once the lease has
// been consumed by a successful Save, and it refuses to delete a lease that is
// not a genuine store-owned directory.
func (l *SnapshotLease) Release() error {
	if l == nil || l.consumed {
		return nil
	}
	if l.store == nil {
		return fmt.Errorf("%w: lease has no owning store", ErrSnapshotLease)
	}
	if err := l.store.validateLease(l); err != nil {
		return err
	}
	l.consumed = true
	return l.store.snapshotsRoot.RemoveAll(filepath.Base(l.dir))
}

// PlanStore persists plans beneath the Git common directory. It is safe to open
// from any linked worktree; every instance shares one collision and retention
// domain.
type PlanStore struct {
	root         string
	rootDir      *os.Root
	locksDir     string
	snapshotsDir string
	// snapshotsRoot pins the snapshots directory so lease validation, release, and
	// consumption operate relative to a fixed capability that a swap or symlink of
	// the absolute snapshots path cannot redirect.
	snapshotsRoot *os.Root
	lockPath      string
	lockTimeout   time.Duration
	maxManifest   int64
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
	// renameFn performs the cross-root snapshot moves (lease->temp and the
	// rollback restore); injectable so a test can force a restore failure and
	// prove the sole snapshot is preserved rather than deleted.
	renameFn func(oldpath, newpath string) error
	// beforeMarshal, when set, is called immediately before the manifest is
	// marshaled; a test uses it to prove an over-ceiling artifact is rejected by
	// the preflight budget before any marshal/allocation is reached.
	beforeMarshal func()
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
	prowlDir := filepath.Join(common, "prowl")
	root := filepath.Join(prowlDir, reviewsSubdir)
	// Lock files and snapshots live as siblings of the reviews root (not inside
	// it), so a swap or symlink-replacement of the reviews directory cannot
	// redirect a lock or snapshot write, and reader/pruner locks stay stable.
	locks := filepath.Join(prowlDir, locksSubdir)
	snapshots := filepath.Join(prowlDir, snapshotsSubdir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
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
	snapshotsRoot, err := os.OpenRoot(snapshots)
	if err != nil {
		_ = rootDir.Close()
		return nil, err
	}
	return &PlanStore{
		root:          root,
		rootDir:       rootDir,
		locksDir:      locks,
		snapshotsDir:  snapshots,
		snapshotsRoot: snapshotsRoot,
		lockPath:      filepath.Join(prowlDir, storeLockName),
		lockTimeout:   defaultLockTimeout,
		maxManifest:   maxManifestBytesV1,
		digest:        func(b []byte) Digest { return sha256.Sum256(b) },
		now:           time.Now,
		sameDevice:    sameDevice,
		renameFn:      os.Rename,
	}, nil
}

// Root returns the review-state root directory.
func (s *PlanStore) Root() string { return s.root }

// Close releases the pinned reviews-root and snapshots-root descriptors.
func (s *PlanStore) Close() error {
	var err error
	if s.rootDir != nil {
		err = s.rootDir.Close()
		s.rootDir = nil
	}
	if s.snapshotsRoot != nil {
		if cerr := s.snapshotsRoot.Close(); cerr != nil && err == nil {
			err = cerr
		}
		s.snapshotsRoot = nil
	}
	return err
}

// NewSnapshotLease mints a fresh store-owned snapshot directory under the store's
// snapshots root. The caller materializes into lease.Dir() and then hands the
// lease to Save (or Release()s it on an abandoned attempt).
func (s *PlanStore) NewSnapshotLease(ctx context.Context) (*SnapshotLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Create the lease through the pinned snapshots capability with a unique name,
	// so the directory is minted on the fixed inode a later validation/consume/
	// release resolves, not via a swappable absolute path.
	for range 100 {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := leasePrefix + hex.EncodeToString(b[:])
		err := s.snapshotsRoot.Mkdir(name, 0o700)
		if err == nil {
			return &SnapshotLease{dir: filepath.Join(s.snapshotsDir, name), store: s}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return nil, errors.New("review: could not create a unique snapshot lease directory")
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
	if err := verifyWorkspaceCaptureFingerprint(artifacts.Plan, artifacts.WorkspaceCaptureFingerprint); err != nil {
		return SaveResult{}, err
	}
	reviewID := artifacts.Plan.ReviewID

	release, err := s.lock(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	defer release()

	if existing, err := s.loadManifest(ctx, reviewID); err == nil {
		existingFull := sha256.Sum256(existing.PlanIdentityBytes)
		if existingFull == full {
			// Identical immutable plan: reuse only after the existing manifest
			// passes the same full verification a fresh Save would, so a corrupt
			// or invalid persisted plan is never silently accepted.
			if verr := s.verifyManifest(ctx, existing); verr != nil {
				return SaveResult{}, verr
			}
			// Delete the redundant incoming lease; a bad lease surfaces its error.
			if rerr := lease.Release(); rerr != nil {
				return SaveResult{}, rerr
			}
			return SaveResult{ReviewID: reviewID, Reused: true}, nil
		}
		return SaveResult{}, fmt.Errorf("%w: review %s already persisted with a different identity", ErrIDCollision, reviewID)
	} else if !errors.Is(err, ErrReviewNotFound) {
		return SaveResult{}, err
	}

	// Incrementally bound every manifest content contributor (identity bytes,
	// records, mandatory units, citations, unit candidates, and the plan) with
	// overflow-safe remaining checks before marshaling, so Save never allocates or
	// persists a review its own Load would reject as oversized. The post-marshal
	// length check below remains a defense-in-depth backstop.
	if err := boundManifestContent(artifacts, s.maxManifest); err != nil {
		return SaveResult{}, err
	}

	registry, err := s.buildDomainRegistry(ctx, reviewID)
	if err != nil {
		return SaveResult{}, err
	}
	if _, err := addPlanToRegistry(ctx, registry, s.digest, artifacts.Plan, artifacts.Citations, artifacts.UnitCandidates, artifacts.MandatoryUnits, artifacts.IDRecords, full); err != nil {
		return SaveResult{}, err
	}
	// Bind the canonical identity bytes to this plan and validate its mandatory
	// unit payloads (only for the plan being saved, not the domain siblings).
	if err := verifyArtifactIntegrity(artifacts.Plan, artifacts.IDRecords, artifacts.PlanIdentity, artifacts.PlanIdentityBytes, artifacts.PublishedIndexSignature, artifacts.MandatoryUnits); err != nil {
		return SaveResult{}, err
	}

	manifest := planManifest{
		Schema:                      planStoreSchemaV1,
		ReviewID:                    reviewID,
		PlanDigest:                  artifacts.Plan.PlanDigest,
		ScopeKind:                   artifacts.Plan.Scope.Kind,
		WorkspaceFingerprint:        artifacts.WorkspaceFingerprint,
		WorkspaceCaptureFingerprint: artifacts.WorkspaceCaptureFingerprint,
		CreatedAt:                   s.now().Unix(),
		Plan:                        artifacts.Plan,
		UnitCandidates:              artifacts.UnitCandidates,
		MandatoryUnits:              artifacts.MandatoryUnits,
		Citations:                   artifacts.Citations,
		PublishedIndexSignature:     artifacts.PublishedIndexSignature,
		PlanIdentityBytes:           artifacts.PlanIdentityBytes,
		PlanIdentity:                artifacts.PlanIdentity,
		IDRecords:                   artifacts.IDRecords,
	}
	if s.beforeMarshal != nil {
		s.beforeMarshal()
	}
	payload, err := marshalManifest(manifest)
	if err != nil {
		return SaveResult{}, err
	}
	if int64(len(payload)) > s.maxManifest {
		return SaveResult{}, fmt.Errorf("%w: manifest is %d bytes, exceeds the %d-byte ceiling", ErrManifestCorrupt, len(payload), s.maxManifest)
	}
	if err := s.publish(reviewID, payload, lease); err != nil {
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

// publish writes the manifest into a temp directory created through the pinned
// reviews root, validates and moves the caller's leased snapshot into it, then
// atomically renames it to the review directory - all relative to the pinned
// descriptor, so a swap of the reviews path can neither split reads from writes
// nor redirect the rename outside the owned root. A pre-publication failure
// restores the caller's lease; if that restore itself fails, the sole snapshot is
// preserved in the temp tree and the rollback failure is surfaced rather than
// deleting the only copy.
func (s *PlanStore) publish(reviewID string, payload []byte, lease *SnapshotLease) (err error) {
	tmpName, err := s.mkdirTemp()
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
			// Restore the caller's lease before removing the temp tree. If the
			// restore fails, keep the temp tree (holding the only snapshot) and
			// surface the rollback failure instead of destroying the sole copy.
			src := filepath.Join(s.root, tmpName, snapshotName)
			if rerr := s.renameFn(src, lease.dir); rerr != nil {
				err = errors.Join(err, fmt.Errorf("%w: snapshot preserved at %s; rollback failed: %v", ErrSnapshotOwnership, src, rerr))
				return
			}
			lease.consumed = false
		}
		_ = s.rootDir.RemoveAll(tmpName)
	}()
	tr, err := s.rootDir.OpenRoot(tmpName)
	if err != nil {
		return err
	}
	werr := tr.WriteFile(manifestName, payload, 0o600)
	tr.Close()
	if werr != nil {
		return werr
	}
	if lease != nil {
		// consumeLease may move the source before a post-move check fails; capture
		// moved even on error so the defer restores/preserves the sole snapshot.
		var cerr error
		moved, cerr = s.consumeLease(lease, tmpName)
		if cerr != nil {
			return cerr
		}
	}
	if rerr := s.rootDir.Rename(tmpName, reviewID); rerr != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotOwnership, rerr)
	}
	committed = true
	return nil
}

// mkdirTemp creates a uniquely-named temp directory through the pinned reviews
// root, so it is created on the same inode reads and the final rename use.
func (s *PlanStore) mkdirTemp() (string, error) {
	for range 100 {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := ".tmp-save-" + hex.EncodeToString(b[:])
		err := s.rootDir.Mkdir(name, 0o700)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("review: could not create a unique temp directory")
}

// validateLease proves a lease is a genuine store-owned directory: minted by this
// store, unconsumed, a direct lease-prefixed child of the snapshots root, and a
// real directory reached without a symlink. It never follows or deletes.
func (s *PlanStore) validateLease(lease *SnapshotLease) error {
	if lease == nil {
		return fmt.Errorf("%w: nil lease", ErrSnapshotLease)
	}
	if lease.store != s {
		return fmt.Errorf("%w: lease was not issued by this store", ErrSnapshotLease)
	}
	if lease.consumed {
		return fmt.Errorf("%w: lease already consumed", ErrSnapshotLease)
	}
	if filepath.Dir(lease.dir) != s.snapshotsDir {
		return fmt.Errorf("%w: %q is not under the store snapshots root", ErrSnapshotLease, lease.dir)
	}
	base := filepath.Base(lease.dir)
	if !strings.HasPrefix(base, leasePrefix) {
		return fmt.Errorf("%w: %q is not a lease directory", ErrSnapshotLease, lease.dir)
	}
	info, err := s.snapshotsRoot.Lstat(base)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotLease, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %q is not a real directory", ErrSnapshotLease, lease.dir)
	}
	return nil
}

// consumeLease validates a store-owned lease, requires it be on the same device
// as the review root, moves it into <tmpName>/snapshot, and then re-verifies the
// published entry with a no-follow stat through the pinned root, so a source
// swapped for a symlink between validation and the move cannot be published.
func (s *PlanStore) consumeLease(lease *SnapshotLease, tmpName string) (moved bool, err error) {
	if err := s.validateLease(lease); err != nil {
		return false, err
	}
	same, err := s.sameDevice(s.root, lease.dir)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSnapshotLease, err)
	}
	if !same {
		return false, fmt.Errorf("%w: %q is on a different device", ErrSnapshotLease, lease.dir)
	}
	dest := filepath.Join(s.root, tmpName, snapshotName)
	if err := s.renameFn(lease.dir, dest); err != nil {
		return false, fmt.Errorf("%w: %v", ErrSnapshotOwnership, err)
	}
	// The source has been moved; report moved=true from here on so a caller can
	// restore/preserve it and never delete the sole snapshot on a later failure.
	lease.consumed = true
	tr, err := s.rootDir.OpenRoot(tmpName)
	if err != nil {
		return true, err
	}
	defer tr.Close()
	info, err := tr.Lstat(snapshotName)
	if err != nil {
		return true, fmt.Errorf("%w: %v", ErrSnapshotLease, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return true, fmt.Errorf("%w: published snapshot is not a real directory", ErrSnapshotLease)
	}
	return true, nil
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
		Plan:                        manifest.Plan,
		UnitCandidates:              manifest.UnitCandidates,
		MandatoryUnits:              manifest.MandatoryUnits,
		Citations:                   manifest.Citations,
		PublishedIndexSignature:     manifest.PublishedIndexSignature,
		PlanIdentityBytes:           manifest.PlanIdentityBytes,
		PlanIdentity:                manifest.PlanIdentity,
		IDRecords:                   manifest.IDRecords,
		WorkspaceCaptureFingerprint: manifest.WorkspaceCaptureFingerprint,
		WorkspaceFingerprint:        manifest.WorkspaceFingerprint,
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

func verifyWorkspaceCaptureFingerprint(plan Plan, fingerprint string) error {
	if plan.Scope.Kind == ScopeWorkspace {
		if !isCanonicalDigestHex(fingerprint) {
			return fmt.Errorf("%w: workspace capture fingerprint is not canonical SHA-256", ErrWorkspaceFingerprint)
		}
		return nil
	}
	if fingerprint != "" {
		return fmt.Errorf("%w: committed/range plan must not carry a workspace capture fingerprint", ErrWorkspaceFingerprint)
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
	if err := verifyWorkspaceCaptureFingerprint(m.Plan, m.WorkspaceCaptureFingerprint); err != nil {
		return err
	}
	registry, err := s.buildDomainRegistry(ctx, m.ReviewID)
	if err != nil {
		return err
	}
	if _, err := addPlanToRegistry(ctx, registry, s.digest, m.Plan, m.Citations, m.UnitCandidates, m.MandatoryUnits, m.IDRecords, full); err != nil {
		return err
	}
	if err := verifyArtifactIntegrity(m.Plan, m.IDRecords, m.PlanIdentity, m.PlanIdentityBytes, m.PublishedIndexSignature, m.MandatoryUnits); err != nil {
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
	enumerated, err := collectPlanIDs(plan, citations, unitCandidates, mandatoryUnits)
	if err != nil {
		return nil, err
	}
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

// collectPlanIDs collects every content-derived public ID the plan, its unit
// candidates, mandatory units, and citations reference, enforcing the exact kind
// each field must carry and rejecting any malformed or wrong-kind reference so a
// field can never smuggle an ID of another kind past the registry.
func collectPlanIDs(plan Plan, citations map[string]CitationProof, unitCandidates map[string][]contextpacket.Candidate, mandatoryUnits map[string][]byte) (map[string]struct{}, error) {
	set := map[string]struct{}{}
	addKind := func(id, kind string) error {
		k, ok := contentKindOf(id)
		if !ok {
			return fmt.Errorf("%w: malformed content id %q", ErrIDRegistry, id)
		}
		if k != kind {
			return fmt.Errorf("%w: id %q is kind %q, expected %q", ErrIDRegistry, id, k, kind)
		}
		set[id] = struct{}{}
		return nil
	}
	addContent := func(id string) error {
		if _, ok := contentKindOf(id); !ok {
			return fmt.Errorf("%w: malformed content id %q", ErrIDRegistry, id)
		}
		set[id] = struct{}{}
		return nil
	}
	addOneOf := func(id string, kinds ...string) error {
		k, ok := contentKindOf(id)
		if !ok {
			return fmt.Errorf("%w: malformed content id %q", ErrIDRegistry, id)
		}
		for _, want := range kinds {
			if k == want {
				set[id] = struct{}{}
				return nil
			}
		}
		return fmt.Errorf("%w: id %q is kind %q, not one of %v", ErrIDRegistry, id, k, kinds)
	}
	for _, p := range plan.ChangedPaths {
		if err := addKind(p.PathID, PathIDPrefixV1); err != nil {
			return nil, err
		}
	}
	for _, c := range plan.Cohorts {
		if err := addKind(c.CohortID, CohortIDPrefixV1); err != nil {
			return nil, err
		}
		for _, u := range c.UnitIDs {
			if err := addKind(u, UnitIDPrefixV1); err != nil {
				return nil, err
			}
		}
		for _, l := range c.Layers {
			if err := addKind(l.LayerID, LayerIDPrefixV1); err != nil {
				return nil, err
			}
			for _, u := range l.UnitIDs {
				if err := addKind(u, UnitIDPrefixV1); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, u := range plan.PrimaryUnits {
		if err := addKind(u.UnitID, UnitIDPrefixV1); err != nil {
			return nil, err
		}
		if err := addKind(u.CohortID, CohortIDPrefixV1); err != nil {
			return nil, err
		}
		if err := addKind(u.LayerID, LayerIDPrefixV1); err != nil {
			return nil, err
		}
		for _, h := range u.Hunks {
			if err := addKind(h.PathID, PathIDPrefixV1); err != nil {
				return nil, err
			}
		}
	}
	for _, a := range plan.RequiredAudits {
		for _, target := range a.TargetIDs {
			if err := addOneOf(target, HunkIDPrefixV1, PathIDPrefixV1, CohortIDPrefixV1, UnitIDPrefixV1, TargetIDPrefixV1); err != nil {
				return nil, err
			}
		}
	}
	for key, c := range citations {
		if err := addContent(key); err != nil {
			return nil, err
		}
		if err := addContent(c.ID); err != nil {
			return nil, err
		}
	}
	for key := range unitCandidates {
		if err := addKind(key, UnitIDPrefixV1); err != nil {
			return nil, err
		}
	}
	for key := range mandatoryUnits {
		if err := addKind(key, UnitIDPrefixV1); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// verifyArtifactIntegrity binds the canonical identity bytes to the persisted
// plan and records, binds the served index signature to the identity head index
// signature, and validates the mandatory unit payloads. It is run only for the
// plan being saved or loaded (never for unrelated domain siblings).
func verifyArtifactIntegrity(plan Plan, records []IDRecord, pi PlanIdentity, identityBytes []byte, publishedIndexSignature string, mandatory map[string][]byte) error {
	if err := verifyIdentityBinding(plan, records, pi, identityBytes); err != nil {
		return err
	}
	// The published index signature is stored and served alongside the plan; bind
	// it to the identity's head index signature so a swapped signature cannot ride
	// on an otherwise-valid review id.
	if string(pi.HeadIndexSignature) != publishedIndexSignature {
		return fmt.Errorf("%w: published index signature disagrees with the identity head index signature", ErrPlanIdentityMismatch)
	}
	return validateMandatoryUnits(plan, mandatory)
}

// verifyIdentityBinding proves the persisted identity describes exactly this plan.
// It first requires the canonical ReviewPlanIdentityV1 encoding of the persisted
// PlanIdentity struct to equal PlanIdentityBytes byte-for-byte (so the review id,
// which is the hash of those bytes, names precisely this structured identity).
// It then binds every plan-bearing field of that struct, in order, to the Plan
// and its exact ID records: paths (id + review_class + coverage + reason +
// role_ids, the p_ record's old/new/status, and the scope digest embedded in
// each p_ record); the plan scope digest, recomputed from Plan.Scope and the
// canonical patch of the record frames; primary units (unit id plus each owned
// hunk, whose id is recomputed from the unit's exact patch bytes); structured
// cohorts against the displayed cohort/layer collections; direct cohorts against
// PrimaryUnits' retained cohort/layer relationships; and audits (audit_id +
// target ids). Finally, it requires the full set of content-ID digests the
// struct embeds to equal the record full set, binding hunks and rejecting extras.
func verifyIdentityBinding(plan Plan, records []IDRecord, pi PlanIdentity, identityBytes []byte) error {
	if !bytes.Equal(ReviewPlanIdentityV1(pi), identityBytes) {
		return fmt.Errorf("%w: identity bytes are not the canonical encoding of the identity struct", ErrPlanIdentityMismatch)
	}
	byPublic := make(map[string]IDRecord, len(records))
	for _, r := range records {
		byPublic[r.Public] = r
	}
	fullOf := func(public string) (Digest, error) {
		r, ok := byPublic[public]
		if !ok {
			return Digest{}, fmt.Errorf("%w: plan references %s with no identity record", ErrIDRegistry, public)
		}
		return r.Full, nil
	}
	wantFull := func(id StableID, public string) error {
		want, err := fullOf(public)
		if err != nil {
			return err
		}
		if id.Full != want {
			return fmt.Errorf("%w: identity digest for %s disagrees with its record", ErrPlanIdentityMismatch, public)
		}
		return nil
	}

	// paths: ordered, every plan-bearing field plus the record old/new/status.
	if len(pi.Paths) != len(plan.ChangedPaths) {
		return fmt.Errorf("%w: identity has %d paths, plan has %d", ErrPlanIdentityMismatch, len(pi.Paths), len(plan.ChangedPaths))
	}
	// patchRows collects each changed path's canonical record frame (in identity
	// order) so the plan scope digest can be recomputed from the plan and records.
	type patchRow struct {
		newPath, oldPath, status string
		frame                    []byte
	}
	var patchRows []patchRow
	for i, e := range pi.Paths {
		cp := plan.ChangedPaths[i]
		if err := wantFull(e.PathID, cp.PathID); err != nil {
			return err
		}
		if e.ReviewClass != cp.ReviewClass || e.Coverage != cp.Coverage || e.Reason != cp.Reason || !stringSlicesEqual(e.RoleIDs, cp.Roles) {
			return fmt.Errorf("%w: changed path %s class/coverage/reason/roles disagree with the identity", ErrPlanIdentityMismatch, cp.PathID)
		}
		rf, err := decodeFramedFields(byPublic[cp.PathID].Canonical)
		if err != nil {
			return fmt.Errorf("%w: undecodable path record %s: %v", ErrPlanIdentityMismatch, cp.PathID, err)
		}
		rec, err := decodeFramedFields(rf["record"])
		if err != nil {
			return fmt.Errorf("%w: undecodable path record body %s: %v", ErrPlanIdentityMismatch, cp.PathID, err)
		}
		if string(rec["new_path"]) != cp.NewPath || string(rec["old_path"]) != cp.OldPath || string(rec["status"]) != cp.Status {
			return fmt.Errorf("%w: changed path %s disagrees with its identity record", ErrPlanIdentityMismatch, cp.PathID)
		}
		if !bytes.Equal(rf["scope"], pi.ScopeDigest[:]) {
			return fmt.Errorf("%w: changed path %s binds a different scope than the identity", ErrPlanIdentityMismatch, cp.PathID)
		}
		patchRows = append(patchRows, patchRow{
			newPath: string(rec["new_path"]), oldPath: string(rec["old_path"]), status: string(rec["status"]),
			frame: rf["record"],
		})
	}

	// Recompute the canonical patch from the ordered record frames and bind the
	// resulting scope digest to the identity, so a mutated Plan.Scope (base, head,
	// object format, or kind) or a swapped patch is rejected.
	sort.SliceStable(patchRows, func(i, j int) bool {
		if patchRows[i].newPath != patchRows[j].newPath {
			return patchRows[i].newPath < patchRows[j].newPath
		}
		if patchRows[i].oldPath != patchRows[j].oldPath {
			return patchRows[i].oldPath < patchRows[j].oldPath
		}
		return patchRows[i].status < patchRows[j].status
	})
	patchFrames := make([][]byte, len(patchRows))
	for i := range patchRows {
		patchFrames[i] = patchRows[i].frame
	}
	if ScopeDigest(plan.Scope.ObjectFormat, plan.Scope.Base, plan.Scope.Head, plan.Scope.Kind, sha256.Sum256(FrameList(patchFrames...))) != pi.ScopeDigest {
		return fmt.Errorf("%w: identity scope digest disagrees with the plan scope and canonical patch", ErrPlanIdentityMismatch)
	}

	// primary units: ordered unit id.
	if len(pi.Units) != len(plan.PrimaryUnits) {
		return fmt.Errorf("%w: identity has %d units, plan has %d", ErrPlanIdentityMismatch, len(pi.Units), len(plan.PrimaryUnits))
	}
	for i, e := range pi.Units {
		u := plan.PrimaryUnits[i]
		if err := wantFull(e.UnitID, u.UnitID); err != nil {
			return err
		}
		// Bind each identity hunk id to the primary unit's owned hunk, recomputing
		// the hunk id from the unit's exact patch bytes so mutated patch content is
		// rejected even when the unit id is unchanged.
		if len(e.HunkIDs) != len(u.Hunks) {
			return fmt.Errorf("%w: identity unit %s has %d hunks, plan has %d", ErrPlanIdentityMismatch, u.UnitID, len(e.HunkIDs), len(u.Hunks))
		}
		for j, hid := range e.HunkIDs {
			uh := u.Hunks[j]
			pathFull, err := fullOf(uh.PathID)
			if err != nil {
				return err
			}
			payload, err := base64.StdEncoding.DecodeString(uh.PatchBase64)
			if err != nil {
				return fmt.Errorf("%w: unit %s hunk %d patch is not valid base64", ErrPlanIdentityMismatch, u.UnitID, j)
			}
			raw := RawHunk{
				Ordinal:           uint64(uh.Ordinal),
				OldStart:          uint64(uh.OldStart),
				OldLines:          uint64(uh.OldCount),
				NewStart:          uint64(uh.NewStart),
				NewLines:          uint64(uh.NewCount),
				NoFinalNewlineOld: uh.NoFinalNewlineOld,
				NoFinalNewlineNew: uh.NoFinalNewlineNew,
				Payload:           payload,
			}
			if HunkID(pi.ScopeDigest, pathFull, raw).Full != hid.Full {
				return fmt.Errorf("%w: unit %s hunk %d identity disagrees with its patch bytes", ErrPlanIdentityMismatch, u.UnitID, j)
			}
			if uh.HunkID != hid.Public {
				return fmt.Errorf("%w: unit %s hunk %d exposes hunk_id %s, want %s", ErrPlanIdentityMismatch, u.UnitID, j, uh.HunkID, hid.Public)
			}
			if err := wantFull(hid, uh.HunkID); err != nil {
				return err
			}
		}
	}

	// Structured plans expose their complete cohort/layer matrix, so bind every
	// identity row to that display collection exactly. Direct plans intentionally
	// omit the display matrix; bind their retained partition rows to the ordered
	// PrimaryUnits instead.
	if plan.Mode == ModeDirect {
		if err := verifyDirectIdentityCohorts(plan.PrimaryUnits, pi.Cohorts, records, fullOf, wantFull); err != nil {
			return err
		}
	} else {
		type flatCohort struct {
			cohort string
			layer  string
			units  []string
		}
		var flat []flatCohort
		for _, c := range plan.Cohorts {
			for _, l := range c.Layers {
				flat = append(flat, flatCohort{cohort: c.CohortID, layer: l.LayerID, units: l.UnitIDs})
			}
		}
		if len(pi.Cohorts) != len(flat) {
			return fmt.Errorf("%w: identity has %d cohort rows, plan flattens to %d", ErrPlanIdentityMismatch, len(pi.Cohorts), len(flat))
		}
		for i, e := range pi.Cohorts {
			if err := wantFull(e.CohortID, flat[i].cohort); err != nil {
				return err
			}
			if err := wantFull(e.LayerID, flat[i].layer); err != nil {
				return err
			}
			if len(e.UnitIDs) != len(flat[i].units) {
				return fmt.Errorf("%w: identity cohort %d member count disagrees with the plan", ErrPlanIdentityMismatch, i)
			}
			for j, u := range e.UnitIDs {
				if err := wantFull(u, flat[i].units[j]); err != nil {
					return err
				}
			}
		}
	}

	// audits: ordered audit id and target ids.
	if len(pi.Audits) != len(plan.RequiredAudits) {
		return fmt.Errorf("%w: identity has %d audits, plan has %d", ErrPlanIdentityMismatch, len(pi.Audits), len(plan.RequiredAudits))
	}
	for i, e := range pi.Audits {
		a := plan.RequiredAudits[i]
		if e.AuditID != a.AuditID {
			return fmt.Errorf("%w: identity audit %d id disagrees with the plan", ErrPlanIdentityMismatch, i)
		}
		if len(e.TargetIDs) != len(a.TargetIDs) {
			return fmt.Errorf("%w: identity audit %d target count disagrees with the plan", ErrPlanIdentityMismatch, i)
		}
		for j, t := range e.TargetIDs {
			if err := wantFull(t, a.TargetIDs[j]); err != nil {
				return err
			}
		}
	}

	// full content-digest set equality (binds hunks and rejects extras).
	embedded := map[Digest]bool{}
	for _, e := range pi.Paths {
		embedded[e.PathID.Full] = true
	}
	for _, e := range pi.Hunks {
		embedded[e.HunkID.Full] = true
	}
	for _, e := range pi.Units {
		embedded[e.UnitID.Full] = true
	}
	for _, e := range pi.Cohorts {
		embedded[e.CohortID.Full] = true
		embedded[e.LayerID.Full] = true
	}
	for _, e := range pi.Audits {
		for _, t := range e.TargetIDs {
			embedded[t.Full] = true
		}
	}
	recordFulls := make(map[Digest]bool, len(records))
	for _, r := range records {
		recordFulls[r.Full] = true
	}
	if len(embedded) != len(recordFulls) {
		return fmt.Errorf("%w: identity embeds %d content ids, records carry %d", ErrPlanIdentityMismatch, len(embedded), len(recordFulls))
	}
	for d := range recordFulls {
		if !embedded[d] {
			return fmt.Errorf("%w: a record digest is absent from the identity", ErrPlanIdentityMismatch)
		}
	}
	return nil
}

func verifyDirectIdentityCohorts(primary []Unit, rows []PlanCohortEntry, records []IDRecord, fullOf func(string) (Digest, error), wantFull func(StableID, string) error) error {
	type partition struct {
		cohort Digest
		layer  Digest
	}
	hasRecord := func(kind string, full Digest) bool {
		for _, record := range records {
			if record.Kind == kind && record.Full == full {
				return true
			}
		}
		return false
	}
	seen := make(map[partition]bool, len(rows))
	unitIndex := 0
	for rowIndex, row := range rows {
		key := partition{cohort: row.CohortID.Full, layer: row.LayerID.Full}
		if seen[key] {
			return fmt.Errorf("%w: direct identity repeats cohort/layer row %d", ErrPlanIdentityMismatch, rowIndex)
		}
		seen[key] = true
		if !hasRecord(CohortIDPrefixV1, key.cohort) || !hasRecord(LayerIDPrefixV1, key.layer) {
			return fmt.Errorf("%w: direct identity cohort row %d has no matching cohort/layer records", ErrPlanIdentityMismatch, rowIndex)
		}
		for _, unitID := range row.UnitIDs {
			if unitIndex >= len(primary) {
				return fmt.Errorf("%w: direct identity has more partitioned units than the plan", ErrPlanIdentityMismatch)
			}
			unit := primary[unitIndex]
			if err := wantFull(unitID, unit.UnitID); err != nil {
				return err
			}
			cohort, err := fullOf(unit.CohortID)
			if err != nil {
				return err
			}
			layer, err := fullOf(unit.LayerID)
			if err != nil {
				return err
			}
			if cohort != key.cohort || layer != key.layer {
				return fmt.Errorf("%w: direct unit %s cohort/layer disagrees with identity row %d", ErrPlanIdentityMismatch, unit.UnitID, rowIndex)
			}
			unitIndex++
		}
	}
	if unitIndex != len(primary) {
		return fmt.Errorf("%w: direct identity partitions %d units, plan has %d", ErrPlanIdentityMismatch, unitIndex, len(primary))
	}
	return nil
}

// stringSlicesEqual reports whether two string slices are element-wise equal.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// validateMandatoryUnits requires the mandatory-unit key set to equal the plan's
// primary-unit set exactly - one mandatory object per primary unit, no missing
// and no extra keys - and each payload to be byte-equal to the corresponding
// primary unit's CanonicalMandatoryJSON, so the persisted mandatory objects are
// exactly the plan-bound, already-validated units and nothing else.
func validateMandatoryUnits(plan Plan, mandatory map[string][]byte) error {
	primary := make(map[string]Unit, len(plan.PrimaryUnits))
	for _, u := range plan.PrimaryUnits {
		primary[u.UnitID] = u
	}
	if len(mandatory) != len(primary) {
		return fmt.Errorf("%w: %d mandatory units for %d primary units", ErrManifestCorrupt, len(mandatory), len(primary))
	}
	for id, u := range primary {
		raw, ok := mandatory[id]
		if !ok {
			return fmt.Errorf("%w: primary unit %s has no mandatory object", ErrManifestCorrupt, id)
		}
		canonical, err := u.CanonicalMandatoryJSON()
		if err != nil {
			return fmt.Errorf("%w: mandatory unit %s: %v", ErrManifestCorrupt, id, err)
		}
		if !bytes.Equal(raw, canonical) {
			return fmt.Errorf("%w: mandatory unit %s is not byte-equal to its primary unit canonical form", ErrManifestCorrupt, id)
		}
	}
	return nil
}

// decodeFramedFields parses a FramedFieldsV1 record (a sequence of
// uint16be(name_len)|name|uint64be(value_len)|value) into a name->value map.
func decodeFramedFields(b []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, errors.New("truncated field name length")
		}
		nl := int(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
		if nl == 0 || len(b) < nl {
			return nil, errors.New("truncated field name")
		}
		name := string(b[:nl])
		b = b[nl:]
		if len(b) < 8 {
			return nil, errors.New("truncated value length")
		}
		vl := binary.BigEndian.Uint64(b[:8])
		b = b[8:]
		if vl > uint64(len(b)) {
			return nil, errors.New("truncated value")
		}
		out[name] = b[:vl]
		b = b[vl:]
	}
	return out, nil
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
// oversized manifest fails closed without buffering past the ceiling. It rejects
// any trailing bytes by requiring the next decode to be io.EOF, and rejects a
func decodeManifest(ctx context.Context, r io.Reader, max int64) (planManifest, error) {
	cr := &ctxLimitReader{ctx: ctx, r: r, remaining: max + 1}
	dec := json.NewDecoder(cr)
	var m planManifest
	if err := dec.Decode(&m); err != nil {
		return planManifest{}, manifestDecodeError(ctx, cr, err)
	}
	// Require exactly one JSON document: the next decode must be a clean io.EOF.
	// json.Decoder.More cannot be trusted here - it hides reader errors and treats
	// a stray delimiter as end-of-stream - so decode again and demand io.EOF.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return planManifest{}, fmt.Errorf("%w: trailing data after manifest", ErrManifestCorrupt)
		}
		return planManifest{}, manifestDecodeError(ctx, cr, err)
	}
	if cr.exceeded {
		return planManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrManifestCorrupt, max)
	}
	return m, nil
}

// manifestDecodeError maps a decoder error to a typed cause: cancellation, an
// over-ceiling read, or a generic corruption.
func manifestDecodeError(ctx context.Context, cr *ctxLimitReader, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if cr.exceeded || errors.Is(err, errManifestTooLarge) {
		return fmt.Errorf("%w: manifest exceeds its byte ceiling", ErrManifestCorrupt)
	}
	return fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
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

// boundManifestContent charges every manifest content contributor against a fixed
// budget with overflow-safe remaining checks, rejecting an over-ceiling artifact
// before it is marshaled or persisted. Variable-size struct/slice/map values
// (the plan, identity, unit candidates, mandatory units, citations, and id
// records) are walked structurally with accountValue, which never marshals, so a
// single giant string, byte slice, or collection is rejected before any encoder
// or allocation. The post-marshal length check remains a defense-in-depth backstop.
func boundManifestContent(a PlanArtifacts, max int64) error {
	remaining := max
	charge := func(n int64) error {
		if n < 0 || n > remaining {
			return fmt.Errorf("%w: manifest content exceeds the %d-byte ceiling", ErrManifestCorrupt, max)
		}
		remaining -= n
		return nil
	}
	if err := charge(base64Len(len(a.PlanIdentityBytes)) + int64(len(a.WorkspaceFingerprint)+len(a.WorkspaceCaptureFingerprint)+len(a.PublishedIndexSignature))); err != nil {
		return err
	}
	for _, v := range []any{a.Plan, a.PlanIdentity, a.UnitCandidates, a.MandatoryUnits, a.Citations, a.IDRecords} {
		if err := accountValue(reflect.ValueOf(v), &remaining); err != nil {
			if errors.Is(err, errManifestTooLarge) {
				return fmt.Errorf("%w: manifest content exceeds the %d-byte ceiling", ErrManifestCorrupt, max)
			}
			return err
		}
	}
	return nil
}

// base64Len is the standard-base64 encoded length of n raw bytes, used to charge
// []byte/array values that JSON serializes as base64 strings.
func base64Len(n int) int64 { return int64(4 * ((n + 2) / 3)) }

// accountValue charges a conservative upper bound on the encoding/json size of v
// against remaining without marshaling, failing closed with errManifestTooLarge
// the moment the running total would exceed the budget. It never undercounts: it
// distinguishes []byte slices (base64 string) from fixed [N]byte arrays (JSON
// numeric array), scans every string for worst-case JSON escaping, and accounts
// map/slice/struct punctuation and key escaping.
func accountValue(v reflect.Value, remaining *int64) error {
	charge := func(n int64) error {
		if n < 0 || n > *remaining {
			return errManifestTooLarge
		}
		*remaining -= n
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return charge(4) // "null"
		}
		return accountValue(v.Elem(), remaining)
	case reflect.String:
		return accountString(v.String(), remaining)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return charge(base64Len(v.Len()) + 2) // base64 string with quotes
		}
		if err := charge(2); err != nil { // []
			return err
		}
		for i := range v.Len() {
			if err := charge(1); err != nil { // comma
				return err
			}
			if err := accountValue(v.Index(i), remaining); err != nil {
				return err
			}
		}
		return nil
	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// encoding/json renders a fixed byte array as a JSON numeric array,
			// not base64: each element is up to 3 digits plus a separator.
			return charge(int64(v.Len())*4 + 2)
		}
		if err := charge(2); err != nil {
			return err
		}
		for i := range v.Len() {
			if err := charge(1); err != nil {
				return err
			}
			if err := accountValue(v.Index(i), remaining); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if err := charge(2); err != nil { // {}
			return err
		}
		iter := v.MapRange()
		for iter.Next() {
			if err := charge(2); err != nil { // colon + comma
				return err
			}
			if err := accountValue(iter.Key(), remaining); err != nil {
				return err
			}
			if err := accountValue(iter.Value(), remaining); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if err := charge(2); err != nil {
			return err
		}
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).PkgPath != "" {
				continue // unexported field; not serialized
			}
			// Field key (a quoted JSON string) plus ':' and ','. Field names are
			// ASCII identifiers, but scan for escaping to never undercount.
			if err := accountString(t.Field(i).Name, remaining); err != nil {
				return err
			}
			if err := charge(2); err != nil {
				return err
			}
			if err := accountValue(v.Field(i), remaining); err != nil {
				return err
			}
		}
		return nil
	default:
		return charge(24) // numbers, bools
	}
}

// accountString charges the worst-case JSON-escaped length of s (quotes included)
// against remaining with a bounded scan that stops as soon as the running cost
// would exceed the budget, so a huge string is rejected without a full pass.
func accountString(s string, remaining *int64) error {
	cost := int64(2) // surrounding quotes
	for i := range len(s) {
		switch c := s[i]; {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			cost += 2 // short escape
		case c < 0x20 || c == '<' || c == '>' || c == '&' || c >= 0x80:
			cost += 6 // \uXXXX (controls, HTML-escaped, and any non-ASCII byte)
		default:
			cost += 1
		}
		if cost > *remaining {
			return errManifestTooLarge
		}
	}
	*remaining -= cost
	return nil
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
	// Remove through the pinned reviews root (no-follow within the confined root),
	// so a swap of the reviews path cannot redirect the recursive delete outside
	// the owned tree.
	if !validReviewID(reviewID) {
		return fmt.Errorf("review: refusing to delete malformed review id %q", reviewID)
	}
	if err := s.rootDir.RemoveAll(reviewID); err != nil {
		return err
	}
	// The striped lock file itself is retained (bounded by the stripe count) so
	// an in-flight reader's fd stays valid.
	return nil
}
