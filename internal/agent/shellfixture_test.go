package agent

// Shell fixtures for the tests that run a real command through the bash
// tool. The tool hands the snippet to the platform shell — `bash -c` on
// Unix (exec_unix.go), `cmd /C` on Windows (exec_windows.go) — so a POSIX
// utility name is not a portable command, it is a different program per
// operating system: cmd.exe has no pwd, sleep, seq, printf, touch or yes,
// and a test written around them fails on "is not recognized" instead of on
// the behaviour it names. Each helper below spells one such behaviour for
// the shell that will run it, so the assertions stay about Execute (output
// capture, working directory, exit code, kill, truncation) on every
// platform, and none of them has to be skipped.
//
// File creation is the one fixture that needs no helper: `echo x > name`
// means the same thing to both shells (and the redirection is exactly what
// takes it out of the §7.1 safe shape, so the approval gate still asks).
// Names passed to these helpers stay free of spaces and path separators, so
// no quoting is needed — cmd.exe mangles quoted arguments under /C.

import (
	"fmt"
	"runtime"
	"strings"
)

// sleepCmd returns a command that keeps running for about secs and prints
// nothing. Keep secs barely longer than what the calling test needs: on
// Unix a killed run dies with its whole process group, but on Windows only
// cmd.exe is killed and the ping it spawned keeps the inherited output
// handle open, so Execute returns — and a job turns Done — only once the
// ping has counted down. Values are therefore in the seconds range, and
// cmd.exe pings one reply per second, so N counts take about N-1 seconds.
func sleepCmd(secs int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("ping -n %d 127.0.0.1 >nul 2>&1", secs+1)
	}
	return fmt.Sprintf("sleep %d", secs)
}

// pwdCmd returns a command printing the child's working directory.
func pwdCmd() string {
	if runtime.GOOS == "windows" {
		return "cd"
	}
	return "pwd"
}

// printFileCmd returns a command printing the whole contents of the file
// name, read relative to the working directory.
func printFileCmd(name string) string {
	if runtime.GOOS == "windows" {
		return "type " + name
	}
	return "cat " + name
}

// echoLinesCmd returns a command printing the given lines in order and then
// running for secs — a job with output to poll while it is still alive.
func echoLinesCmd(secs int, lines ...string) string {
	var b strings.Builder
	if runtime.GOOS == "windows" {
		for _, l := range lines {
			// No space before &: cmd's echo would print it.
			b.WriteString("echo " + l + "& ")
		}
		b.WriteString(sleepCmd(secs))
		return b.String()
	}
	b.WriteString("printf '")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString(`\n`)
	}
	fmt.Fprintf(&b, "'; %s", sleepCmd(secs))
	return b.String()
}

// countCmd returns a command printing the numbers 1 through n, one per line.
func countCmd(n int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("for /L %%i in (1,1,%d) do @echo %%i", n)
	}
	return fmt.Sprintf("seq 1 %d", n)
}

// stderrExitCmd returns a command writing line to stderr and exiting with
// code.
func stderrExitCmd(line string, code int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("echo %s 1>&2 & exit %d", line, code)
	}
	return fmt.Sprintf("echo %s >&2; exit %d", line, code)
}
