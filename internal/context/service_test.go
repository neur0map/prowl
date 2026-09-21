package context

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/store"
)

func TestPublishedGenerationGuard(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := &Service{Store: db, RequirePublished: true}
	if _, err := service.Search(Request{Question: "anything"}); !errors.Is(err, store.ErrGenerationIncomplete) {
		t.Fatalf("unpublished context error = %v", err)
	}
}

func TestServicePrefersHealthyKnowledgeAndFallsBackToChangedSource(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := []byte("authentication tokens are validated locally\n")
	if err := os.WriteFile(filepath.Join(root, "auth.txt"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(store.File{RelPath: "auth.txt", Lang: "text", Hash: "hash", Size: int64(len(source)), MTime: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileGraph(fileID, nil, nil, nil, []store.Chunk{{StartLine: 1, EndLine: 1, Text: string(source)}}); err != nil {
		t.Fatal(err)
	}
	hash, _ := knowledge.HashRegion(source, 1, 1)
	repo := knowledge.NewRepository(filepath.Join(root, "knowledge"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	doc, err := okfv01.Codec{}.Parse("authentication.md", []byte("---\ntype: Concept\ntitle: Authentication tokens\ndescription: Authentication tokens are validated locally.\nprowl:\n  id: authentication\n  confidence: verified\n  anchors:\n    - path: auth.txt\n      line_start: 1\n      line_end: 1\n      content_hash: "+hash+"\n---\nUse the local validator before accepting a token.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(doc); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: db, Knowledge: repo, Root: root}
	request := Request{Question: "authentication tokens", Mode: ModeCompact, BudgetTokens: 500}
	healthy, err := service.Search(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(healthy.Items) < 2 || healthy.Items[0].ID != "concept:authentication" || healthy.Items[0].Freshness != "current" {
		t.Fatalf("healthy ranking = %+v", healthy.Items)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.txt"), []byte("authentication tokens now use a remote validator\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, err := service.Search(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.Items) < 2 || !strings.HasPrefix(stale.Items[0].ID, "source:") || stale.Items[1].ID != "concept:authentication" || stale.Items[1].Freshness != "stale" {
		t.Fatalf("stale fallback ranking = %+v", stale.Items)
	}
	get, err := service.Get(Request{IDs: []string{stale.Items[0].ID, "concept:authentication", "missing"}, Mode: ModeStandard, BudgetTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(get.Items) != 2 || get.Omitted["not_found"] != 1 || get.TraceID == "" {
		t.Fatalf("get packet = %+v", get)
	}
}

func TestServiceRequiresQuestionAndIDs(t *testing.T) {
	service := Service{}
	if _, err := service.Search(Request{}); err == nil {
		t.Fatal("empty search accepted")
	}
	if _, err := service.Get(Request{}); err == nil {
		t.Fatal("empty get accepted")
	}
}

func TestServiceExpandsOneGraphHopWithSelectionReason(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":           "module fixture\n\ngo 1.25\n",
		"auth/main.go":     "package auth\nimport \"fixture/policy\"\n// Checks each incoming request and returns whether policy allowed it.\nfunc Authenticate() bool { return policy.Allowed() }\n",
		"policy/policy.go": "package policy\nimport \"fixture/audit\"\nfunc Allowed() bool { audit.Record(); return true }\n",
		"audit/audit.go":   "package audit\nfunc Record() {}\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	database, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := index.IndexWithOptions(database, root, index.Options{Languages: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	packet, err := (&Service{Store: database, Root: root}).Search(Request{Question: "checks incoming request", Mode: ModeCompact, BudgetTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	foundDirect := false
	foundSecondHop := false
	for _, item := range packet.Items {
		if item.Title == "auth/main.go" {
			foundDirect = true
		}
		if item.Title == "policy/policy.go" && strings.Contains(strings.Join(item.WhySelected, " "), "graph distance 1") {
			found = true
		}
		if item.Title == "audit/audit.go" {
			foundSecondHop = true
		}
	}
	if !foundDirect || !found || foundSecondHop {
		t.Fatalf("natural-language direct hit or graph neighbor missing from packet: %+v", packet.Items)
	}
}
