//go:build windows

package main

import "github.com/aymanbagabas/go-pty"

// reopenSlaveFor is a no-op on Windows: ConPTY has no revoke-on-kill like
// the macOS pty, so a killed shell never invalidates the slave descriptor
// and the next shell can reuse the same pty as-is.
func reopenSlaveFor(p pty.Pty) bool {
	return false
}
