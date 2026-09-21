package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// registerKeysRoutes mounts the credential, provider-catalogue and key-health
// surface the Keys page drives. Every route is session-gated: these read and
// mutate the operator's own credentials, so a machine-plane key must never
// reach them.
//
// The health-check routes live here rather than in an analytics slice on
// purpose - probing a key is a credential operation, and its one delicate
// property (a relayed upstream rejection must not sign the operator out) is the
// same property the whole keys surface turns on.
func (s *Server) registerKeysRoutes() {
	s.mux.HandleFunc("GET /api/keys", s.RequireKey(s.handleListKeys))
	s.mux.HandleFunc("GET /api/keys/providers", s.RequireKey(s.handleListProviders))
	s.mux.HandleFunc("POST /api/keys", s.RequireKey(s.handleAddKey))
	s.mux.HandleFunc("POST /api/keys/{id}/reveal", s.RequireKey(s.handleRevealKey))
	// The literal platform sub-tree is registered alongside the {id} form; the
	// two never collide because they have different segment counts, and Go's
	// mux resolves the more specific pattern regardless of registration order.
	s.mux.HandleFunc("PATCH /api/keys/platform/{platform}", s.RequireKey(s.handleTogglePlatform))
	s.mux.HandleFunc("PATCH /api/keys/{id}", s.RequireKey(s.handleUpdateKey))
	s.mux.HandleFunc("DELETE /api/keys/{id}/cooldowns", s.RequireKey(s.handleClearCooldowns))
	s.mux.HandleFunc("DELETE /api/keys/{id}", s.RequireKey(s.handleDeleteKey))

	s.mux.HandleFunc("GET /api/health", s.RequireKey(s.handleHealth))
	s.mux.HandleFunc("POST /api/health/check/{keyId}", s.RequireKey(s.handleCheckKey))
	s.mux.HandleFunc("POST /api/health/check-all", s.RequireKey(s.handleCheckAll))
}

// ── wire shapes ──────────────────────────────────────────────────────────────

// keyView is one row of GET /api/keys. Field names are camelCase to match the
// vendored client verbatim (keys.ts:311-337); they deliberately differ from the
// KeyVault's snake_case JSON tags, so this surface owns its own struct rather
// than marshalling KeyRow.
//
// The nullable fields are pointers, not bare values: the client renders a null
// baseUrl/lastCheckedAt/lastHealthError as "-" and a zero as real data, so an
// absent measurement must serialise as null, never as 0 or "".
type keyView struct {
	ID         int64   `json:"id"`
	Platform   string  `json:"platform"`
	Label      string  `json:"label"`
	MaskedKey  string  `json:"maskedKey"`
	BaseURL    *string `json:"baseUrl"`
	Status     string  `json:"status"`
	Enabled    bool    `json:"enabled"`
	Keyless    bool    `json:"keyless"`
	Exportable bool    `json:"exportable"`
	// Timestamps go out as the reference's TEXT format, not the integers
	// this schema stores: the client calls string methods on them.
	CreatedAt       string         `json:"createdAt"`
	LastCheckedAt   *string        `json:"lastCheckedAt"`
	LastHealthError *string        `json:"lastHealthError"`
	ModelScope      []string       `json:"modelScope"`
	MaskedProxyURL  string         `json:"maskedProxyUrl"`
	Models          *[]customModel `json:"models,omitempty"`
	Cooldowns       []cooldownView `json:"cooldowns"`
}

// customModel is one relay model bound to a custom endpoint key. The Go schema
// carries only chat models (there are no separate embedding/media tables), so
// every entry is kind "chat" with a null family (keys.ts:279-285).
type customModel struct {
	ID          int64   `json:"id"`
	Kind        string  `json:"kind"`
	ModelID     string  `json:"modelId"`
	DisplayName string  `json:"displayName"`
	Family      *string `json:"family"`
}

// cooldownView explains why a healthy, enabled key is being skipped: the router
// benches it per (platform, model, key) and this is the live bench for one
// model (keys.ts:332-336).
type cooldownView struct {
	ModelID     string `json:"modelId"`
	ExpiresAtMs int64  `json:"expiresAtMs"`
	RemainingMs int64  `json:"remainingMs"`
}

// ── GET /api/keys ────────────────────────────────────────────────────────────

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.engine.Vault().List(ctx)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not list keys")
		return
	}
	reg := s.engine.Registry()
	db := s.engine.DB()

	// Supplementary data - the custom-endpoint model lists and the model ids a
	// key can be benched on - is best-effort: a failure here must not blank the
	// whole page, so it degrades to empty rather than 500ing the list.
	customModels := loadCustomModels(ctx, db)
	platModels, customKeyModels := loadModelIDs(ctx, db)

	now := time.Now()
	cds := s.engine.Cooldowns()

	out := make([]keyView, 0, len(rows))
	for _, row := range rows {
		keyless := false
		if p, ok := reg.Get(row.Platform); ok {
			keyless = p.Keyless()
		}
		v := keyView{
			ID:             row.ID,
			Platform:       row.Platform,
			Label:          row.Label,
			MaskedKey:      row.Masked,
			Status:         string(row.Status),
			Enabled:        row.Enabled,
			Keyless:        keyless,
			Exportable:     exportableKey(row.Platform, keyless, row.BaseURL),
			CreatedAt:      sqliteDateTime(row.CreatedAt.Unix()),
			ModelScope:     row.ModelScope,
			MaskedProxyURL: "",
			Cooldowns:      []cooldownView{},
		}
		if row.BaseURL != "" {
			b := row.BaseURL
			v.BaseURL = &b
		}
		if !row.LastCheckedAt.IsZero() {
			t := sqliteDateTime(row.LastCheckedAt.Unix())
			v.LastCheckedAt = &t
		}
		if row.LastHealthError != "" {
			e := row.LastHealthError
			v.LastHealthError = &e
		}

		var candidate []string
		if row.Platform == "custom" {
			candidate = customKeyModels[row.ID]
			models := customModels[row.ID]
			if models == nil {
				models = []customModel{}
			}
			v.Models = &models
		} else {
			candidate = platModels[row.Platform]
		}
		for _, mid := range candidate {
			cd, ok := cds.Active(gateway.QuotaKey(row.Platform, mid, row.ID))
			if !ok {
				continue
			}
			v.Cooldowns = append(v.Cooldowns, cooldownView{
				ModelID:     mid,
				ExpiresAtMs: cd.Until.UnixMilli(),
				RemainingMs: remainingMs(cd.Until, now),
			})
		}
		out = append(out, v)
	}
	// A bare array, no wrapper: the client's query function is typed to exactly
	// this shape (spec §6 trap 10).
	WriteJSON(w, http.StatusOK, out)
}

// exportableKey approximates the reference's isExportableKey (keys.ts:172-176)
// without decrypting every row on a list poll: a custom endpoint is exportable
// once it has a base URL, and any non-keyless key holds a real secret while a
// keyless one holds only the no-key sentinel.
func exportableKey(platform string, keyless bool, baseURL string) bool {
	if platform == "custom" {
		return baseURL != ""
	}
	return !keyless
}

func remainingMs(until, now time.Time) int64 {
	d := until.Sub(now)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// loadCustomModels groups every custom relay model by the key row it is bound
// to. The Go schema binds each model to a single key_id, so the grouping is by
// key rather than by endpoint.
func loadCustomModels(ctx context.Context, db *sql.DB) map[int64][]customModel {
	out := map[int64][]customModel{}
	rows, err := db.QueryContext(ctx,
		`SELECT key_id, id, model_id, display_name
		   FROM models
		  WHERE platform = 'custom' AND key_id IS NOT NULL
		  ORDER BY key_id, display_name`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var (
			keyID sql.NullInt64
			id    int64
			mid   string
			name  string
		)
		if err := rows.Scan(&keyID, &id, &mid, &name); err != nil {
			return out
		}
		if !keyID.Valid {
			continue
		}
		out[keyID.Int64] = append(out[keyID.Int64], customModel{
			ID:          id,
			Kind:        "chat",
			ModelID:     mid,
			DisplayName: name,
			Family:      nil,
		})
	}
	return out
}

// loadModelIDs returns, for cooldown enumeration, the distinct model ids each
// platform serves and the model ids each custom key serves. The router benches
// per (platform, model, key), and the in-memory engine has no way to list a
// key's benches, so the caller probes each candidate model instead.
func loadModelIDs(ctx context.Context, db *sql.DB) (byPlatform map[string][]string, byCustomKey map[int64][]string) {
	byPlatform = map[string][]string{}
	byCustomKey = map[int64][]string{}
	seen := map[string]map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT platform, model_id, key_id FROM models`)
	if err != nil {
		return byPlatform, byCustomKey
	}
	defer rows.Close()
	for rows.Next() {
		var (
			platform string
			modelID  string
			keyID    sql.NullInt64
		)
		if err := rows.Scan(&platform, &modelID, &keyID); err != nil {
			return byPlatform, byCustomKey
		}
		if platform == "custom" && keyID.Valid {
			byCustomKey[keyID.Int64] = append(byCustomKey[keyID.Int64], modelID)
			continue
		}
		if seen[platform] == nil {
			seen[platform] = map[string]bool{}
		}
		if seen[platform][modelID] {
			continue
		}
		seen[platform][modelID] = true
		byPlatform[platform] = append(byPlatform[platform], modelID)
	}
	return byPlatform, byCustomKey
}

// ── GET /api/keys/providers ──────────────────────────────────────────────────

type providerView struct {
	Platform        string `json:"platform"`
	Name            string `json:"name"`
	Keyless         bool   `json:"keyless"`
	Configured      bool   `json:"configured"`
	KeyCount        int    `json:"keyCount"`
	EnabledKeyCount int    `json:"enabledKeyCount"`
	// ModelCount is the enabled-model count the platform currently exposes.
	// It is a Prowl addition beyond the reference's checklist fields; the
	// client ignores unknown fields, so it costs nothing and answers the
	// directory's "how much does this provider actually offer" question.
	ModelCount int `json:"modelCount"`
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := s.engine.DB()

	type counts struct{ total, enabled int }
	byPlatform := map[string]counts{}
	if rows, err := db.QueryContext(ctx,
		`SELECT platform, COUNT(*), SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END)
		   FROM api_keys GROUP BY platform`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			var total, enabled int
			if err := rows.Scan(&p, &total, &enabled); err == nil {
				byPlatform[p] = counts{total: total, enabled: enabled}
			}
		}
	}

	models := map[string]int{}
	if rows, err := db.QueryContext(ctx,
		`SELECT platform, COUNT(*) FROM models WHERE enabled = 1 AND available = 1 GROUP BY platform`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var p string
			var n int
			if err := rows.Scan(&p, &n); err == nil {
				models[p] = n
			}
		}
	}

	// Registry.All() includes a `custom` entry, but custom is a per-key
	// user-defined placeholder, not a fixed provider to check off, so the
	// checklist excludes it (keys.ts:216-217).
	providers := make([]providerView, 0)
	configured := 0
	for _, p := range s.engine.Registry().All() {
		if p.Platform() == "custom" {
			continue
		}
		c := byPlatform[p.Platform()]
		view := providerView{
			Platform:        p.Platform(),
			Name:            p.Name(),
			Keyless:         p.Keyless(),
			Configured:      c.total > 0,
			KeyCount:        c.total,
			EnabledKeyCount: c.enabled,
			ModelCount:      models[p.Platform()],
		}
		if view.Configured {
			configured++
		}
		providers = append(providers, view)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })

	WriteJSON(w, http.StatusOK, map[string]any{
		"providers": providers,
		"summary": map[string]int{
			"total":        len(providers),
			"configured":   configured,
			"unconfigured": len(providers) - configured,
		},
	})
}

// ── POST /api/keys ───────────────────────────────────────────────────────────

func (s *Server) handleAddKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Platform string `json:"platform"`
		Key      string `json:"key"`
		Label    string `json:"label"`
		ProxyURL string `json:"proxyUrl"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	platform := strings.TrimSpace(req.Platform)
	if !s.knownPlatform(platform) {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			fmt.Sprintf("unknown platform %q", req.Platform))
		return
	}

	keyless := false
	if p, ok := s.engine.Registry().Get(platform); ok {
		keyless = p.Keyless()
	}
	rawKey := strings.TrimSpace(req.Key)
	if !keyless && rawKey == "" {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "key is required")
		return
	}
	// A keyless provider (an anonymous gateway) stores a sentinel so routing
	// sees the platform as configured; the provider omits the auth header.
	keyToStore := rawKey
	if keyless && rawKey == "" {
		keyToStore = "no-key"
	}

	ctx := r.Context()
	db := s.engine.DB()

	// A keyless provider needs only one sentinel row: re-enable the existing
	// one instead of piling up a duplicate every time the user clicks Add
	// (keys.ts:565-582).
	if keyless {
		var existing int64
		err := db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE platform = ? LIMIT 1`, platform).Scan(&existing)
		if err == nil {
			if _, err := db.ExecContext(ctx,
				`UPDATE api_keys SET enabled = 1, status = 'unknown' WHERE id = ?`, existing); err != nil {
				WriteError(w, http.StatusInternalServerError, TypeServer, "could not re-enable key")
				return
			}
			s.discoverModels(ctx, platform, existing)
			s.writeAddedKey(w, ctx, http.StatusOK, existing, platform, req.Label, false)
			return
		} else if err != sql.ErrNoRows {
			WriteError(w, http.StatusInternalServerError, TypeServer, "could not add key")
			return
		}
	}

	id, err := s.engine.Vault().Add(platform, keyToStore, gateway.AddOptions{Label: req.Label})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not add key")
		return
	}
	s.discoverModels(ctx, platform, id)
	s.writeAddedKey(w, ctx, http.StatusCreated, id, platform, req.Label, true)
}

// writeAddedKey emits the add/re-enable response. The masked preview is read
// back through the vault so the plaintext never has to be re-masked here, and
// modelsAvailable/notice tell the client whether the just-added key has any
// catalog models to route to yet (keys.ts:595-607).
func (s *Server) writeAddedKey(w http.ResponseWriter, ctx context.Context, status int, id int64, platform, label string, withProxy bool) {
	masked := ""
	if row, ok, err := s.engine.Vault().Get(ctx, id); err == nil && ok {
		masked = row.Masked
	}
	available := s.enabledModelCount(ctx, platform)
	body := map[string]any{
		"id":              id,
		"platform":        platform,
		"label":           label,
		"maskedKey":       masked,
		"status":          string(gateway.StatusUnknown),
		"enabled":         true,
		"modelsAvailable": available,
	}
	if withProxy {
		body["maskedProxyUrl"] = ""
	}
	if available == 0 {
		body["notice"] = fmt.Sprintf(
			"Key saved, but no %s models are in your current catalog yet. "+
				"Add %s as a custom OpenAI-compatible provider with its base URL "+
				"to use it now.", platform, platform)
	}
	WriteJSON(w, status, body)
}

func (s *Server) enabledModelCount(ctx context.Context, platform string) int {
	var n int
	_ = s.engine.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM models WHERE platform = ? AND enabled = 1 AND available = 1`, platform).Scan(&n)
	return n
}

// discoverModels lists the models the just-added credential can reach and
// upserts the ones the catalog does not already ship as operator-discovered
// rows, so a provider that ships zero catalog models becomes routable once a
// key is present. It is best-effort and deliberately silent on failure:
//
//   - Only adapters that implement the OpenAI-compatible discovery verb are
//     probed; a native-wire adapter (Anthropic, Codex) is skipped.
//   - A discovery failure - unreachable endpoint, auth rejection, unparseable
//     body - inserts nothing and NEVER marks the credential healthy. Listing
//     models is not evidence the key works: several providers serve /models
//     with no credential, so a successful list against a dead key would be a
//     verdict discovery never earned. Health stays whatever a real probe or a
//     routed request decides.
//   - A model already in the table (a catalog row, a custom endpoint, an
//     earlier discovery) is left untouched by ON CONFLICT DO NOTHING, so only
//     genuinely new ids are added and catalog metadata is never clobbered.
//   - The credential is revealed transiently to make the request and never
//     enters a log or the response; only counts and platform names surface.
func (s *Server) discoverModels(ctx context.Context, platform string, id int64) {
	row, ok, err := s.engine.Vault().Get(ctx, id)
	if err != nil || !ok {
		return
	}
	// Resolve with the key's own base URL so a custom endpoint discovers
	// against its relay; a built-in platform ignores the URL and returns its
	// singleton adapter.
	prov, ok := s.engine.Registry().Resolve(platform, row.BaseURL)
	if !ok {
		return
	}
	lister, ok := prov.(provider.ModelLister)
	if !ok {
		return
	}
	secret, err := s.engine.Vault().Reveal(ctx, id)
	if err != nil {
		return
	}
	models, err := lister.ListModels(ctx, secret)
	if err != nil || len(models) == 0 {
		return
	}
	// A models row alone is not routable: the default active chain is an INNER
	// JOIN on fallback_config, so a model with no chain row is listed but never
	// selected. Mirror custom-model registration - add each newly discovered
	// model to the global fallback chain and to every profile - so a provider
	// that shipped zero models becomes routable, not merely present. The whole
	// upsert runs in one transaction; the store caps SQLite at a single
	// connection and Reveal's read cursor is already consumed, so no read is
	// held open across these writes.
	tx, err := s.engine.DB().BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()

	// A discovered model's capability is unknown. The scorer makes a LOWER
	// ordinal rank more capable (IntelligenceComposite subtracts sqrt(rank)),
	// so the schema default of 0 would make every unknown model look like the
	// best in its tier. Seed the current catalogue's worst rank + 1 for both
	// axes so an unknown model sorts at the bottom until real traffic or an
	// operator edit says otherwise.
	var intelRank, speedRank int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(intelligence_rank), 0) + 1, COALESCE(MAX(speed_rank), 0) + 1 FROM models`).
		Scan(&intelRank, &speedRank); err != nil {
		return
	}

	insModel, err := tx.PrepareContext(ctx, `
		INSERT INTO models (platform, model_id, display_name, intelligence_rank, speed_rank, source, endpoint_scope, enabled)
		VALUES (?, ?, ?, ?, ?, 'discovered', '', 1)
		ON CONFLICT(platform, model_id, endpoint_scope) DO NOTHING`)
	if err != nil {
		return
	}
	defer insModel.Close()
	insFallback, err := tx.PrepareContext(ctx, `
		INSERT INTO fallback_config (model_db_id, position, enabled)
		VALUES (?, (SELECT COALESCE(MAX(position), 0) + 1 FROM fallback_config), 1)
		ON CONFLICT(model_db_id) DO NOTHING`)
	if err != nil {
		return
	}
	defer insFallback.Close()
	insProfile, err := tx.PrepareContext(ctx, `
		INSERT INTO profile_models (profile_id, model_db_id, position)
		SELECT p.id, ?, COALESCE((SELECT MAX(position) + 1 FROM profile_models pm2 WHERE pm2.profile_id = p.id), 1)
		  FROM profiles p
		 WHERE NOT EXISTS (SELECT 1 FROM profile_models pm WHERE pm.profile_id = p.id AND pm.model_db_id = ?)`)
	if err != nil {
		return
	}
	defer insProfile.Close()

	for _, m := range models {
		res, err := insModel.ExecContext(ctx, platform, m.ID, m.Name, intelRank, speedRank)
		if err != nil {
			return
		}
		// Only a genuinely new row joins the chain; an already-known model
		// (catalog, custom, or a prior discovery) keeps the operator's own
		// membership and enable choices untouched.
		if affected, err := res.RowsAffected(); err != nil || affected == 0 {
			continue
		}
		newID, err := res.LastInsertId()
		if err != nil || newID == 0 {
			continue
		}
		if _, err := insFallback.ExecContext(ctx, newID); err != nil {
			return
		}
		if _, err := insProfile.ExecContext(ctx, newID, newID); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

// ── PATCH /api/keys/{id} ─────────────────────────────────────────────────────

func (s *Server) handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(r, "id")
	if !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid key ID")
		return
	}
	// Decoding into raw messages is what distinguishes an absent field from an
	// explicit null: modelScope:null clears the scope, while an absent
	// modelScope leaves it untouched.
	var raw map[string]json.RawMessage
	if !DecodeJSON(w, r, &raw) {
		return
	}

	var (
		sets []string
		args []any
		resp = map[string]any{"success": true}
	)

	if v, present := raw["enabled"]; present {
		var enabled bool
		if json.Unmarshal(v, &enabled) != nil {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "enabled must be a boolean")
			return
		}
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(enabled))
		resp["enabled"] = enabled
	}
	if v, present := raw["label"]; present {
		var label string
		if json.Unmarshal(v, &label) != nil {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "label must be a string")
			return
		}
		sets = append(sets, "label = ?")
		args = append(args, label)
		resp["label"] = label
	}
	if v, present := raw["baseUrl"]; present {
		var baseURL string
		if json.Unmarshal(v, &baseURL) != nil {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl must be a string")
			return
		}
		trimmed := strings.TrimSpace(baseURL)
		// Edit is the way around the create-time SSRF guard: an unchecked
		// base_url here would let an operator (or anything on their session)
		// re-point an existing key at a metadata address, and every later
		// provider call would carry that key's credential there. Clearing the
		// URL (empty) has no outbound target and is left alone.
		if trimmed != "" {
			if ok, reason := keysCustomAssessURL(trimmed); !ok {
				WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl rejected: "+reason)
				return
			}
		}
		sets = append(sets, "base_url = ?")
		args = append(args, nullIfEmpty(trimmed))
		resp["baseUrl"] = baseURL
	}
	if v, present := raw["modelScope"]; present {
		var arr []string
		if string(v) != "null" {
			if json.Unmarshal(v, &arr) != nil {
				WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "modelScope must be a string array or null")
				return
			}
		}
		ids := dedupeStrings(arr)
		sets = append(sets, "model_scope_json = ?")
		if len(ids) > 0 {
			b, _ := json.Marshal(ids)
			args = append(args, string(b))
			resp["modelScope"] = ids
		} else {
			args = append(args, nil)
			resp["modelScope"] = nil
		}
	}
	// The per-key proxy override the reference stores has no column in this
	// schema, so proxyUrl is accepted but not persisted; the client is told the
	// masked result is empty rather than being rejected.
	if _, present := raw["proxyUrl"]; present {
		resp["maskedProxyUrl"] = ""
	}

	if len(sets) == 0 {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			"At least one of enabled, label, modelScope or baseUrl must be provided")
		return
	}

	args = append(args, id)
	res, err := s.engine.DB().ExecContext(r.Context(),
		"UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update key")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		WriteError(w, http.StatusNotFound, TypeInvalidRequest, "Key not found")
		return
	}
	WriteJSON(w, http.StatusOK, resp)
}

// ── PATCH /api/keys/platform/{platform} ──────────────────────────────────────

func (s *Server) handleTogglePlatform(w http.ResponseWriter, r *http.Request) {
	platform := r.PathValue("platform")
	if !s.knownPlatform(platform) {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			fmt.Sprintf("Invalid platform '%s'", platform))
		return
	}
	var raw map[string]json.RawMessage
	if !DecodeJSON(w, r, &raw) {
		return
	}
	v, present := raw["enabled"]
	var enabled bool
	if !present || json.Unmarshal(v, &enabled) != nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "enabled must be a boolean")
		return
	}
	res, err := s.engine.DB().ExecContext(r.Context(),
		`UPDATE api_keys SET enabled = ? WHERE platform = ?`, boolToInt(enabled), platform)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not update keys")
		return
	}
	updated, _ := res.RowsAffected()
	WriteJSON(w, http.StatusOK, map[string]any{
		"success": true, "enabled": enabled, "updatedKeys": updated,
	})
}

// ── DELETE /api/keys/{id} ────────────────────────────────────────────────────

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(r, "id")
	if !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid key ID")
		return
	}
	ctx := r.Context()
	if _, found, err := s.engine.Vault().Get(ctx, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete key")
		return
	} else if !found {
		WriteError(w, http.StatusNotFound, TypeInvalidRequest, "Key not found")
		return
	}
	// The models.key_id foreign key is ON DELETE CASCADE and foreign_keys is
	// enforced, so deleting the key sweeps its custom models with it.
	if err := s.engine.Vault().Delete(ctx, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not delete key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ── POST /api/keys/{id}/reveal ───────────────────────────────────────────────

func (s *Server) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	// Reveal is the ONLY endpoint that returns a credential in the clear. The
	// reference gates it behind a password re-prompt except on a loopback
	// desktop build; Prowl is that single-operator local harness and exposes no
	// standalone password verifier, so the session gate is the whole check.
	id, ok := parseIDParam(r, "id")
	if !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid key ID")
		return
	}
	ctx := r.Context()
	if _, found, err := s.engine.Vault().Get(ctx, id); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not reveal key")
		return
	} else if !found {
		WriteError(w, http.StatusNotFound, TypeInvalidRequest, "Key not found")
		return
	}
	plaintext, err := s.engine.Vault().Reveal(ctx, id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer,
			"This key could not be decrypted. It was stored with a different ENCRYPTION_KEY.")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"key": plaintext})
}

// ── DELETE /api/keys/{id}/cooldowns ──────────────────────────────────────────

func (s *Server) handleClearCooldowns(w http.ResponseWriter, r *http.Request) {
	// This route speaks the older bare-string error family, not the OpenAI
	// envelope (keys.ts:347-357); the client tolerates both, and reproducing
	// the family per route keeps the contract faithful.
	id, ok := parseIDParam(r, "id")
	if !ok {
		WriteBareError(w, http.StatusBadRequest, "Invalid key id")
		return
	}
	ctx := r.Context()
	row, found, err := s.engine.Vault().Get(ctx, id)
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, "could not clear cooldowns")
		return
	}
	if !found {
		WriteBareError(w, http.StatusNotFound, "Key not found")
		return
	}

	var candidate []string
	platModels, customKeyModels := loadModelIDs(ctx, s.engine.DB())
	if row.Platform == "custom" {
		candidate = customKeyModels[id]
	} else {
		candidate = platModels[row.Platform]
	}

	// Only a heuristic bench is our own guess to withdraw. A provider-stated
	// wait, an out-of-credit bench or a tier lock is not ours to lift, so
	// CooldownEngine.Clear leaves it in place and it is not counted.
	cds := s.engine.Cooldowns()
	cleared := 0
	for _, mid := range candidate {
		key := gateway.QuotaKey(row.Platform, mid, id)
		if cd, active := cds.Active(key); active && cd.Source == gateway.SourceHeuristic {
			cds.Clear(key)
			cleared++
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"cleared": cleared})
}

// ── GET /api/health ──────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	db := s.engine.DB()
	reg := s.engine.Registry()

	platforms := make([]map[string]any, 0)
	if rows, err := db.QueryContext(ctx, `
		SELECT platform,
		       COUNT(*),
		       SUM(CASE WHEN status = 'healthy' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status = 'error'   THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status = 'unknown' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END)
		  FROM api_keys GROUP BY platform`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var (
				platform                                  string
				total, healthy, errored, unknown, enabled int
			)
			if err := rows.Scan(&platform, &total, &healthy, &errored, &unknown, &enabled); err != nil {
				continue
			}
			platforms = append(platforms, map[string]any{
				"platform":    platform,
				"hasProvider": platform == "custom" || reg.Has(platform),
				"totalKeys":   total,
				"healthyKeys": healthy,
				// The Go health model has no rate_limited/invalid states - a
				// credential is unknown, healthy or error - so these are always
				// zero rather than fabricated from another signal.
				"rateLimitedKeys": 0,
				"invalidKeys":     0,
				"errorKeys":       errored,
				"unknownKeys":     unknown,
				"enabledKeys":     enabled,
			})
		}
	}

	keys := make([]map[string]any, 0)
	if rows, err := db.QueryContext(ctx, `
		SELECT id, platform, label, status, enabled, created_at, last_checked_at, last_health_error
		  FROM api_keys ORDER BY platform, created_at DESC`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var (
				id                      int64
				platform, label, status string
				enabled                 int
				createdAt               int64
				lastChecked             sql.NullInt64
				lastErr                 sql.NullString
			)
			if err := rows.Scan(&id, &platform, &label, &status, &enabled, &createdAt, &lastChecked, &lastErr); err != nil {
				continue
			}
			k := map[string]any{
				"id":       id,
				"platform": platform,
				"label":    label,
				"status":   status,
				"enabled":  enabled == 1,
				// Date strings, matching the reference: the client calls
				// string methods on these, so an integer crashes the page.
				"createdAt":       sqliteDateTime(createdAt),
				"lastCheckedAt":   nil,
				"lastHealthError": nil,
			}
			if lastChecked.Valid {
				k["lastCheckedAt"] = sqliteDateTime(lastChecked.Int64)
			}
			if lastErr.Valid {
				k["lastHealthError"] = lastErr.String
			}
			keys = append(keys, k)
		}
	}

	// The provider's own account of its remaining quota, which is not the
	// same as our counters. The dashboard shows both because when they
	// disagree, the difference is the interesting part.
	// Flattened to one row per metric: the dashboard's quota view reads
	// `metric`, `source` and a date-string `observedAt` per row, and the
	// stored pool shape has none of those.
	quotaStates := s.quotaSignals(ctx)

	WriteJSON(w, http.StatusOK, map[string]any{
		"platforms":   platforms,
		"keys":        keys,
		"quotaStates": quotaStates,
		"degradation": s.degradationSnapshot(ctx),
	})
}

// degradationSnapshot reports the fleet's health and mode.
//
// An "enabled provider" is a platform with at least one key; a healthy one has
// at least one enabled key that is not in error. An unknown status counts as
// healthy, because "not yet probed" is not evidence of failure
// (degradation.ts:19-34, HEALTHY_STATUSES 'healthy' and 'unknown').
//
// The snapshot is fed to the engine's monitor, which owns the hysteresis: a
// single bad reading must not flip the fleet, and a recovery must hold before
// exploration resumes.
func (s *Server) degradationSnapshot(ctx context.Context) map[string]any {
	// The same snapshot the routing path observes per request, so the dashboard
	// and the live router read one fleet-health source and can never disagree.
	snapshot := s.fleetHealthSnapshot(ctx)
	state := s.engine.Degradation().Observe(snapshot)

	out := map[string]any{
		"healthyProviders": snapshot.Healthy,
		"totalProviders":   snapshot.Total,
		"ratio":            snapshot.Ratio(),
		"state":            string(state),
		"degradedAt":       nil,
	}
	// A pending transition is shown so an operator can see the gateway
	// noticing a problem before it acts on it.
	if pending, elapsed := s.engine.Degradation().PendingTransition(); pending {
		out["pendingForMs"] = elapsed.Milliseconds()
	}
	return out
}

// ── POST /api/health/check/{keyId} ───────────────────────────────────────────

func (s *Server) handleCheckKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(r, "keyId")
	if !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "Invalid key ID")
		return
	}
	// The trap this whole slice turns on: probing a key relays an upstream
	// verdict. A confirmed-bad credential (the provider answered 401/403) comes
	// back as a 200 whose body says status "error" - NEVER as a 401 carrying
	// authentication_error, which is the one response that ends the operator's
	// session. CheckKey records the verdict and returns no error for it, so the
	// bad-key path lands here with status "error" and a 200.
	status, err := s.engine.Vault().CheckKey(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			WriteError(w, http.StatusNotFound, TypeInvalidRequest, "Key not found")
			return
		}
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not check key")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"keyId": id, "status": string(status)})
}

// ── POST /api/health/check-all ───────────────────────────────────────────────

func (s *Server) handleCheckAll(w http.ResponseWriter, r *http.Request) {
	// Forced: the operator pressed the button and wants an answer about every
	// key now, so the scheduled pass's recency skip and provider spacing do
	// not apply (health.ts:74-79).
	if _, err := s.engine.Vault().CheckAllKeys(r.Context(), gateway.HealthPassOptions{Force: true}); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not check keys")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ── small helpers ────────────────────────────────────────────────────────────

func (s *Server) knownPlatform(platform string) bool {
	return platform == "custom" || s.engine.Registry().Has(platform)
}

func parseIDParam(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue(name)), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
