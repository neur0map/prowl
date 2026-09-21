package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/neur0map/prowl/internal/gateway"
	"github.com/neur0map/prowl/internal/gateway/provider"
)

// Routing for the modalities whose models live outside the `models` table.
//
// Chain construction cannot read embedding_models or media_models, but
// everything after candidate selection is identical to chat. Building a
// gateway.Chain over their candidates reuses the real failover loop, so an
// embedding failure cools its provider down and lands in the request trail
// instead of vanishing into a private retry loop.

// Model-id namespaces for the shared penalty and cooldown stores. Those stores
// key on ModelDBID, and all three catalogues start their ids at 1, so without
// an offset a failing embedding model would bench the chat model of the same
// id.
const (
	embeddingModelIDBase int64 = 1 << 40
	mediaModelIDBase     int64 = 2 << 40
)

type modalityCandidate struct {
	ModelDBID int64
	Platform  string
	ModelID   string
	KeyID     int64
	KeyLabel  string
	BaseURL   string

	// MaxInputTokens and Dimensions are 0 when the catalogue publishes none.
	MaxInputTokens int64
	Dimensions     int64

	// RequestStyle is the media row's meta_json deployment flavour. Cloudflare
	// hosts a "json" (base64 in a JSON body) and a binary transcription
	// deployment under one platform, told apart only by this field, so a row
	// that names it must carry it into the request.
	RequestStyle string
}

// modalityChain adapts a candidate list to the engine's Chain contract. Order
// comes from the catalogue's own priority column: these pools are small and
// operator-ordered, so adaptive scoring would add noise rather than signal.
type modalityChain struct {
	candidates []modalityCandidate
}

func (m *modalityChain) Route(_ int, skip *gateway.SkipState) (gateway.Route, error) {
	for _, candidate := range m.candidates {
		if skip != nil && modalitySkipped(skip, candidate) {
			continue
		}
		return gateway.Route{
			Platform:  candidate.Platform,
			ModelID:   candidate.ModelID,
			ModelDBID: candidate.ModelDBID,
			KeyID:     candidate.KeyID,
			KeyLabel:  candidate.KeyLabel,
			BaseURL:   candidate.BaseURL,
		}, nil
	}
	return gateway.Route{}, &gateway.RouteError{
		Status:  http.StatusServiceUnavailable,
		Message: "no configured provider can serve this request",
	}
}

func modalitySkipped(skip *gateway.SkipState, c modalityCandidate) bool {
	if _, ok := skip.Models[c.ModelDBID]; ok {
		return true
	}
	if _, ok := skip.Platforms[c.Platform]; ok {
		return true
	}
	key := gateway.RouteKey{Platform: c.Platform, ModelID: c.ModelID, KeyID: c.KeyID}
	_, ok := skip.Keys[key]
	return ok
}

func (m *modalityChain) RoutableKeys(modelDBID int64) []int64 {
	var keys []int64
	for _, candidate := range m.candidates {
		if candidate.ModelDBID == modelDBID {
			keys = append(keys, candidate.KeyID)
		}
	}
	return keys
}

func (m *modalityChain) HasOtherUsableKey(modelDBID, excludingKeyID int64, skipKeys map[gateway.RouteKey]struct{}) bool {
	for _, candidate := range m.candidates {
		if candidate.ModelDBID != modelDBID || candidate.KeyID == excludingKeyID {
			continue
		}
		key := gateway.RouteKey{
			Platform: candidate.Platform, ModelID: candidate.ModelID, KeyID: candidate.KeyID,
		}
		if _, skipped := skipKeys[key]; skipped {
			continue
		}
		return true
	}
	return false
}

// candidateFor recovers the per-model metadata a Route does not carry.
func (m *modalityChain) candidateFor(route gateway.Route) (modalityCandidate, bool) {
	for _, candidate := range m.candidates {
		if candidate.ModelDBID == route.ModelDBID && candidate.KeyID == route.KeyID {
			return candidate, true
		}
	}
	return modalityCandidate{}, false
}

// embeddingCandidates resolves a family name or one platform's model id. A
// family resolving to several providers is what makes cross-provider failover
// for the same logical model possible.
func embeddingCandidates(ctx context.Context, db *sql.DB, model string) ([]modalityCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT em.id, em.platform, em.model_id,
		       COALESCE(em.dimensions, 0), COALESCE(em.max_input_tokens, 0),
		       k.id, k.label, COALESCE(k.base_url, '')
		  FROM embedding_models em
		  JOIN api_keys k
		    ON k.platform = em.platform AND k.enabled = 1 AND k.status <> 'error'
		 WHERE em.enabled = 1
		   AND (em.key_id IS NULL OR em.key_id = k.id)
		   AND (em.family = ? OR em.model_id = ? OR (em.platform || '/' || em.model_id) = ?)
		 ORDER BY em.priority ASC, em.id ASC, k.id ASC`, model, model, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []modalityCandidate
	for rows.Next() {
		var c modalityCandidate
		if err := rows.Scan(&c.ModelDBID, &c.Platform, &c.ModelID,
			&c.Dimensions, &c.MaxInputTokens,
			&c.KeyID, &c.KeyLabel, &c.BaseURL); err != nil {
			return nil, err
		}
		c.ModelDBID += embeddingModelIDBase
		out = append(out, c)
	}
	return out, rows.Err()
}

// mediaCandidates resolves one modality's pool. An empty or "auto" model means
// the caller is asking the gateway to choose.
func mediaCandidates(ctx context.Context, db *sql.DB, modality, model string) ([]modalityCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT mm.id, mm.platform, mm.model_id, COALESCE(mm.meta_json, ''),
		       k.id, k.label, COALESCE(k.base_url, '')
		  FROM media_models mm
		  JOIN api_keys k
		    ON k.platform = mm.platform AND k.enabled = 1 AND k.status <> 'error'
		 WHERE mm.enabled = 1
		   AND mm.modality = ?
		   AND (mm.key_id IS NULL OR mm.key_id = k.id)
		   AND (? = '' OR ? = 'auto' OR mm.model_id = ? OR (mm.platform || '/' || mm.model_id) = ?)
		 ORDER BY mm.priority ASC, mm.id ASC, k.id ASC`,
		modality, model, model, model, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []modalityCandidate
	for rows.Next() {
		var c modalityCandidate
		var meta string
		if err := rows.Scan(&c.ModelDBID, &c.Platform, &c.ModelID, &meta,
			&c.KeyID, &c.KeyLabel, &c.BaseURL); err != nil {
			return nil, err
		}
		c.ModelDBID += mediaModelIDBase
		c.RequestStyle = mediaRequestStyle(meta)
		out = append(out, c)
	}
	return out, rows.Err()
}

// mediaRequestStyle reads the deployment flavour a media row's meta_json names.
// A row that carries no meta_json, or one whose JSON omits the field, yields
// the empty (default binary) style, so a malformed row degrades to the safe
// default rather than failing the request.
func mediaRequestStyle(meta string) string {
	if meta == "" {
		return ""
	}
	var m struct {
		RequestStyle string `json:"requestStyle"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return ""
	}
	return m.RequestStyle
}

// modalityUnavailable separates the reasons a pool can be empty, because each
// needs a different action. Naming a model the catalogue HAS but no key can
// serve as "not found" would send the operator hunting for a typo instead of
// adding a credential.
func modalityUnavailable(w http.ResponseWriter, modality, model string, hasAnyModel, modelKnown bool) {
	named := model != "" && model != "auto"
	switch {
	case !hasAnyModel:
		WriteErrorCode(w, http.StatusServiceUnavailable, TypeServiceUnavailable, "no_models",
			fmt.Sprintf("this gateway has no %s models configured", modality))
	case named && !modelKnown:
		WriteErrorCode(w, http.StatusNotFound, TypeNotFound, "model_not_found",
			fmt.Sprintf("no %s model matches %q - check the name, or omit it to let the gateway choose", modality, model))
	case named:
		WriteErrorCode(w, http.StatusServiceUnavailable, TypeServiceUnavailable, "no_provider_keys",
			fmt.Sprintf("%q exists but no enabled provider key can serve it - add one in the Credentials tab of `prowl`", model))
	default:
		WriteErrorCode(w, http.StatusServiceUnavailable, TypeServiceUnavailable, "no_provider_keys",
			fmt.Sprintf("no provider key can serve %s yet - add one in the Credentials tab of `prowl`", modality))
	}
}

// embeddingModelKnown reports whether the catalogue carries the named family
// or model id at all, ignoring whether a key can serve it.
func embeddingModelKnown(ctx context.Context, db *sql.DB, model string) bool {
	var total int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM embedding_models
		 WHERE enabled = 1
		   AND (family = ? OR model_id = ? OR (platform || '/' || model_id) = ?)`,
		model, model, model).Scan(&total)
	return err == nil && total > 0
}

func mediaModelKnown(ctx context.Context, db *sql.DB, modality, model string) bool {
	var total int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM media_models
		 WHERE enabled = 1 AND modality = ?
		   AND (model_id = ? OR (platform || '/' || model_id) = ?)`,
		modality, model, model).Scan(&total)
	return err == nil && total > 0
}

// modalityFailureMessage is the redacted explanation for a failed embedding or
// media run, for the Logs view. It reads the exhaustion body when there is one
// and falls back to a plain notice, and it never carries a raw credential.
func modalityFailureMessage(result *gateway.Result, secrets []string) string {
	if result != nil && result.Exhaustion != nil {
		return gateway.RedactWith(result.Exhaustion.Message, secrets...)
	}
	return "every provider attempt failed"
}

// writeModalityFailure renders a failed modality run. Upstream text is scrubbed
// with the keys this request revealed before it reaches the caller, and the
// envelope never carries TypeAuthentication.
func writeModalityFailure(w http.ResponseWriter, result *gateway.Result, secrets []string) {
	if result == nil {
		WriteError(w, http.StatusBadGateway, TypeUpstream, "the router returned no result")
		return
	}
	if result.Exhaustion != nil {
		ex := result.Exhaustion
		message := gateway.RedactWith(ex.Message, secrets...)
		if ex.Status == http.StatusTooManyRequests && !ex.RetryAt.IsZero() {
			WriteRateLimited(w, message, ex.RetryAt)
			return
		}
		WriteErrorCode(w, ex.Status, exhaustionType(ex.Status), ex.Code, message)
		return
	}
	WriteError(w, http.StatusBadGateway, TypeUpstream, "every provider attempt failed")
}

// observeQuotaHeaders hands a provider's rate-limit headers to the ledger.
//
// Remaining quota is only ever published in headers, so this is the single
// place a real "how much is left" reading can come from. Without it the
// dashboard reports "no published quota" for every provider no matter how much
// traffic has gone through it.
//
// A 429 or 402 is itself the signal that a pool is spent - ObserveResponse
// records remaining=0 for it even with no headers - so an exhaustion reaches
// the ledger regardless of whether the provider bothered to send rate-limit
// headers. A 200 with no headers carries no signal and must never have a zero
// invented for it.
func (s *Server) observeQuotaHeaders(route gateway.Route, status int, headers http.Header) {
	depleted := status == http.StatusTooManyRequests || status == http.StatusPaymentRequired
	if len(headers) == 0 && !depleted {
		return
	}
	if _, err := s.engine.Ledger().ObserveResponse(
		route.Platform, route.ModelID, route.KeyID, status, headers); err != nil {
		slog.Debug("Could not record quota observation", "error", err,
			"platform", route.Platform)
	}
	// A subscription reports rolling windows as utilisation rather than
	// counts, which the counts-shaped observation cannot carry. Recording
	// them separately is what makes an active subscription show a budget at
	// all.
	s.engine.Ledger().ObserveUnifiedWindows(route.Platform, route.KeyID, headers)
}

// observeQuota pulls the headers off a failed attempt. A 429 carries the most
// useful reading of all, so the error path must not drop it - and a bare
// 429/402 with no headers still tells the ledger the pool is spent, so the
// forward is unconditional once the error is an HTTPError.
func (c *chatRelay) observeQuota(route gateway.Route, err error) {
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) {
		return
	}
	c.server.observeQuotaHeaders(route, httpErr.Status, httpErr.Headers)
}

// hasEnabledEmbeddingModels and hasEnabledMediaModels separate "no key" from
// "no such feature here".
func hasEnabledEmbeddingModels(ctx context.Context, db *sql.DB) bool {
	var total int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embedding_models WHERE enabled = 1`).Scan(&total)
	return err == nil && total > 0
}

func hasEnabledMediaModels(ctx context.Context, db *sql.DB, modality string) bool {
	var total int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_models WHERE enabled = 1 AND modality = ?`,
		modality).Scan(&total)
	return err == nil && total > 0
}
