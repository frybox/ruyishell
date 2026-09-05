// Package markdown renders a streaming markdown stream to terminal escape
// sequences. It is written for chat-style replies that arrive token by
// token: text is styled incrementally, and lines that end with an unclosed
// construct (a bold opener, an unclosed inline code span, a fence opener)
// are held back until the construct closes or the stream ends, so raw
// markdown markers never flash on screen. Output uses \n as the row
// separator; the caller converts it to \r\n for the raw terminal.
package markdown

import (
	"strings"
	"unicode"

	"ruyishell/internal/theme"
)

// SGR attribute groups the renderer emits. Attributes are combined by
// re-emitting the full set after each transition; bold and italic stack
// because SGR codes are cumulative until a reset. The colored surfaces
// (inline code, dim markers, code blocks) are theme-aware: they read from
// the active theme so their contrast adapts to the terminal's background
// polarity and color capability instead of fixed codes.
const (
	bold      = "\x1b[1m"
	italic    = "\x1b[3m"
	underline = "\x1b[4m"
	reset     = "\x1b[0m"
)

// inlineCode is the inline-code / list-marker foreground (theme-aware).
func inlineCode() string { return theme.Current().InlineFg() }

// dim is the blockquote-marker / horizontal-rule / link-url foreground
// (theme-aware). The old bare \x1b[2m was a low-contrast gray that was
// unreadable on several terminals; the theme picks a readable muted tone
// per background polarity.
func dim() string { return theme.Current().BlockFg() }

// codeBG is the fenced code block style: background fill plus foreground.
// The old fixed \x1b[100m (a dark gray that only works on a dark terminal)
// is replaced by a theme-aware bg+fg so code blocks stay readable on a
// light terminal too.
func codeBG() string {
	return theme.Current().CodeBG() + theme.Current().CodeFg()
}

// lineKind classifies the leading construct of the current line. It is
// decided from the first partial line and kept for the whole line so a
// heading or list marker split across chunks is not misread.
type lineKind int

const (
	kindNone       lineKind = iota
	kindNormal              // plain text
	kindBlockquote          // "> ..."
	kindHeading             // "## ..."
	kindList                // "- ...", "1. ..."
	kindHR                  // "---" etc.
)

// Renderer converts an incrementally-received markdown string into styled
// terminal output. Write consumes a chunk and returns the styled bytes it
// can emit now (possibly empty while a construct is held open); Close
// flushes anything still held. cur holds the current line's received
// runes; emitted counts how many of them were already written to the
// screen, so a line can be emitted in pieces without repeating text.
type Renderer struct {
	inCode  bool
	fence   string
	cur     []rune
	emitted int
	kind    lineKind
	prev    rune // last content rune emitted, for intraword emphasis
}

// New returns a Renderer ready for a new markdown stream.
func New() *Renderer {
	return &Renderer{}
}

// Write consumes a chunk of the markdown stream and returns the styled
// output that can be emitted immediately. Complete lines are rendered as
// they arrive; a trailing partial line is only emitted when no construct
// is open across it, otherwise it is held for the next chunk.
func (r *Renderer) Write(s string) string {
	r.cur = append(r.cur, []rune(s)...)
	var out strings.Builder
	for {
		idx := indexOfRune(r.cur, '\n')
		if idx < 0 {
			break
		}
		line := r.cur[:idx]
		r.cur = r.cur[idx+1:]
		row := true
		var styled string
		switch {
		case r.inCode:
			if r.isFenceClose(string(line)) {
				r.inCode = false
				r.fence = ""
				row = false
			} else {
				styled = codeLine(string(line))
			}
		case r.isFenceOpen(string(line)):
			r.inCode = true
			r.fence = string(fenceChar(string(line)))
			row = false
		default:
			styled = r.renderLineSuffix(line, true)
		}
		// Fence openers/closers are invisible: no row break. Blank lines
		// still advance the row even though they style to nothing.
		if row {
			out.WriteString(styled)
			out.WriteString("\n")
		}
		r.resetLine()
	}
	// A trailing partial line: emit it only when nothing is open across
	// it. Code-block lines are always held until the block closes.
	if len(r.cur) > 0 && !r.inCode {
		out.WriteString(r.renderLineSuffix(r.cur, false))
	}
	return out.String()
}

// Close flushes the held partial line (rendered as complete) and resets
// the renderer, returning the final styled output.
func (r *Renderer) Close() string {
	if len(r.cur) == 0 {
		return ""
	}
	line := string(r.cur)
	r.cur = nil
	if r.inCode {
		if r.isFenceClose(line) {
			r.inCode = false
			r.fence = ""
			return ""
		}
		r.inCode = false
		r.fence = ""
		return codeLine(line)
	}
	if r.isFenceOpen(line) {
		// A dangling opener never rendered; drop it rather than flash it.
		return ""
	}
	out := r.renderLineSuffix([]rune(line), true)
	r.resetLine()
	return out
}

// resetLine begins a fresh line: the current line's emission state is
// discarded. The accumulated runes are left for the caller to slice.
func (r *Renderer) resetLine() {
	r.emitted = 0
	r.kind = kindNone
	r.prev = 0
}

// renderLineSuffix styles the not-yet-emitted tail of the current line.
// complete is false while the line is still receiving tokens; in that case
// a tail ending in an open construct is held back so its markers do not
// flash raw. The caller appends the row separator.
func (r *Renderer) renderLineSuffix(ls []rune, complete bool) string {
	if len(ls) <= r.emitted {
		return ""
	}
	// A line of only spaces is blank; render nothing (the row separator
	// still advances the terminal).
	if r.emitted == 0 && strings.TrimSpace(string(ls)) == "" {
		return ""
	}
	var out strings.Builder
	if r.emitted == 0 && r.kind == kindNone {
		if isHR(string(ls)) {
			if complete {
				r.kind = kindHR
				out.WriteString(dim() + strings.TrimSpace(string(ls)) + reset)
				r.emitted = len(ls)
				return out.String()
			}
			// Hold a partial horizontal rule: it could still grow into a
			// plain or list line, so do not commit to dim styling yet.
			return ""
		}
		// A line that is only hashes is ambiguous until more arrives: it
		// could become a heading prefix, a bare heading, or plain text.
		if allHashes(ls) && !complete {
			return ""
		}
		r.kind = detectKind(ls)
		r.emitted = r.emitHead(&out, ls)
		if len(ls) <= r.emitted {
			r.prev = ls[len(ls)-1]
			return out.String()
		}
	} else if r.kind == kindHeading {
		// The "#..." head is consumed silently; a heading prefix split
		// across chunks must not leak visible hashes.
		if hl := len(headingPrefix(string(ls))); hl > r.emitted {
			r.emitted = hl
			if len(ls) <= r.emitted {
				return out.String()
			}
		}
	}
	suffix := ls[r.emitted:]
	if !complete && openInline(suffix, r.prev) {
		return out.String() // hold: a construct is open at the line end
	}
	styled, open := scanInline(suffix, r.prev, r.baseKinds())
	out.WriteString(styled)
	// A complete line always resets (no style may leak to the next row);
	// a partial line resets only when it ends with a style still active
	// (headings), so plain mid-word chunks stay reset-free on the stream.
	if complete || open {
		out.WriteString(reset)
	}
	r.emitted = len(ls)
	r.prev = ls[len(ls)-1]
	return out.String()
}

// baseKinds are the styles that stay active across the whole line content.
// Headings render bold+underline; other lines start with no style.
func (r *Renderer) baseKinds() []byte {
	if r.kind == kindHeading {
		return []byte{'b', 'u'}
	}
	return nil
}

// emitHead writes the styled leading marker(s) of the current line and
// returns how many runes of ls they consumed. Heading prefixes are
// consumed without output; blockquote and list markers are styled.
func (r *Renderer) emitHead(out *strings.Builder, ls []rune) int {
	switch r.kind {
	case kindHeading:
		return len([]rune(headingPrefix(string(ls))))
	case kindBlockquote:
		i, n := 0, 0
		for i < len(ls) && ls[i] == '>' {
			out.WriteString(dim() + ">" + reset)
			i++
			n++
			if i < len(ls) && ls[i] == ' ' {
				out.WriteString(" ")
				i++
				n++
			}
		}
		return n
	case kindList:
		if m, _ := listMarker(string(ls)); m != "" {
			out.WriteString(inlineCode() + m + reset)
			return len([]rune(m))
		}
	}
	return 0
}

// detectKind classifies the leading construct of ls. It runs once per
// line, on the first partial, so a split marker is not misread.
func detectKind(ls []rune) lineKind {
	s := string(ls)
	if strings.HasPrefix(s, ">") {
		return kindBlockquote
	}
	if headingPrefix(s) != "" {
		return kindHeading
	}
	if m, _ := listMarker(s); m != "" {
		return kindList
	}
	return kindNormal
}

// allHashes reports whether every rune of ls is a '#'. Such a partial line
// is held until a non-hash rune or the row end decides its kind.
func allHashes(ls []rune) bool {
	for _, r := range ls {
		if r != '#' {
			return false
		}
	}
	return true
}

// codeLine renders one line of a fenced code block on a background fill.
func codeLine(l string) string {
	return codeBG() + l + reset
}

// isFenceOpen reports whether l opens a fenced code block: up to three
// leading spaces, then a run of at least three backticks or tildes.
func (r *Renderer) isFenceOpen(l string) bool {
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(l) {
		return false
	}
	ch := l[i]
	if ch != '`' && ch != '~' {
		return false
	}
	for i < len(l) && l[i] == ch {
		i++
	}
	if i < 3 {
		return false
	}
	// A tilde fence allows only trailing spaces; a backtick fence may carry
	// an info string (e.g. ```go).
	if ch == '~' {
		for i < len(l) {
			if l[i] != ' ' {
				return false
			}
			i++
		}
	}
	return true
}

// isFenceClose reports whether l closes the currently open fence: at least
// as many of the fence character, then only trailing spaces.
func (r *Renderer) isFenceClose(l string) bool {
	if r.fence == "" {
		return false
	}
	i := 0
	for i < len(l) && l[i] == ' ' {
		i++
	}
	ch := r.fence[0]
	for i < len(l) && l[i] == ch {
		i++
	}
	if i < len(r.fence) {
		return false
	}
	for i < len(l) {
		if l[i] != ' ' {
			return false
		}
		i++
	}
	return true
}

// fenceChar returns the fence character of an opening fence line.
func fenceChar(l string) byte {
	for i := 0; i < len(l); i++ {
		if l[i] == '`' || l[i] == '~' {
			return l[i]
		}
	}
	return '`'
}

// headingPrefix returns the "#..." header prefix of l (including the
// trailing space, or the bare hashes when the line is only hashes), or ""
// when l is not an ATX heading.
func headingPrefix(l string) string {
	n := 0
	for n < len(l) && n < 6 && l[n] == '#' {
		n++
	}
	if n == 0 {
		return ""
	}
	if n == len(l) {
		return l
	}
	if l[n] != ' ' {
		return ""
	}
	return l[:n+1]
}

// isHR reports whether s is a thematic break: at least three of the same
// -, * or _ character with only spaces between them.
func isHR(s string) bool {
	seen := 0
	var ch byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			continue
		}
		if c != '-' && c != '*' && c != '_' {
			return false
		}
		if seen == 0 {
			ch = c
		} else if c != ch {
			return false
		}
		seen++
	}
	return seen >= 3
}

// listMarker returns the leading list marker of l (dash/plus/asterisk or
// an ordered "N." / "N)" followed by a space) and the remaining content.
func listMarker(l string) (marker, rest string) {
	for _, m := range []string{"- ", "+ ", "* "} {
		if strings.HasPrefix(l, m) {
			return m, l[len(m):]
		}
	}
	i := 0
	for i < len(l) && l[i] >= '0' && l[i] <= '9' {
		i++
	}
	if i == 0 || i > 9 || i+1 >= len(l) {
		return "", l
	}
	if (l[i] == '.' || l[i] == ')') && l[i+1] == ' ' {
		return l[:i+2], l[i+2:]
	}
	return "", l
}

// openInline reports whether an inline construct is still open at the end
// of runes (an unclosed code span, link text, trailing backslash, or an
// emphasis opener), which would break if the tail were emitted now.
// firstPrev is the rune emitted just before runes, for intraword checks.
func openInline(runes []rune, firstPrev rune) bool {
	n := len(runes)
	inCode := false
	escaped := false
	bold := false
	italic := false
	link := false
	for i := 0; i < n; i++ {
		c := runes[i]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
			continue
		case '`':
			inCode = !inCode
			continue
		case '[':
			link = true
			continue
		case ')':
			link = false
			continue
		}
		if inCode {
			continue
		}
		if c != '*' && c != '_' {
			continue
		}
		double := i+1 < n && runes[i+1] == c
		nextIdx := i + 1
		if double {
			nextIdx = i + 2
		}
		var prevChar rune
		if i > 0 {
			prevChar = runes[i-1]
		} else {
			prevChar = firstPrev
		}
		intraword := isWordRune(prevChar) && nextIdx < n && isWordRune(runes[nextIdx])
		isOpener, isCloser := delimFlanking(runes, nextIdx, prevChar)
		if nextIdx >= n {
			// At the end of a partial line the delimiter may still grow
			// into an opener; hold it rather than flash it as literal.
			isOpener = true
			isCloser = false
		}
		isOpener = isOpener && !intraword
		isCloser = isCloser && !intraword
		if double {
			if isOpener && !bold {
				bold = true
			} else if isCloser && bold {
				bold = false
			}
			i++
		} else {
			if isOpener && !italic {
				italic = true
			} else if isCloser && italic {
				italic = false
			}
		}
	}
	// A trailing backtick run that is odd (an open code span) or exactly
	// two (a possible fence opener prefix) is held so raw markers never
	// flash; a link opened with '[' stays held until ')' closes it.
	trailing := 0
	for k := n - 1; k >= 0 && runes[k] == '`'; k-- {
		trailing++
	}
	if trailing%2 == 1 || trailing == 2 {
		inCode = true
	}
	return inCode || link || escaped || bold || italic
}

// scanInline styles the inline markdown constructs in runes: emphasis
// (**bold**, *italic*, __bold__, _italic_), inline code spans, links
// [text](url), and backslash escapes. Emphasis markers switch styles and
// are not themselves printed; a line-local construct left unclosed renders
// with the opener treated as literal text. The second return reports
// whether a style is still active at the end (the caller resets then).
// baseKinds carries line-level styles (headings) that stay active for the
// whole content; firstPrev is the rune emitted just before runes.
func scanInline(runes []rune, firstPrev rune, baseKinds []byte) (string, bool) {
	n := len(runes)
	kinds := append([]byte{}, baseKinds...)
	lastKinds := []byte{}
	var out strings.Builder
	var buf strings.Builder

	// flush emits the buffered plain text under the current style set,
	// only when the style set changed since the last flush.
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		if !sameKinds(lastKinds, kinds) {
			out.WriteString(styleOf(kinds))
			lastKinds = append(lastKinds[:0], kinds...)
		}
		out.WriteString(buf.String())
		buf.Reset()
	}

	i := 0
	for i < n {
		c := runes[i]
		switch {
		case c == '\\':
			if i+1 < n {
				buf.WriteRune(runes[i+1])
				i += 2
			} else {
				buf.WriteRune(c)
				i++
			}
		case c == '`':
			j := i + 1
			for j < n && runes[j] != '`' {
				j++
			}
			if j < n && j > i+1 {
				flush()
				out.WriteString(inlineCode() + string(runes[i+1:j]) + reset)
				lastKinds = nil
				i = j + 1
			} else {
				buf.WriteRune(c)
				i++
			}
		case c == '[':
			j := i + 1
			for j < n && runes[j] != ']' {
				j++
			}
			if j < n && j+1 < n && runes[j+1] == '(' {
				k := j + 2
				for k < n && runes[k] != ')' {
					k++
				}
				if k < n {
					flush()
					text := string(runes[i+1 : j])
					url := string(runes[j+2 : k])
					out.WriteString(underline)
					inner, _ := scanInline([]rune(text), 0, nil)
					out.WriteString(inner)
					out.WriteString(reset)
					out.WriteString(dim() + " (" + url + ")" + reset)
					lastKinds = nil
					i = k + 1
					continue
				}
			}
			buf.WriteRune(c)
			i++
		case c == '*' || c == '_':
			double := i+1 < n && runes[i+1] == c
			nextIdx := i + 1
			if double {
				nextIdx = i + 2
			}
			kind := byte('i')
			if double {
				kind = 'b'
			}
			var prevChar rune
			if i > 0 {
				prevChar = runes[i-1]
			} else {
				prevChar = firstPrev
			}
			intraword := isWordRune(prevChar) && nextIdx < n && isWordRune(runes[nextIdx])
			isOpener, isCloser := delimFlanking(runes, nextIdx, prevChar)
			isOpener = isOpener && !intraword
			isCloser = isCloser && !intraword
			active := hasKind(kinds, kind)
			switch {
			case isOpener && !active:
				flush()
				kinds = append(kinds, kind)
				i = nextIdx
			case isCloser && active:
				flush()
				kinds = removeKind(kinds, kind)
				i = nextIdx
			default:
				buf.WriteRune(c)
				if double {
					buf.WriteRune(c)
				}
				i = nextIdx
			}
		default:
			buf.WriteRune(c)
			i++
		}
	}
	flush()
	return out.String(), len(lastKinds) > 0
}

// styleOf maps the active style kinds to an SGR sequence; an empty set
// resets so styles never leak past their span.
func styleOf(kinds []byte) string {
	if len(kinds) == 0 {
		return reset
	}
	var codes []string
	for _, k := range kinds {
		switch k {
		case 'b':
			codes = append(codes, "1")
		case 'i':
			codes = append(codes, "3")
		case 'u':
			codes = append(codes, "4")
		}
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func sameKinds(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasKind(kinds []byte, k byte) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func removeKind(kinds []byte, k byte) []byte {
	for i := len(kinds) - 1; i >= 0; i-- {
		if kinds[i] == k {
			return append(kinds[:i], kinds[i+1:]...)
		}
	}
	return kinds
}

func indexOfRune(s []rune, r rune) int {
	for i, c := range s {
		if c == r {
			return i
		}
	}
	return -1
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t'
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isWordRune reports whether r is an ASCII letter or digit. The intraword
// emphasis rule (foo_bar stays literal) only guards ASCII identifiers; in
// CJK text a marker between letters (加**粗**文) is intended emphasis.
func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isPunct(r rune) bool {
	return !isAlnum(r) && r != ' ' && r != '\t'
}

// delimFlanking classifies a delimiter run that ends before nextIdx, with
// prevChar the rune immediately before it, using CommonMark's left/right
// flanking rules. A run can be both (nested emphasis) or neither.
func delimFlanking(runes []rune, nextIdx int, prevChar rune) (opener, closer bool) {
	n := len(runes)
	prevWS := prevChar == 0 || isSpace(prevChar)
	prevPunct := prevChar != 0 && isPunct(prevChar)
	nextWS := nextIdx >= n || isSpace(runes[nextIdx])
	nextPunct := nextIdx < n && isPunct(runes[nextIdx])
	left := !nextWS && (!nextPunct || prevWS || prevPunct)
	right := !prevWS && (!prevPunct || nextWS || nextPunct)
	return left, right
}
