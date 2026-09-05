// The prompt marker (主屏方案 §3.2 修订, [tui] prompt_marker) makes rysh own
// the terminal title while it runs: the title carries "rysh · model ·
// session", and the shell's own title-setting prompts (a common PS1
// convention) must not steal it back on every prompt repaint. A prompt
// writes its title with OSC 0 (icon+window) or OSC 2 (window), so the
// output loop runs the pty stream through this filter, which drops exactly
// those two sequences and passes everything else — every other OSC (OSC 7
// cwd reports the cwd tracker consumes, OSC 8 hyperlinks, ...), CSI and SGR
// alike — through byte for byte.

package main

import "bytes"

// oscMaxLen bounds an unterminated OSC the filter buffers while deciding
// whether it is a title: past it the bytes are flushed as-is and the filter
// resynchronizes on the next ESC, so a pathological stream cannot wedge it.
const oscMaxLen = 1024

const (
	tfNormal = iota
	tfEsc    // a lone ESC is held: the next byte decides (a "]" starts an OSC)
	tfOsc    // inside an OSC: bytes buffered until the BEL/ST terminator
)

// titleFilter is a stateful stream filter; one instance per rysh run, fed
// from the pty output loop only.
type titleFilter struct {
	state int
	buf   []byte // the OSC seen so far, starting with its ESC byte
}

// Filter returns p with any OSC 0/2 title sequences removed. It is safe to
// call back to back: a sequence split across chunk boundaries is carried in
// the filter's state, and a held lone ESC is re-emitted with the byte that
// resolves it.
func (f *titleFilter) Filter(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for _, b := range p {
		switch f.state {
		case tfNormal:
			if b == 0x1b {
				f.state = tfEsc
				continue
			}
			out = append(out, b)
		case tfEsc:
			switch {
			case b == 0x1b:
				// The held ESC was a lone escape after all: emit it and
				// keep this byte as the new held ESC.
				out = append(out, 0x1b)
			case b == ']':
				f.buf = f.buf[:0]
				f.buf = append(f.buf, 0x1b, ']')
				f.state = tfOsc
			default:
				out = append(out, 0x1b, b)
				f.state = tfNormal
			}
		case tfOsc:
			st := b == '\\' && len(f.buf) > 0 && f.buf[len(f.buf)-1] == 0x1b
			if b == 0x07 || st {
				// Terminator (BEL, or the ST whose ESC is already in buf).
				if isTitleOSC(f.buf) {
					// rysh owns the title: the sequence is swallowed.
				} else {
					// Pass through with its terminator: BEL is the byte b,
					// for the ST the ESC is already in buf and b is the \.
					out = append(out, f.buf...)
					out = append(out, b)
				}
				f.buf = f.buf[:0]
				f.state = tfNormal
				continue
			}
			f.buf = append(f.buf, b)
			if len(f.buf) > oscMaxLen {
				// No terminator in sight: flush and resynchronize.
				out = append(out, f.buf...)
				f.buf = f.buf[:0]
				f.state = tfNormal
			}
		}
	}
	return out
}

// isTitleOSC reports whether buf (an OSC without its terminator, starting
// with its ESC byte) is a title-setting sequence: OSC 0 or OSC 2 with a
// ";" parameter separator.
func isTitleOSC(buf []byte) bool {
	return len(buf) >= 4 && buf[0] == 0x1b && buf[1] == ']' &&
		(buf[2] == '0' || buf[2] == '2') && buf[3] == ';'
}

// parseTitleResponse extracts the title from a terminal's answer to the
// OSC 1046 request: OSC 11 ; ? ; <title>, BEL or ST terminated. ok is
// false when the response has not fully arrived yet.
func parseTitleResponse(acc []byte) (string, bool) {
	const prefix = "\x1b]11;?;"
	j := bytes.Index(acc, []byte(prefix))
	if j < 0 {
		return "", false
	}
	rest := acc[j+len(prefix):]
	if b := bytes.IndexByte(rest, 0x07); b >= 0 {
		return string(rest[:b]), true
	}
	if b := bytes.Index(rest, []byte("\x1b\\")); b >= 0 {
		return string(rest[:b]), true
	}
	return "", false
}
