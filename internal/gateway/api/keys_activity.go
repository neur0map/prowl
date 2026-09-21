package api

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// Per-key activity: what the pool is actually using, and why a member is not
// working.
//
// A status dot says a key is unhealthy but not why, and nothing said which
// keys carry the traffic. Both answers are already in the request log; this
// route joins them per key so the dashboard can show "served 41, failed 3,
// last refused because the daily quota is spent" instead of a red dot.
//
// It is a Prowl route, deliberately separate from /api/keys: that response is
// field-identical to the reference and adding to it would break a client that
// expects the original shape.

type keyActivity struct {
	KeyID int64 `json:"keyId"`

	Served  int64  `json:"served"`
	Failed  int64  `json:"failed"`
	Tokens  int64  `json:"tokens"`
	Hops    int64  `json:"hops"`
	AvgMs   int64  `json:"avgMs"`
	Window  int    `json:"windowHours"`
	LastUse *int64 `json:"lastUsedAt"`

	// LastError is the most recent refusal, in the provider's own words, with
	// the kind the router classified it as.
	LastError     string `json:"lastError,omitempty"`
	LastErrorKind string `json:"lastErrorKind,omitempty"`
	LastErrorAt   *int64 `json:"lastErrorAt"`

	// CoolingUntil is set while the router is holding this key back, with
	// CoolingModels saying how much of the key that covers.
	CoolingUntil  *int64 `json:"coolingUntil"`
	CoolingModels int    `json:"coolingModels"`
}

func (s *Server) registerKeyActivityRoutes() {
	s.mux.HandleFunc("GET /api/keys/activity", s.RequireKey(s.handleKeysActivity))
}

func (s *Server) handleKeysActivity(w http.ResponseWriter, r *http.Request) {
	const windowHours = 24
	since := time.Now().UTC().Add(-windowHours * time.Hour).Unix()

	rows, err := s.engine.DB().QueryContext(r.Context(), `
		SELECT key_id,
		       SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END) AS served,
		       SUM(CASE WHEN outcome <> 'success' THEN 1 ELSE 0 END) AS failed,
		       COALESCE(SUM(output_tokens), 0) AS tokens,
		       CAST(COALESCE(AVG(latency_ms), 0) AS INTEGER) AS avg_ms,
		       MAX(created_at) AS last_used
		  FROM requests
		 WHERE key_id IS NOT NULL AND created_at >= ?
		 GROUP BY key_id`, since)
	if err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rows.Close() }()

	byKey := map[int64]*keyActivity{}
	for rows.Next() {
		a := &keyActivity{Window: windowHours}
		var lastUse sql.NullInt64
		if err := rows.Scan(&a.KeyID, &a.Served, &a.Failed, &a.Tokens, &a.AvgMs, &lastUse); err != nil {
			WriteBareError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if lastUse.Valid {
			v := lastUse.Int64
			a.LastUse = &v
		}
		byKey[a.KeyID] = a
	}
	if err := rows.Err(); err != nil {
		WriteBareError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Failed hops are the invisible work: a request that eventually succeeded
	// still burned an attempt on every key that refused it first, and that is
	// what explains a key contributing nothing while looking healthy.
	hopRows, err := s.engine.DB().QueryContext(r.Context(), `
		SELECT key_id, COUNT(*) AS hops
		  FROM request_attempts
		 WHERE key_id IS NOT NULL AND created_at >= ?
		 GROUP BY key_id`, since)
	if err == nil {
		defer func() { _ = hopRows.Close() }()
		for hopRows.Next() {
			var keyID, hops int64
			if err := hopRows.Scan(&keyID, &hops); err != nil {
				break
			}
			if a, ok := byKey[keyID]; ok {
				a.Hops = hops
				continue
			}
			byKey[keyID] = &keyActivity{KeyID: keyID, Hops: hops, Window: windowHours}
		}
	}

	// The newest refusal per key, which is the "why" the status dot omits.
	errRows, err := s.engine.DB().QueryContext(r.Context(), `
		SELECT a.key_id, a.error_kind, COALESCE(a.error_message, ''), a.created_at
		  FROM request_attempts a
		  JOIN (SELECT key_id, MAX(id) AS id
		          FROM request_attempts
		         WHERE key_id IS NOT NULL AND created_at >= ?
		         GROUP BY key_id) newest
		    ON newest.id = a.id`, since)
	if err == nil {
		defer func() { _ = errRows.Close() }()
		for errRows.Next() {
			var (
				keyID     int64
				kind, msg string
				at        int64
			)
			if err := errRows.Scan(&keyID, &kind, &msg, &at); err != nil {
				break
			}
			a, ok := byKey[keyID]
			if !ok {
				a = &keyActivity{KeyID: keyID, Window: windowHours}
				byKey[keyID] = a
			}
			a.LastErrorKind = kind
			a.LastError = gateway.RedactWith(msg)
			a.LastErrorAt = &at
		}
	}

	// A cooling key is held back on purpose, which reads very differently
	// from a broken one.
	for keyID, bench := range s.engine.Cooldowns().BenchedByKey() {
		a, ok := byKey[keyID]
		if !ok {
			a = &keyActivity{KeyID: keyID, Window: windowHours}
			byKey[keyID] = a
		}
		ts := bench.Until.Unix()
		a.CoolingUntil = &ts
		a.CoolingModels = bench.Models
	}

	out := make([]*keyActivity, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, a)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"activity": out, "windowHours": windowHours})
}
