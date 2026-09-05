//go:build !windows

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"ruyishell/internal/screen"
)

// probeTerminalTitle requests the terminal's current window title
// (OSC 1046, xterm and compatibles) and reads the answer (OSC 11 ; ? ;
// <title>, BEL or ST terminated) with a deadline, so rysh can restore it on
// exit. It must run before the key reader starts: at this point nothing
// else reads stdin, and the reads are non-blocking toggles around a single
// fd, so user keystrokes cannot be half-consumed. A terminal that does not
// support the query stays silent and "" is returned — the title is then
// simply left untouched on exit.
func probeTerminalTitle(deadline time.Duration) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return ""
	}
	os.Stdout.WriteString(screen.RequestTitle())
	fd := int(os.Stdin.Fd())
	buf := make([]byte, 256)
	var acc []byte
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		setNonblock(fd, true)
		n, _ := unix.Read(fd, buf)
		setNonblock(fd, false)
		if n <= 0 { // EAGAIN (nothing yet) or a transient read error
			time.Sleep(5 * time.Millisecond)
			continue
		}
		acc = append(acc, buf[:n]...)
		if title, ok := parseTitleResponse(acc); ok {
			return title
		}
	}
	return ""
}

// setNonblock toggles O_NONBLOCK on fd via fcntl; errors are dropped on
// purpose — a failed toggle just makes the next read blocking for at most
// one poll cycle of the probe.
func setNonblock(fd int, on bool) {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return
	}
	if on {
		flags |= unix.O_NONBLOCK
	} else {
		flags &^= unix.O_NONBLOCK
	}
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
}
