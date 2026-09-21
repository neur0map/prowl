package main

import (
	"bytes"
	"strings"
	"testing"

	versionpkg "github.com/neur0map/prowl/internal/version"
)

// runVersion builds the root the way main() does and captures what
// `prowl --version` prints, so the assertions observe the command's real
// output rather than the source that produced it.
func runVersion(t *testing.T, version, commit string) string {
	t.Helper()
	origVersion, origCommit := versionpkg.Version, versionpkg.Commit
	t.Cleanup(func() { versionpkg.Version, versionpkg.Commit = origVersion, origCommit })

	root := newRootCommand(version, commit, "")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("prowl --version failed: %v", err)
	}
	return out.String()
}

// A release build stamps the source commit through main.commit into
// internal/version.Commit (which the gateway's update surface reads) and
// surfaces it on --version alongside the semantic version.
func TestReleaseBuildReportsStampedCommit(t *testing.T) {
	const (
		ver = "v9.9.9"
		sha = "0123456789abcdef0123456789abcdef01234567"
	)
	out := runVersion(t, ver, sha)

	if !strings.Contains(out, ver) {
		t.Errorf("--version dropped the semantic version %q: %q", ver, out)
	}
	if !strings.Contains(out, sha) {
		t.Errorf("--version omitted the stamped commit %q: %q", sha, out)
	}
	// The gateway's update checker resolves the install as "source" only when
	// internal/version.Commit is populated, so the stamp must reach it.
	if versionpkg.Commit != sha {
		t.Errorf("internal/version.Commit = %q, want the stamped %q", versionpkg.Commit, sha)
	}
}

// A local (unstamped) build has no commit: --version prints only the version
// and internal/version.Commit stays empty, which reads as an unknown-origin
// install rather than a fabricated one.
func TestLocalBuildLeavesCommitEmpty(t *testing.T) {
	const ver = "v9.9.9"
	out := runVersion(t, ver, "")

	if !strings.Contains(out, ver) {
		t.Errorf("--version dropped the semantic version %q: %q", ver, out)
	}
	if strings.Contains(out, "commit") {
		t.Errorf("unstamped --version should not mention a commit: %q", out)
	}
	if versionpkg.Commit != "" {
		t.Errorf("internal/version.Commit = %q, want empty for a local build", versionpkg.Commit)
	}
}
