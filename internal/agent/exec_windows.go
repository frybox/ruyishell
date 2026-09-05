//go:build windows

package agent

import "os/exec"

// configureProcessGroup is a no-op on Windows: CommandContext kills the
// direct child on timeout and stray grandchildren may survive.
func configureProcessGroup(cmd *exec.Cmd) {}

// shellCommand returns the command used to run a shell snippet on Windows.
func shellCommand(command string) (string, []string) {
	return "cmd", []string{"/C", command}
}
