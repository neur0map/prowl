package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/config"
	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

// RefreshIndex publishes a complete workspace index and returns its content
// signature. A workspace transaction accepts only equal signatures from its two
// refreshes.
type RefreshIndex func(context.Context) (publishedSignature string, err error)

// ErrStale reports that a workspace capture could not be proven stable, or that
// a persisted workspace review no longer describes the current worktree.
var ErrStale = errors.New("review: workspace review is stale")

// ErrMandatoryUnitTooLarge reports a persisted mandatory object above the v1
// unit ceiling. The object is rejected before decoding or optional packing.
var ErrMandatoryUnitTooLarge = errors.New("review: mandatory unit exceeds its byte cap")

// StaleError retains the stable mismatch reason while supporting errors.Is with
// ErrStale.
type StaleError struct{ Reason string }

func (e *StaleError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrStale.Error()
	}
	return ErrStale.Error() + ": " + e.Reason
}
func (e *StaleError) Unwrap() error { return ErrStale }

// PruneWarning is returned alongside an already accepted Plan. It is not a save
// failure: callers may use the plan and separately report the retention warning.
type PruneWarning struct {
	ReviewID string
	Err      error
}

func (w *PruneWarning) Error() string {
	return fmt.Sprintf("review: plan %s persisted; retention cleanup: %v", w.ReviewID, w.Err)
}
func (w *PruneWarning) Unwrap() error { return w.Err }

// ServiceOptions assembles the transport-independent review service from the
// current project and the sanitized Git runner.
type ServiceOptions struct {
	Root          string
	WorkspacePath string
	Config        config.Config
	Store         *store.Store
	Query         *query.Querier
	Context       *contextpacket.Service
	Knowledge     *knowledge.Repository
	Git           CaptureRunner
	Refresh       RefreshIndex
}

type reviewPlanStore interface {
	NewSnapshotLease(context.Context) (*SnapshotLease, error)
	Save(context.Context, PlanArtifacts, *SnapshotLease) (SaveResult, error)
	Load(context.Context, string) (PlanArtifacts, error)
	LockReview(context.Context, string) (func(), error)
}

// Service orchestrates immutable capture, planning, persistence, and persisted
// unit retrieval. The function fields are narrow deterministic test seams; the
// production constructor always installs the concrete Task 1-6 implementations.
type Service struct {
	root          string
	workspacePath string
	config        config.Config
	indexStore    *store.Store
	query         *query.Querier
	context       *contextpacket.Service
	knowledge     *knowledge.Repository
	git           CaptureRunner
	refresh       RefreshIndex

	store       reviewPlanStore
	resolve     func(context.Context, PlanRequest) (Scope, error)
	captureOnce func(context.Context, Scope) (Capture, error)
	openView    func(context.Context, Scope, *SnapshotLease) (*HeadView, error)
	assemble    func(context.Context, Capture, *HeadView, string, bool) (PlanArtifacts, error)
	graph       GraphQueries
}

// NewService returns a configured review service. Invalid/missing runtime
// dependencies are reported by Plan or Unit so construction remains allocation-
// only and side-effect free.
func NewService(options ServiceOptions) *Service {
	git := options.Git
	if git == nil {
		git = ExecGit{}
	}
	s := &Service{
		root: options.Root, workspacePath: options.WorkspacePath, config: options.Config,
		indexStore: options.Store, query: options.Query, context: options.Context,
		knowledge: options.Knowledge, git: git, refresh: options.Refresh,
	}
	s.resolve = func(ctx context.Context, request PlanRequest) (Scope, error) {
		return ResolveScope(ctx, s.git, s.root, request)
	}
	s.captureOnce = func(ctx context.Context, scope Scope) (Capture, error) {
		return (&Capturer{Root: s.root, Runner: s.git}).CaptureOnce(ctx, scope)
	}
	s.openView = s.openHeadView
	s.assemble = s.buildArtifacts
	return s
}

// Plan captures and persists one accepted review transaction. Committed scopes
// resolve and capture exactly once. Workspace scopes retry the complete
// capture/refresh/build/capture/refresh sequence once and persist only an exact
// capture-and-signature match.
func (s *Service) Plan(ctx context.Context, request PlanRequest) (Plan, error) {
	if err := request.Validate(); err != nil {
		return Plan{}, err
	}
	if err := s.readyForPlan(); err != nil {
		return Plan{}, err
	}
	if request.Kind() == ScopeWorkspace {
		return s.planWorkspace(ctx, request)
	}
	return s.planCommitted(ctx, request)
}

func (s *Service) readyForPlan() error {
	if s == nil {
		return errors.New("review: nil service")
	}
	if strings.TrimSpace(s.root) == "" {
		return errors.New("review: service requires Root")
	}
	if s.resolve == nil || s.captureOnce == nil || s.openView == nil || s.assemble == nil {
		return errors.New("review: service is incompletely configured")
	}
	return nil
}

func (s *Service) acquireStore(ctx context.Context) (reviewPlanStore, func() error, error) {
	if s.store != nil {
		return s.store, func() error { return nil }, nil
	}
	store, err := OpenPlanStore(ctx, s.git, s.root)
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

func (s *Service) planCommitted(ctx context.Context, request PlanRequest) (plan Plan, err error) {
	scope, err := s.resolve(ctx, request)
	if err != nil {
		return Plan{}, err
	}
	capture, err := s.captureOnce(ctx, scope)
	if err != nil {
		return Plan{}, err
	}
	planStore, closeStore, err := s.acquireStore(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	lease, err := planStore.NewSnapshotLease(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	view, err := s.openView(ctx, capture.Scope, lease)
	if err != nil {
		return Plan{}, err
	}
	defer func() { err = errors.Join(err, closeHeadView(view)) }()
	signature, err := publishedViewSignature(ctx, view)
	if err != nil {
		return Plan{}, err
	}
	artifacts, err := s.assemble(ctx, capture, view, signature, request.ForceStructured)
	if err != nil {
		return Plan{}, err
	}
	if err := validateSnapshotOmissions(artifacts.Plan, relevantSnapshotOmissions(artifacts.Plan, view.Omissions)); err != nil {
		return Plan{}, err
	}
	result, err := planStore.Save(ctx, artifacts, lease)
	if err != nil {
		return Plan{}, err
	}
	plan = artifacts.Plan
	if result.PruneWarning != nil {
		return plan, &PruneWarning{ReviewID: plan.ReviewID, Err: result.PruneWarning}
	}
	return plan, nil
}

func (s *Service) planWorkspace(ctx context.Context, request PlanRequest) (Plan, error) {
	if s.refresh == nil {
		return Plan{}, errors.New("review: workspace plan requires Refresh")
	}
	var mismatch string
	for attempt := 0; attempt < 2; attempt++ {
		scope, err := s.resolve(ctx, request)
		if err != nil {
			return Plan{}, err
		}
		first, err := s.captureOnce(ctx, scope)
		if err != nil {
			if isCaptureMismatch(err) {
				mismatch = err.Error()
				if attempt == 0 {
					continue
				}
				return Plan{}, &StaleError{Reason: mismatch}
			}
			return Plan{}, err
		}
		signature1, err := s.refresh(ctx)
		if err != nil {
			return Plan{}, err
		}
		view, err := s.openView(ctx, first.Scope, nil)
		if err != nil {
			if errors.Is(err, ErrReuseUnavailable) {
				mismatch = err.Error()
				if attempt == 0 {
					continue
				}
				return Plan{}, &StaleError{Reason: mismatch}
			}
			return Plan{}, err
		}
		artifacts, buildErr := s.assemble(ctx, first, view, signature1, request.ForceStructured)
		if buildErr != nil {
			return Plan{}, errors.Join(buildErr, closeHeadView(view))
		}
		second, captureErr := s.captureOnce(ctx, first.Scope)
		if captureErr != nil && !isCaptureMismatch(captureErr) {
			return Plan{}, errors.Join(captureErr, closeHeadView(view))
		}
		signature2, refreshErr := s.refresh(ctx)
		closeErr := closeHeadView(view)
		if refreshErr != nil {
			return Plan{}, errors.Join(refreshErr, closeErr)
		}
		if closeErr != nil {
			return Plan{}, closeErr
		}
		if captureErr != nil {
			mismatch = captureErr.Error()
		} else if !sameCapture(first, second) {
			mismatch = "capture fingerprints differ"
		} else if signature1 != signature2 {
			mismatch = "published index signatures differ"
		} else {
			artifacts.WorkspaceFingerprint = hex.EncodeToString(second.Scope.Head.Value)
			artifacts.WorkspaceCaptureFingerprint = canonicalCaptureFingerprint(second)
			return s.persistWorkspace(ctx, artifacts)
		}
		if attempt == 1 {
			return Plan{}, &StaleError{Reason: mismatch}
		}
	}
	return Plan{}, &StaleError{Reason: mismatch}
}

func (s *Service) persistWorkspace(ctx context.Context, artifacts PlanArtifacts) (plan Plan, err error) {
	planStore, closeStore, err := s.acquireStore(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	lease, err := planStore.NewSnapshotLease(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	result, err := planStore.Save(ctx, artifacts, lease)
	if err != nil {
		return Plan{}, err
	}
	plan = artifacts.Plan
	if result.PruneWarning != nil {
		return plan, &PruneWarning{ReviewID: plan.ReviewID, Err: result.PruneWarning}
	}
	return plan, nil
}

func (s *Service) openHeadView(ctx context.Context, scope Scope, lease *SnapshotLease) (*HeadView, error) {
	opts := HeadViewOptions{Runner: s.git, RepoRoot: s.root, ObjectFormat: scope.ObjectFormat, Config: s.config}
	if scope.Base.Kind == SideGitOID {
		opts.BaseTreeish = hex.EncodeToString(scope.Base.Value)
	}
	opts.Reuse = &ReusableView{Root: s.root, Store: s.indexStore}
	if scope.Kind != ScopeWorkspace {
		if lease == nil || lease.Dir() == "" {
			return nil, errors.New("review: committed head view requires a snapshot lease")
		}
		opts.HeadTreeish = hex.EncodeToString(scope.Head.Value)
		opts.SnapshotParent = lease.Dir()
	}
	return OpenHeadView(ctx, opts)
}

func closeHeadView(view *HeadView) error {
	if view == nil || view.Close == nil {
		return nil
	}
	close := view.Close
	view.Close = nil
	return close()
}

func publishedViewSignature(ctx context.Context, view *HeadView) (string, error) {
	if view == nil || view.Store == nil {
		return "", nil
	}
	signature, err := view.Store.GetMetaContext(ctx, "cli_sig")
	if err != nil {
		return "", fmt.Errorf("review: read published head index signature: %w", err)
	}
	return signature, nil
}

func sameCapture(a, b Capture) bool {
	return a.Scope.Kind == b.Scope.Kind && a.Scope.ObjectFormat == b.Scope.ObjectFormat &&
		a.Scope.Digest == b.Scope.Digest && a.CanonicalPatch == b.CanonicalPatch &&
		a.RawAdditions == b.RawAdditions && a.RawDeletions == b.RawDeletions &&
		a.RawChurn == b.RawChurn && a.ChangedPaths == b.ChangedPaths &&
		a.ReviewableChurn == b.ReviewableChurn && sameSide(a.Scope.Base, b.Scope.Base) &&
		sameSide(a.Scope.Head, b.Scope.Head) &&
		bytes.Equal(CanonicalPatchV1(a.Paths), CanonicalPatchV1(b.Paths))
}

func canonicalCaptureFingerprint(capture Capture) string {
	canonical := Frame(
		Field{Name: "scope", Value: capture.Scope.Digest[:]},
		Field{Name: "canonical_patch", Value: capture.CanonicalPatch[:]},
		Field{Name: "paths", Value: CanonicalPatchV1(capture.Paths)},
		Field{Name: "raw_additions", Value: u64(uint64(capture.RawAdditions))},
		Field{Name: "raw_deletions", Value: u64(uint64(capture.RawDeletions))},
		Field{Name: "raw_churn", Value: u64(uint64(capture.RawChurn))},
		Field{Name: "reviewable_churn", Value: u64(uint64(capture.ReviewableChurn))},
		Field{Name: "changed_paths", Value: u64(uint64(capture.ChangedPaths))},
	)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func sameSide(a, b SideIdentity) bool {
	return a.Kind == b.Kind && bytes.Equal(a.Value, b.Value)
}

func isCaptureMismatch(err error) bool {
	return errors.Is(err, ErrConcurrentModification) || errors.Is(err, ErrHeadMoved)
}

func relevantSnapshotOmissions(plan Plan, omissions []Omission) []Omission {
	relevant := make([]Omission, 0, len(omissions))
	for _, omission := range omissions {
		for _, changed := range plan.ChangedPaths {
			var changedPath string
			switch omission.Side {
			case SideBase:
				changedPath = changed.OldPath
			case SideHead:
				changedPath = changed.NewPath
			}
			if changedPath != "" && filepath.ToSlash(changedPath) == filepath.ToSlash(omission.Path) {
				relevant = append(relevant, omission)
				break
			}
		}
	}
	return relevant
}

func validateSnapshotOmissions(plan Plan, omissions []Omission) error {
	for _, omission := range omissions {
		found := false
		for _, path := range plan.ChangedPaths {
			name := path.NewPath
			if name == "" {
				name = path.OldPath
			}
			if filepath.ToSlash(name) != filepath.ToSlash(omission.Path) {
				continue
			}
			found = true
			if path.ReviewClass != string(ReviewClassUnreviewable) || path.Coverage != string(PathCoverageNone) {
				return fmt.Errorf("review: snapshot omission %s is not accounted as unreviewable", omission.Path)
			}
			break
		}
		if !found {
			return fmt.Errorf("review: snapshot omission %s is absent from changed paths", omission.Path)
		}
	}
	return nil
}
