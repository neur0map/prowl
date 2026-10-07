package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/projectstatus"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/workspace"
)

type projectResponse struct {
	Root       string        `json:"root"`
	Name       string        `json:"name"`
	State      string        `json:"state"`
	Error      string        `json:"error,omitempty"`
	Status     *query.Status `json:"status,omitempty"`
	EmbedModel string        `json:"embedModel"`
	Job        *projectJob   `json:"job,omitempty"`
}

type projectJob struct {
	Kind      string `json:"kind"`
	StartedAt string `json:"startedAt"`
	Error     string `json:"error,omitempty"`
}

type runningProjectJob struct {
	kind      string
	startedAt time.Time
	error     string
	running   bool
	done      chan struct{}
}

type addProjectRequest struct {
	Root         string   `json:"root"`
	Integrations []string `json:"integrations"`
}

type reindexProjectRequest struct {
	Root string `json:"root"`
}

func (s *Server) registerProjectsRoutes() {
	s.mux.HandleFunc("GET /api/projects", s.RequireKey(s.handleProjectsList))
	s.mux.HandleFunc("POST /api/projects", s.RequireKey(s.handleProjectAdd))
	s.mux.HandleFunc("DELETE /api/projects", s.RequireKey(s.handleProjectDelete))
	s.mux.HandleFunc("POST /api/projects/reindex", s.RequireKey(s.handleProjectReindex))
	s.mux.HandleFunc("/api/projects", s.RequireKey(projectMethodNotAllowed))
	s.mux.HandleFunc("/api/projects/reindex", s.RequireKey(projectMethodNotAllowed))
}

func projectMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	WriteError(w, http.StatusMethodNotAllowed, TypeInvalidRequest, "method not allowed")
}

func (s *Server) handleProjectsList(w http.ResponseWriter, _ *http.Request) {
	projects, err := s.projects()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read projects: "+err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) handleProjectAdd(w http.ResponseWriter, r *http.Request) {
	var request addProjectRequest
	if !DecodeJSON(w, r, &request) {
		return
	}
	root, err := validateProjectRoot(request.Root)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	job, started := s.startProjectJob(root, "index", func(ctx context.Context) error {
		_, err := application.InitializeProject(ctx, application.InitOptions{
			Root:               root,
			Integrations:       append([]string(nil), request.Integrations...),
			IntegrationsSet:    true,
			InferencerProvider: application.DefaultInferencer,
		})
		return err
	})
	if !started {
		WriteErrorCode(w, http.StatusConflict, TypeInvalidRequest, "job_running", "an index job is already running for this project")
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"project": indexingProject(root, job)})
}

func (s *Server) handleProjectReindex(w http.ResponseWriter, r *http.Request) {
	var request reindexProjectRequest
	if !DecodeJSON(w, r, &request) {
		return
	}
	root, err := validateProjectRoot(request.Root)
	if err != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, err.Error())
		return
	}
	registered, err := registeredProject(root)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the project registry")
		return
	}
	if !registered {
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "repo_not_found", "the requested project is not registered")
		return
	}
	job, started := s.startProjectJob(root, "reindex", func(ctx context.Context) error {
		project, err := application.OpenProject(ctx, root, application.Options{
			EnableAI: true, InferencerProvider: application.DefaultInferencer,
		})
		if err != nil {
			return err
		}
		defer project.Close()
		_, err = project.Refresh(ctx)
		return err
	})
	if !started {
		WriteErrorCode(w, http.StatusConflict, TypeInvalidRequest, "job_running", "an index job is already running for this project")
		return
	}
	WriteJSON(w, http.StatusAccepted, map[string]any{"project": indexingProject(root, job)})
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	root := strings.TrimSpace(r.URL.Query().Get("root"))
	if root == "" || !filepath.IsAbs(root) {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "root must be an absolute path")
		return
	}
	root = filepath.Clean(root)
	if canonical, err := canonicalPath(root); err == nil {
		root = canonical
	}
	if s.projectJobRunning(root) {
		WriteErrorCode(w, http.StatusConflict, TypeInvalidRequest, "job_running", "an index job is already running for this project")
		return
	}
	removed, err := workspace.Remove(root)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update the project registry")
		return
	}
	s.projectJobsMu.Lock()
	if job := s.projectJobs[root]; job != nil && !job.running {
		delete(s.projectJobs, root)
	}
	s.projectJobsMu.Unlock()
	WriteJSON(w, http.StatusOK, map[string]bool{"removed": removed})
}

func (s *Server) projects() ([]projectResponse, error) {
	rows, err := projectstatus.Load()
	if err != nil {
		return nil, err
	}
	byRoot := make(map[string]projectResponse, len(rows))
	for _, row := range rows {
		byRoot[row.Root] = responseFromStatus(row)
	}

	s.projectJobsMu.Lock()
	for root, job := range s.projectJobs {
		response := byRoot[root]
		if response.Root == "" {
			response = projectResponse{
				Root: root, Name: filepath.Base(root), State: "not indexed",
				EmbedModel: projectstatus.DefaultEmbedModel,
			}
		}
		response.Job = responseJob(job)
		if job.running {
			response.State = "indexing"
			response.Error = ""
			response.Status = nil
		} else if job.error != "" {
			response.State = "error"
			response.Error = job.error
		}
		byRoot[root] = response
	}
	s.projectJobsMu.Unlock()

	projects := make([]projectResponse, 0, len(byRoot))
	for _, project := range byRoot {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Root < projects[j].Root })
	return projects, nil
}

func responseFromStatus(row projectstatus.Row) projectResponse {
	response := projectResponse{
		Root:       row.Root,
		Name:       filepath.Base(row.Root),
		State:      row.State,
		EmbedModel: row.EmbedModel,
	}
	if row.Err != nil {
		response.Error = row.Err.Error()
	}
	if row.State == "ready" || row.State == "semantic building" {
		status := row.Status
		response.Status = &status
	}
	return response
}

func indexingProject(root string, job *runningProjectJob) projectResponse {
	return projectResponse{
		Root:       root,
		Name:       filepath.Base(root),
		State:      "indexing",
		EmbedModel: projectstatus.DefaultEmbedModel,
		Job: &projectJob{
			Kind:      job.kind,
			StartedAt: job.startedAt.UTC().Format(time.RFC3339),
		},
	}
}

func responseJob(job *runningProjectJob) *projectJob {
	if job == nil {
		return nil
	}
	return &projectJob{
		Kind:      job.kind,
		StartedAt: job.startedAt.UTC().Format(time.RFC3339),
		Error:     job.error,
	}
}

func (s *Server) startProjectJob(root, kind string, run func(context.Context) error) (*runningProjectJob, bool) {
	s.projectJobsMu.Lock()
	if existing := s.projectJobs[root]; existing != nil && existing.running {
		s.projectJobsMu.Unlock()
		return existing, false
	}
	job := &runningProjectJob{kind: kind, startedAt: time.Now(), running: true, done: make(chan struct{})}
	if s.projectJobs == nil {
		s.projectJobs = make(map[string]*runningProjectJob)
	}
	s.projectJobs[root] = job
	ctx := s.projectJobsContext
	s.projectJobsMu.Unlock()

	go func() {
		err := run(ctx)
		s.projectJobsMu.Lock()
		job.running = false
		if err != nil && !errors.Is(err, context.Canceled) {
			job.error = err.Error()
		} else if err == nil && s.projectJobs[root] == job {
			delete(s.projectJobs, root)
		}
		close(job.done)
		s.projectJobsMu.Unlock()
	}()
	return job, true
}

func (s *Server) projectJobRunning(root string) bool {
	s.projectJobsMu.Lock()
	defer s.projectJobsMu.Unlock()
	job := s.projectJobs[root]
	return job != nil && job.running
}

func validateProjectRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("root must be an absolute path")
	}
	canonical, err := canonicalPath(root)
	if err != nil {
		return "", errors.New("root must be an existing directory")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("root must be an existing directory")
	}
	if canonical == string(filepath.Separator) {
		return "", errors.New("root must not be the filesystem root")
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if homeCanonical, homeErr := canonicalPath(home); homeErr == nil && canonical == homeCanonical {
			return "", errors.New("root must not be the home directory")
		}
	}
	return canonical, nil
}

func registeredProject(root string) (bool, error) {
	entries, err := workspace.List()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		canonical, err := canonicalPath(entry.Root)
		if err == nil && canonical == root {
			return true, nil
		}
	}
	return false, nil
}
