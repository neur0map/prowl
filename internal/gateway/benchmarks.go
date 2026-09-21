package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	benchmarkSource          = "Artificial Analysis"
	benchmarkAPIKeyEnv       = "ARTIFICIAL_ANALYSIS_API_KEY"
	benchmarkRefreshInterval = 24 * time.Hour
	benchmarkRetryInterval   = 6 * time.Hour
	benchmarkMaxResponse     = 8 << 20
	benchmarkMaxPages        = 20
)

var benchmarkEndpoint = "https://artificialanalysis.ai/api/v2/language/models/free"

var benchmarkHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var ErrBenchmarkKeyMissing = errors.New("benchmark refresh needs ARTIFICIAL_ANALYSIS_API_KEY")

// BenchmarkRefreshStatus is safe to expose in the local console. The API key is
// deliberately absent; Prowl reads it from the process environment and never
// persists or echoes it.
type BenchmarkRefreshStatus struct {
	Source       string     `json:"source"`
	Configured   bool       `json:"configured"`
	Status       string     `json:"status"`
	LastAttempt  *time.Time `json:"lastAttempt,omitempty"`
	LastSuccess  *time.Time `json:"lastSuccess,omitempty"`
	Matched      int        `json:"matched"`
	Available    int        `json:"available"`
	Error        string     `json:"error,omitempty"`
	IndexVersion float64    `json:"indexVersion,omitempty"`
	Attribution  string     `json:"attribution"`
}

// BenchmarkStatus reports provenance and freshness without contacting the
// network.
func (e *Engine) BenchmarkStatus(ctx context.Context) BenchmarkRefreshStatus {
	status := BenchmarkRefreshStatus{
		Source:      benchmarkSource,
		Configured:  strings.TrimSpace(os.Getenv(benchmarkAPIKeyEnv)) != "",
		Status:      "not refreshed",
		Attribution: "Benchmark data from Artificial Analysis (artificialanalysis.ai)",
	}
	var attempt, success sql.NullInt64
	if err := e.DB().QueryRowContext(ctx, `
		SELECT status, last_attempt, last_success, matched, available,
		       COALESCE(error, ''), COALESCE(index_version, 0)
		  FROM benchmark_refresh_state WHERE source = ?`, benchmarkSource).Scan(
		&status.Status, &attempt, &success, &status.Matched, &status.Available,
		&status.Error, &status.IndexVersion,
	); err != nil {
		return status
	}
	if attempt.Valid {
		value := time.Unix(attempt.Int64, 0).UTC()
		status.LastAttempt = &value
	}
	if success.Valid {
		value := time.Unix(success.Int64, 0).UTC()
		status.LastSuccess = &value
	}
	return status
}

// RefreshBenchmarks fetches every page of the free headline-index endpoint,
// conservatively matches external model identities to local catalogue rows, and
// replaces only matched scores in one transaction. Existing scores survive a
// failed fetch.
func (e *Engine) RefreshBenchmarks(ctx context.Context) (BenchmarkRefreshStatus, error) {
	key := strings.TrimSpace(os.Getenv(benchmarkAPIKeyEnv))
	if key == "" {
		return e.BenchmarkStatus(ctx), ErrBenchmarkKeyMissing
	}
	now := time.Now().UTC()
	models, version, err := fetchBenchmarkModels(ctx, key)
	if err != nil {
		e.recordBenchmarkRefresh(ctx, now, nil, 0, 0, 0, err)
		return e.BenchmarkStatus(ctx), err
	}
	matched, err := persistBenchmarkModels(ctx, e.DB(), now, version, models)
	if err != nil {
		e.recordBenchmarkRefresh(ctx, now, nil, matched, len(models), version, err)
		return e.BenchmarkStatus(ctx), err
	}
	e.recordBenchmarkRefresh(ctx, now, &now, matched, len(models), version, nil)
	return e.BenchmarkStatus(ctx), nil
}

func (e *Engine) recordBenchmarkRefresh(ctx context.Context, attempt time.Time, success *time.Time, matched, available int, version float64, refreshErr error) {
	status := "current"
	errorText := ""
	if refreshErr != nil {
		status = "error"
		errorText = refreshErr.Error()
		if len(errorText) > 500 {
			errorText = errorText[:500]
		}
	}
	var successUnix any
	if success != nil {
		successUnix = success.Unix()
	}
	_, _ = e.DB().ExecContext(ctx, `
		INSERT INTO benchmark_refresh_state
		  (source, status, last_attempt, last_success, matched, available, error, index_version)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)
		ON CONFLICT(source) DO UPDATE SET
		  status = excluded.status,
		  last_attempt = excluded.last_attempt,
		  last_success = COALESCE(excluded.last_success, benchmark_refresh_state.last_success),
		  matched = CASE WHEN excluded.last_success IS NULL THEN benchmark_refresh_state.matched ELSE excluded.matched END,
		  available = CASE WHEN excluded.last_success IS NULL THEN benchmark_refresh_state.available ELSE excluded.available END,
		  error = excluded.error,
		  index_version = CASE WHEN excluded.last_success IS NULL THEN benchmark_refresh_state.index_version ELSE excluded.index_version END`,
		benchmarkSource, status, attempt.Unix(), successUnix, matched, available, errorText, version)
}

// LoadBenchmarkScores returns only the requested local ids. A missing row means
// the smart scorer should use the catalogue capability prior.
func LoadBenchmarkScores(ctx context.Context, db *sql.DB, entries []ChainEntry) map[int64]BenchmarkScores {
	result := make(map[int64]BenchmarkScores)
	if len(entries) == 0 {
		return result
	}
	ids := make([]string, 0, len(entries))
	seen := make(map[int64]bool, len(entries))
	for _, entry := range entries {
		if seen[entry.ModelDBID] {
			continue
		}
		seen[entry.ModelDBID] = true
		ids = append(ids, strconv.FormatInt(entry.ModelDBID, 10))
	}
	rows, err := db.QueryContext(ctx, `
		SELECT model_db_id, COALESCE(intelligence, 0), COALESCE(coding, 0),
		       COALESCE(agentic, 0), COALESCE(math, 0), COALESCE(multilingual, 0),
		       source, refreshed_at
		  FROM model_benchmarks
		 WHERE model_db_id IN (`+strings.Join(ids, ",")+`)`)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var id, refreshed int64
		var score BenchmarkScores
		if rows.Scan(&id, &score.Intelligence, &score.Coding, &score.Agentic,
			&score.Math, &score.Multilingual, &score.Source, &refreshed) != nil {
			continue
		}
		score.Fresh = time.Since(time.Unix(refreshed, 0)) <= 7*24*time.Hour
		result[id] = score
	}
	return result
}

type externalBenchmarkModel struct {
	ID           string
	Name         string
	Slug         string
	Intelligence float64
	Coding       float64
	Agentic      float64
	Math         float64
	Multilingual float64
}

type benchmarkEnvelope struct {
	IntelligenceIndexVersion float64 `json:"intelligence_index_version"`
	Pagination               struct {
		HasMore    bool `json:"has_more"`
		TotalPages int  `json:"total_pages"`
	} `json:"pagination"`
	Data []struct {
		ID          string                     `json:"id"`
		Name        string                     `json:"name"`
		Slug        string                     `json:"slug"`
		Evaluations map[string]json.RawMessage `json:"evaluations"`
	} `json:"data"`
}

func fetchBenchmarkModels(ctx context.Context, key string) ([]externalBenchmarkModel, float64, error) {
	var (
		all     []externalBenchmarkModel
		version float64
	)
	for page := 1; page <= benchmarkMaxPages; page++ {
		separator := "?"
		if strings.Contains(benchmarkEndpoint, "?") {
			separator = "&"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			benchmarkEndpoint+separator+"page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("x-api-key", key)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "prowl benchmark-refresh")
		resp, err := benchmarkHTTPClient.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("fetch %s benchmarks: %w", benchmarkSource, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, benchmarkMaxResponse+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, 0, readErr
		}
		if len(body) > benchmarkMaxResponse {
			return nil, 0, fmt.Errorf("%s benchmark response exceeds %d bytes", benchmarkSource, benchmarkMaxResponse)
		}
		if resp.StatusCode != http.StatusOK {
			var failure struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(body, &failure)
			if failure.Error == "" {
				failure.Error = http.StatusText(resp.StatusCode)
			}
			return nil, 0, fmt.Errorf("%s benchmark API returned HTTP %d: %s", benchmarkSource, resp.StatusCode, failure.Error)
		}
		var envelope benchmarkEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, 0, fmt.Errorf("decode %s benchmarks: %w", benchmarkSource, err)
		}
		if version == 0 {
			version = envelope.IntelligenceIndexVersion
		}
		for _, model := range envelope.Data {
			all = append(all, externalBenchmarkModel{
				ID: model.ID, Name: model.Name, Slug: model.Slug,
				Intelligence: normalizedEvaluation(model.Evaluations, "artificial_analysis_intelligence_index"),
				Coding:       normalizedEvaluation(model.Evaluations, "artificial_analysis_coding_index"),
				Agentic:      normalizedEvaluation(model.Evaluations, "artificial_analysis_agentic_index"),
				Math:         firstEvaluation(model.Evaluations, "artificial_analysis_math_index", "aime_25", "aime_24", "math_500"),
				Multilingual: normalizedEvaluation(model.Evaluations, "artificial_analysis_multilingual_index"),
			})
		}
		if !envelope.Pagination.HasMore || (envelope.Pagination.TotalPages > 0 && page >= envelope.Pagination.TotalPages) {
			return all, version, nil
		}
	}
	return nil, 0, fmt.Errorf("%s benchmark pagination exceeded %d pages", benchmarkSource, benchmarkMaxPages)
}

func normalizedEvaluation(values map[string]json.RawMessage, key string) float64 {
	raw, ok := values[key]
	if !ok || string(raw) == "null" {
		return 0
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || value <= 0 {
		return 0
	}
	if value > 1 {
		value /= 100
	}
	return clamp01(value)
}

func firstEvaluation(values map[string]json.RawMessage, keys ...string) float64 {
	for _, key := range keys {
		if value := normalizedEvaluation(values, key); value > 0 {
			return value
		}
	}
	return 0
}

func persistBenchmarkModels(ctx context.Context, db *sql.DB, refreshed time.Time, version float64, external []externalBenchmarkModel) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, model_id, display_name FROM models`)
	if err != nil {
		return 0, err
	}
	type localModel struct {
		id          int64
		modelID     string
		displayName string
	}
	var locals []localModel
	for rows.Next() {
		var model localModel
		if err := rows.Scan(&model.id, &model.modelID, &model.displayName); err != nil {
			_ = rows.Close()
			return 0, err
		}
		locals = append(locals, model)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	matcher := newBenchmarkMatcher(external)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	matched := 0
	for _, local := range locals {
		model, ok := matcher.match(local.modelID, local.displayName)
		if !ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_benchmarks
			  (model_db_id, source, source_model_id, source_model_name, index_version,
			   intelligence, coding, agentic, math, multilingual, refreshed_at)
			VALUES (?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0), ?)
			ON CONFLICT(model_db_id) DO UPDATE SET
			  source = excluded.source,
			  source_model_id = excluded.source_model_id,
			  source_model_name = excluded.source_model_name,
			  index_version = excluded.index_version,
			  intelligence = excluded.intelligence,
			  coding = excluded.coding,
			  agentic = excluded.agentic,
			  math = excluded.math,
			  multilingual = excluded.multilingual,
			  refreshed_at = excluded.refreshed_at`,
			local.id, benchmarkSource, model.ID, model.Name, version,
			model.Intelligence, model.Coding, model.Agentic, model.Math, model.Multilingual,
			refreshed.Unix()); err != nil {
			return matched, err
		}
		matched++
	}
	if err := tx.Commit(); err != nil {
		return matched, err
	}
	return matched, nil
}

type benchmarkMatcher struct {
	byCompact   map[string][]externalBenchmarkModel
	bySignature map[string][]externalBenchmarkModel
}

func newBenchmarkMatcher(models []externalBenchmarkModel) benchmarkMatcher {
	matcher := benchmarkMatcher{
		byCompact:   make(map[string][]externalBenchmarkModel),
		bySignature: make(map[string][]externalBenchmarkModel),
	}
	for _, model := range models {
		for _, name := range []string{model.Slug, model.Name} {
			compact := normalizeModelName(name)
			if compact != "" {
				matcher.byCompact[compact] = appendUniqueBenchmark(matcher.byCompact[compact], model)
			}
			signature := modelSignature(name)
			if signature != "" {
				matcher.bySignature[signature] = appendUniqueBenchmark(matcher.bySignature[signature], model)
			}
		}
	}
	return matcher
}

func (m benchmarkMatcher) match(names ...string) (externalBenchmarkModel, bool) {
	for _, name := range names {
		if models := m.byCompact[normalizeModelName(name)]; len(models) == 1 {
			return models[0], true
		}
	}
	for _, name := range names {
		if models := m.bySignature[modelSignature(name)]; len(models) == 1 {
			return models[0], true
		}
	}
	return externalBenchmarkModel{}, false
}

func appendUniqueBenchmark(models []externalBenchmarkModel, candidate externalBenchmarkModel) []externalBenchmarkModel {
	for _, model := range models {
		if model.ID == candidate.ID {
			return models
		}
	}
	return append(models, candidate)
}

func modelSignature(value string) string {
	value = strings.ToLower(value)
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		value = value[slash+1:]
	}
	if paren := strings.IndexByte(value, '('); paren >= 0 {
		value = value[:paren]
	}
	var tokens []string
	start := -1
	for index, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = index
			}
			continue
		}
		if start >= 0 {
			tokens = appendModelToken(tokens, value[start:index])
			start = -1
		}
	}
	if start >= 0 {
		tokens = appendModelToken(tokens, value[start:])
	}
	sort.Strings(tokens)
	return strings.Join(tokens, ":")
}

func appendModelToken(tokens []string, token string) []string {
	switch token {
	case "latest", "preview", "chat", "instruct", "model", "api":
		return tokens
	}
	if len(token) == 8 && strings.HasPrefix(token, "20") {
		if _, err := strconv.Atoi(token); err == nil {
			return tokens
		}
	}
	return append(tokens, token)
}
