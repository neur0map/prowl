package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/neur0map/prowl/internal/store"
)

type deadcodeResponse struct {
	Orphans []deadcodeFile `json:"orphans"`
	Leaves  []deadcodeFile `json:"leaves"`
	Summary string         `json:"summary"`
}

type deadcodeFile struct {
	Path      string `json:"path"`
	Exports   int    `json:"exports"`
	Community string `json:"community"`
}

func (s *Server) handleDeadcode(w io.Writer, id interface{}, params json.RawMessage) {
	st, _, err := s.storeFor(params)
	if err != nil {
		s.writeError(w, id, -32602, err.Error())
		return
	}

	var args struct {
		IncludeTests bool `json:"include_tests"`
	}
	json.Unmarshal(params, &args)

	files, err := st.AllFiles()
	if err != nil {
		s.writeError(w, id, -32603, "AllFiles: "+err.Error())
		return
	}

	edges, err := st.AllEdges()
	if err != nil {
		s.writeError(w, id, -32603, "AllEdges: "+err.Error())
		return
	}

	// Build connectivity sets from edges in one pass.
	hasOutgoing := make(map[string]bool) // file has outgoing edges (calls/imports something)
	hasIncoming := make(map[string]bool) // file has incoming edges (something calls/imports it)
	for _, e := range edges {
		hasOutgoing[e.SourcePath] = true
		hasIncoming[e.TargetPath] = true
	}

	var orphans, leaves []deadcodeFile
	for _, path := range files {
		if !args.IncludeTests && isTestFile(path) {
			continue
		}

		out := hasOutgoing[path]
		in := hasIncoming[path]

		if !out && !in {
			orphans = append(orphans, buildDeadcodeFile(st, path))
		} else if !in && out {
			leaves = append(leaves, buildDeadcodeFile(st, path))
		}
	}

	summary := fmt.Sprintf("%d orphans (no connections), %d leaves (calls out but nothing depends on them), out of %d total files",
		len(orphans), len(leaves), len(files))

	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": mustJSON(deadcodeResponse{
					Orphans: orphans,
					Leaves:  leaves,
					Summary: summary,
				}),
			},
		},
	})
}

func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go") ||
		strings.HasSuffix(path, ".test.ts") ||
		strings.HasSuffix(path, ".test.tsx") ||
		strings.HasSuffix(path, ".test.js") ||
		strings.HasSuffix(path, ".spec.ts") ||
		strings.HasSuffix(path, ".spec.tsx") ||
		strings.HasSuffix(path, ".spec.js") ||
		strings.Contains(path, "__tests__/") ||
		strings.Contains(path, "__test__/")
}

func buildDeadcodeFile(st *store.Store, path string) deadcodeFile {
	syms, _ := st.SymbolsForFile(path)
	exports := 0
	for _, sym := range syms {
		if sym.IsExported {
			exports++
		}
	}
	community, _ := st.CommunityOf(path)
	return deadcodeFile{
		Path:      path,
		Exports:   exports,
		Community: community,
	}
}

func mustJSON(v interface{}) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}
