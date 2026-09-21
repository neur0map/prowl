package context

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/store"
)

// Service compiles the same packet for every transport without a model.
type Service struct {
	Store     *store.Store
	Knowledge *knowledge.Repository
	Root      string
	Estimator CostEstimator
	Tracer    Tracer
	Reranker  SemanticReranker
	// RequirePublished rejects structural reads while a failed refresh is
	// awaiting repair. Low-level callers remain compatible by leaving it false.
	RequirePublished bool
	ReadGuard        store.ReadGuard
}

func (service *Service) beginRead() (func(), error) {
	if service.ReadGuard == nil {
		return func() {}, nil
	}
	return service.ReadGuard(context.Background())
}

// Search retrieves, ranks, and packs curated knowledge plus raw source evidence.
func (service *Service) Search(request Request) (packet Packet, err error) {
	release, err := service.beginRead()
	if err != nil {
		return Packet{}, err
	}
	defer release()
	started := time.Now()
	defer service.recordTrace(request, &packet, &err, started)
	if service.RequirePublished {
		if err := service.Store.RequirePublishedGeneration(); err != nil {
			return Packet{}, err
		}
	}
	if strings.TrimSpace(request.Question) == "" {
		return Packet{}, fmt.Errorf("context search question is required")
	}
	if err := request.Validate(); err != nil {
		return Packet{}, err
	}
	curated, err := knowledgeCandidates(service.Knowledge, service.Root, request.Question)
	if err != nil {
		return Packet{}, err
	}
	sources, err := sourceCandidates(service.Store, request.Question, 40)
	if err != nil {
		return Packet{}, err
	}
	graph, err := graphCandidates(service.Store, sources, 8)
	if err != nil {
		return Packet{}, err
	}
	candidates := append(append(curated, sources...), graph...)
	covered := map[string]bool{}
	for _, candidate := range candidates {
		if len(candidate.Citations) > 0 {
			covered[candidate.Citations[0].Path] = true
		}
	}
	paths, err := pathCandidates(service.Store, request.Question, covered, 8)
	if err != nil {
		return Packet{}, err
	}
	candidates = append(candidates, paths...)
	applySymbolMatch(candidates, service.Store, request.Question)
	applyPathMatch(candidates, request.Question)
	reranker := service.Reranker
	if request.Reranker != nil {
		reranker = request.Reranker
	}
	candidates = applySemanticScores(request.Question, candidates, reranker)
	packet, err = Pack(request, candidates, service.Estimator)
	if err != nil {
		return Packet{}, err
	}
	packet.TraceID = newTraceID()
	return packet, nil
}

// Get fetches selected IDs with the same budget and packet contract.
func (service *Service) Get(request Request) (packet Packet, err error) {
	release, err := service.beginRead()
	if err != nil {
		return Packet{}, err
	}
	defer release()
	started := time.Now()
	defer service.recordTrace(request, &packet, &err, started)
	if service.RequirePublished {
		if err := service.Store.RequirePublishedGeneration(); err != nil {
			return Packet{}, err
		}
	}
	if len(request.IDs) == 0 {
		return Packet{}, fmt.Errorf("context get requires at least one id")
	}
	if err := request.Validate(); err != nil {
		return Packet{}, err
	}
	allKnowledge, err := knowledgeCandidates(service.Knowledge, service.Root, "")
	if err != nil {
		return Packet{}, err
	}
	knowledgeByID := map[string]Candidate{}
	for _, candidate := range allKnowledge {
		knowledgeByID[candidate.ID] = candidate
	}
	var candidates []Candidate
	missing := 0
	for _, id := range request.IDs {
		if candidate, ok := knowledgeByID[id]; ok {
			candidate.DirectMatch = true
			candidates = append(candidates, candidate)
			continue
		}
		candidate, ok, err := service.sourceByID(id)
		if err != nil {
			return Packet{}, err
		}
		if !ok {
			missing++
			continue
		}
		candidate.DirectMatch = true
		candidates = append(candidates, candidate)
	}
	packet, err = Pack(request, candidates, service.Estimator)
	if err != nil {
		return Packet{}, err
	}
	if missing > 0 {
		packet.Omitted["not_found"] = missing
	}
	packet.TraceID = newTraceID()
	return packet, nil
}

func (service *Service) recordTrace(request Request, packet *Packet, operationErr *error, started time.Time) {
	if service.Tracer == nil {
		return
	}
	if packet.TraceID == "" {
		packet.TraceID = newTraceID()
	}
	status, errorCode := "success", ""
	if *operationErr != nil {
		status, errorCode = "error", "context_error"
	}
	_ = service.Tracer.Record(TraceEvent{Request: request, Packet: *packet, Status: status, ErrorCode: errorCode, Duration: time.Since(started)})
}

func (service *Service) sourceByID(id string) (Candidate, bool, error) {
	if service.Store == nil || !strings.HasPrefix(id, "source:") {
		return Candidate{}, false, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, "source:"))
	if err != nil {
		return Candidate{}, false, nil
	}
	value := string(decoded)
	separator := strings.LastIndex(value, ":")
	if separator < 1 {
		return Candidate{}, false, nil
	}
	line, err := strconv.Atoi(value[separator+1:])
	if err != nil {
		return Candidate{}, false, nil
	}
	chunk, ok, err := service.Store.ChunkAt(filepath.ToSlash(value[:separator]), line)
	if err != nil || !ok {
		return Candidate{}, ok, err
	}
	end := citationEndLine(chunk.StartLine, chunk.Text)
	return Candidate{
		Item: Item{
			ID: id, Kind: "source", Title: filepath.ToSlash(chunk.File), Summary: firstParagraph([]byte(chunk.Text)),
			WhySelected: []string{"selected source ID"}, Freshness: "current", Confidence: 1,
			Audience:       []string{"assistant", "user"},
			Citations:      []Citation{{URI: sourceResourceURI(chunk.File), Path: filepath.ToSlash(chunk.File), LineStart: chunk.StartLine, LineEnd: end}},
			DetailResource: sourceResourceURI(chunk.File),
		},
		CompactContent: firstParagraph([]byte(chunk.Text)), StandardContent: chunk.Text, FullContent: chunk.Text,
	}, true, nil
}

func newTraceID() string {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "trace-unavailable"
	}
	return hex.EncodeToString(data[:])
}
