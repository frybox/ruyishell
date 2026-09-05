package main

import (
	"fmt"
	"strings"
	"time"
)

// procInfo is one entry of a live-process snapshot, as returned by liveProcs.
type procInfo struct {
	pid  int
	ppid int
	name string
}

const (
	// procTreeDepth bounds the descendant walk: a shell and the jobs it
	// launches are only a few levels deep, and the bound keeps a cycle in a
	// malformed snapshot from looping forever.
	procTreeDepth = 16
	// shellTreeWait is how long killShellTree gives a shell generation to
	// disappear before it proceeds anyway.
	shellTreeWait = 2 * time.Second
	// shellTreePoll is the interval between process snapshots while waiting.
	shellTreePoll = 20 * time.Millisecond
)

// procTree returns every live descendant of root, breadth-first. A login shell
// can sit behind a launcher stub, so callers walk the whole subtree instead of
// assuming the shell or its jobs are direct children.
func procTree(root int) []procInfo {
	byParent := map[int][]procInfo{}
	for _, p := range liveProcs() {
		byParent[p.ppid] = append(byParent[p.ppid], p)
	}
	var out []procInfo
	queue := append([]procInfo{}, byParent[root]...)
	for depth := 0; len(queue) > 0 && depth < procTreeDepth; depth++ {
		var next []procInfo
		for _, p := range queue {
			out = append(out, p)
			next = append(next, byParent[p.pid]...)
		}
		queue = next
	}
	return out
}

// killShellTree ends the process rooted at pid together with everything it
// forked, and waits (bounded by shellTreeWait) for all of them to stop. It
// returns the descendants it found beyond pid itself and the pids still
// executing afterwards.
//
// The tree has to be snapshotted before anything is killed: terminating a
// launcher stub orphans its child, which is then no longer reachable from the
// root, even though it is very much still alive and still attached to the
// terminal.
func killShellTree(pid int) (descendants, survivors []int) {
	if pid <= 0 {
		return nil, nil
	}
	descendants = pidsOf(procTree(pid))
	return descendants, killPids(append(append([]int{}, descendants...), pid))
}

// killPids ends every pid in the list at once and waits (bounded by
// shellTreeWait) for them to stop, returning the ones still executing. Pass a
// tree deepest-first, so a parent cannot fork a replacement while its own
// children are still being taken down. hardTerminate is used rather than
// terminateProcess because the latter waits per process, and an interactive
// shell is free to ignore the signal it sends first.
func killPids(pids []int) []int {
	for i := len(pids) - 1; i >= 0; i-- {
		hardTerminate(pids[i])
	}
	deadline := time.Now().Add(shellTreeWait)
	survivors := livePids(pids)
	for len(survivors) > 0 && time.Now().Before(deadline) {
		time.Sleep(shellTreePoll)
		survivors = livePids(pids)
	}
	return survivors
}

// pidsOf drops everything but the process ids of a snapshot list.
func pidsOf(procs []procInfo) []int {
	out := make([]int, 0, len(procs))
	for _, p := range procs {
		out = append(out, p.pid)
	}
	return out
}

// livePids filters pids down to the ones still executing.
func livePids(pids []int) []int {
	var out []int
	for _, pid := range pids {
		if procRunning(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// formatProcs renders a process list as "name(pid)<ppid>", for logs and test
// failure messages.
func formatProcs(procs []procInfo) string {
	parts := make([]string, 0, len(procs))
	for _, p := range procs {
		parts = append(parts, fmt.Sprintf("%s(%d)<%d>", p.name, p.pid, p.ppid))
	}
	return strings.Join(parts, " ")
}
