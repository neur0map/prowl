package graph

import (
	"path/filepath"
	"testing"

	"github.com/neur0map/prowl/internal/store"
)

func TestResolveKotlinJVMImports(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(rel, lang string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: lang, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	util := mk("lib/src/main/kotlin/com/lib/Util.kt", "kotlin")
	helper := mk("lib/src/main/java/com/lib/Helper.java", "java") // cross-language target
	mailer := mk("app/src/main/kotlin/com/app/Mailer.kt", "kotlin")
	javaApp := mk("app/src/main/java/com/app/App.java", "java")

	// Mailer.kt imports a Kotlin class, a Java class (cross-language), a wildcard
	// package, and an external library.
	if err := s.ReplaceFileGraph(mailer, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "com.lib.Util", Line: 1},
		{Kind: "includes", Raw: "com.lib.Helper", Line: 2},
		{Kind: "includes", Raw: "com.lib.*", Line: 3},
		{Kind: "includes", Raw: "kotlinx.coroutines.flow.Flow", Line: 4},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// App.java imports the Kotlin class Mailer (reverse cross-language).
	if err := s.ReplaceFileGraph(javaApp, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "com.app.Mailer", Line: 1},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}

	// Kotlin -> Kotlin: com.lib.Util resolves to Util.kt.
	if in, _ := s.IncomingEdges("file", util, "includes"); len(in) != 1 || in[0].File != "app/src/main/kotlin/com/app/Mailer.kt" {
		t.Fatalf("Util.kt incoming = %+v, want one from Mailer.kt", in)
	}
	// Kotlin -> Java: com.lib.Helper resolves to Helper.java.
	if in, _ := s.IncomingEdges("file", helper, "includes"); len(in) != 1 || in[0].File != "app/src/main/kotlin/com/app/Mailer.kt" {
		t.Fatalf("Helper.java incoming = %+v, want one from Mailer.kt (cross-language)", in)
	}
	// Java -> Kotlin: com.app.Mailer resolves to Mailer.kt.
	if in, _ := s.IncomingEdges("file", mailer, "includes"); len(in) != 1 || in[0].File != "app/src/main/java/com/app/App.java" {
		t.Fatalf("Mailer.kt incoming = %+v, want one from App.java (cross-language)", in)
	}
	// Wildcard import com.lib.* fans out to every file in com/lib as pkg edges.
	inPkgUtil, _ := s.IncomingEdges("file", util, "pkg")
	inPkgHelper, _ := s.IncomingEdges("file", helper, "pkg")
	if len(inPkgUtil) != 1 || len(inPkgHelper) != 1 {
		t.Fatalf("wildcard pkg edges: util=%+v helper=%+v, want one each", inPkgUtil, inPkgHelper)
	}
	// The external import matches no project file and stays unresolved. The three
	// class imports resolve to files; the wildcard stays an informational include
	// edge (its dependency is carried by the pkg edges checked above).
	unresolved := map[string]bool{}
	dang, _ := s.UnresolvedEdges("includes")
	for _, d := range dang {
		unresolved[d.Raw] = true
	}
	if !unresolved["kotlinx.coroutines.flow.Flow"] {
		t.Fatalf("external import should stay unresolved: %+v", dang)
	}
	for _, raw := range []string{"com.lib.Util", "com.lib.Helper", "com.app.Mailer"} {
		if unresolved[raw] {
			t.Fatalf("%s should have resolved to a file, but is unresolved", raw)
		}
	}
}

// TestResolveJVMMemberImport checks that a member or nested-type import resolves
// to its enclosing class file by dropping trailing segments (a very common
// Kotlin pattern: companion extensions like HttpUrl.Companion.toHttpUrl).
func TestResolveJVMMemberImport(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(rel, lang string) int64 {
		id, err := s.UpsertFile(store.File{RelPath: rel, Lang: lang, Hash: rel, Size: 1, MTime: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	httpUrl := mk("okhttp/src/jvmMain/kotlin/okhttp3/HttpUrl.kt", "kotlin")
	caller := mk("okhttp/src/jvmTest/kotlin/okhttp3/Use.kt", "kotlin")
	if err := s.ReplaceFileGraph(caller, nil, nil, []store.RawEdge{
		{Kind: "includes", Raw: "okhttp3.HttpUrl.Companion.toHttpUrl", Line: 1}, // member -> enclosing file
		{Kind: "includes", Raw: "okhttp3.internal.closeQuietly", Line: 2},       // top-level func, no file
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := Resolve(s); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.IncomingEdges("file", httpUrl, "includes"); len(in) != 1 || in[0].File != "okhttp/src/jvmTest/kotlin/okhttp3/Use.kt" {
		t.Fatalf("HttpUrl.kt incoming = %+v, want one from Use.kt (member import folds to enclosing class)", in)
	}
	// The top-level function import maps to no file and stays unresolved.
	dang, _ := s.UnresolvedEdges("includes")
	if len(dang) != 1 || dang[0].Raw != "okhttp3.internal.closeQuietly" {
		t.Fatalf("unresolved = %+v, want only the top-level function import", dang)
	}
}
