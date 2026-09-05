package screen

import (
	"os"
	"strings"
	"testing"
)

func TestShellStatus(t *testing.T) {
	s := ShellStatus(24, CursorBar, "", "")
	for _, want := range []string{
		SaveCursor,
		"\x1b[24H", // move to the bottom status row
		statusBarStyle(),
		"\x1b[K", // erase the row first (fills it with the status-bar bg)
		"[SH]",
		promptSH(), // filled green badge (theme-aware)
		BarCursor,
		RestoreCursor,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("ShellStatus(24) missing %q; got %q", want, s)
		}
	}
	// The bar background must survive the badge's own reset, so the
	// session text after the badge stays on the bar background.
	if strings.Count(s, statusBarStyle()) < 2 {
		t.Fatalf("ShellStatus should re-apply the bar style after the badge; got %q", s)
	}
	if !strings.Contains(s, ColorReset+BarCursor) {
		t.Fatalf("ShellStatus should reset the bar style before the cursor; got %q", s)
	}
}

func TestTitleSequences(t *testing.T) {
	if got := SetTitle("rysh · m · session s1"); got != "\x1b]2;rysh · m · session s1\x1b\\" {
		t.Fatalf("SetTitle = %q", got)
	}
	if got := RequestTitle(); got != "\x1b]1046;?\x1b\\" {
		t.Fatalf("RequestTitle = %q", got)
	}
}

func TestCursorStyleSeq(t *testing.T) {
	for _, tc := range []struct {
		style CursorStyle
		want  string
	}{
		{"", BarCursor}, // absent config defaults to the bar
		{CursorBar, BarCursor},
		{CursorBlock, BlockCursor},
		{CursorDefault, ""}, // terminal's own cursor left unchanged
	} {
		if got := CursorStyleSeq(tc.style); got != tc.want {
			t.Fatalf("CursorStyleSeq(%q) = %q, want %q", tc.style, got, tc.want)
		}
	}
	if s := ShellStatus(24, CursorBlock, "", ""); !strings.Contains(s, BlockCursor) || strings.Contains(s, BarCursor) {
		t.Fatalf("ShellStatus block = %q, want block cursor and no bar", s)
	}
	if s := ShellStatus(24, CursorDefault, "", ""); strings.Contains(s, " q") {
		t.Fatalf("ShellStatus default should have no DECSCUSR style; got %q", s)
	}
}

func TestScrollRegion(t *testing.T) {
	if got, want := ScrollRegion(23), "\x1b[1;23r"; got != want {
		t.Fatalf("ScrollRegion(23) = %q, want %q", got, want)
	}
	if got, want := ResetScrollRegion(), "\x1b[r"; got != want {
		t.Fatalf("ResetScrollRegion() = %q, want %q", got, want)
	}
}

func TestCrashCleanupSequences(t *testing.T) {
	if got, want := ExitAltScreen(), "\x1b[?1049l"; got != want {
		t.Fatalf("ExitAltScreen() = %q, want %q", got, want)
	}
	if got, want := ShowCursor(), "\x1b[?25h"; got != want {
		t.Fatalf("ShowCursor() = %q, want %q", got, want)
	}
	if got, want := HideCursor(), "\x1b[?25l"; got != want {
		t.Fatalf("HideCursor() = %q, want %q", got, want)
	}
}

func TestDetectorAltScreen(t *testing.T) {
	d := NewDetector()

	if d.Alt() {
		t.Fatalf("detector should start outside the alt screen")
	}

	// Entering the alt screen releases the scroll region once.
	act, seq, cut := d.Feed([]byte("\x1b[?1049h"))
	if act != ActionRelease {
		t.Fatalf("alt-screen enter: got %v, want ActionRelease", act)
	}
	if seq != "\x1b[?1049h" || cut != 0 {
		t.Fatalf("alt-screen enter: seq/cut = %q/%d, want the sequence at 0", seq, cut)
	}
	if !d.Alt() {
		t.Fatalf("detector should track alt screen as active")
	}

	// Repeated enter is a no-op.
	if act, _, _ := d.Feed([]byte("\x1b[?1049h")); act != ActionNone {
		t.Fatalf("repeated alt-screen enter: got %v, want ActionNone", act)
	}

	// Leaving the alt screen reapplies the scroll region.
	act, _, _ = d.Feed([]byte("\x1b[?1049l"))
	if act != ActionReapply {
		t.Fatalf("alt-screen exit: got %v, want ActionReapply", act)
	}
	if d.Alt() {
		t.Fatalf("detector should track alt screen as inactive")
	}

	// Exit without a prior enter is a no-op.
	d = NewDetector()
	if act, _, _ := d.Feed([]byte("\x1b[?1047l")); act != ActionNone {
		t.Fatalf("alt-screen exit w/o enter: got %v, want ActionNone", act)
	}
}

func TestDetectorAltScreenVariants(t *testing.T) {
	// The older ?1047 and ?47 forms are recognized for both enter and exit.
	for _, enter := range []string{"\x1b[?1047h", "\x1b[?47h"} {
		d := NewDetector()
		if act, seq, _ := d.Feed([]byte(enter)); act != ActionRelease || seq != enter {
			t.Fatalf("enter %q: got %v/%q, want ActionRelease", enter, act, seq)
		}
	}
	d := NewDetector()
	d.Feed([]byte("\x1b[?1047h"))
	if act, seq, _ := d.Feed([]byte("\x1b[?1047l")); act != ActionReapply || seq != "\x1b[?1047l" {
		t.Fatalf("exit: got %v/%q, want ActionReapply", act, seq)
	}
}

func TestDetectorEmbeddedSequenceCut(t *testing.T) {
	// A sequence embedded after ordinary output reports its byte offset, so
	// the caller can strip exactly the sequence and forward the rest.
	d := NewDetector()
	act, seq, cut := d.Feed([]byte("hello \x1b[?1049h"))
	if act != ActionRelease || seq != "\x1b[?1049h" || cut != 6 {
		t.Fatalf("embedded enter: act/seq/cut = %v/%q/%d, want ActionRelease/\\x1b[?1049h/6", act, seq, cut)
	}
}

func TestDetectorClears(t *testing.T) {
	for _, seq := range []string{"\x1b[2J", "\x1b[3J", "\x1b[J"} {
		d := NewDetector()
		if act, _, _ := d.Feed([]byte(seq)); act != ActionRedraw {
			t.Fatalf("clear %q: got %v, want ActionRedraw", seq, act)
		}
	}
}

func TestDetectorReset(t *testing.T) {
	// RIS (full terminal reset) reports ActionReset with the sequence.
	d := NewDetector()
	act, seq, cut := d.Feed([]byte("\x1b c"))
	if act != ActionReset || seq != "\x1b c" || cut != 0 {
		t.Fatalf("RIS: act/seq/cut = %v/%q/%d, want ActionReset/\\x1b c/0", act, seq, cut)
	}
	if d.Alt() {
		t.Fatalf("RIS should clear alt state")
	}
	if act, _, _ := d.Feed([]byte("\x1b[2J")); act != ActionRedraw {
		t.Fatalf("RIS then clear: got %v, want ActionRedraw", act)
	}
	d.Reset()
	if act, _, _ := d.Feed([]byte("\x1b[2J")); act != ActionRedraw {
		t.Fatalf("after Reset: got %v, want ActionRedraw", act)
	}
	if d.Alt() {
		t.Fatalf("Reset should clear alt state")
	}
}

func TestDetectorOrdinaryOutput(t *testing.T) {
	d := NewDetector()
	if act, _, _ := d.Feed([]byte("hello world\n$ ")); act != ActionNone {
		t.Fatalf("ordinary output: got %v, want ActionNone", act)
	}
}

// Feed (output loop) and Reset (driver loop) run on different goroutines; a
// concurrent mix must be race-free and must never panic.
func TestDetectorConcurrentFeedReset(t *testing.T) {
	d := NewDetector()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			d.Feed([]byte("\x1b[?1049h plain output \x1b[2J"))
			d.Alt()
		}
	}()
	for i := 0; i < 2000; i++ {
		d.Reset()
	}
	<-done
}

func TestDetectorSplitSequence(t *testing.T) {
	d := NewDetector()
	// The alt-screen enter arrives split across two chunks.
	if act, _, _ := d.Feed([]byte("\x1b[?104")); act != ActionNone {
		t.Fatalf("partial sequence: got %v, want ActionNone", act)
	}
	// The completing chunk holds only a partial tail of the sequence, so
	// cut is -1: the caller forwards it whole and recovers in place.
	act, seq, cut := d.Feed([]byte("9h"))
	if act != ActionRelease {
		t.Fatalf("completed sequence: got %v, want ActionRelease", act)
	}
	if seq != "\x1b[?1049h" || cut != -1 {
		t.Fatalf("completed sequence: seq/cut = %q/%d, want the sequence with cut -1", seq, cut)
	}
}

// The real-world vim case: sequences land in the middle of a chunk, not at
// its tail. On enter, the app's first repaint follows the sequence; on
// exit, the shell's prompt follows it in the same read. Both must be
// detected, since rysh forwards every byte to the terminal anyway.
func TestDetectorSequenceMidChunk(t *testing.T) {
	d := NewDetector()
	act, seq, cut := d.Feed([]byte("echo vi\r\n\x1b[?1049h" + strings.Repeat("A", 200)))
	if act != ActionRelease || seq != "\x1b[?1049h" || cut != 9 {
		t.Fatalf("mid-chunk enter: act/seq/cut = %v/%q/%d, want ActionRelease/\\x1b[?1049h/9", act, seq, cut)
	}
	if !d.Alt() {
		t.Fatalf("detector should track alt screen as active")
	}
	// The app clears and repaints its own screen while it owns the alt
	// screen; those clears wipe the status row too, so the session loop
	// must repaint it (tmux-style: row h is always rysh's, never the
	// fullscreen app's).
	if act, _, _ := d.Feed([]byte("\x1b[2J\x1b[H" + strings.Repeat("x", 40))); act != ActionRedraw {
		t.Fatalf("clear while alt: got %v, want ActionRedraw", act)
	}
	// Exit: the shell's prompt follows the sequence in the same chunk.
	act, seq, cut = d.Feed([]byte("\x1b[?1049luser@host:~$ "))
	if act != ActionReapply || seq != "\x1b[?1049l" || cut != 0 {
		t.Fatalf("mid-chunk exit: act/seq/cut = %v/%q/%d, want ActionReapply/\\x1b[?1049l/0", act, seq, cut)
	}
	if d.Alt() {
		t.Fatalf("detector should track alt screen as inactive")
	}
}

// A fullscreen app that enters and leaves within one chunk ends back on the
// normal screen: reapply, so the scroll region and status row are re-asserted
// (terminals may change the region across alt-screen transitions).
func TestDetectorEnterAndExitOneChunk(t *testing.T) {
	d := NewDetector()
	act, _, _ := d.Feed([]byte("\x1b[?1049hfullscreen output\x1b[?1049l$ "))
	if act != ActionReapply {
		t.Fatalf("enter+exit in one chunk: got %v, want ActionReapply", act)
	}
	if d.Alt() {
		t.Fatalf("detector should track alt screen as inactive")
	}
}

// A carried partial that the next chunk does not complete must not fire.
func TestDetectorCarryNoFalsePositive(t *testing.T) {
	d := NewDetector()
	if act, _, _ := d.Feed([]byte("\x1b[?104")); act != ActionNone {
		t.Fatalf("partial sequence: got %v, want ActionNone", act)
	}
	// "9x" completes the digits but not the sequence (needs h or l).
	if act, _, _ := d.Feed([]byte("9x more output\n")); act != ActionNone {
		t.Fatalf("non-completing carry: got %v, want ActionNone", act)
	}
	// The detector is still armed for a later, genuine completion.
	if act, _, _ := d.Feed([]byte("plain output\n\x1b[2J")); act != ActionRedraw {
		t.Fatalf("clear after stray carry: got %v, want ActionRedraw", act)
	}
}

func TestAIStatus(t *testing.T) {
	s := AIStatus(24, "s1", "ollama/llama3.1:8b", "running", "⟳", "thinking")
	for _, want := range []string{
		SaveCursor,
		"\x1b[24H",
		statusBarStyle(),
		"\x1b[K",
		promptAI(), // filled magenta badge (theme-aware)
		"s1",
		"ollama/llama3.1:8b",
		"running",
		"⟳",
		"thinking",
		BlockCursor,
		RestoreCursor,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("AIStatus missing %q; got %q", want, s)
		}
	}
	// The bar background must survive the badge's own reset, so the text
	// after the badge stays on the bar background.
	if strings.Count(s, statusBarStyle()) < 2 {
		t.Fatalf("AIStatus should re-apply the bar style after the badge; got %q", s)
	}
	if !strings.Contains(s, ColorReset+BlockCursor) {
		t.Fatalf("AIStatus should reset the bar style before the cursor; got %q", s)
	}
	// An empty message and spinner yield no separators.
	short := AIStatus(24, "", "m", "idle", "", "")
	if strings.Contains(short, "│") {
		t.Fatalf("AIStatus with empty message still has a separator: %q", short)
	}
}

func TestClearScreen(t *testing.T) {
	if got, want := ClearScreen(), "\x1b[2J\x1b[1;1H"; got != want {
		t.Fatalf("ClearScreen() = %q, want %q", got, want)
	}
}

func TestApprovalBar(t *testing.T) {
	text := "? 运行 echo hi（y 是 / n 否 / a 总是）"
	got := ApprovalBar(text)
	// Bold black on amber, then the erase that fills the rest of the row
	// with the bar background (BCE), then a reset so the next line is plain.
	want := approvalStyle() + text + "\x1b[K" + ColorReset
	if got != want {
		t.Fatalf("ApprovalBar() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, approvalStyle()) {
		t.Fatalf("ApprovalBar must open with the bar style, got %q", got)
	}
	if !strings.Contains(got, EraseToEOL()) {
		t.Fatalf("ApprovalBar must erase to the right margin so the bar spans the row: %q", got)
	}
	if strings.Contains(got[:strings.Index(got, EraseToEOL())], ColorReset) {
		t.Fatalf("ApprovalBar must not reset the bar style before EraseToEOL: %q", got)
	}
}

func TestCursorHelpers(t *testing.T) {
	if got := CursorUp(0); got != "" {
		t.Fatalf("CursorUp(0) = %q, want empty", got)
	}
	if got := CursorUp(3); got != "\x1b[3A" {
		t.Fatalf("CursorUp(3) = %q", got)
	}
	if got := CursorDown(2); got != "\x1b[2B" {
		t.Fatalf("CursorDown(2) = %q", got)
	}
	if got := CursorForward(5); got != "\x1b[5C" {
		t.Fatalf("CursorForward(5) = %q", got)
	}
	if got := CursorForward(0); got != "" {
		t.Fatalf("CursorForward(0) = %q, want empty", got)
	}
}

func TestPSExpand(t *testing.T) {
	// \u and \h expand to non-empty values.
	s, w := PSExpand(`\u@\h: \w > `, "/home/user/work")
	if !strings.Contains(s, "@") {
		t.Fatalf("PSExpand did not expand \\u@\\h: %q", s)
	}
	if w <= 0 {
		t.Fatalf("PSExpand width = %d, want > 0", w)
	}

	// HOME is abbreviated to ~.
	home, err := osUserHome()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	s, w = PSExpand(`\w`, home)
	if s != "~" || w != 1 {
		t.Fatalf(`PSExpand(\w, HOME) = %q/%d, want "~"/1`, s, w)
	}
	s, _ = PSExpand(`\w`, home+"/sub")
	if s != "~/sub" {
		t.Fatalf(`PSExpand(\w, HOME/sub) = %q, want "~/sub"`, s)
	}

	// \t expands to HH:MM:SS.
	s, _ = PSExpand(`\t`, "")
	if len(s) != 8 || s[2] != ':' || s[5] != ':' {
		t.Fatalf(`PSExpand(\t) = %q, want HH:MM:SS`, s)
	}

	// \\ is a literal backslash.
	if s, _ := PSExpand(`a\\b`, ""); s != `a\b` {
		t.Fatalf(`PSExpand(a\\b) = %q, want a\b`, s)
	}

	// \x1b (and \e) spell the ESC byte, so SGR from config text or the
	// raw-string default renders as a real escape sequence, not literal
	// text.
	s, w = PSExpand(`\x1b[1;35m[AI]\x1b[0m hi`, "")
	if s != "\x1b[1;35m[AI]\x1b[0m hi" {
		t.Fatalf(`PSExpand(\x1b...) = %q, want real ESC bytes`, s)
	}
	if w != 7 { // "[AI] hi"
		t.Fatalf(`PSExpand(\x1b...) width = %d, want 7`, w)
	}
	s, _ = PSExpand(`\e[1;35m[AI]\e[0m hi`, "")
	if s != "\x1b[1;35m[AI]\x1b[0m hi" {
		t.Fatalf(`PSExpand(\e...) = %q, want real ESC bytes`, s)
	}
	s2, w2 := PSExpand("\\[\x1b[1;35m[AI]\x1b[0m\\] hi", "")
	if !strings.Contains(s2, "\x1b[1;35m") {
		t.Fatalf("non-printing region dropped: %q", s2)
	}
	if w2 != 3 {
		t.Fatalf("non-printing width = %d, want 3 for \" hi\"", w2)
	}

	// SGR in the visible part adds no width, wide runes count double.
	_, w3 := PSExpand("\x1b[31m你好\x1b[0m", "")
	if w3 != 4 {
		t.Fatalf("wide-rune width = %d, want 4", w3)
	}
}

func osUserHome() (string, error) {
	return os.UserHomeDir()
}
