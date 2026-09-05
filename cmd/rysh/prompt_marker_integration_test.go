// Regression coverage for the prompt marker (主屏方案 §3.2 修订, [tui]
// prompt_marker). With the marker on (the default): rysh owns the terminal
// title for the run, the shell's own OSC 0/2 title escapes never reach the
// stream, and a bash child shell gets a dim "(rysh)" tag prepended to its
// prompt — unless the user's own rc manages PROMPT_COMMAND (starship /
// direnv style), in which case the user's value wins and the marker stays
// out without breaking the prompt. With prompt_marker = "off" the stream
// is pure passthrough again: no title, no stripping, no tag.

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// A static PS1 set by the login profile: the PROMPT_COMMAND the marker
// exports runs before each prompt and prepends the dim tag.
func TestPromptMarkerBashPrefix(t *testing.T) {
	bashPath, ok := findProgram("bash")
	if !ok {
		t.Skip("bash not installed")
	}
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"auto\"\n", bashPath)
	_, _, r := startRyshEnvShell(t, "echo RYSH_READY\nPS1='$ '\n", cfg, bashPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	// The tag must sit directly ahead of the prompt (dim, then reset,
	// then " $ "). Matching the tag alone would also pass on a bash
	// syntax-error message echoing the PROMPT_COMMAND source.
	assertContains(t, startup, "\x1b[2m(rysh)\x1b[0m $ ")
	if strings.Contains(startup, "syntax error") {
		t.Fatalf("PROMPT_COMMAND one-liner failed to parse: %q", startup)
	}
}

// The user's own rc manages PROMPT_COMMAND (starship / direnv style):
// their assignment overwrites the marker's exported value, so the marker
// must stay out entirely — no tag anywhere, while the user's dynamic
// prompt keeps working untouched.
func TestPromptMarkerYieldsToUserPromptCommand(t *testing.T) {
	bashPath, ok := findProgram("bash")
	if !ok {
		t.Skip("bash not installed")
	}
	profile := "echo RYSH_READY\nPS1='orig $'\nPROMPT_COMMAND='PS1=\"dyn $ \"'\n"
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"auto\"\n", bashPath)
	_, _, r := startRyshEnvShell(t, profile, cfg, bashPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, "dyn $")      // the user's rewritten prompt
	assertContains(t, startup, "RYSH_READY") // the shell is healthy
	if strings.Contains(startup, "(rysh)") {
		t.Fatalf("marker tag over a user-managed PROMPT_COMMAND: %q", startup)
	}
}

// The title half of the marker, shell-independent: a prompt that sets the
// terminal title on every repaint (a common PS1 convention, real ESC/BEL
// bytes below) must not leak its title into the stream, and rysh's own
// title must be set instead. Runs on the default /bin/sh child.
func TestPromptMarkerTitle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ConPTY title handling not exercised here")
	}
	profile := "echo RYSH_READY\nPS1=\"\x1b]2;shell-title\x07$ \"\n"
	cfg := "[tui]\nprompt_marker = \"auto\"\n"
	_, _, r := startRyshEnv(t, profile, cfg)
	startup := r.readUntil(t, "$ ", startupTimeout)
	if strings.Contains(startup, "shell-title") {
		t.Fatalf("the shell's own title escape leaked into the stream: %q", startup)
	}
	assertContains(t, startup, "\x1b]2;rysh") // rysh owns the title
}

// prompt_marker = "off": the original pure passthrough — the shell's title
// escape flows through untouched, rysh sets no title and no prompt tag.
func TestPromptMarkerOff(t *testing.T) {
	bashPath, ok := findProgram("bash")
	if !ok {
		t.Skip("bash not installed")
	}
	profile := "echo RYSH_READY\nPS1=\"\x1b]2;shell-title\x07$ \"\n"
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"off\"\n", bashPath)
	_, _, r := startRyshEnvShell(t, profile, cfg, bashPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, "shell-title") // passthrough, unfiltered
	if strings.Contains(startup, "\x1b]2;rysh") {
		t.Fatalf("rysh set a title with the marker off: %q", startup)
	}
	if strings.Contains(startup, "(rysh)") {
		t.Fatalf("prompt tag with the marker off: %q", startup)
	}
}

// unexportZdotdir removes ZDOTDIR from the test environment for the test's
// duration: an exported ZDOTDIR would redirect the child's dot-file lookup
// away from the harness HOME. It must be removed, not emptied — zsh
// misbehaves on a set-but-empty ZDOTDIR (it invokes zsh-newuser-install
// and skips the $HOME startup files).
func unexportZdotdir(t *testing.T) {
	t.Helper()
	if old, had := os.LookupEnv("ZDOTDIR"); had {
		os.Unsetenv("ZDOTDIR")
		t.Cleanup(func() { os.Setenv("ZDOTDIR", old) })
	}
}

// The zsh twin of TestPromptMarkerBashPrefix. A zsh child gets a private
// ZDOTDIR whose single .zshenv registers a precmd hook that prepends the
// dim tag ahead of each prompt; the hook must re-run idempotently and
// never double the tag.
func TestPromptMarkerZshPrefix(t *testing.T) {
	zshPath, ok := findProgram("zsh")
	if !ok {
		t.Skip("zsh not installed")
	}
	unexportZdotdir(t)
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"auto\"\n", zshPath)
	p, _, r := startRyshEnvShell(t, "echo RYSH_READY\nPS1='$ '\n", cfg, zshPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, promptTag+" $ ")
	assertContains(t, startup, "RYSH_READY")
	if strings.Contains(startup, "parse error") || strings.Contains(startup, "invalid") || strings.Contains(startup, "no such file") {
		t.Fatalf("marker .zshenv failed to parse or run: %q", startup)
	}
	// An empty command makes the hook re-run: the second prompt must carry
	// the tag exactly once.
	p.Write([]byte("\r"))
	second := r.readUntil(t, "$ ", startupTimeout)
	if n := strings.Count(second, promptTag); n != 1 {
		t.Fatalf("tag appears %d times on the second prompt: %q", n, second)
	}
}

// The zsh twin of TestPromptMarkerYieldsToUserPromptCommand. The user's own
// precmd function rewrites the prompt outright before every prompt (the
// starship / powerlevel10k style). The hook is registered before the
// user's rc runs, so the user's rewrite wins and the marker stays out.
func TestPromptMarkerZshYieldsToUserPrcmd(t *testing.T) {
	zshPath, ok := findProgram("zsh")
	if !ok {
		t.Skip("zsh not installed")
	}
	unexportZdotdir(t)
	profile := "echo RYSH_READY\nprecmd_functions+=(u)\nu() { PROMPT='dyn $ '; }\n"
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"auto\"\n", zshPath)
	_, _, r := startRyshEnvShell(t, profile, cfg, zshPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, "dyn $") // the user's rewritten prompt
	assertContains(t, startup, "RYSH_READY")
	if strings.Contains(startup, "(rysh)") {
		t.Fatalf("marker tag over a user-managed zsh prompt: %q", startup)
	}
}

// The zsh half of prompt_marker = "off": the child is untouched — no
// ZDOTDIR redirection, no hook, no tag.
func TestPromptMarkerZshOff(t *testing.T) {
	zshPath, ok := findProgram("zsh")
	if !ok {
		t.Skip("zsh not installed")
	}
	unexportZdotdir(t)
	profile := "echo RYSH_READY\nPS1='$ '\n"
	cfg := fmt.Sprintf("shell = %q\n[tui]\nprompt_marker = \"off\"\n", zshPath)
	_, _, r := startRyshEnvShell(t, profile, cfg, zshPath)
	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, "RYSH_READY")
	if strings.Contains(startup, "(rysh)") {
		t.Fatalf("prompt tag with the marker off: %q", startup)
	}
}
