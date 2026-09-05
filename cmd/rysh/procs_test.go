package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveShellNames are the executables a rysh instance may hold as its login
// shell, compared with any extension trimmed so one set covers the Unix names
// and the Windows .exe names. Only these count as "the shell": the rysh
// subprocesses a user runs inside it (rysh ls/resume/new) are one-shot and must not
// be mistaken for a leftover shell.
var liveShellNames = map[string]bool{
	"sh":   true,
	"bash": true,
	"zsh":  true,
	"fish": true,
}

func isLiveShell(name string) bool {
	return liveShellNames[strings.TrimSuffix(strings.ToLower(name), filepath.Ext(name))]
}

// requireProcessTree skips a test on a platform where the process tree cannot
// be inspected, so the absence of /proc never reads as "no leftover shells".
func requireProcessTree(t *testing.T) {
	t.Helper()
	if len(liveProcs()) == 0 {
		t.Skip("process tree cannot be inspected on this platform")
	}
}

// shellDescendants returns the pids of the live shell processes under root,
// following the whole subtree so a shell started through a launcher stub is
// still counted once.
func shellDescendants(t *testing.T, root int) []int {
	t.Helper()
	requireProcessTree(t)
	var shells []int
	for _, p := range procTree(root) {
		if isLiveShell(p.name) {
			shells = append(shells, p.pid)
		}
	}
	return shells
}

// waitForShellsGone blocks until none of the pids in gone is still executing,
// and returns the survivors. A shell generation that was replaced has to be
// observed dead rather than assumed dead: on Windows the login shell is a
// launcher stub with the real shell behind it, so terminating the stub alone
// leaves a child attached to the terminal.
func waitForShellsGone(t *testing.T, gone []int, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var survivors []int
	for {
		survivors = livePids(gone)
		if len(survivors) == 0 || time.Now().After(deadline) {
			return survivors
		}
		time.Sleep(50 * time.Millisecond)
	}
}
