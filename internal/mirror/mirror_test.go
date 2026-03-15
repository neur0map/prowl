package mirror

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRepo(t *testing.T) {
	tests := []struct {
		input       string
		wantOwner   string
		wantRepo    string
		wantRef     string
		wantErr     bool
	}{
		// Short form
		{"charmbracelet/bubbletea", "charmbracelet", "bubbletea", "", false},
		{"owner/repo", "owner", "repo", "", false},

		// Short form with ref
		{"owner/repo@v2", "owner", "repo", "v2", false},
		{"owner/repo@main", "owner", "repo", "main", false},

		// Full URL
		{"https://github.com/charmbracelet/bubbletea", "charmbracelet", "bubbletea", "", false},
		{"https://github.com/owner/repo", "owner", "repo", "", false},

		// URL with /tree/branch
		{"https://github.com/owner/repo/tree/main", "owner", "repo", "main", false},
		{"https://github.com/owner/repo/tree/feature/branch", "owner", "repo", "feature/branch", false},

		// Errors
		{"", "", "", "", true},
		{"just-one-part", "", "", "", true},
		{"https://gitlab.com/owner/repo", "", "", "", true},
		{"https://github.com/lonely", "", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, ref, err := ParseRepo(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRepo(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if owner != tt.wantOwner || repo != tt.wantRepo || ref != tt.wantRef {
			t.Errorf("ParseRepo(%q) = (%q, %q, %q), want (%q, %q, %q)",
				tt.input, owner, repo, ref, tt.wantOwner, tt.wantRepo, tt.wantRef)
		}
	}
}

func TestExtractTarball(t *testing.T) {
	// Build a synthetic tar.gz in memory
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	files := map[string]string{
		"owner-repo-abc123/main.go":                  "package main",
		"owner-repo-abc123/internal/server.go":       "package internal",
		"owner-repo-abc123/.github/workflows/ci.yml": "name: CI",
		"owner-repo-abc123/README.md":                "# Hello",
	}

	for name, content := range files {
		tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(content)),
			Mode: 0644,
		})
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()

	// Extract
	dest := t.TempDir()
	if err := extractTarball(&buf, dest); err != nil {
		t.Fatalf("extractTarball: %v", err)
	}

	// Verify prefix stripping worked: main.go should be at dest/main.go, not dest/owner-repo-abc123/main.go
	if _, err := os.Stat(filepath.Join(dest, "main.go")); err != nil {
		t.Error("main.go not extracted (prefix stripping failed)")
	}
	if _, err := os.Stat(filepath.Join(dest, "internal", "server.go")); err != nil {
		t.Error("internal/server.go not extracted")
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Error("README.md not extracted")
	}

	// .github should be excluded
	if _, err := os.Stat(filepath.Join(dest, ".github")); !os.IsNotExist(err) {
		t.Error(".github/ should have been excluded")
	}
}

func TestETagCaching(t *testing.T) {
	callCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		etag := `"test-etag-123"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// Return a minimal tarball
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)

		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		content := "package main"
		tw.WriteHeader(&tar.Header{
			Name: "owner-repo-abc/main.go",
			Size: int64(len(content)),
			Mode: 0644,
		})
		tw.Write([]byte(content))
		tw.Close()
		gz.Close()
	}))
	defer ts.Close()

	// Use a temp dir as the mirror cache
	tmpHome := t.TempDir()
	dir := filepath.Join(tmpHome, "test-mirror")

	// First "download" — simulate by hitting the test server and extracting manually
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(dir, 0755)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err := extractTarball(bytes.NewReader(body), dir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	writeETag(dir, `"test-etag-123"`)

	// Second request with If-None-Match should get 304
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.Header.Set("If-None-Match", readETag(dir))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("expected 304, got %d", resp2.StatusCode)
	}
	if callCount != 2 {
		t.Errorf("expected 2 server calls, got %d", callCount)
	}
}
