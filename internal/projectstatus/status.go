// Package projectstatus reads the indexed-project state shown by gateway clients.
package projectstatus

import (
	"context"
	"os"

	"github.com/neur0map/prowl/internal/embed"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

// DefaultEmbedModel is the built-in embedder used when an index has no model metadata.
const DefaultEmbedModel = embed.ModelName

// Row is one registered project's current index state.
type Row struct {
	Root       string
	Status     query.Status
	State      string
	Err        error
	EmbedModel string
}

// Load returns the same project-state calculation for every registered root.
func Load() ([]Row, error) {
	entries, err := workspace.List()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, Inspect(entry.Root))
	}
	return rows, nil
}

// Inspect reads one root without changing its index.
func Inspect(root string) Row {
	row := Row{Root: root, State: "not indexed", EmbedModel: DefaultEmbedModel}
	ws, err := workspace.Resolve(root)
	if err != nil {
		row.Err = err
		return row
	}
	if _, err := os.Stat(ws.DB); err != nil {
		row.Err = err
		return row
	}
	database, err := store.Open(ws.DB)
	if err != nil {
		row.State = "error"
		row.Err = err
		return row
	}
	defer database.Close()

	row.Status, row.Err = query.New(database).Status()
	if row.Err != nil {
		row.State = "error"
		return row
	}
	model, err := database.GetMetaContext(context.Background(), "embed_model")
	if err != nil {
		row.State = "error"
		row.Err = err
		return row
	}
	if model != "" {
		row.EmbedModel = model
	}
	row.State = "ready"
	if row.Status.Semantic.Remaining > 0 {
		row.State = "semantic building"
	}
	return row
}
