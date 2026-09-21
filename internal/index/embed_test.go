package index

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

// fakeEmbedder is a deterministic stand-in for an embedding model: it maps text
// to a fixed-dim vector by byte folding. It exercises the real storage and
// retrieval paths without needing a live Ollama.
type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		for j, b := range []byte(t) {
			v[j%f.dim] += float32(b)
		}
		out[i] = v
	}
	return out, nil
}

func (f fakeEmbedder) Generate(_ context.Context, _ string) (string, error) { return "", nil }

func (f fakeEmbedder) Rerank(_ context.Context, _ string, docs []string) ([]int, error) {
	order := make([]int, len(docs))
	for i := range order {
		order[i] = i
	}
	return order, nil
}

func TestBuildVectors(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := filepath.Join("..", "..", "testdata", "sample-config")
	if _, err := Index(s, root, nil); err != nil {
		t.Fatal(err)
	}

	pending, _ := s.ChunksWithoutVectors(0)
	if len(pending) == 0 {
		t.Fatal("no chunks to embed")
	}

	pass, err := BuildVectors(context.Background(), s, fakeEmbedder{dim: 32}, "fake", VectorBudget{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Embedded != len(pending) {
		t.Fatalf("embedded %d, want %d", pass.Embedded, len(pending))
	}
	if pass.Remaining != 0 {
		t.Fatalf("remaining %d after full drain, want 0", pass.Remaining)
	}
	if !s.VectorsReady() || !s.VectorsComplete() {
		t.Fatalf("ready=%v complete=%v after full build", s.VectorsReady(), s.VectorsComplete())
	}
	if left, _ := s.ChunksWithoutVectors(0); len(left) != 0 {
		t.Fatalf("%d chunks still without vectors", len(left))
	}

	// Incremental: a second run embeds nothing.
	if again, err := BuildVectors(context.Background(), s, fakeEmbedder{dim: 32}, "fake", VectorBudget{}, nil); err != nil || again.Embedded != 0 {
		t.Fatalf("re-run embedded %d err=%v, want 0", again.Embedded, err)
	}

	// A vector search returns results.
	hits, err := s.VectorSearch(make([]float32, 32), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("vector search returned nothing")
	}
}

// Embedding a large repo is tens of minutes of model round trips, so no single
// pass can be assumed to finish. Each budgeted pass must keep its work and the
// next one must resume from the backlog instead of starting over.
func TestBuildVectorsResumesAcrossBudgetedPasses(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Index(s, filepath.Join("..", "..", "testdata", "sample-config"), nil); err != nil {
		t.Fatal(err)
	}
	total, err := s.CountChunksWithoutVectors()
	if err != nil {
		t.Fatal(err)
	}
	if total < 3 {
		t.Fatalf("fixture has %d chunks, need at least 3 to test partial passes", total)
	}

	emb := fakeEmbedder{dim: 32}
	first, err := BuildVectors(context.Background(), s, emb, "fake", VectorBudget{MaxChunks: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Embedded == 0 || first.Embedded >= total {
		t.Fatalf("first pass embedded %d of %d, want a partial pass", first.Embedded, total)
	}
	if first.Remaining != total-first.Embedded {
		t.Fatalf("remaining %d, want %d", first.Remaining, total-first.Embedded)
	}
	if s.VectorsComplete() {
		t.Fatal("a partial backlog must not report a complete vector index")
	}
	// Partial coverage is still usable: semantic search must not be switched off
	// repo-wide just because the backlog has not drained.
	if !s.VectorsReady() {
		t.Fatal("partially embedded index reported as unusable")
	}

	embedded := first.Embedded
	for range 100 {
		pass, err := BuildVectors(context.Background(), s, emb, "fake", VectorBudget{MaxChunks: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		embedded += pass.Embedded
		if pass.Remaining == 0 {
			break
		}
	}
	if embedded != total {
		t.Fatalf("resumed passes embedded %d in total, want %d (work was discarded between passes)", embedded, total)
	}
	if !s.VectorsComplete() {
		t.Fatal("drained backlog did not mark the vector index complete")
	}
}
