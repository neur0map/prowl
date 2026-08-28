//go:build windows

package review

import (
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsProcessController confines the child to a Job Object created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so terminating the job (or closing its
// handle) kills the process and every descendant still in the job as a unit.
// The job is created in prepare (before Start) so kill reads only job, which is
// never written after Start; the process is assigned in started. The sanitized
// runner disables hooks, filters, pagers, and helpers, so it does not spawn
// children in the narrow window before assignment.
type windowsProcessController struct {
	job  windows.Handle
	proc windows.Handle
}

func newProcessController() processController { return &windowsProcessController{} }

func (c *windowsProcessController) prepare(*exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return err
	}
	c.job = job
	return nil
}

func (c *windowsProcessController) started(cmd *exec.Cmd) error {
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(c.job, proc); err != nil {
		windows.CloseHandle(proc)
		return err
	}
	c.proc = proc
	return nil
}

func (c *windowsProcessController) kill() {
	if c.job != 0 {
		_ = windows.TerminateJobObject(c.job, 1)
	}
}

func (c *windowsProcessController) release() {
	if c.proc != 0 {
		windows.CloseHandle(c.proc)
		c.proc = 0
	}
	if c.job != 0 {
		// Kill-on-close terminates any survivor still assigned to the job.
		windows.CloseHandle(c.job)
		c.job = 0
	}
}

// platformEnvAllowed keeps the Windows environment variables git needs to run
// (console/system paths) that are absent from the cross-platform base set.
func platformEnvAllowed(name string) bool {
	switch name {
	case "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
		"PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)", "COMMONPROGRAMFILES",
		"ALLUSERSPROFILE", "APPDATA", "LOCALAPPDATA", "USERPROFILE",
		"HOMEDRIVE", "HOMEPATH", "TEMP", "TMP",
		"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE":
		return true
	}
	return false
}
