package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"ruyishell/internal/agent"
	"ruyishell/internal/provider"
	"ruyishell/internal/session"
)

func TestTrimHistory(t *testing.T) {
	msg := func(role, content string) ctxMsg {
		return ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: role, Content: content}}}
	}
	// Empty and below-cap histories pass through unchanged.
	if got := trimHistory(nil, 4); len(got) != 0 {
		t.Fatalf("trimHistory(nil) = %d messages, want 0", len(got))
	}
	two := []ctxMsg{msg("user", "a"), msg("assistant", "b")}
	if got := trimHistory(two, 4); len(got) != 2 {
		t.Fatalf("trimHistory under cap = %d messages, want 2", len(got))
	}

	// A history above the cap is trimmed in whole turns from the front so
	// it still starts with a user message.
	h := []ctxMsg{
		msg("user", "1"), msg("assistant", "a"),
		msg("user", "2"), msg("assistant", "b"),
		msg("user", "3"), msg("assistant", "c"),
	}
	got := trimHistory(h, 4)
	if len(got) != 4 {
		t.Fatalf("trimHistory cap 4 = %d messages, want 4", len(got))
	}
	if got[0].Msg.Role != "user" || got[0].Msg.Content != "2" {
		t.Fatalf("trimmed history should start with the oldest retained user message, got %+v", got[0])
	}
	if got[3].Msg.Content != "c" {
		t.Fatalf("trimmed history should keep the newest assistant reply, got %+v", got[3])
	}
	// The input slice is not mutated.
	if len(h) != 6 {
		t.Fatalf("trimHistory mutated its input: %d messages, want 6", len(h))
	}

	// Tool-result system messages stay attached to their turn: trimming
	// drops the whole turn (user + assistant + trailing tool results).
	h2 := []ctxMsg{
		msg("user", "1"), msg("assistant", "a"), msg("system", "tool a"),
		msg("user", "2"), msg("assistant", "b"),
	}
	got2 := trimHistory(h2, 4)
	if len(got2) != 2 || got2[0].Msg.Content != "2" || got2[1].Msg.Content != "b" {
		t.Fatalf("trim with tool messages = %+v, want [user 2, assistant b]", got2)
	}

	// A single turn plus its tool result fits within the cap and is kept.
	h3 := []ctxMsg{
		msg("user", "1"), msg("assistant", "a"), msg("system", "tool a"),
	}
	if got3 := trimHistory(h3, 3); len(got3) != 3 || got3[2].Msg.Content != "tool a" {
		t.Fatalf("trim single tooled turn = %+v, want all 3", got3)
	}
}

func TestBuildTimelineInterleavesChronologically(t *testing.T) {
	t0 := time.Now().Add(-3 * time.Second)
	t1 := time.Now().Add(-2 * time.Second)
	t2 := time.Now().Add(-1 * time.Second)
	evs := []session.ShellEvent{
		{Dir: "/a", Command: "cmd1", Time: t0},
		{Dir: "/a", Command: "cmd2", Time: t2},
	}
	hist := []ctxMsg{
		{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: "q1"}}, ts: t0.Add(time.Millisecond)},
		{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "assistant", Content: "a1"}}, ts: t1},
	}
	items := buildTimeline(evs, hist)
	var got []string
	for _, it := range items {
		if it.ev != nil {
			got = append(got, "ev:"+it.ev.Command)
		} else {
			got = append(got, "msg:"+it.msg.Msg.Content)
		}
	}
	want := []string{"ev:cmd1", "msg:q1", "msg:a1", "ev:cmd2"}
	if !slices.Equal(got, want) {
		t.Fatalf("buildTimeline = %v, want %v", got, want)
	}
}

func TestBuildTimelineTieGoesToShellEvent(t *testing.T) {
	ts := time.Now()
	evs := []session.ShellEvent{{Dir: "/a", Command: "c", Time: ts}}
	hist := []ctxMsg{{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: "q"}}, ts: ts}}
	items := buildTimeline(evs, hist)
	if len(items) != 2 || items[0].ev == nil || items[1].msg == nil {
		t.Fatalf("buildTimeline tie = %+v, want the event before the turn", items)
	}
}

func TestTrimShellEventsBudget(t *testing.T) {
	mk := func(cmd string) session.ShellEvent {
		return session.ShellEvent{Command: cmd, Output: strings.Repeat("o", session.MaxOutput)}
	}
	// Each formatted event is just over half the 8KB budget, so at most one
	// fits; the oldest are dropped first.
	got := trimShellEvents([]session.ShellEvent{mk("a"), mk("b"), mk("c")})
	if len(got) != 1 || got[0].Command != "c" {
		t.Fatalf("over budget: %+v, want only the newest event", got)
	}
	small := []session.ShellEvent{{Command: "a"}, {Command: "b"}}
	if g := trimShellEvents(small); len(g) != 2 {
		t.Fatalf("under budget: dropped events: %+v", g)
	}
}

func TestCurrentEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires /proc")
	}
	// A live child process whose environment carries an allowlisted var, a
	// secret-looking var that must be filtered, and the usual ones. The
	// child prints READY once it is running: /proc/<pid>/environ reads empty
	// during the fork→exec window, so we only assert after the signal. A
	// loop keeps the shell alive (no execve) so the environ is stable.
	cmd := exec.Command("sh", "-c", "echo READY; while :; do sleep 1; done")
	cmd.Env = append(os.Environ(), "EDITOR=emacs", "RYSH_SECRET_TOKEN=shh")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err := io.ReadFull(stdout, []byte("READY")); err != nil {
		t.Fatalf("child did not signal readiness: %v", err)
	}

	env := currentEnv(cmd.Process.Pid, defaultEnvAllowlist)
	if !strings.Contains(env, "EDITOR=emacs") {
		t.Fatalf("currentEnv missing allowlisted var: %q", env)
	}
	if strings.Contains(env, "RYSH_SECRET_TOKEN") {
		t.Fatalf("currentEnv leaked a non-allowlisted var: %q", env)
	}
	if !strings.Contains(env, "HOME=") {
		t.Fatalf("currentEnv missing HOME: %q", env)
	}
	// A dead pid yields nothing rather than an error.
	if got := currentEnv(999999999, defaultEnvAllowlist); got != "" {
		t.Fatalf("currentEnv on a dead pid = %q, want empty", got)
	}
}

// capture runs fn with os.Stdout and os.Stderr redirected to pipes and
// returns the captured outputs and fn's exit code.
func capture(t *testing.T, fn func() int) (string, string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	code := fn()
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(rOut)
	errOut, _ := io.ReadAll(rErr)
	return string(out), string(errOut), code
}

// Nested interactive mode is refused: with insideEnv set (the marker a
// parent rysh injects into its child shell's environment), a bare `rysh`
// start exits 1 with the refusal notice on stderr — before any pty exists —
// while the version flag still prints and exits 0.
func TestNestedInteractiveRefused(t *testing.T) {
	t.Setenv(insideEnv, "1")
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	t.Run("bare start refused", func(t *testing.T) {
		os.Args = []string{"rysh"}
		stdout, stderr, code := capture(t, ryshMain)
		if code != 1 {
			t.Fatalf("nested bare start exit = %d, want 1 (stdout %q, stderr %q)", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "已经在 rysh 中") {
			t.Fatalf("nested bare start stderr missing the notice: %q", stderr)
		}
		if !strings.Contains(stderr, subcommandUsage()) {
			t.Fatalf("nested bare start stderr missing the subcommand usage: %q", stderr)
		}
		if stdout != "" {
			t.Fatalf("nested bare start wrote to stdout: %q", stdout)
		}
	})

	t.Run("bare rysh ai refused", func(t *testing.T) {
		os.Args = []string{"rysh", "ai"}
		_, stderr, code := capture(t, ryshMain)
		if code != 1 {
			t.Fatalf("nested rysh ai (no message) exit = %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "已经在 rysh 中") {
			t.Fatalf("nested rysh ai stderr missing the notice: %q", stderr)
		}
		if !strings.Contains(stderr, "rysh ai \"消息\"") {
			t.Fatalf("nested rysh ai stderr missing the usage: %q", stderr)
		}
	})

	t.Run("unknown subcommand refused", func(t *testing.T) {
		os.Args = []string{"rysh", "frobnicate"}
		_, stderr, code := capture(t, ryshMain)
		if code != 1 {
			t.Fatalf("unknown subcommand exit = %d, want 1 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stderr, "未知的 rysh 子命令: frobnicate") {
			t.Fatalf("unknown subcommand stderr missing the notice: %q", stderr)
		}
	})

	t.Run("version flag still works", func(t *testing.T) {
		os.Args = []string{"rysh", "-v"}
		stdout, stderr, code := capture(t, ryshMain)
		if code != 0 {
			t.Fatalf("nested -v exit = %d, want 0 (stderr %q)", code, stderr)
		}
		if !strings.Contains(stdout, "rysh "+version) {
			t.Fatalf("nested -v stdout missing the version: %q", stdout)
		}
	})
}

// childEnv carries exactly one RYSH_INSIDE marker (any stale entry is
// replaced, so the env never holds duplicate keys), drops stale
// RYSH_SESSION_ID / RYSH_CTL / RYSH_PID entries, adds the fresh
// session/ctl/pid values when applicable, and keeps the other variables.
func TestChildEnv(t *testing.T) {
	t.Setenv("RYSH_KEEP_ME", "yes")
	t.Setenv(insideEnv, "stale")
	t.Setenv(sessionEnv, "stale-sess")
	t.Setenv(ctlEnv, "stale-ctl")
	t.Setenv(pidEnv, "stale-pid")
	var markers, kept, sess, ctl, pid int
	for _, kv := range childEnv("sabc", "/tmp/ctl/42.cmd") {
		switch {
		case strings.HasPrefix(kv, insideEnv+"="):
			markers++
			if kv != insideEnv+"=1" {
				t.Fatalf("childEnv marker = %q, want %q=1", kv, insideEnv)
			}
		case strings.HasPrefix(kv, sessionEnv+"="):
			sess++
			if kv != sessionEnv+"=sabc" {
				t.Fatalf("childEnv session = %q, want %q=sabc", kv, sessionEnv)
			}
		case strings.HasPrefix(kv, ctlEnv+"="):
			ctl++
			if kv != ctlEnv+"=/tmp/ctl/42.cmd" {
				t.Fatalf("childEnv ctl = %q, want %q=/tmp/ctl/42.cmd", kv, ctlEnv)
			}
		case strings.HasPrefix(kv, pidEnv+"="):
			pid++
			if kv != pidEnv+"="+strconv.Itoa(os.Getpid()) {
				t.Fatalf("childEnv pid = %q, want %q=%d", kv, pidEnv, os.Getpid())
			}
		}
		if kv == "RYSH_KEEP_ME=yes" {
			kept++
		}
	}
	if markers != 1 {
		t.Fatalf("childEnv RYSH_INSIDE markers = %d, want exactly 1", markers)
	}
	if sess != 1 {
		t.Fatalf("childEnv RYSH_SESSION_ID markers = %d, want exactly 1", sess)
	}
	if ctl != 1 {
		t.Fatalf("childEnv RYSH_CTL markers = %d, want exactly 1", ctl)
	}
	if pid != 1 {
		t.Fatalf("childEnv RYSH_PID markers = %d, want exactly 1", pid)
	}
	if kept != 1 {
		t.Fatalf("childEnv lost RYSH_KEEP_ME")
	}

	// Empty session/ctl values are omitted entirely; RYSH_PID is always set.
	for _, kv := range childEnv("", "") {
		if strings.HasPrefix(kv, sessionEnv+"=") || strings.HasPrefix(kv, ctlEnv+"=") {
			t.Fatalf("childEnv emitted an empty session/ctl entry: %q", kv)
		}
	}
}

// selfPID honors RYSH_PID (the interactive instance's pid inherited by a
// one-shot subprocess) and falls back to the process's own pid otherwise.
func TestSelfPID(t *testing.T) {
	want := 4242
	t.Setenv(pidEnv, strconv.Itoa(want))
	if got := selfPID(); got != want {
		t.Fatalf("selfPID with RYSH_PID set = %d, want %d", got, want)
	}
	t.Setenv(pidEnv, "not-a-number")
	if got := selfPID(); got != os.Getpid() {
		t.Fatalf("selfPID with bogus RYSH_PID = %d, want own pid %d", got, os.Getpid())
	}
	t.Setenv(pidEnv, "")
	if got := selfPID(); got != os.Getpid() {
		t.Fatalf("selfPID without RYSH_PID = %d, want own pid %d", got, os.Getpid())
	}
}

// titleFrom derives the display name: the first line, whitespace collapsed,
// truncated to 16 runes; empty input yields empty output.
func TestTitleFrom(t *testing.T) {
	if got := titleFrom(""); got != "" {
		t.Fatalf("titleFrom(\"\") = %q, want empty", got)
	}
	if got := titleFrom("hello world\nsecond line"); got != "hello world" {
		t.Fatalf("titleFrom multi-line = %q, want the first line only", got)
	}
	if got := titleFrom("  你好\t世界  "); got != "你好 世界" {
		t.Fatalf("titleFrom collapsed = %q, want %q", got, "你好 世界")
	}
	long := strings.Repeat("字", 20)
	if got := titleFrom(long); got != strings.Repeat("字", 16) {
		t.Fatalf("titleFrom truncated = %d runes, want 16", len([]rune(got)))
	}
}

// updateSessionTitle writes the name only while the session is unnamed, and
// persists it to the per-session name file.
func TestUpdateSessionTitle(t *testing.T) {
	store := session.NewStoreAt(t.TempDir())
	m, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	updateSessionTitle(store, m, "  你好 世界  ")
	if m.Name != "你好 世界" {
		t.Fatalf("updateSessionTitle set name %q, want 你好 世界", m.Name)
	}
	if got := store.LoadName(m.ID); got != "你好 世界" {
		t.Fatalf("LoadName = %q, want the saved title", got)
	}
	// A named session is not overwritten by later prompts.
	updateSessionTitle(store, m, "second prompt")
	if m.Name != "你好 世界" {
		t.Fatalf("updateSessionTitle overwrote a named session: %q", m.Name)
	}
}

// resolveSession maps a number or a raw id; numbers are tried first, and a
// missing identifier reports an error.
func TestResolveSession(t *testing.T) {
	store := session.NewStoreAt(t.TempDir())
	m1, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	byNum, err := resolveSession(store, "2")
	if err != nil || byNum.ID != m2.ID {
		t.Fatalf("resolveSession by number = %+v, %v; want session %s", byNum, err, m2.ID)
	}
	byID, err := resolveSession(store, m1.ID)
	if err != nil || byID.ID != m1.ID {
		t.Fatalf("resolveSession by id = %+v, %v; want session %s", byID, err, m1.ID)
	}
	if _, err := resolveSession(store, "99"); err == nil || !strings.Contains(err.Error(), "没有会话 99") {
		t.Fatalf("resolveSession missing = %v, want 没有会话 99", err)
	}
	if _, err := resolveSession(store, "sffffffff"); err == nil || !strings.Contains(err.Error(), "没有会话 sffffffff") {
		t.Fatalf("resolveSession missing id = %v, want 没有会话", err)
	}
}

// scanAttached filters stale attachment records: dead pids and selfPID are
// dropped, live foreign pids are kept.
func TestScanAttached(t *testing.T) {
	dir := t.TempDir()
	// A live pid (this test process) and a dead pid.
	live := os.Getpid()
	dead := 999999999
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.sess", live)), []byte("sabc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.sess", dead)), []byte("sdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-.sess files are ignored.
	if err := os.WriteFile(filepath.Join(dir, "999.cmd"), []byte("switch s1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Excluding selfPID drops the live self record; the dead pid is gone too.
	if got := scanAttached(dir, live); len(got) != 0 {
		t.Fatalf("scanAttached(self) = %v, want empty", got)
	}
	// With a foreign selfPID the live record survives, the dead one does not.
	got := scanAttached(dir, 0)
	if got["sabc"] != live {
		t.Fatalf("scanAttached missing the live record: %v", got)
	}
	if _, ok := got["sdef"]; ok {
		t.Fatalf("scanAttached kept a dead pid: %v", got)
	}
}

// renderSessionList renders the page header, the column header, the active
// marker, creation/update times, and the occupancy column; page boundaries
// split 10 rows per page.
func TestRenderSessionList(t *testing.T) {
	store := session.NewStoreAt(t.TempDir())
	m1, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := store.CreateSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchUpdated(m1.ID); err != nil {
		t.Fatal(err)
	}
	lines := renderSessionList(store, m1.ID, 1, map[string]int{m2.ID: 1234})
	assertContains(t, strings.Join(lines, "\n"), "会话列表（第 1/1 页 · 共 2 个）")
	// The column header names the time and occupancy columns.
	assertContains(t, lines[1], "创建时间")
	assertContains(t, lines[1], "最后更新")
	assertContains(t, lines[1], "占用")
	// Active row: > marker, name, creation time, update time, 本实例.
	assertContains(t, lines[2], m1.ID)
	assertContains(t, lines[2], "新会话")
	assertContains(t, lines[2], fmtListTime(m1.Created))
	assertContains(t, lines[2], fmtListTime(store.UpdatedTimes()[m1.ID]))
	assertContains(t, lines[2], "本实例")
	// Row 2: attached by another live rysh, never updated, not active.
	assertContains(t, lines[3], m2.ID)
	assertContains(t, lines[3], fmtListTime(m2.Created))
	assertContains(t, lines[3], "[rysh 1234]")
	if strings.Contains(lines[3], "本实例") {
		t.Fatalf("attached row also marked 本实例: %q", lines[3])
	}
	if !strings.HasPrefix(lines[2], ">") || strings.HasPrefix(lines[3], ">") {
		t.Fatalf("active marker on the wrong row: %q / %q", lines[2], lines[3])
	}
	// Column alignment: every column starts at the same display width in the
	// header and in the data rows (CJK cells count double, so 编号 is four
	// cells wide and the single-digit numbers pad out to that).
	cellAt := func(line, cell string) int {
		i := strings.Index(line, cell)
		if i < 0 {
			t.Fatalf("cell %q missing from %q", cell, line)
		}
		return ansi.StringWidth(line[:i])
	}
	if a, b, c := cellAt(lines[1], "ID"), cellAt(lines[2], m1.ID), cellAt(lines[3], m2.ID); a != b || b != c {
		t.Fatalf("ID column misaligned: %d/%d/%d in %q", a, b, c, lines)
	}
	if a, b := cellAt(lines[1], "创建时间"), cellAt(lines[2], fmtListTime(m1.Created)); a != b {
		t.Fatalf("creation time column misaligned: %d vs %d in %q", a, b, lines)
	}
	if a, b := cellAt(lines[1], "占用"), cellAt(lines[3], "[rysh 1234]"); a != b {
		t.Fatalf("occupancy column misaligned: %d vs %d in %q", a, b, lines)
	}

	// Pagination: 12 sessions → 2 pages; page 2 shows sessions 11 and 12
	// plus the flip hint; an out-of-range page reports the missing page.
	store2 := session.NewStoreAt(t.TempDir())
	for i := 0; i < 12; i++ {
		if _, err := store2.CreateSession(); err != nil {
			t.Fatal(err)
		}
	}
	metas, err := store2.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	p2 := renderSessionList(store2, "", 2, nil)
	assertContains(t, p2[0], "第 2/2 页 · 共 12 个")
	if len(p2) != 5 {
		t.Fatalf("page 2 = %d lines, want 5 (page header + column header + 2 rows + hint)", len(p2))
	}
	assertContains(t, p2[2], metas[10].ID)
	assertContains(t, p2[3], metas[11].ID)
	assertContains(t, p2[4], "ls <页号> 翻页")
	assertContains(t, renderSessionList(store2, "", 3, nil)[0], "没有第 3 页")
	assertContains(t, renderSessionList(store2, "", 0, nil)[0], "没有第 0 页")
}

// TestRenderHistory locks the /history renderer: a numbered, paginated list
// of the session's user inputs (usr records, oldest first), -v mode pairing
// each input with its merged asw reply, the page header/footers, the
// out-of-range page message, and the empty-history case.
func TestRenderHistory(t *testing.T) {
	dir := t.TempDir()
	log, err := session.OpenLog(dir, "histr0")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	writes := [][2]string{
		{"usr", "first input"},
		{"asw", "hello "},
		{"asw", "world"},
		{"usr", "second input"},
		{"usr", "third input"},
	}
	for _, w := range writes {
		if err := log.Write(w[0], w[1]); err != nil {
			t.Fatalf("log.Write(%s, %q): %v", w[0], w[1], err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}

	// Plain: 3 inputs, 1 page, numbered oldest first, no reply lines.
	lines := renderHistory(dir, "histr0", 1, false)
	assertContains(t, lines[0], "用户输入历史（第 1/1 页 · 共 3 条）")
	assertContains(t, lines[1], "1. first input")
	assertContains(t, lines[2], "2. second input")
	assertContains(t, lines[3], "3. third input")
	if len(lines) != 4 {
		t.Fatalf("plain single page = %d lines, want 4 (header + 3): %q", len(lines), lines)
	}
	for _, ln := range lines[1:] {
		if strings.Contains(ln, "↳") {
			t.Fatalf("plain /history must not show replies: %q", ln)
		}
	}

	// -v: each input paired with its merged reply; an input with no reply
	// shows （无输出）.
	v := strings.Join(renderHistory(dir, "histr0", 1, true), "\n")
	assertContains(t, v, "用户输入历史（第 1/1 页 · 共 3 条）")
	assertContains(t, v, "1. first input")
	assertContains(t, v, "  ↳ hello world")
	assertContains(t, v, "2. second input")
	assertContains(t, v, "  ↳ （无输出）")
	assertContains(t, v, "3. third input")
	assertContains(t, v, "  ↳ （无输出）")

	// Pagination: 12 inputs → 2 pages in plain mode (page size 10); page 2
	// shows inputs 11 and 12 plus the flip hint; out-of-range reports it.
	log2, err := session.OpenLog(dir, "histpg0")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	for i := 1; i <= 12; i++ {
		if err := log2.Write("usr", fmt.Sprintf("input %02d", i)); err != nil {
			t.Fatalf("log.Write: %v", err)
		}
	}
	if err := log2.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	p2 := renderHistory(dir, "histpg0", 2, false)
	assertContains(t, p2[0], "第 2/2 页 · 共 12 条")
	assertContains(t, p2[1], "11. input 11")
	assertContains(t, p2[2], "12. input 12")
	assertContains(t, p2[3], "history <页号> 翻页")
	if len(p2) != 4 {
		t.Fatalf("page 2 = %d lines, want 4 (header + 2 + hint): %q", len(p2), p2)
	}
	assertContains(t, renderHistory(dir, "histpg0", 3, false)[0], "没有第 3 页")
	assertContains(t, renderHistory(dir, "histpg0", 0, false)[0], "没有第 0 页")

	// Empty history reports 共 0 条.
	empty := renderHistory(dir, "no-such-id", 1, false)
	if len(empty) != 1 || !strings.Contains(empty[0], "共 0 条") {
		t.Fatalf("empty history = %q, want a single 共 0 条 line", empty)
	}
}

// fmtListTime renders unix milliseconds as "01-02 15:04" within the
// current year, "2006-01-02" across years, and "-" for missing records.
func TestFmtListTime(t *testing.T) {
	if got := fmtListTime(0); got != "-" {
		t.Fatalf("fmtListTime(0) = %q, want \"-\"", got)
	}
	now := time.Now()
	if got := fmtListTime(now.UnixMilli()); got != now.Format("01-02 15:04") {
		t.Fatalf("fmtListTime(now) = %q, want %q", got, now.Format("01-02 15:04"))
	}
	old := time.Date(2020, 5, 4, 3, 2, 0, 0, time.Local)
	if got := fmtListTime(old.UnixMilli()); got != "2020-05-04" {
		t.Fatalf("fmtListTime(old) = %q, want %q", got, "2020-05-04")
	}
}

// reconstructHistory rebuilds the conversation: usr prompts that start with
// / or ! are skipped, contiguous asw segments merge into one assistant
// message, rea reasoning is dropped, tool records become system messages,
// and everything else is ignored.
func TestReconstructHistory(t *testing.T) {
	recs := []session.Record{
		{Ts: 1000, Kind: "usr", P: "hello"},
		{Ts: 2000, Kind: "usr", P: "/new"},
		{Ts: 3000, Kind: "usr", P: "!ls"},
		{Ts: 4000, Kind: "asw", P: "part1"},
		{Ts: 4001, Kind: "rea", P: "thinking"},
		{Ts: 4002, Kind: "asw", P: "part2"},
		{Ts: 5000, Kind: "tool", P: "[tool] $ ls"},
		{Ts: 6000, Kind: "noti", P: "已新建会话: sabc"},
		{Ts: 7000, Kind: "sys", P: "session:switch"},
	}
	hist := reconstructHistory(recs)
	var got []string
	for _, m := range hist {
		got = append(got, m.Msg.Role+":"+m.Msg.Content)
	}
	want := []string{"user:hello", "assistant:part1part2", "system:[tool] $ ls"}
	if !slices.Equal(got, want) {
		t.Fatalf("reconstructHistory = %v, want %v", got, want)
	}
}

// The control channel round-trips switch requests: writeControlSwitch
// appends to $RYSH_CTL, readControlRequests parses and truncates them so
// each request is processed once.
func TestControlChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.cmd")
	t.Setenv(ctlEnv, path)

	// No file yet: read is a no-op.
	if reqs := readControlRequests(path); len(reqs) != 0 {
		t.Fatalf("readControlRequests on missing file = %v, want empty", reqs)
	}

	writeControlSwitch("sabc", "已新建会话: sabc")
	writeControlSwitch("sdef", "已切换到会话 2")
	reqs := readControlRequests(path)
	if len(reqs) != 2 {
		t.Fatalf("readControlRequests = %d requests, want 2", len(reqs))
	}
	if reqs[0].id != "sabc" || reqs[0].notice != "已新建会话: sabc" {
		t.Fatalf("request 0 = %+v, want sabc/已新建会话: sabc", reqs[0])
	}
	if reqs[1].id != "sdef" || reqs[1].notice != "已切换到会话 2" {
		t.Fatalf("request 1 = %+v, want sdef/已切换到会话 2", reqs[1])
	}
	// The file is truncated after processing.
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatalf("control channel not truncated: %q", data)
	}
	// Garbage lines are skipped.
	if err := os.WriteFile(path, []byte("garbage\nswitch sabc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs = readControlRequests(path)
	if len(reqs) != 1 || reqs[0].id != "sabc" {
		t.Fatalf("readControlRequests with garbage = %+v, want one sabc request", reqs)
	}
}

// The unified replay must reassemble streaming records exactly as the live
// output appeared. shl records carry their own trailing \n (one per complete
// output line) and asw records are raw delta slices of one answer, so joining
// either kind with a "\n" separator adds a blank line between every shell
// output line and breaks the answer at every delta boundary — the regression
// this test locks. reconstructHistory already concatenates these verbatim;
// the replay must agree with it.
func TestRenderUnifiedReplayJoinsFragmentsVerbatim(t *testing.T) {
	dir := t.TempDir()
	log, err := session.OpenLog(dir, "sjtest0")
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	writes := [][2]string{
		{"shk", "echo ONE"},
		{"shl", "ONE\n"},
		{"shl", "\x1b[32mgreen\x1b[0m\n"},
		{"usr", "say hi"},
		{"asw", "hel"},
		{"asw", "lo "},
		{"asw", "world"},
	}
	for _, w := range writes {
		if err := log.Write(w[0], w[1]); err != nil {
			t.Fatalf("log.Write(%s, %q): %v", w[0], w[1], err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log.Close: %v", err)
	}
	got := renderUnifiedReplay(dir, "sjtest0", 200)
	if got == "" {
		t.Fatal("renderUnifiedReplay returned empty for a non-empty log")
	}
	if !strings.Contains(got, "─── $ echo ONE ───") {
		t.Fatalf("command header missing from replay: %q", got)
	}
	// Shell output lines replay back-to-back, verbatim, with no inserted
	// blank line between them.
	if want := "ONE\n\x1b[32mgreen\x1b[0m\n"; !strings.Contains(got, want) {
		t.Fatalf("shell output lines not replayed verbatim: got %q", got)
	}
	if strings.Contains(got, "ONE\n\n") {
		t.Fatalf("replay inserted a blank line between output lines: %q", got)
	}
	// The answer's deltas reassemble into the original text, no breaks.
	if !strings.Contains(got, "hello world") {
		t.Fatalf("answer deltas not reassembled verbatim: %q", got)
	}
}

// The block-atomic tail cut must render its "more" marker when older blocks
// are dropped, and a prefix cut (enterAI's once-only startup-history replay)
// must render exactly the given records — never records beyond them.
func TestRenderUnifiedReplayMoreMarkerAndPrefix(t *testing.T) {
	recs := []session.Record{
		{Kind: "shk", P: "echo A"},
		{Kind: "shl", P: "AOUT1\nAOUT2\n"},
		{Kind: "shk", P: "echo B"},
		{Kind: "shl", P: "BOUT\n"},
	}

	// A two-line budget keeps only the last block, with the marker on top.
	got := renderUnifiedReplayRecords(recs, 2)
	if !strings.Contains(got, "… 更早的对话已省略") {
		t.Fatalf("truncation marker missing from replay: %q", got)
	}
	if !strings.Contains(got, "BOUT\n") {
		t.Fatalf("last block missing from truncated replay: %q", got)
	}
	if strings.Contains(got, "AOUT1") || strings.Contains(got, "echo A") {
		t.Fatalf("truncated replay kept dropped blocks: %q", got)
	}

	// The prefix cut renders only the leading records, within budget.
	prefix := renderUnifiedReplayRecords(recs[:2], 200)
	if !strings.Contains(prefix, "AOUT2\n") {
		t.Fatalf("prefix replay missing its last block: %q", prefix)
	}
	if strings.Contains(prefix, "BOUT") || strings.Contains(prefix, "echo B") {
		t.Fatalf("prefix replay leaked records beyond the cut: %q", prefix)
	}
	if strings.Contains(prefix, "… 更早的对话已省略") {
		t.Fatalf("prefix replay marked truncation within budget: %q", prefix)
	}
}

// The spinner's elapsed wait is grok-style: whole seconds below a minute,
// then minutes and seconds (1m56s).
func TestSpinnerElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{time.Second, "1s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m0s"},
		{116 * time.Second, "1m56s"},
		{3661 * time.Second, "61m1s"},
	}
	for _, c := range cases {
		if got := spinnerElapsed(c.d); got != c.want {
			t.Fatalf("spinnerElapsed(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
