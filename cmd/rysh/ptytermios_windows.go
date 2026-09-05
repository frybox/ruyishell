//go:build windows

package main

// resetPtyTermios is a no-op on Windows: ConPTY owns the terminal state,
// and a killed shell cannot leave a corrupt termios behind.
func resetPtyTermios(fd uintptr) {}
