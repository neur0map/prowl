package index

import (
	"context"
	"fmt"
	"time"

	"github.com/neur0map/prowl/internal/assist"
	"github.com/neur0map/prowl/internal/store"
)

const (
	embedBatch = 32
	// embedWindow is how many pending chunks are read from SQLite at a time.
	// A large repo has ~100k pending chunks holding ~100MB of source text, so
	// the backlog is walked in windows rather than materialized in full.
	embedWindow = 512
)

// VectorBudget bounds one embedding pass. A zero budget drains the whole
// backlog. Bounding matters because embedding a large repo is thousands of
// sequential model round trips: a query that implicitly warms the index must
// make durable progress and then answer, not block for an hour.
type VectorBudget struct {
	MaxChunks   int
	MaxDuration time.Duration
}

// VectorPass reports one embedding pass: how many chunks it embedded and how
// many still lack a vector afterwards. Remaining is 0 exactly when semantic
// search is fully built.
type VectorPass struct {
	Embedded  int
	Remaining int
}

// BuildVectors embeds chunks that lack a vector, newest backlog first by id,
// until the budget is spent or the backlog is drained.
//
// It must be called on a published store, not inside an index generation: each
// batch is committed as it is embedded, so an interrupted pass keeps its work
// and the next call resumes from where it stopped. On a large repo the first
// pass cannot finish in one invocation, and discarding partial progress would
// mean the semantic index could never be built at all.
//
// A progress callback, when non-nil, is invoked after each committed batch so a
// long first build is visible instead of looking like a hang.
func BuildVectors(ctx context.Context, s *store.Store, inf assist.Embedder, model string, budget VectorBudget, progress func(VectorPass)) (VectorPass, error) {
	var pass VectorPass
	outstanding, err := s.CountChunksWithoutVectors()
	if err != nil {
		return pass, err
	}
	pass.Remaining = outstanding
	if err := s.SetMeta("vectors_complete", "0"); err != nil {
		return pass, err
	}
	if currentModel, _ := s.GetMeta("embed_model"); s.VectorsInitialized() && currentModel != model {
		if err := s.ResetVectors(); err != nil {
			return pass, err
		}
	}
	// Record the model up front: vectors written by this pass are keyed to it,
	// and a pass that stops early must not leave them attributed to the old one.
	if err := s.SetMeta("embed_model", model); err != nil {
		return pass, err
	}
	// Drop vectors an older version stored for content-free chunks: they embed to
	// the model's degenerate mean vector and dominate KNN for every query.
	if _, err := s.PruneContentFreeVectors(); err != nil {
		return pass, err
	}
	deadline := time.Time{}
	if budget.MaxDuration > 0 {
		deadline = time.Now().Add(budget.MaxDuration)
	}
	spent := func() bool {
		if budget.MaxChunks > 0 && pass.Embedded >= budget.MaxChunks {
			return true
		}
		return !deadline.IsZero() && !time.Now().Before(deadline)
	}
	for {
		if err := ctx.Err(); err != nil {
			return pass, err
		}
		if spent() {
			break
		}
		pending, err := s.ChunksWithoutVectors(embedWindow)
		if err != nil {
			return pass, err
		}
		if len(pending) == 0 {
			break
		}
		for start := 0; start < len(pending); start += embedBatch {
			if err := ctx.Err(); err != nil {
				return pass, err
			}
			if spent() {
				break
			}
			end := min(start+embedBatch, len(pending))
			if budget.MaxChunks > 0 {
				end = min(end, start+budget.MaxChunks-pass.Embedded)
			}
			batch := pending[start:end]
			texts := make([]string, len(batch))
			for i, c := range batch {
				texts[i] = c.Text
			}
			vecs, err := inf.Embed(ctx, texts)
			if err != nil {
				return pass, err
			}
			if len(vecs) != len(batch) {
				return pass, fmt.Errorf("embed returned %d vectors for %d texts", len(vecs), len(batch))
			}
			if !s.VectorsInitialized() {
				if len(vecs[0]) == 0 {
					return pass, fmt.Errorf("embedding model returned empty vector")
				}
				if err := s.EnableVectors(len(vecs[0]), model); err != nil {
					return pass, err
				}
			}
			for i, c := range batch {
				if err := s.UpsertChunkVector(c.ID, vecs[i]); err != nil {
					return pass, err
				}
				pass.Embedded++
			}
			if progress != nil {
				pass.Remaining = max(outstanding-pass.Embedded, 0)
				progress(pass)
			}
		}
	}
	remaining, err := s.CountChunksWithoutVectors()
	if err != nil {
		return pass, err
	}
	pass.Remaining = remaining
	if remaining == 0 {
		if err := s.SetMeta("vectors_complete", "1"); err != nil {
			return pass, err
		}
	}
	return pass, nil
}
