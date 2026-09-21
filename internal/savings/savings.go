package savings

import (
	"path/filepath"
	"sort"

	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

// Project is one registered project's contribution to the combined estimate.
type Project struct {
	Name  string
	Saved int64
}

// Aggregate reads usage from every registered project. Projects that have not
// answered a query yet, or whose index cannot be opened, do not contribute.
func Aggregate() (perProject []Project, combined query.Savings) {
	entries, err := workspace.List()
	if err != nil {
		return nil, combined
	}
	for _, entry := range entries {
		ws, err := workspace.Resolve(entry.Root)
		if err != nil {
			continue
		}
		db, err := store.Open(ws.DB)
		if err != nil {
			continue
		}
		stats, err := db.Stats()
		_ = db.Close()
		if err != nil {
			continue
		}
		estimate := query.ComputeSavings(stats)
		if estimate.Queries == 0 {
			continue
		}
		perProject = append(perProject, Project{
			Name:  filepath.Base(entry.Root),
			Saved: estimate.SavedTokens,
		})
		combined.Queries += estimate.Queries
		combined.SavedTokens += estimate.SavedTokens
		combined.AnswerTokens += estimate.AnswerTokens
	}
	sort.Slice(perProject, func(i, j int) bool {
		return perProject[i].Saved > perProject[j].Saved
	})
	return perProject, combined
}
