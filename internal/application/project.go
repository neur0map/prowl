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
	"github.com/prowl-agent/prowl-agent/internal/knowledge/okfv01"
	"github.com/prowl-agent/prowl-agent/internal/query"
	"github.com/prowl-agent/prowl-agent/internal/review"
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
	// VectorProgress, when set, receives semantic-index build progress. A binary
	// upgrade re-chunks the repository and so invalidates every vector; rebuilding
	// them is fast but not instant, and a transport that can report it should.
	VectorProgress func(index.VectorPass)
}

// inferencerEmbeds reports whether an inferencer can produce embeddings. A
// provider may opt out (e.g. an agent-CLI backend that only reranks) so the
// refresh skips vector building instead of recording a spurious embed error.
func inferencerEmbeds(inf assist.Inferencer) bool {
	if c, ok := inf.(interface{ SupportsEmbeddings() bool }); ok {
		return c.SupportsEmbeddings()
	}
	return true
}

// embedModelID returns the identity of the embedder that produced a project's
// vectors. It keys stored vectors, so a change of embedder (or a version bump of
// the bundled model) triggers a clean re-embed. There is no configured fallback:
// the embedding backend is not user-selectable.
func embedModelID(inf assist.Inferencer) string {
	if m, ok := inf.(interface{ EmbedModelID() string }); ok {
		return m.EmbedModelID()
	}
	return ""
}

// RefreshResult describes one deterministic refresh. EmbeddingError is a
// best-effort AI warning; structural indexing failures are returned as errors.
// VectorsRemaining is how many chunks still lack an embedding once the refresh
// finished, so a caller can say semantic search is still warming rather than
// leave an agent guessing.
type RefreshResult struct {
	Summary          index.Summary
	Embedded         int
	VectorsRemaining int
	EmbeddingError   error
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
	Review       *review.Service
	Inferencer   assist.Inferencer
	ReadGuard    store.ReadGuard
	// InitialRefresh is non-zero when OpenProject repaired or refreshed stale
	// project state during assembly.
	InitialRefresh RefreshResult

	refreshGate    chan struct{}
	vectorProgress func(index.VectorPass)
	closeOnce      sync.Once
	closeErr       error
	closed         atomic.Bool
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
	readGuard := generationReadGuard(filepath.Join(state.Derived, "index-refresh.lock"))
	querier.RequirePublishedGeneration().WithReadGuard(readGuard)
	contextService := &contextpacket.Service{Store: database, Knowledge: knowledgeRepo, Root: state.Root, Tracer: contextpacket.StoreTracer{Store: database}, RequirePublished: true, ReadGuard: readGuard}
	if inferencer != nil {
		reranker := contextpacket.AssistSemanticReranker{Inferencer: inferencer}
		// Agent CLIs answer via a subprocess completion, slower than a local
		// daemon, so give reranking a longer budget before it falls back to the
		// deterministic order.
		if _, ok := inferencer.(*assist.AgentCLI); ok {
			reranker.Timeout = 45 * time.Second
		}
		contextService.Reranker = reranker
	}
	project := &Project{
		Workspace:      state,
		Config:         cfg,
		Store:          database,
		Capabilities:   catalog,
		Query:          querier,
		Context:        contextService,
		Knowledge:      knowledgeRepo,
		Inferencer:     inferencer,
		ReadGuard:      readGuard,
		refreshGate:    make(chan struct{}, 1),
		vectorProgress: opts.VectorProgress,
	}
	project.Review = review.NewService(review.ServiceOptions{
		Root:          state.Root,
		WorkspacePath: state.Path,
		Config:        cfg,
		Store:         database,
		Query:         querier,
		Context:       contextService,
		Knowledge:     knowledgeRepo,
		Git:           review.ExecGit{},
		Refresh: func(ctx context.Context) (string, error) {
			if _, err := project.Refresh(ctx); err != nil {
				return "", err
			}
			return database.GetMetaContext(ctx, "cli_sig")
		},
	})
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
	return p.refreshWithProgress(ctx, nil)
}

// Embedding is never rationed. It runs in-process against the bundled code
// embedder at roughly 650 chunks/second, so a repository's whole backlog is
// seconds to a minute of local CPU -- there is nothing to ration. This used to be
// capped per invocation because embeddings could be routed to a remote Ollama
// model at ~47 chunks/second, where a full build took tens of minutes; that path
// is gone. A binary upgrade re-chunks every file and so invalidates every vector,
// and draining here is what keeps semantic search whole immediately afterwards
// instead of creeping back a couple of seconds per command.

// withRefreshLock serializes derived-index mutation against other goroutines in
// this process and other prowl processes on the same project.
func (p *Project) withRefreshLock(ctx context.Context, mutate func() error) error {
	if p.closed.Load() {
		return errors.New("project is closed")
	}
	select {
	case p.refreshGate <- struct{}{}:
		defer func() { <-p.refreshGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.closed.Load() {
		return errors.New("project is closed")
	}
	fileLock := flock.New(filepath.Join(p.Workspace.Derived, "index-refresh.lock"))
	locked, err := fileLock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("project refresh lock was not acquired")
	}
	return errors.Join(mutate(), fileLock.Unlock())
}

func (p *Project) refreshWithProgress(ctx context.Context, report index.ProgressReporter) (RefreshResult, error) {
	reporter := report
	if report != nil {
		lastProgress := -1
		reporter = func(snapshot index.Progress) error {
			if snapshot.Percent < lastProgress {
				return nil
			}
			lastProgress = snapshot.Percent
			return report(snapshot)
		}
	}
	var result RefreshResult
	err := p.withRefreshLock(ctx, func() error {
		var err error
		for range 2 {
			result, err = p.refresh(ctx, reporter, index.VectorBudget{})
			if !errors.Is(err, errSourcesChanged) {
				break
			}
		}
		if err != nil {
			if stateErr := p.Store.SetMeta("index_state", "incomplete"); stateErr != nil {
				err = errors.Join(err, stateErr)
			}
		}
		return err
	})
	return result, err
}

// refreshVectors embeds outstanding chunks without touching the structural
// index. An incomplete embedding backlog used to force a full reindex of the
// repository on every command; the structural index is already current here, so
// only the derived vectors need work.
func (p *Project) refreshVectors(ctx context.Context, budget index.VectorBudget) (RefreshResult, error) {
	if p.Inferencer == nil || !inferencerEmbeds(p.Inferencer) {
		return RefreshResult{}, nil
	}
	remaining, err := p.Store.CountChunksWithoutVectors()
	if err != nil {
		return RefreshResult{}, err
	}
	stored, _ := p.Store.GetMeta("embed_model")
	if remaining == 0 && stored == p.embedModel() {
		return RefreshResult{}, nil
	}
	var result RefreshResult
	lockErr := p.withRefreshLock(ctx, func() error {
		pass, embedErr := p.embedPass(ctx, budget)
		result.Embedded, result.VectorsRemaining, result.EmbeddingError = pass.Embedded, pass.Remaining, embedErr
		if embedErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	return result, lockErr
}

var errSourcesChanged = index.ErrSourcesChanged

func (p *Project) refresh(ctx context.Context, report index.ProgressReporter, budget index.VectorBudget) (RefreshResult, error) {
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

	var summary index.Summary
	if report == nil {
		summary, err = index.IndexWithOptionsContext(ctx, current, p.Workspace.Root, opt)
	} else {
		summary, err = index.IndexWithOptionsProgressContext(ctx, current, p.Workspace.Root, opt, report)
	}
	if err != nil {
		return result, err
	}
	result.Summary = summary
	if p.afterIndex != nil {
		p.afterIndex()
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

	// Vectors are a derived cache, not part of the atomic structural generation.
	// Embedding them after the commit is what makes a large repo tractable: a
	// first pass over ~100k chunks takes tens of minutes, and inside the
	// generation transaction every interruption rolled all of it back, so the
	// semantic index could never finish. Each batch is now durable on its own
	// and the next pass resumes from the remaining backlog.
	pass, embedErr := p.embedPass(ctx, budget)
	result.Embedded, result.VectorsRemaining, result.EmbeddingError = pass.Embedded, pass.Remaining, embedErr
	if embedErr != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

// embedPass runs one budgeted, resumable embedding pass against the published
// store. A project with no embedding backend has no work to do.
func (p *Project) embedPass(ctx context.Context, budget index.VectorBudget) (index.VectorPass, error) {
	if p.Inferencer == nil || !inferencerEmbeds(p.Inferencer) {
		return index.VectorPass{}, nil
	}
	return index.BuildVectors(ctx, p.Store, p.Inferencer, p.embedModel(), budget, p.vectorProgress)
}

// embedModel is the model identity stored vectors are keyed by.
func (p *Project) embedModel() string {
	return embedModelID(p.Inferencer)
}

// BuildSemanticIndex drains the embedding backlog with no time budget, reporting
// progress after each committed batch. It is the explicit counterpart to the
// bounded pass every command runs: on a large repo the first build is thousands
// of model round trips, so it must be both visible and resumable rather than a
// silent wait a user can only lose by interrupting.
func (p *Project) BuildSemanticIndex(ctx context.Context, progress func(index.VectorPass)) (RefreshResult, error) {
	if p.Inferencer == nil || !inferencerEmbeds(p.Inferencer) {
		return RefreshResult{}, nil
	}
	var result RefreshResult
	lockErr := p.withRefreshLock(ctx, func() error {
		pass, embedErr := index.BuildVectors(ctx, p.Store, p.Inferencer, p.embedModel(), index.VectorBudget{}, progress)
		result.Embedded, result.VectorsRemaining, result.EmbeddingError = pass.Embedded, pass.Remaining, embedErr
		if embedErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	return result, lockErr
}

// ensureFresh brings the derived index up to date before any query is served. A
// stale structural index is reindexed and the embedding backlog is then drained,
// so a prowl upgrade heals itself on the next command: a new binary revision
// forces a full re-parse, which re-chunks every file and thereby invalidates
// every vector, and semantic search is whole again when the command answers
// rather than creeping back over hundreds of invocations.
func (p *Project) ensureFresh(ctx context.Context) (RefreshResult, error) {
	opt := index.Options{Ignore: p.Config.Ignore, Languages: p.Config.Languages}
	sig, sigErr := index.SignatureWithOptionsContext(ctx, p.Workspace.Root, opt)
	if sigErr != nil && ctx.Err() != nil {
		return RefreshResult{}, ctx.Err()
	}
	oldSig, _ := p.Store.GetMeta("cli_sig")
	oldVersion, _ := p.Store.GetMeta("index_version")
	indexState, _ := p.Store.GetMeta("index_state")
	if sigErr == nil && indexState == "complete" && oldVersion == index.Version() && oldSig == strconv.FormatUint(sig, 16) {
		return p.refreshVectors(ctx, index.VectorBudget{})
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
			p.closeErr = errors.Join(p.closeErr, p.Store.Close())
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
