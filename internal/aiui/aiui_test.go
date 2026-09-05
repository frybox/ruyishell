package aiui

import (
	"regexp"
	"strings"
	"testing"

	"ruyishell/internal/keys"
)

// typed sends each rune of s through HandleKey and fails on a non-None action.
func typed(t *testing.T, m *Model, s string) {
	t.Helper()
	for _, r := range s {
		if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: r}); act != ActionNone {
			t.Fatalf("typing %q returned %v, want ActionNone", r, act)
		}
	}
}

func TestPasteMultiLine(t *testing.T) {
	m := New()
	// A bracketed-paste block with an embedded newline. The markers plus
	// content are fed event by event exactly as keys decodes them.
	for _, ev := range []keys.Event{
		{Kind: keys.PasteStart},
		{Kind: keys.Rune, R: 'a'}, {Kind: keys.Rune, R: 'b'},
		{Kind: keys.Enter},
		{Kind: keys.Rune, R: 'c'}, {Kind: keys.Rune, R: 'd'},
		{Kind: keys.PasteEnd},
	} {
		if act := m.HandleKey(ev); act != ActionNone {
			t.Fatalf("paste event %d returned %v, want ActionNone", ev, act)
		}
	}
	if got := m.Draft(); got != "ab\ncd" {
		t.Fatalf("draft after paste = %q, want %q", got, "ab\ncd")
	}
	// The embedded newline did not submit, so the draft still holds it.
	// A single ^Z must undo the whole paste block.
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x1a}}); act != ActionNone {
		t.Fatalf("^Z returned %v", act)
	}
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after undo = %q, want empty", got)
	}
}

func TestPasteIsAtomic(t *testing.T) {
	m := New()
	typed(t, m, "hi")
	// Paste after existing text: inserted at the cursor, one undo step.
	for _, ev := range []keys.Event{
		{Kind: keys.PasteStart},
		{Kind: keys.Rune, R: 'x'}, {Kind: keys.Enter}, {Kind: keys.Rune, R: 'y'},
		{Kind: keys.PasteEnd},
	} {
		m.HandleKey(ev)
	}
	if got := m.Draft(); got != "hix\ny" {
		t.Fatalf("draft = %q, want %q", got, "hix\ny")
	}
	// Ctrl+Z twice: first undoes the whole paste, second undoes nothing
	// extra (the pre-paste "hi" was a separate snapshot chain).
	m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x1a}})
	if got := m.Draft(); got != "hi" {
		t.Fatalf("draft after one undo = %q, want %q", got, "hi")
	}
}

func TestNewDefaults(t *testing.T) {
	m := New()
	if m.Draft() != "" {
		t.Fatalf("draft = %q, want empty", m.Draft())
	}
	if m.Streaming() {
		t.Fatalf("new model should not be streaming")
	}
}

func TestDraftEditing(t *testing.T) {
	m := New()
	typed(t, m, "hi")
	if got := m.Draft(); got != "hi" {
		t.Fatalf("draft = %q, want %q", got, "hi")
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Backspace}); act != ActionNone {
		t.Fatalf("backspace returned %v", act)
	}
	if got := m.Draft(); got != "h" {
		t.Fatalf("draft after backspace = %q, want %q", got, "h")
	}
	typed(t, m, "ello")
	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	typed(t, m, "x")
	if got := m.Draft(); got != "xhello" {
		t.Fatalf("draft after home+insert = %q, want %q", got, "xhello")
	}
	if act := m.HandleKey(keys.Event{Kind: keys.ArrowRight}); act != ActionNone {
		t.Fatalf("arrow-right returned %v", act)
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x05}}); act != ActionNone { // ^E
		t.Fatalf("^E returned %v", act)
	}
	typed(t, m, "!")
	if got := m.Draft(); got != "xhello!" {
		t.Fatalf("draft after ^E+insert = %q, want %q", got, "xhello!")
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x15}}); act != ActionNone { // ^U
		t.Fatalf("^U returned %v", act)
	}
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after ^U = %q, want empty", got)
	}
}

func TestSetDraft(t *testing.T) {
	m := New()
	typed(t, m, "old")
	m.RenderInput(func(s string) {}, 40) // commit a render so dirty clears

	// SetDraft replaces the whole draft (text seeded from the shared shell
	// line), puts the cursor at the given position (clamped), and clears
	// the undo history: the text was composed in the shell, so it has no
	// editable history here.
	m.SetDraft("shared line", len("shared line"))
	if got := m.Draft(); got != "shared line" {
		t.Fatalf("draft after SetDraft = %q, want %q", got, "shared line")
	}
	if got := m.Pos(); got != len("shared line") {
		t.Fatalf("cursor after SetDraft = %d, want end (%d)", got, len("shared line"))
	}
	if len(m.undo) != 0 {
		t.Fatalf("undo not cleared by SetDraft: %d entries", len(m.undo))
	}
	// Undo after SetDraft is a no-op: the seeded text has no history.
	pressUndo(m)
	if got := m.Draft(); got != "shared line" {
		t.Fatalf("draft changed by undo after SetDraft: %q", got)
	}
	// The next render must be a full redraw (dirty).
	if !m.Dirty() {
		t.Fatalf("SetDraft should mark the editor dirty")
	}
	// Editing the seeded draft works normally from the end.
	typed(t, m, "!")
	if got := m.Draft(); got != "shared line!" {
		t.Fatalf("draft after typing into seeded text = %q, want %q", got, "shared line!")
	}

	// A mid-line position is kept: text typed after the seed inserts at the
	// cursor, not at the end.
	m.SetDraft("echo AB", 5) // before the 'A'
	if got := m.Pos(); got != 5 {
		t.Fatalf("cursor after mid SetDraft = %d, want 5", got)
	}
	typed(t, m, "Z")
	if got := m.Draft(); got != "echo ZAB" {
		t.Fatalf("draft after typing into mid-seeded text = %q, want %q", got, "echo ZAB")
	}

	// Positions outside the text are clamped into range.
	m.SetDraft("hi", 99)
	if got := m.Pos(); got != 2 {
		t.Fatalf("cursor after oversized SetDraft = %d, want 2", got)
	}
	m.SetDraft("hi", -3)
	if got := m.Pos(); got != 0 {
		t.Fatalf("cursor after negative SetDraft = %d, want 0", got)
	}

	// An empty SetDraft clears the draft (the shell line was empty).
	m.SetDraft("", 0)
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after empty SetDraft = %q, want empty", got)
	}
}

// pressUndo sends Ctrl+Z (^Z / 0x1a) to the draft editor.
func pressUndo(m *Model) {
	_ = m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x1a}})
}

func TestUndoDraft(t *testing.T) {
	m := New()
	typed(t, m, "hi")
	pressUndo(m)
	if got := m.Draft(); got != "h" {
		t.Fatalf("draft after undo = %q, want h", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after 2x undo = %q, want empty", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after undo on empty = %q, want empty", got)
	}
}

func TestUndoRestoresCursor(t *testing.T) {
	m := New()
	typed(t, m, "abc")
	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	typed(t, m, "x")
	if got := m.Draft(); got != "xabc" {
		t.Fatalf("draft = %q, want xabc", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "abc" {
		t.Fatalf("draft after undo = %q, want abc", got)
	}
	if m.pos != 0 {
		t.Fatalf("cursor after undo = %d, want 0 (front)", m.pos)
	}
}

func TestUndoBackspace(t *testing.T) {
	m := New()
	typed(t, m, "hi")
	if act := m.HandleKey(keys.Event{Kind: keys.Backspace}); act != ActionNone {
		t.Fatalf("backspace returned %v", act)
	}
	if got := m.Draft(); got != "h" {
		t.Fatalf("draft after backspace = %q, want h", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "hi" {
		t.Fatalf("draft after undo backspace = %q, want hi", got)
	}
}

func TestUndoClear(t *testing.T) {
	m := New()
	typed(t, m, "a")
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x15}}); act != ActionNone { // ^U
		t.Fatalf("^U returned %v", act)
	}
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after ^U = %q, want empty", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "a" {
		t.Fatalf("draft after undo ^U = %q, want a", got)
	}

	// Esc with a non-empty draft clears it; undo restores it.
	m2 := New()
	typed(t, m2, "x")
	if act := m2.HandleKey(keys.Event{Kind: keys.Esc}); act != ActionNone {
		t.Fatalf("Esc with draft returned %v, want ActionNone", act)
	}
	if got := m2.Draft(); got != "" {
		t.Fatalf("draft after Esc = %q, want empty", got)
	}
	pressUndo(m2)
	if got := m2.Draft(); got != "x" {
		t.Fatalf("draft after undo Esc = %q, want x", got)
	}
}

func TestUndoClearedOnSubmit(t *testing.T) {
	m := New()
	typed(t, m, "ok")
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionSubmit {
		t.Fatalf("Enter returned %v, want ActionSubmit", act)
	}
	var out strings.Builder
	m.Submit(func(s string) { out.WriteString(s) }, 40)
	m.EndStream()
	typed(t, m, "a")
	pressUndo(m)
	if got := m.Draft(); got != "" {
		t.Fatalf("draft after undo post-submit = %q, want empty (history cleared)", got)
	}
}

func TestUndoBounded(t *testing.T) {
	m := New()
	for i := 0; i < maxUndo+10; i++ {
		typed(t, m, string(rune('a'+i%26)))
	}
	if len(m.undo) != maxUndo {
		t.Fatalf("undo depth = %d, want %d", len(m.undo), maxUndo)
	}
	for i := 0; i < maxUndo; i++ {
		pressUndo(m)
	}
	if len(m.undo) != 0 {
		t.Fatalf("undo not exhausted: %d remaining", len(m.undo))
	}
	if got := len(m.draft); got != 10 {
		t.Fatalf("draft length after maxUndo undos = %d, want 10", got)
	}
}

func TestEscAndCtrlTab(t *testing.T) {
	m := New()
	// Esc no longer leaves AI mode: with an empty draft it is a no-op.
	if act := m.HandleKey(keys.Event{Kind: keys.Esc}); act != ActionNone {
		t.Fatalf("Esc empty draft = %v, want ActionNone", act)
	}
	// A leading space on an empty draft returns to shell mode.
	if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: ' '}); act != ActionLeave {
		t.Fatalf("leading space on empty draft = %v, want ActionLeave", act)
	}
	// Space inside a draft is a normal character, not a mode switch.
	m3 := New()
	typed(t, m3, "a")
	if act := m3.HandleKey(keys.Event{Kind: keys.Rune, R: ' '}); act != ActionNone {
		t.Fatalf("space in draft = %v, want ActionNone", act)
	}
	if got := m3.Draft(); got != "a " {
		t.Fatalf("draft after space = %q, want %q", got, "a ")
	}
	// Ctrl-Tab always leaves, keeping the draft for injection.
	typed(t, m, "ls")
	if act := m.HandleKey(keys.Event{Kind: keys.CtrlTab}); act != ActionLeave {
		t.Fatalf("Ctrl-Tab = %v, want ActionLeave", act)
	}
	if got := m.Draft(); got != "ls" {
		t.Fatalf("draft after Ctrl-Tab = %q, want %q (kept for injection)", got, "ls")
	}
	// Esc with a non-empty draft only clears it.
	m2 := New()
	typed(t, m2, "x")
	if act := m2.HandleKey(keys.Event{Kind: keys.Esc}); act != ActionNone {
		t.Fatalf("Esc with draft = %v, want ActionNone", act)
	}
	if got := m2.Draft(); got != "" {
		t.Fatalf("draft after Esc = %q, want empty", got)
	}
}

func TestSubmit(t *testing.T) {
	m := New()
	// Enter with an empty draft is a no-op.
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionNone {
		t.Fatalf("empty draft Enter = %v, want ActionNone", act)
	}
	// Enter submits a non-empty draft.
	typed(t, m, "hi")
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionSubmit {
		t.Fatalf("Enter = %v, want ActionSubmit", act)
	}
	var out strings.Builder
	if got := m.Submit(func(s string) { out.WriteString(s) }, 40); got != "hi" {
		t.Fatalf("Submit returned %q, want %q", got, "hi")
	}
	if m.Draft() != "" {
		t.Fatalf("draft not cleared after submit: %q", m.Draft())
	}
	// Submit alone does not lock the editor; the driver calls StartStream
	// when it actually launches the task.
	if m.Streaming() {
		t.Fatalf("Submit should not set streaming by itself")
	}
	m.StartStream()
	if !m.Streaming() {
		t.Fatalf("StartStream should lock the editor")
	}
}

func TestStreamingLock(t *testing.T) {
	m := New()
	typed(t, m, "hello")
	m.StartStream()
	// Rune, Enter and Ctrl-Tab are ignored while streaming.
	if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: 'x'}); act != ActionNone {
		t.Fatalf("rune during streaming = %v, want ActionNone", act)
	}
	if got := m.Draft(); got != "hello" {
		t.Fatalf("draft changed while streaming: %q", got)
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionNone {
		t.Fatalf("Enter during streaming = %v, want ActionNone", act)
	}
	if act := m.HandleKey(keys.Event{Kind: keys.CtrlTab}); act != ActionNone {
		t.Fatalf("Ctrl-Tab during streaming = %v, want ActionNone", act)
	}
	// ^C cancels the in-flight task.
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x03}}); act != ActionCancel {
		t.Fatalf("^C during streaming = %v, want ActionCancel", act)
	}
	// After EndStream the editor accepts input again.
	m.EndStream()
	typed(t, m, "a")
	if got := m.Draft(); got != "helloa" {
		t.Fatalf("draft after stream end = %q, want helloa", got)
	}
}

func TestApprovalKeysWhileStreaming(t *testing.T) {
	m := New()
	m.StartStream()

	// Without a pending approval, y/n/a stay locked out like any key.
	for _, r := range []rune{'y', 'n', 'a'} {
		if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: r}); act != ActionNone {
			t.Fatalf("rune %q while streaming without approval = %v, want ActionNone", r, act)
		}
	}

	// Pending approval: y/n/a route to the three answer actions,
	// case-insensitively; other keys stay ignored.
	m.SetApproval(true)
	cases := map[rune]Action{'y': ActionApproveYes, 'Y': ActionApproveYes,
		'n': ActionApproveNo, 'N': ActionApproveNo,
		'a': ActionApproveAlways, 'A': ActionApproveAlways}
	for r, want := range cases {
		if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: r}); act != want {
			t.Fatalf("approval rune %q = %v, want %v", r, act, want)
		}
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: 'x'}); act != ActionNone {
		t.Fatalf("rune x during approval = %v, want ActionNone", act)
	}
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionNone {
		t.Fatalf("Enter during approval = %v, want ActionNone", act)
	}
	// ^C still cancels alongside an open prompt (deny + task cancel).
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x03}}); act != ActionCancel {
		t.Fatalf("^C during approval = %v, want ActionCancel", act)
	}

	// The answer keys never leak into the draft, and closing the window
	// restores the plain streaming lock.
	if got := m.Draft(); got != "" {
		t.Fatalf("draft changed during approval: %q", got)
	}
	m.SetApproval(false)
	if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: 'y'}); act != ActionNone {
		t.Fatalf("rune y after approval closed = %v, want ActionNone", act)
	}
}

func TestRenderSingleLine(t *testing.T) {
	m := New()
	m.BeginInput("> ", 2)
	typed(t, m, "hi")
	var out strings.Builder
	m.RenderInput(func(s string) { out.WriteString(s) }, 40)
	got := out.String()
	if !strings.Contains(got, "> hi") {
		t.Fatalf("render missing prompt+draft: %q", got)
	}
	if !strings.HasSuffix(got, "\x1b[4C") {
		t.Fatalf("render should move the cursor to column 4: %q", got)
	}
	if m.prevRows != 1 || m.prevCursorRow != 0 {
		t.Fatalf("layout = rows %d cursorRow %d, want 1/0", m.prevRows, m.prevCursorRow)
	}
}

func TestRenderWraps(t *testing.T) {
	m := New()
	m.SetSize(5, 24)
	m.BeginInput("> ", 2)
	typed(t, m, "abcdef")
	var out strings.Builder
	m.RenderInput(func(s string) { out.WriteString(s) }, 0) // use the stored width
	got := out.String()
	if !strings.Contains(got, "> abcdef") {
		t.Fatalf("render missing prompt+draft: %q", got)
	}
	// 8 cells over a 5-column width -> 2 rows, cursor at row 2, column 3.
	if m.prevRows != 2 || m.prevCursorRow != 1 {
		t.Fatalf("layout = rows %d cursorRow %d, want 2/1", m.prevRows, m.prevCursorRow)
	}
	if !strings.HasSuffix(got, "\r\x1b[3C") {
		t.Fatalf("render should end with cursor to column 3: %q", got)
	}
}

// TestRenderCJKRowBoundary pins the layout when a wide (CJK) character
// crosses a row boundary or exactly fills one. The linear total/cols model
// drifts one column per wrapped row in that case, so the cursor and the
// remembered row must match the per-rune terminal wrap.
func TestRenderCJKRowBoundary(t *testing.T) {
	// 19 CJK (2 cells each) + prompt width 2 = 40 cells = exactly one
	// 40-column row. The cursor belongs at the start of the next row.
	m := New()
	m.SetSize(40, 24)
	m.BeginInput("> ", 2)
	typed(t, m, strings.Repeat("中", 19))
	var out strings.Builder
	m.RenderInput(func(s string) { out.WriteString(s) }, 40)
	got := out.String()
	if !strings.Contains(got, strings.Repeat("中", 2)) {
		t.Fatalf("render missing draft: %q", got)
	}
	if m.prevRows != 1 || m.prevCursorRow != 1 {
		t.Fatalf("layout = rows %d cursorRow %d, want 1/1 (full row)", m.prevRows, m.prevCursorRow)
	}
	// One more char pushes past the boundary onto row 1.
	typed(t, m, "中")
	out.Reset()
	m.RenderInput(func(s string) { out.WriteString(s) }, 40)
	if m.prevRows != 2 || m.prevCursorRow != 1 {
		t.Fatalf("after wrap layout = rows %d cursorRow %d, want 2/1", m.prevRows, m.prevCursorRow)
	}
}

// TestRenderMixedCJKDrift pins that a single-width and wide character mix
// does not accumulate a per-row column offset. The cursor must land where
// the terminal actually wraps, not where a flat total/cols modulo says.
func TestRenderMixedCJKDrift(t *testing.T) {
	m := New()
	m.SetSize(80, 24)
	m.BeginInput("xxxxxxxxxx", 10)
	// 72 chars: 8 repeats of "中文混合abcde" (13 cells) = 104 cells over
	// 80 columns. The naive total/cols puts the cursor at row 1 col 34,
	// but the true wrap (a wide char that cannot fit wastes a cell) is row 1 col 35.
	typed(t, m, "中文混合abcde中文混合abcde中文混合abcde中文混合abcde中文混合abcde中文混合abcde中文混合abcde中文混合abcde")
	var out strings.Builder
	m.RenderInput(func(s string) { out.WriteString(s) }, 80)
	got := out.String()
	// The cursor must not be left at the drift-prone column 34.
	if strings.HasSuffix(got, "\r\x1b[34C") {
		t.Fatalf("cursor drifted to column 34: %q", got)
	}
	if m.prevCursorRow != 1 {
		t.Fatalf("cursorRow = %d, want 1", m.prevCursorRow)
	}
}

// TestRenderBoundaryNoGhostRedraw pins the "重行" symptom: after the draft
// exactly fills a whole row and the user keeps typing, every redraw must
// use only relative moves and erase exactly the rows the previous frame
// drew, so no line is ever left duplicated on screen. The invariant this
// relies on is that prevCursorRow equals the row the cursor really sits on
// after each frame (deferred wrap would otherwise make it one too high).
func TestRenderBoundaryNoGhostRedraw(t *testing.T) {
	m := New()
	m.SetSize(40, 24)
	m.BeginInput("> ", 2)
	typed(t, m, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 38 a -> total 40, full row
	var out strings.Builder
	write := func(s string) { out.WriteString(s) }
	m.RenderInput(write, 40)
	if m.prevRows != 1 || m.prevCursorRow != 1 {
		t.Fatalf("at boundary layout = rows %d cursorRow %d, want 1/1", m.prevRows, m.prevCursorRow)
	}

	typed(t, m, "b") // crosses the boundary onto row 2
	m.RenderInput(write, 40)
	// 41 cells over 40 columns -> 2 rows, cursor at row 1 column 1.
	if m.prevRows != 2 || m.prevCursorRow != 1 {
		t.Fatalf("after crossing layout = rows %d cursorRow %d, want 2/1", m.prevRows, m.prevCursorRow)
	}
	// The whole typed session must never address absolute rows or clear the
	// screen: every redraw stays inside the block.
	if got := cupPosRe.FindString(out.String()); got != "" {
		t.Fatalf("redraw used absolute addressing: %q", got)
	}
}

// cupPosRe matches absolute cursor addressing (CUP: row;col H, or its f
// variant). The input block must never emit one; see
// TestRenderInputStaysRelative.
var cupPosRe = regexp.MustCompile(`\x1b\[[0-9;]*[Hf]`)

// The main-screen invariant behind §7.10: every write the input block makes
// addresses only the rows of the block it drew last time, through relative
// moves. Nothing pins that block to a known screen row (no scroll region, no
// status row), so an absolute cursor position — or a screen clear — would
// rewrite history that has already scrolled away instead of the input line.
// Checked across a first draw, a wrapped redraw, cursor-only moves, and the
// erase that hands the row back.
func TestRenderInputStaysRelative(t *testing.T) {
	m := New()
	m.SetSize(5, 24)
	m.BeginInput("> ", 2)

	var out strings.Builder
	write := func(s string) { out.WriteString(s) }
	// take returns the writes since the last take and checks they stayed
	// inside the block.
	take := func(label string) string {
		t.Helper()
		s := out.String()
		out.Reset()
		if got := cupPosRe.FindString(s); got != "" {
			t.Fatalf("%s addressed an absolute row: %q in %q", label, got, s)
		}
		for _, seq := range []string{"\x1b[2J", "\x1b[3J", "\x1b[1;23r"} {
			if strings.Contains(s, seq) {
				t.Fatalf("%s emitted %q: %q", label, seq, s)
			}
		}
		return s
	}

	m.RenderInput(write, 0) // cols < 1 falls back to the size from SetSize
	take("first draw")

	typed(t, m, "abcdef") // 8 cells over 5 columns wraps to a second row
	m.RenderInput(write, 0)
	if got := take("wrapped redraw"); !strings.Contains(got, "> abcdef") {
		t.Fatalf("wrapped redraw missing the draft: %q", got)
	}

	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	m.RenderInput(write, 0)
	take("cursor move to another row")

	if act := m.HandleKey(keys.Event{Kind: keys.End}); act != ActionNone {
		t.Fatalf("end returned %v", act)
	}
	m.RenderInput(write, 0)
	take("cursor move back")

	m.EraseInput(write)
	take("erase")
}

func TestEraseInput(t *testing.T) {
	m := New()
	m.SetSize(5, 24)
	m.BeginInput("> ", 2)
	typed(t, m, "abcdef")
	m.RenderInput(func(s string) {}, 0)
	if m.prevRows != 2 {
		t.Fatalf("setup: prevRows = %d, want 2", m.prevRows)
	}
	var out strings.Builder
	m.EraseInput(func(s string) { out.WriteString(s) })
	got := out.String()
	if strings.Count(got, "\x1b[2K") != 2 {
		t.Fatalf("erase should clear 2 rows: %q", got)
	}
	if !strings.HasSuffix(got, "\r") {
		t.Fatalf("erase should end at the block top: %q", got)
	}
	if m.prevRows != 0 || m.prevCursorRow != 0 {
		t.Fatalf("erase should reset the layout: %d/%d", m.prevRows, m.prevCursorRow)
	}
}

func TestDirtyTracking(t *testing.T) {
	m := New()
	m.BeginInput("> ", 2)
	if !m.Dirty() {
		t.Fatalf("BeginInput should mark the editor dirty")
	}
	m.RenderInput(func(s string) {}, 40)
	if m.Dirty() {
		t.Fatalf("RenderInput should clear the dirty flag")
	}
	// Typing commits text and marks dirty.
	typed(t, m, "hi")
	if !m.Dirty() {
		t.Fatalf("typing should mark the editor dirty")
	}
	m.RenderInput(func(s string) {}, 40)
	if m.Dirty() {
		t.Fatalf("RenderInput should clear the dirty flag")
	}
	// A key that changes nothing leaves the editor clean.
	if act := m.HandleKey(keys.Event{Kind: keys.ArrowRight}); act != ActionNone { // pos already at end
		t.Fatalf("arrow-right returned %v", act)
	}
	if m.Dirty() {
		t.Fatalf("no-op key should not mark the editor dirty")
	}
	// A cursor-only move marks dirty.
	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	if !m.Dirty() {
		t.Fatalf("cursor move should mark the editor dirty")
	}
}

func TestDirtyLockedWhileStreaming(t *testing.T) {
	m := New()
	m.BeginInput("> ", 2)
	typed(t, m, "hi")
	m.RenderInput(func(s string) {}, 40)
	m.StartStream()
	// Keys ignored while streaming do not dirty the editor.
	if act := m.HandleKey(keys.Event{Kind: keys.Rune, R: 'x'}); act != ActionNone {
		t.Fatalf("rune during streaming returned %v", act)
	}
	if act := m.HandleKey(keys.Event{Kind: keys.ArrowLeft}); act != ActionNone {
		t.Fatalf("arrow-left during streaming returned %v", act)
	}
	if m.Dirty() {
		t.Fatalf("keys during streaming must not dirty the editor")
	}
	if got := m.Draft(); got != "hi" {
		t.Fatalf("draft changed while streaming: %q", got)
	}
}

func TestRenderCursorOnlyMove(t *testing.T) {
	m := New()
	m.SetSize(5, 24)
	m.BeginInput("> ", 2)
	typed(t, m, "abcdef")
	m.RenderInput(func(s string) {}, 0) // full draw: cursor at row 1, col 3
	if m.prevRows != 2 || m.prevCursorRow != 1 {
		t.Fatalf("setup: layout = rows %d cursorRow %d, want 2/1", m.prevRows, m.prevCursorRow)
	}
	// Arrow-left moves the cursor without erasing or rewriting the line.
	if act := m.HandleKey(keys.Event{Kind: keys.ArrowLeft}); act != ActionNone {
		t.Fatalf("arrow-left returned %v", act)
	}
	var out strings.Builder
	m.RenderInput(func(s string) { out.WriteString(s) }, 0)
	if got := out.String(); got != "\r\x1b[2C" {
		t.Fatalf("cursor-only render = %q, want cursor to row 1 col 2", got)
	}
	if strings.Contains(out.String(), "\x1b[2K") {
		t.Fatalf("cursor-only render must not erase rows: %q", out.String())
	}
	if strings.Contains(out.String(), "> abcdef") {
		t.Fatalf("cursor-only render must not rewrite the line: %q", out.String())
	}
	if m.Dirty() {
		t.Fatalf("render should clear the dirty flag")
	}
	// Home moves up a row with a pure cursor move.
	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	out.Reset()
	m.RenderInput(func(s string) { out.WriteString(s) }, 0)
	if got := out.String(); got != "\x1b[1A\r\x1b[2C" {
		t.Fatalf("cursor-only home render = %q, want up + cursor to row 0 col 2", got)
	}
}

func TestSubmitRendersCursorAtEnd(t *testing.T) {
	m := New()
	m.BeginInput("> ", 2)
	typed(t, m, "abc")
	m.RenderInput(func(s string) {}, 40)
	if act := m.HandleKey(keys.Event{Kind: keys.Home}); act != ActionNone {
		t.Fatalf("home returned %v", act)
	}
	m.RenderInput(func(s string) {}, 40)
	if act := m.HandleKey(keys.Event{Kind: keys.Enter}); act != ActionSubmit {
		t.Fatalf("enter returned %v, want ActionSubmit", act)
	}
	var out strings.Builder
	if got := m.Submit(func(s string) { out.WriteString(s) }, 40); got != "abc" {
		t.Fatalf("submit returned %q, want abc", got)
	}
	// The cursor ends at the end of the committed text (col 5), so the
	// driver's following \r\n starts on the line under the full text.
	if !strings.HasSuffix(out.String(), "\x1b[5C") {
		t.Fatalf("submit should leave the cursor at the end: %q", out.String())
	}
	if m.Dirty() {
		t.Fatalf("submit should leave the editor clean")
	}
}

// pressTab sends a plain Tab (0x09).
func pressTab(m *Model) {
	_ = m.HandleKey(keys.Event{Kind: keys.Tab})
}

// prefixCompleter returns a completer serving fixed candidates filtered to
// the word under the cursor, like the driver's.
func prefixCompleter(m *Model, cands []string) *int {
	calls := new(int)
	m.SetCompleter(func(draft string, pos int) []string {
		*calls++
		var out []string
		for _, c := range cands {
			if strings.HasPrefix(c, draft[:pos]) {
				out = append(out, c)
			}
		}
		return out
	})
	return calls
}

// The first Tab inserts the candidates' longest common prefix, which need
// not be a candidate; the next Tab then takes the first candidate that the
// inserted text does not already equal.
func TestTabLongestCommonPrefix(t *testing.T) {
	m := New()
	calls := prefixCompleter(m, []string{"/model", "/modelx"})
	typed(t, m, "/m")
	pressTab(m)
	if got := m.Draft(); got != "/model" {
		t.Fatalf("draft after Tab = %q, want %q (longest common prefix)", got, "/model")
	}
	if got := m.Pos(); got != 6 {
		t.Fatalf("cursor after Tab = %d, want 6", got)
	}
	// The inserted prefix equals cands[0], so cycling skips it.
	pressTab(m)
	if got := m.Draft(); got != "/modelx" {
		t.Fatalf("draft after second Tab = %q, want %q", got, "/modelx")
	}
	if *calls != 1 {
		t.Fatalf("completer called %d times, want 1", *calls)
	}
}

// When the common prefix extends nothing, the first Tab takes the first
// candidate; later Tabs cycle the stored list and never re-query.
func TestTabCyclesStoredCandidates(t *testing.T) {
	m := New()
	calls := prefixCompleter(m, []string{"/ls", "/model"})
	typed(t, m, "/")
	pressTab(m)
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after Tab = %q, want %q (first candidate)", got, "/ls")
	}
	pressTab(m)
	if got := m.Draft(); got != "/model" {
		t.Fatalf("draft after second Tab = %q, want %q", got, "/model")
	}
	pressTab(m)
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after third Tab = %q, want %q (cycled)", got, "/ls")
	}
	if *calls != 1 {
		t.Fatalf("completer called %d times, want 1 (cycling must not re-query)", *calls)
	}
}

// Any edit resets the cycle: the next Tab is a fresh first-Tab attempt.
func TestTabResetOnEdit(t *testing.T) {
	m := New()
	calls := prefixCompleter(m, []string{"/ls", "/model"})
	typed(t, m, "/")
	pressTab(m)
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after Tab = %q, want /ls", got)
	}
	typed(t, m, "x")
	if got := m.Draft(); got != "/lsx" {
		t.Fatalf("draft = %q, want /lsx", got)
	}
	pressTab(m)
	if *calls != 2 {
		t.Fatalf("completer not re-queried after an edit: %d calls", *calls)
	}
	if got := m.Draft(); got != "/lsx" {
		t.Fatalf("draft after re-query = %q, want /lsx (no candidate extends the edit)", got)
	}
}

// Backspace resets the cycle the same way.
func TestTabResetOnBackspace(t *testing.T) {
	m := New()
	calls := prefixCompleter(m, []string{"/ls", "/model"})
	typed(t, m, "/")
	pressTab(m)
	if act := m.HandleKey(keys.Event{Kind: keys.Backspace}); act != ActionNone {
		t.Fatalf("backspace returned %v", act)
	}
	if got := m.Draft(); got != "/l" {
		t.Fatalf("draft after backspace = %q, want /l", got)
	}
	pressTab(m)
	if *calls != 2 {
		t.Fatalf("completer not re-queried after a backspace: %d calls", *calls)
	}
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after re-Tab = %q, want /ls (single match)", got)
	}
}

// Without a completer, or with one that finds nothing, Tab is a no-op.
func TestTabNoCompletion(t *testing.T) {
	m := New()
	typed(t, m, "/m")
	pressTab(m)
	if got := m.Draft(); got != "/m" {
		t.Fatalf("Tab without a completer changed the draft: %q", got)
	}

	m2 := New()
	m2.SetCompleter(func(draft string, pos int) []string { return nil })
	typed(t, m2, "/m")
	pressTab(m2)
	pressTab(m2)
	if got := m2.Draft(); got != "/m" {
		t.Fatalf("Tab with no candidates changed the draft: %q", got)
	}
}

// The inserted candidate is undoable like any edit.
func TestTabUndoable(t *testing.T) {
	m := New()
	m.SetCompleter(func(draft string, pos int) []string {
		return []string{"/ls", "/model"}
	})
	typed(t, m, "/")
	pressTab(m)
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after Tab = %q, want /ls", got)
	}
	pressUndo(m)
	if got := m.Draft(); got != "/" {
		t.Fatalf("draft after undo = %q, want /", got)
	}
	if got := m.Pos(); got != 1 {
		t.Fatalf("cursor after undo = %d, want 1", got)
	}
}

// Tab is locked like every other key while a task streams, and the attempt
// state survives so the first Tab after the unlock completes.
func TestTabLockedWhileStreaming(t *testing.T) {
	m := New()
	m.SetCompleter(func(draft string, pos int) []string {
		return []string{"/ls", "/model"}
	})
	typed(t, m, "/")
	m.StartStream()
	pressTab(m)
	if got := m.Draft(); got != "/" {
		t.Fatalf("Tab during streaming changed the draft: %q", got)
	}
	m.EndStream()
	pressTab(m)
	if got := m.Draft(); got != "/ls" {
		t.Fatalf("draft after unlocked Tab = %q, want /ls", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"line1\nline2", "line1\nline2"},
		{"a\tb", "a\tb"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"a\r\nb", "a\nb"},
		{"a\x7fb", "ab"},
		{"a\x03b", "ab"},
	}
	for _, c := range cases {
		if got := Sanitize(c.in); got != c.want {
			t.Fatalf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHistoryRecall(t *testing.T) {
	m := New()
	m.SetHistory([]string{"first", "second"})
	typed(t, m, "live")

	// Up recalls the newest user input.
	if act := m.HandleKey(keys.Event{Kind: keys.ArrowUp}); act != ActionNone {
		t.Fatalf("Up returned %v, want ActionNone", act)
	}
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after Up = %q, want second", got)
	}
	if got := m.Pos(); got != 6 {
		t.Fatalf("cursor after Up = %d, want 6 (end of draft)", got)
	}

	// ^P recalls the previous entry.
	if act := m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x10}}); act != ActionNone {
		t.Fatalf("^P returned %v, want ActionNone", act)
	}
	if got := m.Draft(); got != "first" {
		t.Fatalf("draft after ^P = %q, want first", got)
	}

	// Up at the oldest entry stays put.
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "first" {
		t.Fatalf("draft after Up at oldest = %q, want first", got)
	}

	// ^N walks forward again...
	m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x0e}})
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after ^N = %q, want second", got)
	}
	// ...and past the newest entry restores the live draft.
	m.HandleKey(keys.Event{Kind: keys.Other, Raw: []byte{0x0e}})
	if got := m.Draft(); got != "live" {
		t.Fatalf("draft past newest = %q, want live (restored)", got)
	}

	// Down with no recall is a no-op.
	m.HandleKey(keys.Event{Kind: keys.ArrowDown})
	if got := m.Draft(); got != "live" {
		t.Fatalf("draft after Down without recall = %q, want live", got)
	}

	// Up again; then typing detaches from recall.
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after Up = %q, want second", got)
	}
	typed(t, m, "x")
	if got := m.Draft(); got != "secondx" {
		t.Fatalf("draft after typing = %q, want secondx", got)
	}
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after Up post-detach = %q, want second (fresh recall)", got)
	}

	// Backspace also detaches.
	m.HandleKey(keys.Event{Kind: keys.Backspace})
	if got := m.Draft(); got != "secon" {
		t.Fatalf("draft after backspace = %q, want secon", got)
	}
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after Up post-backspace = %q, want second", got)
	}

	// SetHistory resets an in-flight recall and keeps the draft.
	m.SetHistory([]string{"a"})
	if got := m.Draft(); got != "second" {
		t.Fatalf("draft after SetHistory = %q, want second (kept)", got)
	}
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "a" {
		t.Fatalf("draft after Up on new history = %q, want a", got)
	}

	// Submit resets recall, so the next Up recalls the newest entry again.
	if got := m.Submit(func(string) {}, 40); got != "a" {
		t.Fatalf("Submit returned %q, want a", got)
	}
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "a" {
		t.Fatalf("draft after Up post-Submit = %q, want a", got)
	}
}

func TestHistoryRecallEmpty(t *testing.T) {
	m := New()
	m.SetHistory(nil)
	typed(t, m, "d")
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	m.HandleKey(keys.Event{Kind: keys.ArrowDown})
	if got := m.Draft(); got != "d" {
		t.Fatalf("draft with empty history = %q, want d", got)
	}

	// Remember appends to the recall list; the submitted text is recallable.
	m.Remember("n1")
	m.HandleKey(keys.Event{Kind: keys.ArrowUp})
	if got := m.Draft(); got != "n1" {
		t.Fatalf("draft after Remember + Up = %q, want n1", got)
	}
}
