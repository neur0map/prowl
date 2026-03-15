package mcp

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/store"
)

type overviewResponse struct {
	Files       int                 `json:"files"`
	Languages   map[string]int      `json:"languages"`
	Symbols     int                 `json:"symbols"`
	Edges       int                 `json:"edges"`
	Embeddings  int                 `json:"embeddings"`
	Communities []communityOverview `json:"communities"`
	Processes   []processOverview   `json:"processes"`
}

type communityOverview struct {
	Name    string   `json:"name"`
	ID      int      `json:"id"`
	Members []string `json:"members"`
}

type processOverview struct {
	Name  string   `json:"name"`
	Entry string   `json:"entry"`
	Steps []string `json:"steps"`
}

func (s *Server) handleOverview(w io.Writer, id interface{}, params json.RawMessage) {
	st, ctxDir, err := s.storeFor(params)
	if err != nil {
		s.writeError(w, id, -32602, err.Error())
		return
	}

	files, symbols, edges, _ := st.Stats()
	embeddings, _ := st.EmbeddingCount()

	// Language breakdown from file extensions
	allFiles, _ := st.AllFiles()
	langCounts := map[string]int{}
	for _, f := range allFiles {
		ext := strings.TrimPrefix(filepath.Ext(f), ".")
		lang := extToLanguage(ext)
		if lang != "" {
			langCounts[lang]++
		}
	}

	// Communities from store
	storeComms, _ := st.AllCommunities()
	var comms []communityOverview
	for _, c := range storeComms {
		comms = append(comms, communityOverview{
			Name:    c.Name,
			ID:      c.ID,
			Members: communityMembersByName(st, c.Name, allFiles),
		})
	}

	// Processes from _meta/processes.txt
	procs := readProcesses(ctxDir)

	resp := overviewResponse{
		Files:       files,
		Languages:   langCounts,
		Symbols:     symbols,
		Edges:       edges,
		Embeddings:  embeddings,
		Communities: comms,
		Processes:   procs,
	}

	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}

func (s *Server) handleFileContext(w io.Writer, id interface{}, params json.RawMessage) {
	_, ctxDir, err := s.storeFor(params)
	if err != nil {
		s.writeError(w, id, -32602, err.Error())
		return
	}

	var args struct {
		Path string `json:"path"`
	}
	json.Unmarshal(params, &args)
	if args.Path == "" {
		s.writeError(w, id, -32602, "Missing required parameter: path")
		return
	}

	fc, err := readFileContext(ctxDir, args.Path)
	if err != nil {
		s.writeError(w, id, -32602, "File not indexed: "+args.Path)
		return
	}

	s.recordAccess(args.Path)
	data, _ := json.Marshal(fc)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}

func extToLanguage(ext string) string {
	switch ext {
	case "ts", "tsx":
		return "typescript"
	case "js", "jsx":
		return "javascript"
	case "go":
		return "go"
	case "rs":
		return "rust"
	case "py":
		return "python"
	case "java":
		return "java"
	case "cs":
		return "csharp"
	case "swift":
		return "swift"
	case "cpp", "cc", "cxx":
		return "cpp"
	case "c", "h":
		return "c"
	default:
		return ""
	}
}

func communityMembersByName(st *store.Store, commName string, allFiles []string) []string {
	var members []string
	for _, f := range allFiles {
		name, err := st.CommunityOf(f)
		if err == nil && name == commName {
			members = append(members, st.FileDigest(f))
		}
	}
	return members
}

func readProcesses(contextDir string) []processOverview {
	lines := readLines(filepath.Join(contextDir, "_meta", "processes.txt"))
	var procs []processOverview
	var current *processOverview
	inSteps := false

	for _, line := range lines {
		if strings.HasPrefix(line, "process:") {
			if current != nil {
				procs = append(procs, *current)
			}
			name := strings.TrimSpace(strings.TrimPrefix(line, "process:"))
			if idx := strings.Index(name, " ["); idx >= 0 {
				name = name[:idx]
			}
			current = &processOverview{Name: name}
			inSteps = false
		} else if strings.HasPrefix(line, "entry:") && current != nil {
			current.Entry = strings.TrimSpace(strings.TrimPrefix(line, "entry:"))
		} else if line == "steps:" {
			inSteps = true
		} else if strings.HasPrefix(line, "->") && inSteps && current != nil {
			step := strings.TrimSpace(strings.TrimPrefix(line, "->"))
			current.Steps = append(current.Steps, step)
		}
	}
	if current != nil {
		procs = append(procs, *current)
	}

	// Filter: only processes with 3+ steps, cap at 10
	var filtered []processOverview
	for _, p := range procs {
		if len(p.Steps) >= 3 && len(filtered) < 10 {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 && len(procs) > 0 {
		if len(procs) > 10 {
			procs = procs[:10]
		}
		return procs
	}
	return filtered
}
