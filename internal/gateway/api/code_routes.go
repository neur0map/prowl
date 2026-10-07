package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/doctor"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/workspace"
)

const codeRefreshTTL = 20 * time.Second

type codeRepo struct {
	mu      sync.Mutex
	root    string
	project *application.Project
	last    time.Time
}

func (s *Server) registerCodeRoutes() {
	s.mux.HandleFunc("GET /api/code/repos", s.RequireKey(s.handleCodeRepos))
	s.mux.HandleFunc("GET /api/code/{operation}", s.RequireKey(s.handleCodeQuery))
	s.mux.HandleFunc("/api/code/", s.RequireKey(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, TypeInvalidRequest, "code intelligence routes are read-only")
			return
		}
		s.handleAPINotFound(w, r)
	}))
}

func (s *Server) handleCodeRepos(w http.ResponseWriter, _ *http.Request) {
	entries, err := workspace.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the project registry")
		return
	}
	type repoRow struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	rows := make([]repoRow, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, repoRow{Name: filepath.Base(entry.Root), Path: entry.Root})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"repos": rows})
}

func (s *Server) handleCodeQuery(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.resolveCodeRepo(w, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	querier, err := repo.fresh(r.Context())
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, TypeServiceUnavailable, "index refresh: "+err.Error())
		return
	}

	var output any
	switch r.PathValue("operation") {
	case "status":
		output, err = querier.Status()
	case "overview":
		output, err = querier.Overview()
	case "find":
		if value, valid := codeArg(w, r, "q"); valid {
			output, err = querier.FindSymbol(value)
		} else {
			return
		}
	case "def":
		if value, valid := codeArg(w, r, "q"); valid {
			output, err = querier.Definition(repo.root, value)
		} else {
			return
		}
	case "outline":
		if value, valid := codeArg(w, r, "path"); valid {
			output, err = querier.Outline(value)
		} else {
			return
		}
	case "references":
		if value, valid := codeArg(w, r, "q"); valid {
			output, err = querier.References(value)
		} else {
			return
		}
	case "impact":
		if value, valid := codeArg(w, r, "path"); valid {
			output, err = querier.BlastSummarize(value)
		} else {
			return
		}
	case "search":
		value, valid := codeArg(w, r, "q")
		if !valid {
			return
		}
		if r.URL.Query().Get("smart") == "1" {
			output, err = querier.SmartSearch(r.Context(), value)
		} else {
			output, err = querier.SimilarCode(r.Context(), value)
		}
	case "peek":
		path, valid := codeArg(w, r, "path")
		if !valid {
			return
		}
		output, err = codePeek(r, repo.root, path)
		if errCode, ok := err.(*codeRequestError); ok {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, errCode.Error())
			return
		}
	case "history":
		value, valid := codeArg(w, r, "q")
		if !valid {
			return
		}
		limit := 10
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if parsed, parseErr := strconv.Atoi(raw); parseErr == nil && parsed > 0 {
				limit = min(parsed, 50)
			}
		}
		output, err = querier.History(r.Context(), repo.root, value, limit)
	case "doctor":
		output, err = repo.doctor(r.Context())
	default:
		s.handleAPINotFound(w, r)
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, output)
}

func (s *Server) resolveCodeRepo(w http.ResponseWriter, requested string) (*codeRepo, bool) {
	entries, err := workspace.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the project registry")
		return nil, false
	}
	if requested == "" {
		if len(entries) != 1 {
			WriteErrorCode(w, http.StatusBadRequest, TypeInvalidRequest, "repo_required", "repo is required unless exactly one project is registered")
			return nil, false
		}
		return s.codeRepo(entries[0].Root), true
	}

	if filepath.IsAbs(requested) {
		canonical, err := canonicalPath(requested)
		if err == nil {
			for _, entry := range entries {
				entryCanonical, entryErr := canonicalPath(entry.Root)
				if entryErr == nil && entryCanonical == canonical {
					return s.codeRepo(entry.Root), true
				}
			}
		}
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "repo_not_found", "the requested project is not registered")
		return nil, false
	}

	var match string
	for _, entry := range entries {
		if filepath.Base(entry.Root) != requested {
			continue
		}
		if match != "" {
			WriteErrorCode(w, http.StatusBadRequest, TypeInvalidRequest, "repo_required", "repo basename is ambiguous; use an absolute path")
			return nil, false
		}
		match = entry.Root
	}
	if match == "" {
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "repo_not_found", "the requested project is not registered")
		return nil, false
	}
	return s.codeRepo(match), true
}

func (s *Server) codeRepo(root string) *codeRepo {
	canonical, err := canonicalPath(root)
	if err != nil {
		canonical = filepath.Clean(root)
	}
	s.codeMu.Lock()
	defer s.codeMu.Unlock()
	if s.codeRepos == nil {
		s.codeRepos = make(map[string]*codeRepo)
	}
	if repo := s.codeRepos[canonical]; repo != nil {
		return repo
	}
	repo := &codeRepo{root: root}
	s.codeRepos[canonical] = repo
	return repo
}

func (repo *codeRepo) fresh(ctx context.Context) (*query.Querier, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.project == nil {
		project, err := application.OpenProject(ctx, repo.root, application.Options{
			EnableAI: true, InferencerProvider: application.DefaultInferencer,
		})
		if err != nil {
			return nil, err
		}
		repo.project = project
	}
	if repo.last.IsZero() || time.Since(repo.last) >= codeRefreshTTL {
		if _, err := repo.project.Refresh(ctx); err != nil {
			return nil, err
		}
		repo.last = time.Now()
	}
	return repo.project.Query, nil
}

func (repo *codeRepo) doctor(ctx context.Context) (doctor.Report, error) {
	repo.mu.Lock()
	project := repo.project
	repo.mu.Unlock()
	if project == nil {
		return doctor.Report{}, errors.New("project is not open")
	}
	release, err := project.ReadGuard(ctx)
	if err != nil {
		return doctor.Report{}, err
	}
	defer release()
	if err := project.Store.RequirePublishedGeneration(); err != nil {
		return doctor.Report{}, err
	}
	rules, err := config.LoadRules(project.Workspace.Path)
	if err != nil {
		return doctor.Report{}, err
	}
	return doctor.Run(project.Store, rules, doctor.Options{Root: repo.root})
}

func codeArg(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, key+" is required")
		return "", false
	}
	return value, true
}

type codeRequestError struct{ message string }

func (e *codeRequestError) Error() string { return e.message }

func codePeek(r *http.Request, root, path string) (any, error) {
	start, end := 1, 0
	if raw := r.URL.Query().Get("start"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return nil, &codeRequestError{message: "start must be a positive line number"}
		}
		start = parsed
	}
	if raw := r.URL.Query().Get("end"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < start {
			return nil, &codeRequestError{message: "end must be a line number >= start"}
		}
		end = parsed
	}
	return query.PeekLines(root, path, start, end)
}

func canonicalPath(path string) (string, error) {
	return filepath.EvalSymlinks(filepath.Clean(path))
}
