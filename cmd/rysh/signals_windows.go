//go:build windows

package main

import (
	"os"
	"syscall"
)

// Windows console delivers size changes as console events rather than
// SIGWINCH; pty resizing on Windows is a follow-up.
func winchSignals() []os.Signal {
	return nil
}

func forwardSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func isHardSignal(s os.Signal) bool {
	return s == os.Interrupt
}

// exitCodeOf forwards the child's own exit code; Windows has no
// signal-derived exit status the way Unix does.
func exitCodeOf(ps *os.ProcessState) int {
	return ps.ExitCode()
}

// pidAlive reports whether pid is a running process. It is used on every
// platform by scanAttached to drop stale attachment records, so it must
// actually inspect the process rather than always report "gone".
//
// STILL_ACTIVE (259) is the exit code GetExitCodeProcess reports for a
// process that has not terminated; syscall does not export it on Windows.
const stillActive = 259

// terminateWaitMs bounds the wait for a killed process to signal, matching
// the Unix escalation's final grace period.
const terminateWaitMs = 2000

// sigtermExitCode is the status a Unix process killed by SIGTERM reports
// (128 + 15). Windows has no signal-derived exit status, so terminateProcess
// writes it into the process exit code itself.
const sigtermExitCode = 143

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// openForTerminate opens pid so it can be killed and waited on. The caller
// closes the returned handle.
func openForTerminate(pid int) (syscall.Handle, error) {
	if pid <= 0 {
		return 0, syscall.EINVAL
	}
	return syscall.OpenProcess(syscall.PROCESS_TERMINATE|syscall.SYNCHRONIZE, false, uint32(pid))
}

// hardTerminate ends pid and returns as soon as the request has been accepted,
// without waiting for the process to stop. Callers that take down a whole
// process tree use this so one stubborn member cannot hold up the rest.
//
// TerminateProcess lets the killer choose the exit code, and a killed process
// cannot set it itself, so the same code a plain kill would have produced on
// Unix is written here: 128 + SIGTERM. A parent (or a test) then observes the
// same "terminated by a signal" status on both platforms.
func hardTerminate(pid int) bool {
	h, err := openForTerminate(pid)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	return syscall.TerminateProcess(h, sigtermExitCode) == nil
}

// terminateProcess ends pid. Windows has no SIGTERM that can be delivered to
// a process we do not share a console with, so this goes straight to the
// TerminateProcess equivalent of the Unix SIGKILL escalation and waits for the
// process object to signal. It reports whether the process ended.
func terminateProcess(pid int) bool {
	h, err := openForTerminate(pid)
	if err != nil {
		// Already gone, or access denied to a process we must not kill.
		return false
	}
	defer syscall.CloseHandle(h)
	if err := syscall.TerminateProcess(h, sigtermExitCode); err != nil {
		return false
	}
	event, err := syscall.WaitForSingleObject(h, terminateWaitMs)
	return err == nil && event == syscall.WAIT_OBJECT_0
}
