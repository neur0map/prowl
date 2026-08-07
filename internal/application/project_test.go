package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/prowl-agent/prowl-agent/internal/assist"
	"github.com/prowl-agent/prowl-agent/internal/boundedio"
	"github.com/prowl-agent/prowl-agent/internal/config"
	contextpacket "github.com/prowl-agent/prowl-agent/internal/context"
	"github.com/prowl-agent/prowl-agent/internal/store"
	"github.com/prowl-agent/prowl-agent/internal/workspace"
)

func TestOpenProjectResolvesWorkspaceFromParentDirectory(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	start := filepath.Join(root, "internal", "nested")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}

	project, err := OpenProject(context.Background(), start, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()

	if project.Workspace.Root != root {
		t.Fatalf("workspace root = %q, want %q", project.Workspace.Root, root)
	}
	if project.Store == nil || project.Query == nil || project.Knowledge == nil || project.Context == nil || project.Capabilities == nil {
		t.Fatalf("incomplete project assembly: %+v", project)
	}
	packet, err := project.Context.Search(contextpacket.Request{Question: "OriginalSymbol", Mode: contextpacket.ModeCompact, BudgetTokens: 1000})
	if err != nil || len(packet.Items) == 0 {
		t.Fatalf("context search after assembly = %+v, %v", packet, err)
	}
}

func TestOpenProjectReturnsMalformedConfigError(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	configPath := filepath.Join(root, workspace.Dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("languages = [\"go\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	project, err := OpenProject(context.Background(), root, Options{})
	if err == nil {
		if project != nil {
			_ = project.Close()
		}
		t.Fatal("OpenProject succeeded with malformed config")
	}
	if project != nil {
		t.Fatalf("project = %+v, want nil on configuration error", project)
	}
	if !strings.Contains(err.Error(), "load project config") {
		t.Fatalf("error = %q, want configuration context", err)
	}
}

func TestStartupFreshnessOpensCurrentProjectWithoutRefreshing(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	seed, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Second, CandidatePaths: 2000})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	if project.InitialRefresh != (RefreshResult{}) {
		t.Fatalf("bounded startup unexpectedly refreshed: %+v", project.InitialRefresh)
	}
}

func TestStartupFreshnessOpensCurrentProjectWithInRootSourceSymlink(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	if err := os.Symlink("sample.go", filepath.Join(root, "alias.go")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	seed, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Second, CandidatePaths: 2000})
	if err != nil {
		t.Fatalf("fresh canonical generation refused: %v", err)
	}
	defer project.Close()
}

func TestStartupRefreshJobReturnsStaleProjectAsPending(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	seed, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "package sample\n\nfunc StaleAtStartup() {}\n")

	project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Second, CandidatePaths: 2000})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	if !project.StartupRefreshPending() {
		t.Fatal("stale project did not report pending startup refresh")
	}
}

func TestStartupFreshnessCandidateAndDeadlineBoundsReturnTypedError(t *testing.T) {
	t.Run("candidate cap", func(t *testing.T) {
		root := newProjectFixture(t, config.Default())
		if err := os.WriteFile(filepath.Join(root, "second.go"), []byte("package sample\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Second, CandidatePaths: 1})
		if project != nil || !errors.Is(err, ErrStartupRefreshRequired) {
			t.Fatalf("project=%+v error=%v want startup refresh required", project, err)
		}
	})

	t.Run("single deadline", func(t *testing.T) {
		root := newProjectFixture(t, config.Default())
		project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Nanosecond, CandidatePaths: 2000})
		if project != nil || !errors.Is(err, ErrStartupRefreshRequired) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("project=%+v error=%v want typed deadline", project, err)
		}
	})

	t.Run("contended generation probe", func(t *testing.T) {
		root := newProjectFixture(t, config.Default())
		seed, err := OpenProject(context.Background(), root, Options{})
		if err != nil {
			t.Fatal(err)
		}
		lockPath := filepath.Join(seed.Workspace.Path, "index-refresh.lock")
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		lock := flock.New(lockPath)
		if err := lock.Lock(); err != nil {
			t.Fatal(err)
		}
		defer lock.Unlock()
		started := time.Now()
		project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: 30 * time.Millisecond, CandidatePaths: 2000})
		if project != nil || !errors.Is(err, ErrStartupRefreshRequired) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("project=%+v error=%v want typed lock deadline", project, err)
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("bounded startup took %v", elapsed)
		}
	})

	t.Run("contended store assembly", func(t *testing.T) {
		root := newProjectFixture(t, config.Default())
		seed, err := OpenProject(context.Background(), root, Options{})
		if err != nil {
			t.Fatal(err)
		}
		dbPath := seed.Workspace.DB
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		locker, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatal(err)
		}
		defer locker.Close()
		if _, err := locker.Exec(`PRAGMA locking_mode=EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		defer locker.Exec(`ROLLBACK`)
		started := time.Now()
		project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: 40 * time.Millisecond, CandidatePaths: 2000})
		if project != nil || !errors.Is(err, ErrStartupRefreshRequired) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("project=%+v error=%v want typed store deadline", project, err)
		}
		if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
			t.Fatalf("bounded store startup took %v", elapsed)
		}
	})
}

func TestStartupFreshnessFIFOConfigReturnsWithinDefaultDeadline(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := newProjectFixture(t, config.Default())
	configPath := filepath.Join(root, ".prowl", "config.toml")
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("mkfifo", configPath).Run(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	project, err := OpenWorkbenchProject(context.Background(), root, Options{}, DefaultStartupLimits())
	if project != nil || !errors.Is(err, boundedio.ErrNonRegular) {
		t.Fatalf("project=%+v error=%v want non-regular config refusal", project, err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("FIFO config escaped startup bound: %v", elapsed)
	}
}

func TestStartupFreshnessMalformedConfigIsNotRefreshRequired(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	configPath := filepath.Join(root, workspace.Dir, "config.toml")
	if err := os.WriteFile(configPath, []byte("languages = [\"go\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := OpenWorkbenchProject(context.Background(), root, Options{}, StartupLimits{Timeout: time.Second, CandidatePaths: 2000})
	if project != nil || err == nil || errors.Is(err, ErrStartupRefreshRequired) || !strings.Contains(err.Error(), "load project config") {
		t.Fatalf("project=%+v error=%v want malformed config", project, err)
	}
}

func TestOpenProjectRefreshesStaleStructuralIndex(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	first, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	writeSource(t, root, "package sample\n\nfunc RefreshedSymbol() {}\n")
	second, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	hits, err := second.Query.FindSymbol("RefreshedSymbol")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].File != "sample.go" {
		t.Fatalf("refreshed hits = %+v", hits)
	}
}

func TestOpenProjectRepairsIncompleteIndexGeneration(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	first, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Store.SetMeta("index_state", "incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "package sample\n\nfunc RecoveredSymbol() {}\n")

	second, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	state, err := second.Store.GetMeta("index_state")
	if err != nil || state != "complete" {
		t.Fatalf("repaired index_state = %q, %v; want complete", state, err)
	}
	hits, err := second.Query.FindSymbol("RecoveredSymbol")
	if err != nil || len(hits) != 1 {
		t.Fatalf("recovered query = %+v, %v", hits, err)
	}
}

func TestOpenProjectHonorsCanceledRefresh(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	root := newProjectFixture(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())

	project, err := OpenProject(ctx, root, Options{
		EnableAI: true,
		InferencerProvider: func(context.Context, config.Config) assist.Inferencer {
			// Providers are resolved after the store opens and immediately before
			// freshness work, making this a deterministic refresh cancellation.
			cancel()
			return nil
		},
	})
	if project != nil {
		_ = project.Close()
		t.Fatalf("project = %+v, want nil", project)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestProjectCloseIsIdempotent(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			errors <- project.Close()
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent Close: %v", err)
		}
	}
	if err := project.Close(); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}
	if _, err := project.Query.FindSymbol("OriginalSymbol"); err == nil {
		t.Fatal("query succeeded after project close")
	}
}

func TestOpenProjectIsDeterministicWithoutModel(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	cfg.AI.OllamaURL = "http://127.0.0.1:1"
	root := newProjectFixture(t, cfg)

	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatalf("no-model OpenProject: %v", err)
	}
	defer project.Close()

	hits, err := project.Query.FindSymbol("OriginalSymbol")
	if err != nil || len(hits) != 1 {
		t.Fatalf("deterministic query = %+v, %v", hits, err)
	}
	if project.Context.Reranker != nil {
		t.Fatalf("no-model context reranker = %#v, want nil", project.Context.Reranker)
	}
	aiEnabled, err := project.Store.GetMeta("ai_enabled")
	if err != nil {
		t.Fatal(err)
	}
	if aiEnabled != "true" {
		t.Fatalf("configured AI metadata = %q, want true", aiEnabled)
	}
}

func TestOpenProjectAddsOptionalAIAndEmbeddings(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	root := newProjectFixture(t, cfg)
	inferencer := &recordingInferencer{}
	providerCalls := 0

	project, err := OpenProject(context.Background(), root, Options{
		EnableAI: true,
		InferencerProvider: func(_ context.Context, loaded config.Config) assist.Inferencer {
			providerCalls++
			if !loaded.AI.Enabled {
				t.Fatal("provider received disabled AI config")
			}
			return inferencer
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()

	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want 1", providerCalls)
	}
	if inferencer.embedCalls.Load() == 0 {
		t.Fatal("optional inferencer did not build embeddings")
	}
	if project.Context.Reranker == nil {
		t.Fatal("optional inferencer was not attached to context service")
	}
}

func TestOpenProjectRetriesIncompleteVectors(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	root := newProjectFixture(t, cfg)

	failed, err := OpenProject(context.Background(), root, Options{
		EnableAI: true,
		InferencerProvider: func(context.Context, config.Config) assist.Inferencer {
			return failingInferencer{}
		},
	})
	if err != nil {
		t.Fatalf("structural fallback after embedding error: %v", err)
	}
	complete, err := failed.Store.GetMeta("vectors_complete")
	if err != nil {
		t.Fatal(err)
	}
	if complete != "0" {
		t.Fatalf("failed vector generation = %q, want 0", complete)
	}
	if err := failed.Close(); err != nil {
		t.Fatal(err)
	}

	healthyInferencer := &recordingInferencer{}
	healthy, err := OpenProject(context.Background(), root, Options{
		EnableAI: true,
		InferencerProvider: func(context.Context, config.Config) assist.Inferencer {
			return healthyInferencer
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	if healthyInferencer.embedCalls.Load() == 0 {
		t.Fatal("incomplete vectors were not retried")
	}
	complete, err = healthy.Store.GetMeta("vectors_complete")
	if err != nil {
		t.Fatal(err)
	}
	if complete != "1" {
		t.Fatalf("repaired vector generation = %q, want 1", complete)
	}
}

func TestProjectRefreshCancellationWhileLocallyContended(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	project.refreshGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = project.Refresh(ctx)
	<-project.refreshGate
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended refresh error = %v, want deadline exceeded", err)
	}
}

func TestProjectRefreshCancellationWhileProcessLockContended(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	lock := flock.New(filepath.Join(project.Workspace.Path, "index-refresh.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = project.Refresh(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("process-lock refresh error = %v, want deadline exceeded", err)
	}
}

func TestOpenProjectRebuildsVectorsAfterModelChange(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	cfg.AI.EmbedModel = "model-a"
	root := newProjectFixture(t, cfg)
	firstInferencer := &recordingInferencer{}
	first, err := OpenProject(context.Background(), root, Options{EnableAI: true, InferencerProvider: func(context.Context, config.Config) assist.Inferencer { return firstInferencer }})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := workspace.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AI.EmbedModel = "model-b"
	if err := config.Save(state.Path, cfg); err != nil {
		t.Fatal(err)
	}
	secondInferencer := &recordingInferencer{}
	second, err := OpenProject(context.Background(), root, Options{EnableAI: true, InferencerProvider: func(context.Context, config.Config) assist.Inferencer { return secondInferencer }})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if secondInferencer.embedCalls.Load() == 0 {
		t.Fatal("embedding model change did not rebuild vectors")
	}
	model, err := second.Store.GetMeta("embed_model")
	if err != nil {
		t.Fatal(err)
	}
	if model != "model-b" {
		t.Fatalf("embed_model = %q, want model-b", model)
	}
}

func TestProjectRefreshRetriesChangedSourcesThenPublishes(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	writeSource(t, root, "package sample\n\nfunc BeforeRetry() {}\n")
	var calls atomic.Int32
	project.afterIndex = func() {
		if calls.Add(1) == 1 {
			writeSource(t, root, "package sample\n\nfunc AfterRetry() {}\n")
		}
	}
	if _, err := project.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("index attempts = %d, want 2", calls.Load())
	}
	hits, err := project.Query.FindSymbol("AfterRetry")
	if err != nil || len(hits) != 1 {
		t.Fatalf("retried generation query = %+v, %v", hits, err)
	}
}

func TestProjectRefreshRepeatedSourceChangesRemainUnpublished(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	writeSource(t, root, "package sample\n\nfunc PendingRefresh() {}\n")
	var calls atomic.Int32
	project.afterIndex = func() {
		n := calls.Add(1)
		writeSource(t, root, fmt.Sprintf("package sample\n\nfunc Changed%d() {}\n", n))
	}
	if _, err := project.Refresh(context.Background()); !errors.Is(err, errSourcesChanged) {
		t.Fatalf("refresh error = %v, want sources-changed", err)
	}
	state, err := project.Store.GetMeta("index_state")
	if err != nil || state != "incomplete" {
		t.Fatalf("failed generation state = %q, %v", state, err)
	}
	if _, err := project.Query.FindSymbol("OriginalSymbol"); !errors.Is(err, store.ErrGenerationIncomplete) {
		t.Fatalf("query error = %v, want incomplete generation", err)
	}
}

func TestProjectRefreshPublishesAtomically(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	writeSource(t, root, "package sample\n\nfunc AtomicReplacement() {}\n")
	entered := make(chan struct{})
	release := make(chan struct{})
	project.afterIndex = func() {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := project.Refresh(context.Background())
		done <- err
	}()
	<-entered
	queryDone := make(chan []store.SymbolHit, 1)
	queryErr := make(chan error, 1)
	go func() {
		hits, err := project.Query.FindSymbol("AtomicReplacement")
		queryDone <- hits
		queryErr <- err
	}()
	select {
	case hits := <-queryDone:
		t.Fatalf("logical reader crossed uncommitted refresh: %+v", hits)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	newHits := <-queryDone
	if err := <-queryErr; err != nil || len(newHits) != 1 {
		t.Fatalf("published view = %+v, %v", newHits, err)
	}
}

func TestProjectRefreshRejectsAfterClose(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	project, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := project.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("refresh after close error = %v", err)
	}
}

func TestOpenProjectRebuildsVectorsWithUnknownModelMetadata(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	root := newProjectFixture(t, cfg)
	first, err := OpenProject(context.Background(), root, Options{EnableAI: true, InferencerProvider: func(context.Context, config.Config) assist.Inferencer { return &recordingInferencer{} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Store.SetMeta("embed_model", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	secondInferencer := &recordingInferencer{}
	second, err := OpenProject(context.Background(), root, Options{EnableAI: true, InferencerProvider: func(context.Context, config.Config) assist.Inferencer { return secondInferencer }})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if secondInferencer.embedCalls.Load() == 0 {
		t.Fatal("unknown vector model metadata did not force rebuild")
	}
	model, _ := second.Store.GetMeta("embed_model")
	if model != cfg.AI.EmbedModel || !second.Store.VectorsReady() {
		t.Fatalf("rebuilt model=%q ready=%v", model, second.Store.VectorsReady())
	}
}

func TestOpenProjectDoesNotPublishPartialVectors(t *testing.T) {
	cfg := config.Default()
	cfg.AI.Enabled = true
	root := newProjectFixture(t, cfg)
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("part-%02d.go", i)), []byte(fmt.Sprintf("package sample\nfunc Part%02d() {}\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inferencer := &partialFailInferencer{}
	project, err := OpenProject(context.Background(), root, Options{EnableAI: true, InferencerProvider: func(context.Context, config.Config) assist.Inferencer { return inferencer }})
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	if project.InitialRefresh.EmbeddingError == nil {
		t.Fatal("partial embedding failure was not reported")
	}
	if project.Store.VectorsReady() || project.Store.VectorsInitialized() {
		t.Fatalf("partial vectors published: ready=%v initialized=%v", project.Store.VectorsReady(), project.Store.VectorsInitialized())
	}
	calls := inferencer.calls.Load()
	if _, err := project.Query.SimilarCode(context.Background(), "Part39"); err != nil {
		t.Fatal(err)
	}
	if inferencer.calls.Load() != calls {
		t.Fatal("semantic query used an unpublished partial vector generation")
	}
}

func newProjectFixture(t *testing.T, cfg config.Config) string {
	t.Helper()
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(state.Path, cfg); err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "package sample\n\nfunc OriginalSymbol() {}\n")
	return root
}

func writeSource(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type failingInferencer struct{}

func (failingInferencer) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("embedding unavailable")
}

func (failingInferencer) Generate(context.Context, string) (string, error) {
	return "", errors.New("generation unavailable")
}

func (failingInferencer) Rerank(context.Context, string, []string) ([]int, error) {
	return nil, errors.New("reranking unavailable")
}

type partialFailInferencer struct{ calls atomic.Int32 }

func (inferencer *partialFailInferencer) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if inferencer.calls.Add(1) > 1 {
		return nil, errors.New("embedding failed after first batch")
	}
	vectors := make([][]float32, len(texts))
	for index := range vectors {
		vectors[index] = []float32{1, 0}
	}
	return vectors, nil
}

func (*partialFailInferencer) Generate(context.Context, string) (string, error) { return "", nil }
func (*partialFailInferencer) Rerank(_ context.Context, _ string, documents []string) ([]int, error) {
	order := make([]int, len(documents))
	for index := range order {
		order[index] = index
	}
	return order, nil
}

type recordingInferencer struct{ embedCalls atomic.Int32 }

func (inferencer *recordingInferencer) Embed(_ context.Context, texts []string) ([][]float32, error) {
	inferencer.embedCalls.Add(1)
	vectors := make([][]float32, len(texts))
	for index := range vectors {
		vectors[index] = []float32{1, 0}
	}
	return vectors, nil
}

func (*recordingInferencer) Generate(context.Context, string) (string, error) { return "", nil }

func (*recordingInferencer) Rerank(_ context.Context, _ string, documents []string) ([]int, error) {
	order := make([]int, len(documents))
	for index := range order {
		order[index] = index
	}
	return order, nil
}

func TestProjectRefreshRejectsABARestoredSource(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	path := filepath.Join(root, "main.go")
	original := []byte("package main\nfunc Original() {}\n")
	transient := []byte("package main\nfunc Transient() {}\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.beforeIndex = func() {
		if err := os.WriteFile(path, transient, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p.afterIndex = func() {
		if err := os.WriteFile(path, original, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Refresh(context.Background()); !errors.Is(err, errSourcesChanged) {
		t.Fatalf("ABA refresh error = %v, want %v", err, errSourcesChanged)
	}
	if err := p.Store.RequirePublishedGeneration(); !errors.Is(err, store.ErrGenerationIncomplete) {
		t.Fatalf("published generation after ABA = %v", err)
	}
}

func TestProjectCloseWaitsForActiveRefresh(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	p, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "package sample\nfunc DuringClose() {}\n")
	entered := make(chan struct{})
	release := make(chan struct{})
	p.afterIndex = func() { close(entered); <-release }
	refreshDone := make(chan error, 1)
	go func() { _, err := p.Refresh(context.Background()); refreshDone <- err }()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close crossed active refresh: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if _, err := p.Refresh(context.Background()); err == nil {
		t.Fatal("refresh after close succeeded")
	}
}

func TestProjectRefreshRejectsRestoredFileOmittedDuringIndexWalk(t *testing.T) {
	root := newProjectFixture(t, config.Default())
	p, err := OpenProject(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	path := filepath.Join(root, "sample.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	p.beforeIndex = func() {
		attempts++
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove before index: %v", err)
		}
	}
	p.afterIndex = func() {
		if err := os.WriteFile(path, data, info.Mode()); err != nil {
			t.Fatalf("restore after index: %v", err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatalf("restore mtime: %v", err)
		}
	}
	if _, err := p.Refresh(context.Background()); !errors.Is(err, errSourcesChanged) {
		t.Fatalf("refresh error = %v, want sources changed", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if state, err := p.Store.GetMeta("index_state"); err != nil || state != "incomplete" {
		t.Fatalf("index state = %q, %v", state, err)
	}
	if _, err := p.Query.FindSymbol("OriginalSymbol"); !errors.Is(err, store.ErrGenerationIncomplete) {
		t.Fatalf("query error = %v, want incomplete generation", err)
	}
}
