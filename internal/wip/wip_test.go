package wip

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/query"
)

type fakeBlaster struct{ summary query.BlastSummary }

func (f fakeBlaster) BlastSummarize(string) (query.BlastSummary, error) { return f.summary, nil }

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInvestigateReportsStatusesMarkersAndImpact(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "wip@example.test")
	git(t, root, "config", "user.name", "WIP Test")
	write(t, root, "committed.go", "package sample\n\nfunc Committed() {}\n")
	git(t, root, "add", "committed.go")
	git(t, root, "commit", "-m", "base")

	// A staged addition, a modification to a committed file, and an untracked
	// file that carries an unfinished-work marker.
	write(t, root, "staged.go", "package sample\n")
	git(t, root, "add", "staged.go")
	write(t, root, "committed.go", "package sample\n\nfunc Committed() {}\n// FIXME: revisit\n")
	write(t, root, "untracked.go", "package sample\n\n// TODO: implement later\nfunc Pending() {}\n")

	indexed := map[string]bool{"committed.go": true}
	report, err := Investigate(context.Background(), root, indexed, fakeBlaster{summary: query.BlastSummary{Total: 7, Direct: 2}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Clean {
		t.Fatal("report marked clean with uncommitted work present")
	}

	byPath := map[string]FileReport{}
	for _, f := range report.Files {
		byPath[f.Path] = f
	}

	staged, ok := byPath["staged.go"]
	if !ok || staged.Status != "staged" {
		t.Fatalf("staged.go = %+v, want status staged", staged)
	}
	untracked, ok := byPath["untracked.go"]
	if !ok || untracked.Status != "untracked" {
		t.Fatalf("untracked.go = %+v, want status untracked", untracked)
	}
	if len(untracked.Markers) != 1 || untracked.Markers[0].Kind != "TODO" || untracked.Markers[0].Line != 3 {
		t.Fatalf("untracked markers = %+v, want one TODO on line 3", untracked.Markers)
	}
	committed, ok := byPath["committed.go"]
	if !ok || committed.Status != "modified" {
		t.Fatalf("committed.go = %+v, want status modified", committed)
	}
	if len(committed.Markers) != 1 || committed.Markers[0].Kind != "FIXME" {
		t.Fatalf("committed markers = %+v, want one FIXME", committed.Markers)
	}
	if committed.Impact == nil || committed.Impact.Total != 7 {
		t.Fatalf("committed impact = %+v, want blast total 7 for the indexed file", committed.Impact)
	}
	if staged.Impact != nil {
		t.Fatalf("staged.go is not indexed, want no impact, got %+v", staged.Impact)
	}
	if report.Counts.Markers != 2 {
		t.Fatalf("marker count = %d, want 2", report.Counts.Markers)
	}
}

func TestInvestigateCleanTreeIsEmpty(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "wip@example.test")
	git(t, root, "config", "user.name", "WIP Test")
	write(t, root, "committed.go", "package sample\n")
	git(t, root, "add", "committed.go")
	git(t, root, "commit", "-m", "base")

	report, err := Investigate(context.Background(), root, nil, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Clean || len(report.Files) != 0 {
		t.Fatalf("clean tree report = %+v, want clean with no files", report)
	}
}

func TestInvestigateCustomMarkersOnly(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "wip@example.test")
	git(t, root, "config", "user.name", "WIP Test")
	write(t, root, "note.go", "package sample\n// TODO: default marker\n// REVIEW: custom marker\n")

	report, err := Investigate(context.Background(), root, nil, nil, Options{Markers: []string{"REVIEW"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 1 {
		t.Fatalf("files = %+v, want one", report.Files)
	}
	markers := report.Files[0].Markers
	if len(markers) != 1 || markers[0].Kind != "REVIEW" {
		t.Fatalf("markers = %+v, want only the custom REVIEW marker", markers)
	}
}
