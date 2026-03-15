package updater

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Release describes a GitHub release newer than the current version.
type Release struct {
	TagName string // e.g. "v0.3.0"
	Version string // e.g. "0.3.0"
	URL     string // GitHub release page URL
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// CheckLatest checks whether a newer release exists on GitHub.
// Returns nil if current is up to date or on any error (fails silently).
func CheckLatest(currentVersion string) *Release {
	if currentVersion == "" || currentVersion == "dev" {
		return nil
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/neur0map/prowl/releases/latest")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	if !isNewer(latest, currentVersion) {
		return nil
	}

	return &Release{
		TagName: rel.TagName,
		Version: latest,
		URL:     rel.HTMLURL,
	}
}

// Update checks for a newer release and replaces the current binary.
func Update(currentVersion string) error {
	if currentVersion == "" || currentVersion == "dev" {
		return fmt.Errorf("cannot update a dev build — install from a release")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/neur0map/prowl/releases/latest")
	if err != nil {
		return fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return fmt.Errorf("parse release: %w", err)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	if !isNewer(latest, currentVersion) {
		fmt.Printf("prowl %s is already up to date\n", currentVersion)
		return nil
	}

	// Find matching asset
	assetName := fmt.Sprintf("prowl-%s-%s", runtime.GOOS, runtime.GOARCH)
	var downloadURL string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	// Download to temp file
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	binPath, err = filepath.EvalSymlinks(binPath)
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}

	dlClient := &http.Client{Timeout: 60 * time.Second}
	dlResp, err := dlClient.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download release: %w", err)
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", dlResp.StatusCode)
	}

	tmpPath := binPath + ".new"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(f, dlResp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write binary: %w", err)
	}
	f.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod: %w", err)
	}

	// Swap: current → .old, new → current, remove .old
	oldPath := binPath + ".old"
	if err := os.Rename(binPath, oldPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		// Try to restore
		os.Rename(oldPath, binPath)
		return fmt.Errorf("install new binary: %w", err)
	}
	os.Remove(oldPath)

	fmt.Printf("Updated prowl: v%s → v%s\n", currentVersion, latest)
	return nil
}

// isNewer returns true if version a is strictly newer than version b.
// Compares dot-separated numeric segments left to right.
func isNewer(a, b string) bool {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")

	maxLen := len(pa)
	if len(pb) > maxLen {
		maxLen = len(pb)
	}

	for i := 0; i < maxLen; i++ {
		var va, vb int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &va)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &vb)
		}
		if va > vb {
			return true
		}
		if va < vb {
			return false
		}
	}
	return false
}
