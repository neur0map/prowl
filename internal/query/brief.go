package query

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/store"
)

// LangCount pairs a language with how many scoped files use it.
type LangCount struct {
	Lang  string `json:"lang"`
	Files int    `json:"files"`
}

// Brief is a bounded, cited orientation for one path or subsystem: how big it
// is, which languages it uses, the architecture guides to read, and the most
// depended-on files to read first. It is one call an agent can make (or a parent
// can hand a subagent) to start warm on a slice of the repo instead of
// re-deriving its shape by reading files. Every path listed is a real indexed
// file, so the answer is cited by construction.
type Brief struct {
	Scope     string         `json:"scope"`
	Files     int            `json:"files"`
	Languages []LangCount    `json:"languages"`
	Guides    []string       `json:"guides,omitempty"`
	KeyFiles  []store.FanRow `json:"key_files"`
}

// Brief assembles a scoped orientation. scope is a repo-relative path prefix (a
// file or directory); "" or "." briefs the whole repo. Key files are ranked by
// dependency fan-in within scope; when fan-in is sparse (a leaf subsystem), the
// largest in-scope files fill in so the answer still points somewhere useful.
func (q *Querier) Brief(scope string) (Brief, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return Brief{}, err
	}
	defer release()

	clean := path.Clean(strings.TrimSpace(scope))
	clean = strings.Trim(clean, "/")
	inScope := func(rel string) bool {
		if clean == "" || clean == "." {
			return true
		}
		return rel == clean || strings.HasPrefix(rel, clean+"/")
	}

	files, err := q.s.AllFiles()
	if err != nil {
		return Brief{}, err
	}
	b := Brief{Scope: clean}
	if b.Scope == "" {
		b.Scope = "."
	}
	langCounts := map[string]int{}
	var scoped []store.File
	for _, f := range files {
		if inScope(f.RelPath) {
			scoped = append(scoped, f)
			langCounts[f.Lang]++
		}
	}
	b.Files = len(scoped)
	if b.Files == 0 {
		return Brief{}, fmt.Errorf("no indexed files under %q", scope)
	}
	for lang, n := range langCounts {
		b.Languages = append(b.Languages, LangCount{Lang: lang, Files: n})
	}
	sort.Slice(b.Languages, func(i, j int) bool {
		if b.Languages[i].Files != b.Languages[j].Files {
			return b.Languages[i].Files > b.Languages[j].Files
		}
		return b.Languages[i].Lang < b.Languages[j].Lang
	})

	b.Guides = guideDocs(files)

	edges, err := q.s.FileDepEdges()
	if err != nil {
		return Brief{}, err
	}
	allPaths := make([]string, 0, len(files))
	for _, f := range files {
		allPaths = append(allPaths, f.RelPath)
	}
	score, degree := fileCentrality(allPaths, edges)
	sizeOf := make(map[string]int64, len(scoped))
	inScopePaths := make([]string, 0, len(scoped))
	for _, f := range scoped {
		if isVendored(f.RelPath) {
			continue
		}
		inScopePaths = append(inScopePaths, f.RelPath)
		sizeOf[f.RelPath] = f.Size
	}
	sort.Slice(inScopePaths, func(i, j int) bool {
		pi, pj := inScopePaths[i], inScopePaths[j]
		if score[pi] != score[pj] {
			return score[pi] > score[pj]
		}
		if degree[pi] != degree[pj] {
			return degree[pi] > degree[pj]
		}
		if sizeOf[pi] != sizeOf[pj] {
			return sizeOf[pi] > sizeOf[pj]
		}
		return pi < pj
	})
	const topKey = 10
	for _, p := range inScopePaths {
		b.KeyFiles = append(b.KeyFiles, store.FanRow{File: p, In: degree[p]})
		if len(b.KeyFiles) >= topKey {
			break
		}
	}

	return b, nil
}
