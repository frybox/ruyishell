//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup puts the command in its own process group and makes
// the context timeout kill the whole group, so a timed-out run cannot leave
// stray children (e.g. a subshell) behind.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second
}

// shellCommand returns the command used to run a shell snippet on Unix.
func shellCommand(command string) (string, []string) {
	return "bash", []string{"-c", command}
}
