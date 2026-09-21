package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/neur0map/prowl/internal/graph"
	"github.com/neur0map/prowl/internal/parse"
	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/redact"
	"github.com/neur0map/prowl/internal/store"
)

// ErrSourcesChanged marks a transient source-tree read inconsistency. Project
// refresh retries this class once before leaving publication incomplete.
var ErrSourcesChanged = errors.New("project sources changed during refresh")

// Summary reports what an Index run did.
type Summary struct {
	Indexed int // supported files seen
	Parsed  int // files (re)parsed this run
	Skipped int // unchanged files
	Deleted int // files removed from the index
	Symbols int // symbols in the index (total)
	Edges   int // edges in the index (total)
}

// Options controls which files and detected languages enter the structural
// index. An empty language list, or one containing "auto", enables every
// supported parser. Explicit lists use the canonical IDs returned by parse.Detect.
type Options struct {
	Ignore    []string
	Languages []string
}

type Progress struct {
	Phase   string
	Percent int
}
type ProgressReporter func(Progress) error

// Index incrementally synchronizes the store with the project rooted at root.
// Only files whose content hash changed are reparsed; removed files are deleted;
// the global resolution passes always run afterward.
func Index(s *store.Store, root string, ignore []string) (Summary, error) {
	return IndexWithOptions(s, root, Options{Ignore: ignore})
}

// IndexWithOptions incrementally synchronizes the store while enforcing the
// configured language policy.
func IndexWithOptions(s *store.Store, root string, opt Options) (Summary, error) {
	return IndexWithOptionsContext(context.Background(), s, root, opt)
}

// IndexWithOptionsContext incrementally synchronizes the store while honoring
// cancellation between traversal, file parsing, and graph resolution phases.
func indexWithOptions(ctx context.Context, s *store.Store, root string, opt Options, report ProgressReporter) (Summary, error) {
	var sum Summary
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	// Derived-table updates are incremental. Invalidate the generation before the
	// first mutation and publish completion only after all graph and metadata
	// writes succeed, so cancellation cannot masquerade as a fresh index.
	if err := s.SetMeta("index_state", "incomplete"); err != nil {
		return sum, err
	}
	reportProgress := func(phase string, percent int) error {
		if report == nil {
			return nil
		}
		return report(Progress{Phase: phase, Percent: percent})
	}
	if err := reportProgress("walking", 0); err != nil {
		return sum, err
	}
	rels, err := WalkContext(ctx, root, opt.Ignore)
	if err != nil {
		return sum, err
	}
	if err := reportProgress("indexing", 1); err != nil {
		return sum, err
	}
	current := make(map[string]bool, len(rels))
	ver := indexVersion()
	prevVer, _ := s.GetMeta("index_version")
	force := prevVer != ver // a binary upgrade re-parses everything, not just changed files
	if force {
		if err := s.ResetDerived(); err != nil { // fast bulk clear for a full re-parse
			return sum, err
		}
	}

	// Pre-load current file hashes so parse workers decide skip-vs-reparse without
	// touching the store, keeping all DB access on the single consumer goroutine.
	var known map[string]string
	if !force {
		if existing, err := s.AllFiles(); err == nil {
			known = make(map[string]string, len(existing))
			for _, f := range existing {
				known[f.RelPath] = f.Hash
			}
		}
	}

	// Reading and tree-sitter extraction are CPU-bound and independent per file,
	// so fan them out across workers; the store is single-writer, so results are
	// applied serially. Ordered futures keep the consume order equal to rels, so
	// file IDs and the published index stay deterministic across runs.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := runtime.GOMAXPROCS(0)
	if workers > len(rels) {
		workers = len(rels)
	}
	if workers < 1 {
		workers = 1
	}
	futures := make(chan chan fileParse, workers)
	go func() {
		defer close(futures)
		sem := make(chan struct{}, workers)
		for _, rel := range rels {
			select {
			case <-cctx.Done():
				return
			case sem <- struct{}{}:
			}
			ch := make(chan fileParse, 1)
			select {
			case <-cctx.Done():
				<-sem
				return
			case futures <- ch:
			}
			go func(rel string, ch chan fileParse) {
				defer func() { <-sem }()
				ch <- parseFile(root, rel, opt, force, known)
			}(rel, ch)
		}
	}()
	drain := func() {
		cancel()
		for range futures {
		}
	}

	processed := 0
	for ch := range futures {
		if err := ctx.Err(); err != nil {
			drain()
			return sum, err
		}
		fp := <-ch
		processed++
		if processed%32 == 0 && processed < len(rels) {
			if err := reportProgress("indexing", 1+processed*89/len(rels)); err != nil {
				drain()
				return sum, err
			}
		}
		if fp.err != nil {
			drain()
			return sum, fp.err
		}
		if !fp.detected {
			continue
		}
		current[fp.rel] = true
		sum.Indexed++
		if !fp.changed {
			sum.Skipped++
			continue
		}
		if !fp.hasExt {
			continue
		}
		syms, ress, edges, chunks, redacted := mapResult(fp.res)
		fid, err := s.UpsertFile(store.File{
			RelPath: fp.rel, Lang: fp.lang, Role: graph.InferRole(fp.rel, fp.lang),
			Size: fp.size, Hash: fp.hash, MTime: fp.mtime, Redacted: int64(redacted),
		})
		if err != nil {
			drain()
			return sum, err
		}
		if err := s.ReplaceFileGraph(fid, syms, ress, edges, chunks); err != nil {
			drain()
			return sum, err
		}
		sum.Parsed++
	}
	if err := reportProgress("indexing", 90); err != nil {
		return sum, err
	}

	// Remove files that disappeared from the project.
	all, err := s.AllFiles()
	if err != nil {
		return sum, err
	}
	for _, f := range all {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if !current[f.RelPath] {
			if err := s.DeleteFileByPath(f.RelPath); err != nil {
				return sum, err
			}
			sum.Deleted++
		}
	}
	all, err = s.AllFiles()
	if err != nil {
		return sum, err
	}

	// Record package/module metadata before resolution. A generation cannot be
	// marked complete if any of these writes fails.
	metadata := []struct {
		key   string
		value func() string
	}{
		{key: "go_module", value: func() string { return goModulePath(root) }},
		{key: "rust_crates", value: func() string { return rustCrates(root, all) }},
		{key: "ts_packages", value: func() string { return tsPackages(root, all) }},
		{key: "dart_packages", value: func() string { return dartPackages(root, all) }},
		{key: "ts_aliases", value: func() string { return tsAliases(root, all) }},
	}
	for _, item := range metadata {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if err := s.SetMeta(item.key, item.value()); err != nil {
			return sum, err
		}
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	if err := reportProgress("resolving", 95); err != nil {
		return sum, err
	}
	if err := graph.Resolve(s); err != nil {
		return sum, err
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	if c, err := s.Counts(); err == nil {
		sum.Symbols = c.Symbols
		sum.Edges = c.Edges
	}
	if err := s.SetMeta("last_index", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return sum, err
	}
	if err := s.SetMeta("index_version", ver); err != nil {
		return sum, err
	}
	if err := s.SetMeta("index_state", "complete"); err != nil {
		return sum, err
	}
	if err := reportProgress("complete", 100); err != nil {
		return sum, errors.Join(err, s.SetMeta("index_state", "incomplete"))
	}
	return sum, nil
}

// fileParse carries one file's parse result from a worker to the serial DB
// writer. detected marks a supported, non-empty source (counts as Indexed);
// changed marks a content-hash difference that must be written; hasExt marks a
// registered extractor, so res is meaningful.
type fileParse struct {
	rel      string
	detected bool
	changed  bool
	hasExt   bool
	lang     string
	hash     string
	size     int64
	mtime    int64
	res      extract.Result
	err      error
}

// parseFile performs one file's read, generated-content stripping, language
// detection, hashing, and (when changed) tree-sitter extraction. It does no
// database access -- the skip decision uses the pre-loaded hash map -- so it is
// safe to run concurrently: parse.Parse builds a fresh parser per call.
func parseFile(root, rel string, opt Options, force bool, known map[string]string) fileParse {
	if isGeneratedArtifact(rel) {
		return fileParse{rel: rel} // lockfile / minified bundle: skip before reading
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		return fileParse{rel: rel, err: fmt.Errorf("%w: read indexed source %s: %v", ErrSourcesChanged, rel, err)}
	}
	data = stripGeneratedContent(rel, data)
	if len(strings.TrimSpace(string(data))) == 0 {
		return fileParse{rel: rel}
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	lang := parse.Detect(rel, head)
	if lang == "" || !languageAllowed(lang, opt.Languages) {
		return fileParse{rel: rel}
	}
	fp := fileParse{
		rel: rel, detected: true, lang: lang,
		hash: strconv.FormatUint(xxhash.Sum64(data), 16), size: int64(len(data)),
	}
	if !force {
		if h, ok := known[rel]; ok && h == fp.hash {
			return fp
		}
	}
	fp.changed = true
	ex, ok := extract.For(lang)
	if !ok {
		return fp
	}
	fp.hasExt = true
	res, _ := ex.Extract(data)
	if lang == "qml" {
		b := filepath.Base(rel)
		res.Symbols = append(res.Symbols, extract.Symbol{
			Name: b[:len(b)-len(filepath.Ext(b))], Kind: "component", StartLine: 1, EndLine: 1,
		})
	}
	// Attach each symbol's leading doc comment. The file is split into lines once
	// here and reused across every symbol: doc extraction is per symbol on a
	// 64k-symbol repository, so re-splitting per symbol would land on the
	// cold-index hot path (C2). Skipped when the file yields no symbols.
	if len(res.Symbols) > 0 {
		extract.PopulateDocs(strings.Split(string(data), "\n"), res.Symbols)
	}
	if info, err := os.Stat(full); err == nil {
		fp.mtime = info.ModTime().Unix()
	}
	fp.res = res
	return fp
}

// isGeneratedArtifact reports whether rel is a generated dependency lockfile or a
// minified bundle. These carry no code-intelligence value -- a single
// package-lock.json expands into thousands of meaningless "setting" symbols that
// flood the index and pollute find -- so the indexer skips them entirely. Their
// real information (declared dependencies) lives in the manifest, which is indexed.
func isGeneratedArtifact(rel string) bool {
	base := strings.ToLower(rel)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "npm-shrinkwrap.json",
		"go.sum", "cargo.lock", "gemfile.lock", "poetry.lock", "composer.lock",
		"pipfile.lock", "flake.lock", "bun.lockb", "packages.lock.json",
		"pubspec.lock", "mix.lock", "gradle.lockfile":
		return true
	}
	if strings.HasSuffix(base, ".lock") {
		return true
	}
	return strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") ||
		strings.HasSuffix(base, ".min.mjs") || strings.HasSuffix(base, ".map")
}

func IndexWithOptionsContext(ctx context.Context, s *store.Store, root string, opt Options) (Summary, error) {
	return indexWithOptions(ctx, s, root, opt, nil)
}

func IndexWithOptionsProgressContext(ctx context.Context, s *store.Store, root string, opt Options, report ProgressReporter) (Summary, error) {
	return indexWithOptions(ctx, s, root, opt, report)
}

func languageAllowed(lang string, configured []string) bool {
	if len(configured) == 0 {
		return true
	}
	for _, candidate := range configured {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "auto" || candidate == "*" || candidate == lang {
			return true
		}
	}
	return false
}

func stripGeneratedContent(rel string, data []byte) []byte {
	// prowl-installed agent skills live at <harness>/skills/<name>/SKILL.md and
	// are entirely prowl-authored guidance, not project source, so they never
	// belong in the code index (same reasoning as the generated AGENTS.md block).
	if isInstalledSkillPath(rel) {
		return nil
	}
	if !strings.EqualFold(filepath.Base(rel), "AGENTS.md") {
		return data
	}
	// Both the guidance block and the auto-generated map block are prowl-authored,
	// so a purely-generated AGENTS.md strips to empty and is skipped by the caller.
	markers := [][2]string{
		{"<!-- prowl-agent -->", "<!-- /prowl-agent -->"},
		{"<!-- prowl-agent:map -->", "<!-- /prowl-agent:map -->"},
	}
	content := string(data)
	for _, m := range markers {
		for {
			start := strings.Index(content, m[0])
			if start < 0 {
				break
			}
			endRel := strings.Index(content[start:], m[1])
			if endRel < 0 {
				break
			}
			end := start + endRel + len(m[1])
			content = content[:start] + content[end:]
		}
	}
	return []byte(strings.TrimSpace(content))
}

// isInstalledSkillPath reports whether rel is a prowl-installed agent skill:
// a SKILL.md inside a "skills" directory that sits under a harness config dir
// (a path segment beginning with "."). A user's own docs/skills/x/SKILL.md is
// left indexed because its "skills" parent is not a dotted harness dir.
func isInstalledSkillPath(rel string) bool {
	if !strings.EqualFold(filepath.Base(rel), "SKILL.md") {
		return false
	}
	segments := strings.Split(filepath.ToSlash(rel), "/")
	for i := 1; i < len(segments); i++ {
		if segments[i] == "skills" && strings.HasPrefix(segments[i-1], ".") {
			return true
		}
	}
	return false
}

// Version returns the indexing-logic version (the binary's VCS revision). The
// CLI compares it against the stored value to force a re-index after a prowl
// upgrade even when the file fingerprint is unchanged.
func Version() string { return indexVersion() }

// indexVersion identifies the extraction/resolution logic the index was built
// with, from the binary's VCS revision. When it changes (a binary upgrade), Index
// forces a full re-parse so extractor and resolver fixes take effect, instead of
// incremental hashing skipping unchanged files and serving stale data.
func indexVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		// A clean release build is identified by its commit. A dirty build shares
		// that commit across edits, so fall through to the binary mtime below.
		if rev != "" && modified != "true" {
			return rev
		}
	}
	// Dev or dirty build: key off the binary's own mtime so each rebuild forces a
	// full reparse instead of reusing an index made by different extractor logic.
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			return "dev-" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
		}
	}
	return "dev"
}

// mapResult converts an extract.Result into store rows, masking every
// secret-shaped value before it becomes a row. Masking runs at every sink --
// chunk text, symbol signatures, symbol doc comments, resource values, and edge
// targets -- so no raw-source field reaches the store (C4). Chunk text is masked
// through redact.Chunks, not per-chunk redact.Text, so a private-key body that
// tiles across more than one chunk -- whose middle chunk carries no PEM marker
// for a per-chunk scan to trigger on -- is still masked rather than stored as raw
// key material (C4). The returned count is the chunk pass alone: chunkStructured
// tiles every non-blank line, so a single-line credential appears in exactly one
// chunk, while signatures, doc comments, resource values and edge targets are
// derived views of that same text -- counting them too would report one
// credential as two or three. A key spanning several chunks counts once per
// masked chunk fragment.
func mapResult(r extract.Result) ([]store.Symbol, []store.Resource, []store.RawEdge, []store.Chunk, int) {
	syms := make([]store.Symbol, len(r.Symbols))
	for i, s := range r.Symbols {
		sig, _ := redact.Text(s.Signature)
		doc, _ := redact.Text(s.Doc)
		syms[i] = store.Symbol{Name: s.Name, Kind: s.Kind, Signature: sig, Doc: doc, StartLine: s.StartLine, EndLine: s.EndLine, ParentName: s.Parent, Complexity: s.Complexity}
	}
	ress := make([]store.Resource, len(r.Resources))
	for i, rs := range r.Resources {
		val, _ := redact.Text(rs.Value)
		ress[i] = store.Resource{Kind: rs.Kind, Name: rs.Name, Value: val, Line: rs.Line}
	}
	edges := make([]store.RawEdge, len(r.Edges))
	for i, e := range r.Edges {
		raw, _ := redact.Text(e.Raw)
		edges[i] = store.RawEdge{SrcName: e.SrcName, Kind: e.Kind, Raw: raw, Line: e.Line}
	}
	texts := make([]string, len(r.Chunks))
	for i, c := range r.Chunks {
		texts[i] = c.Text
	}
	maskedTexts, redacted := redact.Chunks(texts)
	chunks := make([]store.Chunk, len(r.Chunks))
	for i, c := range r.Chunks {
		chunks[i] = store.Chunk{StartLine: c.StartLine, EndLine: c.EndLine, Text: maskedTexts[i]}
	}
	return syms, ress, edges, chunks, redacted
}

// goModulePath returns the module path declared in go.mod at root, or "" when
// there is no go.mod. Used to resolve in-module Go imports to package files.
func goModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// rustCrates maps each workspace crate's import name (its Cargo.toml [package]
// name with - turned to _) to its src directory, so the resolver can link
// `use other_crate::...` across a Cargo workspace. Returns a JSON object string,
// or "" when there are no crates. Reads from the indexed file list (Cargo.toml
// entries) so it costs no extra walk.
func rustCrates(root string, files []store.File) string {
	crates := map[string]string{}
	for _, f := range files {
		if filepath.Base(f.RelPath) != "Cargo.toml" {
			continue
		}
		name := cargoPackageName(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if name == "" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		src := "src"
		if dir != "." {
			src = dir + "/src"
		}
		crates[strings.ReplaceAll(name, "-", "_")] = src
	}
	if len(crates) == 0 {
		return ""
	}
	b, err := json.Marshal(crates)
	if err != nil {
		return ""
	}
	return string(b)
}

// cargoPackageName returns the [package] name from a Cargo.toml, or "" (e.g. a
// virtual workspace manifest with no [package]).
func cargoPackageName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	inPackage := false
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			inPackage = t == "[package]"
			continue
		}
		if inPackage {
			if rest, ok := strings.CutPrefix(t, "name"); ok {
				if v, ok := strings.CutPrefix(strings.TrimSpace(rest), "="); ok {
					return strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	return ""
}

// tsPackages maps each workspace package's name (its package.json "name" field)
// to its repo-relative directory, so the resolver can link a bare `@scope/pkg`
// or `pkg/subpath` import to that package's source. Returns a JSON object
// string, or "" when no named packages exist. node_modules is gitignored, so
// only first-party workspace packages are indexed. Reads from the indexed file
// list (package.json entries) so it costs no extra walk.
func tsPackages(root string, files []store.File) string {
	pkgs := map[string]string{}
	for _, f := range files {
		if filepath.Base(f.RelPath) != "package.json" {
			continue
		}
		name := packageJSONName(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if name == "" {
			continue
		}
		pkgs[name] = filepath.ToSlash(filepath.Dir(f.RelPath))
	}
	if len(pkgs) == 0 {
		return ""
	}
	b, err := json.Marshal(pkgs)
	if err != nil {
		return ""
	}
	return string(b)
}

// packageJSONName returns the "name" field of a package.json, or "" (e.g. a
// private root manifest with no name).
func packageJSONName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pj struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pj) != nil {
		return ""
	}
	return pj.Name
}

// dartPackages maps each Dart/Flutter package's name (its pubspec.yaml `name:`)
// to its repo-relative directory, so the resolver can link a `package:<name>/x`
// import to that package's lib/ source. Returns a JSON object string, or "" when
// no pubspec is present. Covers single-package apps and melos-style monorepos.
func dartPackages(root string, files []store.File) string {
	pkgs := map[string]string{}
	for _, f := range files {
		if filepath.Base(f.RelPath) != "pubspec.yaml" {
			continue
		}
		name := pubspecName(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if name == "" {
			continue
		}
		pkgs[name] = filepath.ToSlash(filepath.Dir(f.RelPath))
	}
	if len(pkgs) == 0 {
		return ""
	}
	b, err := json.Marshal(pkgs)
	if err != nil {
		return ""
	}
	return string(b)
}

// pubspecName returns the top-level `name:` field of a pubspec.yaml, or "".
func pubspecName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

// tsAliases reads tsconfig.json / jsconfig.json path aliases so the resolver can
// link an import like `@/components/Button` to its real source. For each config
// it records the alias prefix (from a `paths` wildcard) mapped to the
// repo-relative directory prefix (resolved against baseUrl), scoped to the
// config's directory so a monorepo's per-package aliases stay correct. Returns a
// JSON array string, or "" when no aliases are found.
func tsAliases(root string, files []store.File) string {
	type cfg struct {
		Dir     string              `json:"dir"`
		Aliases map[string][]string `json:"aliases"`
	}
	var out []cfg
	for _, f := range files {
		if b := filepath.Base(f.RelPath); b != "tsconfig.json" && b != "jsconfig.json" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		aliases := tsConfigPaths(filepath.Join(root, filepath.FromSlash(f.RelPath)), dir)
		if len(aliases) > 0 {
			out = append(out, cfg{Dir: dir, Aliases: aliases})
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// tsConfigPaths reads compilerOptions.baseUrl/paths from a (JSONC) tsconfig and
// returns each wildcard alias prefix mapped to its repo-relative target dir
// prefix(es). When a config defines no paths of its own it follows a local
// `extends` to a base config (the Turborepo/Nx shared-base pattern), resolving
// the base's targets relative to the base's own directory. Non-wildcard aliases
// and npm-package `extends` are not handled.
func tsConfigPaths(file, dir string) map[string][]string {
	return tsConfigPathsDepth(file, dir, 0)
}

func tsConfigPathsDepth(file, dir string, depth int) map[string][]string {
	if depth > 3 {
		return nil // guard against an extends cycle
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var cfg struct {
		Extends         any `json:"extends"`
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if json.Unmarshal(stripJSONC(data), &cfg) != nil {
		return nil
	}
	if len(cfg.CompilerOptions.Paths) == 0 {
		// No own paths: inherit from a local base config (a string `extends`
		// pointing at a relative file; npm-package/array extends are skipped).
		ext, _ := cfg.Extends.(string)
		if ext == "" || (!strings.HasPrefix(ext, ".") && !strings.Contains(ext, "/")) {
			return nil
		}
		if !strings.HasSuffix(ext, ".json") {
			ext += ".json"
		}
		baseFile := filepath.Join(filepath.Dir(file), filepath.FromSlash(ext))
		baseDir := path.Clean(path.Join(dir, path.Dir(ext)))
		return tsConfigPathsDepth(baseFile, baseDir, depth+1)
	}
	baseURL := cfg.CompilerOptions.BaseURL
	if baseURL == "" {
		baseURL = "."
	}
	out := map[string][]string{}
	for pat, targets := range cfg.CompilerOptions.Paths {
		ap, ok := strings.CutSuffix(pat, "*")
		if !ok {
			continue // only wildcard aliases (the common case)
		}
		for _, t := range targets {
			tp, ok := strings.CutSuffix(t, "*")
			if !ok {
				continue
			}
			out[ap] = append(out[ap], path.Clean(path.Join(dir, baseURL, tp)))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stripJSONC removes // and /* */ comments and trailing commas from JSONC, which
// tsconfig files routinely use, so encoding/json can parse them.
func stripJSONC(data []byte) []byte {
	b := make([]byte, 0, len(data))
	inStr, esc := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inStr {
			b = append(b, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b = append(b, c)
			continue
		}
		if c == '/' && i+1 < len(data) {
			if data[i+1] == '/' {
				for i < len(data) && data[i] != '\n' {
					i++
				}
				b = append(b, '\n')
				continue
			}
			if data[i+1] == '*' {
				i += 2
				for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
					i++
				}
				i++ // land on the closing '/'; the loop's i++ steps past it
				continue
			}
		}
		b = append(b, c)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == ',' {
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // drop a trailing comma
			}
		}
		out = append(out, b[i])
	}
	return out
}

// ValidateSnapshotContext proves that every file row about to be published was
// derived from the bytes still present on disk.
func ValidateSnapshotContext(ctx context.Context, s *store.Store, root string) error {
	return ValidateSnapshotWithExpectedContext(ctx, s, root, Options{}, nil)
}

// ValidateSnapshotWithExpectedContext validates both published rows and the
// pre-signature's expected indexable paths. The reverse check prevents a source
// that transiently disappeared during the index walk from being omitted.
func ValidateSnapshotWithExpectedContext(ctx context.Context, s *store.Store, root string, opt Options, expected []string) error {
	files, err := s.AllFiles()
	if err != nil {
		return err
	}
	byPath := make(map[string]store.File, len(files))
	for _, file := range files {
		byPath[file.RelPath] = file
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.RelPath)))
		if err != nil {
			return fmt.Errorf("validate indexed file %s: %w", file.RelPath, err)
		}
		data = stripGeneratedContent(file.RelPath, data)
		hash := strconv.FormatUint(xxhash.Sum64(data), 16)
		if int64(len(data)) != file.Size || hash != file.Hash {
			return fmt.Errorf("indexed file %s changed after it was read", file.RelPath)
		}
	}
	for _, rel := range expected {
		if err := ctx.Err(); err != nil {
			return err
		}
		if isGeneratedArtifact(rel) {
			continue // the indexer skips these; they are never expected in the store
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("%w: validate expected source %s: %v", ErrSourcesChanged, rel, err)
		}
		data = stripGeneratedContent(rel, data)
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		head := data
		if len(head) > 512 {
			head = head[:512]
		}
		lang := parse.Detect(rel, head)
		if lang == "" || !languageAllowed(lang, opt.Languages) {
			continue
		}
		if _, ok := byPath[rel]; !ok {
			return fmt.Errorf("%w: indexed source %s was omitted", ErrSourcesChanged, rel)
		}
	}
	return nil
}
