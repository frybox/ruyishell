//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// procFS is the mount point of the Linux process table; platforms without it
// have no process tree to inspect.
const procFS = "/proc"

// procStat reads the fields of a /proc/<pid>/stat line needed here. The comm
// field is parenthesized and may itself contain spaces and parentheses, so it
// is taken between the first and last parenthesis and the remaining fields are
// counted from the closing one.
func procStat(path string) (name string, state byte, ppid int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, 0, err
	}
	open := bytes.IndexByte(raw, '(')
	closing := bytes.LastIndexByte(raw, ')')
	if open < 0 || closing <= open {
		return "", 0, 0, os.ErrInvalid
	}
	name = string(raw[open+1 : closing])
	fields := bytes.Fields(raw[closing+1:])
	if len(fields) < 2 {
		return name, 0, 0, os.ErrInvalid
	}
	state = fields[0][0]
	ppid, err = strconv.Atoi(string(fields[1]))
	if err != nil {
		return name, state, 0, err
	}
	return name, state, ppid, nil
}

// liveProcs returns a point-in-time list of every running process from /proc,
// skipping zombies: a process that has exited but has not been reaped yet
// still has an entry, but it no longer runs and can neither read the terminal
// nor hold it open. Without /proc there is no tree to inspect and the tree
// kill degenerates to the root process alone.
func liveProcs() []procInfo {
	if _, err := os.Stat(procFS); err != nil {
		return nil
	}
	entries, err := os.ReadDir(procFS)
	if err != nil {
		return nil
	}
	out := []procInfo{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, state, ppid, err := procStat(filepath.Join(procFS, e.Name(), "stat"))
		if err != nil || state == 'Z' {
			continue
		}
		out = append(out, procInfo{pid: pid, ppid: ppid, name: name})
	}
	return out
}

// procRunning reports whether pid is still executing. A zombie counts as gone:
// it holds a pid slot and a wait status, nothing else.
func procRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(procFS); err != nil {
		// No /proc: the signal probe is all there is, and it cannot tell a
		// zombie from a running process.
		return pidAlive(pid)
	}
	_, state, _, err := procStat(filepath.Join(procFS, strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	return state != 'Z'
}
