package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/neur0map/prowl/internal/mirror"
	"github.com/neur0map/prowl/internal/pipeline"
	"github.com/neur0map/prowl/internal/store"
)

func (s *Server) handleClone(w io.Writer, id interface{}, params json.RawMessage) {
	var args struct {
		Repo  string `json:"repo"`
		Ref   string `json:"ref"`
		Token string `json:"token"`
	}
	json.Unmarshal(params, &args)
	if args.Repo == "" {
		s.writeError(w, id, -32602, "Missing required parameter: repo")
		return
	}

	owner, repo, ref, err := mirror.ParseRepo(args.Repo)
	if err != nil {
		s.writeError(w, id, -32602, "Invalid repo: "+err.Error())
		return
	}
	if args.Ref != "" {
		ref = args.Ref
	}

	// Download tarball
	mirrorPath, changed, err := mirror.Download(owner, repo, ref, args.Token)
	if err != nil {
		s.writeError(w, id, -32603, "Download failed: "+err.Error())
		return
	}

	label := owner + "/" + repo

	if changed {
		err = pipeline.Index(mirrorPath, pipeline.WithProgressWriter(os.Stderr))
		if err != nil {
			s.writeError(w, id, -32603, "Indexing failed: "+err.Error())
			return
		}
	}

	// Open the comparison store
	dbPath := fmt.Sprintf("%s/.prowl/prowl.db", mirrorPath)
	ctxDir := fmt.Sprintf("%s/.prowl/context", mirrorPath)

	// Close any existing comparison store
	if s.compareStore != nil {
		s.compareStore.Close()
	}

	st, err := store.Open(dbPath)
	if err != nil {
		s.writeError(w, id, -32603, "Open comparison store: "+err.Error())
		return
	}

	s.compareStore = st
	s.compareContextDir = ctxDir
	s.compareRepo = label

	// Build response with stats
	files, symbols, edges, _ := st.Stats()
	embeddings, _ := st.EmbeddingCount()

	status := "downloaded and indexed"
	if !changed {
		status = "cached (unchanged)"
	}

	resp := map[string]interface{}{
		"repo":       label,
		"status":     status,
		"mirror_dir": mirrorPath,
		"files":      files,
		"symbols":    symbols,
		"edges":      edges,
		"embeddings": embeddings,
		"hint":       "Use project:\"comparison\" with any prowl tool to query this repo",
	}

	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}

func (s *Server) handleCloneStatus(w io.Writer, id interface{}) {
	if s.compareStore == nil {
		s.writeResult(w, id, map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": `{"status":"no comparison repo loaded"}`},
			},
		})
		return
	}

	files, symbols, edges, _ := s.compareStore.Stats()
	embeddings, _ := s.compareStore.EmbeddingCount()

	resp := map[string]interface{}{
		"repo":       s.compareRepo,
		"files":      files,
		"symbols":    symbols,
		"edges":      edges,
		"embeddings": embeddings,
	}

	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}

func (s *Server) handleCloneClose(w io.Writer, id interface{}) {
	if s.compareStore == nil {
		s.writeResult(w, id, map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": `{"status":"no comparison repo to close"}`},
			},
		})
		return
	}

	label := s.compareRepo
	s.compareStore.Close()
	s.compareStore = nil
	s.compareRepo = ""
	s.compareContextDir = ""

	resp := map[string]interface{}{
		"status": "closed",
		"repo":   label,
	}
	data, _ := json.Marshal(resp)
	s.writeResult(w, id, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": string(data)},
		},
	})
}
