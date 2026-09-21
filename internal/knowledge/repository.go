package knowledge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/neur0map/prowl/internal/boundedio"
)

const (
	indexStart = "<!-- prowl:index:start -->"
	indexEnd   = "<!-- prowl:index:end -->"
	// MaxListDocuments and MaxListEntries are finite compatibility bounds used by List.
	MaxListDocuments = 10000
	MaxListEntries   = 100000
)

// Codec is implemented by a versioned interchange codec such as OKF v0.1.
type Codec interface {
	Parse(path string, data []byte) (*Document, error)
	Marshal(doc *Document) ([]byte, error)
}

// Repository owns canonical Markdown documents in one knowledge bundle.
type Repository struct {
	Root  string
	Codec Codec
}

func NewRepository(root string, codec Codec) *Repository {
	return &Repository{Root: root, Codec: codec}
}

// Init creates a valid empty bundle without overwriting existing human files.
func (r *Repository) Init() error {
	if r.Codec == nil {
		return errors.New("knowledge codec is required")
	}
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return err
	}
	indexPath := filepath.Join(r.Root, "index.md")
	if _, err := os.Stat(indexPath); os.IsNotExist(err) {
		content := []byte("# Knowledge\n\n" + indexStart + "\n_No concepts yet._\n" + indexEnd + "\n")
		if err := atomicWrite(indexPath, content, 0o644); err != nil {
			return err
		}
	}
	logPath := filepath.Join(r.Root, "log.md")
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		if err := atomicWrite(logPath, []byte("# Knowledge log\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ListLimits bounds both parsed documents and all entries visited by a list walk.
type ListLimits struct {
	Documents int
	Entries   int
}

// DocumentLimitError reports that a bounded repository walk found limit+1 documents.
type DocumentLimitError struct{ Limit int }

func (err *DocumentLimitError) Error() string {
	return fmt.Sprintf("knowledge documents exceed limit %d", err.Limit)
}

// EntryLimitError reports that a bounded repository walk visited limit+1 entries.
type EntryLimitError struct{ Limit int }

func (err *EntryLimitError) Error() string {
	return fmt.Sprintf("knowledge entries exceed limit %d", err.Limit)
}

// List parses concept Markdown files using finite compatibility bounds.
func (repository *Repository) List() ([]*Document, error) {
	return repository.ListContextBounded(context.Background(), ListLimits{
		Documents: MaxListDocuments,
		Entries:   MaxListEntries,
	})
}

// ListContext preserves the document-bounded API and applies a finite entry bound.
func (repository *Repository) ListContext(ctx context.Context, maxDocuments int) ([]*Document, error) {
	return repository.ListContextBounded(ctx, ListLimits{
		Documents: maxDocuments,
		Entries:   MaxListEntries,
	})
}

// ListContextBounded parses documents from one pinned root using deterministic walk order.
func (repository *Repository) ListContextBounded(ctx context.Context, limits ListLimits) ([]*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	maxInt := int(^uint(0) >> 1)
	if limits.Documents <= 0 || limits.Documents == maxInt {
		return nil, &DocumentLimitError{Limit: limits.Documents}
	}
	if limits.Entries <= 0 || limits.Entries == maxInt {
		return nil, &EntryLimitError{Limit: limits.Entries}
	}

	root, err := os.OpenRoot(repository.Root)
	if os.IsNotExist(err) {
		return []*Document{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()

	var docs []*Document
	entries := 1 // the pinned root itself
	if entries > limits.Entries {
		return nil, &EntryLimitError{Limit: limits.Entries}
	}
	var walkDirectory func(string) error
	walkDirectory = func(directory string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, err := root.Open(filepath.FromSlash(directory))
		if err != nil {
			return err
		}
		remaining := limits.Entries - entries
		children, readErr := handle.ReadDir(remaining + 1)
		closeErr := handle.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(children) > remaining {
			return &EntryLimitError{Limit: limits.Entries}
		}
		entries += len(children)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, entry := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			childPath := entry.Name()
			if directory != "." {
				childPath = path.Join(directory, childPath)
			}
			if entry.IsDir() {
				if err := walkDirectory(childPath); err != nil {
					return err
				}
				continue
			}
			if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			base := strings.ToLower(entry.Name())
			if base == "index.md" || base == "log.md" {
				continue
			}
			if len(docs) >= limits.Documents {
				return &DocumentLimitError{Limit: limits.Documents}
			}
			doc, err := repository.readDocumentContext(ctx, root, childPath)
			if err != nil {
				return err
			}
			docs = append(docs, doc)
		}
		return nil
	}
	err = walkDirectory(".")
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	if err == nil {
		err = ctx.Err()
	}
	return docs, err
}

func (repository *Repository) readDocumentContext(ctx context.Context, root *os.Root, path string) (*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := boundedio.OpenRegular(root, filepath.FromSlash(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, readErr := boundedio.ReadAllContext(ctx, file, MaxDocumentBytes)
	if errors.Is(readErr, boundedio.ErrTooLarge) {
		readErr = fmt.Errorf("knowledge file exceeds %d bytes: %s", MaxDocumentBytes, path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	doc, parseErr := repository.Codec.Parse(path, data)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return doc, parseErr
}

// Read parses a bundle-relative document after enforcing path containment.
func (r *Repository) Read(rel string) (*Document, error) {
	_, clean, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	data, err := readRootFile(r.Root, clean, MaxDocumentBytes)
	if err != nil {
		return nil, err
	}
	return r.Codec.Parse(clean, data)
}

// ReadBundleFile returns a bounded regular file from the canonical bundle.
func (r *Repository) ReadBundleFile(rel string) ([]byte, error) {
	_, clean, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	return readRootFile(r.Root, clean, MaxDocumentBytes)
}

// Write validates and atomically writes a canonical document.
func (r *Repository) Write(doc *Document) error {
	if doc == nil {
		return errors.New("nil knowledge document")
	}
	_, clean, err := r.resolve(doc.Path)
	if err != nil {
		return err
	}
	doc.Path = clean
	data, err := r.Codec.Marshal(doc)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	mode := fs.FileMode(0o644)
	if info, err := root.Lstat(filepath.FromSlash(clean)); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("knowledge destination is not a regular file: %s", clean)
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWriteInRoot(root, clean, data, mode)
}

// Import copies and validates a Markdown document without modifying the source.
func (r *Repository) Import(source, destination string) (*Document, error) {
	data, err := readRegularFile(source, MaxDocumentBytes)
	if err != nil {
		return nil, err
	}
	doc, err := r.Codec.Parse(filepath.ToSlash(destination), data)
	if err != nil {
		return nil, err
	}
	path, _, err := r.resolve(doc.Path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("knowledge destination already exists: %s", doc.Path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := r.Write(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// GenerateIndex refreshes only Prowl's marker-owned section in root index.md.
func (r *Repository) GenerateIndex() error {
	docs, err := r.List()
	if err != nil {
		return err
	}
	data, err := readRootFile(r.Root, "index.md", MaxDocumentBytes)
	if os.IsNotExist(err) {
		data = []byte("# Knowledge\n")
	} else if err != nil {
		return err
	}
	var generated strings.Builder
	generated.WriteString(indexStart + "\n")
	if len(docs) == 0 {
		generated.WriteString("_No concepts yet._\n")
	} else {
		for _, doc := range docs {
			title := doc.Title
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(doc.Path), filepath.Ext(doc.Path))
			}
			generated.WriteString(fmt.Sprintf("- [%s](%s) — %s\n", title, doc.Path, doc.Type))
		}
	}
	generated.WriteString(indexEnd + "\n")
	updated := replaceOwnedBlock(data, []byte(generated.String()))
	return r.writeBundleFile("index.md", updated, 0o644)
}

// AppendLog adds one UTC event using an atomic file replacement.
func (r *Repository) AppendLog(action, path string, at time.Time) error {
	data, err := readRootFile(r.Root, "log.md", MaxDocumentBytes)
	if os.IsNotExist(err) {
		data = []byte("# Knowledge log\n")
	} else if err != nil {
		return err
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	line := fmt.Sprintf("- %s — %s `%s`\n", at.UTC().Format(time.RFC3339), action, filepath.ToSlash(path))
	return r.writeBundleFile("log.md", append(data, []byte(line)...), 0o644)
}

// Export copies the complete bundle, including unknown files, to an empty path.
func (r *Repository) Export(destination string) error {
	if _, err := os.Stat(destination); err == nil {
		entries, readErr := os.ReadDir(destination)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			return fmt.Errorf("export destination is not empty: %s", destination)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return filepath.WalkDir(r.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.Root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := readRootFile(r.Root, filepath.ToSlash(rel), 0)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return atomicWrite(target, data, info.Mode().Perm())
	})
}

func (r *Repository) resolve(rel string) (string, string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel))))
	if clean == "" || clean == "." || filepath.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("unsafe knowledge path %q", rel)
	}
	path := filepath.Join(r.Root, filepath.FromSlash(clean))
	return path, clean, nil
}

func replaceOwnedBlock(data, block []byte) []byte {
	start := bytes.Index(data, []byte(indexStart))
	if start >= 0 {
		endRel := bytes.Index(data[start:], []byte(indexEnd))
		if endRel >= 0 {
			end := start + endRel + len(indexEnd)
			if end < len(data) && data[end] == '\n' {
				end++
			}
			out := append(append([]byte(nil), data[:start]...), block...)
			return append(out, data[end:]...)
		}
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(out, block...)
}

func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prowl-write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncDirectoryAfterRename(directory)
}

func (r *Repository) writeBundleFile(rel string, data []byte, mode fs.FileMode) error {
	_, clean, err := r.resolve(rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	return atomicWriteInRoot(root, clean, data, mode)
}

func atomicWriteInRoot(root *os.Root, rel string, data []byte, mode fs.FileMode) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	dir := filepath.Dir(clean)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".prowl-write-"+hex.EncodeToString(random[:]))
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, clean); err != nil {
		return err
	}
	directory, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return syncDirectoryAfterRename(directory)
}
