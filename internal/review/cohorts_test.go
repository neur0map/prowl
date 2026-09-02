package review

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/prowl-agent/prowl-agent/internal/query"
)

type fakeGraphQueries struct {
	clusters    []query.Cluster
	relations   map[string]query.Relations
	blast       map[string]query.BlastSummary
	entrypoints map[string]query.EntrypointSet
	tests       map[string]query.TestsResult
	fail        map[string]error
}

func (f fakeGraphQueries) Clusters() ([]query.Cluster, error) {
	if err := f.fail["Clusters:"]; err != nil {
		return nil, err
	}
	return f.clusters, nil
}
func (f fakeGraphQueries) FileRelations(path string) (query.Relations, error) {
	if err := f.fail["FileRelations:"+path]; err != nil {
		return query.Relations{}, err
	}
	return f.relations[path], nil
}
func (f fakeGraphQueries) BlastSummarize(path string) (query.BlastSummary, error) {
	if err := f.fail["BlastSummarize:"+path]; err != nil {
		return query.BlastSummary{}, err
	}
	return f.blast[path], nil
}
func (f fakeGraphQueries) EntrypointsFor(path string) (query.EntrypointSet, error) {
	if err := f.fail["EntrypointsFor:"+path]; err != nil {
		return query.EntrypointSet{}, err
	}
	return f.entrypoints[path], nil
}
func (f fakeGraphQueries) TestsFor(path string) (query.TestsResult, error) {
	if err := f.fail["TestsFor:"+path]; err != nil {
		return query.TestsResult{}, err
	}
	return f.tests[path], nil
}

func TestCohortAttachesTestsAndKeepsUnrelatedSeparate(t *testing.T) {
	graph := fakeGraphQueries{
		clusters: []query.Cluster{{Label: "core", Files: []string{"a.go", "b.go"}}},
		relations: map[string]query.Relations{
			"a.go":      {File: "a.go", Exists: true, Includes: []query.EdgeView{{File: "b.go", Resolved: true}}},
			"b.go":      {File: "b.go", Exists: true},
			"a_test.go": {File: "a_test.go", Exists: true},
			"other.go":  {File: "other.go", Exists: true},
		},
		tests: map[string]query.TestsResult{"a.go": {Tests: []string{"a_test.go"}}},
	}
	enriched := EnrichGraph(context.Background(), []string{"other.go", "a_test.go", "b.go", "a.go"}, graph)
	cohorts := BuildFileCohorts([]string{"other.go", "a_test.go", "b.go", "a.go"}, enriched)
	if len(cohorts) != 2 {
		t.Fatalf("cohorts = %#v", cohorts)
	}
	if got := cohorts[0].Files; !reflect.DeepEqual(got, []string{"a.go", "a_test.go", "b.go"}) {
		t.Fatalf("connected cohort = %v", got)
	}
	if got := cohorts[1].Files; !reflect.DeepEqual(got, []string{"other.go"}) {
		t.Fatalf("unrelated cohort = %v", got)
	}
}

func TestLayerCondensesSCCAndOrdersDependenciesBeforeTests(t *testing.T) {
	enriched := GraphEnrichment{Facts: map[string]FileFacts{
		"a.go":      {Path: "a.go", Relations: query.Relations{Includes: []query.EdgeView{{File: "b.go", Resolved: true}}}, Tests: query.TestsResult{Tests: []string{"a_test.go"}}},
		"b.go":      {Path: "b.go", Relations: query.Relations{Includes: []query.EdgeView{{File: "a.go", Resolved: true}}}},
		"a_test.go": {Path: "a_test.go", Roles: []string{RoleTest}},
	}, Clusters: []query.Cluster{{Label: "core", Files: []string{"a.go", "b.go"}}}}
	cohorts := BuildFileCohorts([]string{"a_test.go", "b.go", "a.go"}, enriched)
	if len(cohorts) != 1 || len(cohorts[0].Layers) != 2 {
		t.Fatalf("layers = %#v", cohorts)
	}
	if !reflect.DeepEqual(cohorts[0].Layers[0].Paths, []string{"a.go", "b.go"}) ||
		!reflect.DeepEqual(cohorts[0].Layers[1].Paths, []string{"a_test.go"}) {
		t.Fatalf("dependency order = %#v", cohorts[0].Layers)
	}
}

func TestCohortGraphFailuresEmitStableTypedOmissions(t *testing.T) {
	boom := errors.New("host-specific detail")
	graph := fakeGraphQueries{fail: map[string]error{
		"Clusters:":           boom,
		"FileRelations:a.go":  boom,
		"BlastSummarize:a.go": boom,
		"EntrypointsFor:a.go": boom,
		"TestsFor:a.go":       boom,
	}}
	enriched := EnrichGraph(context.Background(), []string{"a.go"}, graph)
	if len(enriched.Omissions) != 5 {
		t.Fatalf("omissions = %#v", enriched.Omissions)
	}
	for _, omission := range enriched.Omissions {
		if omission.ErrorClass != GraphErrorQueryFailed || omission.Error() == "" {
			t.Fatalf("unstable omission = %#v", omission)
		}
	}
	cohorts := BuildFileCohorts([]string{"a.go"}, enriched)
	if len(cohorts) != 1 || !reflect.DeepEqual(cohorts[0].Files, []string{"a.go"}) {
		t.Fatalf("fallback lost ownership: %#v", cohorts)
	}
}

func TestCohortRecognizesRootAndSegmentedTestPaths(t *testing.T) {
	for _, path := range []string{
		"test/unit.go",
		"tests/unit.go",
		"pkg/test/unit.go",
		"pkg/tests/unit.go",
		"pkg/__tests__/unit.ts",
		"pkg/spec/unit.rb",
		"pkg/specs/unit.rb",
		"pkg/testdata/fixture.go",
		"pkg/integration_test/scenario.go",
		"pkg/unit_test.go",
		"pkg/unit_spec.rb",
		"pkg/unit.spec.ts",
		"pkg/test_unit.py",
	} {
		if !isTestFile(path) {
			t.Errorf("test path not recognized: %s", path)
		}
	}
	for _, path := range []string{"contest/unit.go", "testing/unit.go", "pkg/latest/unit.go"} {
		if isTestFile(path) {
			t.Errorf("non-test path classified as test: %s", path)
		}
	}
}

func TestCohortRecognizesEstablishedDocumentationFormats(t *testing.T) {
	for _, path := range []string{"guide.adoc", "guide.mdx", "notes.txt", "docs/guide.custom", "pkg/docs/guide.custom", "doc/guide.custom"} {
		if !isDocumentationPath(path) {
			t.Errorf("documentation path not recognized: %s", path)
		}
	}
	if isDocumentationPath("src/guide.go") {
		t.Fatal("source file classified as documentation")
	}
}

func TestCohortGroupsCompatibleMechanicalPathsByNearestCommonDirectory(t *testing.T) {
	paths := []string{"generated/client/a.go", "generated/server/b.go", "src/main.go"}
	enriched := GraphEnrichment{Facts: map[string]FileFacts{
		"generated/client/a.go": {Path: "generated/client/a.go", Roles: []string{RoleMechanical}},
		"generated/server/b.go": {Path: "generated/server/b.go", Roles: []string{RoleMechanical, RoleOperations}},
		"src/main.go":           {Path: "src/main.go", Roles: []string{RoleImplementation}},
	}}
	cohorts := BuildFileCohorts(paths, enriched)
	var mechanical *FileCohort
	for i := range cohorts {
		if reflect.DeepEqual(cohorts[i].Files, []string{"generated/client/a.go", "generated/server/b.go"}) {
			mechanical = &cohorts[i]
			break
		}
	}
	if mechanical == nil {
		t.Fatalf("mechanical paths not grouped: %#v", cohorts)
	}
	if mechanical.Key != "mechanical:generated" {
		t.Fatalf("mechanical key = %q", mechanical.Key)
	}
}

func TestCohortMechanicalFallbackDoesNotBridgeGraphOwnedPath(t *testing.T) {
	paths := []string{"src/main.go", "generated/owned/output.go", "generated/isolated/output.go"}
	enriched := GraphEnrichment{Facts: map[string]FileFacts{
		"src/main.go":                  {Path: "src/main.go", Roles: []string{RoleImplementation}, Relations: query.Relations{Includes: []query.EdgeView{{File: "generated/owned/output.go", Resolved: true}}}},
		"generated/owned/output.go":    {Path: "generated/owned/output.go", Roles: []string{RoleMechanical}},
		"generated/isolated/output.go": {Path: "generated/isolated/output.go", Roles: []string{RoleMechanical}},
	}}
	cohorts := BuildFileCohorts(paths, enriched)
	if len(cohorts) != 2 {
		t.Fatalf("mechanical fallback bridged graph-owned path: %#v", cohorts)
	}
	for _, cohort := range cohorts {
		if len(cohort.Files) == 3 {
			t.Fatalf("all paths were incorrectly bridged: %#v", cohorts)
		}
	}
}
