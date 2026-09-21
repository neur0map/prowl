package knowledge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
)

type countingCodec struct {
	started chan struct{}
	release chan struct{}
	count   int
	data    [][]byte
}

func (codec *countingCodec) Parse(path string, data []byte) (*knowledge.Document, error) {
	codec.count++
	codec.data = append(codec.data, append([]byte(nil), data...))
	if codec.count == 1 && codec.started != nil {
		close(codec.started)
		<-codec.release
	}
	return &knowledge.Document{Path: path}, nil
}

func (*countingCodec) Marshal(*knowledge.Document) ([]byte, error) { return nil, nil }

func writeTestFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryListContextBoundedDocumentAndEntryLimits(t *testing.T) {
	t.Run("exact document cap and max plus one", func(t *testing.T) {
		root := t.TempDir()
		writeTestFiles(t, root, map[string]string{"b.md": "b", "a.md": "a", "c.md": "c"})
		codec := &countingCodec{}
		repo := knowledge.NewRepository(root, codec)
		limits := knowledge.ListLimits{Documents: 3, Entries: 4} // root plus three files
		if docs, err := repo.ListContextBounded(context.Background(), limits); err != nil {
			t.Fatalf("exact limit: %v", err)
		} else if got := []string{docs[0].Path, docs[1].Path, docs[2].Path}; !reflect.DeepEqual(got, []string{"a.md", "b.md", "c.md"}) {
			t.Fatalf("order=%v", got)
		}
		codec.count = 0
		_, err := repo.ListContextBounded(context.Background(), knowledge.ListLimits{Documents: 2, Entries: 4})
		var limited *knowledge.DocumentLimitError
		if !errors.As(err, &limited) || limited.Limit != 2 || codec.count != 2 {
			t.Fatalf("error=%v limited=%+v parses=%d", err, limited, codec.count)
		}
	})

	t.Run("exact entry cap and max plus one", func(t *testing.T) {
		root := t.TempDir()
		writeTestFiles(t, root, map[string]string{"a.md": "a", "b.txt": "b"})
		repo := knowledge.NewRepository(root, &countingCodec{})
		if _, err := repo.ListContextBounded(context.Background(), knowledge.ListLimits{Documents: 2, Entries: 3}); err != nil {
			t.Fatalf("exact entry limit: %v", err)
		}
		writeTestFiles(t, root, map[string]string{"c.txt": "c"})
		_, err := repo.ListContextBounded(context.Background(), knowledge.ListLimits{Documents: 2, Entries: 3})
		var limited *knowledge.EntryLimitError
		if !errors.As(err, &limited) || limited.Limit != 3 {
			t.Fatalf("error=%v limited=%+v", err, limited)
		}
	})

	t.Run("non Markdown entries consume the entry budget", func(t *testing.T) {
		root := t.TempDir()
		files := make(map[string]string)
		for i := 0; i < 20; i++ {
			files[fmt.Sprintf("ignored-%02d.txt", i)] = "ignored"
		}
		writeTestFiles(t, root, files)
		codec := &countingCodec{}
		_, err := knowledge.NewRepository(root, codec).ListContextBounded(
			context.Background(), knowledge.ListLimits{Documents: 1, Entries: 5},
		)
		var limited *knowledge.EntryLimitError
		if !errors.As(err, &limited) || codec.count != 0 {
			t.Fatalf("error=%v parses=%d", err, codec.count)
		}
	})

	t.Run("rejects nonpositive and overflowing limits", func(t *testing.T) {
		repo := knowledge.NewRepository(t.TempDir(), &countingCodec{})
		maxInt := int(^uint(0) >> 1)
		for _, limits := range []knowledge.ListLimits{
			{Documents: 0, Entries: 1},
			{Documents: 1, Entries: 0},
			{Documents: -1, Entries: 1},
			{Documents: 1, Entries: -1},
			{Documents: maxInt, Entries: 1},
			{Documents: 1, Entries: maxInt},
		} {
			if _, err := repo.ListContextBounded(context.Background(), limits); err == nil {
				t.Fatalf("limits %+v were accepted", limits)
			}
		}
		if _, err := repo.ListContext(context.Background(), maxInt); err == nil {
			t.Fatal("overflowing compatibility document limit was accepted")
		}
	})
}

func TestRepositoryListContextBoundedRejectsFIFOWithoutBlocking(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := t.TempDir()
	fifo := filepath.Join(root, "blocked.md")
	if output, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v: %s", err, output)
	}
	repository := knowledge.NewRepository(root, &countingCodec{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := repository.ListContextBounded(ctx, knowledge.ListLimits{Documents: 1, Entries: 2})
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO knowledge document was accepted")
		}
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("FIFO knowledge read took %v", elapsed)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("FIFO knowledge read blocked beyond its deadline")
	}
}

func TestRepositoryListContextCancellationDoesNotLeak(t *testing.T) {
	root := t.TempDir()
	writeTestFiles(t, root, map[string]string{"a.md": "a", "b.md": "b"})
	codec := &countingCodec{started: make(chan struct{}), release: make(chan struct{})}
	repo := knowledge.NewRepository(root, codec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := repo.ListContextBounded(ctx, knowledge.ListLimits{Documents: 2, Entries: 3})
		done <- err
	}()
	<-codec.started
	cancel()
	close(codec.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || codec.count != 1 {
			t.Fatalf("error=%v parses=%d", err, codec.count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled list goroutine did not exit")
	}
}

func TestRepositoryRootSwapCannotEscapePinnedRoot(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "knowledge")
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFiles(t, root, map[string]string{"a.md": "original-a", "b.md": "original-b"})
	writeTestFiles(t, outside, map[string]string{"a.md": "OUTSIDE-a", "b.md": "OUTSIDE-b"})

	codec := &countingCodec{started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-codec.release:
		default:
			close(codec.release)
		}
	}()
	repo := knowledge.NewRepository(root, codec)
	done := make(chan error, 1)
	go func() {
		_, err := repo.ListContextBounded(context.Background(), knowledge.ListLimits{Documents: 2, Entries: 3})
		done <- err
	}()
	<-codec.started
	moved := filepath.Join(tmp, "knowledge-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	close(codec.release)

	select {
	case <-done: // Completion from the pinned root and a safe failure are both valid.
		for _, data := range codec.data {
			if bytes.Contains(data, []byte("OUTSIDE")) {
				t.Fatalf("parsed bytes from replacement root: %q", data)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("root-swap list goroutine did not exit")
	}
}

func TestRepositoryInitWriteListIndexLogAndExport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "knowledge")
	repo := knowledge.NewRepository(root, okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, "index.md")
	if err := os.WriteFile(indexPath, []byte("# My curated index\n\nHuman notes stay.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := okfv01.Codec{}.Parse("architecture/storage.md", []byte("---\ntype: Decision\ntitle: Durable storage\nx-future: 7\nprowl:\n  id: storage-1\n---\nSQLite is derived.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Write(doc); err != nil {
		t.Fatal(err)
	}
	if err := repo.GenerateIndex(); err != nil {
		t.Fatal(err)
	}
	if err := repo.GenerateIndex(); err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(indexPath)
	if !bytes.Contains(index, []byte("Human notes stay.")) || strings.Count(string(index), "Durable storage") != 1 {
		t.Fatalf("index did not preserve human content or duplicated generated content:\n%s", index)
	}
	docs, err := repo.List()
	if err != nil || len(docs) != 1 || docs[0].Prowl.ID != "storage-1" {
		t.Fatalf("List = %+v, %v", docs, err)
	}
	at := time.Date(2026, 7, 23, 15, 0, 0, 0, time.FixedZone("local", 3600))
	if err := repo.AppendLog("accepted", doc.Path, at); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log.md"))
	if !bytes.Contains(log, []byte("2026-07-23T14:00:00Z — accepted `architecture/storage.md`")) {
		t.Fatalf("UTC log entry missing: %s", log)
	}
	export := filepath.Join(t.TempDir(), "export")
	if err := repo.Export(export); err != nil {
		t.Fatal(err)
	}
	exported := knowledge.NewRepository(export, okfv01.Codec{})
	roundTrip, err := exported.Read(doc.Path)
	if err != nil || roundTrip.Prowl.ID != doc.Prowl.ID {
		t.Fatalf("export round trip = %+v, %v", roundTrip, err)
	}
	mapping := roundTrip.Frontmatter.Content[0]
	foundUnknown := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "x-future" && mapping.Content[i+1].Value == "7" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatal("unknown field lost during repository export")
	}
}

func TestRepositoryRejectsSymlinkAndOversizedDocuments(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "knowledge")
	repo := knowledge.NewRepository(root, okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(tmp, "secret.md")
	if err := os.WriteFile(outside, []byte("---\ntype: Note\n---\nsecret outside bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "leak.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("leak.md"); err == nil {
		t.Fatal("repository followed a document symlink")
	}
	oversized := filepath.Join(root, "oversized.md")
	if err := os.WriteFile(oversized, bytes.Repeat([]byte("x"), int(knowledge.MaxDocumentBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("oversized.md"); err == nil {
		t.Fatal("repository accepted an oversized document")
	}
}

func TestRepositoryImportPreservesSourceAndRejectsCollision(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "candidate.md")
	original := []byte("---\ntype: Note\ntitle: Candidate\n---\nBody.\n")
	if err := os.WriteFile(source, original, 0o640); err != nil {
		t.Fatal(err)
	}
	repo := knowledge.NewRepository(filepath.Join(tmp, "bundle"), okfv01.Codec{})
	if err := repo.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Import(source, "notes/candidate.md"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(after, original) {
		t.Fatal("import modified source")
	}
	if _, err := repo.Import(source, "notes/candidate.md"); err == nil {
		t.Fatal("import should reject destination collision")
	}
	if _, err := repo.Import(source, "../outside.md"); err == nil {
		t.Fatal("import should reject path traversal")
	}
}
