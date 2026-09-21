package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/selfupdate"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

func sampleStatus() query.Status {
	return query.Status{
		Counts: store.Counts{
			Files: 578, Symbols: 56535, Edges: 3383, Resolved: 34, Resources: 17,
			Langs: map[string]int{"cpp": 508, "bash": 21, "json": 16},
		},
		Savings: query.Savings{Queries: 100, AnswerTokens: 632000, SavedTokens: 3100000},
	}
}

func TestRenderStatusCard(t *testing.T) {
	out := renderStatusCard("v1", "/home/x/proj", "proj", sampleStatus(), selfupdate.Result{Available: true},
		[]projSaving{{Name: "proj", Saved: 3100000}, {Name: "other", Saved: 1000000}},
		query.Savings{Queries: 150, SavedTokens: 4100000})
	for _, want := range []string{"prowl", "proj", "INDEX", "LANGUAGES", "cpp", "TOKENS SAVED", "ACROSS YOUR PROJECTS", "combined", "update available", "measure it yourself"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q", want)
		}
	}
}

func TestRenderStatusCardCombinedFromEmptyProject(t *testing.T) {
	st := sampleStatus()
	st.Savings = query.Savings{} // current project has no queries yet
	out := renderStatusCard("v1", "/home/x/empty", "empty", st, selfupdate.Result{},
		[]projSaving{{Name: "ryoku-arch", Saved: 1300000}},
		query.Savings{Queries: 23, SavedTokens: 1300000})
	for _, want := range []string{"ACROSS YOUR PROJECTS", "ryoku-arch", "across your projects", "1.3M"} {
		if !strings.Contains(out, want) {
			t.Errorf("card missing %q\n%s", want, out)
		}
	}
}

func TestPlainStatusShowsSavingsAndUpdate(t *testing.T) {
	var b strings.Builder
	printPlainStatus(&b, "/p", sampleStatus(), selfupdate.Result{Available: true},
		query.Savings{Queries: 150, SavedTokens: 4100000})
	s := b.String()
	for _, want := range []string{"Files:", "Symbols:", "Saved:", "Combined:", "Update:", "Verify:"} {
		if !strings.Contains(s, want) {
			t.Errorf("plain status missing %q\n%s", want, s)
		}
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int64]string{500: "500", 1500: "1.5k", 56535: "57k", 3100000: "3.1M"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestComma(t *testing.T) {
	if got := comma(56535); got != "56,535" {
		t.Errorf("comma(56535) = %q", got)
	}
	if got := comma(7); got != "7" {
		t.Errorf("comma(7) = %q", got)
	}
	if got := comma(1000000); got != "1,000,000" {
		t.Errorf("comma(1000000) = %q", got)
	}
}

func TestComputeSavingsMargin(t *testing.T) {
	// baseline 4000, answer 0 -> raw 4000 -> *0.7/4 = 700 tokens (conservative).
	if sv := query.ComputeSavings(store.Stats{Queries: 1, BaselineBytes: 4000}); sv.SavedTokens != 700 {
		t.Fatalf("saved = %d, want 700 (0.7 margin)", sv.SavedTokens)
	}
	if sv := query.ComputeSavings(store.Stats{AnswerBytes: 100}); sv.SavedTokens != 0 {
		t.Fatalf("saved should clamp to 0, got %d", sv.SavedTokens)
	}
}

func TestAggregateSavings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	mk := func(name string, baseline int64) {
		root := filepath.Join(t.TempDir(), name)
		ws, err := workspace.Create(root)
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(ws.DB)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.BumpStats(5, 1000, baseline); err != nil {
			t.Fatal(err)
		}
		s.Close()
		if err := workspace.Register(root, false); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", 40000)
	mk("b", 20000)
	per, combined := aggregateSavings()
	if len(per) != 2 {
		t.Fatalf("perProject = %d, want 2", len(per))
	}
	if per[0].Saved < per[1].Saved {
		t.Fatal("per-project not sorted largest first")
	}
	if combined.Queries != 10 {
		t.Fatalf("combined queries = %d, want 10", combined.Queries)
	}
}
