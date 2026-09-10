//go:build linux

package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal so a child process sees stdout as a TTY,
// which is required to force Git to launch its pager. Linux-only.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Skipf("TIOCSPTLCK: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Skipf("TIOCGPTN: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		master.Close()
		t.Skipf("open pts: %v", err)
	}
	return master, slave
}

// TestExecGitForcedPagerNeutralized forces Git to page to a real TTY and proves
// that a hostile inherited GIT_PAGER fires the pager (positive control), while
// the sanitized environment pins the pager to cat so the hostile command never
// runs (sanitized assertion).
func TestExecGitForcedPagerNeutralized(t *testing.T) {
	gitBin(t)
	root, markers := initRepo(t)
	marker := filepath.Join(markers, "pager")
	hostilePager := "touch '" + marker + "'; cat"
	t.Setenv("GIT_PAGER", hostilePager)

	// runOnPTY runs an already-built command with a TTY as stdout so Git will
	// launch its pager, draining the master end.
	runOnPTY := func(cmd *exec.Cmd) {
		master, slave := openPTY(t)
		defer master.Close()
		defer slave.Close()
		done := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := master.Read(buf); err != nil {
					close(done)
					return
				}
			}
		}()
		cmd.Stdout = slave
		cmd.Stderr = slave
		_ = cmd.Run()
		slave.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	// Control: the inherited hostile GIT_PAGER fires under forced pagination.
	clearMarkers(t, markers)
	ctrl := exec.Command("git", "--paginate", "log")
	ctrl.Dir = root
	ctrl.Env = os.Environ()
	runOnPTY(ctrl)
	if !markerPresent(markers, "pager") {
		t.Fatal("control: forced pager did not fire; assertion would be vacuous")
	}

	// Sanitized: build the command through ExecGit's real sanitizedCommand path
	// (the same env scrub and -c config run uses), only supplying a TTY stdout.
	// scrubGitEnv pins GIT_PAGER=cat and baseConfig pins core.pager=cat, so the
	// hostile pager is never launched even on a TTY.
	clearMarkers(t, markers)
	g := ExecGit{HooksDir: t.TempDir()}
	sanitized := g.sanitizedCommand(root, g.baseConfig(nil, nil), []string{"--paginate", "log"})
	runOnPTY(sanitized)
	if markerPresent(markers, "pager") {
		t.Fatal("sanitized environment still ran the hostile pager")
	}
}
