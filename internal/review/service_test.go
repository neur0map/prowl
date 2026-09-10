package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/config"
	"github.com/prowl-agent/prowl-agent/internal/query"
)

type serviceTestStore struct {
	artifacts PlanArtifacts
	result    SaveResult
	saveErr   error
	loads     int
	saves     int
	leases    int
}

func (s *serviceTestStore) NewSnapshotLease(context.Context) (*SnapshotLease, error) {
	s.leases++
	return &SnapshotLease{consumed: true}, nil
}

func (s *serviceTestStore) Save(_ context.Context, artifacts PlanArtifacts, _ *SnapshotLease) (SaveResult, error) {
	s.saves++
	s.artifacts = artifacts
	return s.result, s.saveErr
}

func (s *serviceTestStore) Load(context.Context, string) (PlanArtifacts, error) {
	s.loads++
	return s.artifacts, nil
}

func (s *serviceTestStore) LockReview(context.Context, string) (func(), error) {
	return func() {}, nil
}

func serviceCapture(kind ScopeKind, marker byte) Capture {
	headKind := SideGitOID
	head := make([]byte, 20)
	if kind == ScopeWorkspace {
		headKind = SideWorkspaceSHA256
		head = make([]byte, 32)
	}
	head[0] = marker
	return Capture{
		Scope: Scope{
			Kind:         kind,
			ObjectFormat: "sha1",
			Base:         SideIdentity{Kind: SideGitOID, Value: make([]byte, 20)},
			Head:         SideIdentity{Kind: headKind, Value: head},
			Digest:       Digest{marker},
		},
		CanonicalPatch: Digest{marker},
	}
}

func serviceArtifacts(capture Capture, signature string) PlanArtifacts {
	identity := PlanIdentity{
		PlannerVersion: "service.test.v1",
		ScopeDigest:    capture.Scope.Digest,
		IndexSchema:    "prowl.index.v1",
		IndexVersion:   "1",
	}
	identityBytes := ReviewPlanIdentityV1(identity)
	full := sha256.Sum256(identityBytes)
	plan := Plan{
		Schema:     PlanSchemaV1,
		ReviewID:   PublicID(ReviewIDPrefixV1, full, PublicIDReviewBytesV1).Public,
		PlanDigest: hex.EncodeToString(full[:]),
		Mode:       ModeDirect,
		Scope:      capture.Scope,
	}
	return PlanArtifacts{Plan: plan, PlanIdentity: identity, PlanIdentityBytes: identityBytes, PublishedIndexSignature: signature}
}

func orchestrationService(t *testing.T, kind ScopeKind, captures []Capture, signatures []string) (*Service, *serviceTestStore, *int, *int, *int) {
	t.Helper()
	refreshes := 0
	closes := 0
	store := &serviceTestStore{}
	svc := NewService(ServiceOptions{
		Root: ".",
		Refresh: func(context.Context) (string, error) {
			if refreshes >= len(signatures) {
				t.Fatalf("unexpected refresh %d", refreshes+1)
			}
			sig := signatures[refreshes]
			refreshes++
			return sig, nil
		},
	})
	svc.store = store
	svc.resolve = func(context.Context, PlanRequest) (Scope, error) {
		return serviceCapture(kind, 1).Scope, nil
	}
	captureIndex := 0
	svc.captureOnce = func(context.Context, Scope) (Capture, error) {
		if captureIndex >= len(captures) {
			t.Fatalf("unexpected capture %d", captureIndex+1)
		}
		capture := captures[captureIndex]
		captureIndex++
		return capture, nil
	}
	svc.openView = func(context.Context, Scope, *SnapshotLease) (*HeadView, error) {
		return &HeadView{Close: func() error { closes++; return nil }}, nil
	}
	svc.assemble = func(_ context.Context, capture Capture, _ *HeadView, signature string, _ bool) (PlanArtifacts, error) {
		return serviceArtifacts(capture, signature), nil
	}
	return svc, store, &captureIndex, &refreshes, &closes
}

func TestServiceCommittedCaptureOnce(t *testing.T) {
	svc, store, captures, refreshes, closes := orchestrationService(t, ScopeCommit, []Capture{serviceCapture(ScopeCommit, 1)}, nil)
	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReviewID == "" || store.saves != 1 || store.leases != 1 {
		t.Fatalf("plan=%+v saves=%d leases=%d", plan, store.saves, store.leases)
	}
	if *captures != 1 || *refreshes != 0 || *closes != 1 {
		t.Fatalf("captures=%d refreshes=%d closes=%d, want 1/0/1", *captures, *refreshes, *closes)
	}
}

func TestServiceWorkspaceTwoCaptureTwoRefreshAcceptRetryFail(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		c := serviceCapture(ScopeWorkspace, 1)
		svc, store, captures, refreshes, closes := orchestrationService(t, ScopeWorkspace, []Capture{c, c}, []string{"sig", "sig"})
		if _, err := svc.Plan(context.Background(), PlanRequest{}); err != nil {
			t.Fatal(err)
		}
		if store.saves != 1 || *captures != 2 || *refreshes != 2 || *closes != 1 {
			t.Fatalf("saves=%d captures=%d refreshes=%d closes=%d", store.saves, *captures, *refreshes, *closes)
		}
	})

	t.Run("retry whole transaction", func(t *testing.T) {
		a, b, c := serviceCapture(ScopeWorkspace, 1), serviceCapture(ScopeWorkspace, 2), serviceCapture(ScopeWorkspace, 3)
		svc, store, captures, refreshes, closes := orchestrationService(t, ScopeWorkspace, []Capture{a, b, c, c}, []string{"s1", "s1", "s2", "s2"})
		if _, err := svc.Plan(context.Background(), PlanRequest{}); err != nil {
			t.Fatal(err)
		}
		if store.saves != 1 || *captures != 4 || *refreshes != 4 || *closes != 2 {
			t.Fatalf("saves=%d captures=%d refreshes=%d closes=%d", store.saves, *captures, *refreshes, *closes)
		}
	})

	t.Run("typed stale after second mismatch", func(t *testing.T) {
		a, b := serviceCapture(ScopeWorkspace, 1), serviceCapture(ScopeWorkspace, 2)
		svc, store, captures, refreshes, closes := orchestrationService(t, ScopeWorkspace, []Capture{a, b, a, b}, []string{"s", "s", "s", "s"})
		if _, err := svc.Plan(context.Background(), PlanRequest{}); !errors.Is(err, ErrStale) {
			t.Fatalf("error=%v, want ErrStale", err)
		}
		if store.saves != 0 || *captures != 4 || *refreshes != 4 || *closes != 2 {
			t.Fatalf("saves=%d captures=%d refreshes=%d closes=%d", store.saves, *captures, *refreshes, *closes)
		}
	})
}

func TestServiceWorkspaceReuseUnavailableRetriesWholeTransaction(t *testing.T) {
	capture := serviceCapture(ScopeWorkspace, 1)
	svc, store, captures, refreshes, closes := orchestrationService(
		t,
		ScopeWorkspace,
		[]Capture{capture, capture},
		[]string{"sig", "sig"},
	)
	opens := 0
	svc.openView = func(context.Context, Scope, *SnapshotLease) (*HeadView, error) {
		opens++
		return nil, ErrReuseUnavailable
	}

	_, err := svc.Plan(context.Background(), PlanRequest{})
	var stale *StaleError
	if !errors.As(err, &stale) || !errors.Is(err, ErrStale) {
		t.Fatalf("error=%T %v, want typed StaleError", err, err)
	}
	if !strings.Contains(stale.Reason, ErrReuseUnavailable.Error()) {
		t.Fatalf("stale reason=%q, want ErrReuseUnavailable", stale.Reason)
	}
	if store.saves != 0 || *captures != 2 || *refreshes != 2 || opens != 2 || *closes != 0 {
		t.Fatalf("saves=%d captures=%d refreshes=%d opens=%d closes=%d, want 0/2/2/2/0", store.saves, *captures, *refreshes, opens, *closes)
	}
}

func TestServiceCleanupReuseAndPruneWarning(t *testing.T) {
	capture := serviceCapture(ScopeCommit, 1)
	t.Run("build error closes view", func(t *testing.T) {
		svc, store, _, _, closes := orchestrationService(t, ScopeCommit, []Capture{capture}, nil)
		boom := errors.New("build failed")
		svc.assemble = func(context.Context, Capture, *HeadView, string, bool) (PlanArtifacts, error) {
			return PlanArtifacts{}, boom
		}
		if _, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"}); !errors.Is(err, boom) {
			t.Fatalf("error=%v", err)
		}
		if store.saves != 0 || *closes != 1 {
			t.Fatalf("saves=%d closes=%d", store.saves, *closes)
		}
	})

	t.Run("save error closes view", func(t *testing.T) {
		svc, store, _, _, closes := orchestrationService(t, ScopeCommit, []Capture{capture}, nil)
		boom := errors.New("save failed")
		store.saveErr = boom
		if _, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"}); !errors.Is(err, boom) {
			t.Fatalf("error=%v", err)
		}
		if store.saves != 1 || *closes != 1 {
			t.Fatalf("saves=%d closes=%d", store.saves, *closes)
		}
	})

	t.Run("identical reuse", func(t *testing.T) {
		svc, store, _, _, _ := orchestrationService(t, ScopeCommit, []Capture{capture}, nil)
		store.result = SaveResult{ReviewID: "rvw_service", Reused: true}
		if _, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"}); err != nil {
			t.Fatal(err)
		}
		if !store.result.Reused || store.saves != 1 {
			t.Fatalf("result=%+v saves=%d", store.result, store.saves)
		}
	})

	t.Run("accepted plan and separate prune warning", func(t *testing.T) {
		svc, store, _, _, _ := orchestrationService(t, ScopeCommit, []Capture{capture}, nil)
		boom := errors.New("prune failed")
		store.result = SaveResult{ReviewID: "rvw_service", PruneWarning: boom}
		plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD"})
		var warning *PruneWarning
		if plan.ReviewID == "" || !errors.As(err, &warning) || !errors.Is(err, boom) {
			t.Fatalf("plan=%+v error=%v", plan, err)
		}
	})
}

func TestServiceSymlinkNULOmissionRequiresUnreviewableAccounting(t *testing.T) {
	plan := Plan{ChangedPaths: []PlanPath{{PathID: "p_x", NewPath: "bad-link", ReviewClass: string(ReviewClassUnreviewable), Coverage: string(PathCoverageNone)}}}
	if err := validateSnapshotOmissions(plan, []Omission{{Path: "bad-link", Side: SideHead, Reason: "symlink target contains NUL"}}); err != nil {
		t.Fatal(err)
	}
	plan.ChangedPaths[0].Coverage = string(PathCoverageFull)
	if err := validateSnapshotOmissions(plan, []Omission{{Path: "bad-link", Side: SideHead, Reason: "symlink target contains NUL"}}); err == nil {
		t.Fatal("reviewable malformed symlink omission was accepted")
	}
}

func TestRelevantSnapshotOmissionsMatchesChangedPathSide(t *testing.T) {
	plan := Plan{ChangedPaths: []PlanPath{{OldPath: "old/name.go", NewPath: "new/name.go"}}}
	omissions := []Omission{
		{Path: "old/name.go", Side: SideBase, Reason: "base"},
		{Path: "new/name.go", Side: SideHead, Reason: "head"},
		{Path: "old/name.go", Side: SideHead, Reason: "wrong head"},
		{Path: "new/name.go", Side: SideBase, Reason: "wrong base"},
		{Path: "unrelated", Side: SideHead, Reason: "unrelated"},
	}
	got := relevantSnapshotOmissions(plan, omissions)
	if len(got) != 2 || got[0].Reason != "base" || got[1].Reason != "head" {
		t.Fatalf("relevant omissions=%+v, want exact base-old/head-new matches", got)
	}
}

func TestServiceBuildArtifactsPersistAndUnitRoundTrip(t *testing.T) {
	record := RawPathRecord{Kind: "tracked", Status: "M", OldPath: "a.go", NewPath: "a.go", TextClass: string(TextClassText), Additions: 1, Deletions: 1, Hunks: []RawHunk{{Ordinal: 0, OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1, Payload: []byte("-func a() {}\n+func a() { println(1) }\n")}}}
	capture := captureFor(record)
	capture.CanonicalPatch = CanonicalPatchDigest(capture.Paths)
	capture.Scope.Digest = ScopeDigest(capture.Scope.ObjectFormat, capture.Scope.Base, capture.Scope.Head, capture.Scope.Kind, capture.CanonicalPatch)
	view := &HeadView{
		Sources: mapSourceResolver{
			"base:a.go": []byte("package p\n\nfunc a() {}\n"),
			"head:a.go": []byte("package p\n\nfunc a() { println(1) }\n"),
		},
	}
	svc := NewService(ServiceOptions{Root: "."})
	graph := basicGraph("a.go")
	graph.clusters = []query.Cluster{{Label: "cluster-a", Files: []string{"a.go"}}}
	svc.graph = graph
	artifacts, err := svc.buildArtifacts(context.Background(), capture, view, "sig", true)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	lease := makeLease(t, store, "service-round-trip")
	if _, err := store.Save(context.Background(), artifacts, lease); err != nil {
		t.Fatal(err)
	}
	svc.store = store
	unitID := artifacts.Plan.PrimaryUnits[0].UnitID
	unit, err := svc.Unit(context.Background(), artifacts.Plan.ReviewID, unitID, UnitRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if unit.UnitID != unitID {
		t.Fatalf("unit=%s want %s", unit.UnitID, unitID)
	}
}

type countingCaptureRunner struct {
	CaptureRunner
	rawStatusCalls int
}

func (r *countingCaptureRunner) RawStatus(ctx context.Context, root string, limit int64, args ...string) ([]byte, error) {
	r.rawStatusCalls++
	return r.CaptureRunner.RawStatus(ctx, root, limit, args...)
}

func TestServiceCommittedEndToEndDirectAndForcedStructured(t *testing.T) {
	tests := []struct {
		name            string
		forceStructured bool
		wantMode        Mode
	}{
		{name: "direct below threshold", wantMode: ModeDirect},
		{name: "forced structured below threshold", forceStructured: true, wantMode: ModeStructured},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			fixture.commitFile(t, "a.go", "package p\n\nfunc A(v int) int { return v }\n")
			fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 2 }\n")
			runner := &countingCaptureRunner{CaptureRunner: execRunner(t)}
			svc := NewService(ServiceOptions{Root: fixture.root, Config: config.Default(), Git: runner})

			plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD", ForceStructured: tt.forceStructured})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Mode != tt.wantMode || plan.StructuredRequired {
				t.Fatalf("mode=%s structured_required=%v, want %s/false", plan.Mode, plan.StructuredRequired, tt.wantMode)
			}
			if runner.rawStatusCalls != 1 {
				t.Fatalf("RawStatus calls=%d, want one committed capture", runner.rawStatusCalls)
			}
			if len(plan.PrimaryUnits) != 1 {
				t.Fatalf("units=%d, want one", len(plan.PrimaryUnits))
			}
			if tt.wantMode == ModeDirect && (len(plan.Cohorts) != 0 || len(plan.RequiredAudits) != 0) {
				t.Fatalf("direct plan exposed structured collections: %+v", plan)
			}
			if tt.wantMode == ModeStructured && len(plan.Cohorts) == 0 {
				t.Fatal("forced structured plan omitted cohorts")
			}
			planStore, err := OpenPlanStore(context.Background(), runner, fixture.root)
			if err != nil {
				t.Fatal(err)
			}
			loaded, loadErr := planStore.Load(context.Background(), plan.ReviewID)
			closeErr := planStore.Close()
			if loadErr != nil || closeErr != nil {
				t.Fatalf("load err=%v close err=%v", loadErr, closeErr)
			}
			if loaded.Plan.Mode != tt.wantMode || len(loaded.PlanIdentity.Cohorts) == 0 {
				t.Fatalf("persisted mode=%s identity cohorts=%d", loaded.Plan.Mode, len(loaded.PlanIdentity.Cohorts))
			}
			packet, err := svc.UnitPacket(context.Background(), plan.ReviewID, plan.PrimaryUnits[0].UnitID, UnitRequest{BudgetBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			if packet.Mandatory.ReviewID != plan.ReviewID || packet.Mandatory.UnitID != plan.PrimaryUnits[0].UnitID {
				t.Fatalf("unit=%+v plan=%+v", packet.Mandatory, plan)
			}
			wantCheck := "prowl-agent review check --review " + plan.ReviewID + " --report review-results.json"
			if len(packet.NextCommands) == 0 || packet.NextCommands[len(packet.NextCommands)-1].Command != wantCheck {
				t.Fatalf("next commands=%+v, want final %q", packet.NextCommands, wantCheck)
			}
			ownFetch := "prowl-agent review unit " + plan.ReviewID + "/" + plan.PrimaryUnits[0].UnitID
			for _, command := range packet.NextCommands {
				if command.Command == ownFetch {
					t.Fatalf("unit packet retained its own fetch command: %+v", packet.NextCommands)
				}
			}
		})
	}
}

func TestServiceCommittedPassesVerifiedReuseAndFallsBack(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 0 }\n")
	fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 1 }\n")
	older := fixture.revParse(t, "HEAD")
	fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 2 }\n")
	db := indexCurrent(t, fixture.root)
	defer db.Close()
	runner := execRunner(t)
	svc := NewService(ServiceOptions{Root: fixture.root, Store: db, Git: runner})
	planStore, err := OpenPlanStore(context.Background(), runner, fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	defer planStore.Close()

	t.Run("clean exact HEAD reuses project index", func(t *testing.T) {
		scope, err := ResolveScope(context.Background(), runner, fixture.root, PlanRequest{Commit: "HEAD"})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := planStore.NewSnapshotLease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		view, err := svc.openHeadView(context.Background(), scope, lease)
		if err != nil {
			t.Fatal(err)
		}
		defer closeHeadView(view)
		if view.Store != db {
			t.Fatal("eligible committed view materialized instead of reusing project index")
		}
	})

	t.Run("mismatched HEAD materializes", func(t *testing.T) {
		scope, err := ResolveScope(context.Background(), runner, fixture.root, PlanRequest{Commit: older})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := planStore.NewSnapshotLease(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		view, err := svc.openHeadView(context.Background(), scope, lease)
		if err != nil {
			t.Fatal(err)
		}
		defer closeHeadView(view)
		if view.Store == db {
			t.Fatal("mismatched committed head reused the project index")
		}
	})
}

func TestServiceCommittedIgnoresUnchangedHostileSnapshotOmission(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 1 }\n")
	fixture.commitBlobEntry(t, "120000", "unrelated-evil", "target\x00injection")
	fixture.commitBlobEntry(t, "120000", "unrelated-oversized-link", strings.Repeat("x", int(MaxSymlinkTargetBytesV1)+1))
	fixture.commitFile(t, "a.go", "package p\n\nfunc A() int { return 2 }\n")
	svc := NewService(ServiceOptions{Root: fixture.root, Config: config.Default(), Git: execRunner(t)})

	plan, err := svc.Plan(context.Background(), PlanRequest{Commit: "HEAD", ForceStructured: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ChangedPaths) != 1 || plan.ChangedPaths[0].NewPath != "a.go" {
		t.Fatalf("changed paths=%+v", plan.ChangedPaths)
	}
	if plan.ChangedPaths[0].ReviewClass == string(ReviewClassUnreviewable) || plan.ChangedPaths[0].Coverage == string(PathCoverageNone) {
		t.Fatalf("unrelated omission degraded changed path: %+v", plan.ChangedPaths[0])
	}
}
