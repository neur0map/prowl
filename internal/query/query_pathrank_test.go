package query

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// TestSearchFloatsPathNamedConcept proves the fix for the real report where an
// agent's `search` returned tangential files and the file that implements the
// searched concept never surfaced. A file whose PATH names the concept must lead
// even when (a) its text lacks the exact query token and (b) other files match
// the token only in their text.
func TestSearchFloatsPathNamedConcept(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(rel, text string) {
		fid, err := s.UpsertFile(store.File{RelPath: rel, Lang: "go", Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceFileGraph(fid, nil, nil, nil, []store.Chunk{{StartLine: 1, EndLine: 1, Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	// Path names the concept; its text never contains the token "download".
	mk("scripts/stash-download.sh", "curl the media then save it to the stash folder")
	// Tangential files that only mention the token in prose.
	mk("shell/Motion.qml", "animate the download progress bar smoothly")
	mk("bar/BrightnessControl.qml", "adjust brightness; you can download presets")

	// "downloader" never FTS-matches any of these (all say "download"), so pure
	// FTS-over-text returns the path-named file at no rank at all.
	hits, err := New(s).SimilarCode(context.Background(), "downloader")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].File != "scripts/stash-download.sh" {
		t.Fatalf("path-named file must lead for 'downloader'; got %v", filesOf(hits))
	}

	// Even for the exact token, the path-named file must outrank text-only hits.
	hits, err = New(s).SimilarCode(context.Background(), "download")
	if err != nil {
		t.Fatal(err)
	}
	files := filesOf(hits)
	if len(files) == 0 || files[0] != "scripts/stash-download.sh" {
		t.Fatalf("path-named file must lead for 'download'; got %v", files)
	}
	if pos(files, "scripts/stash-download.sh") > pos(files, "shell/Motion.qml") {
		t.Fatalf("path-named file must outrank text-only match; got %v", files)
	}
}

func filesOf(hits []store.ChunkHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.File
	}
	return out
}

func pos(files []string, want string) int {
	for i, f := range files {
		if f == want {
			return i
		}
	}
	return len(files)
}
