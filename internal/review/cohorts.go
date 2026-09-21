package review

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/neur0map/prowl/internal/query"
)

// GraphQueries is the complete graph surface used by planning. Keeping it this
// narrow makes enrichment deterministic and independently failure-injectable.
type GraphQueries interface {
	Clusters() ([]query.Cluster, error)
	FileRelations(path string) (query.Relations, error)
	BlastSummarize(path string) (query.BlastSummary, error)
	EntrypointsFor(path string) (query.EntrypointSet, error)
	TestsFor(path string) (query.TestsResult, error)
}

// Stable role identifiers. Roles are non-exclusive facts, never findings.
const (
	RoleContract       = "contract/type/schema/migration"
	RoleImplementation = "implementation"
	RoleConsumer       = "consumer/integration/entrypoint"
	RoleTest           = "test"
	RoleOperations     = "configuration/build/deployment"
	RoleDocumentation  = "documentation"
	RoleMechanical     = "generated/vendor/dependency/lock"
	RoleUnknown        = "unknown/unindexed"
)

// GraphErrorClass deliberately excludes provider messages and machine paths.
type GraphErrorClass string

const (
	GraphErrorQueryFailed GraphErrorClass = "query_failed"
	GraphErrorCanceled    GraphErrorClass = "canceled"
	GraphErrorDeadline    GraphErrorClass = "deadline_exceeded"
	GraphErrorNotFound    GraphErrorClass = "not_found"
	GraphErrorPermission  GraphErrorClass = "permission_denied"
	GraphErrorUnavailable GraphErrorClass = "unavailable"
)

// GraphOmission is a typed, stable enrichment omission. Err is retained for
// errors.Is/debugging but never contributes to Error(), signals, or identities.
type GraphOmission struct {
	Query      string
	Path       string
	ErrorClass GraphErrorClass
	Err        error
}

func (o GraphOmission) Error() string {
	if o.Path == "" {
		return fmt.Sprintf("review: graph omission query=%s class=%s", o.Query, o.ErrorClass)
	}
	return fmt.Sprintf("review: graph omission query=%s path=%s class=%s", o.Query, o.Path, o.ErrorClass)
}
func (o GraphOmission) Unwrap() error { return o.Err }

// FileFacts contains only indexed query results and deterministic path facts.
type FileFacts struct {
	Path        string
	Roles       []string
	Relations   query.Relations
	Blast       query.BlastSummary
	Entrypoints query.EntrypointSet
	Tests       query.TestsResult
	Signals     []AttentionSignal
}

// GraphEnrichment is the all-or-conservative result of graph collection.
type GraphEnrichment struct {
	Clusters  []query.Cluster
	Facts     map[string]FileFacts
	Omissions []GraphOmission
}

// EnrichGraph runs every graph query independently. One failure never prevents
// the other facts from being retained and never removes the owning path.
func EnrichGraph(ctx context.Context, paths []string, graph GraphQueries) GraphEnrichment {
	paths = sortedUniqueStrings(paths)
	out := GraphEnrichment{Facts: make(map[string]FileFacts, len(paths))}
	if graph == nil {
		out.Omissions = append(out.Omissions, graphOmission("Clusters", "", errors.New("unavailable")))
	} else if err := ctx.Err(); err != nil {
		out.Omissions = append(out.Omissions, graphOmission("Clusters", "", err))
	} else if clusters, err := graph.Clusters(); err != nil {
		out.Omissions = append(out.Omissions, graphOmission("Clusters", "", err))
	} else {
		out.Clusters = canonicalClusters(clusters)
	}

	for _, p := range paths {
		facts := FileFacts{Path: p, Relations: query.Relations{File: p}, Blast: query.BlastSummary{File: p}, Entrypoints: query.EntrypointSet{File: p}, Tests: query.TestsResult{File: p}}
		if graph == nil {
			for _, name := range []string{"FileRelations", "BlastSummarize", "EntrypointsFor", "TestsFor"} {
				out.Omissions = append(out.Omissions, graphOmission(name, p, errors.New("unavailable")))
			}
		} else {
			if err := ctx.Err(); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("FileRelations", p, err))
			} else if value, err := graph.FileRelations(p); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("FileRelations", p, err))
			} else {
				facts.Relations = value
			}
			if err := ctx.Err(); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("BlastSummarize", p, err))
			} else if value, err := graph.BlastSummarize(p); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("BlastSummarize", p, err))
			} else {
				facts.Blast = value
			}
			if err := ctx.Err(); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("EntrypointsFor", p, err))
			} else if value, err := graph.EntrypointsFor(p); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("EntrypointsFor", p, err))
			} else {
				facts.Entrypoints = value
			}
			if err := ctx.Err(); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("TestsFor", p, err))
			} else if value, err := graph.TestsFor(p); err != nil {
				out.Omissions = append(out.Omissions, graphOmission("TestsFor", p, err))
			} else {
				facts.Tests = value
			}
		}
		facts.Roles = deriveFileRoles(facts)
		facts.Signals = deriveGraphSignals(facts)
		out.Facts[p] = facts
	}
	sort.Slice(out.Omissions, func(i, j int) bool {
		a, b := out.Omissions[i], out.Omissions[j]
		return cmp.Or(strings.Compare(a.Query, b.Query), strings.Compare(a.Path, b.Path), strings.Compare(string(a.ErrorClass), string(b.ErrorClass))) < 0
	})
	return out
}

func graphOmission(name, p string, err error) GraphOmission {
	return GraphOmission{Query: name, Path: p, ErrorClass: classifyGraphError(err), Err: err}
}

func classifyGraphError(err error) GraphErrorClass {
	switch {
	case errors.Is(err, context.Canceled):
		return GraphErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return GraphErrorDeadline
	case errors.Is(err, fs.ErrNotExist):
		return GraphErrorNotFound
	case errors.Is(err, fs.ErrPermission):
		return GraphErrorPermission
	case err != nil && err.Error() == "unavailable":
		return GraphErrorUnavailable
	default:
		return GraphErrorQueryFailed
	}
}

func canonicalClusters(in []query.Cluster) []query.Cluster {
	out := make([]query.Cluster, len(in))
	for i, cluster := range in {
		out[i] = cluster
		out[i].Files = sortedUniqueStrings(cluster.Files)
	}
	sort.Slice(out, func(i, j int) bool {
		return cmp.Or(strings.Compare(out[i].Label, out[j].Label), strings.Compare(out[i].Lang, out[j].Lang)) < 0
	})
	return out
}

func deriveFileRoles(facts FileFacts) []string {
	p := strings.ToLower(facts.Path)
	roles := map[string]bool{}
	if isTestFile(p) {
		roles[RoleTest] = true
	}
	if isDocumentationPath(p) {
		roles[RoleDocumentation] = true
	}
	if isMechanicalPath(p) {
		roles[RoleMechanical] = true
	}
	if isOperationsPath(p) {
		roles[RoleOperations] = true
	}
	for _, symbol := range facts.Relations.Symbols {
		kind := strings.ToLower(symbol.Kind)
		if strings.Contains(kind, "type") || strings.Contains(kind, "interface") || strings.Contains(kind, "schema") || strings.Contains(kind, "migration") || strings.Contains(kind, "field") || strings.Contains(kind, "option") {
			roles[RoleContract] = true
		}
		if strings.Contains(kind, "func") || strings.Contains(kind, "method") || strings.Contains(kind, "class") || strings.Contains(kind, "component") {
			roles[RoleImplementation] = true
		}
	}
	if facts.Entrypoints.Count == 1 && len(facts.Entrypoints.Entrypoints) == 1 && facts.Entrypoints.Entrypoints[0] == facts.Path {
		roles[RoleConsumer] = true
	}
	if facts.Relations.Exists && len(roles) == 0 {
		roles[RoleImplementation] = true
	}
	if !facts.Relations.Exists {
		roles[RoleUnknown] = true
	}
	return orderedRoles(roles)
}

func orderedRoles(set map[string]bool) []string {
	order := []string{RoleContract, RoleImplementation, RoleConsumer, RoleTest, RoleOperations, RoleDocumentation, RoleMechanical, RoleUnknown}
	out := make([]string, 0, len(set))
	for _, role := range order {
		if set[role] {
			out = append(out, role)
		}
	}
	return out
}

func deriveGraphSignals(facts FileFacts) []AttentionSignal {
	var signals []AttentionSignal
	add := func(kind, fact string) { signals = append(signals, newAttentionSignal(kind, facts.Path, fact)) }
	if facts.Blast.Total >= 20 || facts.Blast.Direct >= 10 {
		add("high_fan_in", fmt.Sprintf("indexed blast radius has %d dependents (%d direct)", facts.Blast.Total, facts.Blast.Direct))
	}
	if facts.Entrypoints.Count > 0 {
		add("entrypoint_reachability", fmt.Sprintf("indexed graph reaches %d entrypoints", facts.Entrypoints.Count))
	}
	if len(facts.Tests.Tests) == 0 && !containsString(facts.Roles, RoleTest) {
		add("no_mapped_test", "indexed test query returned no mapped test")
	}
	if containsString(facts.Roles, RoleOperations) {
		add("operational_change", "recognized schema, migration, manifest, lock, build, configuration, or deployment path")
	}
	if containsString(facts.Roles, RoleUnknown) {
		add("unsupported_unindexed", "file is absent from indexed relations")
	}
	var crossSubsystemFacts []string
	for _, edge := range append(append([]query.EdgeView(nil), facts.Relations.Includes...), facts.Relations.IncludedBy...) {
		if edge.Resolved && subsystemOf(edge.File) != subsystemOf(facts.Path) {
			crossSubsystemFacts = append(crossSubsystemFacts, "indexed relation crosses "+subsystemOf(facts.Path)+" -> "+subsystemOf(edge.File))
		}
	}
	if len(crossSubsystemFacts) > 0 {
		sort.Strings(crossSubsystemFacts)
		add("cross_subsystem_dependency", crossSubsystemFacts[0])
	}
	sort.Slice(signals, func(i, j int) bool { return signals[i].ID < signals[j].ID })
	return signals
}

func newAttentionSignal(kind, p, fact string) AttentionSignal {
	digest := sha256.Sum256(Frame(Field{Name: "schema", Value: []byte("review.signal.v1")}, Field{Name: "kind", Value: []byte(kind)}, Field{Name: "path", Value: []byte(p)}, Field{Name: "fact", Value: []byte(fact)}))
	return AttentionSignal{ID: PublicID("sig_", digest, PublicIDContentBytesV1).Public, Kind: kind, Fact: fact}
}

// FileLayer is one deterministic file-level dependency layer.
type FileLayer struct {
	Ordinal int
	Paths   []string
}

// FileCohort is a file-level membership set built before hunk packing.
type FileCohort struct {
	Key    string
	Label  string
	Files  []string
	Layers []FileLayer
}

// BuildFileCohorts joins only graph-proven/role-proven relationships, then
// condenses SCCs and assigns dependency-first topological depth ordinals.
func BuildFileCohorts(paths []string, enrichment GraphEnrichment) []FileCohort {
	paths = sortedUniqueStrings(paths)
	changed := make(map[string]bool, len(paths))
	uf := newPathUnion(paths)
	for _, p := range paths {
		changed[p] = true
	}
	clusterLabels := make(map[string][]string)
	for _, cluster := range enrichment.Clusters {
		var members []string
		for _, p := range cluster.Files {
			if changed[p] {
				members = append(members, p)
				clusterLabels[p] = append(clusterLabels[p], cluster.Label)
			}
		}
		for i := 1; i < len(members); i++ {
			uf.union(members[0], members[i])
		}
	}
	for _, p := range paths {
		facts := enrichment.Facts[p]
		for _, edge := range append(append([]query.EdgeView(nil), facts.Relations.Includes...), facts.Relations.IncludedBy...) {
			if edge.Resolved && changed[edge.File] {
				uf.union(p, edge.File)
			}
		}
		for _, test := range facts.Tests.Tests {
			if changed[test] {
				uf.union(p, test)
			}
		}
	}
	components := map[string][]string{}
	for _, p := range paths {
		root := uf.find(p)
		components[root] = append(components[root], p)
	}
	roots := make([]string, 0, len(components))
	for root := range components {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	var firstMechanical string
	for _, root := range roots {
		members := components[root]
		allMechanical := true
		for _, p := range members {
			if !containsString(enrichment.Facts[p].Roles, RoleMechanical) {
				allMechanical = false
				break
			}
		}
		if !allMechanical {
			continue
		}
		if firstMechanical == "" {
			firstMechanical = members[0]
			continue
		}
		uf.union(firstMechanical, members[0])
	}
	groups := map[string][]string{}
	for _, p := range paths {
		root := uf.find(p)
		groups[root] = append(groups[root], p)
	}
	cohorts := make([]FileCohort, 0, len(groups))
	for _, files := range groups {
		sort.Strings(files)
		labels := []string{}
		for _, p := range files {
			labels = append(labels, clusterLabels[p]...)
		}
		labels = sortedUniqueStrings(labels)
		key := cohortMembershipKey(files, labels, enrichment.Facts)
		cohorts = append(cohorts, FileCohort{Key: key, Label: strings.TrimPrefix(key, "cluster:"), Files: files, Layers: dependencyLayers(files, enrichment.Facts)})
	}
	sort.Slice(cohorts, func(i, j int) bool {
		return cmp.Or(strings.Compare(cohorts[i].Key, cohorts[j].Key), strings.Compare(strings.Join(cohorts[i].Files, "\x00"), strings.Join(cohorts[j].Files, "\x00"))) < 0
	})
	return cohorts
}

func cohortMembershipKey(files, labels []string, facts map[string]FileFacts) string {
	if len(labels) > 0 {
		return "cluster:" + strings.Join(labels, "+")
	}
	if len(files) == 1 {
		return "path:" + files[0]
	}
	allMechanical := true
	for _, p := range files {
		allMechanical = allMechanical && containsString(facts[p].Roles, RoleMechanical)
	}
	if allMechanical {
		return "mechanical:" + nearestCommonDir(files)
	}
	return "connected:" + strings.Join(files, "+")
}

func dependencyLayers(files []string, facts map[string]FileFacts) []FileLayer {
	inSet := map[string]bool{}
	adj := map[string]map[string]bool{}
	for _, p := range files {
		inSet[p] = true
		adj[p] = map[string]bool{}
	}
	addEdge := func(from, to string) {
		if from != to && inSet[from] && inSet[to] {
			adj[from][to] = true
		}
	}
	for _, p := range files {
		for _, edge := range facts[p].Relations.Includes {
			if edge.Resolved {
				addEdge(edge.File, p)
			}
		}
		for _, edge := range facts[p].Relations.IncludedBy {
			if edge.Resolved {
				addEdge(p, edge.File)
			}
		}
		for _, test := range facts[p].Tests.Tests {
			addEdge(p, test)
		}
	}
	components, componentOf := stronglyConnected(files, adj)
	componentAdj := make([]map[int]bool, len(components))
	indegree := make([]int, len(components))
	depth := make([]int, len(components))
	for i := range componentAdj {
		componentAdj[i] = map[int]bool{}
	}
	for from, tos := range adj {
		for to := range tos {
			a, b := componentOf[from], componentOf[to]
			if a != b && !componentAdj[a][b] {
				componentAdj[a][b] = true
				indegree[b]++
			}
		}
	}
	ready := []int{}
	for i, n := range indegree {
		if n == 0 {
			ready = append(ready, i)
		}
	}
	sortComponents(ready, components)
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		nexts := sortedComponentKeys(componentAdj[current], components)
		for _, next := range nexts {
			if depth[next] < depth[current]+1 {
				depth[next] = depth[current] + 1
			}
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sortComponents(ready, components)
			}
		}
	}
	byDepth := map[int][]string{}
	maxDepth := 0
	for i, component := range components {
		byDepth[depth[i]] = append(byDepth[depth[i]], component...)
		if depth[i] > maxDepth {
			maxDepth = depth[i]
		}
	}
	layers := make([]FileLayer, 0, maxDepth+1)
	for ordinal := 0; ordinal <= maxDepth; ordinal++ {
		members := byDepth[ordinal]
		if len(members) == 0 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			return cmp.Or(cmp.Compare(roleRank(facts[members[i]].Roles), roleRank(facts[members[j]].Roles)), strings.Compare(members[i], members[j])) < 0
		})
		layers = append(layers, FileLayer{Ordinal: len(layers), Paths: members})
	}
	return layers
}

func stronglyConnected(files []string, adj map[string]map[string]bool) ([][]string, map[string]int) {
	index := 0
	indices, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	stack := []string{}
	var components [][]string
	var visit func(string)
	visit = func(v string) {
		indices[v], low[v] = index, index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range sortedMapKeys(adj[v]) {
			if _, seen := indices[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], indices[w])
			}
		}
		if low[v] == indices[v] {
			component := []string{}
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				component = append(component, w)
				if w == v {
					break
				}
			}
			sort.Strings(component)
			components = append(components, component)
		}
	}
	for _, p := range files {
		if _, seen := indices[p]; !seen {
			visit(p)
		}
	}
	sort.Slice(components, func(i, j int) bool { return components[i][0] < components[j][0] })
	componentOf := map[string]int{}
	for i, component := range components {
		for _, p := range component {
			componentOf[p] = i
		}
	}
	return components, componentOf
}

func sortComponents(ids []int, components [][]string) {
	sort.Slice(ids, func(i, j int) bool { return components[ids[i]][0] < components[ids[j]][0] })
}
func sortedComponentKeys(set map[int]bool, components [][]string) []int {
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sortComponents(out, components)
	return out
}
func sortedMapKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func roleRank(roles []string) int {
	for i, role := range []string{RoleContract, RoleImplementation, RoleConsumer, RoleTest, RoleOperations, RoleDocumentation, RoleMechanical, RoleUnknown} {
		if containsString(roles, role) {
			return i
		}
	}
	return 99
}

type pathUnion struct{ parent map[string]string }

func newPathUnion(paths []string) *pathUnion {
	u := &pathUnion{parent: map[string]string{}}
	for _, p := range paths {
		u.parent[p] = p
	}
	return u
}
func (u *pathUnion) find(p string) string {
	parent := u.parent[p]
	if parent != p {
		u.parent[p] = u.find(parent)
	}
	return u.parent[p]
}
func (u *pathUnion) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if ra < rb {
		u.parent[rb] = ra
	} else {
		u.parent[ra] = rb
	}
}

func sortedUniqueStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return compactStrings(out)
}
func compactStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
func subsystemOf(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	if len(parts) == 1 {
		return "."
	}
	return ""
}
func nearestCommonDir(files []string) string {
	if len(files) == 0 {
		return "."
	}
	common := strings.Split(path.Dir(files[0]), "/")
	for _, file := range files[1:] {
		parts := strings.Split(path.Dir(file), "/")
		n := min(len(common), len(parts))
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
	}
	if len(common) == 0 {
		return "."
	}
	return strings.Join(common, "/")
}

func isTestFile(p string) bool {
	p = strings.ToLower(p)
	base := path.Base(p)
	for _, marker := range []string{"_test.", ".test.", "_spec.", ".spec."} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") {
		return true
	}
	for _, segment := range strings.Split(p, "/") {
		switch segment {
		case "test", "tests", "__tests__", "spec", "specs", "testdata", "integration_test":
			return true
		}
	}
	return false
}

func isDocumentationPath(p string) bool {
	p = strings.ToLower(p)
	switch path.Ext(p) {
	case ".adoc", ".md", ".markdown", ".mdx", ".rst", ".txt":
		return true
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "doc" || segment == "docs" {
			return true
		}
	}
	return false
}

func isMechanicalPath(p string) bool {
	base := path.Base(p)
	return strings.Contains(p, "/vendor/") || strings.HasPrefix(p, "vendor/") ||
		strings.Contains(p, "/generated/") || strings.HasPrefix(p, "generated/") ||
		strings.Contains(p, "/gen/") || strings.HasPrefix(p, "gen/") ||
		strings.Contains(p, "/snapshots/") || strings.HasPrefix(p, "snapshots/") ||
		strings.HasSuffix(base, ".lock") || base == "go.sum" || base == "package-lock.json" ||
		base == "pnpm-lock.yaml" || base == "yarn.lock"
}
func isOperationsPath(p string) bool {
	base, ext := path.Base(p), path.Ext(p)
	return strings.Contains(p, "/migrations/") || strings.HasPrefix(p, "migrations/") || strings.Contains(p, "/deploy/") || strings.Contains(p, "/.github/") || strings.HasPrefix(base, "Dockerfile") || base == "Makefile" || base == "go.mod" || base == "package.json" || ext == ".yaml" || ext == ".yml" || ext == ".toml" || ext == ".ini" || ext == ".conf"
}
