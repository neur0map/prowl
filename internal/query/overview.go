package query

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/neur0map/prowl/internal/store"
)

// Cluster is a subsystem: either a connected group of files (via includes,
// exec/keybind chains, and shared resources) or, when a cohesive codebase forms
// one giant component, a directory grouping of it. Lang is the dominant language.
type Cluster struct {
	Label string   `json:"label"`
	Lang  string   `json:"lang"`
	Files []string `json:"files"`
}

// Clusters groups files connected through includes, exec/keybind chains, and
// shared resources into subsystems via connected components. Singletons are
// omitted. Clusters are ordered by size, then label.
func (q *Querier) Clusters() ([]Cluster, error) {
	release, err := q.beginRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	files, err := q.s.AllFiles()
	if err != nil {
		return nil, err
	}
	uf := newUnionFind()
	langOf := make(map[string]string, len(files))
	for _, f := range files {
		uf.add(f.RelPath)
		langOf[f.RelPath] = f.Lang
	}
	dep, err := q.s.FileDepEdges()
	if err != nil {
		return nil, err
	}
	for _, e := range dep {
		uf.union(e.SrcFile, e.DstFile)
	}
	res, err := q.s.ResourceFileLinks()
	if err != nil {
		return nil, err
	}
	for _, e := range res {
		uf.union(e.SrcFile, e.DstFile)
	}

	var clusters []Cluster
	for _, members := range uf.groups() {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		clusters = append(clusters, Cluster{Label: clusterLabel(members), Lang: dominantLang(members, langOf), Files: members})
	}
	clusters = splitBlobCluster(clusters, langOf)
	sortClusters(clusters)
	return clusters, nil
}

func sortClusters(clusters []Cluster) {
	sort.Slice(clusters, func(i, j int) bool {
		if len(clusters[i].Files) != len(clusters[j].Files) {
			return len(clusters[i].Files) > len(clusters[j].Files)
		}
		return clusters[i].Label < clusters[j].Label
	})
}

// splitBlobCluster detects a single dominant connected component (a cohesive
// codebase where nearly everything is one component) and subdivides it by
// directory subsystem, so clusters stay a useful onboarding map instead of one
// opaque blob. Repos with several balanced components (e.g. config include
// trees) are left unchanged.
func splitBlobCluster(clusters []Cluster, langOf map[string]string) []Cluster {
	if len(clusters) == 0 {
		return clusters
	}
	sortClusters(clusters)
	total := 0
	for _, c := range clusters {
		total += len(c.Files)
	}
	big := clusters[0]
	if len(big.Files) < 25 || len(big.Files)*100 < total*70 {
		return clusters
	}
	byDir := map[string][]string{}
	for _, m := range big.Files {
		byDir[subsystem(m)] = append(byDir[subsystem(m)], m)
	}
	if len(byDir) < 2 {
		return clusters
	}
	out := append([]Cluster(nil), clusters[1:]...)
	for dir, fs := range byDir {
		if len(fs) < 2 {
			continue
		}
		sort.Strings(fs)
		out = append(out, Cluster{Label: dir, Lang: dominantLang(fs, langOf), Files: fs})
	}
	return out
}

// clusterLabel names a cluster by the most common directory subsystem (up to two
// path segments), so a monorepo's packages/foo and packages/bar get distinct
// labels instead of both collapsing to "packages".
func clusterLabel(members []string) string {
	counts := map[string]int{}
	for _, m := range members {
		counts[subsystem(m)]++
	}
	labels := make([]string, 0, len(counts))
	for s := range counts {
		labels = append(labels, s)
	}
	sort.Strings(labels)
	best, bestN := "", 0
	for _, s := range labels {
		if counts[s] > bestN {
			best, bestN = s, counts[s]
		}
	}
	if best == "" || best == "." {
		return "misc"
	}
	return best
}

// dominantLang returns the most common language among a cluster's files.
func dominantLang(members []string, langOf map[string]string) string {
	counts := map[string]int{}
	for _, m := range members {
		counts[langOf[m]]++
	}
	best, bestN := "", 0
	for l, n := range counts {
		if n > bestN || (n == bestN && l < best) {
			best, bestN = l, n
		}
	}
	return best
}

// guideDocs picks the project's architecture/onboarding docs from indexed
// markdown, so an agent's first call points it at the human-written guides
// (README, ARCHITECTURE, CONTRIBUTING, docs/** guides) before it reads code.
// Ranked: root README, then other root guides, then docs/ guides; capped at 8.
func guideDocs(files []store.File) []string {
	type scored struct {
		path string
		rank int
	}
	var picks []scored
	rootGuides := map[string]bool{
		"architecture.md": true, "contributing.md": true, "development.md": true,
		"develop.md": true, "hacking.md": true, "design.md": true, "agents.md": true,
	}
	for _, f := range files {
		if f.Lang != "markdown" {
			continue
		}
		rel := f.RelPath
		base := strings.ToLower(path.Base(rel))
		lower := strings.ToLower(rel)
		depth := strings.Count(rel, "/")
		switch {
		case depth == 0 && base == "readme.md":
			picks = append(picks, scored{rel, 0})
		case depth == 0 && rootGuides[base]:
			picks = append(picks, scored{rel, 1})
		case (strings.HasPrefix(lower, "docs/") || strings.HasPrefix(lower, "doc/")) && matchesGuide(lower):
			picks = append(picks, scored{rel, 2})
		}
	}
	sort.Slice(picks, func(i, j int) bool {
		if picks[i].rank != picks[j].rank {
			return picks[i].rank < picks[j].rank
		}
		return picks[i].path < picks[j].path
	})
	out := make([]string, 0, len(picks))
	for _, p := range picks {
		if len(out) >= 8 {
			break
		}
		out = append(out, p.path)
	}
	return out
}

// matchesGuide reports whether a doc path looks like an architecture/onboarding
// guide rather than reference material.
func matchesGuide(s string) bool {
	for _, kw := range []string{
		"guide", "architecture", "develop", "contribut", "design", "overview",
		"getting-started", "getting_started", "structure", "codebase", "hacking", "onboard",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// ClusterSummary is a cluster in the overview map: its label, dominant language,
// and file count. The full file list lives in the `clusters` command, so the
// overview (an agent's first call) stays a compact map, not a file dump.
type ClusterSummary struct {
	Label string `json:"label"`
	Lang  string `json:"lang"`
	Files int    `json:"files"`
}

// Overview is a compact map of the whole project for an agent's first call.
type Overview struct {
	Counts          store.Counts        `json:"counts"`
	Roles           map[string]int      `json:"roles"`
	Docs            []string            `json:"docs"`
	Entrypoints     []string            `json:"entrypoints"`
	EntrypointCount int                 `json:"entrypoint_count"`
	Clusters        []ClusterSummary    `json:"clusters"`
	Palette         []store.ResourceRow `json:"palette"`
	Keybinds        int                 `json:"keybinds"`
	Hotspots        []store.FanRow      `json:"hotspots"`
}

// OverviewLimits bounds Overview's output shape and the size of any single
// metadata string. It deliberately does not bound repository size: overview is
// an agent's first call, so on a large repo it must still answer with a compact,
// honest map instead of refusing. Bulk inputs (files, edges, resource links) are
// read in full, exactly as the `clusters` command already does.
type OverviewLimits struct {
	Palette, Hotspots           int
	Languages, Roles            int
	Docs, Entrypoints, Clusters int
	StringBytes                 int
}

// DefaultOverviewLimits returns fresh limits preserving Overview's compact output
// while preventing callers from mutating process-global defaults.
func DefaultOverviewLimits() OverviewLimits {
	return OverviewLimits{
		Palette: 16, Hotspots: 5,
		Languages: 64, Roles: 64,
		Docs: 8, Entrypoints: 20, Clusters: 8,
		StringBytes: 4096,
	}
}

// BoundedWorkError reports that a derived projection exceeded a hard limit.
type BoundedWorkError struct {
	Component string
	Limit     int
}

func (err *BoundedWorkError) Error() string {
	return fmt.Sprintf("overview %s exceeds limit %d", err.Component, err.Limit)
}

// Overview is the compatibility wrapper for the bounded context-aware path.
func (q *Querier) Overview() (Overview, error) {
	return q.OverviewContext(context.Background(), DefaultOverviewLimits())
}

// OverviewContext assembles the high-level map and passes ctx to every SQL read.
func (q *Querier) OverviewContext(ctx context.Context, limits OverviewLimits) (Overview, error) {
	if err := validateOverviewLimits(limits); err != nil {
		return Overview{}, err
	}
	release, err := q.beginRead(ctx)
	if err != nil {
		return Overview{}, err
	}
	defer release()
	var o Overview
	c, err := q.s.CountsContext(ctx)
	if err != nil {
		return o, err
	}
	if len(c.Langs) > limits.Languages {
		return o, bounded("languages", limits.Languages)
	}
	if err := validateIdentifierMap(c.Langs, limits.StringBytes, "language identifiers"); err != nil {
		return o, err
	}
	o.Counts = c

	files, err := q.s.AllFilesContext(ctx)
	if err != nil {
		return o, err
	}
	if q.afterOverviewRead != nil {
		q.afterOverviewRead()
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	o.Roles = map[string]int{}
	for _, f := range files {
		if err := validateOverviewPath(f.RelPath, limits.StringBytes); err != nil {
			return o, err
		}
		if err := validateOverviewIdentifier(f.Lang, limits.StringBytes, "language identifiers", false); err != nil {
			return o, err
		}
		if err := validateOverviewIdentifier(f.Role, limits.StringBytes, "role identifiers", true); err != nil {
			return o, err
		}
		role := f.Role
		if role == "" {
			role = "other"
		}
		o.Roles[role]++
		if len(o.Roles) > limits.Roles {
			return o, bounded("roles", limits.Roles)
		}
	}
	o.Docs = guideDocs(files)
	if len(o.Docs) > limits.Docs {
		o.Docs = o.Docs[:limits.Docs]
	}

	// Entrypoints: code files that depend on others but no other code depends on
	// them.
	dep, err := q.s.FileDepEdgesContext(ctx)
	if err != nil {
		return o, err
	}
	langOf := make(map[string]string, len(files))
	for _, file := range files {
		langOf[file.RelPath] = file.Lang
	}
	// "Nothing imports it" on its own admits every manifest, config table, and
	// test file: a package.json carries one dependency edge per dependency and
	// nothing imports a test, so both outrank the program's real entry points.
	// Only a language that can start a program qualifies, and test code never
	// does.
	eligible := func(path string) bool {
		return executableLang(langOf[path]) && !isTestPath(path)
	}
	hasIncoming := map[string]bool{}
	outDegree := map[string]int{}
	for _, e := range dep {
		if err := validateOverviewPath(e.SrcFile, limits.StringBytes); err != nil {
			return o, err
		}
		if err := validateOverviewPath(e.DstFile, limits.StringBytes); err != nil {
			return o, err
		}
		if err := validateOverviewStrings(limits.StringBytes, e.Kind); err != nil {
			return o, err
		}
		outDegree[e.SrcFile]++
		// Only an import from eligible code counts as a dependant. A test
		// importing main.go does not stop main.go being where the program starts,
		// and a manifest listing it does not either -- counting those hid the
		// entry point of every project whose main module has a unit test.
		if eligible(e.SrcFile) {
			hasIncoming[e.DstFile] = true
		}
	}
	for path := range outDegree {
		if hasIncoming[path] || !eligible(path) {
			continue
		}
		o.Entrypoints = append(o.Entrypoints, path)
	}
	o.EntrypointCount = len(o.Entrypoints)
	// Rank by how much each entry pulls in, which is what makes an entry point
	// worth reading first. PageRank centrality cannot order these: an entrypoint
	// has no incoming edges by construction, so every candidate would score the
	// same base value.
	sort.Slice(o.Entrypoints, func(i, j int) bool {
		a, b := o.Entrypoints[i], o.Entrypoints[j]
		if outDegree[a] != outDegree[b] {
			return outDegree[a] > outDegree[b]
		}
		if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
			return da < db
		}
		return a < b
	})
	if len(o.Entrypoints) > limits.Entrypoints {
		o.Entrypoints = o.Entrypoints[:limits.Entrypoints]
	}

	links, err := q.s.ResourceFileLinksContext(ctx)
	if err != nil {
		return o, err
	}
	uf := newUnionFind()
	for _, file := range files {
		uf.add(file.RelPath)
	}
	for _, edge := range dep {
		uf.union(edge.SrcFile, edge.DstFile)
	}
	for _, edge := range links {
		if err := validateOverviewPath(edge.SrcFile, limits.StringBytes); err != nil {
			return o, err
		}
		if err := validateOverviewPath(edge.DstFile, limits.StringBytes); err != nil {
			return o, err
		}
		uf.union(edge.SrcFile, edge.DstFile)
	}
	var clusters []Cluster
	for _, members := range uf.groups() {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		clusters = append(clusters, Cluster{Label: clusterLabel(members), Lang: dominantLang(members, langOf), Files: members})
	}
	clusters = splitBlobCluster(clusters, langOf)
	sortClusters(clusters)
	if len(clusters) > limits.Clusters {
		clusters = clusters[:limits.Clusters]
	}
	o.Clusters = make([]ClusterSummary, len(clusters))
	for i, c := range clusters {
		o.Clusters[i] = ClusterSummary{Label: c.Label, Lang: c.Lang, Files: len(c.Files)}
	}

	// Palette and hotspots are bounded output samples.
	o.Palette, err = q.s.ColorPaletteContext(ctx, limits.Palette)
	if err != nil {
		return o, err
	}

	for _, row := range o.Palette {
		if err := validateOverviewIdentifier(row.Kind, limits.StringBytes, "resource kind identifiers", false); err != nil {
			return o, err
		}
		if err := validateOverviewStrings(limits.StringBytes, row.Name, row.Value); err != nil {
			return o, err
		}
		if err := validateOverviewPath(row.File, limits.StringBytes); err != nil {
			return o, err
		}
	}
	o.Keybinds, err = q.s.CountSymbolsByKindContext(ctx, "keybind")
	if err != nil {
		return o, err
	}
	o.Hotspots = centralFromEdges(dep, limits.Hotspots)

	for _, row := range o.Hotspots {
		if err := validateOverviewPath(row.File, limits.StringBytes); err != nil {
			return o, err
		}
	}
	if err := ctx.Err(); err != nil {
		return o, err
	}
	if o.Docs == nil {
		o.Docs = []string{}
	}
	if o.Entrypoints == nil {
		o.Entrypoints = []string{}
	}
	if o.Clusters == nil {
		o.Clusters = []ClusterSummary{}
	}
	if o.Palette == nil {
		o.Palette = []store.ResourceRow{}
	}
	if o.Hotspots == nil {
		o.Hotspots = []store.FanRow{}
	}
	return o, nil
}

func bounded(component string, limit int) error {
	return &BoundedWorkError{Component: component, Limit: limit}
}

func validateOverviewLimits(limits OverviewLimits) error {
	values := []struct {
		name  string
		value int
		max   int
	}{
		{"palette", limits.Palette, 16}, {"hotspots", limits.Hotspots, 5},
		{"languages", limits.Languages, 64}, {"roles", limits.Roles, 64},
		{"docs", limits.Docs, 8}, {"entrypoints", limits.Entrypoints, 20},
		{"clusters", limits.Clusters, 8}, {"string bytes", limits.StringBytes, 4096},
	}
	for _, item := range values {
		if item.value <= 0 {
			return fmt.Errorf("overview %s limit must be positive", item.name)
		}
		if item.value > item.max {
			return fmt.Errorf("overview %s limit exceeds supported maximum %d", item.name, item.max)
		}
	}
	return nil
}

func validateOverviewStrings(max int, values ...string) error {
	for _, value := range values {
		if !utf8.ValidString(value) || len(value) > max || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return bounded("strings", max)
		}
	}
	return nil
}

func validateOverviewIdentifier(value string, max int, component string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if err := validateOverviewStrings(max, value); err != nil || value == "" || value == "." || value == ".." ||
		strings.ContainsAny(value, `/\`) || filepath.IsAbs(value) || path.IsAbs(value) ||
		(len(value) >= 2 && value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z'))) {
		return bounded(component, max)
	}
	return nil
}

func validateOverviewPath(value string, max int) error {
	if err := validateOverviewStrings(max, value); err != nil || value == "" || strings.Contains(value, "\\") || filepath.IsAbs(value) || path.IsAbs(value) {
		return bounded("paths", max)
	}
	if len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' {
		return bounded("paths", max)
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return bounded("paths", max)
	}
	return nil
}

func validateIdentifierMap(values map[string]int, max int, component string) error {
	for value := range values {
		if err := validateOverviewIdentifier(value, max, component, false); err != nil {
			return err
		}
	}
	return nil
}

// unionFind is a small string-keyed disjoint-set.
type unionFind struct {
	parent map[string]string
	size   map[string]int
}

func newUnionFind() *unionFind {
	return &unionFind{parent: map[string]string{}, size: map[string]int{}}
}

func (u *unionFind) add(x string) {
	if _, ok := u.parent[x]; !ok {
		u.parent[x] = x
		u.size[x] = 1
	}
}

func (u *unionFind) find(x string) string {
	u.add(x)
	for u.parent[x] != x {
		u.parent[x] = u.parent[u.parent[x]]
		x = u.parent[x]
	}
	return x
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.size[ra] < u.size[rb] {
		ra, rb = rb, ra
	}
	u.parent[rb] = ra
	u.size[ra] += u.size[rb]
}

func (u *unionFind) groups() map[string][]string {
	g := map[string][]string{}
	for x := range u.parent {
		r := u.find(x)
		g[r] = append(g[r], x)
	}
	return g
}

// dataLangs are the indexed languages that only ever describe or configure a
// program, never start one. They carry dependency edges -- a manifest names its
// packages, a doc links its siblings -- and nothing imports them back, so
// without this they dominate the entrypoint list.
//
// Deliberately narrow: config dialects such as ini, hyprlang, and CSS stay
// eligible, because in a dotfiles or theming repo the file that sources every
// other config really is that project's entry point.
var dataLangs = map[string]bool{
	"json": true, "yaml": true, "toml": true, "markdown": true,
}

// executableLang reports whether a file's language can be a program entry point.
// An unknown or empty language stays eligible: guessing wrong should keep a real
// entry point rather than silently drop it.
func executableLang(lang string) bool { return !dataLangs[lang] }
