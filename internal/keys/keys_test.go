package keys

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// decode feeds a raw byte string into a fresh Reader and returns the events
// produced (plus any error at the end).
func decode(t *testing.T, raw string) []Event {
	t.Helper()
	rd := NewReader(bytes.NewReader([]byte(raw)))
	var evs []Event
	for {
		ev, err := rd.Next()
		if err == io.EOF {
			return evs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		evs = append(evs, ev)
	}
}

// assertEvents compares decoded events against expected (kind + raw).
func assertEvents(t *testing.T, raw string, want []Event) {
	t.Helper()
	got := decode(t, raw)
	if len(got) != len(want) {
		t.Fatalf("%q: got %d events, want %d (%+v vs %+v)", raw, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || string(got[i].Raw) != string(want[i].Raw) {
			t.Fatalf("%q: event %d = kind %v raw %q, want kind %v raw %q", raw, i, got[i].Kind, got[i].Raw, want[i].Kind, want[i].Raw)
		}
	}
}

func ev(kind Kind, raw string) Event { return Event{Kind: kind, Raw: []byte(raw)} }

func TestASCIIRunes(t *testing.T) {
	assertEvents(t, "hi", []Event{ev(Rune, "h"), ev(Rune, "i")})
	for _, r := range "AZ09_-=.,/;'[]()" {
		assertEvents(t, string(r), []Event{ev(Rune, string(r))})
	}
}

func TestEnter(t *testing.T) {
	assertEvents(t, "\r", []Event{ev(Enter, "\r")})
	assertEvents(t, "\n", []Event{ev(Enter, "\n")})
	assertEvents(t, "a\rb", []Event{ev(Rune, "a"), ev(Enter, "\r"), ev(Rune, "b")})
}

func TestBackspace(t *testing.T) {
	assertEvents(t, "\x7f", []Event{ev(Backspace, "\x7f")})
	assertEvents(t, "\x08", []Event{ev(Backspace, "\x08")})
}

func TestTab(t *testing.T) {
	// A plain tab is its own kind (AI-mode completion), distinct from the
	// Shift+Tab backtab that maps to CtrlTab.
	assertEvents(t, "\t", []Event{ev(Tab, "\t")})
	assertEvents(t, "a\tb", []Event{ev(Rune, "a"), ev(Tab, "\t"), ev(Rune, "b")})
}

func TestControlBytesForwardedRaw(t *testing.T) {
	// 0x09 (tab) is excluded: it is the Tab kind, not a forwarded control
	// byte (see TestTab).
	for _, c := range []byte{0x03, 0x04, 0x1a, 0x0c, 0x11} {
		assertEvents(t, string([]byte{c}), []Event{ev(Other, string([]byte{c}))})
	}
}

func TestLoneEsc(t *testing.T) {
	rd := NewReader(bytes.NewReader([]byte("\x1b")))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != Esc || string(ev.Raw) != "\x1b" {
		t.Fatalf("lone esc: got kind %v raw %q, want Esc ESC", ev.Kind, ev.Raw)
	}
}

func TestLoneEscKeepsNextKeystroke(t *testing.T) {
	// ESC immediately followed by 'x' is an alt-x combo, not a lone Esc;
	// the 'x' must not be lost.
	assertEvents(t, "\x1b x", []Event{ev(Other, "\x1b "), ev(Rune, "x")})
}

func TestCSIArrows(t *testing.T) {
	assertEvents(t, "\x1b[A", []Event{ev(ArrowUp, "\x1b[A")})
	assertEvents(t, "\x1b[B", []Event{ev(ArrowDown, "\x1b[B")})
	assertEvents(t, "\x1b[C", []Event{ev(ArrowRight, "\x1b[C")})
	assertEvents(t, "\x1b[D", []Event{ev(ArrowLeft, "\x1b[D")})
}

func TestCSIPasteMarkers(t *testing.T) {
	assertEvents(t, "\x1b[200~", []Event{ev(PasteStart, "\x1b[200~")})
	assertEvents(t, "\x1b[201~", []Event{ev(PasteEnd, "\x1b[201~")})
	// A full bracketed-paste block: markers plus the runed content.
	assertEvents(t,
		"\x1b[200~ab\ncd\x1b[201~",
		[]Event{
			ev(PasteStart, "\x1b[200~"),
			ev(Rune, "a"), ev(Rune, "b"),
			ev(Enter, "\n"),
			ev(Rune, "c"), ev(Rune, "d"),
			ev(PasteEnd, "\x1b[201~"),
		})
}

func TestCSIHomeEnd(t *testing.T) {
	for _, seq := range []string{"\x1b[H", "\x1b[1~", "\x1b[7~"} {
		assertEvents(t, seq, []Event{ev(Home, seq)})
	}
	for _, seq := range []string{"\x1b[F", "\x1b[4~", "\x1b[8~"} {
		assertEvents(t, seq, []Event{ev(End, seq)})
	}
}

func TestSS3Arrows(t *testing.T) {
	assertEvents(t, "\x1bOA", []Event{ev(ArrowUp, "\x1bOA")})
	assertEvents(t, "\x1bOB", []Event{ev(ArrowDown, "\x1bOB")})
	assertEvents(t, "\x1bOC", []Event{ev(ArrowRight, "\x1bOC")})
	assertEvents(t, "\x1bOD", []Event{ev(ArrowLeft, "\x1bOD")})
	assertEvents(t, "\x1bOH", []Event{ev(Home, "\x1bOH")})
	assertEvents(t, "\x1bOF", []Event{ev(End, "\x1bOF")})
}

func TestScrollKeys(t *testing.T) {
	for _, seq := range []string{"\x1b[5~"} {
		assertEvents(t, seq, []Event{ev(PageUp, seq)})
	}
	for _, seq := range []string{"\x1b[6~"} {
		assertEvents(t, seq, []Event{ev(PageDown, seq)})
	}
}

func TestCtrlEndEncodings(t *testing.T) {
	for _, seq := range []string{"\x1b[1;5F", "\x1b[8;5~"} {
		assertEvents(t, seq, []Event{ev(CtrlEnd, seq)})
	}
}

// The default mode-switch key is Shift+Tab (CSI Z); the default reader
// classifies its known encodings as CtrlTab.
func TestShiftTabEncodings(t *testing.T) {
	for _, seq := range []string{"\x1b[Z", "\x1b[27;2;9~", "\x1b[1;2Z"} {
		assertEvents(t, seq, []Event{ev(CtrlTab, seq)})
	}
}

// The "ctrl-tab" binding still recognizes the legacy Ctrl+Tab encodings. It
// is no longer the default, so it must be selected explicitly.
func TestCtrlTabEncodings(t *testing.T) {
	for _, seq := range []string{"\x1b[27;5;9~", "\x1b[9;5u", "\x1b[1;5I"} {
		rd := NewReaderWith(bytes.NewReader([]byte(seq)), CtrlTabEncodings("ctrl-tab"))
		ev, err := rd.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Kind != CtrlTab || string(ev.Raw) != seq {
			t.Fatalf("%q: got kind %v raw %q, want CtrlTab", seq, ev.Kind, ev.Raw)
		}
	}
}

// The legacy Ctrl+Tab sequence is no longer the default mode switch: under
// the default (shift-tab) binding it is left as Other so it can be
// forwarded to the shell.
func TestReaderDefaultIgnoresCtrlTab(t *testing.T) {
	rd := NewReader(bytes.NewReader([]byte("\x1b[27;5;9~")))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != Other || string(ev.Raw) != "\x1b[27;5;9~" {
		t.Fatalf("ctrl-tab under default: got kind %v raw %q, want Other", ev.Kind, ev.Raw)
	}
}

func TestCtrlTabEncodingsForName(t *testing.T) {
	if got := CtrlTabEncodings(""); len(got) != 3 || string(got[0]) != "\x1b[Z" || string(got[1]) != "\x1b[27;2;9~" || string(got[2]) != "\x1b[1;2Z" {
		t.Fatalf("default: got %q, want [\x1b[Z \x1b[27;2;9~ \x1b[1;2Z]", got)
	}
	if got := CtrlTabEncodings("shift-tab"); len(got) != 3 || string(got[0]) != "\x1b[Z" || string(got[1]) != "\x1b[27;2;9~" || string(got[2]) != "\x1b[1;2Z" {
		t.Fatalf("shift-tab: got %q, want [\x1b[Z \x1b[27;2;9~ \x1b[1;2Z]", got)
	}
	if got := CtrlTabEncodings("ctrl-tab"); len(got) != 3 {
		t.Fatalf("ctrl-tab: got %d encodings, want 3", len(got))
	}
	if got := CtrlTabEncodings("ctrl-space"); string(got[0]) != "\x00" {
		t.Fatalf("ctrl-space: got %q, want NUL", got[0])
	}
	if got := CtrlTabEncodings("ctrl-backslash"); string(got[0]) != "\x1c" {
		t.Fatalf("ctrl-backslash: got %q, want FS", got[0])
	}
}

// A custom mode_switch binding is honored and replaces the default one.
func TestReaderCustomCtrlTab(t *testing.T) {
	rd := NewReaderWith(bytes.NewReader([]byte("\x00")), CtrlTabEncodings("ctrl-space"))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != CtrlTab || string(ev.Raw) != "\x00" {
		t.Fatalf("ctrl-space: got kind %v raw %q, want CtrlTab NUL", ev.Kind, ev.Raw)
	}

	// The default Ctrl+Tab sequence is no longer a mode switch.
	rd = NewReaderWith(bytes.NewReader([]byte("\x1b[27;5;9~")), CtrlTabEncodings("ctrl-space"))
	ev, err = rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind == CtrlTab {
		t.Fatalf("ctrl-tab should be Other under ctrl-space binding, got CtrlTab")
	}
	if ev.Kind != Other || string(ev.Raw) != "\x1b[27;5;9~" {
		t.Fatalf("ctrl-tab under ctrl-space: got kind %v raw %q, want Other", ev.Kind, ev.Raw)
	}
}

func TestReaderCtrlBackslash(t *testing.T) {
	rd := NewReaderWith(bytes.NewReader([]byte("\x1c")), CtrlTabEncodings("ctrl-backslash"))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != CtrlTab || string(ev.Raw) != "\x1c" {
		t.Fatalf("ctrl-backslash: got kind %v raw %q, want CtrlTab FS", ev.Kind, ev.Raw)
	}
}

func TestUTF8Runes(t *testing.T) {
	assertEvents(t, "中文", []Event{ev(Rune, "中"), ev(Rune, "文")})
	assertEvents(t, "é", []Event{ev(Rune, "é")})
}

func TestTruncatedCSI(t *testing.T) {
	// A CSI sequence that never completes (no final byte) is forwarded raw.
	rd := NewReader(bytes.NewReader([]byte("\x1b[1;2")))
	ev, err := rd.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Kind != Other {
		t.Fatalf("truncated CSI: got kind %v, want Other", ev.Kind)
	}
}

func TestEOFReturned(t *testing.T) {
	rd := NewReader(bytes.NewReader(nil))
	if _, err := rd.Next(); err != io.EOF {
		t.Fatalf("empty reader: got %v, want io.EOF", err)
	}
}

func TestDecodeSpeed(t *testing.T) {
	// A large burst must decode promptly (the pump is a single goroutine).
	rd := NewReader(bytes.NewReader(bytes.Repeat([]byte{'a'}, 10000)))
	done := make(chan int)
	go func() {
		n := 0
		for {
			ev, err := rd.Next()
			if err != nil {
				done <- n
				return
			}
			if ev.Kind != Rune {
				done <- -1
				return
			}
			n++
		}
	}()
	select {
	case n := <-done:
		if n != 10000 {
			t.Fatalf("decoded %d runes, want 10000", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("burst decode timed out")
	}
}
