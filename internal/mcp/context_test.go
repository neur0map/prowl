package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// helper: write a file inside a temp directory, creating parent dirs as needed.
func writeContextFile(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadFileContext(t *testing.T) {
	tmp := t.TempDir()
	filePath := "src/lib/utils.ts"

	writeContextFile(t, tmp, filepath.Join(filePath, ".community"), "core-utils\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".exports"), "cn\nformatDate\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".signatures"), "function cn(...inputs: ClassValue[]): string\nfunction formatDate(d: Date): string\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".calls"), "clsx\ntwMerge\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".callers"), "src/components/Header.tsx\nsrc/components/ClusterCard.tsx\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".imports"), "clsx\ntailwind-merge\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".upstream"), "src/index.css\n")

	fc, err := readFileContext(tmp, filePath)
	if err != nil {
		t.Fatalf("readFileContext error: %v", err)
	}

	if fc.Path != filePath {
		t.Errorf("Path = %q, want %q", fc.Path, filePath)
	}
	if fc.Community != "core-utils" {
		t.Errorf("Community = %q, want %q", fc.Community, "core-utils")
	}
	assertSlice(t, "Exports", fc.Exports, []string{"cn", "formatDate"})
	assertSlice(t, "Signatures", fc.Signatures, []string{
		"function cn(...inputs: ClassValue[]): string",
		"function formatDate(d: Date): string",
	})
	assertSlice(t, "Calls", fc.Calls, []string{"clsx", "twMerge"})
	assertSlice(t, "Callers", fc.Callers, []string{
		"src/components/Header.tsx",
		"src/components/ClusterCard.tsx",
	})
	assertSlice(t, "Imports", fc.Imports, []string{"clsx", "tailwind-merge"})
	assertSlice(t, "Upstream", fc.Upstream, []string{"src/index.css"})
}

func TestReadFileContextMissing(t *testing.T) {
	tmp := t.TempDir()

	_, err := readFileContext(tmp, "nonexistent/file.ts")
	if err == nil {
		t.Fatal("expected error for missing context dir, got nil")
	}
	if got := err.Error(); got != "no context for file: nonexistent/file.ts" {
		t.Errorf("error = %q, want %q", got, "no context for file: nonexistent/file.ts")
	}
}

func TestReadFileContextPartial(t *testing.T) {
	tmp := t.TempDir()
	filePath := "src/main.ts"

	// Only .community and .exports present; the other 5 files are absent.
	writeContextFile(t, tmp, filepath.Join(filePath, ".community"), "entry\n")
	writeContextFile(t, tmp, filepath.Join(filePath, ".exports"), "main\n")

	fc, err := readFileContext(tmp, filePath)
	if err != nil {
		t.Fatalf("readFileContext error: %v", err)
	}

	if fc.Community != "entry" {
		t.Errorf("Community = %q, want %q", fc.Community, "entry")
	}
	assertSlice(t, "Exports", fc.Exports, []string{"main"})

	// Missing files should yield nil slices.
	if fc.Signatures != nil {
		t.Errorf("Signatures = %v, want nil", fc.Signatures)
	}
	if fc.Calls != nil {
		t.Errorf("Calls = %v, want nil", fc.Calls)
	}
	if fc.Callers != nil {
		t.Errorf("Callers = %v, want nil", fc.Callers)
	}
	if fc.Imports != nil {
		t.Errorf("Imports = %v, want nil", fc.Imports)
	}
	if fc.Upstream != nil {
		t.Errorf("Upstream = %v, want nil", fc.Upstream)
	}
}

// assertSlice checks that two string slices are equal.
func assertSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len = %d, want %d\n  got:  %v\n  want: %v", name, len(got), len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
