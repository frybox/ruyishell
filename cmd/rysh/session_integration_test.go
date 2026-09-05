// Integration tests for v6 multi-session: the shell-side rysh subcommands
// (ls/new/resume/ai/kill) running inside a live rysh, the control-channel
// switch, the AI-mode slash commands, per-session log isolation,
// attachment refusal, and the top-level session forms.
// plan §9.1.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/charmbracelet/x/ansi"

	"ruyishell/internal/session"
)

// sessionProfile is the login profile used by these tests: one ready marker
// per shell start (so a session switch — which restarts the login shell on
// the same pty — replays it) and a plain prompt.
const sessionProfile = "echo RYSH_READY\nPS1='$ '\n"

// setSelf exports RYSH_SELF so tests can run rysh subcommands from the
// child shell exactly as a user would type them.
func setSelf(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RYSH_SELF", abs)
}

// startRyshIn launches a rysh inside an outer pty with an explicit home
// directory (so two instances can share one session store) and explicit
// extra env vars (e.g. RYSH_SESSION_ID) and argv (e.g. the top-level
// `rysh new` form). extra entries are appended after the sanitized
// environment; os/exec keeps the last occurrence of a duplicate key.
func startRyshIn(t *testing.T, home, profile, cfg string, extra []string, args ...string) (pty.Pty, *pty.Cmd, *ptyReader) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("TERM", "xterm-256color")
	writeStartupProfiles(t, home, profile)
	// Pin the prompt marker off like the other harness: the dedicated
	// marker tests opt in, and a live marker would change every prompt.
	cfg = withMarkerOff(cfg)
	// Point rysh at a POSIX login shell on Windows (mirrors startRyshEnv):
	// without this rysh defaults to PowerShell, which has no "$ " prompt and
	// does not source .profile, so the session/AI scenarios below never see
	// the expected shell prompt.
	if !shellConfigured(cfg) {
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
	// Force rysh to read this test's config. On Windows rysh resolves its
	// config via USERPROFILE, not HOME, so HOME=home alone does not redirect
	// it (unlike on Linux); without RYSH_CONFIG the test config — and its
	// shell override — is ignored and rysh falls back to PowerShell.
	extra = append(extra, "RYSH_CONFIG="+configPath)
	// On Windows os.UserHomeDir() returns USERPROFILE (not HOME), so the
	// session store (~/.rysh) would land in the real user profile and the
	// ledger/ctl/sessions the tests read from home would not exist. Redirect
	// USERPROFILE to the test home so the store is isolated as on Linux.
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
	c := p.Command(os.Args[0], args...)
	c.Env = testChildEnv(append([]string{"HOME=" + home}, extra...)...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	})
	if up, ok := p.(pty.UnixPty); ok {
		_ = up.Slave().Close()
	}
	return p, c, newPtyReader(p)
}

// waitForAll waits until every marker has appeared anywhere in the
// accumulated output, in any order, draining the pty. It does not consume
// the reader window, so later readUntil calls still see everything since
// their previous consumption. Markers must be unique in the stream; for a
// repeated marker (e.g. the per-shell-start RYSH_READY) use waitForCount.
func waitForAll(t *testing.T, r *ptyReader, timeout time.Duration, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		all := string(r.all)
		missing := false
		for _, w := range wants {
			if !strings.Contains(all, w) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in accumulated output; tail: %q", wants, tailBytes(r.all, 2000))
		}
		drainOne(t, r, deadline)
	}
}

// waitForCount waits until want has appeared at least n times in the
// accumulated output (e.g. the login profile's RYSH_READY, replayed by
// every shell start after a session switch).
func waitForCount(t *testing.T, r *ptyReader, timeout time.Duration, want string, n int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Count(string(r.all), want) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q ×%d in accumulated output; tail: %q", want, n, tailBytes(r.all, 2000))
		}
		drainOne(t, r, deadline)
	}
}

func drainOne(t *testing.T, r *ptyReader, deadline time.Time) {
	t.Helper()
	select {
	case b, ok := <-r.ch:
		if !ok {
			t.Fatalf("pty closed while waiting; got %q", tailBytes(r.all, 2000))
		}
		r.all = append(r.all, b)
	case <-time.After(time.Until(deadline)):
	}
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		return string(b[len(b)-n:])
	}
	return string(b)
}

// ledgerLines reads ~/.rysh/created or ~/.rysh/updated as whitespace-split
// fields: created lines are <ts> <num> <id>, updated lines are <ts> <id>.
func ledgerLines(t *testing.T, home, name string) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".rysh", name))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		out = append(out, strings.Fields(ln))
	}
	return out
}

// lastCreatedID returns the session id of the most recently created session.
func lastCreatedID(t *testing.T, home string) string {
	t.Helper()
	lines := ledgerLines(t, home, "created")
	if len(lines) == 0 {
		t.Fatal("created ledger is empty")
	}
	return lines[len(lines)-1][2]
}

// sessRecords returns the ctl attachment records as pid → session id,
// without liveness filtering.
func sessRecords(t *testing.T, home string) map[string]string {
	t.Helper()
	dir := filepath.Join(home, ".rysh", "ctl")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sess") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(e.Name(), ".sess")] = strings.TrimSpace(string(data))
	}
	return out
}

// readSessionLog returns the raw JSONL content of a session's messages.log.
func readSessionLog(t *testing.T, home, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".rysh", "sessions", id, "messages.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newMockChatServer starts a minimal OpenAI-compatible chat-completions
// endpoint that streams reply as a single SSE delta, and returns the
// config.toml content pointing at it.
func newMockChatServer(t *testing.T, reply string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(reply))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
`, srv.URL)
}

// waitProcess waits for the pty child to exit and returns its exit code.
func waitProcess(t *testing.T, c *pty.Cmd, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %s", timeout)
	}
	return c.ProcessState.ExitCode()
}

// waitForNewSessionID polls the created ledger until a session other than
// prev exists and returns its id. `rysh new` / `rysh resume` create or resolve
// the session before handing the switch to the instance, but the landing
// switch kills the login shell — possibly before it echoes the
// subprocess's `RC=…` marker — so switch tests must never wait on that
// echo and rely on the ledger plus the landing markers instead.
func waitForNewSessionID(t *testing.T, home, prev string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if id := lastCreatedID(t, home); id != prev {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session besides %s appeared within %s", prev, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoAltScreen fails if the accumulated output ever entered the
// alternate screen — on the main screen rysh never takes over the terminal,
// so one-shot subcommands and session switches must not emit ?1049h.
func assertNoAltScreen(t *testing.T, r *ptyReader) {
	t.Helper()
	if n := strings.Count(string(r.all), "\x1b[?1049h"); n != 0 {
		t.Fatalf("alternate screen entered %d times, want 0 (main screen only)", n)
	}
}

// lsRow returns the rysh ls list row carrying id, stripped of ANSI (the
// list prints dim, so the row's leading SGR would otherwise defeat the
// callers' ">" prefix checks). Only the lines after the 会话列表 header
// count, so switch notices and replays naming the same id are not mistaken
// for the row; "" means the captured output has no such row.
func lsRow(out, id string) string {
	i := strings.Index(out, "会话列表")
	if i < 0 {
		return ""
	}
	for _, l := range strings.Split(out[i:], "\n") {
		if strings.Contains(l, id) {
			return ansi.Strip(l)
		}
	}
	return ""
}

// `rysh ls` inside a live rysh lists the session store without starting a
// second rysh process: the current session is marked active (the store's
// ledger has exactly the startup session), its creation time and the
// occupancy column are shown, and no alt-screen takeover happens.
func TestShellRyshLs(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	p, _, r := startRyshIn(t, home, sessionProfile, "", nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)
	created, err := strconv.ParseInt(ledgerLines(t, home, "created")[0][0], 10, 64)
	if err != nil {
		t.Fatalf("created ledger ts: %v", err)
	}

	p.Write([]byte("\"$RYSH_SELF\" ls; echo LS_RC=$?\r"))
	out := r.readUntil(t, "LS_RC=0", waitTimeout)
	assertContains(t, out, "会话列表（第 1/1 页 · 共 1 个）")
	assertContains(t, out, "创建时间")
	assertContains(t, out, "最后更新")
	assertContains(t, out, "占用")
	row := lsRow(out, id1)
	if row == "" {
		t.Fatalf("rysh ls has no row for the current session %s: %q", id1, out)
	}
	if !strings.HasPrefix(row, ">") {
		t.Fatalf("rysh ls row for the active session %s not marked: %q", id1, row)
	}
	assertContains(t, row, "新会话")
	assertContains(t, row, "本实例")
	assertContains(t, row, time.UnixMilli(created).Format("01-02 15:04"))
	assertContains(t, out, "LS_RC=0")
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("rysh ls entered the alternate screen: %q", out)
	}
	if strings.Contains(out, "[rysh ") {
		t.Fatalf("rysh ls marked a session as attached: %q", out)
	}
	if recs := sessRecords(t, home); len(recs) != 1 {
		t.Fatalf("attachment records = %v, want exactly the instance's own", recs)
	}
}

// `rysh new` inside a live rysh creates a session and switches the current
// instance to it via the control channel: the login shell restarts on the
// same pty (profile replays), the unified replay and landing notice appear,
// the new session, the ledgers record both sessions, and the instance's
// attachment record now names the new one.
func TestShellRyshNewSwitches(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	p, rc, r := startRyshIn(t, home, sessionProfile, "", nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)
	// The shell generation the instance starts with, identified by pid. A
	// session switch replaces it; nothing of it may stay attached to the pty.
	shellsBefore := shellDescendants(t, rc.Process.Pid)

	p.Write([]byte("\"$RYSH_SELF\" new; echo NEW_RC=$?\r"))
	id2 := waitForNewSessionID(t, home, id1, 5*time.Second)

	// The switch lands: a second shell start replays the profile and the
	// landing notice names the new session.
	waitForCount(t, r, startupTimeout, "RYSH_READY", 2)
	waitForAll(t, r, waitTimeout, "已新建会话: "+id2)
	// The generation started for the session we left has to be gone, not just
	// signalled: the login shell is a launcher stub with the real shell behind
	// it, and an orphan keeps reading the terminal, answering as the session
	// it was started for.
	if survivors := waitForShellsGone(t, shellsBefore, 3*time.Second); len(survivors) > 0 {
		t.Fatalf("shells of the session we left are still attached after new: %v (%s)",
			survivors, formatProcs(procTree(rc.Process.Pid)))
	}

	// The replacement shell carries the new session's identity in its
	// environment, so rysh subprocesses it launches act on that session.
	p.Write([]byte("echo SIDVAL=$RYSH_SESSION_ID\r"))
	p.Write([]byte("echo DUMP_END\r"))
	sidDump := r.readUntil(t, "DUMP_END", waitTimeout)
	assertContains(t, sidDump, "SIDVAL="+id2)

	lines := ledgerLines(t, home, "created")
	if len(lines) != 2 || lines[1][1] != "2" || lines[1][2] != id2 {
		t.Fatalf("created ledger = %v, want 2 rows ending in num 2 id %s", lines, id2)
	}
	if upd := ledgerLines(t, home, "updated"); len(upd) == 0 || upd[len(upd)-1][1] != id2 {
		t.Fatalf("updated ledger = %v, want last row naming %s", upd, id2)
	}
	recs := sessRecords(t, home)
	if len(recs) != 1 {
		t.Fatalf("attachment records = %v, want exactly one", recs)
	}
	for _, id := range recs {
		if id != id2 {
			t.Fatalf("instance still attached to %s, want %s", id, id2)
		}
	}

	// The list now shows the new session as active.
	p.Write([]byte("\"$RYSH_SELF\" ls; echo LS2_RC=$?\r"))
	out := r.readUntil(t, "LS2_RC=0", waitTimeout)
	assertContains(t, out, "共 2 个")
	if row := lsRow(out, id2); !strings.HasPrefix(row, ">") {
		t.Fatalf("rysh ls does not mark the new session %s active: %q", id2, row)
	}
	if row := lsRow(out, id1); strings.HasPrefix(row, ">") {
		t.Fatalf("rysh ls still marks session %s as active: %q", id1, row)
	}
	assertNoAltScreen(t, r)
}

// A shell generation SIGKILLed at its prompt leaves the pty in whatever
// state its line editor set: canonical mode off, echo off. The
// replacement generation must still show typed input, or shell mode is
// blind after a session switch. The first generation emulates a line
// editor (raw, self-echoing, like bash/zsh at the prompt) and consumes a
// marker file; the second generation touches no termios and relies on the
// tty echo, like plain /bin/sh does.
func TestSessionSwitchRestoresShellInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pty termios state on Windows")
	}
	if _, ok := findProgram("stty"); !ok {
		t.Skip("stty not installed")
	}
	setSelf(t)
	home := t.TempDir()
	script := filepath.Join(home, "editor_shell.sh")
	const src = `#!/bin/sh
if [ -f "$HOME/EDITOR_MARK" ]; then
  rm -f "$HOME/EDITOR_MARK"
  stty raw
  stty -echo
  stty icrnl
  EDIT=1
fi
echo RYSH_READY
while :; do
  printf '$ '
  IFS= read -r cmd || exit 0
  [ -n "$EDIT" ] && printf '%s\n' "$cmd"
  case "$cmd" in exit|logout) exit 0;; esac
  eval "$cmd"
done
`
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "EDITOR_MARK"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("shell = %q", script)
	p, _, r := startRyshIn(t, home, "", cfg, nil)
	r.readUntil(t, "RYSH_READY", startupTimeout)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	p.Write([]byte("\"$RYSH_SELF\" new; echo NEW_RC=$?\r"))
	id2 := waitForNewSessionID(t, home, id1, 5*time.Second)
	waitForCount(t, r, startupTimeout, "RYSH_READY", 2)
	waitForAll(t, r, waitTimeout, "已新建会话: "+id2)

	// The replacement shell is up (it started before the landing notice):
	// whatever state the killed generation left on the pty, the input
	// typed now must be visible in shell mode.
	p.Write([]byte("echo MARK\r"))
	out := r.readUntil(t, "echo MARK", waitTimeout)
	assertContains(t, out, "echo MARK")
	out = r.readUntil(t, "MARK", waitTimeout)
	assertContains(t, out, "MARK")
}

// `rysh resume <编号>` inside a live rysh switches back to the numbered session
// through the control channel, symmetric with rysh new.
func TestShellRyshResume(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	p, rc, r := startRyshIn(t, home, sessionProfile, "", nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	p.Write([]byte("\"$RYSH_SELF\" new; echo NEW_RC=$?\r"))
	id2 := waitForNewSessionID(t, home, id1, 5*time.Second)
	waitForCount(t, r, startupTimeout, "RYSH_READY", 2)
	// Baseline for the switch below: the shell generation the instance is
	// running when it enters session 2.
	shellsBefore := shellDescendants(t, rc.Process.Pid)

	p.Write([]byte("\"$RYSH_SELF\" resume 1; echo CS_RC=$?\r"))
	waitForCount(t, r, startupTimeout, "RYSH_READY", 3)
	waitForAll(t, r, waitTimeout, "已切换到会话 1")
	// Same invariant as after new: the shell generation belonging to the
	// session the switch leaves must stop running, or it keeps the pty and
	// reads the input typed for the session entered.
	if survivors := waitForShellsGone(t, shellsBefore, 3*time.Second); len(survivors) > 0 {
		t.Fatalf("shells of the session we left are still attached after cs: %v (%s)",
			survivors, formatProcs(procTree(rc.Process.Pid)))
	}

	if upd := ledgerLines(t, home, "updated"); len(upd) == 0 || upd[len(upd)-1][1] != id1 {
		t.Fatalf("updated ledger = %v, want last row naming %s", upd, id1)
	}
	for _, id := range sessRecords(t, home) {
		if id != id1 {
			t.Fatalf("instance attached to %s, want %s", id, id1)
		}
	}

	p.Write([]byte("\"$RYSH_SELF\" ls; echo LS_RC=$?\r"))
	out := r.readUntil(t, "LS_RC=0", waitTimeout)
	assertContains(t, out, "会话列表（第 1/1 页 · 共 2 个）")
	if row := lsRow(out, id1); !strings.HasPrefix(row, ">") {
		t.Fatalf("rysh ls does not mark the switched-back session %s active: %q", id1, row)
	}
	if row := lsRow(out, id2); strings.HasPrefix(row, ">") {
		t.Fatalf("rysh ls still marks session 2 as active: %q", row)
	}
	assertNoAltScreen(t, r)
}

// The one-shot subcommands never spawn a second attached rysh process:
// exactly one attachment record exists across ls and a control-channel
// switch, and no alt-screen takeover happens.
func TestShellRyshNoSecondInstance(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	p, _, r := startRyshIn(t, home, sessionProfile, "", nil)
	r.readUntil(t, "$ ", startupTimeout)
	if n := len(sessRecords(t, home)); n != 1 {
		t.Fatalf("attachment records at startup = %d, want 1", n)
	}

	p.Write([]byte("\"$RYSH_SELF\" ls >/dev/null; echo L1_RC=$?\r"))
	out := r.readUntil(t, "L1_RC=0", waitTimeout)
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("rysh ls entered the alternate screen: %q", out)
	}
	if n := len(sessRecords(t, home)); n != 1 {
		t.Fatalf("attachment records after ls = %d, want 1", n)
	}

	p.Write([]byte("\"$RYSH_SELF\" new; echo N1_RC=$?\r"))
	waitForNewSessionID(t, home, lastCreatedID(t, home), 5*time.Second)
	waitForCount(t, r, startupTimeout, "RYSH_READY", 2)
	assertNoAltScreen(t, r)
	if n := len(sessRecords(t, home)); n != 1 {
		t.Fatalf("attachment records after switch = %d, want 1", n)
	}
}

// `rysh ai "<消息>"` inside a live rysh runs a one-shot chat against the
// active session: the reply prints to the shell (no pty, no AI mode) and
// the exchange lands in the session's log so the conversation continues
// interactively.
func TestShellRyshAiChat(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	cfg := newMockChatServer(t, "oneshot marker reply")
	p, _, r := startRyshIn(t, home, sessionProfile, cfg, nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	p.Write([]byte("\"$RYSH_SELF\" ai \"oneshot hello\"; echo ONE_RC=$?\r"))
	out := r.readUntil(t, "ONE_RC=0", waitTimeout)
	assertContains(t, out, "oneshot marker reply")
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatalf("rysh ai entered the alternate screen: %q", out)
	}

	log1 := readSessionLog(t, home, id1)
	assertContains(t, log1, `"kind":"usr"`)
	assertContains(t, log1, `"p":"oneshot hello"`)
	assertContains(t, log1, `"kind":"asw"`)
	assertContains(t, log1, `"p":"oneshot marker reply"`)
}

// The AI-mode slash commands drive the same session store: /ls lists, /new
// creates and switches (shell restart + unified replay + landing notice), /resume
// switches back by number.
func TestAISlashCommands(t *testing.T) {
	home := t.TempDir()
	p, _, r := startRyshIn(t, home, sessionProfile, "", nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	ai := enterAI(t, p, r)
	assertContains(t, ai, "\x1b[35m[AI]:")

	p.Write([]byte("/ls\r"))
	out := r.readUntil(t, "本实例", waitTimeout)
	assertContains(t, out, "共 1 个")
	if row := lsRow(out, id1); !strings.HasPrefix(row, ">") {
		t.Fatalf("/ls does not mark the current session %s active: %q", id1, row)
	}

	p.Write([]byte("/new\r"))
	r.readUntil(t, "已新建会话: ", waitTimeout)
	id2 := lastCreatedID(t, home)
	if id2 == id1 {
		t.Fatalf("/new did not create a new session (id %s twice)", id2)
	}
	waitForAll(t, r, startupTimeout, "已新建会话: "+id2)

	p.Write([]byte("/ls\r"))
	out = r.readUntil(t, "本实例", waitTimeout)
	assertContains(t, out, "共 2 个")
	if row := lsRow(out, id2); !strings.HasPrefix(row, ">") {
		t.Fatalf("/ls does not mark the new session %s active: %q", id2, row)
	}

	p.Write([]byte("/resume 1\r"))
	waitForAll(t, r, startupTimeout, "已切换到会话 1")
}

// Each session's log holds only its own conversation: after chatting in
// session 1 and switching to a fresh session 2 via /new, the two
// messages.log files do not mix.
func TestSessionLogsIsolated(t *testing.T) {
	home := t.TempDir()
	cfg := newMockChatServer(t, "mock isolated reply")
	p, _, r := startRyshIn(t, home, sessionProfile, cfg, nil)
	r.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)
	enterAI(t, p, r)

	p.Write([]byte("chat one\r"))
	r.readUntil(t, "mock isolated reply", waitTimeout)
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout) // prompt redraws after finalize

	p.Write([]byte("/new\r"))
	r.readUntil(t, "已新建会话: ", waitTimeout)
	id2 := lastCreatedID(t, home)
	if id2 == id1 {
		t.Fatalf("/new did not create a new session (id %s twice)", id2)
	}
	// The fresh session's AI prompt names the model it will answer with (§3.3).
	r.readUntil(t, "ollama/llama3.1:8b", waitTimeout)

	p.Write([]byte("chat two\r"))
	r.readUntil(t, "mock isolated reply", waitTimeout)
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)

	log1 := readSessionLog(t, home, id1)
	log2 := readSessionLog(t, home, id2)
	assertContains(t, log1, `"p":"chat one"`)
	assertContains(t, log1, `"p":"mock isolated reply"`)
	if strings.Contains(log1, "chat two") {
		t.Fatalf("session 1 log leaked session 2's prompt")
	}
	assertContains(t, log2, `"p":"chat two"`)
	assertContains(t, log2, `"p":"mock isolated reply"`)
	if strings.Contains(log2, "chat one") {
		t.Fatalf("session 2 log leaked session 1's prompt")
	}
}

// A session held by another live rysh cannot be switched to: the holder
// shows up in `rysh ls` with its pid, both the shell-side `rysh resume` and the
// AI-mode `/resume` refuse, and the refusal names the offending process.
func TestAttachmentRefused(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	pA, _, rA := startRyshIn(t, home, sessionProfile, "", nil)
	rA.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	// A second rysh attaches to the same session via RYSH_SESSION_ID.
	_, _, rB := startRyshIn(t, home, sessionProfile, "", []string{"RYSH_SESSION_ID=" + id1})
	rB.readUntil(t, "$ ", startupTimeout)

	recs := sessRecords(t, home)
	if len(recs) != 2 {
		t.Fatalf("attachment records = %v, want one per instance", recs)
	}
	for _, id := range recs {
		if id != id1 {
			t.Fatalf("attachment record names %s, want %s", id, id1)
		}
	}

	// A's `rysh ls` flags the session as held by another rysh.
	pA.Write([]byte("\"$RYSH_SELF\" ls; echo LS_RC=$?\r"))
	out := rA.readUntil(t, "LS_RC=0", waitTimeout)
	assertContains(t, out, "会话列表（第 1/1 页 · 共 1 个）")
	row := lsRow(out, id1)
	if !strings.HasPrefix(row, ">") {
		t.Fatalf("A's ls does not mark its own session %s current: %q (out %q)", id1, row, out)
	}
	assertContains(t, row, "新会话")
	assertContains(t, row, "[rysh ")
	if strings.Contains(row, "本实例") {
		t.Fatalf("ls marks the held session %s as this instance's own: %q", id1, row)
	}

	// Shell-side `rysh resume 1` refuses.
	pA.Write([]byte("\"$RYSH_SELF\" resume 1; echo CS_RC=$?\r"))
	out = rA.readUntil(t, "CS_RC=1", waitTimeout)
	assertContains(t, out, "已被另一个 rysh 进程")

	// AI-mode `/resume 1` refuses the same way.
	enterAI(t, pA, rA)
	pA.Write([]byte("/resume 1\r"))
	rA.readUntil(t, "已被另一个 rysh 进程", waitTimeout)
}

// `rysh kill <编号>` terminates the rysh process attached to the target
// session, which unblocks switching to it afterwards.
func TestRyshKill(t *testing.T) {
	setSelf(t)
	home := t.TempDir()
	pA, _, rA := startRyshIn(t, home, sessionProfile, "", nil)
	rA.readUntil(t, "$ ", startupTimeout)
	id1 := lastCreatedID(t, home)

	// A moves to a fresh session 2, leaving session 1 free for B.
	pA.Write([]byte("\"$RYSH_SELF\" new; echo NEW_RC=$?\r"))
	id2 := waitForNewSessionID(t, home, id1, 5*time.Second)
	waitForCount(t, rA, startupTimeout, "RYSH_READY", 2)
	waitForAll(t, rA, waitTimeout, "已新建会话: "+id2)
	if upd := ledgerLines(t, home, "updated"); len(upd) == 0 || upd[len(upd)-1][1] != id2 {
		t.Fatalf("updated ledger = %v, want last row naming %s", upd, id2)
	}

	_, cB, rB := startRyshIn(t, home, sessionProfile, "", []string{"RYSH_SESSION_ID=" + id1})
	rB.readUntil(t, "$ ", startupTimeout)
	bExit := reapExit(t, cB)

	// Switching to B's session is refused…
	pA.Write([]byte("\"$RYSH_SELF\" resume 1; echo CS_RC=$?\r"))
	out := rA.readUntil(t, "CS_RC=1", waitTimeout)
	assertContains(t, out, "已被另一个 rysh 进程")

	// …so A kills the holder; the kill returns only after B is fully gone.
	pA.Write([]byte("\"$RYSH_SELF\" kill 1; echo KILL_RC=$?\r"))
	out = rA.readUntil(t, "KILL_RC=0", 15*time.Second)
	assertContains(t, out, "已终止 rysh 进程 ")
	select {
	case code := <-bExit:
		if code != 143 && code != 137 {
			t.Fatalf("killed rysh exit code = %d, want 143 or 137", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("killed rysh did not exit within 3s of the kill")
	}
	if recs := sessRecords(t, home); len(recs) != 1 {
		t.Fatalf("attachment records after kill = %v, want only A's", recs)
	}

	// Now the switch to session 1 goes through. As with every
	// control-channel switch, the landing may kill the shell before it
	// echoes CS2_RC, so the landing markers are the sync point.
	pA.Write([]byte("\"$RYSH_SELF\" resume 1; echo CS2_RC=$?\r"))
	waitForCount(t, rA, startupTimeout, "RYSH_READY", 3)
	waitForAll(t, rA, waitTimeout, "已切换到会话 1")
	if upd := ledgerLines(t, home, "updated"); len(upd) == 0 || upd[len(upd)-1][1] != id1 {
		t.Fatalf("updated ledger = %v, want last row naming %s", upd, id1)
	}
}

// reapExit waits for the pty child in the background and reports its exit
// code. In the test harness the outer go test process is the reaper: a
// killed rysh stays a zombie (and kill(pid, 0) still reports it alive)
// until someone calls Wait, which would defeat rysh kill's liveness
// polling — real parents reap their children, so only the test must.
func reapExit(t *testing.T, c *pty.Cmd) <-chan int {
	t.Helper()
	ch := make(chan int, 1)
	go func() {
		_ = c.Wait()
		ch <- c.ProcessState.ExitCode()
	}()
	return ch
}

// The top-level session forms: `rysh new` creates a session and enters the
// interactive program attached to it; `rysh resume <编号>` re-enters an existing
// one; an unknown target is refused before any pty is created. A clean
// `exit` hands the terminal back on the main screen (no alt-screen leave).
func TestTopLevelSessionForms(t *testing.T) {
	home := t.TempDir()

	// Top-level `rysh new`.
	p, c, r := startRyshIn(t, home, sessionProfile, "", nil, "new")
	r.readUntil(t, "$ ", startupTimeout)
	assertContains(t, string(r.all), "RYSH_READY")
	lines := ledgerLines(t, home, "created")
	if len(lines) != 1 || lines[0][1] != "1" {
		t.Fatalf("created ledger = %v, want exactly session 1", lines)
	}
	id1 := lines[0][2]
	assertContains(t, string(r.all), "已新建会话: "+id1)
	waitForAll(t, r, waitTimeout, "已新建会话: "+id1)

	p.Write([]byte("exit\r"))
	if code := waitProcess(t, c, 5*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	waitForAll(t, r, waitTimeout, "\x1b[0 q") // terminal handed back (main screen, no alt-screen leave)

	// Top-level `rysh resume 1` re-enters the same session without creating one.
	p2, c2, r2 := startRyshIn(t, home, sessionProfile, "", nil, "resume", "1")
	r2.readUntil(t, "$ ", startupTimeout)
	if lines := ledgerLines(t, home, "created"); len(lines) != 1 {
		t.Fatalf("created ledger = %v, want still one session", lines)
	}
	waitForAll(t, r2, waitTimeout, "已切换到会话 1")
	p2.Write([]byte("exit\r"))
	if code := waitProcess(t, c2, 5*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// An unknown session number is refused before any pty is created.
	cmd := exec.Command(os.Args[0], "resume", "99")
	resumeEnv := []string{"HOME=" + home}
	if runtime.GOOS == "windows" {
		// Redirect the store on Windows (os.UserHomeDir returns USERPROFILE).
		resumeEnv = append(resumeEnv, "USERPROFILE="+home)
	}
	cmd.Env = testChildEnv(resumeEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("rysh resume 99 exit code = %d, want 1 (output: %s)", code, out)
	}
	assertContains(t, string(out), "没有会话 99")
}

// M8.3: in shell mode, a line the recorder cannot reconstruct — a tab-
// completed command, a history-recalled one, or a multi-line paste (readline
// holds it in a multi-line buffer and only the first line runs) — is NOT
// logged as a command, while a single-line paste and plain typing still are.
func TestShellPasteAndUnreliableNotLogged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DEC 2004 paste markers are a Unix readline behavior")
	}
	// Force bash so bracketed paste (DEC 2004) is enabled in the child's
	// startup and the recorder sees the 200~/201~ markers.
	p, _, r := startRyshEnvShell(t, "echo RYSH_READY\nPS1='$ '\n", "", "/bin/bash")
	r.readUntil(t, "$ ", startupTimeout)
	home := os.Getenv("HOME")

	// 1. Plain command: logged.
	p.Write([]byte("echo PLAIN_1\r"))
	r.readUntil(t, "PLAIN_1", waitTimeout)

	// 2. Single-line paste: logged (the block has no newline, so the line is
	// reconstructable).
	p.Write([]byte("\x1b[200~echo PASTED_1\x1b[201~\r"))
	r.readUntil(t, "PASTED_1", waitTimeout)

	// 3. Multi-line paste: NOT logged (unreliable). Readline holds the
	// block in a multi-line buffer and runs only the first line; ^C clears
	// the remainder so it does not pollute the next step.
	p.Write([]byte("\x1b[200~echo PASTED_A\r\necho PASTED_B\x1b[201~\r"))
	r.readUntil(t, "PASTED_A", waitTimeout)
	p.Write([]byte{0x03})
	r.readQuiet(t, 500*time.Millisecond, waitTimeout)

	// 4. Tab-completed line: NOT logged (unreliable).
	p.Write([]byte("echo\t\r"))
	r.readUntil(t, "$ ", waitTimeout)
	r.readQuiet(t, 500*time.Millisecond, waitTimeout)

	data := readSessionLog(t, home, firstSessionID(t, home))
	var sks []string
	for _, ln := range strings.Split(strings.TrimSpace(data), "\n") {
		var rec session.Record
		if json.Unmarshal([]byte(ln), &rec) == nil && rec.Kind == "shk" {
			sks = append(sks, rec.P)
		}
	}
	joined := strings.Join(sks, "\n")
	// The reconstructable lines are present...
	if !strings.Contains(joined, "echo PLAIN_1") {
		t.Fatalf("plain command not logged; shk records = %q", joined)
	}
	if !strings.Contains(joined, "echo PASTED_1") {
		t.Fatalf("single-line paste not logged; shk records = %q", joined)
	}
	// ...the multi-line paste is not split into partial submits and the
	// tab-completed line is not logged either.
	for _, s := range sks {
		if strings.Contains(s, "PASTED_A") || strings.Contains(s, "PASTED_B") {
			t.Fatalf("multi-line paste was logged (whole or partial): %q", s)
		}
		if strings.HasPrefix(s, "echo ") && !strings.Contains(s, "PASTED") && !strings.Contains(s, "PLAIN") {
			t.Fatalf("tab-completed line was logged despite being unreliable: %q", s)
		}
	}
}

// firstSessionID returns the name of the single session directory in home.
func firstSessionID(t *testing.T, home string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".rysh", "sessions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("sessions = %v (err %v), want exactly one", entries, err)
	}
	return entries[0].Name()
}
