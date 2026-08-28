//go:build !unix && !windows

package review

import (
	"errors"
	"os/exec"
)

// errUnsupportedProcessControl reports that no complete process-tree
// termination guarantee exists on this platform. The runner fails closed rather
// than run Git without a way to reap a hung or overflowing tree.
var errUnsupportedProcessControl = errors.New("review: bounded process-tree control unsupported on this platform")

type unsupportedProcessController struct{}

func newProcessController() processController { return unsupportedProcessController{} }

func (unsupportedProcessController) prepare(*exec.Cmd) error { return nil }
func (unsupportedProcessController) started(*exec.Cmd) error { return errUnsupportedProcessControl }
func (unsupportedProcessController) kill()                   {}
func (unsupportedProcessController) release()                {}

func platformEnvAllowed(string) bool { return false }
