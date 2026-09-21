package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/neur0map/prowl/internal/gateway"
)

// Enrolling a Prowl login into the pool.
//
// A subscription or an api key configured in Prowl is spendable capacity the
// router could not previously reach - the pool only knew about keys pasted
// into the dashboard. Enrolling adds a pool row that REFERENCES the login
// instead of copying its secret, so refreshes are picked up and signing out
// revokes the pool's access too.

func (s *Server) registerLoginRoutes() {
	s.mux.HandleFunc("GET /api/logins", s.RequireKey(s.handleLoginsList))
	s.mux.HandleFunc("POST /api/logins/{id}/enroll", s.RequireKey(s.handleLoginEnroll))
	s.mux.HandleFunc("DELETE /api/logins/{id}/enroll", s.RequireKey(s.handleLoginWithdraw))
	s.mux.HandleFunc("GET /api/logins/usage", s.RequireKey(s.handleLoginsUsage))

	// The sign-in surface: the interactive flows are startable, pollable and
	// cancellable over the API, so the TUI needs no in-process shortcut and
	// one client code path serves both an owned and an attached gateway.
	s.mux.HandleFunc("GET /api/logins/platforms", s.RequireKey(s.handleLoginPlatforms))
	s.mux.HandleFunc("POST /api/signin", s.RequireKey(s.handleSignInStart))
	s.mux.HandleFunc("GET /api/signin/{id}", s.RequireKey(s.handleSignInStatus))
	s.mux.HandleFunc("DELETE /api/signin/{id}", s.RequireKey(s.handleSignInCancel))
	s.mux.HandleFunc("DELETE /api/logins/{id}/forget", s.RequireKey(s.handleLoginForget))
}

func (s *Server) handleLoginPlatforms(w http.ResponseWriter, r *http.Request) {
	if s.signIn == nil {
		WriteJSON(w, http.StatusOK, map[string]any{"platforms": []any{}})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"platforms": s.signIn.Signable()})
}

func (s *Server) handleSignInStart(w http.ResponseWriter, r *http.Request) {
	if s.signIn == nil {
		WriteBareError(w, http.StatusConflict,
			"this gateway is attached, and the attached surface does not run sign-in flows")
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if !DecodeJSON(w, r, &body) {
		return
	}
	session, err := s.signIn.Start(r.Context(), strings.ToLower(strings.TrimSpace(body.Provider)))
	if err != nil {
		WriteBareError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, session)
}

func (s *Server) handleSignInStatus(w http.ResponseWriter, r *http.Request) {
	if s.signIn == nil {
		WriteBareError(w, http.StatusNotFound, "no sign-in surface")
		return
	}
	session, err := s.signIn.Status(r.PathValue("id"))
	if err != nil {
		WriteBareError(w, http.StatusNotFound, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, session)
}

func (s *Server) handleSignInCancel(w http.ResponseWriter, r *http.Request) {
	if s.signIn == nil {
		WriteBareError(w, http.StatusNotFound, "no sign-in surface")
		return
	}
	// Cancel must run even without a live request context: the flow's own
	// deadline and the client disconnect are different things, and a
	// half-open loopback listener after a cancel would be a leaked port.
	canceled := s.signIn.Cancel(r.PathValue("id"))
	WriteJSON(w, http.StatusOK, map[string]any{"canceled": canceled})
}

func (s *Server) handleLoginForget(w http.ResponseWriter, r *http.Request) {
	if s.signIn == nil {
		WriteBareError(w, http.StatusConflict, "no login vault on this surface")
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if enrolled, err := s.enrolledLogins(r.Context()); err == nil {
		if keyID, ok := enrolled[id]; ok {
			if err := s.engine.Vault().Delete(r.Context(), keyID); err != nil {
				WriteBareError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	removed, err := s.signIn.Forget(id)
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) handleLoginsList(w http.ResponseWriter, r *http.Request) {
	src := s.engine.CredentialSource()
	if src == nil {
		// Not an error: a gateway running without the harness simply has no
		// logins to borrow.
		WriteJSON(w, http.StatusOK, map[string]any{"logins": []any{}})
		return
	}

	enrolled, err := s.enrolledLogins(r.Context())
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}

	logins := src.Linkable(r.Context())
	out := make([]map[string]any, 0, len(logins))
	for _, login := range logins {
		keyID, isEnrolled := enrolled[strings.ToLower(login.ID)]
		row := map[string]any{
			"id": login.ID, "name": login.Name, "kind": login.Kind,
			"detail": login.Detail, "enrolled": isEnrolled,
			// Offered is what enrolling would contribute; models is what it
			// already has. Showing both is what makes the action legible.
			"offered": len(src.Models(r.Context(), login.ID)),
			"models":  0,
		}
		if isEnrolled {
			row["keyId"] = keyID
			row["models"] = s.engine.LoginModelCount(r.Context(), keyID)
		}
		out = append(out, row)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"logins": out})
}

// loginKeyLabel is the provider's own display name, falling back to its id.
func loginKeyLabel(src gateway.CredentialSource, ctx context.Context, id string) string {
	for _, p := range src.Linkable(ctx) {
		if strings.EqualFold(p.ID, id) && strings.TrimSpace(p.Name) != "" {
			return p.Name
		}
	}
	return id
}

func (s *Server) handleLoginEnroll(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		WriteBareError(w, http.StatusBadRequest, "a provider id is required")
		return
	}
	src := s.engine.CredentialSource()
	if src == nil {
		WriteBareError(w, http.StatusConflict,
			"this gateway is running on its own, so it has no Prowl logins to borrow")
		return
	}

	// Enrolling something Prowl cannot actually serve would add a pool member
	// that fails on its first request.
	secret, known, err := src.Credential(r.Context(), id)
	switch {
	case err != nil:
		WriteBareError(w, http.StatusBadGateway, err.Error())
		return
	case !known:
		WriteBareError(w, http.StatusNotFound,
			"Prowl has no login for "+id+"; sign in first, then enroll it")
		return
	case secret == "":
		WriteBareError(w, http.StatusConflict,
			"the "+id+" login holds no credential yet; sign in again and retry")
		return
	}

	if s.signIn != nil {
		if err := s.signIn.SetPoolEnabled(id, true); err != nil {
			WriteBareError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	enrolled, err := s.enrolledLogins(r.Context())
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keyID, ok := enrolled[id]; ok {
		// Enrolling again converges rather than no-opping: a login enrolled
		// before models were seeded, or one whose catalogue has since grown,
		// otherwise stays permanently without anything to route to.
		seeded, err := s.engine.SeedLoginModels(r.Context(), keyID, id, src.Models(r.Context(), id))
		if err != nil {
			slog.Warn("Could not refresh an enrolled login's models",
				"provider", id, "error", err)
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"enrolled": true, "keyId": keyID, "models": seeded,
		})
		return
	}

	// The label names the provider, because it is what every surface shows
	// next to the model: "Prowl login" told the operator nothing about who
	// actually serves a request or which subscription it spends.
	keyID, err := s.engine.Vault().AddLinked(r.Context(), id, loginKeyLabel(src, r.Context(), id))
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The credential alone is inert: without models the router has nothing to
	// send to it and the Models page shows no change, which is what made
	// enrolling look like it did nothing.
	seeded, err := s.engine.SeedLoginModels(r.Context(), keyID, id, src.Models(r.Context(), id))
	if err != nil {
		slog.Warn("Enrolled a login but could not seed its models",
			"provider", id, "error", err)
	}
	WriteJSON(w, http.StatusCreated, map[string]any{
		"enrolled": true, "keyId": keyID, "models": seeded,
	})
}

func (s *Server) handleLoginWithdraw(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	enrolled, err := s.enrolledLogins(r.Context())
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}
	keyID, ok := enrolled[id]
	if !ok {
		WriteBareError(w, http.StatusNotFound, id+" is not in the pool")
		return
	}
	if s.signIn != nil {
		if err := s.signIn.SetPoolEnabled(id, false); err != nil {
			WriteBareError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.engine.Vault().Delete(r.Context(), keyID); err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"enrolled": false})
}

// enrolledLogins maps a provider id to the pool row that links to it.
func (s *Server) enrolledLogins(ctx context.Context) (map[string]int64, error) {
	rows, err := s.engine.DB().QueryContext(ctx,
		"SELECT id, encrypted_key FROM api_keys")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var (
			id     int64
			stored string
		)
		if err := rows.Scan(&id, &stored); err != nil {
			return nil, err
		}
		if ref, linked := gateway.LinkedRef(stored); linked {
			out[ref] = id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
