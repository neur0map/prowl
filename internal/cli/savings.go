package cli

import (
	"path/filepath"
	"sort"

	"github.com/prowl-agent/prowl-agent/internal/query"
	"github.com/prowl-agent/prowl-agent/internal/store"
	"github.com/prowl-agent/prowl-agent/internal/workspace"
)

// tokensDocURL points users at the reproducible measurement instructions.
const tokensDocURL = "github.com/neur0map/prowl-agent/blob/main/docs/TOKENS.md"

// projSaving is one project's contribution to the combined savings.
type projSaving struct {
	Name  string
	Saved int64
}

// aggregateSavings reads usage stats from every registered project and returns a
// per-project breakdown (largest first) and the combined totals. Projects that
// have never been queried are skipped.
func aggregateSavings() (perProject []projSaving, combined query.Savings) {
	entries, err := workspace.List()
	if err != nil {
		return nil, combined
	}
	for _, e := range entries {
		ws, err := workspace.Resolve(e.Root)
		if err != nil {
			continue
		}
		s, err := store.Open(ws.DB)
		if err != nil {
			continue
		}
		st, err := s.Stats()
		s.Close()
		if err != nil {
			continue
		}
		sv := query.ComputeSavings(st)
		if sv.Queries == 0 {
			continue
		}
		perProject = append(perProject, projSaving{Name: filepath.Base(e.Root), Saved: sv.SavedTokens})
		combined.Queries += sv.Queries
		combined.SavedTokens += sv.SavedTokens
		combined.AnswerTokens += sv.AnswerTokens
	}
	sort.Slice(perProject, func(i, j int) bool { return perProject[i].Saved > perProject[j].Saved })
	return perProject, combined
}
