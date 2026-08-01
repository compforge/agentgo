//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// configureProcGroup sets up process-group-based killing on Unix.
// The child becomes a process group leader (Setpgid), and cmd.Cancel
// kills the entire group via negative PID, preventing orphan subprocesses.
func configureProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
