//go:build aix || linux || solaris

package main

import "golang.org/x/sys/unix"

// Linux and the other SysV-flavored ports use TCGETS/TCSETS directly.
const (
	termiosGetReq = unix.TCGETS
	termiosSetReq = unix.TCSETS
)
