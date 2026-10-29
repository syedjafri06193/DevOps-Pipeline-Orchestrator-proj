//go:build unix

package exec

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in a new process group and makes
// cancellation signal the whole group, not just the shell.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
