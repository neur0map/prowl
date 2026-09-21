package knowledge_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/store"
)

func TestKnowledgeMetadataRebuildsAfterDatabaseDeletion(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, ".prowl", "knowledge")
	repo := knowledge.NewRepository(bundle, okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	source := []byte("durable evidence\n")
	if err := os.WriteFile(filepath.Join(root, "evidence.txt"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, _ := knowledge.HashRegion(source, 1, 1)
	doc, err := okfv01.Codec{}.Parse("decisions/storage.md", []byte("---\ntype: Decision\ntitle: Keep knowledge in Markdown\ntags: [storage]\nprowl:\n  id: storage-decision\n  status: accepted\n  anchors:\n    - path: evidence.txt\n      line_start: 1\n      line_end: 1\n      content_hash: "+hash+"\n---\nSQLite is rebuildable.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(doc); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, ".prowl", "index.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
	first, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncStore(first, root, now); err != nil {
		t.Fatal(err)
	}
	before, err := first.ListKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ResetDerived(); err != nil {
		t.Fatal(err)
	}
	afterReset, err := first.ListKnowledge()
	if err != nil || !reflect.DeepEqual(before, afterReset) {
		t.Fatalf("ResetDerived removed canonical metadata: before=%+v after=%+v err=%v", before, afterReset, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	second, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := repo.SyncStore(second, root, now); err != nil {
		t.Fatal(err)
	}
	afterRebuild, err := second.ListKnowledge()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, afterRebuild) || len(afterRebuild) != 1 {
		t.Fatalf("knowledge metadata did not rebuild: before=%+v after=%+v", before, afterRebuild)
	}
}
