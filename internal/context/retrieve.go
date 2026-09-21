package context

import (
	"encoding/base64"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/store"
)

func knowledgeCandidates(repo *knowledge.Repository, sourceRoot, query string) ([]Candidate, error) {
	if repo == nil {
		return nil, nil
	}
	docs, err := repo.List()
	if err != nil {
		return nil, err
	}
	terms := queryTerms(query)
	var out []Candidate
	for _, doc := range docs {
		score := lexicalScore(terms, doc.Title+" "+doc.Description+" "+strings.Join(doc.Tags, " ")+" "+string(doc.Body))
		if len(terms) > 0 && score == 0 {
			continue
		}
		freshness := "unverified"
		citations := make([]Citation, 0, len(doc.Prowl.Anchors)+1)
		if doc.Resource != "" {
			citations = append(citations, Citation{URI: doc.Resource})
		}
		if len(doc.Prowl.Anchors) > 0 {
			freshness = "current"
			for _, anchor := range doc.Prowl.Anchors {
				check := knowledge.CheckAnchor(sourceRoot, anchor)
				if check.Status != knowledge.AnchorCurrent {
					freshness = string(check.Status)
				}
				// A moved anchor's evidence is intact, so cite where the lines are
				// now instead of sending the agent to coordinates that shifted.
				lineStart, lineEnd := check.Region()
				citations = append(citations, Citation{URI: sourceResourceURI(anchor.Path), Path: filepath.ToSlash(anchor.Path), LineStart: lineStart, LineEnd: lineEnd, ContentHash: anchor.ContentHash})
			}
		}
		id := doc.Prowl.ID
		if id == "" {
			id = strings.TrimSuffix(filepath.ToSlash(doc.Path), filepath.Ext(doc.Path))
		}
		detailResource := conceptResourceURI(id)
		citations = append([]Citation{{URI: detailResource, Path: filepath.ToSlash(doc.Path)}}, citations...)
		out = append(out, Candidate{
			Item: Item{
				ID: "concept:" + id, Kind: "knowledge:" + doc.Type, Title: doc.Title,
				Summary: doc.Description, WhySelected: []string{"lexical knowledge match"},
				Freshness: freshness, Confidence: confidenceValue(doc.Prowl.Confidence),
				Audience: []string{"assistant", "user"}, Citations: citations,
				DetailResource: detailResource,
			},
			CompactContent: doc.Description, StandardContent: firstParagraph(doc.Body), FullContent: string(doc.Body),
			LexicalScore: score, Knowledge: true,
		})
	}
	return out, nil
}

func sourceCandidates(target *store.Store, query string, limit int) ([]Candidate, error) {
	if target == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	hits, err := target.SearchChunkText(query, limit)
	if err != nil {
		return nil, err
	}
	terms := queryTerms(query)
	out := make([]Candidate, 0, len(hits))
	for _, hit := range hits {
		end := citationEndLine(hit.StartLine, hit.Text)
		idPayload := filepath.ToSlash(hit.File) + ":" + itoa(hit.StartLine)
		id := "source:" + base64.RawURLEncoding.EncodeToString([]byte(idPayload))
		candidate := Candidate{
			Item: Item{
				ID: id, Kind: "source", Title: filepath.ToSlash(hit.File), Summary: firstParagraph([]byte(hit.Text)),
				WhySelected: []string{"full-text source match"}, Freshness: "current", Confidence: 1,
				Audience:       []string{"assistant", "user"},
				Citations:      []Citation{{URI: sourceResourceURI(hit.File), Path: filepath.ToSlash(hit.File), LineStart: hit.StartLine, LineEnd: end}},
				DetailResource: sourceResourceURI(hit.File),
			},
			CompactContent: firstParagraph([]byte(hit.Text)), StandardContent: hit.Text, FullContent: hit.Text,
			LexicalScore: lexicalScore(terms, hit.Text),
		}
		if class := lowSignalClass(hit.File); class != "" && !queryWantsClass(terms, class) {
			candidate.LowSignal = true
			candidate.LowSignalClass = class
		}
		out = append(out, candidate)
	}
	return out, nil
}

// symbolMatchBoost lifts a candidate that DEFINES a symbol matching the query
// above one that only mentions the terms in prose or comments. A file named
// after the concept (ComputeFrame for "how is a frame computed") is more
// authoritative than incidental text, and this signal is deterministic.
const symbolMatchBoost = 8

// applySymbolMatch flags candidates whose file defines a symbol whose name
// contains a meaningful query term, using the same substring match `find` uses
// (so camelCase components are caught). Short tokens are skipped to avoid
// matching common substrings. The flag is scored in RankCandidates.
func applySymbolMatch(candidates []Candidate, target *store.Store, query string) {
	if target == nil {
		return
	}
	files := map[string]bool{}
	for _, term := range queryTerms(query) {
		for _, form := range termForms(term) {
			if len(form) < 4 {
				continue
			}
			hits, err := target.SymbolsBySubstring(form, 50)
			if err != nil {
				continue
			}
			for _, h := range hits {
				files[h.File] = true
			}
		}
	}
	if len(files) == 0 {
		return
	}
	for i := range candidates {
		if len(candidates[i].Citations) > 0 && files[candidates[i].Citations[0].Path] {
			candidates[i].SymbolMatch = true
		}
	}
}

// pathMatchBoost lifts a file whose PATH names the query concept. Developers name
// files after their purpose (cursorRenderer.ts, projectPersistence.ts), so the
// path is deterministic context -- the code-native form of contextual retrieval.
// A file whose body never repeats its own filename would otherwise be missed
// entirely by chunk and symbol search.
const pathMatchBoost = 8

// pathScore scores a repo-relative path against the query terms: basename matches
// count double (a file named for the concept is the answer), directory matches
// add context.
func pathScore(terms []string, rel string) float64 {
	rel = strings.ToLower(filepath.ToSlash(rel))
	base := rel
	dir := ""
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		base, dir = rel[i+1:], rel[:i]
	}
	return lexicalScore(terms, base)*2 + lexicalScore(terms, dir)
}

// namesToConcept reports whether a file's basename contains enough distinct query
// terms to be considered named for the concept: at least two for a multi-word
// query (projectPersistence -> "project"+"persistence"), or the sole term for a
// single-word query. Requiring the basename and two terms keeps incidental
// single-term matches (rank.go for a query mentioning "ranking") from flooding.
func namesToConcept(terms []string, rel string) bool {
	base := strings.ToLower(filepath.ToSlash(rel))
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	need := 2
	if len(terms) < 2 {
		need = 1
	}
	distinct := 0
	for _, t := range terms {
		if len(t) >= 3 && strings.Contains(base, t) {
			distinct++
		}
	}
	return distinct >= need
}

// applyPathMatch flags every candidate whose basename names the query concept and
// adds the path evidence to its lexical score. Run over the full candidate set so
// the signal survives dedup regardless of which same-file candidate is kept.
func applyPathMatch(candidates []Candidate, query string) {
	terms := queryTerms(query)
	for i := range candidates {
		if len(candidates[i].Citations) == 0 {
			continue
		}
		path := candidates[i].Citations[0].Path
		if namesToConcept(terms, path) {
			candidates[i].PathMatch = true
			candidates[i].LexicalScore += pathScore(terms, path)
		}
	}
}

// pathCandidates recalls files whose path names the query concept but that chunk
// and symbol search missed (their body never repeats the filename). Excludes
// paths already covered; scoring is applied later by applyPathMatch.
func pathCandidates(target *store.Store, query string, exclude map[string]bool, limit int) ([]Candidate, error) {
	if target == nil || limit <= 0 {
		return nil, nil
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	files, err := target.AllFiles()
	if err != nil {
		return nil, err
	}
	type scored struct {
		path  string
		score float64
	}
	matched := make([]scored, 0, 8)
	for _, f := range files {
		if exclude[f.RelPath] {
			continue
		}
		if !namesToConcept(terms, f.RelPath) {
			continue
		}
		matched = append(matched, scored{f.RelPath, pathScore(terms, f.RelPath)})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].score == matched[j].score {
			return matched[i].path < matched[j].path
		}
		return matched[i].score > matched[j].score
	})
	if len(matched) > limit {
		matched = matched[:limit]
	}
	out := make([]Candidate, 0, len(matched))
	for _, m := range matched {
		chunk, found, err := target.FirstChunk(m.path)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		end := citationEndLine(chunk.StartLine, chunk.Text)
		idPayload := filepath.ToSlash(m.path) + ":" + itoa(chunk.StartLine)
		id := "source:" + base64.RawURLEncoding.EncodeToString([]byte(idPayload))
		out = append(out, Candidate{
			Item: Item{
				ID: id, Kind: "source", Title: filepath.ToSlash(m.path), Summary: firstParagraph([]byte(chunk.Text)),
				WhySelected: []string{"file path names the query concept"}, Freshness: "current", Confidence: 1,
				Audience:       []string{"assistant", "user"},
				Citations:      []Citation{{URI: sourceResourceURI(m.path), Path: filepath.ToSlash(m.path), LineStart: chunk.StartLine, LineEnd: end}},
				DetailResource: sourceResourceURI(m.path),
			},
			CompactContent: firstParagraph([]byte(chunk.Text)), StandardContent: chunk.Text, FullContent: chunk.Text,
		})
	}
	return out, nil
}

// termForms returns a query term plus light morphological stems, so an inflected
// query word still matches a base-form symbol name under substring matching:
// "indexing" -> "index" (indexWithOptions), "parsed" -> "pars" (parseFile),
// "files" -> "file" (parseFile). Substring matching means an approximate stem
// still hits; forms shorter than 4 chars are dropped by the caller to avoid
// matching common fragments.
func termForms(term string) []string {
	forms := []string{term}
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if strings.HasSuffix(term, suf) {
			forms = append(forms, term[:len(term)-len(suf)])
		}
	}
	return forms
}

func graphCandidates(target *store.Store, direct []Candidate, limit int) ([]Candidate, error) {
	if target == nil || len(direct) == 0 || limit <= 0 {
		return nil, nil
	}
	directPaths := map[string]bool{}
	for _, candidate := range direct {
		if len(candidate.Citations) > 0 && candidate.Citations[0].Path != "" {
			directPaths[candidate.Citations[0].Path] = true
		}
	}
	related := map[string]int{}
	for _, candidate := range direct {
		remaining := limit - len(related)
		if remaining <= 0 {
			break
		}
		if len(candidate.Citations) == 0 || candidate.Citations[0].Path == "" {
			continue
		}
		path := candidate.Citations[0].Path
		fileID, err := target.FileID(path)
		if err != nil {
			continue
		}
		dependencies, err := target.ImmediateGraphNeighbors(fileID, remaining)
		if err != nil {
			return nil, err
		}
		for _, dependency := range dependencies {
			if directPaths[dependency.File] {
				continue
			}
			if previous, exists := related[dependency.File]; !exists || dependency.Depth < previous {
				related[dependency.File] = dependency.Depth
			}
		}
	}
	paths := make([]string, 0, len(related))
	for path := range related {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > limit {
		paths = paths[:limit]
	}
	out := make([]Candidate, 0, len(paths))
	for _, path := range paths {
		chunk, found, err := target.FirstChunk(path)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		end := citationEndLine(chunk.StartLine, chunk.Text)
		idPayload := filepath.ToSlash(chunk.File) + ":" + itoa(chunk.StartLine)
		out = append(out, Candidate{
			Item: Item{
				ID: "source:" + base64.RawURLEncoding.EncodeToString([]byte(idPayload)), Kind: "source", Title: filepath.ToSlash(chunk.File),
				Summary: firstParagraph([]byte(chunk.Text)), WhySelected: []string{"dependency graph expansion"}, Freshness: "current", Confidence: 1,
				Audience: []string{"assistant", "user"}, Citations: []Citation{{URI: sourceResourceURI(chunk.File), Path: filepath.ToSlash(chunk.File), LineStart: chunk.StartLine, LineEnd: end}},
				DetailResource: sourceResourceURI(chunk.File),
			},
			CompactContent: firstParagraph([]byte(chunk.Text)), StandardContent: chunk.Text, FullContent: chunk.Text, GraphDistance: related[path],
		})
	}
	return out, nil
}

func sourceResourceURI(path string) string {
	return "prowl://workspace/current/source/" + url.PathEscape(filepath.ToSlash(path))
}

func conceptResourceURI(id string) string {
	return "prowl://workspace/current/concept/" + url.PathEscape(id)
}

func citationEndLine(start int, text string) int {
	lines := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		lines++
	}
	if lines < 1 {
		lines = 1
	}
	return start + lines - 1
}

// queryStopwords are common English and question words that carry no code
// relevance, so counting them in lexical scoring inflates verbose prose and
// generated data files over the actual code. Code-common words (get, set) are
// deliberately kept.
var queryStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "on": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "for": true, "and": true,
	"or": true, "with": true, "how": true, "does": true, "do": true, "where": true, "what": true,
	"when": true, "why": true, "which": true, "that": true, "this": true, "it": true, "by": true,
	"from": true, "as": true, "at": true, "into": true,
}

func queryTerms(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	seen := map[string]bool{}
	var terms []string
	for _, field := range fields {
		field = strings.Trim(field, ".,:;!?()[]{}\"'`")
		if len(field) > 1 && !seen[field] && !queryStopwords[field] {
			seen[field] = true
			terms = append(terms, field)
		}
	}
	sort.Strings(terms)
	return terms
}

func lexicalScore(terms []string, text string) float64 {
	text = strings.ToLower(text)
	var score float64
	for _, term := range terms {
		count := strings.Count(text, term)
		if count > 3 {
			count = 3
		}
		score += float64(count * 5)
	}
	return score
}

func confidenceValue(value string) float64 {
	switch strings.ToLower(value) {
	case "verified", "high":
		return 1
	case "medium", "likely":
		return 0.7
	case "low", "uncertain":
		return 0.4
	default:
		return 0.8
	}
}

func firstParagraph(body []byte) string {
	text := strings.TrimSpace(string(body))
	if index := strings.Index(text, "\n\n"); index >= 0 {
		text = text[:index]
	}
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}
