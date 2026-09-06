package session

import (
	"strings"
	"testing"
)

func TestSubmitRecordsCommand(t *testing.T) {
	r := New()
	r.Type('l')
	r.Type('s')
	r.Enter("/work")
	evs := r.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].Command != "ls" || evs[0].Dir != "/work" {
		t.Fatalf("event = %+v, want ls in /work", evs[0])
	}
	if evs[0].Time.IsZero() {
		t.Fatalf("event has no timestamp")
	}
}

func TestBlankLineNotRecorded(t *testing.T) {
	r := New()
	r.Enter("/work") // empty line
	r.Type(' ')
	r.Enter("/work") // whitespace-only line
	if got := r.Events(); len(got) != 0 {
		t.Fatalf("blank lines recorded: %+v", got)
	}
}

func TestBackspace(t *testing.T) {
	r := New()
	r.Type('l')
	r.Type('s')
	r.Type('a')
	r.Backspace()
	r.Enter("")
	if evs := r.Events(); len(evs) != 1 || evs[0].Command != "ls" {
		t.Fatalf("backspace not applied: %+v", evs)
	}
}

func TestControlClearsLine(t *testing.T) {
	for _, b := range []byte{0x03, 0x15} { // ^C, ^U
		r := New()
		r.Type('r')
		r.Type('m')
		r.Control([]byte{b})
		r.Enter("")
		if evs := r.Events(); len(evs) != 0 {
			t.Fatalf("control 0x%x did not clear line: %+v", b, evs)
		}
	}
}

func TestControlKillWordAndKillLine(t *testing.T) {
	r := New()
	for _, c := range "rm -rf /tmp/x" {
		r.Type(c)
	}
	r.Control([]byte{0x17}) // ^W kills the last word (/tmp/x is one token)
	r.Type('y')
	r.Enter("")
	if evs := r.Events(); len(evs) != 1 || evs[0].Command != "rm -rf y" {
		t.Fatalf("^W handled wrong: %+v", evs)
	}

	r = New()
	for _, c := range "echo abc" {
		r.Type(c)
	}
	r.Left()                // cursor before the last rune
	r.Control([]byte{0x0b}) // ^K kills from the cursor to the end of line
	r.Enter("")
	if evs := r.Events(); len(evs) != 1 || evs[0].Command != "echo ab" {
		t.Fatalf("^K should kill from the cursor: %+v", evs)
	}
}

func TestOutputCapturedAndCapped(t *testing.T) {
	r := New()
	r.Type('l')
	r.Enter("")
	r.Output([]byte("file1\nfile2\n"))
	if evs := r.Events(); len(evs) != 1 || evs[0].Output != "file1\nfile2\n" {
		t.Fatalf("output not captured: %+v", evs)
	}
	// A new command's first keystroke closes the previous capture.
	r.Type('n')
	if evs := r.Events(); len(evs) != 1 || evs[0].Output != "file1\nfile2\n" {
		t.Fatalf("previous event not closed on new typing: %+v", evs)
	}

	// A long stream is kept as head+tail with an elision marker: the first
	// lines and the final result/errors survive, the middle is dropped.
	r = New()
	r.Type('x')
	r.Enter("")
	big := strings.Repeat("o", MaxOutput*2)
	r.Output([]byte(big))
	evs := r.Events()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	out := evs[0].Output
	// Head and tail each keep shellOutHalf bytes.
	if !strings.HasPrefix(out, strings.Repeat("o", shellOutHalf)) {
		t.Fatalf("head not kept: %q", out[:20])
	}
	if !strings.HasSuffix(out, strings.Repeat("o", shellOutHalf)) {
		t.Fatalf("tail not kept: %q", out[len(out)-20:])
	}
	if !strings.Contains(out, "已省略") {
		t.Fatalf("elision marker missing: %q", out)
	}
	// The middle is gone, so total is head+tail+marker, far below MaxOutput*2.
	if len(out) > MaxOutput+64 {
		t.Fatalf("output not bounded to head+tail+marker: %d bytes", len(out))
	}
	// A short stream is captured verbatim (no marker, no elision).
	r = New()
	r.Type('y')
	r.Enter("")
	r.Output([]byte("file1\nfile2\n"))
	if got := r.Events()[0].Output; got != "file1\nfile2\n" {
		t.Fatalf("short output not verbatim: %q", got)
	}
}

func TestOutputIgnoredWithoutOpenEvent(t *testing.T) {
	r := New()
	r.Output([]byte("initial prompt noise"))
	r.Type('h')
	r.Output([]byte("echo while typing"))
	r.Enter("")
	if evs := r.Events(); len(evs) != 1 || evs[0].Output != "" {
		t.Fatalf("unexpected output captured: %+v", evs)
	}
}

func TestAbortClosesEvent(t *testing.T) {
	r := New()
	r.Type('l')
	r.Enter("")
	r.Output([]byte("listing"))
	r.Abort()
	if evs := r.Events(); len(evs) != 1 || evs[0].Output != "listing" {
		t.Fatalf("abort did not finalize event: %+v", evs)
	}
}

func TestRingEviction(t *testing.T) {
	r := New()
	for i := 0; i < MaxEvents+5; i++ {
		r.Type('c')
		r.Enter("")
		r.Abort()
	}
	evs := r.Events()
	if len(evs) != MaxEvents {
		t.Fatalf("ring size = %d, want %d", len(evs), MaxEvents)
	}
	if evs[0].Command != "c" || evs[len(evs)-1].Command != "c" {
		t.Fatalf("unexpected ring contents: %+v", evs)
	}
}

func TestFormatStripsANSIAndCarriesCWD(t *testing.T) {
	r := New()
	r.Type('l')
	r.Enter("/a")
	r.Output([]byte("\x1b[31mred\x1b[0m files\n$ "))
	r.Abort()

	f := r.Events()[0].Format()
	if !strings.Contains(f, "cwd: /a") || !strings.Contains(f, "$ l") ||
		!strings.Contains(f, "red files") || !strings.HasSuffix(f, "---\n") {
		t.Fatalf("Format = %q, want cwd line, command, stripped output and separator", f)
	}
	if strings.Contains(f, "\x1b[") {
		t.Fatalf("ANSI sequences leaked into context: %q", f)
	}
}

func TestFormatMasksCredentials(t *testing.T) {
	r := New()
	// A credential typed into the command and one printed by it must not
	// cross into the context verbatim.
	for _, c := range "curl -H 'Authorization: Bearer abcdef123456'" {
		r.Type(c)
	}
	r.Enter("/w")
	r.Output([]byte("export API_KEY=sk-abcdefghijklmnop123456\n"))
	r.Abort()
	f := r.Events()[0].Format()
	if strings.Contains(f, "abcdef123456") {
		t.Fatalf("Bearer token leaked into context: %q", f)
	}
	if strings.Contains(f, "sk-abcdefghijklmnop123456") {
		t.Fatalf("API key leaked into context: %q", f)
	}
	if !strings.Contains(f, "[已脱敏]") {
		t.Fatalf("mask marker missing: %q", f)
	}
}

func TestPauseStopsRecording(t *testing.T) {
	r := New()
	// A real command opens a capture; the fullscreen program then starts.
	r.Type('l')
	r.Enter("/w")
	r.Pause()

	// Fullscreen screen content and keystrokes (incl. Enter) are ignored.
	r.Output([]byte("VI_SCREEN"))
	for _, c := range "i" {
		r.Type(c)
	}
	if got := r.Enter("/w"); got != "" {
		t.Fatalf("Enter while paused = %q, want empty", got)
	}
	if got := r.Line(); got != "" {
		t.Fatalf("Line while paused = %q, want empty", got)
	}
	evs := r.Events()
	if len(evs) != 1 || strings.Contains(evs[0].Output, "VI_SCREEN") {
		t.Fatalf("fullscreen content recorded: %+v", evs)
	}

	// Resume: the exit redraw is ignored (no open capture); the next
	// command starts a fresh capture.
	r.Resume()
	r.Output([]byte("VI_EXIT_REDRAW"))
	for _, c := range "echo after" {
		r.Type(c)
	}
	r.Enter("/w2")
	r.Output([]byte("after out"))
	evs = r.Events()
	if len(evs) != 2 {
		t.Fatalf("want 2 events after resume, got %d: %+v", len(evs), evs)
	}
	if evs[1].Command != "echo after" || evs[1].Output != "after out" {
		t.Fatalf("post-resume event = %+v, want clean command and output", evs[1])
	}
	if strings.Contains(evs[1].Output, "VI_EXIT_REDRAW") {
		t.Fatalf("exit redraw attributed to the next command: %q", evs[1].Output)
	}
	// The pre-fullscreen event survives the pause.
	if evs[0].Command != "l" {
		t.Fatalf("pre-fullscreen event lost: %+v", evs[0])
	}
}

func TestPauseClearsInProgressLine(t *testing.T) {
	r := New()
	for _, c := range "ab" {
		r.Type(c)
	}
	r.Pause()
	if got := r.Line(); got != "" {
		t.Fatalf("Line after Pause = %q, want empty", got)
	}
	r.Resume()
	r.Type('c')
	r.Enter("/w")
	evs := r.Events()
	if len(evs) != 1 || evs[0].Command != "c" {
		t.Fatalf("stale bytes survived the pause: %+v", evs)
	}
}

func TestLineReturnsInProgressCommand(t *testing.T) {
	r := New()
	if got := r.Line(); got != "" {
		t.Fatalf("Line on empty recorder = %q, want empty", got)
	}
	for _, c := range "ls -" {
		r.Type(c)
	}
	if got := r.Line(); got != "ls -" {
		t.Fatalf("Line = %q, want %q", got, "ls -")
	}
	r.Backspace()
	if got := r.Line(); got != "ls " {
		t.Fatalf("Line after backspace = %q, want %q", got, "ls ")
	}
	// Control clears the line.
	r.Control([]byte{0x03})
	if got := r.Line(); got != "" {
		t.Fatalf("Line after ^C = %q, want empty", got)
	}
}

func TestEnterReturnsCommand(t *testing.T) {
	r := New()
	for _, c := range "echo hi" {
		r.Type(c)
	}
	if got := r.Enter("/w"); got != "echo hi" {
		t.Fatalf("Enter returned %q, want %q", got, "echo hi")
	}
	// Blank line returns "".
	if got := r.Enter("/w"); got != "" {
		t.Fatalf("Enter on blank line returned %q, want empty", got)
	}
}

// A single-line DEC 2004 paste is buffered and logged as one normal command
// line; a multi-line paste is flagged unreliable (readline holds it in a
// multi-line buffer and only the first line runs on the next Enter, so the
// recorder cannot reconstruct what the shell ran) and is not logged.
func TestPasteBlockSingleCommand(t *testing.T) {
	// Single-line paste: recorded like typed input.
	r := New()
	r.PasteStart()
	if !r.InPaste() {
		t.Fatalf("InPaste() = false inside a paste block")
	}
	r.PasteText("echo PASTED")
	if got := r.Enter("/w"); got != "echo PASTED" {
		t.Fatalf("single-line paste Enter returned %q, want %q", got, "echo PASTED")
	}
	if r.InPaste() {
		t.Fatalf("paste state not cleared after Enter")
	}
	if r.Unreliable() {
		t.Fatalf("single-line paste flagged unreliable")
	}

	// Multi-line paste: not reconstructable, so not logged.
	r.PasteStart()
	r.PasteText("echo A\n")
	r.PasteText("echo B")
	if !r.Unreliable() {
		t.Fatalf("multi-line paste not flagged unreliable")
	}
	if got := r.Enter("/w"); got != "" {
		t.Fatalf("multi-line paste Enter returned %q, want empty (unreliable)", got)
	}
	if r.InPaste() {
		t.Fatalf("paste state not cleared after Enter")
	}
	// Flag clears on submit; the next line records normally.
	r.Type('x')
	if got := r.Enter("/w"); got != "x" {
		t.Fatalf("next Enter returned %q, want %q", got, "x")
	}
}

// A line that used tab completion or history recall is flagged unreliable:
// its reconstructed text is not what the shell ran, so Enter must not log it
// as a command.
func TestUnreliableLineNotLogged(t *testing.T) {
	r := New()
	for _, c := range "ec" {
		r.Type(c)
	}
	r.MarkUnreliable()
	if got := r.Enter("/w"); got != "" {
		t.Fatalf("Enter on an unreliable line returned %q, want empty", got)
	}
	// The flag clears on submit, so the next line records normally.
	if r.Unreliable() {
		t.Fatalf("unreliable flag not cleared after Enter")
	}
	for _, c := range "ls" {
		r.Type(c)
	}
	if got := r.Enter("/w"); got != "ls" {
		t.Fatalf("next Enter returned %q, want %q", got, "ls")
	}
}

func TestCursorTracksEditing(t *testing.T) {
	r := New()
	// Plain typing keeps the cursor at the end of the line.
	for _, c := range "echo AB" {
		r.Type(c)
	}
	if got := r.Cursor(); got != 7 {
		t.Fatalf("cursor after typing = %d, want 7", got)
	}

	// Arrow keys move the cursor within the line.
	r.Left()
	r.Left()
	if got := r.Cursor(); got != 5 {
		t.Fatalf("cursor after two lefts = %d, want 5", got)
	}
	r.Right()
	if got := r.Cursor(); got != 6 {
		t.Fatalf("cursor after one right = %d, want 6", got)
	}
	// Movement clamps at the bounds.
	r.CursorStart()
	if got := r.Cursor(); got != 0 {
		t.Fatalf("cursor after CursorStart = %d, want 0", got)
	}
	r.Left()
	if got := r.Cursor(); got != 0 {
		t.Fatalf("left at column 0 moved: %d", got)
	}
	r.CursorEnd()
	r.Right()
	if got := r.Cursor(); got != 7 {
		t.Fatalf("right at end moved: %d", got)
	}

	// Typing inserts at the cursor, not at the end.
	r.SetCursor(5) // before the 'A'
	r.Type('Z')
	if got, want := r.Line(), "echo ZAB"; got != want {
		t.Fatalf("line after mid-insert = %q, want %q", got, want)
	}
	if got := r.Cursor(); got != 6 {
		t.Fatalf("cursor after mid-insert = %d, want 6", got)
	}

	// Backspace deletes left of the cursor, not the last rune.
	r.Backspace()
	if got, want := r.Line(), "echo AB"; got != want {
		t.Fatalf("line after mid-backspace = %q, want %q", got, want)
	}
	if got := r.Cursor(); got != 5 {
		t.Fatalf("cursor after mid-backspace = %d, want 5", got)
	}

	// Readline bindings: ^A/^E home/end, ^B/^F char left/right.
	r.Control([]byte{0x05}) // ^E
	if got := r.Cursor(); got != 7 {
		t.Fatalf("cursor after ^E = %d, want 7", got)
	}
	r.Control([]byte{0x02}) // ^B
	r.Control([]byte{0x02})
	if got := r.Cursor(); got != 5 {
		t.Fatalf("cursor after two ^B = %d, want 5", got)
	}
	r.Control([]byte{0x01}) // ^A
	if got := r.Cursor(); got != 0 {
		t.Fatalf("cursor after ^A = %d, want 0", got)
	}
	r.Control([]byte{0x06}) // ^F
	if got := r.Cursor(); got != 1 {
		t.Fatalf("cursor after ^F = %d, want 1", got)
	}

	// SetCursor clamps into range.
	r.SetCursor(-2)
	if got := r.Cursor(); got != 0 {
		t.Fatalf("SetCursor(-2) = %d, want 0", got)
	}
	r.SetCursor(999)
	if got := r.Cursor(); got != 7 {
		t.Fatalf("SetCursor(999) = %d, want 7", got)
	}
}

func TestCursorKillWordMidLine(t *testing.T) {
	r := New()
	for _, c := range "one two three" {
		r.Type(c)
	}
	// Park the cursor between "two" and "three"; ^W removes the word left
	// of it rather than the whole line.
	r.SetCursor(8)
	r.Control([]byte{0x17})
	if got, want := r.Line(), "one three"; got != want {
		t.Fatalf("line after mid-line ^W = %q, want %q", got, want)
	}
}
