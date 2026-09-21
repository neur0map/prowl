package docs

import (
	stdctx "context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	contextpacket "github.com/neur0map/prowl/internal/context"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/store"
)

// AddResult summarizes an ingest.
type AddResult struct {
	Source  Source
	Indexed int
}

var docExts = map[string]bool{".md": true, ".mdx": true, ".markdown": true}

// AddRemote crawls a documentation site into the corpus and reindexes it. Any
// existing pages for the same source name are replaced.
func AddRemote(ctx stdctx.Context, home, name string, u *url.URL, cfg CrawlConfig) (AddResult, error) {
	if name == "" {
		name = u.Host
	}
	slug := Slug(name)
	cfg.Start = u
	cfg.OutputDir = filepath.Join(sourcesDir(home), slug)
	cfg.QuarantineDir = filepath.Join(quarantineDir(home), slug)
	_ = os.RemoveAll(cfg.OutputDir)
	_ = os.RemoveAll(cfg.QuarantineDir)

	stats, err := Crawl(ctx, cfg)
	if err != nil {
		return AddResult{}, err
	}
	if stats.Pages == 0 {
		return AddResult{}, fmt.Errorf("no indexable pages crawled from %s (quarantined %d)", u, stats.Quarantined)
	}
	if err := reindex(ctx, home); err != nil {
		return AddResult{}, err
	}
	src := Source{Name: name, Slug: slug, URL: u.String(), AddedAt: time.Now().UTC().Format(time.RFC3339), Pages: stats.Pages, Quarantined: stats.Quarantined}
	if err := upsertManifest(home, src); err != nil {
		return AddResult{}, err
	}
	return AddResult{Source: src, Indexed: stats.Pages}, nil
}

// AddLocal ingests a local directory of Markdown into the corpus and reindexes.
func AddLocal(ctx stdctx.Context, home, name, srcPath string) (AddResult, error) {
	abs, err := filepath.Abs(srcPath)
	if err != nil {
		return AddResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return AddResult{}, err
	}
	if !info.IsDir() {
		return AddResult{}, fmt.Errorf("%s is not a directory", srcPath)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	slug := Slug(name)
	out := filepath.Join(sourcesDir(home), slug)
	_ = os.RemoveAll(out)

	n, quarantined, err := copyMarkdownTree(abs, out)
	if err != nil {
		return AddResult{}, err
	}
	if n == 0 {
		return AddResult{}, fmt.Errorf("no Markdown files found under %s", srcPath)
	}
	if err := reindex(ctx, home); err != nil {
		return AddResult{}, err
	}
	src := Source{Name: name, Slug: slug, Path: abs, AddedAt: time.Now().UTC().Format(time.RFC3339), Pages: n, Quarantined: quarantined}
	if err := upsertManifest(home, src); err != nil {
		return AddResult{}, err
	}
	return AddResult{Source: src, Indexed: n}, nil
}

// Remove deletes a source's pages from the corpus and reindexes.
func Remove(ctx stdctx.Context, home, name string) (Source, error) {
	m, err := LoadManifest(home)
	if err != nil {
		return Source{}, err
	}
	src, ok := m.Remove(name)
	if !ok {
		return Source{}, fmt.Errorf("no such docs source %q", name)
	}
	_ = os.RemoveAll(filepath.Join(sourcesDir(home), src.Slug))
	_ = os.RemoveAll(filepath.Join(quarantineDir(home), src.Slug))
	if err := reindex(ctx, home); err != nil {
		return Source{}, err
	}
	if err := m.Save(home); err != nil {
		return Source{}, err
	}
	return src, nil
}

// Search runs budget-bounded, cited retrieval over the docs corpus with the same
// engine prowl uses for code. It requires no model; without an embedder it uses
// lexical retrieval.
func Search(home, question string, budgetTokens int) (contextpacket.Packet, error) {
	if budgetTokens <= 0 {
		budgetTokens = 1800
	}
	s, err := store.Open(storePath(home))
	if err != nil {
		return contextpacket.Packet{}, err
	}
	defer s.Close()
	svc := &contextpacket.Service{Store: s, Root: sourcesDir(home)}
	return svc.Search(contextpacket.Request{Question: question, Mode: contextpacket.ModeCompact, BudgetTokens: budgetTokens})
}

// reindex rebuilds the docs store from the sources tree. Indexing is incremental
// (content-hash based), so re-running after add/remove is cheap.
func reindex(ctx stdctx.Context, home string) error {
	dir := sourcesDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s, err := store.Open(storePath(home))
	if err != nil {
		return err
	}
	defer s.Close()
	_, err = index.IndexWithOptionsContext(ctx, s, dir, index.Options{})
	return err
}

func upsertManifest(home string, src Source) error {
	m, err := LoadManifest(home)
	if err != nil {
		return err
	}
	m.Upsert(src)
	return m.Save(home)
}

// copyMarkdownTree copies Markdown files from src into dst, mirroring structure,
// skipping oversized files and quarantining any that contain prompt injection.
func copyMarkdownTree(src, dst string) (copied, quarantined int, err error) {
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !docExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxPageBytes {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if looksLikeInjection(string(data)) {
			quarantined++
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return nil
		}
		rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ".md"
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return err
		}
		copied++
		return nil
	})
	return copied, quarantined, err
}
