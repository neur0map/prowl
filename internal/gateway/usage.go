package gateway

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Usage accounting answers one question the user asked for directly: is

// Record is one attempt against one provider.
type Record struct {
	At        time.Time `json:"at"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	LatencyMs int64     `json:"latency_ms"`
	CostUSD   float64   `json:"cost_usd"`

	// Attempt is 1 for the first provider tried, 2 for the first fallback,
	// and so on. Anything above 1 is failover in action.
	Attempt int `json:"attempt"`

	// RoutedFrom names the provider that failed, when this attempt exists
	// because an earlier one did not work.
	RoutedFrom string `json:"routed_from,omitempty"`

	// Class and Effort record what smart routing decided, so a user can see
	// the cheap tier and reasoning budget being chosen.
	Class  string `json:"class,omitempty"`
	Effort string `json:"effort,omitempty"`

	// Estimated is set when the upstream omitted a usage object and the token
	// counts are derived rather than reported. Without it a missing usage
	// frame would credit zero tokens and quietly disable the TPM window.
	Estimated bool `json:"estimated,omitempty"`

	Error string `json:"error,omitempty"`
}

// Failed reports whether the attempt did not succeed.
func (r Record) Failed() bool { return r.Status == 0 || r.Status >= 400 }

// ProviderTotals aggregates one provider's traffic.
type ProviderTotals struct {
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	CostUSD      float64 `json:"cost_usd"`
	Failures     int     `json:"failures"`
	AvgLatencyMs int64   `json:"avg_latency_ms"`

	latencySum int64
}

// Totals aggregates everything.
type Totals struct {
	Requests  int     `json:"requests"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Failures  int     `json:"failures"`
	Failovers int     `json:"failovers"`
}

// UsageReport is the dashboard payload.
type UsageReport struct {
	Totals     Totals           `json:"totals"`
	ByProvider []ProviderTotals `json:"by_provider"`
	Recent     []Record         `json:"recent"`
}

const (
	usageFileName = "usage.jsonl"
	// recentCap bounds the in-memory tail. The dashboard shows a log, not an
	// archive; the file keeps the full history.
	recentCap = 500
)

// UsageLog records attempts and serves aggregates.
type UsageLog struct {
	path string

	mu     sync.Mutex
	recent []Record
	file   *os.File
}

// OpenUsageLog opens (or creates) the log in dir and loads the recent tail so
// the dashboard is populated immediately after a restart.
func OpenUsageLog(dir string) (*UsageLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, usageFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	log := &UsageLog{path: path, file: file}
	log.loadTail()
	return log, nil
}

// loadTail reads the existing file into the bounded in-memory window. A
// corrupt line is skipped rather than fatal: a truncated final write must not
// stop the gateway from starting.
func (l *UsageLog) loadTail() {
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		l.recent = append(l.recent, rec)
		if len(l.recent) > recentCap {
			l.recent = l.recent[len(l.recent)-recentCap:]
		}
	}
}

// Append records one attempt.
func (l *UsageLog) Append(rec Record) {
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = append(l.recent, rec)
	if len(l.recent) > recentCap {
		l.recent = l.recent[len(l.recent)-recentCap:]
	}
	if l.file != nil {
		// A failed write must not break the request being served; the
		// in-memory tail still has the record.
		_, _ = l.file.Write(append(blob, '\n'))
	}
}

// Report aggregates the recorded attempts.
func (l *UsageLog) Report() UsageReport {
	l.mu.Lock()
	records := slices.Clone(l.recent)
	l.mu.Unlock()

	report := UsageReport{ByProvider: []ProviderTotals{}, Recent: []Record{}}
	byProvider := map[string]*ProviderTotals{}
	counted := map[string]int{}

	for _, rec := range records {
		report.Totals.Requests++
		report.Totals.TokensIn += rec.TokensIn
		report.Totals.TokensOut += rec.TokensOut
		report.Totals.CostUSD += rec.CostUSD
		if rec.Failed() {
			report.Totals.Failures++
		}
		if rec.Attempt > 1 || rec.RoutedFrom != "" {
			report.Totals.Failovers++
		}

		p := byProvider[rec.Provider]
		if p == nil {
			p = &ProviderTotals{Provider: rec.Provider}
			byProvider[rec.Provider] = p
		}
		p.Requests++
		p.TokensIn += rec.TokensIn
		p.TokensOut += rec.TokensOut
		p.CostUSD += rec.CostUSD
		if rec.Failed() {
			p.Failures++
		}
		if rec.LatencyMs > 0 {
			p.latencySum += rec.LatencyMs
			counted[rec.Provider]++
		}
	}

	for provider, totals := range byProvider {
		if n := counted[provider]; n > 0 {
			totals.AvgLatencyMs = totals.latencySum / int64(n)
		}
		report.ByProvider = append(report.ByProvider, *totals)
	}
	slices.SortFunc(report.ByProvider, func(a, b ProviderTotals) int {
		if a.Requests != b.Requests {
			return b.Requests - a.Requests
		}
		return strings.Compare(a.Provider, b.Provider)
	})

	// Newest first: the dashboard log reads top-down.
	for i := len(records) - 1; i >= 0 && len(report.Recent) < 100; i-- {
		report.Recent = append(report.Recent, records[i])
	}
	return report
}

// Close releases the log file.
func (l *UsageLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
