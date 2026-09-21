package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/gateway"
)

// The Quota signals view reads ONE ROW PER METRIC, not one per pool.
//
// The engine stores a pool's requests and tokens side by side in a single row
// with Unix timestamps, which is the right storage shape. Handing that to the
// dashboard verbatim gave it a row it could not read: it calls string methods
// on `observedAt` and renders `metric`, `source` and `confidence` per row, so
// the tab died with "e.includes is not a function" and took the page with it.
//
// This flattens the stored row into the metrics it actually carries and
// renders the instants as the date strings the client parses.

type quotaSignal struct {
	Platform     string  `json:"platform"`
	KeyID        int64   `json:"keyId"`
	KeyLabel     *string `json:"keyLabel"`
	QuotaPoolKey string  `json:"quotaPoolKey"`
	Metric       string  `json:"metric"`

	Limit     *int64  `json:"limit"`
	Remaining *int64  `json:"remaining"`
	ResetAt   *string `json:"resetAt"`

	ResetStrategy string  `json:"resetStrategy"`
	Source        string  `json:"source"`
	Confidence    float64 `json:"confidence"`
	Notes         *string `json:"notes"`

	ObservedAt string `json:"observedAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// quotaSignals flattens the stored pool rows into per-metric signals.
func (s *Server) quotaSignals(ctx context.Context) []quotaSignal {
	states, err := s.engine.Ledger().QuotaStates(ctx)
	if err != nil || len(states) == 0 {
		return []quotaSignal{}
	}
	labels := s.keyLabels(ctx)

	out := make([]quotaSignal, 0, len(states))
	for _, st := range states {
		keyID := keyIDFromPoolKey(st.PoolKey)
		observed := sqliteDateTime(st.ObservedAt)

		base := quotaSignal{
			Platform:     st.Platform,
			KeyID:        keyID,
			QuotaPoolKey: st.PoolKey,
			// Every reading here came from the provider's own rate-limit
			// headers on a real response, so it is reported rather than
			// inferred, and carries full confidence. A guess would need a
			// lower number and a different source.
			Source:     "header",
			Confidence: 1,
			ObservedAt: observed,
			UpdatedAt:  observed,
		}
		if label, ok := labels[keyID]; ok && label != "" {
			l := label
			base.KeyLabel = &l
		}
		// A provider-stated reset is exactly that; without one there is
		// nothing to claim about how the window behaves.
		base.ResetStrategy = "unknown"
		if st.ResetsAt != nil {
			iso := isoZ(*st.ResetsAt)
			base.ResetAt = &iso
			base.ResetStrategy = "provider_reported"
		}

		if st.RequestsLimit != nil || st.RequestsRemaining != nil {
			row := base
			row.Metric = "requests"
			row.Limit = st.RequestsLimit
			row.Remaining = st.RequestsRemaining
			// A subscription window is a share of an allowance, not a
			// request count: saying "requests" would misdescribe it.
			if label := gateway.WindowLabel(st.PoolKey); label != "" {
				row.Metric = "credits"
				row.ResetStrategy = "rolling_window"
				note := label + " - share of the subscription allowance"
				row.Notes = &note
			}
			out = append(out, row)
		}
		if st.TokensLimit != nil || st.TokensRemaining != nil {
			row := base
			row.Metric = "tokens"
			row.Limit = st.TokensLimit
			row.Remaining = st.TokensRemaining
			// A body-published balance (Hyper's hypercredits) is stored on the
			// tokens axis for want of a column, but it is a running credit
			// balance read from the response payload, not a token window from a
			// rate-limit header. Label it truthfully so the view does not call
			// it "tokens" from a "header".
			if metric, ok := gateway.BodyQuotaMetric(st.Platform); ok {
				row.Metric = metric
				row.Source = "body"
			}
			out = append(out, row)
		}
	}
	return out
}

// keyLabels maps key ids to their operator-facing labels, so a signal can name
// the credential near its limit instead of only its number.
func (s *Server) keyLabels(ctx context.Context) map[int64]string {
	rows, err := s.engine.DB().QueryContext(ctx, "SELECT id, label FROM api_keys")
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	out := map[int64]string{}
	for rows.Next() {
		var (
			id    int64
			label string
		)
		if err := rows.Scan(&id, &label); err != nil {
			return out
		}
		out[id] = label
	}
	return out
}

// keyIDFromPoolKey reads the key id off a pool key, which the engine builds as
// "platform/keyID" (gateway.PoolKey) and, for a subscription window, extends to
// "platform/keyID::window" (subscription_quota.go). The "::window" suffix has
// to come off first: LastIndex("/") would otherwise leave "keyID::window",
// which ParseInt rejects, so a window signal would lose its credential
// attribution and render as an unnamed key. A pool key without an id yields
// zero, which the client renders as an unnamed key rather than failing.
func keyIDFromPoolKey(poolKey string) int64 {
	if idx := strings.Index(poolKey, "::"); idx >= 0 {
		poolKey = poolKey[:idx]
	}
	idx := strings.LastIndex(poolKey, "/")
	if idx < 0 {
		return 0
	}
	id, err := strconv.ParseInt(poolKey[idx+1:], 10, 64)
	if err != nil {
		return 0
	}
	return id
}
