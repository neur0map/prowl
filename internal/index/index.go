package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/prowl-agent/prowl-agent/internal/graph"
	"github.com/prowl-agent/prowl-agent/internal/parse"
	"github.com/prowl-agent/prowl-agent/internal/parse/extract"
	"github.com/prowl-agent/prowl-agent/internal/store"
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

// Progress is a bounded, aggregate indexing snapshot. Reporters must not retain
// per-file state; progress is emitted only at traversal and file boundaries.
type Progress struct {
	Phase   string
	Percent int
}

// ProgressReporter receives optional bounded progress snapshots.
type ProgressReporter func(Progress)

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
func IndexWithOptionsContext(ctx context.Context, s *store.Store, root string, opt Options) (Summary, error) {
	return indexWithOptionsContext(ctx, s, root, opt, nil)
}

// IndexWithOptionsProgressContext is IndexWithOptionsContext with optional
// aggregate progress reporting.
func IndexWithOptionsProgressContext(ctx context.Context, s *store.Store, root string, opt Options, report ProgressReporter) (Summary, error) {
	if report != nil {
		report(Progress{Phase: "starting", Percent: 0})
	}
	summary, err := indexWithOptionsContext(ctx, s, root, opt, report)
	if err == nil && report != nil {
		report(Progress{Phase: "complete", Percent: 100})
	}
	return summary, err
}

func indexWithOptionsContext(ctx context.Context, s *store.Store, root string, opt Options, report ProgressReporter) (Summary, error) {
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
	rels, err := WalkContext(ctx, root, opt.Ignore)
	if err != nil {
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

	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		full := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(full)
		if err != nil {
			return sum, fmt.Errorf("%w: read indexed source %s: %v", ErrSourcesChanged, rel, err)
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
		current[rel] = true
		sum.Indexed++

		hash := strconv.FormatUint(xxhash.Sum64(data), 16)
		if existing, ok, err := s.GetFileByPath(rel); err != nil {
			return sum, err
		} else if !force && ok && existing.Hash == hash {
			sum.Skipped++
			continue
		}

		ex, ok := extract.For(lang)
		if !ok {
			continue
		}
		res, _ := ex.Extract(data) // partial extraction is acceptable; chunks still stored
		if lang == "qml" {
			b := filepath.Base(rel)
			res.Symbols = append(res.Symbols, extract.Symbol{
				Name: b[:len(b)-len(filepath.Ext(b))], Kind: "component", StartLine: 1, EndLine: 1,
			})
		}

		var mtime int64
		if info, err := os.Stat(full); err == nil {
			mtime = info.ModTime().Unix()
		}
		fid, err := s.UpsertFile(store.File{
			RelPath: rel, Lang: lang, Role: graph.InferRole(rel, lang),
			Size: int64(len(data)), Hash: hash, MTime: mtime,
		})
		if err != nil {
			return sum, err
		}
		syms, ress, edges, chunks := mapResult(res)
		if err := s.ReplaceFileGraph(fid, syms, ress, edges, chunks); err != nil {
			return sum, err
		}
		sum.Parsed++
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
	return sum, nil
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
	if !strings.EqualFold(filepath.Base(rel), "AGENTS.md") {
		return data
	}
	const startMarker = "<!-- prowl-agent -->"
	const endMarker = "<!-- /prowl-agent -->"
	content := string(data)
	for {
		start := strings.Index(content, startMarker)
		if start < 0 {
			break
		}
		endRel := strings.Index(content[start:], endMarker)
		if endRel < 0 {
			break
		}
		end := start + endRel + len(endMarker)
		content = content[:start] + content[end:]
	}
	return []byte(strings.TrimSpace(content))
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

func mapResult(r extract.Result) ([]store.Symbol, []store.Resource, []store.RawEdge, []store.Chunk) {
	syms := make([]store.Symbol, len(r.Symbols))
	for i, s := range r.Symbols {
		syms[i] = store.Symbol{Name: s.Name, Kind: s.Kind, Signature: s.Signature, StartLine: s.StartLine, EndLine: s.EndLine, ParentName: s.Parent, Complexity: s.Complexity}
	}
	ress := make([]store.Resource, len(r.Resources))
	for i, rs := range r.Resources {
		ress[i] = store.Resource{Kind: rs.Kind, Name: rs.Name, Value: rs.Value, Line: rs.Line}
	}
	edges := make([]store.RawEdge, len(r.Edges))
	for i, e := range r.Edges {
		edges[i] = store.RawEdge{SrcName: e.SrcName, Kind: e.Kind, Raw: e.Raw, Line: e.Line}
	}
	chunks := make([]store.Chunk, len(r.Chunks))
	for i, c := range r.Chunks {
		chunks[i] = store.Chunk{StartLine: c.StartLine, EndLine: c.EndLine, Text: c.Text}
	}
	return syms, ress, edges, chunks
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
