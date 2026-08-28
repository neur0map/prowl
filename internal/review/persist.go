package review

// persist.go stores review plans immutably below Git common state so review
// artifacts can never enter a worktree's tracked status, even across linked
// worktrees, which share one collision and retention domain. Plans are written
// through a locked temp-dir-and-rename, their identity registry rejects any
// same-kind public prefix whose full digest differs, workspace plans record a
// staleness fingerprint, an identical plan reuses the immutable manifest, and
// creation prunes plans older than seven days beyond the twenty most recent.

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
	"time"

	"github.com/gofrs/flock"

	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
)

const (
	// planStoreSchemaV1 identifies the on-disk manifest schema.
	planStoreSchemaV1 = "review.manifest.v1"
	// reviewsDir is the review-state subtree beneath the Git common directory.
	reviewsSubdir = "reviews"
	// manifestName is the plan manifest file inside a review directory.
	manifestName = "manifest.json"
	// snapshotName is the materialized head snapshot inside a review directory.
	snapshotName = "snapshot"
	// storeLockName is the store-wide mutation lock file.
	storeLockName = ".lock"
	// reviewLockName is a per-review lock a reader holds to protect a plan from
	// retention pruning while it is in use.
	reviewLockName = ".lock"

	// retentionAgeV1 removes review directories older than seven days.
	retentionAgeV1 = 7 * 24 * time.Hour
	// retentionCountV1 keeps at most the twenty most recent plans.
	retentionCountV1 = 20

	// defaultLockTimeout bounds how long Save/Prune wait for the store lock.
	defaultLockTimeout = 5 * time.Second
)

var (
	// ErrIDCollision reports that a content-derived public ID prefix maps to two
	// different full digests of the same kind, across the shared review domain.
	ErrIDCollision = errors.New("review: public ID prefix collision with a different full digest")
	// ErrPlanLocked reports that the store's mutation lock is held elsewhere.
	ErrPlanLocked = errors.New("review: plan store is locked")
	// ErrManifestCorrupt reports that a manifest failed its integrity digest or
	// is otherwise malformed.
	ErrManifestCorrupt = errors.New("review: review manifest is corrupt")
	// ErrReviewNotFound reports that no persisted plan has the requested id.
	ErrReviewNotFound = errors.New("review: no persisted plan for review id")
	// ErrPlanIdentityMismatch reports that a stored review id and its plan digest
	// are inconsistent.
	ErrPlanIdentityMismatch = errors.New("review: review id inconsistent with plan digest")
)

// contentIDKinds are the content-derived public ID kinds whose canonical inputs
// feed the collision registry. The review id (rvw_) is registered separately
// from the plan digest it already carries.
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

// IDInput is one content-derived identifier's canonical FramedFieldsV1 input and
// its kind prefix. The store derives the full digest and public prefix from it,
// so a hostile or accidental prefix collision is detected during both plan
// construction and manifest loading.
type IDInput struct {
	Kind  string `json:"kind"`
	Bytes []byte `json:"bytes"`
}

// PlanArtifacts is everything persisted atomically with a plan: the plan itself,
// per-unit retrieval candidates, mandatory unit bytes, citation proofs, the
// published head-index signature, the identity inputs for the collision
// registry, and (for workspace plans) the verified capture fingerprint.
type PlanArtifacts struct {
	Plan                    Plan
	UnitCandidates          map[string][]contextpacket.Candidate
	MandatoryUnits          map[string][]byte
	Citations               map[string]CitationProof
	PublishedIndexSignature string
	IDInputs                []IDInput
	// WorkspaceFingerprint records the verified final workspace scope digest for
	// a workspace plan; it is empty for an immutable committed plan.
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
	IDInputs                []IDInput                            `json:"id_inputs,omitempty"`
	ContentDigest           string                               `json:"content_digest"`
}

// PlanStore persists plans beneath the Git common directory. It is safe to open
// from any linked worktree; every instance shares one collision and retention
// domain.
type PlanStore struct {
	root        string
	lockPath    string
	lockTimeout time.Duration
	// digest derives content-ID full digests; it is injectable so a test can
	// force prefix collisions during construction and manifest loading. Manifest
	// integrity always uses a fixed SHA-256 independent of this seam.
	digest func([]byte) Digest
	// now supplies the creation clock; injectable so retention is deterministic.
	now func() time.Time
}

// OpenPlanStore resolves the Git common directory, roots the review state beneath
// it (never inside a worktree), and prepares the store-wide lock.
func OpenPlanStore(ctx context.Context, runner GitRunner, repoRoot string) (*PlanStore, error) {
	if runner == nil {
		return nil, errors.New("review: OpenPlanStore requires a runner")
	}
	common, err := gitCommonDir(ctx, runner, repoRoot)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(common, "prowl", reviewsSubdir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &PlanStore{
		root:        root,
		lockPath:    filepath.Join(root, storeLockName),
		lockTimeout: defaultLockTimeout,
		digest:      func(b []byte) Digest { return sha256.Sum256(b) },
		now:         time.Now,
	}, nil
}

// Root returns the review-state root directory.
func (s *PlanStore) Root() string { return s.root }

// Save persists artifacts atomically under the plan's review id. An identical
// plan (same review id and plan digest) reuses the existing immutable manifest
// and snapshot. A same-kind public prefix that maps to a different full digest,
// including a review id reused with a different plan digest, fails with
// ErrIDCollision. snapshotDir, when non-empty, is moved into the review
// directory. Creation prunes stale and excess plans.
func (s *PlanStore) Save(ctx context.Context, artifacts PlanArtifacts, snapshotDir string) error {
	if err := artifacts.Plan.Validate(); err != nil {
		return err
	}
	reviewID := artifacts.Plan.ReviewID
	if !validReviewID(reviewID) {
		return fmt.Errorf("review: malformed review id %q", reviewID)
	}
	if err := s.verifyReviewIdentity(artifacts.Plan); err != nil {
		return err
	}

	release, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer release()

	reviewDir := filepath.Join(s.root, reviewID)
	if existing, err := s.loadManifest(reviewID); err == nil {
		// Immutable reuse when the plan is byte-identical in identity; a differing
		// plan under the same review id is a collision.
		if existing.PlanDigest == artifacts.Plan.PlanDigest {
			return nil
		}
		return fmt.Errorf("%w: review %s already persisted with a different plan digest", ErrIDCollision, reviewID)
	} else if !errors.Is(err, ErrReviewNotFound) {
		return err
	}

	registry, err := s.buildDomainRegistry(reviewID)
	if err != nil {
		return err
	}
	if err := addArtifactsToRegistry(registry, s.digest, artifacts); err != nil {
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
		IDInputs:                artifacts.IDInputs,
	}
	payload, err := marshalManifest(manifest)
	if err != nil {
		return err
	}

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
			return err
		}
	}
	if err := os.Rename(tmp, reviewDir); err != nil {
		return err
	}
	committed = true

	// Retention runs under the same lock so it never races another save.
	if err := s.pruneLocked(s.now()); err != nil {
		return err
	}
	return nil
}

// Load reads and verifies a persisted plan. It checks the manifest integrity
// digest, the review-id/plan-digest consistency, and rebuilds the collision
// registry from the stored identity inputs, rejecting any same-kind prefix that
// maps to a different full digest.
func (s *PlanStore) Load(ctx context.Context, reviewID string) (PlanArtifacts, error) {
	if !validReviewID(reviewID) {
		return PlanArtifacts{}, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	manifest, err := s.loadManifest(reviewID)
	if err != nil {
		return PlanArtifacts{}, err
	}
	if err := verifyManifestConsistency(manifest); err != nil {
		return PlanArtifacts{}, err
	}
	registry := map[string]Digest{}
	if err := addManifestToRegistry(registry, s.digest, manifest); err != nil {
		return PlanArtifacts{}, err
	}
	return PlanArtifacts{
		Plan:                    manifest.Plan,
		UnitCandidates:          manifest.UnitCandidates,
		MandatoryUnits:          manifest.MandatoryUnits,
		Citations:               manifest.Citations,
		PublishedIndexSignature: manifest.PublishedIndexSignature,
		IDInputs:                manifest.IDInputs,
		WorkspaceFingerprint:    manifest.WorkspaceFingerprint,
	}, nil
}

// Stale reports whether a workspace plan no longer matches the current workspace
// scope. Committed and range plans apply to an immutable resolved head and are
// never stale.
func (s *PlanStore) Stale(ctx context.Context, reviewID, currentFingerprint string) (bool, error) {
	manifest, err := s.loadManifest(reviewID)
	if err != nil {
		return false, err
	}
	if manifest.ScopeKind != ScopeWorkspace {
		return false, nil
	}
	return manifest.WorkspaceFingerprint != currentFingerprint, nil
}

// LockReview acquires an exclusive per-review lock a caller holds while a plan is
// in use, so retention pruning will not delete it. The returned release closes
// the lock.
func (s *PlanStore) LockReview(ctx context.Context, reviewID string) (func(), error) {
	if !validReviewID(reviewID) {
		return nil, fmt.Errorf("review: malformed review id %q", reviewID)
	}
	dir := filepath.Join(s.root, reviewID)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return nil, err
	}
	fl := flock.New(filepath.Join(dir, reviewLockName))
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, ErrPlanLocked
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
	return s.pruneLocked(now)
}

// ---- internals ------------------------------------------------------------

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

// verifyReviewIdentity checks that a plan's public review id is the canonical
// truncation of its plan digest.
func (s *PlanStore) verifyReviewIdentity(plan Plan) error {
	raw, err := hex.DecodeString(plan.PlanDigest)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("%w: plan digest %q", ErrPlanIdentityMismatch, plan.PlanDigest)
	}
	var full Digest
	copy(full[:], raw)
	want := PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public
	if want != plan.ReviewID {
		return fmt.Errorf("%w: review id %q does not match plan digest", ErrPlanIdentityMismatch, plan.ReviewID)
	}
	return nil
}

// verifyManifestConsistency re-derives the public review id from the stored plan
// digest and checks it matches.
func verifyManifestConsistency(m planManifest) error {
	if m.Schema != planStoreSchemaV1 {
		return fmt.Errorf("%w: schema %q", ErrManifestCorrupt, m.Schema)
	}
	if m.ReviewID != m.Plan.ReviewID || m.PlanDigest != m.Plan.PlanDigest {
		return fmt.Errorf("%w: manifest header disagrees with embedded plan", ErrManifestCorrupt)
	}
	raw, err := hex.DecodeString(m.PlanDigest)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("%w: plan digest %q", ErrPlanIdentityMismatch, m.PlanDigest)
	}
	var full Digest
	copy(full[:], raw)
	if PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public != m.ReviewID {
		return fmt.Errorf("%w: review id %q does not match plan digest", ErrPlanIdentityMismatch, m.ReviewID)
	}
	return nil
}

// buildDomainRegistry rebuilds the collision registry from every persisted
// manifest except the one being written, so a new plan's IDs are checked against
// the whole shared domain.
func (s *PlanStore) buildDomainRegistry(exclude string) (map[string]Digest, error) {
	registry := map[string]Digest{}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !validReviewID(e.Name()) || e.Name() == exclude {
			continue
		}
		m, err := s.loadManifest(e.Name())
		if err != nil {
			return nil, err
		}
		if err := addManifestToRegistry(registry, s.digest, m); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// addManifestToRegistry registers a manifest's review id and every content ID.
func addManifestToRegistry(registry map[string]Digest, digest func([]byte) Digest, m planManifest) error {
	if err := registerReview(registry, m.ReviewID, m.PlanDigest); err != nil {
		return err
	}
	return registerInputs(registry, digest, m.IDInputs)
}

// addArtifactsToRegistry registers a new plan's review id and content IDs.
func addArtifactsToRegistry(registry map[string]Digest, digest func([]byte) Digest, a PlanArtifacts) error {
	if err := registerReview(registry, a.Plan.ReviewID, a.Plan.PlanDigest); err != nil {
		return err
	}
	return registerInputs(registry, digest, a.IDInputs)
}

// registerReview adds the review id keyed by its full plan digest.
func registerReview(registry map[string]Digest, reviewID, planDigest string) error {
	raw, err := hex.DecodeString(planDigest)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("%w: plan digest %q", ErrPlanIdentityMismatch, planDigest)
	}
	var full Digest
	copy(full[:], raw)
	return registerID(registry, reviewID, full)
}

// registerInputs derives each content ID's public prefix and full digest from
// its canonical input and registers it.
func registerInputs(registry map[string]Digest, digest func([]byte) Digest, inputs []IDInput) error {
	for _, in := range inputs {
		if !contentIDKinds[in.Kind] {
			return fmt.Errorf("review: unknown identity input kind %q", in.Kind)
		}
		full := digest(in.Bytes)
		public := in.Kind + hex.EncodeToString(full[:PublicIDContentBytesV1])
		if err := registerID(registry, public, full); err != nil {
			return err
		}
	}
	return nil
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

// loadManifest reads and integrity-checks a review manifest.
func (s *PlanStore) loadManifest(reviewID string) (planManifest, error) {
	dir := filepath.Join(s.root, reviewID)
	data, err := readRootedFile(dir, manifestName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planManifest{}, fmt.Errorf("%w: %s", ErrReviewNotFound, reviewID)
		}
		return planManifest{}, err
	}
	var m planManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return planManifest{}, fmt.Errorf("%w: %v", ErrManifestCorrupt, err)
	}
	stored := m.ContentDigest
	m.ContentDigest = ""
	recomputed, err := manifestIntegrityDigest(m)
	if err != nil {
		return planManifest{}, err
	}
	if stored != recomputed {
		return planManifest{}, fmt.Errorf("%w: integrity digest mismatch", ErrManifestCorrupt)
	}
	m.ContentDigest = stored
	return m, nil
}

// pruneLocked applies retention. The caller must hold the store lock.
func (s *PlanStore) pruneLocked(now time.Time) error {
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
		if !e.IsDir() || !validReviewID(e.Name()) {
			continue
		}
		m, err := s.loadManifest(e.Name())
		if err != nil {
			// A corrupt or unreadable review is left in place rather than deleted;
			// pruning never destroys evidence it cannot understand.
			continue
		}
		plans = append(plans, plan{id: e.Name(), created: m.CreatedAt})
	}
	// Newest first.
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].created != plans[j].created {
			return plans[i].created > plans[j].created
		}
		return plans[i].id < plans[j].id
	})
	cutoff := now.Add(-retentionAgeV1).Unix()
	for i, p := range plans {
		tooOld := p.created < cutoff
		tooMany := i >= retentionCountV1
		if !tooOld && !tooMany {
			continue
		}
		if err := s.deleteReview(p.id); err != nil {
			return err
		}
	}
	return nil
}

// deleteReview removes a review directory unless a per-review lock is held. A
// held lock leaves the plan in place.
func (s *PlanStore) deleteReview(reviewID string) error {
	dir := filepath.Join(s.root, reviewID)
	fl := flock.New(filepath.Join(dir, reviewLockName))
	locked, err := fl.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return nil // in use; never delete a locked plan
	}
	// Release before removal so the lock file's descriptor is not held on a path
	// that is being unlinked.
	_ = fl.Unlock()
	return os.RemoveAll(dir)
}

// marshalManifest computes the integrity digest and returns the serialized
// manifest bytes.
func marshalManifest(m planManifest) ([]byte, error) {
	m.ContentDigest = ""
	digest, err := manifestIntegrityDigest(m)
	if err != nil {
		return nil, err
	}
	m.ContentDigest = digest
	return json.Marshal(m)
}

// manifestIntegrityDigest is the fixed SHA-256 of the manifest with an empty
// content digest field. It is independent of the injectable ID digest so a
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

// readRootedFile reads name under dir through a confined root.
func readRootedFile(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}
