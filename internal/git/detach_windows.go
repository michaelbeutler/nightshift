//go:build windows

package git

import "os/exec"

// detach is a no-op on Windows, where there is no controlling terminal to leave.
func detach(*exec.Cmd) {}
