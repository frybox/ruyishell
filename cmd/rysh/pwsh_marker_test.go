package main

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPwshMarkerNoLoop exercises the PowerShell prompt-marker path through a
// real pty (the only harness that shows a returned prompt string; a bare
// stdout pipe cannot, because the PowerShell console host writes the prompt
// to the screen buffer, not the redirected stdout). It guards two things the
// user reported:
//
//  1. bug1: the dim "(rysh)" tag is prepended to the PowerShell prompt.
//  2. bug3: the tag is NOT printed in a tight loop (the user saw
//     "(rysh)(rysh)(rysh)..."). A returned prompt string renders once per
//     prompt; the old Write-Host-in-prompt form re-rendered continuously.
//
// The mode-switch (Shift+Tab -> AI) is shell-agnostic and untouched by this
// change; it is not asserted here because the go-pty ConPTY input injection
// in this sandbox does not deliver the switch key the way a real terminal
// does (every mode-switch integration test behaves the same here), so the
// assertion would be an environment artifact, not a product signal.
func TestPwshMarkerNoLoop(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell marker path is Windows-only")
	}
	pwsh, ok := findProgram("powershell")
	if !ok {
		t.Skip("powershell not found; PowerShell marker path cannot be exercised")
	}

	cfg := "shell = " + strconv.Quote(pwsh) + "\n" +
		"[tui]\n" +
		"prompt_marker = \"on\"\n"

	_, _, r := startRyshEnv(t, "", cfg)

	// bug1: the marker must actually be displayed on the prompt.
	first := r.readUntil(t, "(rysh)", startupTimeout)
	if !strings.Contains(first, "(rysh)") {
		t.Fatalf("PowerShell prompt marker not shown; got %q", first)
	}

	// bug3: after the first prompt, with no keystrokes, the tag must not be
	// emitted repeatedly. Let the loop window open for a few seconds; a
	// looping marker would pour hundreds of "(rysh)" into the stream and
	// keep the pty busy (so readQuiet would hit its timeout with a huge
	// window). A fixed prompt renders once, so the settled window is tiny.
	time.Sleep(3 * time.Second)
	win := r.readQuiet(t, settle, 6*time.Second)
	if n := strings.Count(win, "(rysh)"); n >= 20 {
		t.Fatalf("PowerShell prompt marker is looping (saw %d \"(rysh)\" in a settled window); got %q", n, win)
	}
}
