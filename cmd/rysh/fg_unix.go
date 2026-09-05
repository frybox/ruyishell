//go:build !windows

package main

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// shellInForeground reports whether the child shell — not a program it
// launched — currently owns the pty's foreground process group. The check
// is a live TIOCGPGRP query on the pty master, so it is true at the shell's
// prompt and false while any foreground child runs: fullscreen programs
// (vi, htop), pager sessions (less), remote shells (ssh), pipelines and
// plain commands alike. It is the authoritative "the user's keys go to the
// shell" test for input routing; the alternate-screen detector alone
// misses foreground programs that never touch the alternate screen.
// When the state cannot be determined (dead shell, ioctl failure) it
// reports true so the mode-switch gestures stay available.
func shellInForeground(ptyFd uintptr, shellPid int) bool {
	var fg int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptyFd, uintptr(unix.TIOCGPGRP), uintptr(unsafe.Pointer(&fg)))
	if errno != 0 {
		return true
	}
	shellPgrp, err := unix.Getpgid(shellPid)
	if err != nil {
		return true
	}
	return int(fg) == shellPgrp
}
