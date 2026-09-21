package context

import (
	"context"
	"fmt"
	"time"

	"github.com/neur0map/prowl/internal/assist"
)

// AssistSemanticReranker adapts the existing optional local inferencer to context
// packet ranking. It is wired only when the workspace AI layer is enabled and
// reachable; any provider failure preserves deterministic ranking.
type AssistSemanticReranker struct {
	Inferencer assist.Inferencer
	Timeout    time.Duration
}

func (reranker AssistSemanticReranker) Scores(question string, candidates []Candidate) (map[string]float64, error) {
	if reranker.Inferencer == nil {
		return nil, fmt.Errorf("semantic inferencer unavailable")
	}
	timeout := reranker.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	documents := make([]string, len(candidates))
	for index, candidate := range candidates {
		documents[index] = candidate.Title + "\n" + candidate.Summary + "\n" + candidate.CompactContent
	}
	order, err := reranker.Inferencer.Rerank(ctx, question, documents)
	if err != nil {
		return nil, err
	}
	if len(order) != len(candidates) {
		return nil, fmt.Errorf("semantic reranker returned %d indices for %d candidates", len(order), len(candidates))
	}
	scores := make(map[string]float64, len(order))
	seen := make(map[int]bool, len(order))
	denominator := float64(len(order))
	if denominator == 0 {
		return scores, nil
	}
	for rank, index := range order {
		if index < 0 || index >= len(candidates) || seen[index] {
			return nil, fmt.Errorf("semantic reranker returned invalid candidate index %d", index)
		}
		seen[index] = true
		scores[candidates[index].ID] = float64(len(order)-rank) / denominator
	}
	return scores, nil
}
