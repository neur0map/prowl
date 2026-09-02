// Package selfupdate checks for and installs newer published builds. It follows
// one of two channels: stable, fed by main, which is what an install gets by
// default; and preview, fed by unstable, opted into with PROWL_UPDATE_CHANNEL.
// The check compares the running binary's commit (from the build's embedded VCS
// info) against the latest commit on that channel's branch, is briefly cached,
// and is offline-safe. It sends nothing about the user; it is an anonymous read
// of public commit data.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	// DevVersion is the default version string of a locally built binary.
	DevVersion = "v0.8.1"
	asset      = "prowl-agent-linux-amd64"
	releaseAt  = "https://github.com/neur0map/prowl-agent/releases/download/"
	commitsAt  = "https://api.github.com/repos/neur0map/prowl-agent/commits/"
	cacheTTL   = 2 * time.Minute
	// ChannelEnv opts a binary into a non-default channel.
	ChannelEnv = "PROWL_UPDATE_CHANNEL"
)

// Channel is one published build stream: a rolling release tag holding the
// artifacts, and the branch whose head decides whether a build is current.
type Channel struct {
	Name   string
	tag    string
	branch string
}

// Stable is fed by main and is the default. Preview is fed by unstable, so it
// carries every commit as it lands rather than only reviewed merges.
var (
	Stable  = Channel{Name: "stable", tag: "stable", branch: "main"}
	Preview = Channel{Name: "preview", tag: "preview", branch: "unstable"}
)

// Current resolves the channel this binary follows.
func Current() Channel {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(ChannelEnv)), Preview.Name) {
		return Preview
	}
	return Stable
}

func (c Channel) assetURL() string    { return releaseAt + c.tag + "/" + asset }
func (c Channel) checksumURL() string { return c.assetURL() + ".sha256" }
func (c Channel) commitsAPI() string  { return commitsAt + c.branch }

// Result reports update status. Checked is true once we determined up-to-date or
// not; Available is true when a newer build exists.
type Result struct {
	Available bool
	Checked   bool
	Current   string // short local commit
	Note      string // "offline", "unknown build", or empty
}

// Check reports whether the followed channel's branch has advanced past the
// running binary's commit.
func Check(version string) Result {
	channel := Current()
	local := localCommit(version)
	if local == "" {
		return Result{Note: "unknown build"}
	}
	latest, ok := cachedLatest(channel)
	if !ok {
		fetched, err := latestCommit(channel, 2*time.Second)
		if err != nil {
			return Result{Current: shortSum(local), Note: "offline"}
		}
		latest = fetched
		writeCache(cache{CheckedAt: time.Now().Unix(), Latest: latest, Channel: channel.Name})
	}
	return Result{Available: !sameCommit(local, latest), Checked: true, Current: shortSum(local)}
}

// cachedLatest returns the last fetched branch head while the cache is fresh. It
// caches the commit (not the verdict) so the result is recomputed against the
// current binary every call, staying correct immediately after an update. A
// cache written for another channel is ignored rather than compared across
// branches.
func cachedLatest(channel Channel) (string, bool) {
	c, ok := readCache()
	if !ok || c.Latest == "" || c.Channel != channel.Name {
		return "", false
	}
	return c.Latest, true
}

// localCommit returns the commit the running binary was built from, preferring
// the embedded VCS revision (set for any go build/install in the repo).
func localCommit(version string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	if version != "" && version != DevVersion {
		return version
	}
	return ""
}

// latestCommit reads the head commit SHA of the channel's branch from the public
// GitHub API.
func latestCommit(channel Channel, timeout time.Duration) (string, error) {
	body, err := download(channel.commitsAPI(), timeout)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", errors.New("no sha in response")
	}
	return out.SHA, nil
}

// sameCommit compares two revisions, tolerating short vs full SHAs.
func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 7 {
		return a == b
	}
	return a[:n] == b[:n]
}

// Apply downloads the latest published binary, verifies its checksum, and
// atomically replaces the running executable.
func Apply() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	channel := Current()
	want, err := fetchChecksum(channel, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("fetch checksum: %w", err)
	}
	if cur, err := selfChecksum(); err == nil && strings.EqualFold(cur, want) {
		return "already on the latest build", nil
	}
	body, err := download(channel.assetURL(), 120*time.Second)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return "", errors.New("checksum mismatch on downloaded binary")
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".prowl-agent-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot write to %s (try a writable install dir or sudo): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return "", fmt.Errorf("replace %s: %w", exe, err)
	}
	clearCache()
	return "updated to the latest " + channel.Name + " build (" + shortSum(want) + ")", nil
}

// Managed reports whether prowl-agent must defer updates to a package manager,
// and the message to print when it must. A binary is managed when a build-time
// managedBy value is set (packaged builds stamp "-X main.managedBy=pacman") or
// when the running executable's directory is not writable by the current user
// (a system install a self-update could never replace anyway). The caller wires
// managedBy from main; keeping the decision here lets it stay network- and
// download-free and unit-testable.
func Managed(managedBy string) (message string, managed bool) {
	return managedGuard(managedBy, execDirWritable())
}

// managedGuard is the pure decision core: given the build-time managedBy and
// whether the install directory is writable, it returns the guard message and
// whether the update must be deferred. It names managedBy when set and otherwise
// falls back to "your package manager".
func managedGuard(managedBy string, dirWritable bool) (message string, managed bool) {
	if managedBy == "" && dirWritable {
		return "", false
	}
	name := managedBy
	if name == "" {
		name = "your package manager"
	}
	return "prowl-agent is managed by " + name + "; update it with your package manager (on Ryoku: ryoku update)", true
}

// execDirWritable reports whether the directory holding the running executable
// is writable by the current user, resolving the executable symlink first so the
// check targets the real install directory.
func execDirWritable() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return dirWritable(filepath.Dir(exe))
}

// dirWritable probes a directory for writability the same way Apply does -- by
// creating and removing a private temp file -- so the guard's verdict matches
// what an actual replacement would find, across every OS and permission model.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".prowl-agent-writable-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// parseChecksum extracts the hex digest from a "sha256sum" line.
func parseChecksum(data []byte) (string, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) < 32 {
		return "", errors.New("malformed checksum")
	}
	return fields[0], nil
}

func fetchChecksum(channel Channel, timeout time.Duration) (string, error) {
	body, err := download(channel.checksumURL(), timeout)
	if err != nil {
		return "", err
	}
	return parseChecksum(body)
}

func download(url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "prowl-agent")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func selfChecksum() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

type cache struct {
	CheckedAt int64  `json:"checked_at"`
	Latest    string `json:"latest"`
	Channel   string `json:"channel"`
}

func cachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "prowl-agent", "update.json")
}

func readCache() (cache, bool) {
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return cache{}, false
	}
	var c cache
	if json.Unmarshal(data, &c) != nil {
		return cache{}, false
	}
	if time.Since(time.Unix(c.CheckedAt, 0)) > cacheTTL {
		return cache{}, false
	}
	return c, true
}

func writeCache(c cache) {
	p := cachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if data, err := json.Marshal(c); err == nil {
		_ = os.WriteFile(p, data, 0o644)
	}
}

func clearCache() { _ = os.Remove(cachePath()) }
