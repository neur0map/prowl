package mirror

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseRepo extracts owner, repo, and optional ref from various GitHub inputs.
// Accepts: "owner/repo", "https://github.com/owner/repo", "https://github.com/owner/repo/tree/branch".
func ParseRepo(input string) (owner, repo, ref string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", "", fmt.Errorf("empty repo input")
	}

	// URL form
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, parseErr := url.Parse(input)
		if parseErr != nil {
			return "", "", "", fmt.Errorf("invalid URL: %w", parseErr)
		}
		if u.Host != "github.com" && u.Host != "www.github.com" {
			return "", "", "", fmt.Errorf("not a GitHub URL: %s", u.Host)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("URL must contain owner/repo: %s", input)
		}
		owner, repo = parts[0], parts[1]
		// Handle /tree/branch or /tree/tag
		if len(parts) >= 4 && parts[2] == "tree" {
			ref = strings.Join(parts[3:], "/")
		}
		return owner, repo, ref, nil
	}

	// Short form: owner/repo or owner/repo@ref
	if atIdx := strings.Index(input, "@"); atIdx > 0 {
		ref = input[atIdx+1:]
		input = input[:atIdx]
	}

	parts := strings.Split(input, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("invalid repo format (expected owner/repo): %s", input)
	}
	return parts[0], parts[1], ref, nil
}

// Download fetches a GitHub repo as a tarball and extracts it to the mirror cache.
// Returns the path to the extracted mirror directory and whether new content was downloaded.
// Uses ETag-based caching to avoid re-downloading unchanged repos.
// If token is non-empty, it is sent as a Bearer token for private repos.
func Download(owner, repo, ref, token string) (mirrorPath string, changed bool, err error) {
	dir := mirrorDir(owner, repo)
	mirrorPath = dir

	tarballRef := ref
	if tarballRef == "" {
		tarballRef = "HEAD"
	}
	tarballURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball/%s", owner, repo, tarballRef)

	req, err := http.NewRequest("GET", tarballURL, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Send If-None-Match if we have a cached ETag
	if etag := readETag(dir); etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("tarball request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return dir, false, nil
	case http.StatusOK:
		// Continue to extract
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", false, fmt.Errorf("GitHub API %d: %s", resp.StatusCode, string(body))
	}

	// Clear old content and extract fresh
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", false, err
	}

	if err := extractTarball(resp.Body, dir); err != nil {
		os.RemoveAll(dir)
		return "", false, fmt.Errorf("extract tarball: %w", err)
	}

	// Cache the ETag for future requests
	if etag := resp.Header.Get("ETag"); etag != "" {
		writeETag(dir, etag)
	}

	return dir, true, nil
}

// extractTarball decompresses a tar.gz stream and writes files to destDir.
// It strips the top-level prefix directory that GitHub adds (e.g. "owner-repo-sha/").
func extractTarball(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	prefix := "" // auto-detect from first entry

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		name := filepath.ToSlash(hdr.Name)

		// Auto-detect the prefix from the first entry (GitHub tarball always has one)
		if prefix == "" {
			if idx := strings.IndexByte(name, '/'); idx >= 0 {
				prefix = name[:idx+1]
			}
		}

		// Strip the prefix
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || rel == "." {
			continue
		}

		// Skip files that prowl would ignore anyway
		if shouldSkipMirrorPath(rel) {
			continue
		}

		target := filepath.Join(destDir, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
		case tar.TypeReg:
			if err := writeFile(target, tr, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		}
	}
	return nil
}

// shouldSkipMirrorPath returns true for paths that prowl's ignore rules would skip.
// We apply a subset of common ignore patterns to avoid extracting junk.
var skipSegments = map[string]bool{
	".git": true, ".github": true, ".svn": true,
	"node_modules": true, "vendor": true, "__pycache__": true,
	".vscode": true, ".idea": true,
}

func shouldSkipMirrorPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if skipSegments[p] {
			return true
		}
	}
	return false
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

// ---------------------------------------------------------------------------
// Cache helpers
// ---------------------------------------------------------------------------

func mirrorDir(owner, repo string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".prowl", "mirrors", owner+"-"+repo)
}

func etagPath(dir string) string {
	return filepath.Join(dir, ".etag")
}

func readETag(dir string) string {
	data, err := os.ReadFile(etagPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func writeETag(dir, etag string) {
	os.WriteFile(etagPath(dir), []byte(etag), 0644)
}
