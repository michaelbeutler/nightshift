//go:build !windows

package git

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a new session, without a controlling terminal, as
// the runner is at night: anything that would prompt on /dev/tty fails
// instead of waiting.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
