//go:build !windows

package main

import "golang.org/x/sys/unix"

// resetPtyTermios restores the pty slave to a canonical, echoing,
// output-processed state. A shell generation SIGKILLed at its prompt cannot
// restore the terminal itself: a line editor leaves the pty without
// canonical mode or echo while it reads, and the replacement shell would
// inherit that and swallow typed input. This puts the pty back the way a
// freshly opened one is, so input is visible in shell mode after a session
// switch no matter what state the dead generation left.
func resetPtyTermios(fd uintptr) {
	t, err := unix.IoctlGetTermios(int(fd), termiosGetReq)
	if err != nil {
		return
	}
	t.Iflag |= unix.ICRNL | unix.IXON | unix.IXANY
	t.Oflag |= unix.OPOST | unix.ONLCR
	t.Lflag |= unix.ICANON | unix.ISIG | unix.IEXTEN |
		unix.ECHO | unix.ECHOK | unix.ECHOE | unix.ECHONL |
		unix.ECHOCTL | unix.ECHOPRT | unix.ECHOKE
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	_ = unix.IoctlSetTermios(int(fd), termiosSetReq, t)
}
