package context

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/query"
	"github.com/neur0map/prowl/internal/store"
)

type retrievalCase struct {
	Name             string   `json:"name"`
	Question         string   `json:"question"`
	ExpectedSources  []string `json:"expected_sources"`
	ExpectedConcepts []string `json:"expected_concepts"`
	Distractors      []string `json:"distractors"`
	BudgetTokens     int      `json:"budget_tokens"`
	RequiredTerms    []string `json:"required_terms"`
}

type evaluationResult struct {
	IDs             map[string]bool
	Text            string
	EstimatedTokens int
	ToolCalls       int
}

type evaluationMetrics struct {
	HitRate          float64
	Precision        float64
	Utilization      float64
	EvidenceComplete bool
	BudgetAdherent   bool
	ToolCalls        int
}

type evaluationFixture struct {
	root    string
	store   *store.Store
	service *Service
	cases   []retrievalCase
}

func TestRetrievalEvaluationImprovesHitsUnderFixedBudget(t *testing.T) {
	fixture := newEvaluationFixture(t)
	defer fixture.store.Close()
	strategies := []string{"grep", "existing-prowl", "lexical", "graph", "hybrid"}
	averages := map[string]evaluationMetrics{}
	for _, strategy := range strategies {
		var aggregate evaluationMetrics
		complete, calls := 0, 0
		for _, testCase := range fixture.cases {
			result, err := fixture.run(strategy, testCase)
			if err != nil {
				t.Fatalf("%s/%s: %v", strategy, testCase.Name, err)
			}
			metrics := scoreEvaluation(testCase, result)
			if !metrics.BudgetAdherent {
				t.Fatalf("%s/%s exceeded budget: %+v", strategy, testCase.Name, result)
			}
			if strategy == "hybrid" {
				packet, err := fixture.service.Search(Request{Question: testCase.Question, Mode: ModeCompact, BudgetTokens: testCase.BudgetTokens})
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range packet.Items {
					if len(item.WhySelected) == 0 || item.Freshness == "" || len(item.Citations) == 0 || item.DetailResource == "" {
						t.Fatalf("hybrid item lacks explanation metadata: %+v", item)
					}
				}
				if packet.Omitted == nil {
					t.Fatal("hybrid packet omitted accounting is nil")
				}
			}
			aggregate.HitRate += metrics.HitRate
			aggregate.Precision += metrics.Precision
			aggregate.Utilization += metrics.Utilization
			if metrics.EvidenceComplete {
				complete++
			}
			calls += metrics.ToolCalls
			aggregate.BudgetAdherent = true
			t.Logf("%-15s %-34s recall=%.2f precision=%.2f utilization=%.2f complete=%v operations=%d tokens=%d", strategy, testCase.Name, metrics.HitRate, metrics.Precision, metrics.Utilization, metrics.EvidenceComplete, metrics.ToolCalls, result.EstimatedTokens)
		}
		count := float64(len(fixture.cases))
		aggregate.HitRate /= count
		aggregate.Precision /= count
		aggregate.Utilization /= count
		aggregate.EvidenceComplete = complete == len(fixture.cases)
		aggregate.ToolCalls = calls / len(fixture.cases)
		averages[strategy] = aggregate
	}
	if averages["hybrid"].HitRate <= averages["existing-prowl"].HitRate {
		t.Fatalf("hybrid hit rate %.2f did not improve existing Prowl %.2f", averages["hybrid"].HitRate, averages["existing-prowl"].HitRate)
	}
	if averages["hybrid"].Utilization <= averages["existing-prowl"].Utilization || !averages["hybrid"].EvidenceComplete {
		t.Fatalf("hybrid did not improve evidence completeness: hybrid=%+v existing=%+v", averages["hybrid"], averages["existing-prowl"])
	}
	if averages["hybrid"].Precision <= averages["existing-prowl"].Precision {
		t.Fatalf("hybrid precision %.2f did not improve existing Prowl %.2f", averages["hybrid"].Precision, averages["existing-prowl"].Precision)
	}
	if averages["hybrid"].ToolCalls >= averages["grep"].ToolCalls {
		t.Fatalf("hybrid calls %d did not improve grep exploration %d", averages["hybrid"].ToolCalls, averages["grep"].ToolCalls)
	}
}

func BenchmarkRetrievalStrategies(b *testing.B) {
	fixture := newEvaluationFixture(b)
	defer fixture.store.Close()
	for _, strategy := range []string{"grep", "existing-prowl", "lexical", "graph", "hybrid"} {
		b.Run(strategy, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := fixture.run(strategy, fixture.cases[i%len(fixture.cases)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func newEvaluationFixture(tb testing.TB) *evaluationFixture {
	tb.Helper()
	corpusData, err := os.ReadFile(filepath.Join("testdata", "retrieval-corpus.json"))
	if err != nil {
		tb.Fatal(err)
	}
	var cases []retrievalCase
	if err := json.Unmarshal(corpusData, &cases); err != nil {
		tb.Fatal(err)
	}
	root := tb.TempDir()
	files := map[string]string{
		"go.mod":                   "module fixture\n\ngo 1.25\n",
		"auth/main.go":             "package auth\nimport \"fixture/policy\"\nfunc Authenticate() bool { return policy.Allowed() }\n",
		"policy/policy.go":         "package policy\n// policy rule denies suspended users\nfunc Allowed() bool { return true }\n",
		"migration/run.go":         "package migration\nimport \"fixture/schema\"\nfunc ApplyMigration() int { return schema.Current() }\n",
		"schema/schema.go":         "package schema\n// schema version is checked before writes\nfunc Current() int { return 3 }\n",
		"cache/invalidate.go":      "package cache\nimport \"fixture/storage\"\nfunc InvalidateCache() int { return storage.Epoch() }\n",
		"storage/storage.go":       "package storage\n// storage epoch prevents stale cache reads\nfunc Epoch() int { return 7 }\n",
		"noise/auth_notes.go":      "package noise\n// Authenticate Authenticate Authenticate tutorial only\n",
		"noise/migration_notes.go": "package noise\n// ApplyMigration ApplyMigration ApplyMigration tutorial only\n",
		"noise/cache_notes.go":     "package noise\n// InvalidateCache InvalidateCache InvalidateCache tutorial only\n",
		"bar/battery.go":           "package bar\n// BatteryIndicator displays the battery charge level on the status bar.\nfunc BatteryIndicator() string { return \"battery charge level shown on the status bar\" }\n",
		"i18n/catalog_gen.go":      "package i18n\n// Code generated from locale sources. DO NOT EDIT.\n// battery battery battery charge charge charge level level level status status status bar bar bar display display display the the the\nvar Catalog = map[string]string{\"battery\": \"bateria\", \"charge\": \"carga\", \"level\": \"nivel\", \"status\": \"estado\", \"bar\": \"barra\", \"display\": \"mostrar\"}\n",
		"render/pipeline.go":       "package render\n// ComputeFrame renders one frame in the render loop and returns its id.\nfunc ComputeFrame() int { return sceneFrame() }\nfunc sceneFrame() int { return 1 }\n",
		"guide/frame_notes.go":     "package guide\n// how the frame is computed: the render loop draws each frame; frame timing and frame pacing across the render loop\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	database, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := index.IndexWithOptions(database, root, index.Options{Languages: []string{"go"}}); err != nil {
		database.Close()
		tb.Fatal(err)
	}
	repository := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repository.Init(); err != nil {
		database.Close()
		tb.Fatal(err)
	}
	documents := []struct{ path, title, id, description, body string }{
		{"authentication.md", "Authentication", "authentication", "Authenticate requests through the policy rule, which denies suspended users.", "The policy rule is authoritative for suspended users."},
		{"migrations.md", "Migrations", "migrations", "ApplyMigration only after checking schema version.", "The schema version protects stored data."},
		{"cache.md", "Cache invalidation", "cache-invalidation", "InvalidateCache by advancing the storage epoch.", "The storage epoch prevents stale reads."},
	}
	for _, value := range documents {
		content := fmt.Sprintf("---\ntype: Concept\ntitle: %s\ndescription: %s\nprowl:\n  id: %s\n  confidence: verified\n---\n%s\n", value.title, value.description, value.id, value.body)
		document, err := (okfv01.Codec{}).Parse(value.path, []byte(content))
		if err != nil {
			tb.Fatal(err)
		}
		if err := repository.Write(document); err != nil {
			tb.Fatal(err)
		}
	}
	return &evaluationFixture{root: root, store: database, service: &Service{Store: database, Knowledge: repository, Root: root}, cases: cases}
}

type evaluationToolRequest struct {
	question string
	term     string
	path     string
	strategy string
	request  Request
}

type evaluationDispatcher struct {
	fixture    *evaluationFixture
	operations []string
}

// dispatch is the single tool boundary used by the scripted evaluation agent.
// Telemetry is recorded here, so strategies cannot manufacture operation counts.
func (dispatcher *evaluationDispatcher) dispatch(name string, input evaluationToolRequest) (any, error) {
	dispatcher.operations = append(dispatcher.operations, name)
	switch name {
	case "grep":
		var matches []string
		err := filepath.WalkDir(dispatcher.fixture.root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(strings.ToLower(string(data)), input.term) {
				relative, _ := filepath.Rel(dispatcher.fixture.root, path)
				matches = append(matches, filepath.ToSlash(relative))
			}
			return nil
		})
		sort.Strings(matches)
		return matches, err
	case "read_file":
		return os.ReadFile(filepath.Join(dispatcher.fixture.root, filepath.FromSlash(input.path)))
	case "prowl_search":
		return query.New(dispatcher.fixture.store).SmartSearch(context.Background(), input.question)
	case "context_search":
		if input.strategy == "hybrid" {
			return dispatcher.fixture.service.Search(input.request)
		}
		direct, err := sourceCandidates(dispatcher.fixture.store, input.question, 40)
		if err != nil {
			return Packet{}, err
		}
		candidates := direct
		if input.strategy == "graph" {
			related, err := graphCandidates(dispatcher.fixture.store, direct, 8)
			if err != nil {
				return Packet{}, err
			}
			candidates = append(candidates, related...)
		}
		return Pack(input.request, candidates, nil)
	default:
		return nil, fmt.Errorf("unknown evaluation tool %q", name)
	}
}

type scriptedEvaluationAgent struct{ dispatcher *evaluationDispatcher }

func (agent scriptedEvaluationAgent) run(strategy string, testCase retrievalCase) (evaluationResult, error) {
	request := Request{Question: testCase.Question, Mode: ModeCompact, BudgetTokens: testCase.BudgetTokens}
	switch strategy {
	case "grep":
		hits := map[string]int{}
		for _, term := range queryTerms(testCase.Question) {
			if len(term) < 4 {
				continue
			}
			value, err := agent.dispatcher.dispatch("grep", evaluationToolRequest{term: term})
			if err != nil {
				return evaluationResult{}, err
			}
			for _, path := range value.([]string) {
				hits[path]++
			}
		}
		paths := make([]string, 0, len(hits))
		for path := range hits {
			paths = append(paths, path)
		}
		sort.Slice(paths, func(i, j int) bool {
			if hits[paths[i]] == hits[paths[j]] {
				return paths[i] < paths[j]
			}
			return hits[paths[i]] > hits[paths[j]]
		})
		return agent.readWithinBudget(paths, testCase.BudgetTokens)
	case "existing-prowl":
		value, err := agent.dispatcher.dispatch("prowl_search", evaluationToolRequest{question: testCase.Question})
		if err != nil {
			return evaluationResult{}, err
		}
		search := value.(query.SmartResult)
		paths := make([]string, 0, len(search.Matches))
		for _, hit := range search.Matches {
			paths = append(paths, hit.File)
		}
		return agent.readWithinBudget(paths, testCase.BudgetTokens)
	case "lexical", "graph", "hybrid":
		value, err := agent.dispatcher.dispatch("context_search", evaluationToolRequest{question: testCase.Question, strategy: strategy, request: request})
		if err != nil {
			return evaluationResult{}, err
		}
		return packetEvaluation(value.(Packet)), nil
	default:
		return evaluationResult{}, fmt.Errorf("unknown strategy %q", strategy)
	}
}

func (agent scriptedEvaluationAgent) readWithinBudget(paths []string, budget int) (evaluationResult, error) {
	result := evaluationResult{IDs: map[string]bool{}}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		value, err := agent.dispatcher.dispatch("read_file", evaluationToolRequest{path: path})
		if err != nil {
			return evaluationResult{}, err
		}
		data := value.([]byte)
		cost := ByteQuarterEstimator{}.Tokens(string(data))
		if result.EstimatedTokens+cost > budget {
			continue
		}
		result.IDs[path] = true
		result.Text += " " + string(data)
		result.EstimatedTokens += cost
	}
	return result, nil
}

func (fixture *evaluationFixture) run(strategy string, testCase retrievalCase) (evaluationResult, error) {
	dispatcher := &evaluationDispatcher{fixture: fixture}
	result, err := (scriptedEvaluationAgent{dispatcher: dispatcher}).run(strategy, testCase)
	result.ToolCalls = len(dispatcher.operations)
	return result, err
}

func packetEvaluation(packet Packet) evaluationResult {
	result := evaluationResult{IDs: map[string]bool{}, EstimatedTokens: packet.Budget.EstimatedTokens}
	for _, item := range packet.Items {
		result.IDs[item.ID] = true
		result.IDs[item.Title] = true
		result.Text += " " + item.Title + " " + item.Summary + " " + item.Content
	}
	return result
}

func scoreEvaluation(testCase retrievalCase, result evaluationResult) evaluationMetrics {
	expected := append(append([]string{}, testCase.ExpectedSources...), testCase.ExpectedConcepts...)
	hits := 0
	for _, id := range expected {
		if result.IDs[id] {
			hits++
		}
	}
	used := 0
	for _, term := range testCase.RequiredTerms {
		if strings.Contains(strings.ToLower(result.Text), strings.ToLower(term)) {
			used++
		}
	}
	distractorHits := 0
	for _, id := range testCase.Distractors {
		if result.IDs[id] {
			distractorHits++
		}
	}
	hitRate := float64(hits) / float64(len(expected))
	precision := 1.0
	if hits+distractorHits > 0 {
		precision = float64(hits) / float64(hits+distractorHits)
	}
	utilization := float64(used) / float64(len(testCase.RequiredTerms))
	return evaluationMetrics{HitRate: hitRate, Precision: precision, Utilization: utilization, EvidenceComplete: hits == len(expected) && used == len(testCase.RequiredTerms), BudgetAdherent: result.EstimatedTokens <= testCase.BudgetTokens, ToolCalls: result.ToolCalls}
}
