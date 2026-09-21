package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/gateway"
)

// The TUI's Usage and Logs views read the same durable stores the reference's
// analytics dashboard did, in the shape this client draws: an aggregate line,
// a recent-requests table, a per-platform rollup, and the warn/error log tail.
// No range picker, no charts - those were the web UI's job.

func (s *Server) registerUsageRoutes() {
	s.mux.HandleFunc("GET /api/usage/summary", s.RequireKey(s.handleUsageSummary))
	s.mux.HandleFunc("GET /api/usage/requests", s.RequireKey(s.handleUsageRequests))
	s.mux.HandleFunc("GET /api/logs", s.RequireKey(s.handleServerLogs))
	s.mux.HandleFunc("DELETE /api/logs", s.RequireKey(s.handleServerLogsClear))
}

// usageSummary is the headline row the TUI shows on the overview. Its counts
// are honesty-aware: a request is exact (provider-reported usage), estimated
// (usage the gateway derived) or unavailable (no usage captured), and its cost
// is either known (computed or provider-reported) or unknown (no published
// price). CostUSD sums only the known costs, so it never reads an unknown
// price as free.
type usageSummary struct {
	Window              string  `json:"window"`
	Requests            int64   `json:"requests"`
	Successes           int64   `json:"successes"`
	Failures            int64   `json:"failures"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	ExactRequests       int64   `json:"exactRequests"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
	UnknownCostRequests int64   `json:"unknownCostRequests"`
	AvgLatencyMs        int64   `json:"avgLatencyMs"`
	FailoverRate        float64 `json:"failoverRate"`
}

type usageModelRow struct {
	Platform            string  `json:"platform"`
	Model               string  `json:"model"`
	Requests            int64   `json:"requests"`
	Errors              int64   `json:"errors"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
	AvgMs               int64   `json:"avgMs"`
}

// usageSeriesPoint is one time bucket of the summary chart: hourly for the 24h
// window, daily for 7d and 30d. Points are chronologically ordered and bounded
// to the requested window. CostUSD carries only known cost, matching the
// summary.
type usageSeriesPoint struct {
	Start               string  `json:"start"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	EstimatedRequests   int64   `json:"estimatedRequests"`
	UnavailableRequests int64   `json:"unavailableRequests"`
	CostUSD             float64 `json:"costUsd"`
	CostKnownRequests   int64   `json:"costKnownRequests"`
}

func (s *Server) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if !usageWindows[window] {
		window = "24h"
	}
	since := rangeSince(window)

	db := s.engine.DB()
	var out usageSummary
	var failovers int64
	out.Window = window
	err := db.QueryRowContext(r.Context(), `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN outcome = 'error' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'exact' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'estimated' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'unavailable' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN cost_usd ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER), 0),
		       COALESCE(SUM(CASE WHEN attempts > 1 THEN 1 ELSE 0 END), 0)
		  FROM requests WHERE created_at >= ?`, since).Scan(
		&out.Requests, &out.Successes, &out.Failures,
		&out.InputTokens, &out.OutputTokens,
		&out.ExactRequests, &out.EstimatedRequests, &out.UnavailableRequests,
		&out.CostUSD, &out.CostKnownRequests, &out.UnknownCostRequests,
		&out.AvgLatencyMs, &failovers)
	if err != nil && !isNoRows(err) {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the request trail")
		return
	}
	if out.Requests > 0 {
		out.FailoverRate = float64(failovers) / float64(out.Requests)
	}

	// Per-platform rollup straight from the raw trail: the TUI's Usage table
	// is small, and the hour rollup would need re-summing anyway.
	rows, err := db.QueryContext(r.Context(), `
		SELECT platform, COUNT(*),
		       COALESCE(SUM(CASE WHEN outcome = 'error' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(input_tokens + output_tokens), 0),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER), 0)
		  FROM requests WHERE created_at >= ?
		 GROUP BY platform ORDER BY COUNT(*) DESC LIMIT 12`, since)
	type platformRow struct {
		Platform string `json:"platform"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
		Tokens   int64  `json:"tokens"`
		AvgMs    int64  `json:"avgMs"`
	}
	var platforms []platformRow
	if err == nil {
		for rows.Next() {
			var p platformRow
			if err := rows.Scan(&p.Platform, &p.Requests, &p.Errors, &p.Tokens, &p.AvgMs); err == nil {
				platforms = append(platforms, p)
			}
		}
		_ = rows.Close()
	}

	modelRows, err := db.QueryContext(r.Context(), `
		SELECT platform, model_id, COUNT(*),
		       COALESCE(SUM(CASE WHEN outcome = 'error' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'estimated' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'unavailable' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN cost_usd ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(CAST(AVG(latency_ms) AS INTEGER), 0)
		  FROM requests WHERE created_at >= ?
		 GROUP BY platform, model_id
		 ORDER BY SUM(input_tokens + output_tokens) DESC, COUNT(*) DESC
		 LIMIT 20`, since)
	models := make([]usageModelRow, 0, 20)
	if err == nil {
		for modelRows.Next() {
			var model usageModelRow
			if err := modelRows.Scan(
				&model.Platform, &model.Model, &model.Requests, &model.Errors,
				&model.InputTokens, &model.OutputTokens, &model.EstimatedRequests,
				&model.UnavailableRequests, &model.CostUSD, &model.CostKnownRequests,
				&model.AvgMs,
			); err == nil {
				models = append(models, model)
			}
		}
		_ = modelRows.Close()
	}
	// The summary chart: one bucket per hour (24h) or per day (7d/30d), in
	// chronological order and bounded to the window.
	bucket := seriesBucketSeconds(window)
	series := make([]usageSeriesPoint, 0, 32)
	seriesRows, err := db.QueryContext(r.Context(), `
		SELECT (created_at / ?) * ? AS bucket,
		       COUNT(*),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'estimated' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN usage_quality = 'unavailable' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN cost_usd ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cost_known = 1 THEN 1 ELSE 0 END), 0)
		  FROM requests WHERE created_at >= ?
		 GROUP BY bucket ORDER BY bucket ASC`, bucket, bucket, since)
	if err == nil {
		for seriesRows.Next() {
			var (
				start int64
				p     usageSeriesPoint
			)
			if err := seriesRows.Scan(&start, &p.Requests, &p.InputTokens, &p.OutputTokens,
				&p.EstimatedRequests, &p.UnavailableRequests, &p.CostUSD, &p.CostKnownRequests); err == nil {
				p.Start = isoZ(start)
				series = append(series, p)
			}
		}
		_ = seriesRows.Close()
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"summary": out, "platforms": platforms, "models": models, "series": series,
	})
}

var usageWindows = map[string]bool{
	"24h": true, "7d": true, "30d": true,
}

func rangeSince(window string) int64 {
	now := time.Now()
	if !usageWindows[window] {
		window = "24h"
	}
	switch window {
	case "24h":
		return now.Add(-24 * time.Hour).Unix()
	case "30d":
		return now.Add(-30 * 24 * time.Hour).Unix()
	default: // "7d"
		return now.Add(-7 * 24 * time.Hour).Unix()
	}
}

// seriesBucketSeconds is the chart's time-bucket width for a window: hourly for
// 24h, daily for the longer windows, which is the granularity a terminal chart
// can actually render.
func seriesBucketSeconds(window string) int64 {
	if window == "24h" {
		return 3600
	}
	return 86400
}

func isNoRows(err error) bool { return err == sql.ErrNoRows }

// usageRequests is the recent-calls table.
func (s *Server) handleUsageRequests(w http.ResponseWriter, r *http.Request) {
	limit := 25
	var n int
	if _, err := fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &n); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	rows, err := s.engine.DB().QueryContext(r.Context(), `
		SELECT id, created_at, platform, model_id, outcome, status,
		       input_tokens, output_tokens, estimated, usage_quality,
		       cost_usd, cost_known, latency_ms, attempts,
		       routed_from, class, effort, error_kind, error_message
		  FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the request trail")
		return
	}
	defer func() { _ = rows.Close() }()

	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var (
			id, created     int64
			platform, mid   string
			outcome         string
			status          int
			inTok, outTok   int64
			estimated       int
			quality         string
			cost            float64
			costKnown       int
			latency         int64
			attempts        int
			routed, class   sql.NullString
			effort, errKind sql.NullString
			errMS           sql.NullString
		)
		if err := rows.Scan(&id, &created, &platform, &mid, &outcome, &status,
			&inTok, &outTok, &estimated, &quality, &cost, &costKnown, &latency, &attempts,
			&routed, &class, &effort, &errKind, &errMS); err != nil {
			continue
		}
		// An unknown cost renders as null, never as $0: the surface must tell
		// "no published price" from "free".
		var costOut any
		if costKnown != 0 {
			costOut = cost
		}
		out = append(out, map[string]any{
			"id": id, "createdAt": isoZ(created), "platform": platform, "model": mid,
			"outcome": outcome, "status": status, "inputTokens": inTok,
			"outputTokens": outTok, "estimated": estimated != 0,
			"usageQuality": quality, "costUsd": costOut, "costKnown": costKnown != 0,
			"latencyMs": latency, "attempts": attempts,
			"routedFrom": nullable(routed), "class": nullable(class), "effort": nullable(effort),
			"errorKind": nullable(errKind), "error": nullable(errMS),
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func nullable(v sql.NullString) any {
	if !v.Valid || v.String == "" {
		return nil
	}
	return v.String
}

// handleServerLogs reads the durable warn/error store the request path writes
// to - the failure history a terminal view actually needs.
func (s *Server) handleServerLogs(w http.ResponseWriter, r *http.Request) {
	var since *int64
	if v := strings.TrimSpace(r.URL.Query().Get("sinceId")); v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			since = &n
		}
	}
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	entries, err := logStoreFor(s.engine.DB()).query(r.Context(), since, nil,
		r.URL.Query().Get("q"), r.URL.Query().Get("provider"), limit)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not read the server log")
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"id": e.ID, "level": e.Level, "ts": e.TSms, "source": e.Source,
			"provider": e.Provider, "model": e.Model, "event": e.Event,
			"requestId": e.RequestID, "message": gateway.Redact(e.Message),
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"logs": out, "maxId": logStoreFor(s.engine.DB()).maxID(r.Context())})
}

func (s *Server) handleServerLogsClear(w http.ResponseWriter, r *http.Request) {
	if err := logStoreFor(s.engine.DB()).clear(r.Context()); err != nil {
		WriteError(w, http.StatusInternalServerError, TypeServer, "could not clear the server log")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"success": true})
}
