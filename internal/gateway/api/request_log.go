// Package-local persistence for the inference trail and the durable
// server-log store. In Prowl these writers back the TUI's Activity and
// Logs views directly; the analytics HTTP surface of the reference port
// (dashboard rollups) is deliberately not served.
package api

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// floorHour truncates a Unix-seconds instant to the start of its UTC hour,
// the granularity request_hourly is keyed on.
func floorHour(unix int64) int64 { return unix - unix%3600 }

// normalizeBaseURL is the trailing-slash-insensitive endpoint identity that
// endpoint_scope is stored under (endpoint-scope.ts:25-27).
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// sqliteDateTime renders a Unix-seconds instant the way FreeLLMAPI's SQLite
// text timestamps read: UTC, space separator, no fractional part. The savings
// projection and the first-request marker depend on this exact shape.
func sqliteDateTime(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04:05")
}

// isoZ renders a Unix-seconds instant as the zoned ISO string the recent-call
// views hand the client to parse.
func isoZ(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func nullEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func nullStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// ── Rollup write path ────────────────────────────────────────────────────────

// RequestAttemptLog is one upstream hop of a served request.
type RequestAttemptLog struct {
	Attempt  int
	Platform string
	ModelID  string
	// EndpointScope is the hop's normalized endpoint identity. request_attempts
	// persists a hop by platform/model/key, but a terminal failure row is
	// attributed to the last hop tried, so its scope rides here to keep a
	// custom relay's full identity on the parent requests row.
	EndpointScope string
	KeyID         *int64
	StatusCode    int
	LatencyMs     int64
	ErrorKind     string
	ErrorMessage  string
	CreatedAt     time.Time
}

// RequestLog is one completed inference call to persist. It is the input to the
// rollup write path.
type RequestLog struct {
	CreatedAt     time.Time
	Platform      string
	ModelID       string
	EndpointScope string
	KeyID         *int64
	Outcome       string // "success" | "error" | "canceled"
	StatusCode    int
	InputTokens   int64
	OutputTokens  int64
	Estimated     bool
	// UsageKnown records that the provider actually reported usage, so a
	// legitimate zero/zero count is exact rather than unavailable. It differs
	// from Estimated (a count the gateway derived): UsageKnown false with
	// Estimated false means no usage was captured at all.
	UsageKnown bool
	LatencyMs  int64
	TTFBMs     *int64
	// CostUSD, when positive (or when CostKnown is set), is an authoritative
	// monetary total -- a provider-reported cost -- and overrides any
	// catalog-rate estimate. Left zero with CostKnown false (the usual case),
	// the cost is derived from the catalog's per-million rates at record time;
	// a model with no published price records an unknown cost rather than a
	// silent $0.
	CostUSD float64
	// CostKnown lets a caller that already knows the exact cost assert it even
	// when that cost is legitimately $0, which a bare CostUSD cannot express.
	CostKnown      bool
	Attempts       int
	RequestedModel *string // the client-pinned model id; nil for auto/fusion
	Class          string
	Effort         string
	ErrorKind      string
	ErrorMessage   string
	Trail          []RequestAttemptLog
}

// RecordRequest persists a completed call and, in the same transaction, folds
// it into the durable request_hourly rollup and the lifetime settings counters.
//
// Nothing else populates these tables yet -- the inference proxy still records
// to the legacy JSONL usage log -- so this is the sole writer of the SQL trail
// the analytics surface reads. The rollup upsert is what lets a headline number
// survive the retention prune of the raw requests table.
func (s *Server) RecordRequest(ctx context.Context, log RequestLog) (int64, error) {
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	if log.Outcome == "" {
		log.Outcome = "success"
	}
	attempts := log.Attempts
	if attempts < 1 {
		attempts = 1
	}
	createdUnix := log.CreatedAt.UTC().Unix()

	tx, err := s.engine.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Honesty markers derived once, written to both the trail row and the
	// hourly rollup so a pruned trail still yields an honest chart.
	quality := usageQualityOf(log.Estimated, log.UsageKnown, log.InputTokens, log.OutputTokens)
	cost, costKnown := s.resolveRequestCost(ctx, tx, log, quality)

	res, err := tx.ExecContext(ctx, `
		INSERT INTO requests
		  (created_at, platform, model_id, endpoint_scope, key_id, status, outcome,
		   input_tokens, output_tokens, estimated, latency_ms, ttfb_ms, cost_usd,
		   cost_known, usage_quality, attempts, routed_from, class, effort,
		   error_kind, error_message)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		createdUnix, log.Platform, log.ModelID, log.EndpointScope, nullInt(log.KeyID),
		log.StatusCode, log.Outcome, log.InputTokens, log.OutputTokens, boolInt(log.Estimated),
		log.LatencyMs, nullInt(log.TTFBMs), cost, boolInt(costKnown), quality, attempts,
		nullStr(log.RequestedModel), nullEmpty(log.Class), nullEmpty(log.Effort),
		nullEmpty(log.ErrorKind), nullEmpty(log.ErrorMessage))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, a := range log.Trail {
		created := a.CreatedAt
		if created.IsZero() {
			created = log.CreatedAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO request_attempts
			  (request_id, attempt, platform, model_id, key_id, status, latency_ms,
			   error_kind, error_message, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, a.Attempt, a.Platform, a.ModelID, nullInt(a.KeyID), a.StatusCode,
			a.LatencyMs, nullEmpty(a.ErrorKind), nullEmpty(a.ErrorMessage), created.UTC().Unix()); err != nil {
			return 0, err
		}
	}

	var success, failure int64
	switch log.Outcome {
	case "success":
		success = 1
	case "error":
		failure = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO request_hourly
		  (hour_start, platform, model_id, endpoint_scope, requests, successes,
		   failures, input_tokens, output_tokens, cost_usd, latency_ms_sum,
		   estimated_requests, unavailable_requests, cost_known_requests)
		VALUES (?,?,?,?,1,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(hour_start, platform, model_id, endpoint_scope) DO UPDATE SET
		  requests             = requests + 1,
		  successes            = successes + excluded.successes,
		  failures             = failures + excluded.failures,
		  input_tokens         = input_tokens + excluded.input_tokens,
		  output_tokens        = output_tokens + excluded.output_tokens,
		  cost_usd             = cost_usd + excluded.cost_usd,
		  latency_ms_sum       = latency_ms_sum + excluded.latency_ms_sum,
		  estimated_requests   = estimated_requests + excluded.estimated_requests,
		  unavailable_requests = unavailable_requests + excluded.unavailable_requests,
		  cost_known_requests  = cost_known_requests + excluded.cost_known_requests`,
		floorHour(createdUnix), log.Platform, log.ModelID, log.EndpointScope,
		success, failure, log.InputTokens, log.OutputTokens, cost, log.LatencyMs,
		boolInt(quality == "estimated"), boolInt(quality == "unavailable"), boolInt(costKnown)); err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	if err := incrSetting(ctx, tx, "total_requests", 1, now); err != nil {
		return 0, err
	}
	if err := incrSetting(ctx, tx, "total_input_tokens", log.InputTokens, now); err != nil {
		return 0, err
	}
	if err := incrSetting(ctx, tx, "total_output_tokens", log.OutputTokens, now); err != nil {
		return 0, err
	}
	// first_request_at is set once and never pruned, so "all time" survives the
	// raw-row prune entirely.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ('first_request_at', ?, ?)
		ON CONFLICT(key) DO NOTHING`, sqliteDateTime(createdUnix), now); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// usageQualityOf maps what the dispatch captured onto the three-state honesty
// marker the accounting surface reads. Estimated counts the gateway had to
// derive are estimated; a provider that actually reported usage is exact even
// when both counts are legitimately zero (usageKnown); a call that captured no
// usage at all -- a failed hop, or a provider that reported nothing we could
// estimate -- is unavailable rather than a silent exact-zero. Positive tokens
// with no explicit marker stay exact so a caller that predates the bit is
// unaffected.
func usageQualityOf(estimated, usageKnown bool, inTok, outTok int64) string {
	switch {
	case estimated:
		return "estimated"
	case usageKnown || inTok > 0 || outTok > 0:
		return "exact"
	default:
		return "unavailable"
	}
}

// resolveRequestCost decides the monetary cost of a completed call and whether
// that cost is known. A provider-reported total (a positive log.CostUSD) is
// authoritative and always wins. Otherwise the catalog's per-million input and
// output rates for the exact (platform, model, endpoint) identity are applied
// to the recorded tokens at record time. A model with no published price, or a
// call whose usage was never captured, yields an unknown cost -- never a silent
// $0, so the surface can render it as unknown rather than free.
func (s *Server) resolveRequestCost(ctx context.Context, tx *sql.Tx, log RequestLog, quality string) (float64, bool) {
	// An explicit CostKnown (or any positive total) is an authoritative,
	// provider-reported cost and wins over the catalog-rate estimate -- even a
	// legitimately known $0.
	if log.CostKnown || log.CostUSD > 0 {
		return log.CostUSD, true
	}
	if quality == "unavailable" {
		return 0, false
	}
	var inRate, outRate sql.NullFloat64
	err := tx.QueryRowContext(ctx, `
		SELECT paid_input_per_m, paid_output_per_m FROM models
		 WHERE platform = ? AND model_id = ? AND endpoint_scope = ? LIMIT 1`,
		log.Platform, log.ModelID, log.EndpointScope).Scan(&inRate, &outRate)
	if err != nil || (log.InputTokens > 0 && !inRate.Valid) || (log.OutputTokens > 0 && !outRate.Valid) {
		return 0, false
	}
	cost := 0.0
	if inRate.Valid {
		cost += inRate.Float64 * float64(log.InputTokens) / 1_000_000
	}
	if outRate.Valid {
		cost += outRate.Float64 * float64(log.OutputTokens) / 1_000_000
	}
	return cost, true
}

// incrSetting adds delta to an integer-valued settings row, creating it at
// delta when absent. The arithmetic is one statement so it stays atomic under
// concurrent writers.
func incrSetting(ctx context.Context, tx *sql.Tx, key string, delta, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
		  value = CAST(CAST(settings.value AS INTEGER) + CAST(excluded.value AS INTEGER) AS TEXT),
		  updated_at = excluded.updated_at`,
		key, strconv.FormatInt(delta, 10), now)
	return err
}

// ── Server-log store ─────────────────────────────────────────────────────────

// serverLogRecord is one line offered to the store. Only warn/error tiers are
// durable, mirroring the reference's PERSISTED_LEVELS (server-logs.ts:38).
type serverLogRecord struct {
	Level     string
	Source    string
	Provider  string
	Model     string
	Event     string
	RequestID string
	Message   string
	CreatedAt time.Time
}

type serverLogEntry struct {
	ID        int64
	Level     string
	TSms      int64
	Source    string
	Provider  string
	Model     string
	Event     string
	RequestID string
	Message   string
}

// serverLogStore is the durable warn/error store behind the log view. Row ids
// are minted by SQLite -- server_logs.id is AUTOINCREMENT -- so they are
// assigned in commit order and their high-water mark lives in sqlite_sequence,
// which a Clear (DELETE) never lowers and a restart never forgets. The store
// holds no id counter of its own: the previous in-memory counter raced ahead
// of the committed rows under concurrency and reset to zero after a clear,
// which permanently skipped rows for any client still holding a larger cursor.
type serverLogStore struct {
	db *sql.DB
}

// logStoreFor returns a store bound to a database. The store is stateless now
// that SQLite owns the id sequence, so there is nothing to cache or share.
func logStoreFor(db *sql.DB) *serverLogStore {
	return &serverLogStore{db: db}
}

// record persists a warn/error line and returns it with the id SQLite
// assigned in commit order. Info/debug lines are not durable, so they insert
// nothing and come back with a zero id; logServerEvent, the only caller,
// discards the entry either way.
func (st *serverLogStore) record(ctx context.Context, rec serverLogRecord) (serverLogEntry, error) {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	entry := serverLogEntry{
		Level: rec.Level, TSms: rec.CreatedAt.UnixMilli(),
		Source: rec.Source, Provider: rec.Provider, Model: rec.Model,
		Event: rec.Event, RequestID: rec.RequestID, Message: rec.Message,
	}
	if rec.Level != "warn" && rec.Level != "error" {
		return entry, nil
	}
	res, err := st.db.ExecContext(ctx, `
		INSERT INTO server_logs
		  (level, source, provider, model, event, request_id, message, created_at_ms)
		VALUES (?,?,?,?,?,?,?,?)`,
		rec.Level, nullEmpty(rec.Source), nullEmpty(rec.Provider), nullEmpty(rec.Model),
		nullEmpty(rec.Event), nullEmpty(rec.RequestID), rec.Message, entry.TSms)
	if err != nil {
		return entry, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return entry, err
	}
	entry.ID = id
	return entry, nil
}

// logServerEvent persists a warn/error line for the Logs view. It writes on a
// cancel-detached context and swallows the error into slog.Debug, like the
// neighbouring RecordRequest calls, so surfacing a failure never blocks or
// fails the request that detected it. Only warn/error persist by design, so an
// info/debug caller writes nothing.
func (s *Server) logServerEvent(ctx context.Context, rec serverLogRecord) {
	if _, err := logStoreFor(s.engine.DB()).record(context.WithoutCancel(ctx), rec); err != nil {
		slog.Debug("Could not record server log", "event", rec.Event, "error", err)
	}
}

// maxID is the id high-water mark a poller echoes as sinceId. It reads the
// committed maximum, so it never runs ahead of a row that has not reached disk
// -- the concurrency skip the in-memory counter allowed -- and because
// AUTOINCREMENT never reuses an id it can only move a caught-up cursor forward.
// It ignores level and filter so a poll whose matches were all excluded still
// advances past them instead of rescanning the same tail forever.
func (st *serverLogStore) maxID(ctx context.Context) int64 {
	var max sql.NullInt64
	_ = st.db.QueryRowContext(ctx, `SELECT MAX(id) FROM server_logs`).Scan(&max)
	return max.Int64
}

func (st *serverLogStore) query(ctx context.Context, sinceID *int64, levels []string, needle, provider string, limit int) ([]serverLogEntry, error) {
	// A caller already caught up costs one comparison, not a scan.
	if sinceID != nil && *sinceID >= st.maxID(ctx) {
		return nil, nil
	}
	conds := []string{}
	var args []any
	if sinceID != nil {
		conds = append(conds, "id > ?")
		args = append(args, *sinceID)
	}
	if len(levels) > 0 {
		conds = append(conds, "level IN ("+placeholders(len(levels))+")")
		for _, l := range levels {
			args = append(args, l)
		}
	}
	if provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, provider)
	}
	if needle != "" {
		like := "%" + strings.ToLower(needle) + "%"
		conds = append(conds, "(lower(message) LIKE ? OR lower(COALESCE(provider,'')) LIKE ? OR lower(COALESCE(source,'')) LIKE ? OR lower(COALESCE(event,'')) LIKE ?)")
		args = append(args, like, like, like, like)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)

	rows, err := st.db.QueryContext(ctx, `
		SELECT id, level, source, provider, model, event, request_id, message, created_at_ms
		  FROM server_logs`+where+`
		 ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	entries := []serverLogEntry{}
	for rows.Next() {
		var (
			e                               serverLogEntry
			level, source, providerN, model sql.NullString
			event, requestID                sql.NullString
		)
		if err := rows.Scan(&e.ID, &level, &source, &providerN, &model, &event, &requestID, &e.Message, &e.TSms); err != nil {
			continue
		}
		e.Level = level.String
		e.Source, e.Provider, e.Model = source.String, providerN.String, model.String
		e.Event, e.RequestID = event.String, requestID.String
		entries = append(entries, e)
	}
	// Walked newest-first for the LIMIT; hand them back oldest->newest.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// clear empties the durable rows. Because id is AUTOINCREMENT, DELETE leaves
// sqlite_sequence untouched, so the id high-water mark survives: a client
// still holding a cursor is never handed a reused id, and ids minted after the
// clear continue past everything it has already seen (server-logs.ts:473-474).
func (st *serverLogStore) clear(ctx context.Context) error {
	_, err := st.db.ExecContext(ctx, `DELETE FROM server_logs`)
	return err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
