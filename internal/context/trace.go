package context

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/neur0map/prowl/internal/store"
)

const (
	traceHashVersion = "hmac-sha256-v1"
	traceRetention   = 30 * 24 * time.Hour
)

// TraceEvent exists only in memory; the store adapter hashes Question before
// creating a persistence record.
type TraceEvent struct {
	Request   Request
	Packet    Packet
	Status    string
	ErrorCode string
	Duration  time.Duration
}

// Tracer records privacy-safe context execution metadata.
type Tracer interface {
	Record(TraceEvent) error
}

// StoreTracer persists traces in the workspace database.
type StoreTracer struct{ Store *store.Store }

func (tracer StoreTracer) Record(event TraceEvent) error {
	if tracer.Store == nil {
		return nil
	}
	// Retention cleanup is best-effort telemetry maintenance. It must never make
	// retrieval fail or prevent the current aggregate record from being stored.
	_, _ = tracer.Store.PruneContextRuns(time.Now().Add(-traceRetention))
	salt, err := tracer.Store.ContextTraceSalt()
	if err != nil {
		return err
	}
	query := event.Request.Question
	if query == "" {
		ids, _ := json.Marshal(event.Request.IDs)
		query = string(ids)
	}
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write([]byte(query))
	selected := make([]string, 0, len(event.Packet.Items))
	for _, item := range event.Packet.Items {
		selected = append(selected, item.ID)
	}
	selectedJSON, _ := json.Marshal(selected)
	omissionsJSON, _ := json.Marshal(event.Packet.Omitted)
	timingsJSON, _ := json.Marshal(map[string]int64{"total_ms": event.Duration.Milliseconds()})
	return tracer.Store.SaveContextRun(store.ContextRun{
		ID: event.Packet.TraceID, QueryHash: hex.EncodeToString(mac.Sum(nil)), HashVersion: traceHashVersion,
		Mode: string(event.Request.Mode), BudgetTokens: event.Request.BudgetTokens, BudgetBytes: event.Request.BudgetBytes,
		EstimatedTokens: event.Packet.Budget.EstimatedTokens, EstimatedBytes: event.Packet.Budget.EstimatedBytes,
		SelectedIDsJSON: string(selectedJSON), OmissionsJSON: string(omissionsJSON), TimingsJSON: string(timingsJSON),
		StrategyVersion: "lexical-graph-v1", Status: event.Status, ErrorCode: event.ErrorCode,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}
