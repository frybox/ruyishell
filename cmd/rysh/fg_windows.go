//go:build windows

package main

// Windows consoles have no foreground process group, so there is no live
// "is the shell in the foreground" query: the driver falls back to the
// alternate-screen detector (fullscreenApp) as the only foreground signal.
func shellInForeground(ptyFd uintptr, shellPid int) bool { return true }
