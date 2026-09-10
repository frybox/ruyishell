//go:build !windows

package main

import (
	"syscall"

	"github.com/aymanbagabas/go-pty"
)

// reopenSlaveFor reopens a pty slave whose descriptor the dying session
// leader revoked. Killing a session leader on macOS revokes the slave end
// of the pty and invalidates every fd pointing to it (including the one
// go-pty holds internally): reopen the slave device by its path and dup2
// the new fd onto the old fd number so the next shell can use the same
// pty. On pty types that cannot be reopened (non-Unix), it reports false
// and the caller starts the shell without a reopen.
func reopenSlaveFor(p pty.Pty) bool {
	up, ok := p.(pty.UnixPty)
	if !ok {
		return false
	}
	oldFd := int(up.Slave().Fd())
	newFd, err := syscall.Open(up.Slave().Name(), syscall.O_RDWR, 0)
	if err != nil {
		// Fallback: try the old fd anyway (may fail on macOS).
		resetPtyTermios(up.Slave().Fd())
		return true
	}
	if newFd != oldFd {
		_ = syscall.Dup2(newFd, oldFd)
		_ = syscall.Close(newFd)
	}
	// Now oldFd points to the live slave again.
	resetPtyTermios(uintptr(oldFd))
	return true
}
