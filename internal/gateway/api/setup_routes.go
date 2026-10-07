package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/inject"
	"github.com/neur0map/prowl/internal/gateway/setupstate"
	"github.com/neur0map/prowl/internal/setup"
	"github.com/neur0map/prowl/internal/version"
)

type harnessRow struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Detected        bool                   `json:"detected"`
	Injected        bool                   `json:"injected"`
	Active          bool                   `json:"active"`
	CredentialStale bool                   `json:"credentialStale"`
	Files           []string               `json:"files"`
	Note            string                 `json:"note,omitempty"`
	Skills          setupstate.SkillStatus `json:"skills"`
}

type setupHarnessesResponse struct {
	Routable    bool         `json:"routable"`
	Reason      string       `json:"reason,omitempty"`
	Harnesses   []harnessRow `json:"harnesses"`
	SkillsError string       `json:"skillsError,omitempty"`
}

type harnessSetupRequest struct {
	Activate bool `json:"activate,omitempty"`
	Force    bool `json:"force,omitempty"`
}

type skillsSetupRequest struct {
	Clients []string `json:"clients,omitempty"`
}

type skillsSetupResponse struct {
	Installed int    `json:"installed"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Conflicts int    `json:"conflicts"`
	Message   string `json:"message"`
}

func (s *Server) registerSetupRoutes() {
	s.mux.HandleFunc("GET /api/setup/harnesses", s.RequireKey(s.handleSetupHarnesses))
	s.mux.HandleFunc("POST /api/setup/harnesses/{id}", s.RequireKey(s.handleSetupHarness))
	s.mux.HandleFunc("DELETE /api/setup/harnesses/{id}", s.RequireKey(s.handleRemoveHarness))
	s.mux.HandleFunc("POST /api/setup/skills", s.RequireKey(s.handleSetupSkills))
}

func (s *Server) handleSetupHarnesses(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not find the user home directory")
		return
	}
	currentKey, err := s.UnifiedAPIKey(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the gateway api key")
		return
	}
	routable, reason := s.setupRoutability()
	state := setupstate.Load(home, version.Version)
	response := setupHarnessesResponse{
		Routable:  routable,
		Reason:    reason,
		Harnesses: setupHarnessRows(home, state, s.localToken, currentKey),
	}
	if state.SkillsErr != nil {
		response.SkillsError = state.SkillsErr.Error()
	}
	WriteJSON(w, http.StatusOK, response)
}

func (s *Server) handleSetupHarness(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !supportedHarness(id) {
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "harness_not_found", "unknown harness: "+id)
		return
	}
	var body harnessSetupRequest
	if !DecodeJSON(w, r, &body) {
		return
	}
	if body.Activate && !body.Force {
		if routable, reason := s.setupRoutability(); !routable {
			WriteErrorCode(w, http.StatusConflict, ErrorType("conflict"), "not_routable", reason)
			return
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not find the user home directory")
		return
	}
	port := localListenerPort(r)
	_, err = inject.Apply(inject.Options{
		Home:     home,
		BaseURL:  fmt.Sprintf("http://127.0.0.1:%d/v1", port),
		Token:    s.localToken,
		Models:   inject.RoutingModels(),
		Activate: body.Activate,
	}, id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	state := setupstate.Load(home, version.Version)
	currentKey, keyErr := s.UnifiedAPIKey(r.Context())
	if keyErr != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the gateway api key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"harness": setupHarnessRow(home, state, id, s.localToken, currentKey)})
}

func (s *Server) handleRemoveHarness(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !supportedHarness(id) {
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "harness_not_found", "unknown harness: "+id)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not find the user home directory")
		return
	}
	if _, err := inject.Remove(home, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	state := setupstate.Load(home, version.Version)
	currentKey, keyErr := s.UnifiedAPIKey(r.Context())
	if keyErr != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the gateway api key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"harness": setupHarnessRow(home, state, id, s.localToken, currentKey)})
}

func (s *Server) handleSetupSkills(w http.ResponseWriter, r *http.Request) {
	var body skillsSetupRequest
	if !DecodeJSON(w, r, &body) {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not find the user home directory")
		return
	}
	clients := body.Clients
	if len(clients) == 0 {
		clients = setup.DetectInstalledHarnesses()
	}
	opts := setup.UserInstallOptions{Home: home, Version: version.Version, Clients: clients}
	plan, err := setup.PlanUserSkills(opts)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	response := skillsSetupResponse{Conflicts: len(plan.Conflicts)}
	for _, action := range plan.Actions {
		switch action.Kind {
		case setup.UserActionInstall:
			response.Installed++
		case setup.UserActionUpdate, setup.UserActionRemove:
			response.Updated++
		case setup.UserActionUnchanged:
			response.Unchanged++
		}
	}
	if _, err := setup.ApplyUserSkills(opts, plan, true); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, err.Error())
		return
	}
	response.Message = fmt.Sprintf("%d installed, %d updated, %d unchanged, %d conflicts", response.Installed, response.Updated, response.Unchanged, response.Conflicts)
	WriteJSON(w, http.StatusOK, response)
}

func (s *Server) setupRoutability() (bool, string) {
	resolved, err := gateway.ResolveChain(s.engine.DB(), "auto", s.routingStrategy(context.Background()))
	if err != nil {
		return false, err.Error()
	}
	if len(resolved.Chain) == 0 {
		return false, gateway.ExplainUnroutableChain(s.engine.DB(), resolved.StrategyKey)
	}
	return true, ""
}

func setupHarnessRows(home string, state setupstate.State, localToken, currentKey string) []harnessRow {
	rows := make([]harnessRow, 0, len(state.Supported))
	for _, id := range state.Supported {
		rows = append(rows, setupHarnessRow(home, state, id, localToken, currentKey))
	}
	return rows
}

func setupHarnessRow(home string, state setupstate.State, id, localToken, currentKey string) harnessRow {
	target, injected := state.Targets[id]
	active, activeNote := inject.Active(home, id)
	note := target.Note
	if activeNote != "" {
		note = activeNote
	}
	stale := injected && inject.CredentialStale(target, localToken, currentKey)
	if stale {
		active = false
		note = "The gateway key in this harness no longer works. Reconnecting refreshes it."
	}
	files := target.Files
	if files == nil {
		files = []string{}
	}
	return harnessRow{
		ID:              id,
		Name:            harnessName(id),
		Detected:        state.Installed[id],
		Injected:        injected,
		Active:          active,
		CredentialStale: stale,
		Files:           files,
		Note:            note,
		Skills:          setupstate.StatusFor(state, id),
	}
}

func supportedHarness(id string) bool {
	for _, supported := range inject.Supported() {
		if id == supported {
			return true
		}
	}
	return false
}

func harnessName(id string) string {
	switch id {
	case "omp":
		return "OMP"
	case "pi":
		return "Pi"
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "hermes":
		return "Hermes"
	case "openclaw":
		return "OpenClaw"
	case "prowl-legacy":
		return "Prowl Legacy"
	default:
		return id
	}
}
