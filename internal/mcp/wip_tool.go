package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neur0map/prowl/internal/wip"
)

type wipIn struct {
	Markers []string `json:"markers,omitempty" jsonschema:"unfinished-work markers to scan; defaults to TODO, FIXME, HACK, XXX, BUG, WIP, OPTIMIZE"`
}

// investigateWip reports the uncommitted working set: touched files with their
// git status, the unfinished-work markers inside them, and the blast radius of
// each indexed file. It lets a fresh agent resume where the last one stopped.
func (h *handlers) investigateWip(ctx context.Context, _ *sdk.CallToolRequest, in wipIn) (*sdk.CallToolResult, wip.Report, error) {
	indexed := map[string]bool{}
	if h.store != nil {
		if sizes, err := h.store.FileSizes(); err == nil {
			for path := range sizes {
				indexed[path] = true
			}
		}
	}
	report, err := wip.Investigate(ctx, h.root, indexed, h.q, wip.Options{Markers: in.Markers})
	return nil, report, err
}
