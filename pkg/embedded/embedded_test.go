package embedded

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neur0map/prowl/internal/config"
	"github.com/neur0map/prowl/internal/version"
	"github.com/neur0map/prowl/internal/workspace"
)

// newFixtureWorkspace creates a minimal, indexable Prowl workspace at a fresh
// temp dir whose sole exported symbol is symbol, and returns its root.
func newFixtureWorkspace(t *testing.T, symbol string) string {
	t.Helper()
	root := t.TempDir()
	state, err := workspace.Create(root)
	if err != nil {
		t.Fatalf("workspace.Create: %v", err)
	}
	if err := config.Save(state.Path, config.Default()); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	src := fmt.Sprintf("package sample\n\nfunc %s() {}\n", symbol)
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return root
}

func execEmbedded(workdir string, args ...string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	err = Execute(workdir, args, &out, &errBuf)
	return out.String(), errBuf.String(), err
}

func mustExec(t *testing.T, workdir string, args ...string) string {
	t.Helper()
	out, errText, err := execEmbedded(workdir, args...)
	if err != nil {
		t.Fatalf("Execute(%q, %v) error: %v\nstderr: %s", workdir, args, err, errText)
	}
	return out
}

// TestExecuteTargetsRequestedWorkdir proves a read-only command resolves the
// workspace at the supplied workdir, not the process cwd: two workspaces with
// disjoint symbols are each queried, and neither leaks into the other's result.
func TestExecuteTargetsRequestedWorkdir(t *testing.T) {
	dirA := newFixtureWorkspace(t, "ZebraWidgetAlpha")
	dirB := newFixtureWorkspace(t, "QuokkaGadgetBeta")

	if got := mustExec(t, dirA, "find", "ZebraWidgetAlpha", "--json"); !strings.Contains(got, "ZebraWidgetAlpha") {
		t.Fatalf("find in dirA did not return its own symbol: %s", got)
	}
	if got := mustExec(t, dirA, "find", "QuokkaGadgetBeta", "--json"); strings.Contains(got, "QuokkaGadgetBeta") {
		t.Fatalf("find in dirA leaked dirB's symbol (wrong workspace resolved): %s", got)
	}
	if got := mustExec(t, dirB, "find", "QuokkaGadgetBeta", "--json"); !strings.Contains(got, "QuokkaGadgetBeta") {
		t.Fatalf("find in dirB did not return its own symbol: %s", got)
	}
}

// TestExecuteLeavesConcurrentGoroutineCwdIntact proves Execute never mutates
// the process working directory. A watcher goroutine does cwd-relative access
// against a marker that exists only in the pinned process cwd - not in the
// workspace Execute targets - throughout a batch of Executes. The pre-fix
// os.Chdir would flip cwd to the workspace and break both checks.
func TestExecuteLeavesConcurrentGoroutineCwdIntact(t *testing.T) {
	hostCwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostCwd, "host-marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Chdir(hostCwd)

	work := newFixtureWorkspace(t, "PenguinProbe")

	stop := make(chan struct{})
	done := make(chan struct{})
	var badCwd atomic.Value
	var relFailed atomic.Bool
	var samples atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if cwd, err := os.Getwd(); err != nil || cwd != hostCwd {
				badCwd.Store(cwd)
				return
			}
			if _, err := os.Stat("host-marker.txt"); err != nil { // cwd-relative
				relFailed.Store(true)
				return
			}
			samples.Add(1)
		}
	}()

	for range 25 {
		if got := mustExec(t, work, "find", "PenguinProbe", "--json"); !strings.Contains(got, "PenguinProbe") {
			t.Fatalf("find did not resolve the workspace symbol: %s", got)
		}
	}
	close(stop)
	<-done

	if v := badCwd.Load(); v != nil {
		t.Fatalf("concurrent goroutine observed cwd %q, want %q (Execute mutated process cwd)", v, hostCwd)
	}
	if relFailed.Load() {
		t.Fatal("concurrent cwd-relative access failed while Execute ran (Execute mutated process cwd)")
	}
	if samples.Load() == 0 {
		t.Fatal("watcher never sampled; the concurrency check was ineffective")
	}
	if cwd, _ := os.Getwd(); cwd != hostCwd {
		t.Fatalf("process cwd after Execute = %q, want %q", cwd, hostCwd)
	}
}

// TestEmbeddedUpdateRefusesWithoutReplacingExecutable proves embedded `update`
// defers instead of calling selfupdate.Apply, so the running executable (the
// host) is never overwritten.
func TestEmbeddedUpdateRefusesWithoutReplacingExecutable(t *testing.T) {
	before := exeFingerprint(t)

	out := mustExec(t, t.TempDir(), "update")
	if !strings.Contains(out, "update is unavailable while running embedded") {
		t.Fatalf("embedded update output = %q, want an embedded-deferral message", out)
	}

	if after := exeFingerprint(t); after != before {
		t.Fatalf("embedded update changed the running executable: before=%s after=%s", before, after)
	}
}

// TestEmbeddedGatewayDaemonCommandsRefused proves the daemon-owning gateway
// surfaces are refused with a clear error while the non-spawning subcommands
// stay available.
func TestEmbeddedGatewayDaemonCommandsRefused(t *testing.T) {
	tmp := t.TempDir()

	for _, name := range []string{"up", "restart", "serve"} {
		_, _, err := execEmbedded(tmp, "gateway", name)
		if err == nil {
			t.Fatalf("gateway %s embedded: got nil error, want unavailable", name)
		}
		if !strings.Contains(err.Error(), "unavailable while Prowl runs embedded") {
			t.Fatalf("gateway %s embedded error = %q, want embedded-unavailable", name, err)
		}
	}

	// Bare `gateway` (the interactive console with the daemon-handoff toggle) is
	// refused too.
	if _, _, err := execEmbedded(tmp, "gateway"); err == nil || !strings.Contains(err.Error(), "gateway console is unavailable while Prowl runs embedded") {
		t.Fatalf("bare gateway embedded error = %v, want console-unavailable", err)
	}

	// A non-spawning subcommand still works.
	out := mustExec(t, tmp, "gateway", "providers")
	if !strings.Contains(out, "PROVIDER") {
		t.Fatalf("gateway providers embedded output = %q, want the provider directory", out)
	}
}

// TestExecuteNoArgsShowsHelpNotTUI documents the narrowed contract: a bare
// embedded invocation prints the command help and returns, rather than opening
// the interactive TUI the standalone binary launches.
func TestExecuteNoArgsShowsHelpNotTUI(t *testing.T) {
	out := mustExec(t, t.TempDir())
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "Available Commands:") {
		t.Fatalf("bare embedded output = %q, want command help", out)
	}
	if !strings.Contains(out, "find") {
		t.Fatalf("bare embedded help missing subcommands: %q", out)
	}
}

// TestExecutePublishesEmbeddedVersion proves Execute feeds the embedded build
// identity into the shared version surface the gateway's version API reads.
func TestExecutePublishesEmbeddedVersion(t *testing.T) {
	_ = mustExec(t, t.TempDir(), "version")
	if version.Version != Version {
		t.Fatalf("version.Version = %q, want %q (embedded build identity not published)", version.Version, Version)
	}
}

func exeFingerprint(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("stat executable: %v", err)
	}
	return fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
}
