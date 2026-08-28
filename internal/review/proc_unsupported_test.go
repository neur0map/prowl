//go:build !unix

package review

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// TestExecGitUnsupportedFailsBeforeStart proves the ruling for platforms without
// a process-tree guarantee (Windows and other non-Unix): the controller fails
// closed in prepare, before cmd.Start, and ExecGit surfaces the typed
// unsupported error rather than a start/exec error, so the Git binary is never
// launched. It runs only on those platforms' CI.
func TestExecGitUnsupportedFailsBeforeStart(t *testing.T) {
	pc := newProcessController()
	if err := pc.prepare(exec.Command("definitely-not-a-real-binary")); !errors.Is(err, errUnsupportedProcessControl) {
		t.Fatalf("prepare err=%v, want errUnsupportedProcessControl", err)
	}

	g := ExecGit{Binary: "definitely-not-a-real-binary", HooksDir: t.TempDir(), Timeout: 30 * time.Second, MaxStderr: 1 << 20}
	// If the binary were ever started we would get an exec "not found" error;
	// the typed unsupported error proves prepare failed first.
	if _, err := g.Output(context.Background(), t.TempDir(), 1<<20, "status"); !errors.Is(err, errUnsupportedProcessControl) {
		t.Fatalf("Output err=%v, want errUnsupportedProcessControl", err)
	}
}
