package output

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neur0map/prowl/internal/graph"
)

func TestWriteContext(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/auth.ts", Hash: "a"})
	g.AddSymbol(graph.Symbol{
		Name: "handleLogin", Kind: "function", FilePath: "src/auth.ts",
		StartLine: 12, EndLine: 20, IsExported: true,
		Signature: "export function handleLogin(req: Request, res: Response): Promise<void>",
	})
	g.AddSymbol(graph.Symbol{
		Name: "AuthService", Kind: "class", FilePath: "src/auth.ts",
		StartLine: 25, EndLine: 40, IsExported: true,
		Signature: "export class AuthService",
	})

	g.AddFile(graph.FileRecord{Path: "src/login.ts", Hash: "b"})
	g.AddEdge(graph.Edge{SourcePath: "src/login.ts", TargetPath: "src/auth.ts", Type: "IMPORTS"})
	g.AddEdge(graph.Edge{SourcePath: "src/auth.ts", TargetPath: "src/db.ts", Type: "IMPORTS"})

	err := WriteContext(dir, g, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Check .exports
	exports := readFile(t, filepath.Join(dir, "src", "auth.ts", ".exports"))
	if !strings.Contains(exports, "func handleLogin (line 12)") {
		t.Errorf(".exports missing handleLogin, got:\n%s", exports)
	}
	if !strings.Contains(exports, "class AuthService (line 25)") {
		t.Errorf(".exports missing AuthService, got:\n%s", exports)
	}

	// Check .signatures
	sigs := readFile(t, filepath.Join(dir, "src", "auth.ts", ".signatures"))
	if !strings.Contains(sigs, "export function handleLogin") {
		t.Errorf(".signatures missing handleLogin sig, got:\n%s", sigs)
	}

	// Check .imports
	imports := readFile(t, filepath.Join(dir, "src", "auth.ts", ".imports"))
	if !strings.Contains(imports, "src/db.ts") {
		t.Errorf(".imports missing src/db.ts, got:\n%s", imports)
	}

	// Check .upstream (reverse imports)
	upstream := readFile(t, filepath.Join(dir, "src", "auth.ts", ".upstream"))
	if !strings.Contains(upstream, "src/login.ts") {
		t.Errorf(".upstream missing src/login.ts, got:\n%s", upstream)
	}

	// Check _meta/index.txt
	index := readFile(t, filepath.Join(dir, "_meta", "index.txt"))
	if !strings.Contains(index, "src/auth.ts") {
		t.Errorf("_meta/index.txt missing src/auth.ts")
	}
}

func TestWriteContextCallsAndCallers(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/handler.ts", Hash: "a"})
	g.AddFile(graph.FileRecord{Path: "src/service.ts", Hash: "b"})
	g.AddFile(graph.FileRecord{Path: "src/repo.ts", Hash: "c"})

	// handler calls service, service calls repo
	g.AddEdge(graph.Edge{SourcePath: "src/handler.ts", TargetPath: "src/service.ts", Type: "CALLS", Confidence: 0.9})
	g.AddEdge(graph.Edge{SourcePath: "src/service.ts", TargetPath: "src/repo.ts", Type: "CALLS", Confidence: 0.8})

	err := WriteContext(dir, g, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// handler.ts .calls should contain service.ts
	calls := readFile(t, filepath.Join(dir, "src", "handler.ts", ".calls"))
	if !strings.Contains(calls, "src/service.ts") {
		t.Errorf(".calls missing src/service.ts, got:\n%s", calls)
	}

	// service.ts .callers should contain handler.ts
	callers := readFile(t, filepath.Join(dir, "src", "service.ts", ".callers"))
	if !strings.Contains(callers, "src/handler.ts") {
		t.Errorf(".callers missing src/handler.ts, got:\n%s", callers)
	}

	// service.ts .calls should contain repo.ts
	serviceCalls := readFile(t, filepath.Join(dir, "src", "service.ts", ".calls"))
	if !strings.Contains(serviceCalls, "src/repo.ts") {
		t.Errorf("service .calls missing src/repo.ts, got:\n%s", serviceCalls)
	}

	// repo.ts .callers should contain service.ts
	repoCallers := readFile(t, filepath.Join(dir, "src", "repo.ts", ".callers"))
	if !strings.Contains(repoCallers, "src/service.ts") {
		t.Errorf("repo .callers missing src/service.ts, got:\n%s", repoCallers)
	}

	// handler.ts .callers should be empty (no one calls it)
	handlerCallers := readFile(t, filepath.Join(dir, "src", "handler.ts", ".callers"))
	if strings.TrimSpace(handlerCallers) != "" {
		t.Errorf("handler .callers should be empty, got:\n%s", handlerCallers)
	}

	// repo.ts .calls should be empty (it calls nothing)
	repoCalls := readFile(t, filepath.Join(dir, "src", "repo.ts", ".calls"))
	if strings.TrimSpace(repoCalls) != "" {
		t.Errorf("repo .calls should be empty, got:\n%s", repoCalls)
	}
}

func TestWriteContextCommunity(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/auth.ts", Hash: "a"})
	g.AddFile(graph.FileRecord{Path: "src/login.ts", Hash: "b"})
	g.AddFile(graph.FileRecord{Path: "src/db.ts", Hash: "c"})

	communities := []CommunityData{
		{ID: 0, Name: "auth", Members: []string{"src/auth.ts", "src/login.ts"}},
		{ID: 1, Name: "data", Members: []string{"src/db.ts"}},
	}

	err := WriteContext(dir, g, communities, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Check per-file .community
	authComm := readFile(t, filepath.Join(dir, "src", "auth.ts", ".community"))
	if !strings.Contains(authComm, "auth (id: 0)") {
		t.Errorf(".community missing 'auth (id: 0)', got:\n%s", authComm)
	}

	dbComm := readFile(t, filepath.Join(dir, "src", "db.ts", ".community"))
	if !strings.Contains(dbComm, "data (id: 1)") {
		t.Errorf(".community missing 'data (id: 1)', got:\n%s", dbComm)
	}

	// Check _meta/communities.txt
	commFile := readFile(t, filepath.Join(dir, "_meta", "communities.txt"))
	if !strings.Contains(commFile, "auth (2 members)") {
		t.Errorf("communities.txt missing 'auth (2 members)', got:\n%s", commFile)
	}
	if !strings.Contains(commFile, "data (1 members)") {
		t.Errorf("communities.txt missing 'data (1 members)', got:\n%s", commFile)
	}
}

func TestWriteContextProcesses(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()

	g.AddFile(graph.FileRecord{Path: "src/main.ts", Hash: "a"})

	processes := []ProcessData{
		{
			Name:  "handleRequest",
			Entry: "src/handler.ts",
			Steps: []string{"src/handler.ts", "src/service.ts", "src/repo.ts"},
			Type:  "cross_community",
		},
	}

	err := WriteContext(dir, g, nil, processes)
	if err != nil {
		t.Fatal(err)
	}

	// Check _meta/processes.txt
	procFile := readFile(t, filepath.Join(dir, "_meta", "processes.txt"))
	if !strings.Contains(procFile, "process: handleRequest [cross_community]") {
		t.Errorf("processes.txt missing process header, got:\n%s", procFile)
	}
	if !strings.Contains(procFile, "entry: src/handler.ts") {
		t.Errorf("processes.txt missing entry, got:\n%s", procFile)
	}
	if !strings.Contains(procFile, "-> src/handler.ts") {
		t.Errorf("processes.txt missing step handler, got:\n%s", procFile)
	}
	if !strings.Contains(procFile, "-> src/service.ts") {
		t.Errorf("processes.txt missing step service, got:\n%s", procFile)
	}
	if !strings.Contains(procFile, "-> src/repo.ts") {
		t.Errorf("processes.txt missing step repo, got:\n%s", procFile)
	}
}

func TestWriteCalls(t *testing.T) {
	dir := t.TempDir()
	WriteCalls(dir, "src/a.ts", []string{"src/b.ts", "src/c.ts"})

	data, err := os.ReadFile(filepath.Join(dir, "src/a.ts/.calls"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.TrimSpace(string(data))
	if lines != "src/b.ts\nsrc/c.ts" {
		t.Fatalf("unexpected .calls content: %q", lines)
	}
}

func TestWriteCallers(t *testing.T) {
	dir := t.TempDir()
	WriteCallers(dir, "src/b.ts", []string{"src/a.ts"})

	data, err := os.ReadFile(filepath.Join(dir, "src/b.ts/.callers"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "src/a.ts" {
		t.Fatalf("unexpected .callers content: %q", string(data))
	}
}

func TestWriteCommunity(t *testing.T) {
	dir := t.TempDir()
	WriteCommunity(dir, "src/a.ts", "auth (ID: 3)")

	data, err := os.ReadFile(filepath.Join(dir, "src/a.ts/.community"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "auth (ID: 3)" {
		t.Fatalf("unexpected .community content: %q", string(data))
	}
}

func TestWriteIndexFile(t *testing.T) {
	dir := t.TempDir()
	WriteIndexFile(dir, []string{"src/a.ts", "src/b.ts"})

	data, err := os.ReadFile(filepath.Join(dir, "_meta/index.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "src/a.ts\nsrc/b.ts" {
		t.Fatalf("unexpected index.txt content: %q", string(data))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return string(data)
}
