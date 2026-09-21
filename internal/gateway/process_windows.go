//go:build windows

package gateway

import (
	"os"
	"os/exec"
	"syscall"
)

// ProcessAlive opens the pid with SYNCHRONIZE rights; FindProcess alone always
// succeeds on Windows, so existence has to be probed properly.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(handle)
	_ = proc
	return true
}

// SignalTerminate kills the gateway process; there is no portable graceful
// signal to a detached Windows child, and the HTTP server drains on process
// exit close of its own listeners.
func SignalTerminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// DetachCommand launches the child in a new process group with no console.
func DetachCommand(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= 0x00000008 // DETACHED_PROCESS
	c.SysProcAttr.CreationFlags |= 0x00000200 // CREATE_NEW_PROCESS_GROUP
}
