package docs

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultUserAgent = "prowl-docs/1 (+https://github.com/neur0map/prowl)"
	maxPageBytes     = 4 << 20 // 4 MiB per fetched page
)

// CrawlConfig bounds a documentation crawl.
type CrawlConfig struct {
	Start          *url.URL
	OutputDir      string  // where indexed pages are written (sources/<slug>)
	QuarantineDir  string  // where flagged pages are written, excluded from the corpus
	MaxDepth       int     // link hops from Start (default 3)
	MaxPages       int     // hard page cap (default 200)
	Rate           float64 // requests per second (default 5)
	UserAgent      string
	SamePathPrefix bool // restrict to Start's path prefix (default true)
	NoLLMS         bool // skip the llms.txt / llms-full.txt fast paths
	Progress       func(pages, quarantined int, current string)
}

// CrawlStats reports a completed crawl.
type CrawlStats struct {
	Pages       int
	Quarantined int
}

func (c *CrawlConfig) applyDefaults() {
	if c.MaxDepth <= 0 {
		c.MaxDepth = 3
	}
	if c.MaxPages <= 0 {
		c.MaxPages = 200
	}
	if c.Rate <= 0 {
		c.Rate = 5
	}
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}
}

// Crawl performs a bounded, polite, same-host breadth-first crawl, writing each
// page as path-mirrored Markdown with YAML frontmatter. robots.txt is honored,
// requests are rate limited, and pages containing prompt injection are written
// to QuarantineDir and excluded from the searchable corpus.
func Crawl(ctx context.Context, cfg CrawlConfig) (CrawlStats, error) {
	cfg.applyDefaults()
	client := &http.Client{Timeout: 20 * time.Second}
	robots := fetchRobots(ctx, client, cfg.Start, cfg.UserAgent)
	if !cfg.NoLLMS {
		if md, ok := tryLLMSFull(ctx, client, cfg.Start, cfg.UserAgent); ok {
			return writeLLMSFull(cfg, md)
		}
		if links, ok := tryLLMSIndex(ctx, client, cfg.Start, cfg.UserAgent); ok {
			return crawlSeeds(ctx, client, robots, cfg, links)
		}
	}
	interval := time.Duration(float64(time.Second) / cfg.Rate)
	startPrefix := strings.TrimSuffix(cfg.Start.Path, "/")

	type item struct {
		u     *url.URL
		depth int
	}
	visited := map[string]bool{}
	queue := []item{{cfg.Start, 0}}
	var stats CrawlStats

	for len(queue) > 0 && stats.Pages < cfg.MaxPages {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		cur := queue[0]
		queue = queue[1:]
		key := cur.u.String()
		if visited[key] {
			continue
		}
		visited[key] = true
		if cur.u.Host != cfg.Start.Host {
			continue
		}
		if cfg.SamePathPrefix && !underPrefix(cur.u.Path, startPrefix) {
			continue
		}
		if robots.disallowed(cur.u.Path) {
			continue
		}

		time.Sleep(interval)
		body, ok := fetchHTML(ctx, client, cur.u, cfg.UserAgent)
		if !ok {
			continue
		}
		pg, err := extractPage(body, cur.u)
		if err != nil || strings.TrimSpace(pg.markdown) == "" {
			continue
		}
		if looksLikeInjection(pg.markdown) {
			_ = writePage(cfg.QuarantineDir, cur.u, pg, true)
			stats.Quarantined++
		} else if writePage(cfg.OutputDir, cur.u, pg, false) == nil {
			stats.Pages++
		}
		if cfg.Progress != nil {
			cfg.Progress(stats.Pages, stats.Quarantined, cur.u.String())
		}
		if cur.depth < cfg.MaxDepth {
			for _, l := range pg.links {
				lu, err := url.Parse(l)
				if err != nil || visited[lu.String()] || lu.Host != cfg.Start.Host {
					continue
				}
				queue = append(queue, item{lu, cur.depth + 1})
			}
		}
	}
	return stats, nil
}

func underPrefix(p, prefix string) bool {
	if prefix == "" {
		return true
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/") || strings.HasPrefix(strings.TrimSuffix(p, "/"), prefix+"/")
}

func fetchHTML(ctx context.Context, client *http.Client, u *url.URL, ua string) ([]byte, bool) {
	body, ctype, ok := fetch(ctx, client, u, ua)
	if !ok || !strings.Contains(ctype, "text/html") {
		return nil, false
	}
	return body, true
}

func fetch(ctx context.Context, client *http.Client, u *url.URL, ua string) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, "", false
	}
	return data, resp.Header.Get("Content-Type"), true
}

// writePage writes a page as path-mirrored Markdown with frontmatter, refusing
// any destination that escapes baseDir.
func writePage(baseDir string, u *url.URL, pg page, quarantined bool) error {
	rel := urlToRelPath(u)
	dest := filepath.Join(baseDir, rel)
	cleanBase := filepath.Clean(baseDir)
	if !strings.HasPrefix(filepath.Clean(dest), cleanBase+string(filepath.Separator)) {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("title: " + yamlString(pg.title) + "\n")
	b.WriteString("source_url: " + yamlString(u.String()) + "\n")
	b.WriteString("fetched_at: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	if quarantined {
		b.WriteString("quarantined: true\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(pg.markdown)
	b.WriteString("\n")
	return os.WriteFile(dest, []byte(b.String()), 0o644)
}

// urlToRelPath mirrors the URL path into a Markdown file path.
func urlToRelPath(u *url.URL) string {
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return "index.md"
	}
	switch ext := path.Ext(p); {
	case ext == "":
		p += "/index.md"
	case ext == ".html" || ext == ".htm":
		p = strings.TrimSuffix(p, ext) + ".md"
	case ext == ".md":
	default:
		p += ".md"
	}
	return filepath.FromSlash(path.Clean(p))
}

func yamlString(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\"", "'")
	return "\"" + strings.TrimSpace(s) + "\""
}
