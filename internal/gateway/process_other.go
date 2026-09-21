//go:build !windows

package gateway

import (
	"os"
	"os/exec"
	"syscall"
)

// ProcessAlive reports whether pid names a live process this user may signal.
// Signal 0 is delivered to no one: it only exercises the kernel's existence
// and permission checks, so it is the standard liveness probe.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// SignalTerminate asks a gateway to shut down gracefully; the foreground
// gateway installs a SIGTERM handler that drains in-flight requests first.
func SignalTerminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// DetachCommand severs a child from the controlling terminal so `gateway up`
// survives the shell that started it.
func DetachCommand(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setsid = true
}
