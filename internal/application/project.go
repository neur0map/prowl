// Package application owns deterministic project service assembly shared by
// CLI, MCP, and workbench transports.
package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/prowl-agent/prowl-agent/internal/assist"
	"github.com/prowl-agent/prowl-agent/internal/capability"
	"github.com/prowl-agent/prowl-agent/internal/config"
	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
	"github.com/prowl-agent/prowl-agent/internal/index"
	"github.com/prowl-agent/prowl-agent/internal/knowledge"
	"github.com/prowl-agent/prowl-agent/internal/jobs"
	"github.com/prowl-agent/prowl-agent/internal/knowledge/okfv01"
	"github.com/prowl-agent/prowl-agent/internal/query"
	"github.com/prowl-agent/prowl-agent/internal/store"
	"github.com/prowl-agent/prowl-agent/internal/workspace"
)

// InferencerProvider resolves an optional project inferencer from configuration.
type InferencerProvider func(context.Context, config.Config) assist.Inferencer

// Options controls optional AI assembly. Deterministic structural services are
// always available and never require an inferencer.
type Options struct {
	EnableAI           bool
	InferencerProvider InferencerProvider
}

// StartupLimits bound workbench assembly and its single freshness probe.
type StartupLimits struct {
	Timeout        time.Duration
	CandidatePaths int
}

// DefaultStartupLimits returns fresh production bounds for local workbench startup.
func DefaultStartupLimits() StartupLimits {
	return StartupLimits{Timeout: 250 * time.Millisecond, CandidatePaths: 2000}
}


// RefreshResult describes one deterministic refresh. EmbeddingError is a
// best-effort AI warning; structural indexing failures are returned as errors.
type RefreshResult struct {
	Summary        index.Summary
	Embedded       int
	EmbeddingError error
}

// Project is the shared, deterministic project service graph. It starts no
// goroutines. Close owns the underlying Store and is idempotent.
type Project struct {
	Workspace    *workspace.Workspace
	Config       config.Config
	Store        *store.Store
	Capabilities *capability.Catalog
	Query        *query.Querier
	Context      *contextpacket.Service
	Knowledge    *knowledge.Repository
	Inferencer   assist.Inferencer
	ReadGuard    store.ReadGuard
	// InitialRefresh is non-zero when OpenProject repaired or refreshed stale
	// project state during assembly.
	InitialRefresh RefreshResult
	startupRefreshPending bool
	jobService            *jobs.Service

	refreshGate chan struct{}
	closeOnce   sync.Once
	closeErr    error
	closed      atomic.Bool
	// beforeIndex and afterIndex are package-private deterministic test seams.
	beforeIndex func()
	// afterIndex is a package-private test seam invoked while generation writes
	// are still uncommitted and before post-index signature validation.
	afterIndex func()
}

// OpenProject resolves a workspace, opens its derived store, loads strict
// configuration, refreshes stale structural data, and assembles shared services.
func OpenProject(ctx context.Context, start string, opts Options) (*Project, error) {
	project, err := assembleProject(ctx, start, opts, func(_ context.Context, start string) (*workspace.Workspace, error) {
		return workspace.Resolve(start)
	}, func(_ context.Context, path string) (config.Config, error) {
		return config.Load(path)
	}, func(_ context.Context, path string) (*store.Store, error) {
		return store.Open(path)
	})
	if err != nil {
		return nil, err
	}
	initialRefresh, err := project.ensureFresh(ctx)
	if err != nil {
		return nil, errors.Join(err, project.Close())
	}
	project.InitialRefresh = initialRefresh
	return project, nil
}

// OpenWorkbenchProject assembles services and performs one bounded freshness
// probe. A usable but stale project is returned for the CLI to enqueue durably.
func OpenWorkbenchProject(parent context.Context, start string, opts Options, limits StartupLimits) (*Project, error) {
	if limits.Timeout <= 0 || limits.Timeout > 10*time.Second || limits.CandidatePaths <= 0 || limits.CandidatePaths > 1_000_000 {
		return nil, errors.New("invalid workbench startup limits")
	}
	ctx, cancel := context.WithTimeout(parent, limits.Timeout)
	defer cancel()
	project, err := assembleProject(ctx, start, opts, workspace.ResolveContext, config.LoadContext, store.OpenContext)
	if err != nil {
		return nil, err
	}
	current, probeErr := project.startupFresh(ctx, limits.CandidatePaths)
	if probeErr != nil {
		var candidateLimit index.CandidateLimitError
		if errors.Is(probeErr, context.DeadlineExceeded) || errors.As(probeErr, &candidateLimit) {
			project.startupRefreshPending = true
			return project, nil
		}
		return nil, errors.Join(probeErr, project.Close())
	}
	if !current {
		project.startupRefreshPending = true
	}
	return project, nil
}

// StartupRefreshPending reports whether bounded workbench startup deferred a
// required index refresh to the durable jobs service.
func (p *Project) StartupRefreshPending() bool {
	return p != nil && p.startupRefreshPending
}

// AttachJobsService makes Project.Close stop the sole attached service before
// closing the index store.
func (p *Project) AttachJobsService(service *jobs.Service) error {
	if p == nil || service == nil || p.closed.Load() {
		return errors.New("invalid project jobs service")
	}
	if p.jobService != nil {
		return errors.New("project jobs service already attached")
	}
	p.jobService = service
	return nil
}

func assembleProject(
	ctx context.Context,
	start string,
	opts Options,
	resolveWorkspace func(context.Context, string) (*workspace.Workspace, error),
	loadConfig func(context.Context, string) (config.Config, error),
	openStore func(context.Context, string) (*store.Store, error),
) (*Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := resolveWorkspace(ctx, start)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	database, err := openStore(ctx, state.DB)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Project, error) {
		return nil, errors.Join(err, database.Close())
	}
	cfg, err := loadConfig(ctx, state.Path)
	if err != nil {
		return fail(fmt.Errorf("load project config: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}

	var inferencer assist.Inferencer
	if opts.EnableAI && opts.InferencerProvider != nil {
		inferencer = opts.InferencerProvider(ctx, cfg)
	}

	catalog, err := capability.BuiltinCatalog()
	if err != nil {
		return fail(fmt.Errorf("load capability catalog: %w", err))
	}
	knowledgeRepo := knowledge.NewRepository(state.Knowledge, okfv01.Codec{})
	querier := query.New(database)
	if inferencer != nil {
		querier = query.NewWithAssist(database, inferencer)
	}
	readGuard := generationReadGuard(filepath.Join(state.Path, "index-refresh.lock"))
	querier.RequirePublishedGeneration().WithReadGuard(readGuard)
	contextService := &contextpacket.Service{Store: database, Knowledge: knowledgeRepo, Root: state.Root, Tracer: contextpacket.StoreTracer{Store: database}, RequirePublished: true, ReadGuard: readGuard}
	if inferencer != nil {
		contextService.Reranker = contextpacket.AssistSemanticReranker{Inferencer: inferencer}
	}
	project := &Project{
		Workspace:    state,
		Config:       cfg,
		Store:        database,
		Capabilities: catalog,
		Query:        querier,
		Context:      contextService,
		Knowledge:    knowledgeRepo,
		Inferencer:   inferencer,
		ReadGuard:    readGuard,
		refreshGate:  make(chan struct{}, 1),
	}
	if err := database.SetMetaContext(ctx, "ai_enabled", strconv.FormatBool(cfg.AI.Enabled)); err != nil {

		return fail(fmt.Errorf("record AI state: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return project, nil
}

// Refresh performs the shared deterministic reindex operation used by startup
// and transport-owned freshness watchers. It serializes refreshes but starts no
// background work itself.
func (p *Project) Refresh(ctx context.Context) (RefreshResult, error) {
	if p.closed.Load() {
		return RefreshResult{}, errors.New("project is closed")
	}
	select {
	case p.refreshGate <- struct{}{}:
		defer func() { <-p.refreshGate }()
	case <-ctx.Done():
		return RefreshResult{}, ctx.Err()
	}
	if p.closed.Load() {
		return RefreshResult{}, errors.New("project is closed")
	}

	fileLock := flock.New(filepath.Join(p.Workspace.Path, "index-refresh.lock"))
	locked, err := fileLock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return RefreshResult{}, err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return RefreshResult{}, err
		}
		return RefreshResult{}, errors.New("project refresh lock was not acquired")
	}

	var result RefreshResult
	for attempt := 0; attempt < 2; attempt++ {
		result, err = p.refresh(ctx)
		if !errors.Is(err, errSourcesChanged) {
			break
		}
	}
	if err != nil {
		if stateErr := p.Store.SetMeta("index_state", "incomplete"); stateErr != nil {
			err = errors.Join(err, stateErr)
		}
	}
	return result, errors.Join(err, fileLock.Unlock())
}

var errSourcesChanged = index.ErrSourcesChanged

func (p *Project) refresh(ctx context.Context) (RefreshResult, error) {
	var result RefreshResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	opt := index.Options{Ignore: p.Config.Ignore, Languages: p.Config.Languages}
	preSnapshot, err := index.SourceSnapshotWithOptionsContext(ctx, p.Workspace.Root, opt)
	if err != nil {
		return result, err
	}
	preSignature := preSnapshot.Signature
	generation, err := p.Store.BeginGeneration(ctx)
	if err != nil {
		return result, err
	}
	defer generation.Rollback()
	current := generation.Store
	if p.beforeIndex != nil {
		p.beforeIndex()
	}

	summary, err := index.IndexWithOptionsContext(ctx, current, p.Workspace.Root, opt)
	if err != nil {
		return result, err
	}
	result.Summary = summary
	if p.afterIndex != nil {
		p.afterIndex()
	}
	if err := current.SetMeta("vectors_complete", "0"); err != nil {
		return result, err
	}
	if p.Inferencer != nil {
		result.Embedded, result.EmbeddingError = index.BuildVectors(ctx, current, p.Inferencer, p.Config.AI.EmbedModel)
		if result.EmbeddingError != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err := current.ResetVectors(); err != nil {
				return result, errors.Join(result.EmbeddingError, err)
			}
		} else {
			model, err := current.GetMeta("embed_model")
			if err != nil {
				return result, err
			}
			if model != p.Config.AI.EmbedModel {
				return result, fmt.Errorf("vector model metadata = %q, want %q", model, p.Config.AI.EmbedModel)
			}
		}
	}
	sig, err := index.SignatureWithOptionsContext(ctx, p.Workspace.Root, opt)
	if err != nil {
		return result, err
	}
	if sig != preSignature {
		return result, errSourcesChanged
	}
	if err := index.ValidateSnapshotWithExpectedContext(ctx, current, p.Workspace.Root, opt, preSnapshot.Paths); err != nil {
		return result, errors.Join(errSourcesChanged, err)
	}
	if err := current.SetMeta("cli_sig", strconv.FormatUint(sig, 16)); err != nil {
		return result, err
	}
	if err := current.SetMeta("index_state", "complete"); err != nil {
		return result, err
	}
	if err := generation.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (p *Project) startupFresh(ctx context.Context, maxCandidates int) (bool, error) {
	release, err := p.ReadGuard(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	opt := index.Options{Ignore: p.Config.Ignore, Languages: p.Config.Languages}
	snapshot, err := index.SourceSnapshotWithOptionsLimitContext(ctx, p.Workspace.Root, opt, maxCandidates)
	if err != nil {
		return false, err
	}
	readMeta := func(key string) (string, error) {
		value, err := p.Store.GetMetaContext(ctx, key)
		if err != nil {
			return "", fmt.Errorf("read %s metadata: %w", key, err)
		}
		return value, nil
	}
	oldSig, err := readMeta("cli_sig")
	if err != nil {
		return false, err
	}
	oldVersion, err := readMeta("index_version")
	if err != nil {
		return false, err
	}
	indexState, err := readMeta("index_state")
	if err != nil {
		return false, err
	}
	current := indexState == "complete" && oldVersion == index.Version() && oldSig == strconv.FormatUint(snapshot.Signature, 16)
	if p.Inferencer != nil {
		vectorsComplete, err := readMeta("vectors_complete")
		if err != nil {
			return false, err
		}
		embedModel, err := readMeta("embed_model")
		if err != nil {
			return false, err
		}
		current = current && vectorsComplete == "1" && embedModel == p.Config.AI.EmbedModel
	}
	return current, ctx.Err()
}

func (p *Project) ensureFresh(ctx context.Context) (RefreshResult, error) {
	opt := index.Options{Ignore: p.Config.Ignore, Languages: p.Config.Languages}
	sig, sigErr := index.SignatureWithOptionsContext(ctx, p.Workspace.Root, opt)
	if sigErr != nil && ctx.Err() != nil {
		return RefreshResult{}, ctx.Err()
	}
	oldSig, _ := p.Store.GetMeta("cli_sig")
	oldVersion, _ := p.Store.GetMeta("index_version")
	indexState, _ := p.Store.GetMeta("index_state")
	vectorsComplete, _ := p.Store.GetMeta("vectors_complete")
	embedModel, _ := p.Store.GetMeta("embed_model")
	current := sigErr == nil && indexState == "complete" && oldVersion == index.Version() && oldSig == strconv.FormatUint(sig, 16)
	if p.Inferencer != nil && (vectorsComplete != "1" || embedModel != p.Config.AI.EmbedModel) {
		current = false
	}
	if current {
		return RefreshResult{}, nil
	}
	return p.Refresh(ctx)
}

// Close closes the project store once. Concurrent Close calls are safe; callers
// must stop active service operations before closing the project.
func (p *Project) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		p.refreshGate <- struct{}{}
		defer func() { <-p.refreshGate }()
		if p.Store != nil {
			p.closeErr = p.Store.Close()
		}
	})
	return p.closeErr
}

func generationReadGuard(path string) store.ReadGuard {
	return func(ctx context.Context) (func(), error) {
		fileLock := flock.New(path)
		locked, err := fileLock.TryRLockContext(ctx, 25*time.Millisecond)
		if err != nil {
			return nil, err
		}
		if !locked {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("project generation read lock was not acquired")
		}
		return func() { _ = fileLock.Unlock() }, nil
	}
}

// StartupRefreshPending reports whether workbench startup deferred a refresh.
func (p *Project) StartupRefreshPending() bool {
	return p != nil && p.startupRefreshPending
}
