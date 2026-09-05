//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

// The BSD family (macOS included) names the TCGETS/TCSETS termios ioctls
// TIOCGETA/TIOCSETA.
const (
	termiosGetReq = unix.TIOCGETA
	termiosSetReq = unix.TIOCSETA
)
