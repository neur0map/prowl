package review

// persist.go stores review plans immutably below Git common state so review
// artifacts can never enter a worktree's tracked status, even across linked
// worktrees, which share one collision and retention domain. Plans are written
// through a locked temp-dir-and-rename that consumes the caller's snapshot on
// success and restores it on failure; identity is verified by recomputing the
// plan digest from its canonical bytes; the registry enumerates every plan
// public ID, verifies each record's full/public mapping, and rejects a same-kind
// prefix that maps to a different full digest; workspace plans record a bound
// staleness fingerprint; an identical plan reuses the immutable manifest; and
// creation prunes plans older than seven days beyond the twenty most recent
// while never deleting a plan a reader holds through a stable lock file.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	manifestName      = "manifest.json"
	snapshotName      = "snapshot"
	storeLockName     = ".lock"

	// retentionAgeV1 removes review directories older than seven days.
	retentionAgeV1 = 7 * 24 * time.Hour
	// retentionCountV1 keeps at most the twenty most recent plans.
	retentionCountV1 = 20

	// defaultLockTimeout bounds how long Save/Prune wait for the store lock.
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
	// ErrPlanLocked reports that a mutation lock is held elsewhere.
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
	// ErrSnapshotOwnership reports that a caller snapshot could not be consumed
	// atomically (for example a cross-filesystem move); the caller's data is left
	// intact.
	ErrSnapshotOwnership = errors.New("review: snapshot could not be persisted atomically")
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

// PlanStore persists plans beneath the Git common directory. It is safe to open
// from any linked worktree; every instance shares one collision and retention
// domain.
type PlanStore struct {
	root        string
	locksDir    string
	lockPath    string
	lockTimeout time.Duration
	maxManifest int64
	// digest derives content-ID full digests; it is injectable so a test can
	// force prefix collisions during construction and manifest loading. Plan
	// identity and manifest integrity always use a fixed SHA-256 independent of
	// this seam.
	digest func([]byte) Digest
	// now supplies the creation clock; injectable so retention is deterministic.
	now func() time.Time
}

// OpenPlanStore resolves the Git common directory, roots the review state beneath
// it (never inside a worktree), and prepares the store-wide and per-review lock
// directories.
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
	if err := os.MkdirAll(locks, 0o700); err != nil {
		return nil, err
	}
	return &PlanStore{
		root:        root,
		locksDir:    locks,
		lockPath:    filepath.Join(root, storeLockName),
		lockTimeout: defaultLockTimeout,
		maxManifest: maxManifestBytesV1,
		digest:      func(b []byte) Digest { return sha256.Sum256(b) },
		now:         time.Now,
	}, nil
}

// Root returns the review-state root directory.
func (s *PlanStore) Root() string { return s.root }

// Save persists artifacts atomically under the plan's review id. It recomputes
// the plan digest from the canonical identity bytes (never trusting the supplied
// strings), enforces the identity registry and workspace-fingerprint invariants,
// consumes the caller's snapshot on success, reuses an identical existing plan
// (deleting the redundant incoming snapshot), and restores the caller's snapshot
// on any pre-publication failure. Post-publication pruning is best-effort and
// never fails the save.
func (s *PlanStore) Save(ctx context.Context, artifacts PlanArtifacts, snapshotDir string) error {
	if err := artifacts.Plan.Validate(); err != nil {
		return err
	}
	full, err := s.verifyPlanIdentity(artifacts.Plan, artifacts.PlanIdentityBytes)
	if err != nil {
		return err
	}
	if err := verifyWorkspaceFingerprint(artifacts.Plan, artifacts.WorkspaceFingerprint); err != nil {
		return err
	}
	reviewID := artifacts.Plan.ReviewID

	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()

	reviewDir := filepath.Join(s.root, reviewID)
	if existing, err := s.loadManifest(ctx, reviewID); err == nil {
		existingFull := sha256.Sum256(existing.PlanIdentityBytes)
		if existingFull == full {
			// Identical immutable plan: reuse, deleting the redundant incoming
			// snapshot so the caller does not leak a temp tree.
			if snapshotDir != "" {
				_ = os.RemoveAll(snapshotDir)
			}
			return nil
		}
		return fmt.Errorf("%w: review %s already persisted with a different identity", ErrIDCollision, reviewID)
	} else if !errors.Is(err, ErrReviewNotFound) {
		return err
	}

	registry, err := s.buildDomainRegistry(ctx, reviewID)
	if err != nil {
		return err
	}
	if _, err := addPlanToRegistry(registry, s.digest, artifacts.Plan, artifacts.Citations, artifacts.IDRecords, full); err != nil {
		return err
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
		return err
	}
	if err := s.publish(reviewDir, payload, snapshotDir); err != nil {
		return err
	}

	// Retention is best-effort after publication and never fails the save.
	_ = s.pruneLocked(ctx, s.now())
	return nil
}

// publish writes the manifest and moves the snapshot into a temp directory, then
// atomically renames it to the review directory. A pre-publication failure leaves
// the caller's snapshot intact (restoring it if it was already moved) and no
// review directory or temp directory behind.
func (s *PlanStore) publish(reviewDir string, payload []byte, snapshotDir string) error {
	tmp, err := os.MkdirTemp(s.root, ".tmp-save-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := writeRootedFile(tmp, manifestName, payload); err != nil {
		return err
	}
	if snapshotDir != "" {
		if err := os.Rename(snapshotDir, filepath.Join(tmp, snapshotName)); err != nil {
			// The snapshot was not consumed; the caller keeps it.
			return fmt.Errorf("%w: %v", ErrSnapshotOwnership, err)
		}
	}
	if err := os.Rename(tmp, reviewDir); err != nil {
		if snapshotDir != "" {
			// Restore the caller's snapshot before the temp directory is removed.
			_ = os.Rename(filepath.Join(tmp, snapshotName), snapshotDir)
		}
		return fmt.Errorf("%w: %v", ErrSnapshotOwnership, err)
	}
	committed = true
	return nil
}

// Load reads and fully verifies a persisted plan: bounded context-aware manifest
// read, integrity digest, plan-identity recomputation, workspace-fingerprint
// invariant, and the complete identity registry (mapping and collisions).
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

// LockReview acquires a stable per-review lock (kept outside the deletable review
// directory) that a caller holds while a plan is in use, so retention pruning
// will not delete it. The returned release closes the lock.
func (s *PlanStore) LockReview(ctx context.Context, reviewID string) (func(), error) {
	if !validReviewID(reviewID) {
		return nil, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	// Acquire the stable lock before checking existence so a reader and pruner
	// can never both believe they own the plan.
	fl := flock.New(s.reviewLockPath(reviewID))
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrPlanLocked
	}
	if _, err := os.Stat(filepath.Join(s.root, reviewID)); err != nil {
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

// verifyManifest re-runs every consistency check a load must pass.
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
	registry := map[string]Digest{}
	if _, err := addPlanToRegistry(registry, s.digest, m.Plan, m.Citations, m.IDRecords, full); err != nil {
		return err
	}
	return ctx.Err()
}

// buildDomainRegistry rebuilds the collision registry from every persisted
// manifest except the one being written.
func (s *PlanStore) buildDomainRegistry(ctx context.Context, exclude string) (map[string]Digest, error) {
	registry := map[string]Digest{}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() || !validReviewID(e.Name()) || e.Name() == exclude {
			continue
		}
		m, err := s.loadManifest(ctx, e.Name())
		if err != nil {
			return nil, err
		}
		full := sha256.Sum256(m.PlanIdentityBytes)
		if _, err := addPlanToRegistry(registry, s.digest, m.Plan, m.Citations, m.IDRecords, full); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// addPlanToRegistry verifies a plan's identity records against the IDs the plan
// actually references, then merges them (and the review id) into the registry,
// rejecting a same-kind prefix that maps to a different full digest.
func addPlanToRegistry(registry map[string]Digest, digest func([]byte) Digest, plan Plan, citations map[string]CitationProof, records []IDRecord, reviewFull Digest) (map[string]Digest, error) {
	byPublic, err := verifyRecords(digest, records)
	if err != nil {
		return nil, err
	}
	enumerated := enumeratePlanIDs(plan, citations)
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
// mapping, and rejects a duplicate public within the record set.
func verifyRecords(digest func([]byte) Digest, records []IDRecord) (map[string]Digest, error) {
	byPublic := make(map[string]Digest, len(records))
	for _, r := range records {
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
// the plan and its citations reference.
func enumeratePlanIDs(plan Plan, citations map[string]CitationProof) map[string]struct{} {
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
	for _, c := range citations {
		add(c.ID)
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

// loadManifest reads a review manifest through a confined, bounded, context-aware
// descriptor and checks its integrity digest. The review id is validated before
// any path join or open.
func (s *PlanStore) loadManifest(ctx context.Context, reviewID string) (planManifest, error) {
	if !validReviewID(reviewID) {
		return planManifest{}, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	dir := filepath.Join(s.root, reviewID)
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planManifest{}, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return planManifest{}, err
	}
	defer root.Close()
	f, err := boundedio.OpenRegularNoFollow(root, manifestName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planManifest{}, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return planManifest{}, err
	}
	defer f.Close()
	data, err := boundedio.ReadAllContext(ctx, f, s.maxManifest+1)
	if err != nil {
		if errors.Is(err, boundedio.ErrTooLarge) {
			return planManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrManifestCorrupt, s.maxManifest)
		}
		return planManifest{}, err
	}
	var m planManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return planManifest{}, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
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

// reviewLockPath is the stable per-review lock file, kept outside the deletable
// review directory so a held lock survives the directory's removal window.
func (s *PlanStore) reviewLockPath(reviewID string) string {
	return filepath.Join(s.locksDir, reviewID+".lock")
}

// pruneLocked applies retention. The caller must hold the store lock.
func (s *PlanStore) pruneLocked(ctx context.Context, now time.Time) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	type plan struct {
		id      string
		created int64
	}
	plans := make([]plan, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.IsDir() || !validReviewID(e.Name()) {
			continue
		}
		m, err := s.loadManifest(ctx, e.Name())
		if err != nil {
			// A corrupt or unreadable review is left in place; pruning never
			// destroys evidence it cannot understand.
			continue
		}
		plans = append(plans, plan{id: e.Name(), created: m.CreatedAt})
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

// deleteReview removes a review directory while holding its stable lock through
// the removal, so a reader that already holds the lock is never deleted out from
// under. A lock held elsewhere leaves the plan in place.
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
	// The stable lock file itself is retained (cheap, bounded by plan count) so
	// an in-flight reader's fd stays valid.
	return nil
}
