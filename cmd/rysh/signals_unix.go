//go:build !windows

package main

import (
	"os"
	"syscall"
	"time"
)

func winchSignals() []os.Signal {
	return []os.Signal{syscall.SIGWINCH}
}

func forwardSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}

// isHardSignal reports whether a forwarded signal should force-kill the
// child if it does not exit promptly. SIGINT is intentionally excluded:
// an idle interactive shell ignores it, which is the desired behavior.
func isHardSignal(s os.Signal) bool {
	return s == syscall.SIGTERM || s == syscall.SIGHUP
}

// exitCodeOf converts a child's exit status into the shell-style exit code
// rysh forwards: a signal-terminated child reports 128+signal (the shell
// convention), not the raw -1 os.ProcessState.ExitCode gives for that case.
func exitCodeOf(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// pidAlive reports whether a process with the given pid is running (or
// exists at all). It is used to validate that an attachment record's owner
// is still alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// hardTerminate ends pid with SIGKILL and returns without waiting for it to
// be reaped. SIGKILL is used rather than SIGTERM because an interactive shell
// ignores SIGTERM, and a caller taking down a whole process tree cannot afford
// to wait per process.
func hardTerminate(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	return err == nil || err == syscall.ESRCH
}

// terminateProcess sends SIGTERM to pid and, if it is still alive after a
// short grace period, escalates to SIGKILL. It reports whether the process
// ended.
func terminateProcess(pid int) bool {
	if !pidAlive(pid) {
		return false
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = hardTerminate(pid)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !pidAlive(pid)
}
