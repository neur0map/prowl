//go:build windows

package review

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestExecGitWindowsJobObjectSmoke exercises the Windows Job Object process
// controller end to end: environment scrub, allowlisted config, and kill-on-
// close job assignment around a real Git invocation. It runs only on Windows CI.
func TestExecGitWindowsJobObjectSmoke(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	g := ExecGit{HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	out, err := g.Output(context.Background(), t.TempDir(), 1<<20, "version")
	if err != nil {
		t.Fatalf("git version through the job-object controller: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty git version output")
	}
}
