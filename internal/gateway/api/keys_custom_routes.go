package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// registerKeysCustomRoutes mounts the custom-endpoint, discovery and import
// surface the Keys page drives: point the gateway at any OpenAI-compatible base
// URL, probe it, discover its models, preview an export file and import a
// selection. Every route is session-gated - these read and mutate the
// operator's own credentials, so a machine-plane key must never reach them.
//
// A custom endpoint is a platform='custom' api_keys row carrying its own
// base_url (KeyVault + Registry.Resolve already model it), so nothing here adds
// a second key store; models bind to their endpoint key via models.key_id.
func (s *Server) registerKeysCustomRoutes() {
	s.mux.HandleFunc("POST /api/keys/custom", s.RequireKey(s.handleKeysCustomAdd))
	s.mux.HandleFunc("POST /api/keys/custom/probe", s.RequireKey(s.handleKeysCustomProbe))
	s.mux.HandleFunc("POST /api/keys/custom/discover-models", s.RequireKey(s.handleKeysCustomDiscover))
	s.mux.HandleFunc("POST /api/keys/preview", s.RequireKey(s.handleKeysImportPreview))
	s.mux.HandleFunc("POST /api/keys/import-selected", s.RequireKey(s.handleKeysImportSelected))
}

const (
	// The placeholder secret stored for an endpoint with auth off (llama.cpp,
	// LM Studio, vLLM). It is a sentinel, not a credential, and matches the
	// value POST /api/keys already uses (keys_routes.go handleAddKey).
	keysCustomNoKey = "no-key"

	// Hard cap on the /models body we will read. The largest real OpenAI-style
	// catalog is well under 1 MB (model-discovery.ts:20-22).
	keysCustomMaxCatalogBytes = 2 * 1024 * 1024

	// Discovery output and any submitted model list are untrusted: each entry
	// becomes a database row, so bound how many are accepted and how long an id
	// may be rather than storing whatever arrives (model-discovery.ts:24-28).
	keysCustomMaxDiscovered  = 500
	keysCustomMaxModelIDLen  = 256
	keysCustomMaxImportKeys  = 100
	keysCustomMaxImportFiles = 10

	// Not 1: several relays enforce a floor above it and 400 the probe outright
	// (model-discovery.ts:486, "max_tokens must be greater than 2").
	keysCustomProbeMaxTokens = 4
)

// The probe and discovery reach a user-supplied URL while the operator watches
// a spinner, so they are bounded well below the 120s custom-provider chat
// timeout (model-discovery.ts:447-448,549-552). They are vars, not consts, so a
// test can shrink them to prove the bound without waiting 30s.
var (
	keysCustomProbeTimeout    = 30 * time.Second
	keysCustomDiscoverTimeout = 30 * time.Second
)

// errKeysCustomRedirect refuses to follow a redirect from a custom endpoint. A
// public base_url that passed the SSRF check could otherwise answer 302 → an
// internal address, and following it would re-request that target without
// re-running any guard (url-guard.ts:26-29). Reported as a probe/discovery
// failure, never followed.
var errKeysCustomRedirect = errors.New("custom endpoint attempted a redirect")

// keysCustomHTTPClient is dedicated to interactive reads of a user-supplied
// endpoint: no redirects, and no client-level timeout because the caller's
// context carries the interactive deadline. It is deliberately NOT the router's
// shared adapter, which follows redirects and would inherit the 120s custom
// chat timeout.
var keysCustomHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return errKeysCustomRedirect },
	Transport: &http.Transport{
		// Control re-runs the SSRF verdict on the address actually dialled, so a
		// hostname that resolved to a public IP at check time but rebinds to an
		// internal one at connect time is refused - closing the assess/connect
		// window and making the allow-unresolvable concession safe.
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil {
					return nil
				}
				switch gateway.ClassifyProviderIP(ip) {
				case "metadata":
					return fmt.Errorf("refusing connection to cloud metadata address %s", host)
				case "link-local":
					return fmt.Errorf("refusing connection to link-local address %s", host)
				case "loopback", "private":
					if gateway.ProviderBlockPrivate() {
						return fmt.Errorf("refusing connection to private address %s", host)
					}
				}
				return nil
			},
		}).DialContext,
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

// keysCustomAssessURL is the HTTP layer's entry to the shared SSRF verdict. The
// policy and address classification live in gateway (AssessProviderURL) so
// KeyVault.Add enforces the identical rule at the storage chokepoint every
// writer passes through.
func keysCustomAssessURL(raw string) (bool, string) {
	return gateway.AssessProviderURL(raw)
}

// ── endpoint HTTP + error plumbing ───────────────────────────────────────────

// keysCustomUpstreamError carries the HTTP status the route should answer with,
// so a relayed 401 stays a 401 while never carrying authentication_error - the
// client ends its session on that exact combination, and testing a bad key
// must not sign the operator out (model-discovery.ts:54-63, api.ts:44-53).
type keysCustomUpstreamError struct {
	status  int
	message string
}

func (e *keysCustomUpstreamError) Error() string { return e.message }

// keysCustomFetch makes one bounded, redirect-refusing request to a custom
// endpoint and reads the whole (capped) body. The caller's context carries the
// deadline.
func keysCustomFetch(ctx context.Context, method, endpoint, apiKey string, body []byte) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := keysCustomHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, keysCustomMaxCatalogBytes))
	return resp.StatusCode, b, err
}

func keysCustomIsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// keysCustomUpstreamMessage pulls the provider's own error text out of a body,
// capped, so a chatty relay cannot smuggle a wall of text into a dashboard row.
func keysCustomUpstreamMessage(body []byte) string {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	msg := strings.TrimSpace(env.Message)
	if msg == "" && len(env.Error) > 0 {
		var s string
		if json.Unmarshal(env.Error, &s) == nil {
			msg = strings.TrimSpace(s)
		} else {
			var eo struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &eo) == nil {
				msg = strings.TrimSpace(eo.Message)
			}
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(env.Detail)
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}

// ── endpoint reference + credential pooling ──────────────────────────────────

// keysCustomEndpointRef is the endpoint a { keyId?, baseUrl? } request names.
type keysCustomEndpointRef struct {
	baseURL   string // normalized; the endpoint identity
	keyID     *int64 // an api_keys row of the pool, or nil when never registered
	storedKey string // that row's plaintext, when decryptable
}

// resolveKeysCustomEndpointRef turns a { keyId?, baseUrl? } reference into the
// endpoint it names (keys.ts:676-712). A keyId is the stronger reference; a bare
// baseUrl falls back to the endpoint's first stored credential.
func (s *Server) resolveKeysCustomEndpointRef(ctx context.Context, keyID *int64, baseURL string) (keysCustomEndpointRef, int, error) {
	var ref keysCustomEndpointRef
	reqBase := ""
	if strings.TrimSpace(baseURL) != "" {
		reqBase = normalizeBaseURL(baseURL)
	}

	if keyID != nil {
		var (
			platform string
			bu       sql.NullString
		)
		err := s.engine.DB().QueryRowContext(ctx,
			`SELECT platform, base_url FROM api_keys WHERE id = ?`, *keyID).Scan(&platform, &bu)
		if errors.Is(err, sql.ErrNoRows) {
			return ref, http.StatusBadRequest, errors.New("keyId does not name a custom endpoint")
		}
		if err != nil {
			return ref, http.StatusInternalServerError, err
		}
		if platform != "custom" || !bu.Valid || bu.String == "" {
			return ref, http.StatusBadRequest, errors.New("keyId does not name a custom endpoint")
		}
		if reqBase != "" && reqBase != bu.String {
			return ref, http.StatusBadRequest, errors.New("baseUrl does not match the endpoint keyId belongs to")
		}
		stored, _ := s.engine.Vault().Reveal(ctx, *keyID)
		return keysCustomEndpointRef{baseURL: bu.String, keyID: keyID, storedKey: stored}, http.StatusOK, nil
	}

	if reqBase == "" {
		return ref, http.StatusBadRequest, errors.New("baseUrl or keyId is required")
	}
	ids, err := s.keysCustomEndpointKeyIDsByBase(ctx, reqBase)
	if err != nil {
		return ref, http.StatusInternalServerError, err
	}
	for _, id := range ids {
		if sec, err := s.engine.Vault().Reveal(ctx, id); err == nil {
			keyIDCopy := id
			return keysCustomEndpointRef{baseURL: reqBase, keyID: &keyIDCopy, storedKey: sec}, http.StatusOK, nil
		}
	}
	if len(ids) > 0 {
		keyIDCopy := ids[0]
		return keysCustomEndpointRef{baseURL: reqBase, keyID: &keyIDCopy}, http.StatusOK, nil
	}
	return keysCustomEndpointRef{baseURL: reqBase}, http.StatusOK, nil
}

// keysCustomEndpointKeyIDsByBase lists every api_keys id serving one base_url.
func (s *Server) keysCustomEndpointKeyIDsByBase(ctx context.Context, baseURL string) ([]int64, error) {
	rows, err := s.engine.DB().QueryContext(ctx,
		`SELECT id FROM api_keys WHERE platform = 'custom' AND base_url = ? ORDER BY id`, baseURL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// keysCustomEndpointKeyIDs is the credential pool a model bound to keyID may
// rotate across: every key sharing its base_url (#619). Falls back to the key
// itself when the row is gone or carries no base_url.
func (s *Server) keysCustomEndpointKeyIDs(ctx context.Context, keyID int64) ([]int64, error) {
	var bu sql.NullString
	err := s.engine.DB().QueryRowContext(ctx, `SELECT base_url FROM api_keys WHERE id = ?`, keyID).Scan(&bu)
	if err != nil || !bu.Valid || bu.String == "" {
		return []int64{keyID}, nil
	}
	ids, err := s.keysCustomEndpointKeyIDsByBase(ctx, bu.String)
	if err != nil || len(ids) == 0 {
		return []int64{keyID}, nil
	}
	return ids, nil
}

func keysCustomDefaultLabel(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "Custom"
}

func keysCustomLabelOr(label, baseURL string) string {
	if l := strings.TrimSpace(label); l != "" {
		return l
	}
	return keysCustomDefaultLabel(baseURL)
}

func (s *Server) keysCustomTouchKey(ctx context.Context, id int64, label string) {
	_, _ = s.engine.DB().ExecContext(ctx,
		`UPDATE api_keys SET label = COALESCE(NULLIF(?, ''), label), status = 'unknown', enabled = 1 WHERE id = ?`,
		strings.TrimSpace(label), id)
}

// keysCustomEndpointHasCredential reports whether an endpoint already stores an
// exact secret, so a caller can tell a genuinely new credential from a
// re-submit of one already in the pool (custom-endpoint.ts:140-142).
func (s *Server) keysCustomEndpointHasCredential(ctx context.Context, baseURL, secret string) bool {
	ids, err := s.keysCustomEndpointKeyIDsByBase(ctx, normalizeBaseURL(baseURL))
	if err != nil {
		return false
	}
	for _, id := range ids {
		if sec, err := s.engine.Vault().Reveal(ctx, id); err == nil && sec == secret {
			return true
		}
	}
	return false
}

// resolveKeysCustomEndpointKey resolves the api_keys row a custom-endpoint
// registration binds to, creating or updating it as needed and never destroying
// a stored credential (custom-endpoint.ts:80-135). Base URLs are always stored
// normalized, so two spellings of one endpoint pool together.
func (s *Server) resolveKeysCustomEndpointKey(ctx context.Context, baseURL, providedKey, label string, pinnedKeyID *int64) (int64, bool, error) {
	scope := normalizeBaseURL(baseURL)
	ids, err := s.keysCustomEndpointKeyIDsByBase(ctx, scope)
	if err != nil {
		return 0, false, err
	}
	type stored struct {
		id     int64
		secret string
	}
	pool := make([]stored, 0, len(ids))
	for _, id := range ids {
		sec, err := s.engine.Vault().Reveal(ctx, id)
		if err != nil {
			sec = "" // an undecryptable row still names the endpoint.
		}
		pool = append(pool, stored{id, sec})
	}

	provided := strings.TrimSpace(providedKey)
	if provided == "" {
		if pinnedKeyID != nil {
			for _, p := range pool {
				if p.id == *pinnedKeyID {
					s.keysCustomTouchKey(ctx, p.id, label)
					return p.id, false, nil
				}
			}
		}
		if len(pool) > 0 {
			s.keysCustomTouchKey(ctx, pool[0].id, label)
			return pool[0].id, false, nil
		}
		id, err := s.engine.Vault().Add("custom", keysCustomNoKey,
			gateway.AddOptions{Label: keysCustomLabelOr(label, scope), BaseURL: scope})
		return id, true, err
	}

	for _, p := range pool {
		if p.secret == provided {
			s.keysCustomTouchKey(ctx, p.id, label)
			return p.id, false, nil
		}
	}

	// Only placeholders on record: the endpoint was registered without auth and
	// is being given a key now. Go's vault has no in-place re-encrypt, so add
	// the real credential, re-home any models bound to the sentinels onto it,
	// and drop the dead sentinels - the same end state as the reference's
	// in-place upgrade (custom-endpoint.ts:120-132).
	if len(pool) > 0 {
		allNoKey := true
		for _, p := range pool {
			if p.secret != keysCustomNoKey {
				allNoKey = false
				break
			}
		}
		if allNoKey {
			id, err := s.engine.Vault().Add("custom", provided,
				gateway.AddOptions{Label: keysCustomLabelOr(label, scope), BaseURL: scope})
			if err != nil {
				return 0, false, err
			}
			for _, p := range pool {
				if _, err := s.engine.DB().ExecContext(ctx,
					`UPDATE models SET key_id = ? WHERE key_id = ?`, id, p.id); err != nil {
					return 0, false, err
				}
				if err := s.engine.Vault().Delete(ctx, p.id); err != nil {
					return 0, false, err
				}
			}
			return id, false, nil
		}
	}

	// A new credential for an endpoint that already has one: pool it (#619).
	id, err := s.engine.Vault().Add("custom", provided,
		gateway.AddOptions{Label: keysCustomLabelOr(label, scope), BaseURL: scope})
	return id, true, err
}

// ── model registration (chat only) ───────────────────────────────────────────
//
// The Go schema carries only chat models (there are no embedding/media tables
// in this slice - those are served by /api/embeddings/custom and
// /api/media/custom), so every model registered through the keys surface is a
// chat row. The dashboard already routes embedding/media adds to those other
// endpoints, so this path never receives them.

type keysCustomModelEntry struct {
	modelID     string
	displayName *string
	tools       *bool
	vision      *bool
}

type keysCustomRegisteredModel struct {
	ModelDBID      int64  `json:"modelDbId"`
	Model          string `json:"model"`
	DisplayName    string `json:"displayName"`
	SupportsTools  bool   `json:"supportsTools"`
	SupportsVision bool   `json:"supportsVision"`
	Created        bool   `json:"created"`
}

func keysCustomNullBoolInt(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

func keysCustomBoolIntDefault(b *bool, def int) int {
	if b == nil {
		return def
	}
	if *b {
		return 1
	}
	return 0
}

// keysCustomModelSeed starts a new custom model at the catalog median rather
// than the floor, so a routing strategy explores it instead of burying it at
// intelligence 0 (custom-model-seed.ts). Approximated against the operator's
// own non-custom rows; the exact tier median lives in Slice A's scoring and is
// not exposed here.
func (s *Server) keysCustomModelSeed(ctx context.Context) (size string, intelligence, speed int) {
	rows, err := s.engine.DB().QueryContext(ctx,
		`SELECT size_label, intelligence_rank, speed_rank FROM models WHERE platform != 'custom'`)
	if err != nil {
		return "Medium", 50, 50
	}
	defer rows.Close()
	type rank struct {
		intel, speed int
		size         string
	}
	var all []rank
	var speeds []int
	for rows.Next() {
		var r rank
		if rows.Scan(&r.size, &r.intel, &r.speed) == nil {
			all = append(all, r)
			speeds = append(speeds, r.speed)
		}
	}
	if len(all) == 0 {
		return "Medium", 50, 50
	}
	sort.Slice(all, func(i, j int) bool { return all[i].intel < all[j].intel })
	sort.Ints(speeds)
	mid := all[(len(all)-1)/2]
	size = mid.size
	if strings.TrimSpace(size) == "" {
		size = "Medium"
	}
	return size, mid.intel, speeds[(len(speeds)-1)/2]
}

// registerKeysCustomChatModels upserts each entry against the endpoint and
// returns what changed. It NEVER deletes-and-reinserts: fallback_config and
// profile_models address a model by models.id, so a re-register updates the row
// in place and the id is stable across imports (spec-persistence §7.4 trap 5).
func (s *Server) registerKeysCustomChatModels(ctx context.Context, baseURL string, keyID int64, entries []keysCustomModelEntry) ([]keysCustomRegisteredModel, error) {
	scope := normalizeBaseURL(baseURL)
	poolIDs, err := s.keysCustomEndpointKeyIDs(ctx, keyID)
	if err != nil {
		return nil, err
	}
	pool := make(map[int64]bool, len(poolIDs))
	for _, id := range poolIDs {
		pool[id] = true
	}
	seedSize, seedIntel, seedSpeed := s.keysCustomModelSeed(ctx)

	tx, err := s.engine.DB().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	out := make([]keysCustomRegisteredModel, 0, len(entries))
	for _, e := range entries {
		var (
			existingID int64
			boundKey   sql.NullInt64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, key_id FROM models WHERE platform = 'custom' AND model_id = ? AND endpoint_scope = ?`,
			e.modelID, scope).Scan(&existingID, &boundKey)
		created := errors.Is(err, sql.ErrNoRows)
		if err != nil && !created {
			return nil, err
		}

		bindKey := keyID
		if !created && boundKey.Valid && pool[boundKey.Int64] {
			bindKey = boundKey.Int64
		}

		if created {
			insertName := e.modelID
			if e.displayName != nil {
				insertName = *e.displayName
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO models
				  (platform, model_id, display_name, intelligence_rank, speed_rank, size_label,
				   enabled, key_id, supports_tools, supports_vision, source, endpoint_scope)
				VALUES ('custom', ?, ?, ?, ?, ?, 1, ?, ?, ?, 'user', ?)`,
				e.modelID, insertName, seedIntel, seedSpeed, seedSize,
				bindKey, keysCustomBoolIntDefault(e.tools, 1), keysCustomBoolIntDefault(e.vision, 0), scope)
			if err != nil {
				return nil, err
			}
			if existingID, err = res.LastInsertId(); err != nil {
				return nil, err
			}
		} else {
			// COALESCE keeps a hand-set name/flag when this submit omitted it.
			var updateName any
			if e.displayName != nil {
				updateName = *e.displayName
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE models
				   SET display_name = COALESCE(?, display_name),
				       key_id = ?,
				       enabled = 1,
				       supports_tools = COALESCE(?, supports_tools),
				       supports_vision = COALESCE(?, supports_vision)
				 WHERE id = ?`,
				updateName, bindKey, keysCustomNullBoolInt(e.tools), keysCustomNullBoolInt(e.vision), existingID); err != nil {
				return nil, err
			}
		}

		// Make the model routable: append to the global chain and every profile
		// it is not already in (custom-model-register.ts:140-146).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fallback_config (model_db_id, position, enabled)
			VALUES (?, (SELECT COALESCE(MAX(position), 0) + 1 FROM fallback_config), 1)
			ON CONFLICT(model_db_id) DO NOTHING`, existingID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO profile_models (profile_id, model_db_id, position)
			SELECT p.id, ?, COALESCE((SELECT MAX(position) + 1 FROM profile_models pm2 WHERE pm2.profile_id = p.id), 1)
			  FROM profiles p
			 WHERE NOT EXISTS (SELECT 1 FROM profile_models pm WHERE pm.profile_id = p.id AND pm.model_db_id = ?)`,
			existingID, existingID); err != nil {
			return nil, err
		}

		var (
			name          string
			tools, vision int
		)
		if err := tx.QueryRowContext(ctx,
			`SELECT display_name, supports_tools, supports_vision FROM models WHERE id = ?`, existingID).
			Scan(&name, &tools, &vision); err != nil {
			return nil, err
		}
		out = append(out, keysCustomRegisteredModel{
			ModelDBID: existingID, Model: e.modelID, DisplayName: name,
			SupportsTools: tools == 1, SupportsVision: vision == 1, Created: created,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ── POST /api/keys/custom ────────────────────────────────────────────────────

func (s *Server) handleKeysCustomAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL        string            `json:"baseUrl"`
		KeyID          *int64            `json:"keyId"`
		Model          string            `json:"model"`
		Models         []json.RawMessage `json:"models"`
		DisplayName    string            `json:"displayName"`
		APIKey         string            `json:"apiKey"`
		Label          string            `json:"label"`
		SupportsTools  *bool             `json:"supportsTools"`
		SupportsVision *bool             `json:"supportsVision"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" && req.KeyID == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl or keyId is required")
		return
	}

	ctx := r.Context()
	ref, status, err := s.resolveKeysCustomEndpointRef(ctx, req.KeyID, req.BaseURL)
	if err != nil {
		WriteError(w, status, TypeInvalidRequest, err.Error())
		return
	}
	if ok, reason := keysCustomAssessURL(ref.baseURL); !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl rejected: "+reason)
		return
	}

	entries := s.keysCustomBuildEntries(req.Model, req.Models, req.DisplayName, req.SupportsTools, req.SupportsVision)
	if len(entries) > keysCustomMaxImportKeys {
		entries = entries[:keysCustomMaxImportKeys]
	}
	providedKey := strings.TrimSpace(req.APIKey)

	if len(entries) == 0 {
		// Credential-only add: a second key for an endpoint already registered
		// (#702). It needs the endpoint to exist and a key to actually add.
		if providedKey == "" {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "model or models is required")
			return
		}
		if ref.keyID == nil {
			WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "model or models is required to register a new endpoint")
			return
		}
		if s.keysCustomEndpointHasCredential(ctx, ref.baseURL, providedKey) {
			WriteError(w, http.StatusConflict, TypeInvalidRequest, "this endpoint already has that key")
			return
		}
		keyID, created, err := s.resolveKeysCustomEndpointKey(ctx, ref.baseURL, providedKey, req.Label, ref.keyID)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, TypeServer, gateway.RedactWith("could not add key: "+err.Error(), providedKey))
			return
		}
		out := http.StatusOK
		if created {
			out = http.StatusCreated
		}
		WriteJSON(w, out, map[string]any{
			"success":           true,
			"keyId":             keyID,
			"platform":          "custom",
			"baseUrl":           ref.baseURL,
			"models":            []keysCustomRegisteredModel{},
			"created":           0,
			"alreadyRegistered": 0,
			"maskedKey":         s.keysCustomMasked(ctx, keyID),
		})
		return
	}

	keyID, _, err := s.resolveKeysCustomEndpointKey(ctx, ref.baseURL, providedKey, req.Label, ref.keyID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, gateway.RedactWith("could not register endpoint: "+err.Error(), providedKey))
		return
	}
	registered, err := s.registerKeysCustomChatModels(ctx, ref.baseURL, keyID, entries)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, gateway.RedactWith("could not register models: "+err.Error(), providedKey))
		return
	}
	first := registered[0]

	createdCount, alreadyCount := 0, 0
	for _, m := range registered {
		if m.Created {
			createdCount++
		} else {
			alreadyCount++
		}
	}
	body := map[string]any{
		"success":           true,
		"keyId":             keyID,
		"platform":          "custom",
		"baseUrl":           ref.baseURL,
		"models":            registered,
		"media":             []any{},
		"embeddings":        []any{},
		"videoSkipped":      []string{},
		"modelErrors":       []any{},
		"created":           createdCount,
		"alreadyRegistered": alreadyCount,
		"maskedKey":         s.keysCustomMasked(ctx, keyID),
	}
	body["modelDbId"] = first.ModelDBID
	body["model"] = first.Model
	body["displayName"] = first.DisplayName
	body["supportsTools"] = first.SupportsTools
	body["supportsVision"] = first.SupportsVision
	WriteJSON(w, http.StatusCreated, body)
}

// keysCustomBuildEntries flattens the singular model + plural models inputs into
// one deduped list, dropping blanks and over-long ids, resolving each entry's
// capability flags against the submit-level defaults, and applying the sole
// display name only when it can name exactly one otherwise-unnamed model
// (keys.ts:1001-1037).
func (s *Server) keysCustomBuildEntries(model string, models []json.RawMessage, displayName string, topTools, topVision *bool) []keysCustomModelEntry {
	var entries []keysCustomModelEntry
	seen := map[string]bool{}
	add := func(rawID string, disp *string, tools, vision *bool) {
		id := strings.TrimSpace(rawID)
		if id == "" || len(id) > keysCustomMaxModelIDLen || seen[id] {
			return
		}
		seen[id] = true
		if tools == nil {
			tools = topTools
		}
		if vision == nil {
			vision = topVision
		}
		var d *string
		if disp != nil {
			if dd := strings.TrimSpace(*disp); dd != "" {
				d = &dd
			}
		}
		entries = append(entries, keysCustomModelEntry{modelID: id, displayName: d, tools: tools, vision: vision})
	}

	if strings.TrimSpace(model) != "" {
		add(model, &displayName, nil, nil)
	}
	for _, raw := range models {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			add(str, nil, nil, nil)
			continue
		}
		var obj struct {
			Model          string  `json:"model"`
			DisplayName    *string `json:"displayName"`
			SupportsTools  *bool   `json:"supportsTools"`
			SupportsVision *bool   `json:"supportsVision"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			add(obj.Model, obj.DisplayName, obj.SupportsTools, obj.SupportsVision)
		}
	}

	sole := strings.TrimSpace(displayName)
	if sole != "" && strings.TrimSpace(model) == "" && len(entries) == 1 && entries[0].displayName == nil {
		entries[0].displayName = &sole
	}
	return entries
}

func (s *Server) keysCustomMasked(ctx context.Context, keyID int64) string {
	if row, ok, err := s.engine.Vault().Get(ctx, keyID); err == nil && ok {
		return row.Masked
	}
	return ""
}

// ── POST /api/keys/custom/discover-models ────────────────────────────────────

type keysCustomDiscoveredModel struct {
	ID            string  `json:"id"`
	OwnedBy       *string `json:"ownedBy"`
	Registered    bool    `json:"registered"`
	ContextWindow *int    `json:"contextWindow,omitempty"`
	PriceNote     *string `json:"priceNote,omitempty"`
	IsFree        *bool   `json:"isFree,omitempty"`
	Vision        *bool   `json:"vision,omitempty"`
	Kind          *string `json:"kind,omitempty"`
}

func (s *Server) handleKeysCustomDiscover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"baseUrl"`
		KeyID   *int64 `json:"keyId"`
		APIKey  string `json:"apiKey"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" && req.KeyID == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl or keyId is required")
		return
	}
	ctx := r.Context()
	ref, status, err := s.resolveKeysCustomEndpointRef(ctx, req.KeyID, req.BaseURL)
	if err != nil {
		WriteError(w, status, TypeInvalidRequest, err.Error())
		return
	}
	if ok, reason := keysCustomAssessURL(ref.baseURL); !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl rejected: "+reason)
		return
	}

	apiKey := keysCustomFirstNonEmpty(strings.TrimSpace(req.APIKey), ref.storedKey, keysCustomNoKey)
	dctx, cancel := context.WithTimeout(ctx, keysCustomDiscoverTimeout)
	defer cancel()

	models, uerr, ferr := s.keysCustomDiscover(dctx, ref.baseURL, apiKey)
	if uerr != nil {
		WriteError(w, uerr.status, TypeUpstream, gateway.RedactWith(uerr.message, apiKey))
		return
	}
	if ferr != nil {
		WriteError(w, http.StatusBadGateway, TypeUpstream,
			gateway.RedactWith("Model discovery failed: "+ferr.Error(), apiKey))
		return
	}

	registeredIDs := map[string]bool{}
	if ref.keyID != nil {
		if pool, err := s.keysCustomEndpointKeyIDs(ctx, *ref.keyID); err == nil && len(pool) > 0 {
			registeredIDs = s.keysCustomRegisteredIDs(ctx, pool)
		}
	}
	registeredCount := 0
	for i := range models {
		if registeredIDs[models[i].ID] {
			models[i].Registered = true
			registeredCount++
		}
	}

	var keyIDVal any
	if ref.keyID != nil {
		keyIDVal = *ref.keyID
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"baseUrl":         ref.baseURL,
		"keyId":           keyIDVal,
		"models":          models,
		"total":           len(models),
		"registeredCount": registeredCount,
	})
}

func (s *Server) keysCustomRegisteredIDs(ctx context.Context, pool []int64) map[string]bool {
	out := map[string]bool{}
	placeholders := strings.Repeat("?,", len(pool))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(pool))
	for i, id := range pool {
		args[i] = id
	}
	rows, err := s.engine.DB().QueryContext(ctx,
		`SELECT model_id FROM models WHERE platform = 'custom' AND key_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// keysCustomDiscover asks a custom endpoint what it serves. It treats the
// upstream as hostile-by-accident: cap the body, cap the list, accept the model
// envelopes seen in the wild, and turn any surprise into a clean upstream error
// rather than a 500 (model-discovery.ts:441-482).
func (s *Server) keysCustomDiscover(ctx context.Context, baseURL, apiKey string) ([]keysCustomDiscoveredModel, *keysCustomUpstreamError, error) {
	endpoint := normalizeBaseURL(baseURL) + "/models"
	statusCode, body, err := keysCustomFetch(ctx, http.MethodGet, endpoint, apiKey, nil)
	if err != nil {
		reason := err.Error()
		if keysCustomIsTimeout(err) {
			reason = "timed out"
		}
		return nil, &keysCustomUpstreamError{http.StatusBadGateway, fmt.Sprintf("Could not reach %s/models: %s", baseURL, reason)}, nil
	}
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		msg := fmt.Sprintf("The endpoint rejected the key (HTTP %d)", statusCode)
		if d := keysCustomUpstreamMessage(body); d != "" {
			msg += ": " + d
		}
		return nil, &keysCustomUpstreamError{http.StatusUnauthorized, msg}, nil
	}
	if statusCode < 200 || statusCode >= 300 {
		msg := fmt.Sprintf("%s/models returned HTTP %d", baseURL, statusCode)
		if d := keysCustomUpstreamMessage(body); d != "" {
			msg += ": " + d
		}
		return nil, &keysCustomUpstreamError{http.StatusBadGateway, msg}, nil
	}

	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return nil, &keysCustomUpstreamError{http.StatusBadGateway, fmt.Sprintf("%s/models did not return a model list (response was not JSON).", baseURL)}, nil
	}
	arr, ok := keysCustomFindModelArray(payload, 0)
	if !ok {
		return nil, &keysCustomUpstreamError{http.StatusBadGateway, fmt.Sprintf("%s/models did not return a model list in a format this gateway understands.", baseURL)}, nil
	}
	return keysCustomParseCatalog(arr), nil, nil
}

// keysCustomFindModelArray descends at most one nesting level to find the model
// array inside whatever envelope this relay chose (model-discovery.ts:65-121).
func keysCustomFindModelArray(v any, depth int) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case map[string]any:
		for _, k := range []string{"data", "models", "result", "results", "items"} {
			inner, ok := t[k]
			if !ok {
				continue
			}
			if arr, ok := inner.([]any); ok {
				return arr, true
			}
			if depth < 1 {
				if arr, ok := keysCustomFindModelArray(inner, depth+1); ok {
					return arr, true
				}
			}
		}
	}
	return nil, false
}

var (
	keysCustomIDKeys      = []string{"id", "name", "model", "model_id", "modelId", "slug"}
	keysCustomOwnerKeys   = []string{"owned_by", "ownedBy", "organization", "owner", "provider", "publisher"}
	keysCustomContextKeys = []string{"context_length", "context_window", "max_model_len", "max_context_length", "max_context_tokens", "ctx_len", "contextWindow"}
	keysCustomVisionKeys  = []string{"vision", "supports_vision", "supportsVision", "image_input", "multimodal"}
)

func keysCustomParseCatalog(arr []any) []keysCustomDiscoveredModel {
	seen := map[string]bool{}
	out := make([]keysCustomDiscoveredModel, 0, len(arr))
	for _, e := range arr {
		var (
			id  string
			rec map[string]any
		)
		switch t := e.(type) {
		case string:
			id = strings.TrimSpace(t)
		case map[string]any:
			rec = t
			id = keysCustomFirstString(t, keysCustomIDKeys)
		}
		if id == "" || len(id) > keysCustomMaxModelIDLen || seen[id] {
			continue
		}
		seen[id] = true

		dm := keysCustomDiscoveredModel{ID: id}
		if rec != nil {
			if ob := keysCustomFirstString(rec, keysCustomOwnerKeys); ob != "" {
				dm.OwnedBy = &ob
			}
			if cw, ok := keysCustomFirstNumber(rec, keysCustomContextKeys); ok && cw > 0 {
				c := int(cw)
				dm.ContextWindow = &c
			}
			if keysCustomVision(rec, id) {
				b := true
				dm.Vision = &b
			}
			if note, isFree, ok := keysCustomPriceHint(rec); ok {
				dm.PriceNote = &note
				f := isFree
				dm.IsFree = &f
			}
		} else if keysCustomVisionFromID(id) {
			b := true
			dm.Vision = &b
		}
		if k := keysCustomClassifyModelID(id); k != "" {
			dm.Kind = &k
		}
		out = append(out, dm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > keysCustomMaxDiscovered {
		out = out[:keysCustomMaxDiscovered]
	}
	return out
}

func keysCustomFirstString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func keysCustomFirstNumber(m map[string]any, keys []string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return t, true
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func keysCustomVision(rec map[string]any, id string) bool {
	for _, k := range keysCustomVisionKeys {
		if b, ok := rec[k].(bool); ok && b {
			return true
		}
	}
	for _, k := range []string{"modalities", "input_modalities", "inputModalities", "modality"} {
		switch t := rec[k].(type) {
		case []any:
			for _, m := range t {
				if s, ok := m.(string); ok && keysCustomIsVisionModality(s) {
					return true
				}
			}
		case string:
			if keysCustomIsVisionModality(t) {
				return true
			}
		}
	}
	return keysCustomVisionFromID(id)
}

func keysCustomIsVisionModality(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "image") || strings.Contains(s, "vision")
}

var keysCustomVisionIDMarkers = []string{"llava", "internvl", "pixtral", "moondream", "cogvlm", "vision"}

// keysCustomVisionFromID reads vision off the model id for upstreams that ship
// no modality metadata (#1051). Matched per token so a relay shipping `vllm`
// cannot read as `vl`.
func keysCustomVisionFromID(id string) bool {
	tokens := keysCustomTokens(id)
	for _, tok := range tokens {
		for _, marker := range keysCustomVisionIDMarkers {
			if tok == marker {
				return true
			}
		}
	}
	return false
}

func keysCustomTokens(id string) []string {
	return strings.FieldsFunc(strings.ToLower(id), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// keysCustomClassifyModelID names what a model discernibly IS when it is not a
// chat model, off its id alone, for upstreams with no type metadata (#1051).
// Conservative: returns "" (chat) unless a marker is unambiguous, since a wrong
// media verdict makes a working chat model unreachable. Matched per token.
func keysCustomClassifyModelID(id string) string {
	tokens := keysCustomTokens(id)
	has := func(want ...string) bool {
		for _, tok := range tokens {
			for _, w := range want {
				if tok == w {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("embedding", "embeddings", "embed"):
		return "embedding"
	case has("whisper", "transcribe", "transcription", "stt"):
		return "transcription"
	case has("tts"):
		return "audio"
	case has("dalle", "flux", "sdxl", "imagen", "midjourney"):
		return "image"
	case has("sora", "veo", "kling", "video"):
		return "video"
	}
	return ""
}

// keysCustomPriceHint renders a compact price chip from the two shapes worth
// supporting: an OpenRouter-style pricing object and a plain price string. Any
// other shape leaves the chip absent, which the picker renders as nothing -
// honest, since the upstream gave no usable figure.
func keysCustomPriceHint(rec map[string]any) (string, bool, bool) {
	if pm, ok := rec["pricing"].(map[string]any); ok {
		in := keysCustomPerMillion(pm["prompt"])
		out := keysCustomPerMillion(pm["completion"])
		if in != nil || out != nil {
			free := (in == nil || *in == 0) && (out == nil || *out == 0)
			if free {
				return "free", true, true
			}
			var parts []string
			if in != nil {
				parts = append(parts, fmt.Sprintf("$%s/M in", keysCustomTrimUSD(*in)))
			}
			if out != nil {
				parts = append(parts, fmt.Sprintf("$%s/M out", keysCustomTrimUSD(*out)))
			}
			note := strings.Join(parts, " ")
			if len(note) > 40 {
				note = note[:40]
			}
			return note, false, true
		}
	}
	for _, k := range []string{"price", "pricing"} {
		if s, ok := rec[k].(string); ok && strings.TrimSpace(s) != "" {
			note := strings.TrimSpace(s)
			if len(note) > 40 {
				note = note[:40]
			}
			return note, strings.EqualFold(note, "free"), true
		}
	}
	return "", false, false
}

// keysCustomPerMillion returns a price component in USD per million tokens.
// Below a cent the figure can only be per-token (OpenRouter quotes
// "0.00000125"); at or above it the relay already quoted per million
// (model-discovery.ts:96-98).
func keysCustomPerMillion(raw any) *float64 {
	var v float64
	switch t := raw.(type) {
	case float64:
		v = t
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil
		}
		v = f
	default:
		return nil
	}
	if v < 0 {
		return nil
	}
	if v > 0 && v < 0.01 {
		v *= 1_000_000
	}
	return &v
}

func keysCustomTrimUSD(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ── POST /api/keys/custom/probe ──────────────────────────────────────────────

type keysCustomProbeResult struct {
	modelID      string
	latencyMs    int64
	inputTokens  int64
	outputTokens int64
	usageKnown   bool
}

func (s *Server) handleKeysCustomProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"baseUrl"`
		KeyID   *int64 `json:"keyId"`
		APIKey  string `json:"apiKey"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.BaseURL) == "" && req.KeyID == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl or keyId is required")
		return
	}
	ctx := r.Context()
	ref, status, err := s.resolveKeysCustomEndpointRef(ctx, req.KeyID, req.BaseURL)
	if err != nil {
		WriteError(w, status, TypeInvalidRequest, err.Error())
		return
	}
	if ok, reason := keysCustomAssessURL(ref.baseURL); !ok {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "baseUrl rejected: "+reason)
		return
	}

	apiKey := keysCustomFirstNonEmpty(strings.TrimSpace(req.APIKey), ref.storedKey, keysCustomNoKey)

	// Probe a model actually registered on this endpoint when there is one - the
	// sample must feed a model the router can pick, not whatever id leads the
	// upstream list (keys.ts:905-923).
	modelID := ""
	if ref.keyID != nil {
		if pool, err := s.keysCustomEndpointKeyIDs(ctx, *ref.keyID); err == nil {
			modelID = s.keysCustomFirstRegisteredModel(ctx, pool)
		}
	}

	pctx, cancel := context.WithTimeout(ctx, keysCustomProbeTimeout)
	defer cancel()

	if modelID == "" {
		models, uerr, ferr := s.keysCustomDiscover(pctx, ref.baseURL, apiKey)
		if uerr != nil {
			WriteError(w, uerr.status, TypeUpstream, gateway.RedactWith(uerr.message, apiKey))
			return
		}
		if ferr != nil {
			WriteError(w, http.StatusBadGateway, TypeUpstream, gateway.RedactWith("Probe failed: "+ferr.Error(), apiKey))
			return
		}
		if len(models) == 0 {
			WriteError(w, http.StatusBadGateway, TypeUpstream, "The endpoint returned no models to probe.")
			return
		}
		// Discovery is sorted by id and may mix modalities: an embedding or
		// media model can lead the list yet cannot answer /chat/completions.
		// Probe the first chat-compatible id instead of whatever sorts first,
		// and when the endpoint serves no chat model say so honestly rather
		// than firing a chat probe at a non-chat endpoint.
		modelID = keysCustomFirstChatModel(models)
		if modelID == "" {
			WriteError(w, http.StatusBadGateway, TypeUpstream,
				"The endpoint served no chat model to probe; every discovered model is a non-chat (embedding or media) model.")
			return
		}
	}

	result, uerr, ferr := s.keysCustomProbeModel(pctx, ref.baseURL, apiKey, modelID)
	if uerr != nil {
		WriteError(w, uerr.status, TypeUpstream, gateway.RedactWith(uerr.message, apiKey))
		return
	}
	if ferr != nil {
		reason := ferr.Error()
		if keysCustomIsTimeout(ferr) {
			reason = "timed out"
		}
		WriteError(w, http.StatusBadGateway, TypeUpstream,
			gateway.RedactWith(fmt.Sprintf("Probe request to %s failed: %s", modelID, reason), apiKey))
		return
	}

	// Only a successful probe records a sample and lifts a bench: a real
	// completion is stronger evidence than any health ping (keys.ts:928-939).
	if ref.keyID != nil {
		kid := *ref.keyID
		_, _ = s.RecordRequest(ctx, RequestLog{
			Platform:      "custom",
			ModelID:       result.modelID,
			EndpointScope: normalizeBaseURL(ref.baseURL),
			KeyID:         &kid,
			Outcome:       "success",
			StatusCode:    http.StatusOK,
			InputTokens:   result.inputTokens,
			OutputTokens:  result.outputTokens,
			UsageKnown:    result.usageKnown,
			LatencyMs:     result.latencyMs,
			TTFBMs:        &result.latencyMs,
			Attempts:      1,
			Class:         "chat",
		})
		s.engine.Cooldowns().Clear(gateway.QuotaKey("custom", result.modelID, kid))
	}

	// Best-effort capability snapshot on the same click; a probe that errors
	// simply leaves the flag unset (keys.ts:941-965).
	reasoning, toolCalls := s.keysCustomProbeCapabilities(pctx, ref.baseURL, apiKey, result.modelID)
	resp := map[string]any{"modelId": result.modelID, "latencyMs": result.latencyMs}
	if reasoning != nil {
		resp["reasoning"] = *reasoning
	}
	if toolCalls != nil {
		resp["toolCalls"] = *toolCalls
	}
	WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) keysCustomFirstRegisteredModel(ctx context.Context, pool []int64) string {
	if len(pool) == 0 {
		return ""
	}
	placeholders := strings.Repeat("?,", len(pool))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(pool))
	for i, id := range pool {
		args[i] = id
	}
	var modelID string
	err := s.engine.DB().QueryRowContext(ctx,
		`SELECT model_id FROM models WHERE platform = 'custom' AND key_id IN (`+placeholders+`) ORDER BY id LIMIT 1`,
		args...).Scan(&modelID)
	if err != nil {
		return ""
	}
	return modelID
}

// keysCustomFirstChatModel returns the id of the first chat-compatible model in
// a discovered catalog - one keysCustomClassifyModelID could not tag as a
// non-chat (embedding/media) model, so its Kind is unset. Discovery is sorted
// by id, so an embedding or media id can lead the list; the /chat/completions
// probe must skip past those. Returns "" when every model is non-chat.
func keysCustomFirstChatModel(models []keysCustomDiscoveredModel) string {
	for i := range models {
		if models[i].Kind == nil {
			return models[i].ID
		}
	}
	return ""
}

type keysCustomChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// keysCustomProbeModel fires one minimal real chat request to measure latency
// and confirm the key works end-to-end. A relayed 401/403 is reported as an
// upstream failure, never authentication_error (keys.ts:967-975).
func (s *Server) keysCustomProbeModel(ctx context.Context, baseURL, apiKey, modelID string) (keysCustomProbeResult, *keysCustomUpstreamError, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      modelID,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": keysCustomProbeMaxTokens,
	})
	start := time.Now()
	statusCode, respBody, err := keysCustomFetch(ctx, http.MethodPost,
		normalizeBaseURL(baseURL)+"/chat/completions", apiKey, body)
	if err != nil {
		return keysCustomProbeResult{}, nil, err
	}
	latency := time.Since(start).Milliseconds()

	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		msg := fmt.Sprintf("The endpoint rejected the key (HTTP %d)", statusCode)
		if d := keysCustomUpstreamMessage(respBody); d != "" {
			msg += ": " + d
		}
		return keysCustomProbeResult{}, &keysCustomUpstreamError{http.StatusUnauthorized, msg}, nil
	}
	if statusCode < 200 || statusCode >= 300 {
		msg := fmt.Sprintf("the endpoint returned HTTP %d", statusCode)
		if d := keysCustomUpstreamMessage(respBody); d != "" {
			msg += ": " + d
		}
		return keysCustomProbeResult{}, &keysCustomUpstreamError{http.StatusBadGateway, msg}, nil
	}

	var cr keysCustomChatResponse
	_ = json.Unmarshal(respBody, &cr)
	modelOut := modelID
	if cr.Model != "" {
		modelOut = cr.Model
	}
	res := keysCustomProbeResult{modelID: modelOut, latencyMs: latency}
	if cr.Usage != nil {
		res.inputTokens = int64(cr.Usage.PromptTokens)
		res.outputTokens = int64(cr.Usage.CompletionTokens)
		res.usageKnown = true
	}
	return res, nil, nil
}

// keysCustomProbeCapabilities runs a deterministic reasoning probe (9*7 → 63)
// and a tool-call probe. Both are best-effort: an error leaves the flag unset,
// and only positive evidence sets it (keys.ts:941-957, model-discovery.ts:
// 599-697).
func (s *Server) keysCustomProbeCapabilities(ctx context.Context, baseURL, apiKey, modelID string) (reasoning, toolCalls *bool) {
	chat := normalizeBaseURL(baseURL) + "/chat/completions"

	reasonBody, _ := json.Marshal(map[string]any{
		"model":      modelID,
		"messages":   []map[string]any{{"role": "user", "content": "What is 9*7? Reply with just the number."}},
		"max_tokens": 16,
	})
	if status, body, err := keysCustomFetch(ctx, http.MethodPost, chat, apiKey, reasonBody); err == nil && status >= 200 && status < 300 {
		var cr keysCustomChatResponse
		if json.Unmarshal(body, &cr) == nil && len(cr.Choices) > 0 {
			ok := strings.Contains(keysCustomContentText(cr.Choices[0].Message.Content), "63")
			reasoning = &ok
		}
	}

	toolBody, _ := json.Marshal(map[string]any{
		"model":    modelID,
		"messages": []map[string]any{{"role": "user", "content": "What is the weather in Paris?"}},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get the current weather for a location.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
		"tool_choice": "auto",
		"max_tokens":  16,
	})
	if status, body, err := keysCustomFetch(ctx, http.MethodPost, chat, apiKey, toolBody); err == nil && status >= 200 && status < 300 {
		var cr keysCustomChatResponse
		if json.Unmarshal(body, &cr) == nil && len(cr.Choices) > 0 {
			called := cr.Choices[0].FinishReason == "tool_calls" || keysCustomHasToolCalls(cr.Choices[0].Message.ToolCalls)
			toolCalls = &called
		}
	}
	return reasoning, toolCalls
}

func keysCustomHasToolCalls(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var arr []any
	return json.Unmarshal(raw, &arr) == nil && len(arr) > 0
}

func keysCustomContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []map[string]any
	if json.Unmarshal(raw, &arr) == nil {
		var b strings.Builder
		for _, part := range arr {
			if t, ok := part["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

func keysCustomFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ── POST /api/keys/preview ───────────────────────────────────────────────────

type keysCustomImportModel struct {
	ID             string `json:"id"`
	SupportsTools  *bool  `json:"supportsTools,omitempty"`
	SupportsVision *bool  `json:"supportsVision,omitempty"`
}

type keysCustomPreviewKey struct {
	KeyName          string                  `json:"keyName"`
	KeyValue         string                  `json:"keyValue"`
	DetectedPlatform *string                 `json:"detectedPlatform"`
	Prefix           string                  `json:"prefix"`
	BaseURL          *string                 `json:"baseUrl,omitempty"`
	Models           []keysCustomImportModel `json:"models,omitempty"`
	IsDuplicate      bool                    `json:"isDuplicate"`
}

func (s *Server) handleKeysImportPreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil || r.MultipartForm == nil {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "No files uploaded")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest, "No files uploaded")
		return
	}

	ctx := r.Context()
	existing := s.keysCustomExistingSecrets(ctx)

	keys := []keysCustomPreviewKey{}
	skipped := []string{}
	duplicates := 0

	for i, fh := range files {
		if i >= keysCustomMaxImportFiles {
			break
		}
		f, err := fh.Open()
		if err != nil {
			continue
		}
		content, _ := io.ReadAll(io.LimitReader(f, 5<<20))
		_ = f.Close()

		result := keysCustomParseFile(string(content), fh.Filename)
		for _, pk := range result.keys {
			name, value := keysCustomSplitRawKey(pk.rawKey)
			isDup := existing[strings.TrimSpace(value)]
			if isDup {
				duplicates++
			}
			out := keysCustomPreviewKey{
				KeyName:          name,
				KeyValue:         value,
				DetectedPlatform: pk.platform,
				Prefix:           pk.prefix,
				IsDuplicate:      isDup,
			}
			if pk.baseURL != "" {
				bu := pk.baseURL
				out.BaseURL = &bu
			}
			if len(pk.models) > 0 {
				out.Models = keysCustomToImportModels(pk.models)
			}
			keys = append(keys, out)
		}
		skipped = append(skipped, result.skipped...)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"keys":       keys,
		"total":      len(keys),
		"skipped":    skipped,
		"duplicates": duplicates,
	})
}

func keysCustomToImportModels(entries []keysCustomParsedModel) []keysCustomImportModel {
	out := make([]keysCustomImportModel, 0, len(entries))
	for _, e := range entries {
		out = append(out, keysCustomImportModel{ID: e.id, SupportsTools: e.tools, SupportsVision: e.vision})
	}
	return out
}

func (s *Server) keysCustomExistingSecrets(ctx context.Context) map[string]bool {
	set := map[string]bool{}
	rows, err := s.engine.Vault().List(ctx)
	if err != nil {
		return set
	}
	for _, row := range rows {
		if sec, err := s.engine.Vault().Reveal(ctx, row.ID); err == nil {
			set[strings.TrimSpace(sec)] = true
		}
	}
	return set
}

// ── POST /api/keys/import-selected ───────────────────────────────────────────

func (s *Server) handleKeysImportSelected(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []struct {
			KeyName  string `json:"keyName"`
			KeyValue string `json:"keyValue"`
			Platform string `json:"platform"`
			BaseURL  string `json:"baseUrl"`
			Models   []struct {
				ID             string `json:"id"`
				SupportsTools  *bool  `json:"supportsTools"`
				SupportsVision *bool  `json:"supportsVision"`
			} `json:"models"`
		} `json:"keys"`
	}
	if !DecodeJSON(w, r, &req) {
		return
	}
	if len(req.Keys) > keysCustomMaxImportKeys {
		WriteError(w, http.StatusBadRequest, TypeInvalidRequest,
			fmt.Sprintf("at most %d keys may be imported at once", keysCustomMaxImportKeys))
		return
	}

	ctx := r.Context()
	existing := s.keysCustomExistingSecrets(ctx)

	imported := 0
	modelsRegistered := 0
	errs := []map[string]string{}
	addErr := func(key, msg string) { errs = append(errs, map[string]string{"key": key, "error": msg}) }

	for _, key := range req.Keys {
		keyName := strings.TrimSpace(key.KeyName)
		if keyName == "" {
			keyName = key.Platform
		}

		if key.Platform == "custom" {
			if strings.TrimSpace(key.BaseURL) == "" {
				addErr(keyName, "Custom providers must be added with a base URL")
				continue
			}
			if ok, reason := keysCustomAssessURL(key.BaseURL); !ok {
				addErr(keyName, "baseUrl rejected: "+reason)
				continue
			}
			// 'no-key' is the auth-less placeholder, not a secret - hand it over
			// as "no key submitted" so the resolver stores the sentinel once
			// (keys.ts:1367-1372).
			secret := strings.TrimSpace(key.KeyValue)
			if secret == keysCustomNoKey {
				secret = ""
			}
			keyID, _, err := s.resolveKeysCustomEndpointKey(ctx, key.BaseURL, secret, keyName, nil)
			if err != nil {
				addErr(keyName, gateway.RedactWith(err.Error(), key.KeyValue))
				continue
			}
			imported++
			if len(key.Models) > 0 {
				entries := make([]keysCustomModelEntry, 0, len(key.Models))
				for _, m := range key.Models {
					id := strings.TrimSpace(m.ID)
					if id == "" || len(id) > keysCustomMaxModelIDLen {
						continue
					}
					entries = append(entries, keysCustomModelEntry{modelID: id, tools: m.SupportsTools, vision: m.SupportsVision})
					if len(entries) >= keysCustomMaxDiscovered {
						break
					}
				}
				if reg, err := s.registerKeysCustomChatModels(ctx, key.BaseURL, keyID, entries); err != nil {
					addErr(keyName, gateway.RedactWith(err.Error(), key.KeyValue))
				} else {
					modelsRegistered += len(reg)
				}
			}
			continue
		}

		if existing[strings.TrimSpace(key.KeyValue)] {
			addErr(keyName, "Duplicate key - already exists")
			continue
		}
		if !s.knownPlatform(key.Platform) {
			addErr(keyName, fmt.Sprintf("unknown platform %q", key.Platform))
			continue
		}
		if strings.TrimSpace(key.KeyValue) == "" {
			addErr(keyName, "keyValue must be at least 1 character")
			continue
		}
		if _, err := s.engine.Vault().Add(key.Platform, key.KeyValue, gateway.AddOptions{Label: keyName}); err != nil {
			addErr(keyName, gateway.RedactWith(err.Error(), key.KeyValue))
			continue
		}
		imported++
		existing[strings.TrimSpace(key.KeyValue)] = true
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"imported":         imported,
		"skipped":          []string{},
		"errors":           errs,
		"total":            len(req.Keys),
		"modelsRegistered": modelsRegistered,
	})
}

// ── key-file parser (ported from lib/key-parser.ts) ──────────────────────────

type keysCustomParsedModel struct {
	id     string
	tools  *bool
	vision *bool
}

type keysCustomParsedKey struct {
	rawKey   string
	prefix   string
	platform *string
	baseURL  string
	models   []keysCustomParsedModel
}

type keysCustomParseResult struct {
	keys    []keysCustomParsedKey
	skipped []string
}

type keysCustomKV struct {
	key, value string
}

type keysCustomKeyPair struct {
	key, value string
	platform   string
	baseURL    string
	models     []keysCustomParsedModel
}

// keysCustomPrefixMap maps a *_ env prefix to a platform (key-parser.ts:40-116).
var keysCustomPrefixMap = map[string]string{
	"GOOGLE_": "google", "GEMINI_": "google", "GROQ_": "groq", "CEREBRAS_": "cerebras",
	"SAIL_": "sail", "SAILRESEARCH_": "sail", "SAIL_RESEARCH_": "sail",
	"ELECTRONHUB_": "electronhub", "ELECTRON_HUB_": "electronhub",
	"EXPERIENTIAL_": "experiential", "EXPERIENTIALLABS_": "experiential",
	"EXPERIENTIAL_LABS_": "experiential", "EXPLABS_": "experiential",
	"ROUTER9_": "router9", "ROUTER_9_": "router9",
	"SEPTOR_": "septor", "SEPTORLABS_": "septor", "SEPTOR_LABS_": "septor",
	"BAI_": "bai", "B_AI_": "bai",
	"RADEON_": "radeon", "AMD_RADEON_": "radeon", "AMD_TOKENFACTORY_": "radeon",
	"NVIDIA_": "nvidia", "MISTRAL_": "mistral", "OPENROUTER_": "openrouter",
	"GITHUB_": "github", "COHERE_": "cohere", "CLOUDFLARE_": "cloudflare",
	"ZHIPU_": "zhipu", "OLLAMA_": "ollama", "OLLAMA_CLOUD_": "ollama",
	"HF_": "huggingface", "HUGGINGFACE_": "huggingface", "OPENCODE_": "opencode",
	"AGNES_": "agnes", "REKA_": "reka", "SILICONFLOW_": "siliconflow",
	"ROUTEWAY_": "routeway", "BAZAARLINK_": "bazaarlink", "AINATIVE_": "ainative",
	"AION_": "aion", "AIONLABS_": "aion", "AION_LABS_": "aion", "REQUESTY_": "requesty",
	"NAVY_": "navy", "NAVYAI_": "navy", "API_NAVY_": "navy",
	"NARA_": "nara", "NARAROUTER_": "nara", "BYNARA_": "nara",
	"SEALION_": "sealion", "SEA_LION_": "sealion",
	"ORCAROUTER_": "orcarouter", "ORCA_ROUTER_": "orcarouter", "ORCA_": "orcarouter",
	"UNOROUTER_": "unorouter", "UNO_ROUTER_": "unorouter", "XKIRO_": "xkiro",
	"MODELSCOPE_": "modelscope", "MODEL_SCOPE_": "modelscope",
	"ANYAPI_": "anyapi", "ANY_API_": "anyapi", "AIHORDE_": "aihorde",
	"QIANFAN_": "qianfan", "BAIDU_": "qianfan", "ERNIE_": "qianfan",
	"VOLCENGINE_": "volcengine", "VOLC_": "volcengine", "ARK_": "volcengine", "DOUBAO_": "volcengine",
	"LONGCAT_": "longcat", "XFYUN_": "xfyun", "SPARK_": "xfyun", "IFLYTEK_": "xfyun",
}

// keysCustomAuthJSONMap maps an opencode auth.json provider name to a platform
// (key-parser.ts:118-181).
var keysCustomAuthJSONMap = map[string]string{
	"gemini": "google", "google": "google", "groq": "groq", "sail": "sail",
	"sail-research": "sail", "sailresearch": "sail", "electronhub": "electronhub",
	"electron-hub": "electronhub", "experiential": "experiential",
	"experientiallabs": "experiential", "experiential-labs": "experiential",
	"explabs": "experiential", "router9": "router9", "router-9": "router9",
	"septor": "septor", "septorlabs": "septor", "septor-labs": "septor",
	"bai": "bai", "b-ai": "bai", "radeon": "radeon", "radeon-cloud": "radeon",
	"amd-radeon": "radeon", "amd-tokenfactory": "radeon", "openrouter": "openrouter",
	"ollama-cloud": "ollama", "ollama": "ollama", "nvidia": "nvidia",
	"opencode-zen": "opencode", "opencode": "opencode", "aion": "aion",
	"aion-labs": "aion", "aionlabs": "aion", "requesty": "requesty",
	"navy": "navy", "navyai": "navy", "api-navy": "navy", "nara": "nara",
	"bynara": "nara", "nara-router": "nara", "sealion": "sealion", "sea-lion": "sealion",
	"orcarouter": "orcarouter", "orca-router": "orcarouter", "orca": "orcarouter",
	"unorouter": "unorouter", "uno-router": "unorouter", "xkiro": "xkiro",
	"modelscope": "modelscope", "model-scope": "modelscope", "anyapi": "anyapi",
	"any-api": "anyapi", "qianfan": "qianfan", "baidu": "qianfan", "ernie": "qianfan",
	"volcengine": "volcengine", "volc": "volcengine", "ark": "volcengine",
	"doubao": "volcengine", "longcat": "longcat", "xfyun": "xfyun",
	"spark": "xfyun", "iflytek": "xfyun",
}

var (
	keysCustomReCustomBaseURL = regexp.MustCompile(`^CUSTOM_(.+)_BASE_URL$`)
	keysCustomReCustomModels  = regexp.MustCompile(`^CUSTOM_(.+)_MODELS$`)
	keysCustomRePrefixModels  = regexp.MustCompile(`^(.+)_CUSTOM_MODELS$`)
	keysCustomReBaseURL       = regexp.MustCompile(`^(.+)_BASE_URL$`)
	keysCustomReCustomKey     = regexp.MustCompile(`^CUSTOM_(.+)_KEY$`)
	keysCustomReAPIKey        = regexp.MustCompile(`^(.+)_API_KEY$`)
	keysCustomReKey           = regexp.MustCompile(`^(.+)_KEY$`)
	keysCustomReNumber        = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	keysCustomReHTTP          = regexp.MustCompile(`(?i)^https?://`)
	keysCustomReCSV           = regexp.MustCompile(`^"?([^"]*?)"?,"?([^"]*?)"?(?:,"?([^"]*?)"?)?(?:,"?([^"]*?)"?)?$`)
)

func keysCustomSplitRawKey(rawKey string) (name, value string) {
	if i := strings.Index(rawKey, "="); i != -1 {
		return rawKey[:i], rawKey[i+1:]
	}
	return rawKey, ""
}

func keysCustomDetectPlatform(prefix string) *string {
	if p, ok := keysCustomPrefixMap[prefix]; ok {
		return &p
	}
	return nil
}

func keysCustomParseDotEnv(content string) []keysCustomKV {
	text := strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	seen := map[string]int{}
	var out []keysCustomKV
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq == -1 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimLeft(line[eq+1:], " \t")
		var quote byte
		if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
			quote = value[0]
		}
		if quote != 0 {
			if ci := strings.IndexByte(value[1:], quote); ci != -1 {
				value = value[1 : ci+1]
			} else {
				value = keysCustomStripInlineComment(value)
			}
		} else {
			value = keysCustomStripInlineComment(value)
		}
		if idx, ok := seen[key]; ok {
			out[idx].value = value
		} else {
			seen[key] = len(out)
			out = append(out, keysCustomKV{key, value})
		}
	}
	return out
}

func keysCustomStripInlineComment(value string) string {
	if i := strings.Index(value, " #"); i != -1 {
		value = value[:i]
	}
	return strings.TrimRight(value, " \t")
}

func keysCustomStripJSONCComments(text string) string {
	var out strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '"' {
			out.WriteByte('"')
			i++
			for i < len(text) {
				out.WriteByte(text[i])
				if text[i] == '\\' {
					i++
					if i < len(text) {
						out.WriteByte(text[i])
						i++
					}
				} else if text[i] == '"' {
					i++
					break
				} else {
					i++
				}
			}
			continue
		}
		if i+1 < len(text) && text[i] == '/' && text[i+1] == '/' {
			i += 2
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(text) && text[i] == '/' && text[i+1] == '*' {
			i += 2
			for i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

func keysCustomStripTrailingCommas(text string) string {
	var out strings.Builder
	i := 0
	for i < len(text) {
		if text[i] == '"' {
			out.WriteByte('"')
			i++
			for i < len(text) {
				out.WriteByte(text[i])
				if text[i] == '\\' {
					i++
					if i < len(text) {
						out.WriteByte(text[i])
						i++
					}
				} else if text[i] == '"' {
					i++
					break
				} else {
					i++
				}
			}
			continue
		}
		if text[i] == ',' {
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\n' || text[j] == '\r') {
				j++
			}
			if j < len(text) && (text[j] == '}' || text[j] == ']') {
				i++
				continue
			}
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String()
}

func keysCustomParseJSONObject(content string) []keysCustomKV {
	var parsed map[string]any
	if json.Unmarshal([]byte(content), &parsed) != nil {
		return nil
	}
	var out []keysCustomKV
	for k, v := range parsed {
		if s, ok := v.(string); ok {
			out = append(out, keysCustomKV{k, s})
		}
	}
	return out
}

// keysCustomParseExportJSON reads the app's own export format
// ({ keys: [{ platform, key, label, baseUrl? }] }); returns ok=false when the
// content is not that shape (key-parser.ts:333-376).
func keysCustomParseExportJSON(content string) (keysCustomParseResult, bool) {
	var obj struct {
		Keys []struct {
			Platform string `json:"platform"`
			Key      string `json:"key"`
			Label    string `json:"label"`
			BaseURL  string `json:"baseUrl"`
		} `json:"keys"`
	}
	if json.Unmarshal([]byte(content), &obj) != nil || obj.Keys == nil {
		return keysCustomParseResult{}, false
	}
	if len(obj.Keys) > 0 && (obj.Keys[0].Platform == "" || obj.Keys[0].Key == "") {
		return keysCustomParseResult{}, false
	}
	res := keysCustomParseResult{keys: []keysCustomParsedKey{}, skipped: []string{}}
	for _, row := range obj.Keys {
		platform := row.Platform
		label := row.Label
		if label == "" {
			if platform != "" {
				label = platform
			} else {
				label = "imported"
			}
		}
		if strings.TrimSpace(row.Key) == "" {
			res.skipped = append(res.skipped, label+": empty key value")
			continue
		}
		prefix := ""
		var plat *string
		if platform != "" {
			p := platform
			plat = &p
			prefix = keysCustomPrefixForPlatform(platform)
		}
		res.keys = append(res.keys, keysCustomParsedKey{
			rawKey:   label + "=" + row.Key,
			prefix:   prefix,
			platform: plat,
			baseURL:  strings.TrimSpace(row.BaseURL),
		})
	}
	return res, true
}

func keysCustomPrefixForPlatform(platform string) string {
	for prefix, p := range keysCustomPrefixMap {
		if p == platform {
			return prefix
		}
	}
	return strings.ToUpper(platform) + "_"
}

func keysCustomParseCSV(content string) []keysCustomKeyPair {
	var out []keysCustomKeyPair
	lines := strings.Split(content, "\n")
	start := 0
	for len(lines) > start && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if len(lines) > start && strings.HasPrefix(strings.ToLower(lines[start]), "platform,") {
		start++
	}
	for i := start; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		m := keysCustomReCSV.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		platform := strings.TrimSpace(m[1])
		key := strings.TrimSpace(m[2])
		baseURL := strings.TrimSpace(m[4])
		if key == "" || platform == "" {
			continue
		}
		out = append(out, keysCustomKeyPair{
			key: strings.ToUpper(platform) + "_KEY", value: key, platform: platform, baseURL: baseURL,
		})
	}
	return out
}

func keysCustomParseAuthJSON(content string) keysCustomParseResult {
	res := keysCustomParseResult{keys: []keysCustomParsedKey{}, skipped: []string{}}
	var obj struct {
		CredentialPool map[string][]map[string]any `json:"credential_pool"`
	}
	if json.Unmarshal([]byte(content), &obj) != nil || obj.CredentialPool == nil {
		return res
	}
	for provider, creds := range obj.CredentialPool {
		for _, row := range creds {
			label := provider
			if l, ok := row["label"].(string); ok && l != "" {
				label = l
			} else if id, ok := row["id"].(string); ok && id != "" {
				label = id
			}
			if at, ok := row["auth_type"]; ok {
				if ats, _ := at.(string); ats != "api_key" {
					res.skipped = append(res.skipped, fmt.Sprintf("%s/%s: auth_type is %v", provider, label, at))
					continue
				}
			}
			token, _ := row["access_token"].(string)
			if strings.TrimSpace(token) == "" {
				res.skipped = append(res.skipped, fmt.Sprintf("%s/%s: no access_token", provider, label))
				continue
			}
			platform, ok := keysCustomAuthJSONMap[provider]
			if !ok {
				res.skipped = append(res.skipped, fmt.Sprintf("%s/%s: no platform mapping", provider, label))
				continue
			}
			p := platform
			res.keys = append(res.keys, keysCustomParsedKey{
				rawKey: label + "=" + token, prefix: keysCustomPrefixForPlatform(platform), platform: &p,
			})
		}
	}
	return res
}

// keysCustomParseModelList parses a comma-separated model list, stripping
// trailing -TOOLS / -VISION suffixes into capability flags (key-parser.ts:
// 478-499).
func keysCustomParseModelList(value string) []keysCustomParsedModel {
	var out []keysCustomParsedModel
	seen := map[string]bool{}
	for _, raw := range strings.Split(value, ",") {
		id := strings.TrimSpace(raw)
		var tools, vision *bool
		for {
			if strings.HasSuffix(id, "-TOOLS") {
				t := true
				tools = &t
				id = strings.TrimSuffix(id, "-TOOLS")
			} else if strings.HasSuffix(id, "-VISION") {
				v := true
				vision = &v
				id = strings.TrimSuffix(id, "-VISION")
			} else {
				break
			}
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, keysCustomParsedModel{id: id, tools: tools, vision: vision})
	}
	return out
}

// keysCustomPairCustomEndpointEnv folds paired CUSTOM_<n>_BASE_URL /
// CUSTOM_<n>_KEY (+ _MODELS) lines, and <PREFIX>_CUSTOM_MODELS trios, into one
// custom key carrying its endpoint (key-parser.ts:521-590).
func keysCustomPairCustomEndpointEnv(pairs []keysCustomKV) ([]keysCustomKeyPair, []string) {
	baseURLs := map[string]string{}
	modelLists := map[string][]keysCustomParsedModel{}
	prefixModels := map[string][]keysCustomParsedModel{}
	for _, p := range pairs {
		upper := strings.ToUpper(p.key)
		if m := keysCustomReCustomBaseURL.FindStringSubmatch(upper); m != nil && strings.TrimSpace(p.value) != "" {
			baseURLs[m[1]] = strings.TrimSpace(p.value)
			continue
		}
		if m := keysCustomReCustomModels.FindStringSubmatch(upper); m != nil {
			modelLists[m[1]] = keysCustomParseModelList(p.value)
			continue
		}
		if m := keysCustomRePrefixModels.FindStringSubmatch(upper); m != nil {
			prefixModels[m[1]] = keysCustomParseModelList(p.value)
		}
	}
	prefixURLs := map[string]string{}
	for _, p := range pairs {
		if m := keysCustomReBaseURL.FindStringSubmatch(strings.ToUpper(p.key)); m != nil {
			if _, ok := prefixModels[m[1]]; ok && strings.TrimSpace(p.value) != "" {
				prefixURLs[m[1]] = strings.TrimSpace(p.value)
			}
		}
	}
	if len(baseURLs) == 0 && len(modelLists) == 0 && len(prefixModels) == 0 {
		out := make([]keysCustomKeyPair, len(pairs))
		for i, p := range pairs {
			out[i] = keysCustomKeyPair{key: p.key, value: p.value}
		}
		return out, nil
	}

	var out []keysCustomKeyPair
	attachedLists := map[string]bool{}
	attachedPrefixes := map[string]bool{}
	for _, p := range pairs {
		upper := strings.ToUpper(p.key)
		if keysCustomReCustomBaseURL.MatchString(upper) {
			continue
		}
		if keysCustomReCustomModels.MatchString(upper) || keysCustomRePrefixModels.MatchString(upper) {
			continue
		}
		if m := keysCustomReBaseURL.FindStringSubmatch(upper); m != nil {
			if _, ok := prefixURLs[m[1]]; ok {
				continue
			}
		}

		if m := keysCustomReCustomKey.FindStringSubmatch(upper); m != nil {
			if customURL, ok := baseURLs[m[1]]; ok {
				var models []keysCustomParsedModel
				if ml, ok := modelLists[m[1]]; ok {
					attachedLists[m[1]] = true
					models = ml
				}
				out = append(out, keysCustomKeyPair{key: p.key, value: p.value, platform: "custom", baseURL: customURL, models: models})
				continue
			}
		}

		folded := false
		for _, re := range []*regexp.Regexp{keysCustomReAPIKey, keysCustomReKey} {
			m := re.FindStringSubmatch(upper)
			if m == nil {
				continue
			}
			prefixURL, ok := prefixURLs[m[1]]
			if !ok {
				continue
			}
			var models []keysCustomParsedModel
			if !attachedPrefixes[m[1]] {
				models = prefixModels[m[1]]
			}
			attachedPrefixes[m[1]] = true
			out = append(out, keysCustomKeyPair{key: p.key, value: p.value, platform: "custom", baseURL: prefixURL, models: models})
			folded = true
			break
		}
		if folded {
			continue
		}
		out = append(out, keysCustomKeyPair{key: p.key, value: p.value})
	}

	var skipped []string
	for n := range modelLists {
		if !attachedLists[n] {
			skipped = append(skipped, fmt.Sprintf("CUSTOM_%s_MODELS: no CUSTOM_%s_BASE_URL / CUSTOM_%s_KEY pair to attach to", n, n, n))
		}
	}
	for p := range prefixModels {
		if !attachedPrefixes[p] {
			skipped = append(skipped, fmt.Sprintf("%s_CUSTOM_MODELS: needs %s_BASE_URL and %s_API_KEY (or %s_KEY) in the same paste", p, p, p, p))
		}
	}
	return out, skipped
}

func keysCustomExtractPrefix(key string) string {
	upper := strings.ToUpper(key)
	prefixes := make([]string, 0, len(keysCustomPrefixMap))
	for p := range keysCustomPrefixMap {
		prefixes = append(prefixes, p)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, p := range prefixes {
		if strings.HasPrefix(upper, p) {
			return p
		}
	}
	first := strings.IndexByte(upper, '_')
	if first == -1 {
		return ""
	}
	candidate := upper[:first+1]
	if strings.Contains(upper[first+1:], "_") {
		return candidate
	}
	return ""
}

func keysCustomLooksLikeAPIKey(value string) bool {
	if len(value) < 8 {
		return false
	}
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no":
		return false
	}
	if keysCustomReNumber.MatchString(value) {
		return false
	}
	if keysCustomReHTTP.MatchString(value) {
		return false
	}
	if strings.Contains(value, "/") {
		return false
	}
	return strings.ContainsFunc(value, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	})
}

func keysCustomToParsedKeys(pairs []keysCustomKeyPair) keysCustomParseResult {
	res := keysCustomParseResult{keys: []keysCustomParsedKey{}, skipped: []string{}}
	for _, p := range pairs {
		prefix := keysCustomExtractPrefix(p.key)
		var platform *string
		if p.platform != "" {
			plat := p.platform
			platform = &plat
		} else {
			platform = keysCustomDetectPlatform(prefix)
		}
		if platform != nil {
			res.keys = append(res.keys, keysCustomParsedKey{
				rawKey: p.key + "=" + p.value, prefix: prefix, platform: platform,
				baseURL: p.baseURL, models: p.models,
			})
			continue
		}
		if keysCustomLooksLikeAPIKey(p.value) {
			res.keys = append(res.keys, keysCustomParsedKey{rawKey: p.key + "=" + p.value, prefix: prefix})
		} else {
			res.skipped = append(res.skipped, p.key+": value does not look like an API key")
		}
	}
	return res
}

func keysCustomParseEnvText(text string) keysCustomParseResult {
	pairs, skipped := keysCustomPairCustomEndpointEnv(keysCustomParseDotEnv(text))
	res := keysCustomToParsedKeys(pairs)
	res.skipped = append(res.skipped, skipped...)
	return res
}

// keysCustomParseFile parses one uploaded key file into candidate keys, by
// extension: JSON/JSONC (export format, opencode auth.json, or a flat map),
// CSV, or dotenv text (key-parser.ts:649-679).
func keysCustomParseFile(content, filename string) keysCustomParseResult {
	text := strings.ReplaceAll(strings.TrimPrefix(content, "\ufeff"), "\r\n", "\n")
	ext := ""
	if i := strings.LastIndex(filename, "."); i != -1 {
		ext = strings.ToLower(filename[i:])
	}

	switch ext {
	case ".json", ".jsonc":
		clean := keysCustomStripTrailingCommas(keysCustomStripJSONCComments(text))
		if res, ok := keysCustomParseExportJSON(clean); ok {
			return res
		}
		var probe map[string]any
		if json.Unmarshal([]byte(clean), &probe) != nil {
			return keysCustomParseEnvText(text)
		}
		if _, ok := probe["credential_pool"]; ok {
			return keysCustomParseAuthJSON(clean)
		}
		return keysCustomToParsedKeys(keysCustomKVToPairs(keysCustomParseJSONObject(clean)))
	case ".csv":
		return keysCustomToParsedKeys(keysCustomParseCSV(text))
	}
	return keysCustomParseEnvText(text)
}

func keysCustomKVToPairs(kvs []keysCustomKV) []keysCustomKeyPair {
	out := make([]keysCustomKeyPair, len(kvs))
	for i, kv := range kvs {
		out[i] = keysCustomKeyPair{key: kv.key, value: kv.value}
	}
	return out
}
