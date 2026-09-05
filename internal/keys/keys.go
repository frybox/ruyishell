// Package keys converts a raw byte stream (the terminal's input) into
// keyboard events, keeping the exact raw bytes of every event so they can
// be forwarded to the shell unchanged.
package keys

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Kind identifies the semantic meaning of an input event.
type Kind uint8

const (
	// Other is any event without a dedicated meaning (control bytes,
	// unknown escape sequences, alt-combos). Raw always carries the bytes.
	Other Kind = iota
	// Rune is a printable character (including multi-byte UTF-8).
	Rune
	// Enter is a carriage return or newline.
	Enter
	// Backspace is DEL or BS.
	Backspace
	ArrowLeft
	ArrowRight
	ArrowUp
	ArrowDown
	Home
	End
	// PageUp and PageDown are the scroll keys; CtrlEnd is Ctrl+End (used as
	// the AI view's jump-to-bottom binding).
	PageUp
	PageDown
	CtrlEnd
	// CtrlTab is the mode-switch key (a generic kind, not tied to one
	// physical key). The default binding is Shift+Tab; terminals encode the
	// bound key several ways, and all recognized encodings map to this Kind.
	CtrlTab
	// Esc is a lone escape-key press.
	Esc
	// Tab is a plain tab (0x09): the AI-mode completion key. In shell mode
	// the raw byte is forwarded to the shell's own readline, as with every
	// other byte.
	Tab
	// PasteStart/PasteEnd are the DEC 2004 (bracketed paste) markers the
	// terminal emits around pasted content once the driver enables the mode
	// (CSI ? 2004 h). A paste block is everything between the two markers,
	// including embedded newlines, which must be inserted as a single atomic
	// unit rather than typed key by key (an embedded Enter would otherwise
	// submit a partial draft).
	PasteStart
	PasteEnd
)

// Event is one decoded keyboard input.
type Event struct {
	Kind Kind
	R    rune   // set when Kind == Rune
	Raw  []byte // exact input bytes, for lossless passthrough
}

// EscTimeout is how long a lone ESC is held before it is reported as Esc.
const EscTimeout = 50 * time.Millisecond

// Reader decodes a raw io.Reader into key events.
//
// A single background goroutine continuously reads bytes from the reader
// into a channel; Next assembles events from that channel on demand. This
// is done instead of issuing one Read per Next so that the byte following a
// lone ESC is never lost to a stale goroutine.
type Reader struct {
	r       io.Reader
	ch      chan byte
	timeout time.Duration
	// ctrlTab lists the raw byte sequences classified as CtrlTab (the
	// mode-switch key). Configurable so terminals that cannot send a
	// reliable Ctrl+Tab can bind a different key.
	ctrlTab [][]byte
}

// NewReader returns a Reader decoding events from r with the default
// Shift+Tab mode-switch encoding (the same default as CtrlTabEncodings("")).
func NewReader(r io.Reader) *Reader {
	return NewReaderWith(r, builtinShiftTab)
}

// NewReaderWith returns a Reader decoding events from r; the raw byte
// sequences in ctrlTab are classified as CtrlTab, replacing the built-in
// Ctrl+Tab encodings. Pass CtrlTabEncodings(name) to honor a configured
// mode_switch value.
func NewReaderWith(r io.Reader, ctrlTab [][]byte) *Reader {
	rd := &Reader{r: r, ch: make(chan byte, 64), timeout: EscTimeout, ctrlTab: ctrlTab}
	go rd.pump()
	return rd
}

// pump reads from the underlying reader into ch until it errors or hits
// EOF, then closes ch.
func (rd *Reader) pump() {
	buf := make([]byte, 256)
	for {
		n, err := rd.r.Read(buf)
		for i := 0; i < n; i++ {
			rd.ch <- buf[i]
		}
		if err != nil {
			close(rd.ch)
			return
		}
	}
}

// readByte returns the next buffered byte, waiting at most d. A non-positive
// d waits indefinitely. The bool is false when no byte arrived (timeout or
// the channel was closed).
func (rd *Reader) readByte(d time.Duration) (byte, bool) {
	if d <= 0 {
		b, ok := <-rd.ch
		return b, ok
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case b, ok := <-rd.ch:
		return b, ok
	case <-t.C:
		return 0, false
	}
}

// Next returns the next decoded event. It returns io.EOF when the
// underlying reader is exhausted. Key-up reports (the ConPTY's release
// counterpart to a key-down) carry no input and are skipped, so they never
// reach the driver or get forwarded to the shell.
func (rd *Reader) Next() (Event, error) {
	for {
		b, ok := rd.readByte(0)
		if !ok {
			return Event{}, io.EOF
		}
		ev, skip := rd.decode(b)
		if skip {
			continue
		}
		rd.applyBindings(&ev)
		return ev, nil
	}
}

// decode classifies a single leading byte into an event. The second return
// value is true when the event should be dropped (a key-up report with no
// input to forward). Escape and UTF-8 sequences consume further bytes from
// the stream as needed.
func (rd *Reader) decode(b byte) (Event, bool) {
	ev := Event{Raw: []byte{b}}
	switch {
	case b == '\r' || b == '\n':
		ev.Kind = Enter
	case b == 0x7f || b == 0x08:
		ev.Kind = Backspace
	case b == 0x09:
		ev.Kind = Tab
	case b < 0x20:
		if b == 0x1b {
			return rd.decodeEsc(ev)
		}
		// Other control bytes (^C, ^D, ...) are forwarded raw.
		ev.Kind = Other
	case b < 0x80:
		ev.Kind = Rune
		ev.R = rune(b)
	default:
		return rd.decodeUTF8(ev)
	}
	return ev, false
}

// applyBindings reclassifies an event whose raw bytes match a configured
// mode-switch encoding as CtrlTab. This is how a custom mode_switch binding
// (e.g. Ctrl+Space) is honored.
func (rd *Reader) applyBindings(ev *Event) {
	for _, seq := range rd.ctrlTab {
		if bytes.Equal(ev.Raw, seq) {
			ev.Kind = CtrlTab
			return
		}
	}
}

func (rd *Reader) decodeEsc(ev Event) (Event, bool) {
	b, ok := rd.readByte(rd.timeout)
	if !ok {
		ev.Kind = Esc
		return ev, false
	}
	ev.Raw = append(ev.Raw, b)
	switch b {
	case '[':
		return rd.decodeCSI(ev)
	case 'O':
		return rd.decodeSS3(ev)
	default:
		// Alt+key or an unknown escape prefix: forward raw.
		ev.Kind = Other
		return ev, false
	}
}

// decodeCSI consumes the rest of a CSI sequence (ESC [ ... final byte).
// A ConPTY key-event report (ESC [ <vk> ; <sc> ; <vk2> ; <down> ; <mods> ;
// <flags> _, emitted when ENABLE_VIRTUAL_TERMINAL_INPUT is on) is translated
// into the xterm key bytes the pty shell expects; every other CSI sequence
// keeps its raw bytes (classified by classifyCSI) so it is forwarded
// unchanged. Reports are distinguished from ordinary CSI sequences by their
// final byte ('_') and the digit (or optional '<' introducer) right after '['.
func (rd *Reader) decodeCSI(ev Event) (Event, bool) {
	for len(ev.Raw) < 48 {
		b, ok := rd.readByte(rd.timeout)
		if !ok {
			ev.Kind = Other
			return ev, false
		}
		ev.Raw = append(ev.Raw, b)
		if b >= 0x40 && b <= 0x7e {
			break
		}
	}
	// A ConPTY key-event report (ESC [ <vk> ; <sc> ; <vk2> ; <down> ; <mods> ;
	// <flags> _) is translated into the xterm key bytes the pty shell expects;
	// every other CSI sequence keeps its raw bytes (classified by classifyCSI)
	// so it is forwarded unchanged. Reports end in '_' and start with a digit
	// (or the optional '<' introducer), which ordinary CSI sequences never do.
	if isKeyEventReport(ev.Raw) {
		return rd.decodeKeyEventReport(ev)
	}
	ev.Kind = classifyCSI(ev.Raw)
	return ev, false
}

// isKeyEventReport reports whether raw is a ConPTY key-event report
// (ESC [ <vk> ; <sc> ; <vk2> ; <down> ; <mods> ; <flags> _). The '<'
// introducer is optional across console implementations, so both forms are
// accepted; the report always ends in '_' and carries several ';'-separated
// numeric fields.
func isKeyEventReport(raw []byte) bool {
	if len(raw) < 5 || raw[0] != '\x1b' || raw[1] != '[' || raw[len(raw)-1] != '_' {
		return false
	}
	i := 2
	if raw[i] == '<' {
		i++
	}
	if i >= len(raw) || raw[i] < '0' || raw[i] > '9' {
		return false
	}
	for _, b := range raw[i:] {
		if b == ';' {
			return true
		}
	}
	return false
}

func (rd *Reader) decodeSS3(ev Event) (Event, bool) {
	b, ok := rd.readByte(rd.timeout)
	if !ok {
		ev.Kind = Other
		return ev, false
	}
	ev.Raw = append(ev.Raw, b)
	switch b {
	case 'A':
		ev.Kind = ArrowUp
	case 'B':
		ev.Kind = ArrowDown
	case 'C':
		ev.Kind = ArrowRight
	case 'D':
		ev.Kind = ArrowLeft
	case 'H':
		ev.Kind = Home
	case 'F':
		ev.Kind = End
	default:
		ev.Kind = Other
	}
	return ev, false
}

func (rd *Reader) decodeUTF8(ev Event) (Event, bool) {
	need := utf8Continuations(ev.Raw[0])
	for i := 0; i < need; i++ {
		b, ok := rd.readByte(rd.timeout)
		if !ok {
			ev.Kind = Other
			return ev, false
		}
		ev.Raw = append(ev.Raw, b)
	}
	r, _ := utf8.DecodeRune(ev.Raw)
	if r == utf8.RuneError {
		ev.Kind = Other
		return ev, false
	}
	ev.Kind = Rune
	ev.R = r
	return ev, false
}

// decodeKeyEventReport translates a ConPTY key-event report into the xterm
// key bytes the wrapped pty shell expects, so its line editor (PSReadLine on
// Windows, readline elsewhere) interprets the key correctly instead of choking
// on the Windows-specific report. The report's produced character (vk2) drives
// printable input; virtual-key codes drive the special keys; modifier-only and
// key-release events have no xterm equivalent and are dropped. The resulting
// Event's Raw is always the translated bytes — never the raw report — so
// forwarding hands the shell clean xterm input.
func (rd *Reader) decodeKeyEventReport(ev Event) (Event, bool) {
	start := 2
	if len(ev.Raw) > 2 && ev.Raw[2] == '<' {
		start = 3
	}
	body := string(ev.Raw[start : len(ev.Raw)-1])
	parts := strings.Split(body, ";")
	if len(parts) < 5 {
		// Malformed report: drop it rather than forward a Windows-specific
		// byte string the pty shell's line editor cannot parse.
		return Event{}, true
	}
	vk, err1 := strconv.Atoi(parts[0])
	if err1 != nil {
		return Event{}, true
	}
	// parts[3] and parts[4] are the key-down flag (0|1) and the control-key
	// modifier state (a bitmask), in either order across console
	// implementations; disambiguate by value so the shift/ctrl/alt detection
	// below is correct either way.
	a, errA := strconv.Atoi(parts[3])
	b, errB := strconv.Atoi(parts[4])
	if errA != nil || errB != nil {
		return Event{}, true
	}
	var down, mods int
	switch {
	case (a == 0 || a == 1) && b > 1:
		down, mods = a, b
	case (b == 0 || b == 1) && a > 1:
		down, mods = b, a
	default:
		down, mods = a, b
	}
	// Key releases carry no input in xterm; dropping them keeps the shell from
	// seeing a stuck modifier that was never released.
	if down == 0 {
		return Event{}, true
	}
	const (
		shift = 0x10
		ctrl  = 0x0c // LEFT_CTRL 0x08 | RIGHT_CTRL 0x04
		alt   = 0x03 // LEFT_ALT 0x02 | RIGHT_ALT 0x01
	)
	isShift := mods&shift != 0
	isCtrl := mods&ctrl != 0
	isAlt := mods&alt != 0

	// vk2 (parts[2]) is the produced character's code point.
	vk2, _ := strconv.Atoi(parts[2])

	// Pure modifier and lock keys carry no input of their own in xterm;
	// forwarding their reports corrupts the line editor, so drop them.
	switch vk {
	case 16, 17, 18, 20, 144, 145: // Shift, Ctrl, Alt, CapsLock, NumLock, ScrollLock
		return Event{}, true
	}

	// Mode-switch gesture: Shift+Tab becomes the canonical backtab, which the
	// configured mode_switch binding classifies as CtrlTab. ConPTY reports it
	// two ways: a Tab key with the shift modifier set (vk 9, shift bit set),
	// or the "backtab" key event it emits for CSI Z input (vk 0, produced
	// character ESC). The produced character is unambiguous for the second
	// form, so it is detected by vk/vk2 and not by mods — otherwise the
	// NumLock/CapsLock bits that ConPTY ORs into the control-key-state would
	// make the match miss on a real machine.
	if (vk == 9 && isShift) || (vk == 0 && vk2 == 27) {
		return Event{Raw: []byte("\x1b[Z")}, false
	}

	// Letters: Ctrl+letter is a control byte; otherwise the produced character
	// comes from the virtual key and shift state (VK codes are case-insensitive,
	// so shift supplies the case). When the report carries the produced
	// character (CapsLock, dead-key composition, IME) prefer it, since it is
	// the authoritative code point. Alt sends an ESC prefix.
	if vk >= 0x41 && vk <= 0x5a {
		if isCtrl {
			return Event{Kind: Other, Raw: []byte{byte(1 + vk - 0x41)}}, false
		}
		cp := int('a') + (vk - 0x41)
		if isShift {
			cp = int('A') + (vk - 0x41)
		}
		if vk2 >= 'A' && vk2 <= 'z' {
			cp = vk2
		}
		buf := []byte(string(rune(cp)))
		if isAlt {
			buf = append([]byte{'\x1b'}, buf...)
		}
		return Event{Kind: Rune, R: rune(cp), Raw: buf}, false
	}

	// Special keys by virtual-key code.
	switch vk {
	case 13: // Enter
		return Event{Kind: Enter, Raw: []byte("\r")}, false
	case 8: // Backspace
		return Event{Kind: Backspace, Raw: []byte("\x7f")}, false
	case 9: // Tab
		return Event{Kind: Tab, Raw: []byte("\t")}, false
	case 27: // Escape
		return Event{Kind: Esc, Raw: []byte("\x1b")}, false
	case 32: // Space
		return Event{Kind: Rune, R: ' ', Raw: []byte(" ")}, false
	case 33: // PageUp
		return Event{Kind: PageUp, Raw: []byte("\x1b[5~")}, false
	case 34: // PageDown
		return Event{Kind: PageDown, Raw: []byte("\x1b[6~")}, false
	case 35: // End
		return Event{Kind: End, Raw: []byte("\x1b[F")}, false
	case 36: // Home
		return Event{Kind: Home, Raw: []byte("\x1b[H")}, false
	case 37: // Left
		return Event{Kind: ArrowLeft, Raw: []byte("\x1b[D")}, false
	case 38: // Up
		return Event{Kind: ArrowUp, Raw: []byte("\x1b[A")}, false
	case 39: // Right
		return Event{Kind: ArrowRight, Raw: []byte("\x1b[C")}, false
	case 40: // Down
		return Event{Kind: ArrowDown, Raw: []byte("\x1b[B")}, false
	case 45: // Insert
		return Event{Kind: Other, Raw: []byte("\x1b[2~")}, false
	case 46: // Delete
		return Event{Kind: Other, Raw: []byte("\x1b[3~")}, false
	case 112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123: // F1-F12
		fkeys := []string{"\x1bOP", "\x1bOQ", "\x1bOR", "\x1bOS", "\x1b[15~", "\x1b[17~", "\x1b[18~", "\x1b[19~", "\x1b[20~", "\x1b[21~", "\x1b[23~", "\x1b[24~"}
		return Event{Kind: Other, Raw: []byte(fkeys[vk-112])}, false
	}

	// Printable input (digits, punctuation, CJK, …): the vk2 field is the
	// produced character's code point. Alt sends an ESC prefix.
	if len(parts) >= 3 {
		if cp, err := strconv.Atoi(parts[2]); err == nil && cp >= 0x20 && cp <= 0x10FFFF {
			buf := []byte(string(rune(cp)))
			if isAlt {
				buf = append([]byte{'\x1b'}, buf...)
			}
			return Event{Kind: Rune, R: rune(cp), Raw: buf}, false
		}
	}

	// Unrecognized report (no xterm equivalent, e.g. an unbound function
	// key): dropping it is safer than forwarding the raw Windows-specific
	// bytes, which the pty shell's line editor would misparse into a blank
	// line and a highlighted command. Genuine input always carries a
	// printable vk2 and is handled above.
	return Event{}, true
}

// classifyCSI maps the exact raw bytes of a CSI sequence to a Kind.
func classifyCSI(raw []byte) Kind {
	switch string(raw) {
	case "\x1b[A":
		return ArrowUp
	case "\x1b[B":
		return ArrowDown
	case "\x1b[C":
		return ArrowRight
	case "\x1b[D":
		return ArrowLeft
	case "\x1b[H", "\x1b[1~", "\x1b[7~":
		return Home
	case "\x1b[F", "\x1b[4~", "\x1b[8~":
		return End
	case "\x1b[5~":
		return PageUp
	case "\x1b[6~":
		return PageDown
	case "\x1b[1;5F", "\x1b[8;5~":
		return CtrlEnd
	case "\x1b[200~":
		return PasteStart
	case "\x1b[201~":
		return PasteEnd
	default:
		return Other
	}
}

// builtinShiftTab is the raw byte sequence terminals send for Shift+Tab,
// the default mode-switch key. CSI Z ("backtab") is the standard
// xterm-compatible encoding and is distinct from a plain Tab (0x09), so it
// does not collide with shell tab-completion.
var builtinShiftTab = [][]byte{
	[]byte("\x1b[Z"),
	// xterm's modified "backtab" for terminals that send the
	// CSI-u / explicit-modifier form of Shift+Tab.
	[]byte("\x1b[27;2;9~"),
	// vt100-style modified backtab (parameterized modifiers): some
	// terminals send this instead of the bare CSI Z above.
	[]byte("\x1b[1;2Z"),
}

// builtinCtrlTab are the raw byte sequences terminals send for Ctrl+Tab.
// They are the "ctrl-tab" mode_switch binding (a fallback for terminals
// where Shift+Tab is unavailable or intercepted); CtrlTabEncodings can
// select a different binding.
var builtinCtrlTab = [][]byte{
	[]byte("\x1b[27;5;9~"),
	[]byte("\x1b[9;5u"),
	[]byte("\x1b[1;5I"),
}

// CtrlTabEncodings returns the raw byte sequences classified as the
// mode-switch key for a configured mode_switch value. The empty string
// (unset) selects the default Shift+Tab encoding. "ctrl-tab" selects the
// legacy Ctrl+Tab encodings (useful on terminals that intercept Shift+Tab);
// "ctrl-space" and "ctrl-backslash" are single-byte fallbacks.
func CtrlTabEncodings(name string) [][]byte {
	switch name {
	case "shift-tab":
		return builtinShiftTab
	case "ctrl-tab":
		return builtinCtrlTab
	case "ctrl-space":
		return [][]byte{{0x00}} // Ctrl+Space is NUL in the terminal
	case "ctrl-backslash":
		return [][]byte{{0x1c}} // Ctrl+\ is FS (0x1c)
	default:
		return builtinShiftTab
	}
}

// utf8Continuations returns how many continuation bytes follow the lead
// byte of a UTF-8 sequence.
func utf8Continuations(lead byte) int {
	switch {
	case lead >= 0xc2 && lead <= 0xdf:
		return 1
	case lead >= 0xe0 && lead <= 0xef:
		return 2
	case lead >= 0xf0 && lead <= 0xf4:
		return 3
	}
	return 0
}
