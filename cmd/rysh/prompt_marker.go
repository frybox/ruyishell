package main

import (
	"os"
	"path/filepath"
)

// The bash half of the prompt marker. A login bash has no after-rc hook
// of its own, and --rcfile is only honored by non-login interactive bash,
// so the one reliable injection point for a shell we launch as a login
// shell is the environment: PROMPT_COMMAND is the single env var bash
// imports and runs before every prompt. We export it (see shellEnv in
// main.go); the child's own rc may then overwrite it, in which case the
// marker simply stays out and the user's prompt is never touched. Besides
// the tag, the one-liner emits an OSC 7 cwd report (file://$PWD) before
// every prompt so rysh's cwd tracker follows the shell's cd on every
// platform (the pwsh shim emits the same report).
//
// The zsh half. zsh has neither an rc-file flag nor a PROMPT_COMMAND, so
// the only environment-driven hook is ZDOTDIR: we point it at a private
// directory holding a single .zshenv (see setupZshMarker) that (1) points
// ZDOTDIR back at the user's real dotdir ($RYSH_ZSH_SRC_DIR) so the
// .zprofile/.zshrc/.zlogin and completion dump resolve exactly as before,
// (2) sources the user's real .zshenv, which the redirection would
// otherwise hide, and (3) registers a precmd_functions hook that
// idempotently prepends promptTag, wrapped in %{...%} non-printing
// markers so the line editor's cursor math stays correct. The hook is
// registered before the user's rc runs, so a user precmd hook that
// rewrites $PROMPT outright wins — the same yield as a user-managed bash
// PROMPT_COMMAND. The wrapper directory outlives session-switch shell
// restarts and is removed when rysh exits; the .zshenv is read exactly
// once at child startup, so removal is safe while children still live.

// promptTagText is the marker's visible run; promptTag wraps it dim.
const promptTagText = "(rysh)"

// promptTag is the marker's visible run: a dim "(rysh)".
const promptTag = "\x1b[2m" + promptTagText + "\x1b[0m"

// promptMarkerCmd is the PROMPT_COMMAND one-liner a bash child runs before
// each prompt: idempotently prepend promptTag, wrapped in the [\ ... \]
// non-printing markers so readline's cursor math stays correct. Real ESC
// bytes are fine in an env value. The whole matched run (literal
// backslash-[ / backslash-] markers, the tag bytes, and the trailing
// space) is one single-quoted stretch: an unquoted space inside a case
// pattern would end the pattern word and break the syntax. The quoted
// pattern matches the prefix's exact bytes, so a re-run never doubles it.
//
// The one-liner also emits the OSC 7 cwd report (ESC ] 7 ; file://<cwd>
// BEL) on every prompt so rysh's cwd tracker follows the shell's cd on
// every platform (the pwsh shim emits the same report). The path is the
// %s argument with $PWD expanded by bash at each prompt, so the report
// always carries the current directory; emitting it repeatedly is harmless
// (the tracker keeps the latest). It does not touch PROMPT_COMMAND: the
// marker runs only while it is still in PROMPT_COMMAND — i.e. until a
// user rc overwrites it, exactly when the tag yields — and in that case
// both the tag and the report are simply absent (on Linux /proc still
// feeds the display and the process-cwd mirror via currentCWD).
const promptMarkerCmd = "case $PS1 in '\\[" + promptTag + "\\] '*) ;; *) PS1='\\[" + promptTag + "\\] '$PS1;; esac" +
	"; printf '\\033]7;file://%s\\007' \"$PWD\""

// zshMarkerEnvFile is the sole content of the wrapper ZDOTDIR. The ${var}
// braces keep the [ that follows a variable from parsing as an array
// subscript, and $'\e' supplies the real ESC bytes for the dim SGR pair.
// The quoted case pattern matches the prefix's exact bytes, so a re-run
// never doubles the tag.
const zshMarkerEnvFile = `# rysh prompt marker (see prompt_marker.go). This wrapper ZDOTDIR holds
# only this file: it points ZDOTDIR back at the user's real dotdir so the
# .zprofile/.zshrc/.zlogin and completion dump resolve as before, runs the
# user's real .zshenv (the redirection would otherwise hide it), and
# registers the hook that prepends the dim "(rysh)" tag to the prompt and
# emits an OSC 7 cwd report (file://$PWD) before every prompt so rysh's
# cwd tracker follows cd (the pwsh shim emits the same report).
if [[ -n ${RYSH_ZSH_SRC_DIR:-} ]]; then
  ZDOTDIR=$RYSH_ZSH_SRC_DIR
  if [[ -f $ZDOTDIR/.zshenv ]]; then
    source $ZDOTDIR/.zshenv
  fi
  __rysh_zsh_esc=$'\e'
  __rysh_prompt_tag() {
    local pre="%{${__rysh_zsh_esc}[2m` + promptTagText + `${__rysh_zsh_esc}[0m%} "
    case $PROMPT in "$pre"*) ;; *) PROMPT="$pre$PROMPT" ;; esac
    print -n "${__rysh_zsh_esc}]7;file://${PWD}${__rysh_zsh_esc}\\\\"
  }
  precmd_functions+=(__rysh_prompt_tag)
fi
`

// setupZshMarker creates the wrapper ZDOTDIR and returns the env entries
// for a zsh child (the wrapper path plus the user's real dotdir) and a
// cleanup that removes the wrapper. A nil env means the wrapper could not
// be created: the marker stays out and the child starts plain.
func setupZshMarker() (env []string, cleanup func()) {
	cleanup = func() {}
	// RYSH_ZSH_SRC_DIR wins over ZDOTDIR: inside a nested rysh, ZDOTDIR may
	// point at the outer instance's wrapper directory while
	// RYSH_ZSH_SRC_DIR still names the user's real dotdir.
	srcDir := os.Getenv("RYSH_ZSH_SRC_DIR")
	if srcDir == "" {
		srcDir = os.Getenv("ZDOTDIR")
	}
	if srcDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			srcDir = home
		} else {
			srcDir = os.Getenv("HOME")
		}
	}
	dir, err := os.MkdirTemp("", "rysh-zsh-")
	if err != nil {
		return nil, cleanup
	}
	if err := os.WriteFile(filepath.Join(dir, ".zshenv"), []byte(zshMarkerEnvFile), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, cleanup
	}
	return []string{"ZDOTDIR=" + dir, "RYSH_ZSH_SRC_DIR=" + srcDir}, func() { os.RemoveAll(dir) }
}

// setupPwshMarker creates a temp .ps1 shim that loads the user's PowerShell
// profiles and then wraps prompt to prepend the dim "(rysh)" tag, and
// returns the launch args that run it. PowerShell has no PROMPT_COMMAND
// equivalent, so the only reliable injection point is a -File shim that we
// execute with -NoExit so the interactive prompt survives. The shim loads
// the user's profile chain (AllUsers/CurrentUser x AllHosts/CurrentHost)
// before wrapping, so the resulting prompt is identical to a normal login
// except for the prepended tag. A nil args means the shim could not be
// created: the marker stays out and the shell starts plain.
func setupPwshMarker() (args []string, cleanup func()) {
	cleanup = func() {}
	dir, err := os.MkdirTemp("", "rysh-pwsh-")
	if err != nil {
		return nil, cleanup
	}
	script := filepath.Join(dir, "rysh-marker.ps1")
	const content = `# rysh prompt marker (see prompt_marker.go). Load the user's profiles,
# then wrap prompt to prepend a dim "(rysh)" before the normal prompt.
$__rysh_profiles = @(
	$PROFILE.AllUsersAllHosts, $PROFILE.AllUsersCurrentHost,
	$PROFILE.CurrentUserAllHosts, $PROFILE.CurrentUserCurrentHost
)
foreach ($__p in $__rysh_profiles) {
	if ($__p -and (Test-Path $__p)) { . $__p }
}
# PowerShell's prompt must RETURN a string. Writing the marker with
# Write-Host inside prompt makes the console host re-render the prompt in a
# tight loop (the "(rysh)(rysh)..." spam), so the marker is returned, not
# written. The original prompt function is captured by its ScriptBlock and
# called dynamically on every render, so the prompt reflects the current
# working directory (cd .. updates the path). Calling the ScriptBlock from
# inside the new function is safe (no deadlock); invoking the captured
# FunctionInfo directly would re-resolve to the new prompt and loop.
# The prompt also emits an OSC 7 cwd report (ESC ] 7 ; file://<host>/<path>
# BEL) so rysh's cwdTracker can follow cd on every platform; PowerShell does
# not emit OSC 7 natively, and without it the AI prompt's \w and the agent's
# tool cwd fall back to rysh's own working directory (never changes on
# Windows/macOS).
$__rysh_prev = Get-Item Function:prompt -ErrorAction SilentlyContinue
$__rysh_block = if ($__rysh_prev) { $__rysh_prev.ScriptBlock } else { $null }
function prompt {
	$esc = [char]27
	$loc = $executionContext.SessionState.Path.CurrentLocation.Path
	[Console]::Write("$esc]7;file://$env:COMPUTERNAME/$($loc -replace '\\','/')$([char]7)")
	$base = if ($__rysh_block) { & $__rysh_block } else { "PS $loc$('>' * ($nestedPromptLevel + 1)) " }
	"$esc[2m(rysh)$esc[0m " + $base
}
`
	if err := os.WriteFile(script, []byte(content), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, cleanup
	}
	return []string{"-NoExit", "-ExecutionPolicy", "Bypass", "-File", script}, func() { os.RemoveAll(dir) }
}
