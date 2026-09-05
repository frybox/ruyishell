package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aymanbagabas/go-pty"
	"ruyishell/internal/aiui"
	"ruyishell/internal/session"
)

// TestMain re-executes the test binary as rysh itself: when RYSH_TEST_CHILD
// is set, run the real rysh entry (ryshMain, so nested starts are refused
// exactly as in the installed binary); otherwise run the tests. This lets
// each test drive a genuine rysh instance through an outer pty.
func TestMain(m *testing.M) {
	if os.Getenv("RYSH_TEST_CHILD") == "1" {
		os.Exit(ryshMain())
	}
	// The host's LANG (en_US.UTF-8 on the dev machine) would leak into the
	// child rysh processes and switch the UI to English, while the whole
	// suite asserts on the default Chinese text. Pin it for the parent too:
	// children inherit it via testChildEnv.
	os.Setenv("LANG", "C")
	os.Exit(m.Run())
}

const (
	// startupTimeout bounds waiting for the login shell to print its first
	// prompt. Windows cold-starts the shell through two levels (bin\sh.exe
	// launcher, then usr\bin\bash.exe) and an 8s budget was exceeded once in a
	// full-suite run while the same test passed 5/5 in isolation; the bound is
	// a hang guard, so it is set above the observed cold-start tail rather
	// than tuned to the fast path.
	startupTimeout = 20 * time.Second
	waitTimeout    = 5 * time.Second
)

// defaultSwitchSeq is the byte sequence tests write to toggle shell/AI mode.
// It is the canonical Shift+Tab (CSI Z) on every platform. When written to the
// pty the ConPTY decomposes it into a key-event report, which rysh's decoder
// maps back to CSI Z (the mode-switch key) exactly as a Unix pty delivers it —
// so the harness exercises the same real switch path on Windows as on Unix.
// (The ConPTY also emits a stray "[Z" alongside the report; rysh's decoder
// swallows that artifact so it never reaches the line editor.)
var defaultSwitchSeq = []byte("\x1b[Z")

// ptyReader drains the outer pty master in a goroutine and lets tests wait
// for markers in the accumulated output. readUntil searches only the window
// since the previous call, so each assertion sees fresh output.
type ptyReader struct {
	ch    chan byte
	all   []byte
	start int
}

// cuf1Escape is the cursor-forward-one-column sequence ConPTY puts on the
// wire in place of a shell prompt's trailing space: conhost repaints a cell
// holding a space either as the literal space or as a one-column cursor
// advance, and which one it picks is not stable across runs (the same screen
// has been observed as `$ `, `$\x1b[1C`, and with the space dropped). Tests
// assert on the text a user reads, so the Windows stream is normalized back to
// a single space before matching; elsewhere it stays byte-exact.
func cuf1Escape() string {
	if runtime.GOOS == "windows" {
		return "\x1b[1C"
	}
	return ""
}

// streamNormalizer rewrites the pty stream, holding back a tail that could
// still grow into the escape when a chunk splits mid-sequence.
type streamNormalizer struct{ held []byte }

func newStreamNormalizer() *streamNormalizer { return &streamNormalizer{} }

func (s *streamNormalizer) push(b []byte) []byte {
	esc := cuf1Escape()
	if esc == "" {
		return b
	}
	p := append(s.held, b...)
	s.held = nil
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		rest := string(p[i:])
		if len(rest) < len(esc) && strings.HasPrefix(esc, rest) {
			s.held = append(s.held, p[i:]...)
			return out
		}
		if strings.HasPrefix(rest, esc) {
			out = append(out, ' ')
			i += len(esc)
			continue
		}
		out = append(out, p[i])
		i++
	}
	return out
}

// flush returns whatever was still held back, so no byte is dropped.
func (s *streamNormalizer) flush() []byte {
	out := s.held
	s.held = nil
	return out
}

func newPtyReader(m io.Reader) *ptyReader {
	r := &ptyReader{ch: make(chan byte, 8192)}
	go func() {
		tmp := make([]byte, 4096)
		nz := newStreamNormalizer()
		for {
			n, err := m.Read(tmp)
			for _, b := range nz.push(tmp[:n]) {
				r.ch <- b
			}
			if err != nil {
				for _, b := range nz.flush() {
					r.ch <- b
				}
				close(r.ch)
				return
			}
		}
	}()
	return r
}

// readUntil returns the fresh output window once want has appeared in it.
func (r *ptyReader) readUntil(t *testing.T, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if i := strings.Index(string(r.all[r.start:]), want); i >= 0 {
			end := r.start + i + len(want)
			win := string(r.all[r.start:end])
			r.start = end
			return win
		}
		select {
		case b, ok := <-r.ch:
			if !ok {
				t.Fatalf("pty closed while waiting for %q; got %q", want, string(r.all[r.start:]))
			}
			r.all = append(r.all, b)
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", want, string(r.all[r.start:]))
		}
	}
}

// readQuiet returns the fresh output window once the stream has been idle for
// idle. It is for assertions about what a redraw leaves visible: the tail of a
// repaint is the renderer's cursor reposition (which ConPTY rewrites again on
// Windows), so waiting for one particular byte there would test the escape
// layout rather than the line the user reads.
func (r *ptyReader) readQuiet(t *testing.T, idle, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case b, ok := <-r.ch:
			if !ok {
				t.Fatalf("pty closed while waiting for the stream to settle; got %q", string(r.all[r.start:]))
			}
			r.all = append(r.all, b)
		case <-time.After(idle):
			win := string(r.all[r.start:])
			r.start = len(r.all)
			return win
		case <-deadline:
			t.Fatalf("timed out waiting for the stream to settle; got %q", string(r.all[r.start:]))
		}
	}
}

// settle is the quiet period readQuiet waits for: long enough to span the
// resync's 40 ms resize pauses and ConPTY's chunking, short enough to stay
// inside waitTimeout.
const settle = 400 * time.Millisecond

// visible is the window as a user reads it, with every escape sequence
// consumed, for assertions about the text left on a line.
func visible(s string) string { return aiui.Sanitize(s) }

func assertContains(t *testing.T, s, want string) {
	t.Helper()
	if !strings.Contains(s, want) {
		t.Fatalf("output missing %q; got %q", want, s)
	}
}

// requireLinuxProc skips on non-Linux: cwd/env tracking currently relies on
// /proc/<pid>/{cwd,environ}, which only exists on Linux. The OSC 7 reporting
// path for macOS/Windows is planned; until then these context-asserting
// tests are Linux-only so the CI matrix passes on the other platforms.
func requireLinuxProc(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("cwd/env tracking via /proc is Linux-only; OSC 7 reporting for macOS/Windows is planned")
	}
}

// startRyshEnv launches a real rysh inside an outer pty with a disposable
// HOME. profile is the content of ~/.profile (it should print a ready marker
// and set a plain prompt); cfg is the optional content of ~/.rysh/config.toml
// ("" for none); extra vars (e.g. RYSH_TEST_PANIC=1) are appended to the
// child's environment.

// findProgram locates a POSIX tool by name: PATH first, then the Git for
// Windows bin dirs, which a CI or plain shell PATH often omits.
func findProgram(name string) (string, bool) {
	var candidates []string
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	if runtime.GOOS == "windows" {
		for _, dir := range []string{`C:\Program Files\Git\bin`, `C:\Program Files\Git\usr\bin`} {
			for _, ext := range []string{".exe", ""} {
				candidates = append(candidates, filepath.Join(dir, name+ext))
			}
		}
	}
	for _, p := range candidates {
		if isWSLLauncher(p) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// isWSLLauncher reports whether p is the WSL stub Windows installs in
// System32 under a distro's name. Running it boots a Linux distribution
// instead of the tool, and it exits with an error when none is registered,
// so a bash or sh test that picked it up would drive something that is not a
// POSIX shell at all.
func isWSLLauncher(p string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		return false
	}
	return strings.EqualFold(filepath.Dir(p), filepath.Join(sysRoot, "System32"))
}

// shellPath renders a Go-side file path as a word the child POSIX shell can
// execute: single-quoted (a test temp dir may contain spaces) and with forward
// slashes, because an unquoted Windows path loses its backslashes to bash's
// quote removal ("C:\tmp\fakeprog" arrives as "C:tmpfakeprog").
func shellPath(p string) string {
	return "'" + filepath.ToSlash(p) + "'"
}

// testShellOverride returns a [shell] config fragment that points rysh at a
// POSIX shell, so the tests' startup profile (which prints RYSH_READY and sets
// PS1='$ ') is sourced and the shell prompt is the expected "$ ". On Linux CI
// rysh already launches /bin/sh -l, so this is a no-op there. On Windows rysh
// defaults to PowerShell (which has no "$ " prompt and does not read .profile),
// so the shell has to be named. It must be the shell binary itself: a .cmd
// wrapper would put cmd.exe between rysh and the shell, and a session switch
// kills only rysh's own child, so the wrapped shell would survive as an orphan
// still reading the pty. Windows has no login flag here, so the harness also
// writes the profile as ~/.bashrc, which an interactive non-login shell reads.
// withMarkerOff pins [tui] prompt_marker = "off" in a test cfg, unless the
// cfg manages the marker itself (the dedicated marker tests opt in).
// Placement matters: prepending a [tui] block would swallow the test cfg's
// root-level keys (shell =, default =) into the [tui] table and, for a cfg
// that already opens [tui], produce a duplicate-table parse error that
// discards the whole config. The key is instead inserted into the existing
// [tui] block, or the block is appended at the end of the document.
func withMarkerOff(cfg string) string {
	if strings.Contains(cfg, "prompt_marker") {
		return cfg
	}
	lines := strings.Split(cfg, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "[tui]" {
			rest := append([]string{`prompt_marker = "off"`}, lines[i+1:]...)
			return strings.Join(append(lines[:i+1], rest...), "\n")
		}
	}
	if cfg != "" && !strings.HasSuffix(cfg, "\n") {
		cfg += "\n"
	}
	return cfg + "[tui]\nprompt_marker = \"off\"\n"
}

func testShellOverride(t *testing.T) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	sh, ok := findProgram("sh")
	if !ok {
		t.Skip("no POSIX shell (Git Bash) available; integration tests need /bin/sh")
	}
	return fmt.Sprintf("\nshell = %q\n", sh)
}

// writeStartupProfiles puts the test profile where the child shell will
// actually read it. A login shell sources ~/.profile; a shell started without
// a login flag (the only option on Windows, where rysh has no login arg)
// sources ~/.bashrc instead; a login zsh sources ~/.zshrc (it reads neither
// of the other two). All are written so every launcher works on every
// platform, and shells that never read one of them simply ignore it.
func writeStartupProfiles(t *testing.T, home, profile string) {
	t.Helper()
	for _, name := range []string{".profile", ".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// shellConfigured reports whether cfg already selects an explicit shell, so
// testShellOverride does not fight a test that sets its own shell.
func shellConfigured(cfg string) bool {
	return strings.Contains(cfg, "shell =")
}

func startRyshEnv(t *testing.T, profile, cfg string, extra ...string) (pty.Pty, *pty.Cmd, *ptyReader) {
	return startRyshEnvShell(t, profile, cfg, "/bin/sh", extra...)
}

// startRyshEnvShell is startRyshEnv with the SHELL the child sees, so a test
// can also demand a specific login shell. On Windows rysh defaults to
// PowerShell (no "$ " prompt, no .profile), so the shell has to be named in
// the config as well; cfg already carrying a shell key means the caller did
// that.
func startRyshEnvShell(t *testing.T, profile, cfg, shell string, extra ...string) (pty.Pty, *pty.Cmd, *ptyReader) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", shell)
	t.Setenv("TERM", "xterm-256color")
	writeStartupProfiles(t, home, profile)
	// Keep the harness prompt exactly what the profile sets: the prompt
	// marker is on by default in real configs, but it is a dedicated
	// subject of its own tests, so the harness pins it off unless a test
	// opts in (the harness would otherwise change every prompt width).
	cfg = withMarkerOff(cfg)
	if !shellConfigured(cfg) {
		// Prepend so the override lands at the document root, not inside a
		// table such as [ai] that a test cfg may already have opened.
		cfg = testShellOverride(t) + cfg
	}
	configPath := filepath.Join(home, ".rysh", "config.toml")
	if cfg != "" {
		if err := os.MkdirAll(filepath.Join(home, ".rysh"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// rysh resolves its config via os.UserHomeDir() (USERPROFILE on Windows),
	// which t.Setenv("HOME", ...) does not redirect, so point it at the temp
	// config explicitly. This makes the temp HOME fully isolate the test on
	// every platform.
	extra = append(extra, "RYSH_CONFIG="+configPath)
	// The session store also resolves via os.UserHomeDir(); on Windows that
	// is USERPROFILE, not HOME, so redirect it too for full isolation.
	if runtime.GOOS == "windows" {
		extra = append(extra, "USERPROFILE="+home)
	}

	p, err := pty.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if err := p.Resize(80, 24); err != nil {
		t.Fatal(err)
	}
	c := p.Command(os.Args[0])
	c.Env = testChildEnv(extra...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	})
	// rysh is the only reader of the outer slave once started.
	if up, ok := p.(pty.UnixPty); ok {
		_ = up.Slave().Close()
	}

	return p, c, newPtyReader(p)
}

// testChildEnv builds the environment for a test-spawned rysh: the test
// runner's environment with every RYSH_* session variable stripped (the
// child is a fresh top-level rysh, not a nested one — otherwise running the
// suite from inside a rysh session would refuse every start and inherit a
// foreign RYSH_SESSION_ID) plus RYSH_TEST_CHILD=1 and any extra vars. Extra
// vars come last: os/exec keeps the last occurrence of a duplicate key, so
// a test can still pin e.g. RYSH_SESSION_ID explicitly.
func testChildEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+1+len(extra))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, insideEnv+"=") ||
			strings.HasPrefix(kv, sessionEnv+"=") ||
			strings.HasPrefix(kv, ctlEnv+"=") ||
			strings.HasPrefix(kv, pidEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "RYSH_TEST_CHILD=1")
	return append(env, extra...)
}

// startRyshWithConfig is startRyshEnv with the default ready-marker profile.
func startRyshWithConfig(t *testing.T, cfg string, extra ...string) (pty.Pty, *pty.Cmd, *ptyReader) {
	return startRyshEnv(t, "echo RYSH_READY\nPS1='$ '\n", cfg, extra...)
}

func startRysh(t *testing.T) (pty.Pty, *pty.Cmd, *ptyReader) {
	return startRyshWithConfig(t, "")
}

// startRyshBash launches rysh inside a pty with /bin/bash as the login
// shell, so a test can drive readline editing (arrow keys, mid-line cursor)
// that plain dash does not support. Skipped when bash is not installed.
func startRyshBash(t *testing.T) (pty.Pty, *pty.Cmd, *ptyReader) {
	t.Helper()
	bashPath, ok := findProgram("bash")
	if !ok {
		t.Skip("bash not installed")
	}
	// Name the shell in the config, not just in $SHELL: on Windows rysh
	// defaults to PowerShell and the POSIX SHELL path is not launchable, so
	// a bash-only test would silently run under a shell with no readline.
	cfg := fmt.Sprintf("shell = %q\n", bashPath)
	return startRyshEnvShell(t, "echo RYSH_READY\nPS1='$ '\n", cfg, bashPath)
}

// rewriteConfig overwrites ~/.rysh/config.toml in the test HOME, simulating
// a user editing the file while rysh keeps running.
func rewriteConfig(t *testing.T, cfg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".rysh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".rysh", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// enterAI switches to AI mode (Shift+Tab) and waits for the AI prompt line:
// the prompt ends with the plain-magenta [AI]: marker (a text marker, not
// the old filled status chip) and AI mode must never enter the alternate
// screen or clear the shared stream, because the AI editor shares the
// shell's main screen.
func enterAI(t *testing.T, p pty.Pty, r *ptyReader) string {
	t.Helper()
	p.Write(defaultSwitchSeq)
	win := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	// The AI prompt is followed by the render of the draft carried over from
	// the shell line. Settle it here, so a later window that waits for the
	// draft's text matches the shell's echo after a mode switch instead of
	// this leftover repaint.
	win += r.readQuiet(t, settle, waitTimeout)
	if strings.Contains(win, "\x1b[?1049h") {
		t.Fatalf("AI mode entered the alternate screen: %q", win)
	}
	if strings.Contains(win, "\x1b[2J") || strings.Contains(win, "\x1b[3J") {
		t.Fatalf("AI mode cleared the screen: %q", win)
	}
	return win
}

// Scenario A + regression: basic shell passthrough still works on the main
// screen — rysh runs on the main screen, so startup carries no alternate
// screen, scroll region, status row, or cursor-style sequence.
func TestBasicPassthrough(t *testing.T) {
	p, _, r := startRysh(t)

	startup := r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, startup, "rysh ·")     // startup banner (main screen; model ref blank until configured)
	assertContains(t, startup, "RYSH_READY") // the login profile ran
	assertContains(t, startup, "进入 AI 模式")   // banner hint line (how to reach AI mode)
	assertContains(t, startup, "Shift+Tab")  // default mode_switch binding named in the hint
	// Main-screen mode: rysh never enters the alternate screen, sets no
	// scroll region, and draws no status row or cursor-style chip.
	if strings.Contains(startup, "\x1b[?1049h") {
		t.Fatalf("startup entered the alternate screen: %q", startup)
	}
	if strings.Contains(startup, "\x1b[1;23r") {
		t.Fatalf("startup set a scroll region: %q", startup)
	}
	if strings.Contains(startup, "[SH]") || strings.Contains(startup, "\x1b[1;30;42m") {
		t.Fatalf("startup drew a status chip: %q", startup)
	}
	if strings.Contains(startup, "\x1b[5 q") || strings.Contains(startup, "\x1b[2 q") {
		t.Fatalf("startup emitted a DECSCUSR cursor style: %q", startup)
	}

	p.Write([]byte("echo hello\r"))
	out := r.readUntil(t, "hello\r\n$ ", waitTimeout)
	assertContains(t, out, "echo hello")  // the typed command was echoed
	assertContains(t, out, "hello\r\n$ ") // its output, then the prompt
}

// Scenario A' (spinner): between the banner and the login shell's first
// output the profile is still running, and that wait must stay visible: a
// 启动中... spinner animates on the banner row and is erased when the first
// output lands on it, so a slow profile never reads as a dead screen. The
// profile sleeps a second so the ticker repaints the spinner several times
// before any output exists.
func TestStartupSpinner(t *testing.T) {
	_, _, r := startRyshEnv(t, "sleep 1\necho RYSH_READY\nPS1='$ '\n", "")

	startup := r.readUntil(t, "$ ", startupTimeout)

	// The arm frame plus at least one ticker frame: the spinner animates
	// before the shell's first output.
	if n := strings.Count(startup, "启动中"); n < 2 {
		t.Fatalf("startup spinner not animating (want >= 2 启动中 frames, got %d): %q", n, startup)
	}
	if i, j := strings.Index(startup, "启动中"), strings.Index(startup, "RYSH_READY"); i > j {
		t.Fatalf("spinner landed after the shell's first output:\n%q", startup)
	}
	// The spinner's row is erased before the first output lands on it.
	j := strings.Index(startup, "RYSH_READY")
	last := strings.LastIndex(startup[:j], "启动中")
	if !strings.Contains(startup[last:j], "\x1b[K") {
		t.Fatalf("spinner row not erased before first output:\n%q", startup)
	}
	// The spinner block is bracketed by blank rows: below the banner there
	// is a blank row, then the first arm frame — the spinner never glues
	// to the banner above it.
	if !strings.Contains(visible(startup), "退出 rysh\n\n⠋ 启动中") {
		t.Fatalf("spinner not bracketed by a blank row below the banner:\n%q", visible(startup))
	}
	// The startup arm hides the cursor (the spinner is the live focus), and
	// the first output shows it again, so the shell's prompt lands with a
	// visible cursor.
	if i := strings.Index(startup, "启动中"); !strings.Contains(startup[:i], "\x1b[?25l") {
		t.Fatalf("startup spinner did not hide the cursor:\n%q", startup)
	}
	if !strings.Contains(startup[last:j], "\x1b[?25h") {
		t.Fatalf("cursor not shown when the first output replaced the spinner:\n%q", startup)
	}
}

// Scenario B: the default mode-switch key (Shift+Tab) switches to AI mode:
// the shared main screen gains an AI prompt line (PS-style, ending in the
// plain-magenta [AI]: marker) and the screen is never cleared and no
// alternate screen is entered. Keystrokes build the AI draft instead of
// reaching the shell.
func TestShiftTabEntersAIMode(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	ai := enterAI(t, p, r)
	assertContains(t, ai, "\x1b[35m[AI]:") // prompt ends with a plain-magenta [AI] marker
	assertContains(t, ai, "\x1b[35m")      // default prompt: magenta marker present
	if n := strings.Count(ai, "\x1b[35m[AI]:"); n < 1 {
		t.Fatalf("[AI]: marker missing from the AI window: %q", ai)
	}
	if strings.Contains(ai, "\x1b[2J") || strings.Contains(ai, "\x1b[3J") {
		t.Fatalf("entering AI cleared the screen: %q", ai)
	}

	// Typing builds the AI draft on the prompt line.
	p.Write([]byte("echo HELLO42"))
	typed := r.readUntil(t, "echo HELLO42", waitTimeout)
	assertContains(t, typed, "\x1b[35m[AI]:") // prompt ends with a plain-magenta [AI] marker
}

// Scenario B2: a leading space at a fresh prompt — the second default
// mode-switch gesture — also switches to AI mode. The space reaches rysh as a
// plain 0x20 byte (no terminal escape), so this exercises the same real
// keystroke delivery path on every platform, unlike Shift+Tab which depends
// on terminal escape encoding.
func TestLeadingSpaceEntersAIMode(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// A space before any other input on the line is the mode-switch gesture.
	p.Write([]byte(" "))
	ai := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	if strings.Contains(ai, "\x1b[?1049h") {
		t.Fatalf("leading space entered the alternate screen: %q", ai)
	}
	if strings.Contains(ai, "\x1b[2J") || strings.Contains(ai, "\x1b[3J") {
		t.Fatalf("leading space cleared the screen: %q", ai)
	}
	if n := strings.Count(ai, "\x1b[35m[AI]:"); n < 1 {
		t.Fatalf("[AI]: marker missing from the AI window: %q", ai)
	}
}

// Entering AI mode after shell activity must replace the shell prompt line
// in place: the timeline is displayed live as it streams, so re-rendering
// the tail on entry would duplicate it above the prompt and push the AI
// prompt onto a fresh line instead of replacing the prompt.
func TestEnterAIReplacesPromptWithoutReplay(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("echo MARKQ7\r"))
	r.readUntil(t, "MARKQ7", waitTimeout)
	r.readUntil(t, "$ ", waitTimeout)

	ai := enterAI(t, p, r)
	assertContains(t, ai, "\x1b[35m[AI]:")
	if strings.Contains(ai, "─── $") {
		t.Fatalf("entering AI re-rendered the command timeline: %q", ai)
	}
	if strings.Contains(ai, "MARKQ7") {
		t.Fatalf("entering AI duplicated live shell output above the prompt: %q", ai)
	}

	// Round trip: returning to the shell and re-entering stays in place.
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "$ ", waitTimeout)
	again := enterAI(t, p, r)
	assertContains(t, again, "\x1b[35m[AI]:")
	if strings.Contains(again, "─── $") || strings.Contains(again, "MARKQ7") {
		t.Fatalf("re-entering AI re-rendered the timeline: %q", again)
	}
}

// History the session already carries when rysh attaches (e.g. written by a
// one-shot rysh ai in an earlier run, or a continued session) has never
// been displayed in this terminal: rysh prints a continuation separator
// plus the tail first, and the startup banner right under it, before the
// first shell prompt — the banner after the replay so a replay longer than
// the screen cannot scroll the hint lines off the visible screen. Entering
// AI mode stays an in-place prompt swap — it never re-renders the timeline.
func TestStartupReplaysAttachedHistory(t *testing.T) {
	home := t.TempDir()
	id := "sjstartup1"
	log, err := session.OpenLog(filepath.Join(home, ".rysh", "sessions"), id)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	writes := [][2]string{
		{"usr", "EARLYQuestion1"},
		{"shl", "EARLYOut1\n"},
		{"asw", "EARLYReply1"},
	}
	for _, w := range writes {
		if err := log.Write(w[0], w[1]); err != nil {
			t.Fatalf("log.Write(%s, %q): %v", w[0], w[1], err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}

	p, _, r := startRyshIn(t, home, sessionProfile, "", []string{"RYSH_SESSION_ID=" + id})
	startup := r.readUntil(t, "$ ", startupTimeout)

	// The tail landed before the banner, the banner before the first
	// prompt: a long replay must not be able to push the hint lines off
	// the visible screen.
	assertContains(t, startup, "接续会话")
	assertContains(t, startup, "EARLYQuestion1")
	assertContains(t, startup, "EARLYReply1")
	if i, j := strings.Index(startup, "接续会话"), strings.Index(startup, "进入 AI 模式"); i > j {
		t.Fatalf("banner printed before the attached-history replay:\n%q", startup)
	}

	// Shell output after startup streams live.
	p.Write([]byte("echo LIVE9\r"))
	r.readUntil(t, "LIVE9", waitTimeout)
	r.readUntil(t, "$ ", waitTimeout)

	// Entering AI replaces the shell prompt in place: nothing is
	// re-rendered, so the banner stays where it stands.
	ai := enterAI(t, p, r)
	assertContains(t, ai, "\x1b[35m[AI]:")
	if strings.Contains(ai, "EARLYQuestion1") || strings.Contains(ai, "EARLYReply1") || strings.Contains(ai, "LIVE9") {
		t.Fatalf("AI entry re-rendered history that is already on screen: %q", ai)
	}
}

// A custom `[ai] prompt` (TOML must escape each backslash as \\ for \u/\w
// etc. to survive) renders its SGR badge and directory on the AI prompt
// line as real escape bytes — never as literal "\x1b" text (the expandEsc
// path in screen.PSExpand).
func TestAICustomPrompt(t *testing.T) {
	cfg := `[ai]
prompt = "\\[\\x1b[1;30;45m[MYAI]\\x1b[0m\\] \\w $ "
`
	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	// Custom prompt: switch to AI and wait for the prompt's shell-style
	// " $" ending (the default [AI]: marker is replaced by the config). The
	// trailing space after $ is erased by EraseToEOL in the renderer, so we
	// match on the space-before-$ which is always present.
	p.Write(defaultSwitchSeq)
	ai := r.readUntil(t, " $", waitTimeout)
	assertContains(t, ai, "[MYAI]") // custom badge text rendered (not leaked)
	assertContains(t, ai, "\x1b[")  // badge wrapped in a real SGR escape byte
	assertContains(t, ai, " $")     // custom prompt's shell-style ending
	if strings.Contains(ai, `\x1b`) || strings.Contains(ai, `\e`) {
		t.Fatalf("custom prompt leaked literal escape text: %q", ai)
	}
}

// The config `shell` value replaces the platform default: rysh launches the
// configured path (with login args) instead of shell.Default(). The wrapper
// writes a marker file before exec'ing the real shell, proving it ran.
func TestShellOverride(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "wrapper-ran")
	wrapper := filepath.Join(t.TempDir(), "fake-shell")
	script := "#!/bin/sh\necho SHELL_WRAPPER_RAN > \"$RYSH_WRAPPER_MARKER\"\nexec /bin/sh -l\n"
	if runtime.GOOS == "windows" {
		// CreateProcess cannot launch an extensionless POSIX script, so the
		// wrapper is a .cmd batch file; it chains to the same POSIX shell the
		// other tests use so the "$ " prompt still comes from the startup
		// profile.
		sh, ok := findProgram("sh")
		if !ok {
			t.Skip("no POSIX shell (Git Bash) available; integration tests need /bin/sh")
		}
		wrapper += ".cmd"
		script = "@echo off\r\n" +
			"echo SHELL_WRAPPER_RAN> \"%RYSH_WRAPPER_MARKER%\"\r\n" +
			"\"" + sh + "\" -l\r\n"
	}
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := fmt.Sprintf("shell = %q\n", wrapper)
	_, _, r := startRyshEnv(t, "echo RYSH_READY\nPS1='$ '\n", cfg, "RYSH_WRAPPER_MARKER="+marker)
	r.readUntil(t, "$ ", startupTimeout)

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("configured shell did not run: %v", err)
	}
	// The wrapper's echo terminates the line with CRLF (cmd) or LF (sh).
	markerText := strings.TrimSuffix(strings.TrimSuffix(string(data), "\r\n"), "\n")
	if markerText != "SHELL_WRAPPER_RAN" {
		t.Fatalf("marker = %q, want %q", data, "SHELL_WRAPPER_RAN")
	}
}

// The `[keys] mode_switch` binding replaces the default Shift+Tab. With the
// binding set, the configured key toggles shell/AI mode and the default
// Shift+Tab (and any Ctrl+Tab escape) is forwarded to the shell instead of
// switching. The harness drives the switch with one keystroke; because a
// Windows ConPTY decomposes every keystroke into a key-event report, only the
// default Shift+Tab gesture (canonical CSI Z, which rysh's decoder maps back
// to the mode-switch key) is injectable there, so on Windows the test sets
// mode_switch = "shift-tab" and injects CSI Z, exercising the same config-
// parsing path. On Unix the ctrl-space fallback (NUL) is raw-injectable.
func TestModeSwitchBinding(t *testing.T) {
	binding, switchKey, wantLabel := "ctrl-space", []byte{0x00}, "Ctrl+Space"
	if runtime.GOOS == "windows" {
		binding, switchKey, wantLabel = "shift-tab", []byte("\x1b[Z"), "Shift+Tab"
	}
	p, _, r := startRyshWithConfig(t, "[keys]\nmode_switch = \""+binding+"\"\n")
	startup := r.readUntil(t, "$ ", startupTimeout)
	// The banner hint line names the configured binding, not the default.
	assertContains(t, startup, wantLabel)

	// The configured key enters AI mode.
	p.Write(switchKey)
	ai := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	// The AI prompt carries the plain-magenta [AI]: marker, not a chip.
	if strings.Contains(ai, "\x1b[?1049h") {
		t.Fatalf("configured key entered the AI alt screen: %q", ai)
	}

	// The configured key again exits back to shell mode (empty draft).
	p.Write(switchKey)
	r.readUntil(t, "$ ", waitTimeout)

	// Ctrl+Tab is no longer (or never was) a mode switch: the escape is
	// forwarded to the shell, and the AI alt screen must never appear. The
	// shell's own line editor reacts to the raw escape bytes (it may split
	// them on ';' and print harmless "not found" errors), so the assertions
	// are that we never entered the alt screen and that the shell still
	// returns to a prompt.
	p.Write([]byte("\x1b[27;5;9~"))
	p.Write([]byte("\r"))
	out := r.readUntil(t, "$ ", waitTimeout)
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("Ctrl+Tab entered the AI alt screen: %q", out)
	}

	// Flush any partial line the shell may still hold, then confirm the
	// session still executes a command on its own line.
	p.Write([]byte("\r"))
	r.readUntil(t, "$ ", waitTimeout)
	p.Write([]byte("echo CTRLTAB_FORWARDED\r"))
	done := r.readUntil(t, "CTRLTAB_FORWARDED\r\n$ ", waitTimeout)
	assertContains(t, done, "echo CTRLTAB_FORWARDED")
}

// The `[tui] cursor_style` config is still parsed but no longer has any
// effect: rysh runs on the main screen and never rewrites the terminal cursor
// style, so both values behave identically. Both must still start rysh
// cleanly on the main screen with a plain prompt and without emitting any
// DECSCUSR sequence.
func TestTUIPrefs(t *testing.T) {
	for _, cs := range []string{"block", "default"} {
		t.Run(cs, func(t *testing.T) {
			_, _, r := startRyshWithConfig(t, "[tui]\ncursor_style = \""+cs+"\"\n")
			out := r.readUntil(t, "$ ", startupTimeout)
			assertContains(t, out, "rysh ·") // startup banner (main screen)
			for _, seq := range []string{"\x1b[2 q", "\x1b[5 q"} {
				if strings.Contains(out, seq) {
					t.Fatalf("cursor_style=%s still emitted DECSCUSR %q: %q", cs, seq, out)
				}
			}
		})
	}
}

// Scenario C: the AI draft and the shell input are one shared line. Typed
// in AI mode it is re-injected into the shell on return, and re-captured on
// the way back, so it survives a mode round trip; clearing the line in the
// shell (^C) consumes it, exactly as a submit would.
func TestDraftPersistsAcrossSwitch(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)
	p.Write([]byte("echo HELLO42"))
	r.readUntil(t, "echo HELLO42", waitTimeout)

	// Leave AI mode: the draft is re-injected as the shell's input line, and
	// the shell screen is restored without a clear (the shared stream is
	// untouched by the mode switch). The prompt repaint and the shell's echo
	// of the injected text race for the stream, so take the window up to the
	// echo and then everything until the stream goes quiet.
	p.Write(defaultSwitchSeq)
	sh := r.readUntil(t, "echo HELLO42", waitTimeout) // the re-injected draft echoed back
	sh += r.readQuiet(t, settle, waitTimeout)
	assertContains(t, sh, "echo HELLO42") // the draft came back into the shell line
	assertContains(t, visible(sh), "$")   // the shell prompt is back
	if strings.Contains(sh, "\x1b[2J") || strings.Contains(sh, "\x1b[3J") {
		t.Fatalf("leaving AI cleared the screen: %q", sh)
	}

	// Back into AI mode without touching the shell: the shared line is
	// re-captured, so the draft is restored on the prompt line.
	ai2 := enterAI(t, p, r)
	assertContains(t, ai2, "\x1b[35m[AI]:") // prompt redraws with the marker
	// The tail replay above the prompt reprint
	// the earlier lines verbatim, so the restored draft is what follows the
	// last marker, not just any occurrence in the window.
	if line := visible(ai2); !strings.Contains(line[strings.LastIndex(line, "[AI]:"):], "echo HELLO42") {
		t.Fatalf("draft not restored on the prompt line: %q", line)
	}

	// Clearing the line in the shell consumes it: leave again, wait for the
	// re-injected draft's echo so the line is in the tty's input queue, ^U
	// the shell's line editor (a line-discipline kill, so unlike ^C it does
	// not signal or disturb the shell), and a fresh command runs without
	// the old draft.
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "echo HELLO42", waitTimeout) // re-injected draft echoed back
	p.Write([]byte{0x15})                       // ^U clears the shell's input line
	p.Write([]byte("echo SHELL_AFTER\r"))
	out := r.readUntil(t, "SHELL_AFTER\r\n$ ", waitTimeout)
	assertContains(t, out, "SHELL_AFTER\r\n$ ")
	ai3 := enterAI(t, p, r)
	// Entering AI replays the session tail above the prompt, so the replay
	// reprints the earlier commands verbatim; only what follows the prompt
	// is the input line.
	line := visible(ai3)
	i := strings.LastIndex(line, "[AI]:")
	if i < 0 {
		t.Fatalf("AI prompt missing from %q", line)
	}
	if draft := line[i+len("[AI]:"):]; strings.TrimSpace(draft) != "" {
		t.Fatalf("shell ^U did not consume the shared line: %q", draft)
	}
}

// Scenario C': shell input left unsubmitted when switching to AI mode is
// carried into the AI draft (one shared line), so it comes back to the
// shell on return and the user's half-typed command survives the round trip
// and submits normally.
func TestShellResidualRestored(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Type a partial command in shell mode (no Enter yet).
	p.Write([]byte("echo PARTIAL"))
	r.readUntil(t, "echo PARTIAL", waitTimeout)

	// Switch to AI: the partial shell input becomes the AI draft and is
	// cleared from the shell's own line editor.
	enterAI(t, p, r)

	// Switch back: the draft is re-injected, restoring the command on the
	// shell line. The prompt repaint and the shell's echo of the restored
	// text race for the stream, so take the window up to the echo and then
	// everything until the stream goes quiet.
	p.Write(defaultSwitchSeq)
	sh := r.readUntil(t, "echo PARTIAL", waitTimeout) // restored command echoed
	sh += r.readQuiet(t, settle, waitTimeout)
	assertContains(t, sh, "echo PARTIAL") // restored command echoed
	assertContains(t, visible(sh), "$")   // the shell prompt is back

	// Submitting it runs the restored command.
	p.Write([]byte("\r"))
	out := r.readUntil(t, "PARTIAL\r\n$ ", waitTimeout)
	assertContains(t, out, "PARTIAL\r\n$ ") // its output, then the prompt
}

// Cursor preservation across the mode switch, end to end (bash only):
// a partial command with the cursor parked mid-line survives both
// directions at the same column. Shell -> AI: the draft is seeded with the
// shell cursor's column, so a typed character inserts mid-draft. AI ->
// shell: the re-injected line is followed by Left arrows that park the
// shell's cursor back at the draft's column, so the next typed character
// inserts there too. The test needs a line-editor shell (plain dash has no
// line editor and would garble the arrow bytes), so it is gated on bash.
func TestCursorPreservedAcrossModeSwitch(t *testing.T) {
	p, _, r := startRyshBash(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Type a partial command and move the cursor left by three: it now
	// sits after "echo AB".
	p.Write([]byte("echo ABCDE"))
	r.readUntil(t, "echo ABCDE", waitTimeout)
	p.Write([]byte("\x1b[D\x1b[D\x1b[D"))

	// Shell -> AI: the draft carries the full line and the cursor column,
	// so the X inserts after "echo AB" instead of at the end.
	enterAI(t, p, r)
	p.Write([]byte("X"))
	ai := r.readUntil(t, "echo ABXCDE", waitTimeout)
	assertContains(t, ai, "echo ABXCDE")

	// AI -> shell: the line comes back and the shell cursor is parked at
	// the draft column (after the X), so the next character inserts there.
	// rysh checks the echo of what it injected and re-types a line that came
	// back short, so the whole line is here whichever prompt repaint the
	// terminal chose to show.
	p.Write(defaultSwitchSeq)
	sh := r.readUntil(t, "echo ABXCDE", waitTimeout) // re-injected line echoed
	sh += r.readQuiet(t, settle, waitTimeout)
	vsh := visible(sh)
	assertContains(t, vsh, "echo ABXCDE")
	// sh opens at the mode switch, so every $ in it is the prompt coming back
	// with the resync repaint. The line the shell ends up holding is the last
	// copy in the window: a re-injection the terminal dropped part of is
	// cleared and re-typed, which leaves the discarded attempt in the byte
	// stream (erased on screen, with no erase bytes of its own). ConPTY also
	// redraws the prompt either as "$" + erase-to-EOL + " " or, inside a
	// full-screen reflow, as "$" + erase-to-EOL + CR LF with the space folded
	// into the erased cells. So what the stream can be asked to show is the
	// pairing: the prompt, then the line.
	if j := strings.LastIndex(vsh, "echo ABXCDE"); !strings.Contains(vsh[:j], "$") {
		t.Fatalf("residual re-injected with no redrawn prompt before it: %q", vsh)
	}

	// Z lands mid-line (after the X, not at the end): submitting the line
	// runs "echo ABXZCDE", and its output proves the insertion point. The
	// output is matched at the start of its own line, because a shell line
	// still carrying a discarded copy would print this text too, just not on
	// a line of its own.
	p.Write([]byte("Z\r"))
	out := r.readUntil(t, "ABXZCDE", waitTimeout)
	out += r.readQuiet(t, settle, waitTimeout)
	assertContains(t, visible(out), "\nABXZCDE\n")
}

// Regression: leaving AI mode restores the shell's own prompt in place, on
// the line the AI prompt occupied, instead of pinning the input line to the
// bottom of the scroll region. The resync prompt (\r\n$ for the /bin/sh
// child here) appears after the erased AI block, and \x1b[23H must not
// appear, so the input line does not jump across the screen when switching
// back to the shell.
func TestLeaveAIRestoresPromptInPlace(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Type a partial command in shell mode (no Enter yet), leave for AI, and
	// come back: the command is restored on the shell line.
	p.Write([]byte("echo POSITION"))
	r.readUntil(t, "echo POSITION", waitTimeout)
	enterAI(t, p, r)
	p.Write(defaultSwitchSeq)
	out := r.readUntil(t, "echo POSITION", waitTimeout) // restored command echoed
	out += r.readQuiet(t, settle, waitTimeout)

	assertContains(t, out, "\r\n$") // shell prompt redrawn by the resync
	if i, j := strings.Index(out, "\r\n$"), strings.Index(out, "echo POSITION"); j < i {
		t.Fatalf("restored command precedes the redrawn prompt: %q", out)
	}
	if strings.Contains(out, "\x1b[23H") {
		t.Fatalf("input line pinned to the bottom; got %q", out)
	}
}

// Ctrl+Z (^Z) undoes the last draft edit in AI mode, end to end through the
// pty and the input reader: the byte is classified as Other, forwarded to
// the editor, and the re-rendered prompt line shows the restored text. The
// redraw erases the previous line (\x1b[2K) and re-emits the draft, so the
// restored text after the "[AI] " prompt proves the undone state.
func TestAIUndo(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Type a three-rune draft; the prompt line ends with the full text.
	p.Write([]byte("xyz"))
	r.readUntil(t, "xyz", waitTimeout)

	// ^Z removes the last rune: the redraw erases the row and re-emits the
	// prompt with the shorter draft. The "[AI]: xyz" render is behind the
	// read window, so the settled window can only hold the undone state;
	// what follows the draft is the renderer's cursor reposition (which
	// ConPTY rewrites again on Windows), so compare the visible text.
	p.Write([]byte{0x1a})
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: xy") {
		t.Fatalf("Ctrl+Z left %q on the line, want it to end with %q", got, "[AI]: xy")
	}

	// A second ^Z leaves "[AI]: x".
	p.Write([]byte{0x1a})
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: x") {
		t.Fatalf("second Ctrl+Z left %q on the line, want it to end with %q", got, "[AI]: x")
	}
}

// A multi-line paste (DEC 2004 bracketed paste) inserts the whole block into
// the AI draft without submitting it: the markers wrap the content exactly as
// a real terminal emits them, and the embedded newline must become a literal
// line break in the draft, never a submit. The pty does not itself emit the
// markers, so the test injects the wire bytes directly.
func TestAIMultiLinePaste(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Paste "ab\ncd" wrapped in DEC 2004 markers; a real terminal would send
	// exactly these bytes (the \r is what the terminal puts on the wire).
	p.Write([]byte("\x1b[200~ab\r\ncd\x1b[201~"))
	// Both visual lines of the draft must be visible on the screen...
	r.readUntil(t, "ab", waitTimeout)
	settled := r.readQuiet(t, settle, waitTimeout)
	if !strings.Contains(visible(settled), "cd") {
		t.Fatalf("pasted second line missing from the draft; got %q", visible(settled))
	}
	// ...and the paste must not have submitted the first line (no task
	// start, no model error notice on the stream).
	if got := visible(settled); strings.Contains(got, "Task:") || strings.Contains(got, "no models configured") {
		t.Fatalf("paste submitted the draft: %q", got)
	}

	// One ^Z undoes the whole paste block at once.
	p.Write([]byte{0x1a})
	if got := visible(r.readQuiet(t, settle, waitTimeout)); strings.Contains(got, "ab") {
		t.Fatalf("whole-block undo failed; line still shows %q", got)
	}
}

// A space typed at a fresh prompt enters AI mode and is swallowed; an empty
// draft then returns to shell mode without injecting anything.
func TestSpaceAtLineStartEntersAI(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte(" "))
	win := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	if strings.Contains(win, "\x1b[?1049h") {
		t.Fatalf("space entered the AI alt screen: %q", win)
	}

	// Back to shell with an empty draft: nothing is forwarded.
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "$ ", waitTimeout)

	// The shell prompt is still a plain, untyped line: the space was not
	// forwarded, so the next command starts at column one.
	p.Write([]byte("echo SPACEOK\r"))
	out := r.readUntil(t, "SPACEOK\r\n$ ", waitTimeout)
	assertContains(t, out, "SPACEOK\r\n$ ")
}

// A space typed mid-line is forwarded as a normal character, not a
// mode-switch. After typing "ech", the space must reach the shell so the
// command becomes "ech o hi" (which fails) rather than "echo hi".
func TestMidLineSpaceForwarded(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("ech"))
	r.readUntil(t, "ech", waitTimeout) // the shell has now seen input
	p.Write([]byte(" o hi\r"))
	out := r.readUntil(t, "not found", waitTimeout)
	assertContains(t, out, "not found") // "ech: not found" proves space passed
}

// Regression: after a shell→AI→shell round trip the leading space is a mode
// switch again. Previously the re-injected residual (and any typing before
// AI mode) left fwdSinceNewline true, so the next space was forwarded to
// the shell instead of re-entering AI.
func TestLeadingSpaceSwitchAfterRoundTrip(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Type a partial command, leave for AI and come back: the residual is
	// restored on the shell line.
	p.Write([]byte("echo ROUND"))
	r.readUntil(t, "echo ROUND", waitTimeout)
	enterAI(t, p, r)
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "echo ROUND", waitTimeout) // residual restored

	// The leading space after the round trip must re-enter AI mode.
	p.Write([]byte(" "))
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
}

// A leading space on an empty AI draft returns to shell mode (Esc no longer
// does), and a space mid-draft stays a normal character.
func TestLeadingSpaceLeavesAI(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Esc with an empty draft no longer leaves AI mode; the following space
	// returns to shell mode. (A brief pause separates the lone ESC from the
	// space; otherwise the reader reports \x1b+space as one alt-space combo.)
	p.Write([]byte{0x1b})
	time.Sleep(100 * time.Millisecond)
	p.Write([]byte(" "))
	r.readUntil(t, "$ ", waitTimeout)

	// Re-enter AI: a space mid-draft is a normal character, not a switch.
	enterAI(t, p, r)
	p.Write([]byte("a b"))
	r.readUntil(t, "a b", waitTimeout)
}

// A `!`-leading input submitted in AI mode is a shell command mistyped into
// the AI prompt: it is not sent to the model, the draft is kept, and a
// notice offers the two escape routes. The mode-switch key then hands the
// line to the shell with the `!` stripped, ready to run; clearing the draft
// first (Esc) and pressing a leading space switches with an empty line.
func TestBangInputNoticesAndShellSwitchStripsBang(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Submit a `!` command: the notice names both routes and the draft
	// survives so the prompt line still shows it.
	p.Write([]byte("!echo BANGMARK7\r"))
	out := r.readUntil(t, "再输入命令", waitTimeout)
	assertContains(t, out, "AI 模式下不执行")
	assertContains(t, out, "切换到 shell 模式并保留当前输入")
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: !echo BANGMARK7") {
		t.Fatalf("draft not kept after the notice: %q", got)
	}

	// The mode-switch key hands the line to the shell with the `!`
	// stripped: the shell's line editor echoes the bare command.
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "echo BANGMARK7", waitTimeout)

	// Clear the shell line and go back to AI with an empty draft: a second
	// `!` input gets the notice again, and Esc + leading space leaves with
	// an empty line (nothing to strip or re-inject).
	p.Write([]byte{0x15}) // ^U kills the shell's line
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	p.Write([]byte("!pwd\r"))
	r.readUntil(t, "再输入命令", waitTimeout)
	p.Write([]byte{0x1b}) // Esc clears the draft
	time.Sleep(100 * time.Millisecond)
	p.Write([]byte(" "))
	r.readUntil(t, "$ ", waitTimeout)
}

// Regression: rysh terminates and forwards its exit code when the shell
// exits.
func TestExitCodeForwarded(t *testing.T) {
	p, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("exit\r"))
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the shell exited")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %v, want 0", c.ProcessState)
	}
}

// Regression: on exit rysh resets colors/cursor/hidden-cursor in one
// contiguous write and hands the terminal back on the main screen (no clear,
// no alternate-screen leave), so the user's scrollback is preserved.
func TestExitRestoresScreen(t *testing.T) {
	p, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("exit\r"))
	// The exit path writes Reset() (colors + default cursor + show cursor) in one contiguous write.
	r.readUntil(t, "\x1b[0 q", waitTimeout)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the shell exited")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %v, want 0", c.ProcessState)
	}
}

// TestExitHandsBackTerminal checks the exit handoff resets colors and
// cursor style and returns to the main screen without clearing it: the exit
// path writes Reset() (colors + default cursor + show cursor) in one run.
func TestExitHandsBackTerminal(t *testing.T) {
	p, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("exit\r"))
	r.readUntil(t, "\x1b[0 q", waitTimeout)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the shell exited")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %v, want 0", c.ProcessState)
	}
}

// A child shell killed by a signal makes rysh exit with the shell-style
// 128+signal code (137 for SIGKILL) instead of a raw -1.
func TestSignalTerminatedExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-derived exit codes are Unix-only")
	}
	p, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	p.Write([]byte("kill -9 $$\r"))
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the shell was killed")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 137 {
		t.Fatalf("exit code = %v, want 137 (SIGKILL)", c.ProcessState)
	}
}

// SIGTERM to rysh is forwarded to the shell; because an interactive shell
// ignores SIGTERM, rysh force-kills it after 3s and exits promptly with the
// signal-derived code, restoring the terminal.
func TestSigtermCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM semantics differ on Windows")
	}
	_, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	_ = c.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatalf("rysh did not exit within 6s of SIGTERM")
	}
	code := c.ProcessState.ExitCode()
	if code != 143 && code != 137 { // 143 = child took SIGTERM, 137 = force-killed
		t.Fatalf("exit code = %d, want 143 or 137", code)
	}
	// The terminal is handed back: the exit path writes Reset() (colors + default cursor + show cursor).
	r.readUntil(t, "\x1b[0 q", waitTimeout)
}

// A panic anywhere in rysh leaves the terminal usable and exits non-zero
// (2, the Go runtime's panic exit code) rather than leaving the user stuck
// in the alternate screen with a hidden cursor.
func TestPanicCleanup(t *testing.T) {
	_, c, r := startRyshWithConfig(t, "", "RYSH_TEST_PANIC=1")

	// The crash handler must reset colors and cursor style and re-show the
	// cursor (rysh ran on the main screen, so there is no alternate screen
	// to leave), all in one contiguous write.
	r.readUntil(t, "\x1b[0 q", 12*time.Second)

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the panic")
	}
	if c.ProcessState == nil || c.ProcessState.ExitCode() != 2 {
		t.Fatalf("exit code = %v, want 2 (unhandled panic)", c.ProcessState)
	}
}

// twoModelConfig is a provider config with a default and two models, used
// by the /model scenarios.
const twoModelConfig = `
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
name = "Llama 3.1 8B"

[[providers.ollama.models]]
id = "qwen2.5-coder:7b"
`

// Scenario D: /model (no argument) shows the active model and a numbered
// list of the available models (the current one marked); the AI view header
// carries the active model ref.
func TestModelCommand(t *testing.T) {
	p, _, r := startRyshWithConfig(t, twoModelConfig)
	r.readUntil(t, "$ ", startupTimeout)

	// Enter AI mode: the header shows the default model ref.
	ai := enterAI(t, p, r)
	assertContains(t, ai, "ollama/llama3.1:8b")

	// /model lists every available model, numbered, the active one marked
	// with a leading > (the session list's way).
	p.Write([]byte("/model\r"))
	out := r.readUntil(t, "2. ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "> 1. ollama/llama3.1:8b")
	assertContains(t, out, "2. ollama/qwen2.5-coder:7b")
}

// Scenario E: /model <provider/model> switches the active model immediately
// and the view header reflects the new model.
func TestModelSwitch(t *testing.T) {
	p, _, r := startRyshWithConfig(t, twoModelConfig)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("/model ollama/qwen2.5-coder:7b\r"))
	out := r.readUntil(t, "model: ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "model: ollama/qwen2.5-coder:7b")
	// The next AI prompt line names the switched model (§3.3).
	next := r.readUntil(t, "[AI]:", waitTimeout)
	assertContains(t, next, "ollama/qwen2.5-coder:7b")
}

// Scenario E2: /model <N> picks the Nth model from the /model listing, the
// same as /model <provider/model>; out-of-range numbers are rejected.
func TestModelSwitchByNumber(t *testing.T) {
	p, _, r := startRyshWithConfig(t, twoModelConfig)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("/model 2\r"))
	out := r.readUntil(t, "model: ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "model: ollama/qwen2.5-coder:7b")
	// The next AI prompt line names the switched model (§3.3).
	next := r.readUntil(t, "[AI]:", waitTimeout)
	assertContains(t, next, "ollama/qwen2.5-coder:7b")

	// A number beyond the list is rejected and the model stays unchanged:
	// the re-drawn prompt line still names the previous model.
	p.Write([]byte("/model 9\r"))
	out = r.readUntil(t, "编号超出范围", waitTimeout)
	assertContains(t, out, "编号超出范围")
	next = r.readUntil(t, "[AI]:", waitTimeout)
	assertContains(t, next, "ollama/qwen2.5-coder:7b")
}

// Scenario F: editing the config file while rysh is running takes effect on
// the next /model — no restart needed (hot reload).
func TestModelHotReload(t *testing.T) {
	const initial = `
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
`
	const edited = initial + `
[[providers.ollama.models]]
id = "qwen2.5-coder:7b"
`
	p, _, r := startRyshWithConfig(t, initial)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// First listing sees only the model present at startup.
	p.Write([]byte("/model\r"))
	first := r.readUntil(t, "1. ollama/llama3.1:8b", waitTimeout)
	if strings.Contains(first, "qwen2.5-coder:7b") {
		t.Fatalf("new model listed before the config edit")
	}

	// Edit the config file while rysh keeps running.
	rewriteConfig(t, edited)

	// The next /model re-reads the file and sees the new model immediately.
	p.Write([]byte("/model\r"))
	second := r.readUntil(t, "qwen2.5-coder:7b", waitTimeout)
	assertContains(t, second, "2. ollama/qwen2.5-coder:7b")

	// The newly added model can be switched to without a restart.
	p.Write([]byte("/model ollama/qwen2.5-coder:7b\r"))
	r.readUntil(t, "model: ollama/qwen2.5-coder:7b", waitTimeout)
}

// With no config file, /model reports that no models are configured.
func TestModelNoConfig(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("/model\r"))
	out := r.readUntil(t, "no models configured", waitTimeout)
	assertContains(t, out, "no models configured")
}

// An unknown slash command is rejected with feedback and does not disturb
// the AI session.
func TestUnknownSlashCommand(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("/bogus\r"))
	out := r.readUntil(t, "unknown command: /bogus", waitTimeout)
	assertContains(t, out, "unknown command: /bogus")
}

// streamChunk builds one SSE data payload for a chat completion delta.
func streamChunk(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{"content": content}}},
	})
	return string(b)
}

// streamUsageChunk builds the final SSE usage payload
// (stream_options.include_usage).
func streamUsageChunk(prompt, total int) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": total - prompt, "total_tokens": total},
	})
	return string(b)
}

// M7.6 end to end: a long task against a tiny context window crosses the
// 85% soft trigger, the harness compacts once (checkpoint = summarizer
// answer + mechanical section), the next request is re-projected over the
// checkpoint (mission verbatim, summarized rounds gone, kept tail
// verbatim), a second task in the same session inherits the shrunk
// history instead of the absorbed prefix, and the session log records the
// compact event while the disk transcript keeps the full exchange.
func TestAgentContextCompaction(t *testing.T) {
	requireLinuxProc(t)
	// Two ~1.5KB tool outputs: at compaction time the tail budget
	// (25% of 4096 tokens ≈ 4KB) keeps the newest round but must drop
	// the older one, so the cut lands between the rounds.
	marker1 := strings.Repeat("X", 1500)
	marker2 := strings.Repeat("Y", 1500)
	summary := strings.Repeat("这是交接摘要，涵盖使命、约束、进度与关键决策。", 30)
	mission := "压缩演练：先做两轮大输出，再汇报"
	echoTool := func(id, marker string) string {
		args, _ := json.Marshal(map[string]string{"command": "echo " + marker})
		return streamToolChunk(id, "bash", string(args))
	}

	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()

		var chunks []string
		ans := ""
		usage := 0
		switch n {
		case 0: // round 1: big output, usage well under the trigger
			chunks = []string{echoTool("call_1", marker1)}
			usage = 1200 // 29% of 4096
		case 1: // round 2: the request now carries marker1 …
			if !strings.Contains(string(body), strings.Repeat("X", 40)) {
				http.Error(w, "round-1 output missing from context", http.StatusBadRequest)
				return
			}
			chunks = []string{echoTool("call_2", marker2)}
			usage = 3600 // 88% → trigger at the next boundary
		case 2: // the summarizer: instruction last, no tools advertised
			if !strings.Contains(string(body), "执行上下文压缩") {
				http.Error(w, "turn 2 is not the summarizer request", http.StatusBadRequest)
				return
			}
			if !strings.Contains(string(body), mission) {
				http.Error(w, "summarizer request lacks the mission", http.StatusBadRequest)
				return
			}
			if strings.Contains(string(body), `"tools":[`) {
				http.Error(w, "summarizer request advertises tools", http.StatusBadRequest)
				return
			}
			ans = summary // ≥ minSummaryRunes, so the checkpoint installs
		case 3: // post-compaction round: checkpoint projection
			b := string(body)
			if !strings.Contains(b, "[上下文已压缩]") || !strings.Contains(b, "## 当前状态") {
				http.Error(w, "compacted view lacks the checkpoint message", http.StatusBadRequest)
				return
			}
			if !strings.Contains(b, mission) {
				http.Error(w, "compacted view lacks the verbatim mission", http.StatusBadRequest)
				return
			}
			if !strings.Contains(b, strings.Repeat("Y", 40)) {
				http.Error(w, "compacted view lost the kept tail", http.StatusBadRequest)
				return
			}
			if strings.Contains(b, strings.Repeat("X", 40)) {
				http.Error(w, "compacted view still carries the summarized prefix", http.StatusBadRequest)
				return
			}
			ans = "TASK_ONE_DONE"
			usage = 800
		case 4: // second task: the driver's history stays shrunk
			b := string(body)
			if !strings.Contains(b, "[上下文已压缩]") || !strings.Contains(b, "TASK_ONE_DONE") {
				http.Error(w, "second task lost the checkpointed history", http.StatusBadRequest)
				return
			}
			if !strings.Contains(b, "第二个任务") {
				http.Error(w, "second task prompt missing", http.StatusBadRequest)
				return
			}
			if strings.Contains(b, strings.Repeat("X", 40)) || strings.Contains(b, mission) {
				http.Error(w, "second task resent the absorbed prefix", http.StatusBadRequest)
				return
			}
			ans = "TASK_TWO_DONE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		if ans != "" {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
			fl.Flush()
		}
		if usage > 0 {
			fmt.Fprintf(w, "data: %s\n\n", streamUsageChunk(usage, usage+50))
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
context_window = 4096

[agent]
approval = "auto"
`, srv.URL)

	home := t.TempDir()
	p, _, r := startRyshIn(t, home, "echo RYSH_READY\nPS1='$ '\n", cfg, nil)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Task one: two big rounds, the trigger fires between them.
	p.Write([]byte(mission + "\r"))
	out := r.readUntil(t, "TASK_ONE_DONE", waitTimeout)
	assertContains(t, out, "─── compacting ───")
	assertContains(t, out, "[已压缩上下文: 保留近 1 轮]")

	// Task two in the same AI session: it must run on the shrunk history.
	p.Write([]byte("第二个任务：汇报现状\r"))
	r.readUntil(t, "TASK_TWO_DONE", waitTimeout)

	// Session log: exactly one compact event carrying the checkpoint full
	// text, while the disk transcript still holds the summarized-away
	// round (history is never rewritten — 不变式①).
	entries, err := os.ReadDir(filepath.Join(home, ".rysh", "sessions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("sessions = %v (err %v), want exactly one", entries, err)
	}
	logData := readSessionLog(t, home, entries[0].Name())
	var compacts []session.Record
	for _, ln := range strings.Split(strings.TrimSpace(logData), "\n") {
		var rec session.Record
		if json.Unmarshal([]byte(ln), &rec) == nil && rec.Kind == "compact" {
			compacts = append(compacts, rec)
		}
	}
	if len(compacts) != 1 {
		t.Fatalf("compact records = %d, want 1", len(compacts))
	}
	if !strings.Contains(compacts[0].P, "这是交接摘要") || !strings.Contains(compacts[0].P, "## 当前状态") {
		t.Fatalf("compact record lacks the summary + mechanical section: %q", compacts[0].P)
	}
	if !strings.Contains(logData, strings.Repeat("X", 40)) {
		t.Fatalf("disk log lost the summarized-away round output")
	}
}

// Scenario G: a natural-language prompt in AI mode is sent to the active
// model as a streaming chat completion and the answer appears in the view's
// reply block; rysh stays in AI mode so the user can follow up. The mock
// endpoint also verifies the conversation history grows across turns, and
// that /new resets it (matching pi's /new).
func TestAIPromptStreaming(t *testing.T) {
	requireLinuxProc(t)
	// A mock OpenAI-compatible endpoint that validates each turn's request
	// against the expected conversation history, then streams a fixed
	// answer. The first answer contains a newline to exercise rysh's
	// rendering of streamed newlines as row breaks.
	type msg struct{ role, content string }
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Every request leads with one system message — the shell cwd,
		// its environment, and the agent instructions, merged at the
		// provider boundary (M4 cwd/env tracking + tool calling; strict
		// OpenAI-compatible endpoints allow at most one leading system) —
		// and the remaining messages are the conversation.
		if len(req.Messages) < 2 || req.Messages[0].Role != "system" ||
			!strings.HasPrefix(req.Messages[0].Content, "cwd: ") ||
			!strings.Contains(req.Messages[0].Content, "\n\nenv:") {
			http.Error(w, "missing context preamble", http.StatusBadRequest)
			return
		}
		got := make([]msg, len(req.Messages)-1)
		for i, m := range req.Messages[1:] {
			got[i] = msg{m.Role, m.Content}
		}
		// Expected conversation for each turn: turn 0 is a single user
		// message, turn 1 carries the previous exchange, turn 2 (after
		// /new) is back to a single user message.
		want := [][]msg{
			{{"user", "how do i list files"}},
			{{"user", "how do i list files"}, {"assistant", "you can use `ls`\n to list files"}, {"user", "what about hidden files?"}},
			{{"user", "and symlinks?"}},
		}
		ans := []string{
			"you can use `ls`\n to list files",
			"use `ls -a` to include hidden files",
			"use `ls -l` to see symlinks",
		}
		if turn >= len(want) || req.Model != "llama3.1:8b" || !req.Stream ||
			len(got) != len(want[turn]) || !slices.Equal(got, want[turn]) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		a := ans[turn]
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		half := len(a) / 2
		for _, part := range []string{a[:half], a[half:]} {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(part))
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Turn 1: the echoed prompt and the streamed answer appear in the
	// view, inside the magenta reply block. The answer's newline splits
	// into a second row (" to list files").
	p.Write([]byte("how do i list files\r"))
	out := r.readUntil(t, " to list files", waitTimeout)
	assertContains(t, out, "\x1b[35m[AI]:")
	assertContains(t, out, "how do i list files")
	// Inline code is theme-aware: on the harness's fixed 256-color terminal
	// the inline-code foreground quantizes to 38;5;117.
	assertContains(t, out, "you can use \x1b[38;5;117mls\x1b[0m")

	// Turn 2: the mock sees the first exchange in the request context.
	p.Write([]byte("what about hidden files?\r"))
	follow := r.readUntil(t, "\x1b[38;5;117mls -a\x1b[0m", waitTimeout)
	assertContains(t, follow, "\x1b[35m[AI]:")
	assertContains(t, follow, "what about hidden files?")

	// /new creates a fresh session and switches to it: the confirmation is
	// printed and the next request carries no history.
	p.Write([]byte("/new\r"))
	got := r.readUntil(t, "已新建会话", waitTimeout)
	assertContains(t, got, "已新建会话")
	p.Write([]byte("and symlinks?\r"))
	after := r.readUntil(t, "\x1b[38;5;117mls -l\x1b[0m", waitTimeout)
	assertContains(t, after, "\x1b[35m[AI]:")
	assertContains(t, after, "and symlinks?")
}

// Scenario G”: after a prompt is submitted, the inline pre-output spinner
// ("正在思考") shows in the reply area while the model is silent, instead of
// a fresh prompt inviting the next input. The mock holds the first token so
// the wait is observable; the answer arrives only after the spinner.
func TestAISpinnerBeforeOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Hold the first token long enough for the spinner to be visible.
		time.Sleep(700 * time.Millisecond)
		for _, part := range []string{"hello ", "world"} {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(part))
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("wait for it\r"))
	// The inline spinner appears before any output.
	wait := r.readUntil(t, "正在思考", waitTimeout)
	if strings.Contains(wait, "hello world") {
		t.Fatalf("output landed before the spinner: %q", wait)
	}
	// The output lands only after the spinner has shown.
	out := r.readUntil(t, "hello world", waitTimeout)
	assertContains(t, out, "hello world")
}

// Scenario G” (elapsed): the pre-output spinner's caption carries a
// grok-style elapsed counter that counts the wait for the first token
// (1s, 2s, 1m56s). The mock holds the token just over two seconds, so the
// counter is guaranteed to cross two seconds on screen before the answer
// lands and erases the spinner.
func TestAISpinnerElapsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Hold the first token well past two seconds.
		time.Sleep(2500 * time.Millisecond)
		for _, part := range []string{"counted"} {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(part))
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("count it\r"))
	r.readUntil(t, "正在思考", waitTimeout)
	// The ticker rewrites the whole spinner row every 250ms, so once the
	// wait crosses two seconds the row carries "正在思考... 2s".
	r.readUntil(t, "正在思考... 2s", waitTimeout)
	out := r.readUntil(t, "counted", waitTimeout)
	assertContains(t, out, "counted")
}

// The terminal cursor follows the task's busy state: it is hidden while the
// thinking spinner and the streaming reply are the live indicators (neither
// needs a cursor to mark focus or the input point), and re-shown once the
// task settles at the fresh prompt. The mock holds the first token so the
// spinner (thinking) is on screen, then streams the answer; the test pins
// the cursor-hide to the spinner window, proves no cursor-show leaks into
// the streaming window, and waits for the cursor-show after the reply.
func TestCursorHiddenDuringBusyTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// Hold the first token so the thinking spinner is visible.
		time.Sleep(700 * time.Millisecond)
		for _, part := range []string{"hello ", "world"} {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(part))
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("hi there\r"))
	// Thinking: the spinner is armed and the cursor is hidden with it.
	wait := r.readUntil(t, "正在思考", waitTimeout)
	if !strings.Contains(wait, "\x1b[?25l") {
		t.Fatalf("cursor not hidden while the thinking spinner runs: %q", wait)
	}
	// Streaming: the reply lands with the cursor still hidden — no cursor
	// show leaks in between the spinner and the settled prompt.
	out := r.readUntil(t, "hello world", waitTimeout)
	assertContains(t, out, "hello world")
	if strings.Contains(out, "\x1b[?25h") {
		t.Fatalf("cursor re-shown mid-task, before the reply settled: %q", out)
	}
	// Settled: the task finalizes at the fresh prompt and the cursor comes
	// back for the next input (readUntil fails the test if it never lands).
	r.readUntil(t, "\x1b[?25h", waitTimeout)
}

// Scenario G': ^C while a reply is streaming aborts the in-flight request
// and returns the view to a usable state. The mock streams tokens slowly
// so the cancel lands mid-stream, records whether the client aborted the
// first request, and answers the follow-up turn with distinct tokens so the
// test can prove a fresh stream starts after the cancel.
func TestAICancelStream(t *testing.T) {
	var mu sync.Mutex
	var reqs int
	cancelled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		reqs++
		n := reqs
		mu.Unlock()
		prefix := "tok"
		if n == 2 {
			prefix = "sec"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(fmt.Sprintf("%s%d ", prefix, i)))
			fl.Flush()
			select {
			case <-r.Context().Done():
				// The client (rysh) aborted the request on ^C.
				mu.Lock()
				if n == 1 {
					cancelled = true
				}
				mu.Unlock()
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Start a slow stream and wait for its first token to render.
	p.Write([]byte("cancel me\r"))
	r.readUntil(t, "tok0", waitTimeout)

	// ^C aborts the stream. The view stays in the streaming state until
	// the end-of-turn message lands, so give the cancellation time to
	// propagate before the next input.
	p.Write([]byte("\x03"))
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	got := cancelled
	mu.Unlock()
	if !got {
		t.Fatalf("server never observed the request being aborted on ^C")
	}

	// The view is usable again: a second prompt starts a fresh turn whose
	// (distinct) tokens render, proving the conversation is not stuck.
	p.Write([]byte("again\r"))
	out := r.readUntil(t, "sec0", waitTimeout)
	assertContains(t, out, "\x1b[35m[AI]:")
	assertContains(t, out, "again")
	assertContains(t, out, "sec0")
}

// Scenario H: the agent senses the shell's current directory. After cd-ing
// in shell mode, a prompt's request leads with a system message carrying the
// new cwd (M4 cwd tracking).
func TestAICWDContext(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	var sawCWD string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		if len(req.Messages) > 0 {
			sawCWD = req.Messages[0].Content
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("you are in the tracked directory"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	// cd to a known directory in shell mode, then ask the agent where it is.
	dir := filepath.Join(os.Getenv("HOME"), "workspace")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p.Write([]byte("cd " + dir + "\r"))
	r.readUntil(t, "$ ", waitTimeout)

	enterAI(t, p, r)
	p.Write([]byte("where am i\r"))
	r.readUntil(t, "you are in the tracked directory", waitTimeout)

	mu.Lock()
	cwd := sawCWD
	mu.Unlock()
	// The leading system message is the merged cwd/env/instructions run
	// (provider boundary): the cwd line still leads it.
	if !strings.HasPrefix(cwd, "cwd: "+dir+"\n") {
		t.Fatalf("agent saw cwd %q, want prefix %q", cwd, "cwd: "+dir)
	}
}

// TestOSC7CWD proves the OSC 7 cwd report wins over the /proc fallback: the
// login profile emits a one-shot OSC 7 report for a directory that differs
// from the process's real cwd (the disposable HOME), so the agent seeing the
// OSC 7 path shows the tracker's priority. This is the same path macOS and
// Windows will use, where /proc does not exist.
func TestOSC7CWD(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	var sawCWD string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		if len(req.Messages) > 0 {
			sawCWD = req.Messages[0].Content
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("osc7 cwd seen"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	// The one-shot report names a directory that differs from HOME, proving
	// the tracker used the OSC 7 report rather than the /proc fallback.
	profile := "printf '\\033]7;file://localhost/tmp/rysh-osc7-test\\033\\\\'\necho RYSH_READY\nPS1='$ '\n"

	p, _, r := startRyshEnv(t, profile, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	enterAI(t, p, r)
	p.Write([]byte("where am i\r"))
	r.readUntil(t, "osc7 cwd seen", waitTimeout)

	mu.Lock()
	cwd := sawCWD
	mu.Unlock()
	// The leading system message is the merged cwd/env/instructions run
	// (provider boundary): the cwd line still leads it.
	if !strings.HasPrefix(cwd, "cwd: /tmp/rysh-osc7-test\n") {
		t.Fatalf("agent saw cwd %q, want prefix %q", cwd, "cwd: /tmp/rysh-osc7-test")
	}
}

// Scenario I: a reply containing a bash-fenced block is executed as a shell
// command (M4 agent tool calling); a one-line observation appears in the
// view and the captured output is fed back into the conversation context,
// which the mock verifies on the follow-up request.
func TestAgentToolCall(t *testing.T) {
	requireLinuxProc(t)
	type msg struct{ role, content string }
	var turn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Every request leads with the one merged cwd/env/instructions
		// system message (the provider boundary collapses the leading
		// system run; strict endpoints allow at most one leading system).
		if len(req.Messages) < 2 || req.Messages[0].Role != "system" ||
			!strings.HasPrefix(req.Messages[0].Content, "cwd: ") ||
			!strings.Contains(req.Messages[0].Content, "\n\nenv:") {
			http.Error(w, "missing context preamble", http.StatusBadRequest)
			return
		}
		got := make([]msg, len(req.Messages)-1)
		for i, m := range req.Messages[1:] {
			got[i] = msg{m.Role, m.Content}
		}
		// The answer that carries a native bash tool call for rysh to run.
		const toolReply = "I'll run a command."
		var ans string
		var chunks []string
		switch turn {
		case 0:
			want := []msg{{"user", "list the files"}}
			if !slices.Equal(got, want) {
				http.Error(w, "unexpected first request", http.StatusBadRequest)
				return
			}
			ans = toolReply
			chunks = []string{bashCallChunk("call_1", "echo AGENT_TOOL_OK")}
		case 1:
			// The automatic follow-up request must carry the executed
			// command's captured output as a role:"tool" message with
			// the [bash] result header (agent loop §6, M7.2 wire shape).
			if len(got) != 3 || got[0] != (msg{"user", "list the files"}) ||
				got[1] != (msg{"assistant", toolReply}) ||
				got[2].role != "tool" || !strings.Contains(got[2].content, "AGENT_TOOL_OK") ||
				!strings.Contains(got[2].content, "exit 0") {
				http.Error(w, "tool result missing from context", http.StatusBadRequest)
				return
			}
			ans = "it echoed AGENT_TOOL_OK"
		case 2:
			// The next prompt arrives with the whole first turn (prompt,
			// tool reply, tool result, final answer) in context.
			if len(got) != 5 {
				http.Error(w, "unexpected follow-up", http.StatusBadRequest)
				return
			}
			want := []msg{
				{"user", "list the files"},
				{"assistant", toolReply},
				{"tool", got[2].content}, // tool result, validated above
				{"assistant", "it echoed AGENT_TOOL_OK"},
				{"user", "what did the command output"},
			}
			if !slices.Equal(got, want) {
				http.Error(w, "unexpected follow-up", http.StatusBadRequest)
				return
			}
			ans = "the tool output was AGENT_TOOL_OK"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
		}
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Prompt; rysh runs the bash block in the reply, shows a gray
	// observation line (command + exit code), and automatically calls the
	// model again with the captured output, which answers the prompt.
	p.Write([]byte("list the files\r"))
	out := r.readUntil(t, "it echoed AGENT_TOOL_OK", waitTimeout)
	assertContains(t, out, "\x1b[35m[AI]:")
	assertContains(t, out, "list the files")
	assertContains(t, out, "I'll run a command.")
	assertContains(t, out, "[tool] $ echo AGENT_TOOL_OK (exit 0)")

	// The next prompt arrives with the whole first turn (tool result +
	// final answer) in context.
	p.Write([]byte("what did the command output\r"))
	follow := r.readUntil(t, "the tool output was AGENT_TOOL_OK", waitTimeout)
	assertContains(t, follow, "\x1b[35m[AI]:")
	assertContains(t, follow, "what did the command output")
}

// Scenario I': a model that keeps answering with identical tool calls is
// caught by the LoopGuard, not by a round counter: warnings ride along on
// repeated results, and once the consecutive run reaches loopBreakConsec
// the task ends with one forced text-only wrap-up answer (architecture §3.9).
func TestAgentToolLoopGuard(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	reqs := 0
	warnSeen := false
	// The wrap-up round forbids tools; the mock answers in text then.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs++
		n := reqs
		if strings.Contains(string(body), "[rysh] 警告：这是完全相同的命令与输出的第") {
			warnSeen = true
		}
		mu.Unlock()

		var chunks []string
		ans := "still exploring"
		if strings.Contains(string(body), "执行预算已到极限") {
			ans = "WRAPPED_UP_DONE"
		} else {
			chunks = []string{bashCallChunk(fmt.Sprintf("call_%d", n), "echo LOOP_AGENT")}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// The guard lets loopBreakConsec-1 identical rounds through with the
	// warning riding along from the second run, breaks on the sixth and
	// forces one wrap-up reply instead of stopping dead (readUntil times
	// out if the loop never terminates). 6 is agent.loopBreakConsec (§6.4).
	const loopBreakConsec = 6
	p.Write([]byte("explore\r"))
	out := r.readUntil(t, "wrapping up (loop guard)", waitTimeout)
	if got := strings.Count(out, "[tool] $ echo LOOP_AGENT (exit 0)"); got != loopBreakConsec {
		t.Fatalf("expected %d tool executions before wrap-up, screen shows %d:\n%q", loopBreakConsec, got, out)
	}
	out = r.readUntil(t, "WRAPPED_UP_DONE", waitTimeout)

	mu.Lock()
	gotReqs, warned := reqs, warnSeen
	mu.Unlock()
	if want := loopBreakConsec + 1; gotReqs != want {
		t.Fatalf("mock saw %d requests, want %d (%d tool rounds + wrap-up)", gotReqs, want, loopBreakConsec)
	}
	// The warning rode along in the fed-back results, not on screen.
	if !warned {
		t.Fatal("loop-guard warning never appeared in any request")
	}
}

// Scenario I”: L1 context slimming — once accumulated tool outputs pass
// l1PreserveBytes, the oldest result collapses to a placeholder in later
// requests while everything inside the tail budget stays verbatim
// (architecture §3.9③; one executor stream alone never exceeds ~maxCapture,
// so an oversized single result is covered by unit tests instead).
func TestAgentToolCollapse(t *testing.T) {
	requireLinuxProc(t)
	const (
		cmdA = "head -c 20000 /dev/zero | tr '\\0' 'a'"
		cmdB = "head -c 20000 /dev/zero | tr '\\0' 'b'"
		cmdC = "head -c 20000 /dev/zero | tr '\\0' 'c'"
		cmdD = "head -c 20000 /dev/zero | tr '\\0' 'd'"
	)
	var mu sync.Mutex
	reqs := 0
	q2Body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs++
		n := reqs
		if n >= 3 {
			q2Body = string(body)
		}
		mu.Unlock()
		ans := fmt.Sprintf("TURN%d_DONE", n)
		if n == 1 {
			// Four native bash calls in one round (the old scenario
			// delivered the same four as fenced blocks), each on its own
			// tool_call index — a shared index would fold them into one
			// call at the provider.
			chunks := []string{
				bashCallChunkAt(0, "call_a", cmdA),
				bashCallChunkAt(1, "call_b", cmdB),
				bashCallChunkAt(2, "call_c", cmdC),
				bashCallChunkAt(3, "call_d", cmdD),
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				fl.Flush()
			}
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
			fl.Flush()
			fmt.Fprintf(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"

# This scenario predates the §7 approval gate and pins the ungated path —
# its piped commands would otherwise prompt with nobody to answer.
[agent]
approval = "auto"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("go wide\r"))
	r.readUntil(t, "TURN2_DONE", waitTimeout)
	p.Write([]byte("summarize\r"))
	r.readUntil(t, "TURN3_DONE", waitTimeout)

	mu.Lock()
	body := q2Body
	mu.Unlock()
	if body == "" {
		t.Fatal("second turn request was not captured")
	}
	// Four ~20KB tool results: together they overflow the 64KB L1 tail
	// budget (the executor itself caps one stream near 32KB), so the
	// OLDEST result collapses to a placeholder naming the command, exit
	// code and folded size. body is JSON, so escape the command's
	// backslashes the same way.
	wantFolded := "[已省略] $ " + strings.ReplaceAll(cmdA, `\`, `\\`) +
		" (exit 0)，原始输出 20 KB 已折叠"
	assertContains(t, body, wantFolded)
	// ...while the newer ones survive verbatim...
	assertContains(t, body, strings.Repeat("d", 20000))
	assertContains(t, body, strings.Repeat("b", 20000))
	// ...and no trace of the folded payload remains on the wire.
	if strings.Contains(body, strings.Repeat("a", 4096)) {
		t.Fatalf("collapsed tool output still present in request:\n%.400s…", body)
	}
}

// Scenario J: the agent senses the shell's environment. Every request
// carries a system message with a filtered view of the shell's environment
// (M4 env tracking), so a prompt after shell-mode activity reflects the
// session's env (HOME here, which the test sets).
func TestAIEnvContext(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	var sawEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		// The env block is folded into the single leading system message
		// (merged cwd/env/instructions at the provider boundary).
		if len(req.Messages) > 0 {
			sawEnv = req.Messages[0].Content
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("env understood"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	home := os.Getenv("HOME")

	enterAI(t, p, r)
	p.Write([]byte("what is my home directory\r"))
	r.readUntil(t, "env understood", waitTimeout)

	mu.Lock()
	env := sawEnv
	mu.Unlock()
	if !strings.Contains(env, "\n\nenv:") || !strings.Contains(env, "HOME="+home) {
		t.Fatalf("agent saw env %q, want an env: block containing HOME=%q", env, home)
	}
}

// The [agent] env_allowlist replaces the built-in default exactly: a
// variable on the configured list reaches the model, and one left off it is
// filtered out (M4 env tracking is now configurable instead of hardcoded).
func TestEnvAllowlistConfig(t *testing.T) {
	requireLinuxProc(t)
	t.Setenv("RYSH_TEST_VISIBLE", "hello")
	t.Setenv("RYSH_TEST_SECRET", "s3cret")
	var mu sync.Mutex
	var sawEnv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		// The env block is folded into the single leading system message
		// (merged cwd/env/instructions at the provider boundary).
		if len(req.Messages) > 0 {
			sawEnv = req.Messages[0].Content
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("allowlist ok"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"

[agent]
env_allowlist = ["HOME", "RYSH_TEST_VISIBLE"]
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	enterAI(t, p, r)
	p.Write([]byte("check the env\r"))
	r.readUntil(t, "allowlist ok", waitTimeout)

	mu.Lock()
	env := sawEnv
	mu.Unlock()
	if !strings.Contains(env, "RYSH_TEST_VISIBLE=hello") {
		t.Fatalf("agent env missing allowlisted RYSH_TEST_VISIBLE: %q", env)
	}
	if strings.Contains(env, "RYSH_TEST_SECRET") {
		t.Fatalf("agent env leaked non-allowlisted RYSH_TEST_SECRET: %q", env)
	}
}

// Scenario K: shell commands the user runs in shell mode, and their
// output, reach the AI context (M4 unified event stream, cwd-as-data). A
// command typed before switching to AI mode shows up in the request as a
// system message with the command, its captured output, and the cwd it ran
// in.
func TestShellActivityContext(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	var contents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, m := range req.Messages {
			contents = append(contents, m.Role+": "+m.Content)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("seen the shell activity"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	// Run a command in shell mode, then ask the agent about it.
	p.Write([]byte("echo SHELL_EVENT_42\r"))
	r.readUntil(t, "SHELL_EVENT_42", waitTimeout)

	enterAI(t, p, r)
	p.Write([]byte("what did I just run\r"))
	r.readUntil(t, "seen the shell activity", waitTimeout)

	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(contents, "\n")
	if !strings.Contains(all, "$ echo SHELL_EVENT_42") {
		t.Fatalf("agent context missing the shell command:\n%s", all)
	}
	if !strings.Contains(all, "SHELL_EVENT_42") {
		t.Fatalf("agent context missing the command output:\n%s", all)
	}
	if !strings.Contains(all, "cwd: ") {
		t.Fatalf("agent context missing per-event cwd:\n%s", all)
	}
}

// Scenario K': shell commands and AI turns reach the model as one
// chronological timeline (unified event stream): a command run before the
// first question precedes it, and a command run between questions sits
// between the two turns — each shell event as its own system message —
// instead of all shell activity lumped in front of the conversation.
func TestInterleavedShellAndAITimeline(t *testing.T) {
	requireLinuxProc(t)
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var mu sync.Mutex
	var last []msg
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []msg `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		last = append([]msg(nil), req.Messages...)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("the reply"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	// A shell command before any AI turn.
	p.Write([]byte("echo BEFORE_AI\r"))
	r.readUntil(t, "BEFORE_AI\r\n$ ", waitTimeout)

	// AI turn 1.
	enterAI(t, p, r)
	p.Write([]byte("question one\r"))
	r.readUntil(t, "the reply", waitTimeout)

	// Back to the shell: a command between the AI turns.
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "$ ", waitTimeout)
	p.Write([]byte("echo MID_CMD\r"))
	r.readUntil(t, "MID_CMD\r\n$ ", waitTimeout)

	// AI turn 2: its request must carry the interleaved timeline.
	enterAI(t, p, r)
	p.Write([]byte("question two\r"))
	r.readUntil(t, "the reply", waitTimeout)

	mu.Lock()
	msgs := last
	mu.Unlock()
	// Wire shape: the leading run — cwd, env, instructions, plus the
	// BEFORE_AI event (it predates the first AI turn) — merges into one
	// system message at the provider boundary; the MID_CMD event sits
	// mid-conversation and is demoted to a marked user message there.
	if len(msgs) < 2 || msgs[0].Role != "system" || !strings.HasPrefix(msgs[0].Content, "cwd: ") ||
		!strings.Contains(msgs[0].Content, "\n\nenv:") {
		t.Fatalf("context preamble missing: %+v", msgs)
	}
	body := msgs
	indexOf := func(role, content string) int {
		for i, m := range body {
			if m.Role == role && strings.Contains(m.Content, content) {
				return i
			}
		}
		t.Fatalf("message role=%s containing %q not found in request: %+v", role, content, body)
		return -1
	}
	iBefore := indexOf("system", "$ echo BEFORE_AI")
	iQ1 := indexOf("user", "question one")
	iA1 := indexOf("assistant", "the reply")
	iMid := indexOf("user", "$ echo MID_CMD")
	iQ2 := indexOf("user", "question two")
	if iBefore < 0 || iMid < 0 {
		t.Fatalf("shell events missing from timeline: before=%d mid=%d\n%+v", iBefore, iMid, body)
	}
	if !(iBefore < iQ1 && iQ1 < iA1 && iA1 < iMid && iMid < iQ2) {
		t.Fatalf("timeline not chronological: before=%d q1=%d a1=%d mid=%d q2=%d\n%+v",
			iBefore, iQ1, iA1, iMid, iQ2, body)
	}
	if iQ2 != len(body)-1 {
		t.Fatalf("current prompt should end the request: q2=%d, len=%d", iQ2, len(body))
	}
}

// Scenario K”: while a fullscreen program (alt screen) owns the terminal the
// recorder pauses: its keystrokes (incl. Enter) are not recorded as shell
// commands, its screen content is not captured, and its exit repaint is not
// attributed to the next command. Commands run before and after the program
// are still recorded with clean output.
func TestFullscreenRecorder(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	var contents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, m := range req.Messages {
			contents = append(contents, m.Role+": "+m.Content)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("seen the shell activity"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	// A fake vim: enter the alt screen, consume two lines of "editing"
	// input, repaint, leave the alt screen. Each write merges its alt-screen
	// sequence with trailing output in a single pty read (no pauses), as
	// with a real fullscreen app — most critically, real output follows the
	// exit sequence in the same read, so the detector must find the
	// sequences mid-chunk, not at a chunk's tail.
	fakevim := filepath.Join(t.TempDir(), "fakevim")
	if err := os.WriteFile(fakevim, []byte(`#!/bin/sh
printf '\033[?1049hENTER_MERGED\n'
read -r _
printf 'VI_SCREEN_B\n'
read -r _
printf 'VI_EXIT_REDRAW\033[?1049lEXIT_MERGED\n'
`), 0o755); err != nil {
		t.Fatal(err)
	}

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)

	// A real command before the fullscreen program.
	p.Write([]byte("echo BEFORE_VIM\r"))
	r.readUntil(t, "BEFORE_VIM\r\n$ ", waitTimeout)

	// Launch the fake vim and wait for it to own the terminal. The enter
	// sequence is mid-chunk (the first repaint follows it in the same
	// read): rysh detects the alternate screen, pauses the recorder, and
	// forwards the program's own screen bytes untouched.
	p.Write([]byte(shellPath(fakevim) + "\r"))
	r.readUntil(t, "ENTER_MERGED", waitTimeout)

	// "Edit" inside it: keystrokes and Enter must not be recorded.
	p.Write([]byte("i\r"))
	r.readUntil(t, "VI_SCREEN_B", waitTimeout)
	p.Write([]byte("wq\r"))

	// It exits: the exit sequence and the program's final repaint share
	// one chunk (a real shell prompt would follow it too). rysh resumes
	// recording once the alternate screen is left.
	exit := r.readUntil(t, "EXIT_MERGED", waitTimeout)
	assertContains(t, exit, "\x1b[?1049l")

	// Wait for the shell to redraw its prompt: the shell repaints only
	// after it has reaped the program and taken back the foreground
	// process group — the state rysh's per-keystroke foreground check
	// (shellInForeground) needs before the next command's first
	// keystroke is recorded.
	r.readUntil(t, "$ ", waitTimeout)

	// A real command after it.
	p.Write([]byte("echo AFTER_VIM\r"))
	r.readUntil(t, "AFTER_VIM\r\n$ ", waitTimeout)

	// Ask the agent; inspect what reached the model.
	enterAI(t, p, r)
	p.Write([]byte("what did I run\r"))
	r.readUntil(t, "seen the shell activity", waitTimeout)

	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(contents, "\n")
	for _, want := range []string{"$ echo BEFORE_VIM", "$ echo AFTER_VIM", "BEFORE_VIM", "AFTER_VIM"} {
		if !strings.Contains(all, want) {
			t.Fatalf("agent context missing %q:\n%s", want, all)
		}
	}
	for _, junk := range []string{"ENTER_MERGED", "VI_SCREEN_B", "VI_EXIT_REDRAW", "EXIT_MERGED"} {
		if strings.Contains(all, junk) {
			t.Fatalf("fullscreen content leaked into the context (%q):\n%s", junk, all)
		}
	}
	if strings.Contains(all, "$ i\n") || strings.Contains(all, "$ wq\n") {
		t.Fatalf("vim keystrokes recorded as shell commands:\n%s", all)
	}
}

// Strictest OpenAI-compatible endpoints (sglang: "System message must be
// at the beginning") reject any request in which a system message follows
// the leading system run. rysh's timeline keeps such systems internally —
// a shell event where it happened, a tool result from a prior turn — so
// the outbound request must be wire-compliant; the pre-fix shape 400s on
// the first AI message in any session with shell activity or a
// tool-using turn in its past.
func TestAIRequestSystemPlacementOnStrictEndpoint(t *testing.T) {
	var mu sync.Mutex
	var violations int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// sglang's actual rule (measured): exactly one system message,
		// only at index 0 — a run of leading systems is rejected just
		// the same.
		bad := false
		for i, m := range req.Messages {
			if m.Role == "system" && i != 0 {
				bad = true
				break
			}
		}
		if bad {
			mu.Lock()
			violations++
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"object":"error","message":"System message must be at the beginning.","type":"BadRequest","param":null,"code":400}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk("STRICT_OK_REPLY"))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "strict/qwen"

[providers.strict]
base_url = %q
api = "openai-completions"

[[providers.strict.models]]
id = "qwen"
`, srv.URL)

	// The attached session already carries a turn that used a tool: the
	// reconstructed history puts a system (tool) message after the user
	// and assistant messages of that turn.
	home := t.TempDir()
	id := "sjstrict1"
	log, err := session.OpenLog(filepath.Join(home, ".rysh", "sessions"), id)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	for _, w := range [][2]string{{"usr", "OLDQ"}, {"asw", "OLDA"}, {"tool", "[bash] exit 0"}} {
		if err := log.Write(w[0], w[1]); err != nil {
			t.Fatalf("log.Write(%q): %v", w[0], err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}

	p, _, r := startRyshIn(t, home, sessionProfile, cfg, []string{"RYSH_SESSION_ID=" + id})
	r.readUntil(t, "$ ", startupTimeout)

	// A live shell command after the attached history: its event lands in
	// the timeline after the prior AI turn — a second mid-array system on
	// the pre-fix wire shape.
	p.Write([]byte("echo STRICT7\r"))
	r.readUntil(t, "STRICT7\r\n$ ", waitTimeout)

	ai := enterAI(t, p, r)
	p.Write([]byte("hello\r"))
	win := ai + r.readUntil(t, "STRICT_OK_REPLY", waitTimeout)
	if strings.Contains(win, "System message must be at the beginning") {
		t.Fatalf("strict endpoint rejected the request: %q", win)
	}
	mu.Lock()
	v := violations
	mu.Unlock()
	if v != 0 {
		t.Fatalf("%d request(s) carried a system message after the leading run", v)
	}
}

// Scenario K”': a foreground child that never enters the alternate screen
// (the shape of ssh, less, or a plain interactive program) still blocks
// mode switching: the shell is not in the foreground, so both the mode
// switch key and the fresh-prompt space gesture are refused while the
// child runs, and switching works again once it exits. The gate is the
// live foreground-process-group query, not alternate-screen detection.
func TestForegroundBlocksModeSwitch(t *testing.T) {
	// The gate under test is a live TIOCGPGRP query on the pty; Windows
	// consoles have no foreground process group at all, so
	// shellInForeground always answers "the shell owns the terminal" and
	// only alternate-screen programs are held back (see fg_windows.go and
	// TestFullscreenRecorder).
	if runtime.GOOS == "windows" {
		t.Skip("foreground process groups are Unix-only; ConPTY has no equivalent query")
	}
	// A foreground program that never touches the alternate screen: it
	// waits for one line of input, then prints and exits.
	fakeprog := filepath.Join(t.TempDir(), "fakeprog")
	if err := os.WriteFile(fakeprog, []byte(`#!/bin/sh
echo FAKEPROG_RUNNING
read -r _
echo FAKEPROG_DONE
`), 0o755); err != nil {
		t.Fatal(err)
	}

	p, _, r := startRyshWithConfig(t, "")
	r.readUntil(t, "$ ", startupTimeout)

	// Shift+Tab while a plain foreground child runs must not switch:
	// the child owns the foreground even though no alternate screen
	// was ever entered.
	p.Write([]byte(shellPath(fakeprog) + "\r"))
	r.readUntil(t, "FAKEPROG_RUNNING", waitTimeout)

	p.Write(defaultSwitchSeq)
	locked := r.readUntil(t, "无法切换模式", waitTimeout)
	if strings.Contains(locked, "\x1b[35m[AI]:") {
		t.Fatalf("mode switched to AI while a foreground program runs: %q", locked)
	}

	// The fresh-prompt space gesture is refused the same way.
	p.Write([]byte(" "))
	locked = r.readUntil(t, "无法切换模式", waitTimeout)
	if strings.Contains(locked, "\x1b[35m[AI]:") {
		t.Fatalf("space switched to AI while a foreground program runs: %q", locked)
	}

	// When the child exits the shell is back in the foreground and
	// switching works again.
	p.Write([]byte("\r"))
	r.readUntil(t, "FAKEPROG_DONE", waitTimeout)
	r.readUntil(t, "$ ", waitTimeout)
	enterAI(t, p, r)
}

// Scenario L: while a reply is streaming, mode switching is locked. The
// mode-switch key (Shift+Tab) prints an inline "cannot switch modes" notice
// in the stream instead of returning to shell mode; only ^C aborts the task,
// after which the AI view is usable again.
func TestAILockDuringStream(t *testing.T) {
	var mu sync.Mutex
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		reqs++
		n := reqs
		mu.Unlock()
		prefix := "tok"
		if n == 2 {
			prefix = "sec"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < 10; i++ {
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(fmt.Sprintf("%s%d ", prefix, i)))
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Start a slow stream and wait for its first token to render.
	p.Write([]byte("lock me\r"))
	r.readUntil(t, "tok0", waitTimeout)

	// Shift-Tab while the task streams is locked: rysh stays in AI mode and
	// prints the lock notice inline instead of switching to shell mode.
	p.Write(defaultSwitchSeq)
	locked := r.readUntil(t, "无法切换模式", waitTimeout)
	if strings.Contains(locked, "$ ") {
		t.Fatalf("mode switched to shell while a task streamed: %q", locked)
	}

	// ^C aborts the stream; the view is usable again and a follow-up prompt
	// starts a fresh turn.
	p.Write([]byte("\x03"))
	p.Write([]byte("still ai\r"))
	again := r.readUntil(t, "sec0", waitTimeout)
	assertContains(t, again, "\x1b[35m[AI]:")
	assertContains(t, again, "still ai")
	assertContains(t, again, "sec0")
}

// Scenario M: reasoning deltas (extended thinking) render dim into the
// stream before the visible reply, taking over the reply line where the
// inline pre-output spinner was waiting for the first token.
func TestAIReasoningStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"let me think\"}}]}\n\n")
		fl.Flush()
		// Keep the task busy long enough for the inline spinner to tick
		// before the visible reply replaces it.
		time.Sleep(500 * time.Millisecond)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"final answer\"}}]}\n\n")
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("reason with me\r"))
	out := r.readUntil(t, "final answer", waitTimeout)
	assertContains(t, out, "let me think") // reasoning text in the stream
	// Reasoning is drawn dim+italic via the theme: on the harness's fixed
	// 256-color terminal the muted foreground quantizes to 38;5;145, then
	// italic (3m).
	assertContains(t, out, "\x1b[38;5;145m\x1b[3m") // reasoning drawn dim+italic
}

// Scenario N: the session stream is appended to the JSONL session log, and
// shell output produced while AI mode is active is deferred (logged, not
// shown) and flushed to the screen on return to shell mode. A background job
// started in shell mode fires after the switch to AI, so its output lands in
// the deferral path and must reappear when AI mode is left.
func TestSessionLogAndDeferredShellFlush(t *testing.T) {
	const sid = "itest-flush"
	p, c, r := startRyshWithConfig(t, "", "RYSH_SESSION_ID="+sid)
	r.readUntil(t, "$ ", startupTimeout)

	// Start a background job that produces output ~1s later, then switch
	// to AI mode before it fires.
	p.Write([]byte("(sleep 1; echo DEFERRED_OUT) &\r"))
	r.readUntil(t, "$ ", waitTimeout)
	enterAI(t, p, r)
	time.Sleep(1500 * time.Millisecond)

	// Return to shell mode: the deferred output is flushed to the screen
	// and the shell prompt is redrawn. They are not adjacent bytes: bash
	// reports the finished background job ("[1]+ Done …") between the
	// flushed line and its next prompt, and ConPTY repaints the line end
	// with an erase, so the contract is "the deferred output reached the
	// screen, and the prompt came back", not a fixed byte sequence.
	p.Write(defaultSwitchSeq)
	sh := r.readUntil(t, "DEFERRED_OUT", waitTimeout)
	sh += r.readQuiet(t, settle, waitTimeout)
	assertContains(t, sh, "DEFERRED_OUT")
	assertContains(t, sh, "$ ")

	// Exit and read the session log: the deferred output was logged while
	// in AI mode, and the mode transitions are recorded.
	p.Write([]byte("exit\r"))
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("rysh did not exit after the shell exited")
	}

	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".rysh", "sessions", sid, "messages.log"))
	if err != nil {
		t.Fatalf("read session log: %v", err)
	}
	log := string(data)
	for _, want := range []string{"session:start", "mode:ai", "mode:shell", "DEFERRED_OUT"} {
		if !strings.Contains(log, want) {
			t.Fatalf("session log missing %q:\n%s", want, log)
		}
	}
}

// TestQuitInAI tests that /quit works in AI mode.
func TestQuitInAI(t *testing.T) {
	p, c, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Enter AI mode with the mode-switch key (Shift+Tab).
	p.Write(defaultSwitchSeq)
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)

	// Try /quit in AI mode.
	p.Write([]byte("/quit\r"))
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rysh exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("rysh did not exit within 3s of /quit in AI mode")
	}
}

// /quit and /exit are part of the / command surface, not a hidden
// interception: /help lists them and Tab completes them, so the working
// quit path stays discoverable (it used to be intercept-only, with no
// mention in /help or the completion list).
func TestQuitCommandIsListedAndCompletes(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// /help lists the quit commands.
	p.Write([]byte("/help\r"))
	out := r.readUntil(t, "先收尾在途任务", waitTimeout)
	assertContains(t, out, "/quit（或 /exit）")

	// Tab: /q completes /quit. (The help's own /quit line is already
	// consumed, so the next /q in the stream is the typed draft.)
	p.Write([]byte("/q"))
	r.readUntil(t, "/q", waitTimeout)
	p.Write([]byte("\t"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /quit") {
		t.Fatalf("Tab left %q on the line, want it to end with %q", got, "[AI]: /quit")
	}
}

// Nested interactive rysh is refused: the parent marks its child shell's
// environment (RYSH_INSIDE), so a bare `rysh` started from the inner shell
// prints the refusal notice and exits 1 before creating a pty, so the outer
// screen is never touched and `rysh -v` still works. The outer session stays
// usable afterwards.
func TestNestedRyshRefused(t *testing.T) {
	abs, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RYSH_SELF", abs)
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)

	// Bare nested start: refused with the notice, exit 1, screen untouched.
	p.Write([]byte("\"$RYSH_SELF\"; echo NEST_RC=$?\r"))
	out := r.readUntil(t, "NEST_RC=1", waitTimeout)
	assertContains(t, out, "已经在 rysh 中")
	assertContains(t, out, "NEST_RC=1")
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("nested rysh entered the alternate screen: %q", out)
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("nested rysh cleared the screen: %q", out)
	}

	// `rysh ai` without a message is refused the same way, with the
	// one-shot usage hint.
	p.Write([]byte("\"$RYSH_SELF\" ai; echo AI_RC=$?\r"))
	out = r.readUntil(t, "AI_RC=1", waitTimeout)
	assertContains(t, out, "已经在 rysh 中")
	assertContains(t, out, "rysh ai \"消息\"")
	assertContains(t, out, "AI_RC=1")

	// An unknown subcommand is reported and exits 1.
	p.Write([]byte("\"$RYSH_SELF\" frobnicate; echo UNK_RC=$?\r"))
	out = r.readUntil(t, "UNK_RC=1", waitTimeout)
	assertContains(t, out, "未知的 rysh 子命令: frobnicate")
	assertContains(t, out, "UNK_RC=1")

	// Flag-only forms still work inside rysh.
	p.Write([]byte("\"$RYSH_SELF\" -v; echo V_RC=$?\r"))
	out = r.readUntil(t, "V_RC=0", waitTimeout)
	assertContains(t, out, "rysh dev")
	assertContains(t, out, "V_RC=0")

	// The outer session is unaffected and still executes commands.
	p.Write([]byte("echo AFTER_NEST\r"))
	done := r.readUntil(t, "AFTER_NEST\r\n$ ", waitTimeout)
	assertContains(t, done, "AFTER_NEST")
}

// Tab in AI mode completes the first word of a / command: /m + Tab inserts
// the only match, /model, and submitting runs it (the model list prints).
func TestTabCompletesSlashCommand(t *testing.T) {
	p, _, r := startRyshWithConfig(t, twoModelConfig)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("/m"))
	r.readUntil(t, "/m", waitTimeout)
	p.Write([]byte("\t"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /model") {
		t.Fatalf("Tab left %q on the line, want it to end with %q", got, "[AI]: /model")
	}

	// Submitting runs the completed command: the numbered model list
	// prints (a failed /m would have printed "unknown command" instead).
	p.Write([]byte("\r"))
	out := r.readUntil(t, "2. ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "> 1. ollama/llama3.1:8b")
}

// Tab on /model with the cursor at the first-argument position (after the
// space) prints the numbered model list so the user can type the number;
// a repeat Tab on the same draft does not reprint it. A bare /model + Tab
// is a plain completion (single candidate: nothing changes, no list).
func TestTabShowsModelList(t *testing.T) {
	p, _, r := startRyshWithConfig(t, twoModelConfig)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Bare /model + Tab: plain completion, no list, no insertion.
	p.Write([]byte("/model"))
	r.readUntil(t, "/model", waitTimeout)
	p.Write([]byte("\t"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); strings.Contains(got, "可用模型") {
		t.Fatalf("bare /model Tab printed the model list: %q", got)
	}

	// Space + Tab (first-argument position): the numbered list prints.
	p.Write([]byte(" \t"))
	out := r.readUntil(t, "2. ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "可用模型")
	assertContains(t, out, "> 1. ollama/llama3.1:8b")
	assertContains(t, out, "2. ollama/qwen2.5-coder:7b")

	// A repeat Tab on the same draft is a no-op: the list is not reprinted.
	p.Write([]byte("\t"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); strings.Contains(got, "可用模型") {
		t.Fatalf("repeat Tab reprinted the model list: %q", got)
	}

	// The number then picks the model.
	p.Write([]byte("2\r"))
	out = r.readUntil(t, "model: ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "model: ollama/qwen2.5-coder:7b")
}

// Tab on /model reprints the list when the config file changed on disk
// since the last print (hot reload): the repeat-Tab no-op must not hold
// back a model the user just added to the file, while a third Tab with no
// new edit stays a no-op.
func TestTabModelListReprintsOnConfigEdit(t *testing.T) {
	const initial = `
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
`
	const edited = initial + `
[[providers.ollama.models]]
id = "qwen2.5-coder:7b"
`
	p, _, r := startRyshWithConfig(t, initial)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Space + Tab: the numbered list prints (one model).
	p.Write([]byte("/model \t"))
	out := r.readUntil(t, "1. ollama/llama3.1:8b", waitTimeout)
	assertContains(t, out, "可用模型")

	// Add a model to the config file while rysh keeps running.
	rewriteConfig(t, edited)

	// The same draft again: the on-disk edit invalidates the repeat no-op
	// and the list reprints with the new model.
	p.Write([]byte("\t"))
	out = r.readUntil(t, "2. ollama/qwen2.5-coder:7b", waitTimeout)
	assertContains(t, out, "可用模型")

	// A further Tab with no new edit is a no-op again.
	p.Write([]byte("\t"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); strings.Contains(got, "可用模型") {
		t.Fatalf("third Tab reprinted the model list: %q", got)
	}
}

// Tab on /resume with the cursor at the argument position prints the
// session list (the /ls view, first page) so the number or id to switch
// to can be picked.
func TestTabShowsSessionList(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Space, then Tab at the argument position: the session list prints,
	// the current session marked with a leading >.
	p.Write([]byte("/resume \t"))
	out := r.readUntil(t, ">  1 ", waitTimeout)
	assertContains(t, out, "会话列表（第 1/1 页 · 共 1 个）")
}

// Up/Down (and Ctrl-P/Ctrl-N) cycle the submitted user inputs at the AI
// prompt: Up starts at the newest input and walks back, Down walks forward
// and past the newest input restores the live draft.
func TestUpDownRecallsUserInputs(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Submit two slash commands (no model needed) so recall has inputs.
	p.Write([]byte("/help\r"))
	out := r.readUntil(t, "先收尾在途任务", waitTimeout)
	assertContains(t, out, "/quit（或 /exit）")
	p.Write([]byte("/ls\r"))
	out = r.readUntil(t, "会话列表（第", waitTimeout)

	// ↑ recalls the newest user input.
	p.Write([]byte("\x1b[A"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /ls") {
		t.Fatalf("after ↑ the line shows %q, want the newest input recalled", got)
	}
	// ↑ again recalls the one before.
	p.Write([]byte("\x1b[A"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /help") {
		t.Fatalf("after second ↑ the line shows %q, want the earlier input", got)
	}
	// ↓ walks forward again...
	p.Write([]byte("\x1b[B"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /ls") {
		t.Fatalf("after ↓ the line shows %q, want /ls", got)
	}
	// ...and past the newest restores the live draft (empty here).
	p.Write([]byte("\x1b[B"))
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: ") {
		t.Fatalf("past the newest the line shows %q, want the live draft restored", got)
	}
	// Ctrl-P / Ctrl-N do the same.
	p.Write([]byte{0x10})
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: /ls") {
		t.Fatalf("after ^P the line shows %q, want the newest input", got)
	}
	p.Write([]byte{0x0e})
	if got := visible(r.readQuiet(t, settle, waitTimeout)); !strings.HasSuffix(got, "[AI]: ") {
		t.Fatalf("after ^N the line shows %q, want the live draft restored", got)
	}
}

// /history prints the session's user inputs (usr records) as a numbered
// list, newest last, paginated; -v adds the model's reply under each input
// (（无输出） when the input produced no asw records, like slash commands).
func TestHistorySlash(t *testing.T) {
	p, _, r := startRysh(t)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	// Two submitted inputs so the history has rows.
	p.Write([]byte("/help\r"))
	r.readUntil(t, "先收尾在途任务", waitTimeout)
	p.Write([]byte("/ls\r"))
	r.readUntil(t, "会话列表（第", waitTimeout)

	// Plain: the two earlier inputs listed, numbered oldest first, no reply
	// lines. /history renders before it logs itself (submitAI runs
	// runCommand, then logWrite usr), so it does not yet appear — 共 2 条.
	// Wait for the block's last row so the whole block is in the window.
	p.Write([]byte("/history\r"))
	out := r.readUntil(t, "2. /ls", waitTimeout)
	assertContains(t, out, "用户输入历史（第 1/1 页 · 共 2 条）")
	assertContains(t, out, "1. /help")
	if strings.Contains(out, "↳") {
		t.Fatalf("plain /history must not show replies: %q", out)
	}

	// -v: by now the first /history is logged too, so there are 3 rows, each
	// with a reply line under it (slash commands produce no asw, so all are
	// （无输出）). Wait for the last row, then drain the trailing reply line.
	p.Write([]byte("/history -v\r"))
	out = r.readUntil(t, "3. /history", waitTimeout)
	out += r.readQuiet(t, settle, waitTimeout)
	assertContains(t, out, "用户输入历史（第 1/1 页 · 共 3 条）")
	assertContains(t, out, "1. /help")
	assertContains(t, out, "2. /ls")
	if n := strings.Count(out, "（无输出）"); n != 3 {
		t.Fatalf("/history -v showed %d （无输出） replies, want 3: %q", n, out)
	}
}

// [tui] locale = "en" switches the user-visible UI text to English even
// though the test environment pins LANG to C (the explicit config wins over
// the environment).
func TestLocaleEnglishUI(t *testing.T) {
	_, _, r := startRyshWithConfig(t, "[tui]\nlocale = \"en\"\n")
	startup := r.readUntil(t, "$ ", startupTimeout)
	if !strings.Contains(startup, "enters AI mode") {
		t.Fatalf("English locale: startup banner missing the English tail: %q", startup)
	}
}
