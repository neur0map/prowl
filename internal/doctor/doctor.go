// Package doctor computes deterministic health diagnostics for a project from the
// indexed graph: cyclic includes, fan-in/out risk, oversized configs, duplicate
// keybinds, broken commands, orphan scripts, dangling references, hardcoded
// colors, forbidden layer crossings, and git-churn hotspots.
package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/index"
	"github.com/neur0map/prowl/internal/parse"
	"github.com/neur0map/prowl/internal/store"
)

// Severity grades a finding.
type Severity string

const (
	SevError Severity = "error"
	SevWarn  Severity = "warn"
	SevInfo  Severity = "info"
)

const (
	ProfileGeneral = "general"
	ProfileRice    = "rice"
)

// Finding is one diagnostic result.
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Detail   string   `json:"detail"`
}

// Report is the full diagnostic output with a 0-100 health score.
type Report struct {
	Findings []Finding      `json:"findings"`
	Summary  map[string]int `json:"summary"`
	Score    int            `json:"score"`
}

// Options tunes thresholds.
type Options struct {
	Root            string
	Profile         string
	OversizedLines  int
	FanInThreshold  int
	FanOutThreshold int
	ChurnCommits    int
	Ignore          []string
	ExcludePaths    []string
	// IncludeExcluded reports findings under lifecycle paths (ExcludePaths)
	// instead of filtering them, so the full set -- including masked credentials
	// in test fixtures -- is reachable. The default view rolls those secrets into
	// a single info finding; this flag surfaces every one per file.
	IncludeExcluded bool
}

func (o Options) withDefaults() Options {
	if o.Profile == "" {
		o.Profile = ProfileGeneral
	}
	if o.OversizedLines == 0 {
		o.OversizedLines = 800
	}
	if o.FanInThreshold == 0 {
		o.FanInThreshold = 8
	}
	if o.FanOutThreshold == 0 {
		o.FanOutThreshold = 15
	}
	if o.ChurnCommits == 0 {
		o.ChurnCommits = 200
	}
	if o.ExcludePaths == nil {
		o.ExcludePaths = []string{"migrations/", "install/", "iso/", "shell-install/", ".github/", "vendor/", "legacy/", ".githooks/", "tests/"}
	}
	return o
}

// Run executes general repository checks by default. Desktop/rice checks are
// enabled only with ProfileRice so ordinary codebases do not receive irrelevant
// keybind, command, color, or launcher diagnostics.
func Run(s *store.Store, rules config.Rules, opt Options) (Report, error) {
	opt = opt.withDefaults()
	var f []Finding
	for _, fn := range []func(*store.Store, Options) ([]Finding, error){
		checkCycles, checkFan, checkOversized, checkDangling, checkUnindexedLanguages, hardcodedSecrets,
	} {
		got, err := fn(s, opt)
		if err != nil {
			return Report{}, err
		}
		f = append(f, got...)
	}
	if opt.Profile == ProfileRice {
		for _, fn := range []func(*store.Store, Options) ([]Finding, error){
			checkDuplicateKeybinds, checkBrokenCommands, checkOrphanScripts, checkHardcodedColors,
		} {
			got, err := fn(s, opt)
			if err != nil {
				return Report{}, err
			}
			f = append(f, got...)
		}
	}
	fb, err := checkForbidden(s, rules)
	if err != nil {
		return Report{}, err
	}
	f = append(f, fb...)
	f = append(f, checkChurn(s, opt)...) // best-effort; needs git
	if !opt.IncludeExcluded {
		kept, excluded := filterExcluded(f, opt.ExcludePaths)
		f = kept
		if roll := excludedSecretRollup(excluded, opt.ExcludePaths); roll != nil {
			f = append(f, *roll)
		}
	}

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Check != f[j].Check {
			return f[i].Check < f[j].Check
		}
		if f[i].File != f[j].File {
			return f[i].File < f[j].File
		}
		return f[i].Line < f[j].Line
	})

	summary := map[string]int{}
	weight := 0
	for _, x := range f {
		summary[x.Check]++
		switch x.Severity {
		case SevError:
			weight += 5
		case SevWarn:
			weight += 2
		}
	}
	score := 100 - weight
	if score < 0 {
		score = 0
	}
	return Report{Findings: f, Summary: summary, Score: score}, nil
}

// filterExcluded partitions findings by whether their file lives under a
// lifecycle directory (migrations, installers, CI, vendor, hooks, tests): kept
// are the live-graph findings, excluded are those under a prefix. Most excluded
// findings are dropped as non-actionable noise, but the caller rescues masked
// credentials from the excluded set (see excludedSecretRollup), because a leaked
// key is actionable wherever it lives -- test fixtures included.
func filterExcluded(findings []Finding, prefixes []string) (kept, excluded []Finding) {
	if len(prefixes) == 0 {
		return findings, nil
	}
	for _, fd := range findings {
		under := false
		for _, p := range prefixes {
			if fd.File != "" && strings.HasPrefix(fd.File, p) {
				under = true
				break
			}
		}
		if under {
			excluded = append(excluded, fd)
		} else {
			kept = append(kept, fd)
		}
	}
	return kept, excluded
}

// excludedSecretRollup summarises hardcoded_secret findings dropped by path
// exclusion into one info-severity finding, so a credential masked under a
// lifecycle path is never silently lost from the default report. It is info, not
// warn: the value is already neutralised in the index and the paths were
// excluded on purpose, so this must not trip `doctor --fail-on warn` on a repo
// whose only credential-shaped files are safe test fixtures. The per-file list
// stays reachable via `doctor --include-excluded`. Returns nil when none were
// excluded.
func excludedSecretRollup(excluded []Finding, prefixes []string) *Finding {
	files := 0
	matched := map[string]bool{}
	for _, fd := range excluded {
		if fd.Check != "hardcoded_secret" {
			continue
		}
		files++
		for _, p := range prefixes {
			if strings.HasPrefix(fd.File, p) {
				matched[p] = true
				break
			}
		}
	}
	if files == 0 {
		return nil
	}
	dirs := make([]string, 0, len(matched))
	for p := range matched {
		dirs = append(dirs, p)
	}
	sort.Strings(dirs)
	return &Finding{
		Check:    "hardcoded_secret_excluded",
		Severity: SevInfo,
		Detail: fmt.Sprintf(
			"%d additional file(s) under excluded paths (%s) also had credentials masked; run `prowl doctor --include-excluded` to list them",
			files, strings.Join(dirs, ", ")),
	}
}

func checkCycles(s *store.Store, _ Options) ([]Finding, error) {
	edges, err := s.FileDepEdges("includes")
	if err != nil {
		return nil, err
	}
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e.SrcFile] = append(adj[e.SrcFile], e.DstFile)
	}
	const white, gray, black = 0, 1, 2
	color := map[string]int{}
	var stack []string
	var cycles [][]string
	var dfs func(string)
	dfs = func(n string) {
		color[n] = gray
		stack = append(stack, n)
		for _, m := range adj[n] {
			switch color[m] {
			case gray:
				for i, x := range stack {
					if x == m {
						cycles = append(cycles, append([]string{}, stack[i:]...))
						break
					}
				}
			case white:
				dfs(m)
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if color[n] == white {
			dfs(n)
		}
	}
	var out []Finding
	seen := map[string]bool{}
	for _, c := range cycles {
		if len(c) < 2 {
			continue // ignore self-cycles (resolution artifacts)
		}
		key := strings.Join(c, ">")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Finding{Check: "cyclic_include", Severity: SevError, File: c[0],
			Detail: "include cycle: " + strings.Join(append(c, c[0]), " -> ")})
	}
	return out, nil
}

func checkFan(s *store.Store, opt Options) ([]Finding, error) {
	var out []Finding
	in, err := s.FanIn(100, "pkg")
	if err != nil {
		return nil, err
	}
	for _, r := range in {
		if r.In >= opt.FanInThreshold {
			out = append(out, Finding{Check: "fan_in_risk", Severity: SevWarn, File: r.File,
				Detail: fmt.Sprintf("%d files depend on this; edits ripple widely", r.In)})
		}
	}
	o, err := s.FanOut(100, "pkg")
	if err != nil {
		return nil, err
	}
	for _, r := range o {
		if r.In >= opt.FanOutThreshold {
			out = append(out, Finding{Check: "fan_out_risk", Severity: SevWarn, File: r.File,
				Detail: fmt.Sprintf("references %d other files; a monolithic config", r.In)})
		}
	}
	return out, nil
}

func checkOversized(s *store.Store, opt Options) ([]Finding, error) {
	m, err := s.FileMetrics()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, x := range m {
		if isDataFile(x.File) || x.Lines < opt.OversizedLines {
			continue // data files (translations, indexes) are not splittable configs
		}
		out = append(out, Finding{Check: "oversized_file", Severity: SevWarn, File: x.File,
			Detail: fmt.Sprintf("%d lines; consider splitting", x.Lines)})
	}
	return out, nil
}

func checkDuplicateKeybinds(s *store.Store, _ Options) ([]Finding, error) {
	kb, err := s.SymbolsByKind("keybind")
	if err != nil {
		return nil, err
	}
	wmFiles := map[string]bool{}
	if wm, err := s.FilesByRole("wm-config"); err == nil {
		for _, f := range wm {
			wmFiles[f.RelPath] = true
		}
	}
	groups := map[string][]store.SymbolHit{}
	for _, k := range kb {
		if !wmFiles[k.File] {
			continue // keybind conflicts only matter within a window manager
		}
		groups[canonKey(k.Name)] = append(groups[canonKey(k.Name)], k)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Finding
	for _, key := range keys {
		ks := groups[key]
		if len(ks) < 2 {
			continue
		}
		var locs []string
		for _, k := range ks {
			locs = append(locs, fmt.Sprintf("%s:%d", k.File, k.Line))
		}
		out = append(out, Finding{Check: "duplicate_keybind", Severity: SevWarn, File: ks[0].File, Line: ks[0].Line,
			Detail: fmt.Sprintf("key %q bound %d times: %s", key, len(ks), strings.Join(locs, ", "))})
	}
	return out, nil
}

func canonKey(name string) string {
	toks := strings.Fields(strings.NewReplacer(",", " ", "+", " ").Replace(strings.ToLower(name)))
	sort.Strings(toks)
	return strings.Join(toks, "+")
}

func checkBrokenCommands(s *store.Store, _ Options) ([]Finding, error) {
	ex, err := s.UnresolvedEdges("execs", "binds", "autostarts")
	if err != nil {
		return nil, err
	}
	var out []Finding
	seen := map[string]bool{}
	for _, e := range ex {
		cmd := firstToken(e.Raw)
		if cmd == "" || strings.ContainsAny(cmd, "/$") {
			continue // path or variable, not a bare binary
		}
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		if _, err := exec.LookPath(cmd); err != nil {
			out = append(out, Finding{Check: "broken_command", Severity: SevWarn, File: e.File, Line: e.Line,
				Detail: "command not on PATH: " + cmd})
		}
	}
	return out, nil
}

func firstToken(s string) string {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return ""
	}
	return strings.Trim(f[0], `"'`)
}

func checkOrphanScripts(s *store.Store, _ Options) ([]Finding, error) {
	var out []Finding
	orphans, err := s.OrphanFiles("script")
	if err != nil {
		return nil, err
	}
	for _, o := range orphans {
		if strings.HasPrefix(o.RelPath, "bin/") {
			continue // a user-invoked command suite, not config-referenced scripts
		}
		out = append(out, Finding{Check: "orphan_script", Severity: SevWarn, File: o.RelPath,
			Detail: "script not referenced by any config or keybind"})
	}
	return out, nil
}

func checkDangling(s *store.Store, _ Options) ([]Finding, error) {
	var out []Finding
	dang, err := s.UnresolvedEdges("includes")
	if err != nil {
		return nil, err
	}
	langByID, err := fileLangByID(s)
	if err != nil {
		return nil, err
	}
	for _, e := range dang {
		if store.ModuleImportLang(langByID[e.FileID]) {
			continue // external module import, not a broken project reference
		}
		if !repoRelative(e.Raw) {
			continue // skip runtime (~), system (/), vars, URLs, and external modules
		}
		out = append(out, Finding{Check: "dangling_reference", Severity: SevError, File: e.File, Line: e.Line,
			Detail: e.Kind + ": " + e.Raw})
	}
	return out, nil
}

// hardcodedSecrets reports files where a credential was masked at index time,
// reading the per-file count the indexer persisted in files.redacted. That
// persisted count is the durable evidence: it records that a value was removed
// while the value itself was never stored. The check must not scan chunk text
// for the mask marker -- the source that defines the marker (redact.go's
// const Mask) would then be falsely reported as holding a credential. It is a
// warning, not an error: the secret is already neutralised in the index, so this
// is a repository-hygiene signal (doctor --fail-on gates CI when needed).
func hardcodedSecrets(s *store.Store, _ Options) ([]Finding, error) {
	rows, err := s.FilesWithRedactions()
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, Finding{
			Check:    "hardcoded_secret",
			Severity: SevWarn,
			File:     r.File,
			Detail:   fmt.Sprintf("%d masked credential value(s); move them to the environment", r.Count),
		})
	}
	return out, nil
}

// fileLangByID maps each indexed file id to its language.
func fileLangByID(s *store.Store) (map[int64]string, error) {
	files, err := s.AllFiles()
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(files))
	for _, f := range files {
		m[f.ID] = f.Lang
	}
	return m, nil
}

// repoRelative reports whether a raw include target should resolve inside the
// repo (a genuinely broken include if it does not): a relative slash path or a
// file with an extension, excluding URLs, home (~), system (/), and vars.
func repoRelative(v string) bool {
	v = strings.TrimSpace(strings.Trim(v, `"'`))
	if v == "" || strings.Contains(v, "://") ||
		strings.HasPrefix(v, "~") || strings.HasPrefix(v, "/") ||
		strings.HasPrefix(v, "$") || strings.HasPrefix(v, "@") {
		return false
	}
	return strings.Contains(v, "/") || hasFileExt(v)
}

var includeExts = map[string]bool{
	".css": true, ".scss": true, ".conf": true, ".sh": true, ".bash": true, ".fish": true,
	".py": true, ".lua": true, ".qml": true, ".toml": true, ".yaml": true, ".yml": true,
	".json": true, ".jsonc": true, ".ini": true, ".rasi": true, ".rasi2": true,
}

// hasFileExt reports whether v ends in a known config/script extension (so a
// dotted module like "urllib.error" is not mistaken for a file).
func hasFileExt(v string) bool {
	b := v
	if i := strings.LastIndexByte(v, '/'); i >= 0 {
		b = v[i+1:]
	}
	d := strings.LastIndexByte(b, '.')
	return d > 0 && includeExts[strings.ToLower(b[d:])]
}

func isDataFile(p string) bool {
	for _, ext := range []string{".json", ".jsonc", ".yaml", ".yml"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

func checkHardcodedColors(s *store.Store, _ Options) ([]Finding, error) {
	res, err := s.AllResources()
	if err != nil {
		return nil, err
	}
	declByValue := map[string]string{}
	for _, r := range res {
		if r.Name != "" && r.Value != "" {
			declByValue[r.Value] = r.Name
		}
	}
	var out []Finding
	for _, r := range res {
		if r.Name == "" && r.Value != "" {
			if name, ok := declByValue[r.Value]; ok {
				out = append(out, Finding{Check: "hardcoded_color", Severity: SevInfo, File: r.File, Line: r.Line,
					Detail: r.Value + " duplicates variable " + name})
			}
		}
	}
	return out, nil
}

func checkForbidden(s *store.Store, rules config.Rules) ([]Finding, error) {
	if len(rules.Forbid) == 0 {
		return nil, nil
	}
	edges, err := s.FileDepEdges()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, fb := range rules.Forbid {
		for _, e := range edges {
			if globMatch(fb.From, e.SrcFile) && globMatch(fb.To, e.DstFile) {
				name := fb.Name
				if name == "" {
					name = "forbidden crossing"
				}
				out = append(out, Finding{Check: "forbidden_crossing", Severity: SevError, File: e.SrcFile, Line: e.Line,
					Detail: name + ": " + e.SrcFile + " -> " + e.DstFile})
			}
		}
	}
	return out, nil
}

func globMatch(pattern, path string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return strings.Contains(path, pattern)
	}
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	if ok, _ := filepath.Match(pattern, filepath.Base(path)); ok {
		return true
	}
	return false
}

func checkChurn(s *store.Store, opt Options) []Finding {
	if opt.Root == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", opt.Root, "log", "--no-merges",
		"--pretty=format:", "--name-only", "-n", strconv.Itoa(opt.ChurnCommits)).Output()
	if err != nil {
		return nil // not a git repo
	}
	counts := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			counts[line]++
		}
	}
	files, err := s.AllFiles()
	if err != nil {
		return nil
	}
	indexed := map[string]bool{}
	for _, f := range files {
		indexed[f.RelPath] = true
	}
	type cf struct {
		file string
		n    int
	}
	var list []cf
	for f, n := range counts {
		if indexed[f] && n >= 5 {
			list = append(list, cf{f, n})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].file < list[j].file
	})
	var findings []Finding
	for i, c := range list {
		if i >= 10 {
			break
		}
		findings = append(findings, Finding{Check: "churn_hotspot", Severity: SevInfo, File: c.file,
			Detail: fmt.Sprintf("changed %d times recently; review for stability", c.n)})
	}
	return findings
}

// UnindexedLanguageWarnings reports, as human-readable strings, any language
// that is well represented on disk but barely indexed -- almost always a
// `languages` filter in .prowl/config.toml silently excluding the repo's real
// stack. `init` and `status` surface these so the failure is loud and actionable
// instead of showing up only as empty query results. It returns nil when the
// index is healthy.
func UnindexedLanguageWarnings(s *store.Store, root string, ignore []string) []string {
	findings, err := checkUnindexedLanguages(s, Options{Root: root, Ignore: ignore})
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Detail)
	}
	return out
}

// checkUnindexedLanguages flags a language that is well represented on disk but
// barely or not indexed. The usual cause is a `languages` filter in
// .prowl/config.toml that omits the repository's real stack (e.g. a Go project
// carrying a config copied from a QML/rice setup), which silently leaves prowl
// with a near-empty index and no other symptom -- every query just comes back
// empty. Detection is by file extension (no reads) over the same ignore-aware
// walk the indexer uses, so the on-disk-vs-indexed gap isolates the language
// filter's effect.
func checkUnindexedLanguages(s *store.Store, opt Options) ([]Finding, error) {
	if opt.Root == "" {
		return nil, nil
	}
	files, err := s.AllFiles()
	if err != nil {
		return nil, err
	}
	indexed := map[string]int{}
	for _, f := range files {
		indexed[f.Lang]++
	}
	rels, err := index.WalkContext(context.Background(), opt.Root, opt.Ignore)
	if err != nil {
		return nil, nil // a transient walk failure is not a health finding
	}
	onDisk := map[string]int{}
	for _, rel := range rels {
		if lang := parse.Detect(rel, nil); lang != "" && parse.HasGrammar(lang) {
			onDisk[lang]++
		}
	}
	var out []Finding
	for lang, n := range onDisk {
		// Present in force but under a fifth indexed: the language filter is
		// almost certainly excluding it. 10-file floor avoids incidental files.
		if n >= 10 && indexed[lang]*5 < n {
			out = append(out, Finding{
				Check:    "unindexed-language",
				Severity: SevWarn,
				Detail: fmt.Sprintf("%d %s files present but %d indexed -- the `languages` filter in .prowl/config.toml likely omits %q; add it or set languages to [\"auto\"]",
					n, lang, indexed[lang], lang),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Detail < out[j].Detail })
	return out, nil
}
