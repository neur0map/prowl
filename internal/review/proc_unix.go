//go:build unix

package review

import (
	"os/exec"
	"syscall"
)

// unixProcessController runs the child as its own process-group leader and
// terminates the entire group with SIGKILL, which cannot be caught or ignored,
// so a descendant trapping SIGTERM still dies. The command is captured in
// prepare (before Start); kill reads cmd.Process, which Start assigns before it
// spawns the I/O copier goroutines, so no controller state is written after
// Start and the read is race-free.
type unixProcessController struct {
	cmd *exec.Cmd
}

func newProcessController() processController { return &unixProcessController{} }

func (c *unixProcessController) prepare(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.cmd = cmd
	return nil
}

func (c *unixProcessController) started(*exec.Cmd) error { return nil }

func (c *unixProcessController) kill() {
	if c.cmd != nil && c.cmd.Process != nil {
		// Setpgid made the child a group leader, so its PGID equals its PID.
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (c *unixProcessController) release() {}

// platformEnvAllowed adds no names beyond the cross-platform base allowlist.
func platformEnvAllowed(string) bool { return false }
