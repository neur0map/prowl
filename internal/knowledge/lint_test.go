package knowledge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
)

func TestLintSurfacesHealthFindingsWithoutRejectingBundle(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, ".prowl", "knowledge")
	repo := knowledge.NewRepository(bundle, okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	source := []byte("one\nstable region\nthree\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, _ := knowledge.HashRegion(source, 2, 2)
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(bundle, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "---\ntype: Decision\ntitle: A\nresource: relative/path\nprowl:\n  id: duplicate\n  status: accepted\n  valid_from: 2026-08-01\n  valid_to: 2026-07-01\n  anchors:\n    - path: source.txt\n      line_start: 2\n      line_end: 2\n      content_hash: "+hash+"\n---\n[Missing](missing.md)\n")
	write("b.md", "---\ntype: FutureType\ntitle: B\nprowl:\n  id: duplicate\n  status: rejected\n---\nStandalone.\n")
	write("malformed.md", "---\ntitle: [broken\n---\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("one\nchanged region\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := repo.Lint(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	for _, code := range []string{
		"knowledge.broken_link", "knowledge.invalid_resource", "knowledge.invalid_temporal_range",
		"knowledge.stale_anchor", "knowledge.duplicate_id", "knowledge.contradiction",
		"knowledge.invalid_document", "knowledge.orphan",
	} {
		if !codes[code] {
			t.Errorf("missing finding %s in %+v", code, findings)
		}
	}
}

func TestLintAcceptsUnknownTypesAndCurrentEvidence(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, ".prowl", "knowledge")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte("evidence\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, _ := knowledge.HashRegion(source, 1, 1)
	doc := "---\ntype: NewTypeFromFutureSpec\ntitle: Future\nprowl:\n  id: future\n  anchors:\n    - path: source.txt\n      line_start: 1\n      line_end: 1\n      content_hash: " + hash + "\n---\n"
	if err := os.WriteFile(filepath.Join(bundle, "future.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := knowledge.NewRepository(bundle, okfv01.Codec{})
	findings, err := repo.Lint(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Severity == "error" || finding.Code == "knowledge.stale_anchor" {
			t.Fatalf("healthy future document failed lint: %+v", findings)
		}
	}
}

// A shifted anchor is reported as moved, not stale, and --repair rewrites its
// range in place so the note keeps verifiable evidence instead of decaying into
// a warning nobody acts on.
func TestRepairAnchorsRewritesMovedRanges(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, ".prowl", "knowledge")
	repo := knowledge.NewRepository(bundle, okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	source := []byte("package p\n\nfunc Keep() {\n\treturn\n}\n")
	sourcePath := filepath.Join(root, "source.go")
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := knowledge.HashRegion(source, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	doc := "---\ntype: Decision\ntitle: Keep\nprowl:\n  id: keep\n  status: accepted\n  custom_field: preserved\n  anchors:\n    - path: source.go\n      line_start: 3\n      line_end: 5\n      content_hash: " + hash + "\n---\nKeep does nothing on purpose.\n"
	if err := os.WriteFile(filepath.Join(bundle, "keep.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	// Insert two lines above the anchored region; its content is untouched.
	shifted := []byte("package p\n\nvar added = 1\n\nfunc Keep() {\n\treturn\n}\n")
	if err := os.WriteFile(sourcePath, shifted, 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := repo.Lint(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Code == "knowledge.stale_anchor" {
			t.Fatalf("shifted anchor reported stale: %+v", findings)
		}
	}
	if !hasFinding(findings, "knowledge.moved_anchor") {
		t.Fatalf("no moved_anchor finding in %+v", findings)
	}

	repairs, err := repo.RepairAnchors(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 {
		t.Fatalf("repairs = %+v, want one", repairs)
	}
	if repairs[0].ToStart != 5 || repairs[0].ToEnd != 7 || repairs[0].Source != "source.go" {
		t.Errorf("repair = %+v, want source.go 5-7", repairs[0])
	}

	// The rewritten document must be clean on a second pass and keep the
	// frontmatter Prowl does not own.
	after, err := os.ReadFile(filepath.Join(bundle, "keep.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "custom_field: preserved") {
		t.Errorf("repair dropped unknown frontmatter:\n%s", after)
	}
	findings, err = repo.Lint(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Code == "knowledge.moved_anchor" || finding.Code == "knowledge.stale_anchor" {
			t.Errorf("anchor still unhealthy after repair: %+v", finding)
		}
	}
	if repeat, err := repo.RepairAnchors(root, nil); err != nil || len(repeat) != 0 {
		t.Errorf("second repair = %+v (err %v), want no work", repeat, err)
	}
}

func hasFinding(findings []knowledge.Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
