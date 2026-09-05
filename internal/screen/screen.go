// Package screen builds the terminal escape sequences rysh uses to render its
// single shared screen. Since the 主屏方案 it draws on the main screen only,
// with native scrollback and no bottom status row: what the session driver
// actually uses from here are the cursor save/restore, DECSCUSR and
// color constants, the colour-reset/clear helpers for shutdown, the PS-style
// AI prompt expansion, and the alt-screen Detector that pauses recording while
// a fullscreen program owns the terminal. The status-row renderers
// (ShellStatus, AIStatus, ScrollRegion, CursorStyleSeq) are kept as tested
// building blocks but are no longer called (主屏方案 §7.1).
package screen

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"

	"ruyishell/internal/theme"
)

// Cursor save/restore (DECSC / DECRC). Shell-status drawing saves and
// restores around itself so the shell's own cursor is undisturbed.
const (
	SaveCursor    = "\x1b7"
	RestoreCursor = "\x1b8"
)

// DECSCUSR cursor styles.
const (
	BarCursor   = "\x1b[5 q" // blinking bar: shell mode
	BlockCursor = "\x1b[2 q" // solid block: AI mode
	ResetCursor = "\x1b[0 q" // terminal default
)

// DimGray switches to a muted foreground (theme-aware). It is used for the
// reasoning block and for tool/notice lines, which read as secondary text
// in the stream. The old bare \x1b[2m produced a low-contrast gray that was
// unreadable on several terminals; the theme picks a readable muted tone
// per background polarity.
func DimGray() string { return theme.Current().DimGray() }

// DimItalic switches to a muted + italic foreground. Used for reasoning
// block content: dim de-emphasizes the tone, italic adds an extra visual
// cue that this is thinking, not the final answer.
func DimItalic() string { return theme.Current().DimItalic() }

// approvalStyle is the §11.3 approval row: bold black text on an amber bar,
// so the ask prompt reads as "needs your attention" at a glance even when it
// is buried at the end of a long reply. Like statusBarStyle it is applied
// before EraseToEOL, so the erased cells pick up the bar background; see
// ApprovalBar.
func approvalStyle() string { return theme.Current().ApprovalStyle() }

// ColorReset clears all SGR attributes (color, style, dim).
const ColorReset = "\x1b[0m"

// SelectedBg is a slightly raised background used to highlight the
// active/selected row of a list (the > line of the model list, the
// current session row of /ls). It is applied to the text and then followed
// by EraseToEOL so the bar fills to the right margin (the trailing cells
// pick up the current background). The bar is theme-aware: on a dark
// terminal it is a clearly-visible raised gray; on a light terminal a
// sunken light gray, so the selected row reads in both polarities instead
// of a fixed gray that vanishes on light backgrounds.
func SelectedBg() string { return theme.Current().SelectedBg() }

// SetTitle returns the OSC 2 sequence setting the terminal's window/tab
// title. The ST (ESC \) terminator is more reliable than BEL across
// terminals. The shell's own title-setting prompts compete for the same
// slot, so while rysh runs it re-asserts this (prompt marker, 主屏方案 §3.2
// 修订) and strips the shell's OSC 0/2 from the pty stream.
func SetTitle(title string) string { return "\x1b]2;" + title + "\x1b\\" }

// RequestTitle returns the OSC 1046 request for the terminal's current
// window title (xterm and compatibles). The answer, when the terminal
// supports the query, arrives on input as OSC 11 ; ? ; <title> (BEL or ST
// terminated); terminals that do not support it stay silent.
func RequestTitle() string { return "\x1b]1046;?\x1b\\" }

// statusBarStyle is the status-row style: high-contrast text on a raised
// bar, so the pinned bottom row reads as a bar distinct from the session
// stream. It is applied before EraseToEOL (which fills the erased cells
// with the current background) and re-applied after each badge, whose own
// reset would otherwise clear the bar background for the following text.
// Theme-aware: dark text on a light bar / light text on a dark bar.
func statusBarStyle() string { return theme.Current().StatusBarStyle() }

var (
	// promptSH is the shell-mode status chip: black [SH] on a filled green
	// badge, so the mode reads at a glance.
	promptSH = func() string {
		return theme.Current().ChipSH() + "[SH]" + ColorReset
	}
	// promptAI is the AI-mode status chip: black [AI] on a filled magenta
	// badge.
	promptAI = func() string {
		return theme.Current().ChipAI() + "[AI]" + ColorReset
	}
)

// MoveTo returns a cursor-positioning sequence for a 1-based row and column.
func MoveTo(row, col int) string {
	return fmt.Sprintf("\x1b[%d;%dH", row, col)
}

// MoveToRow moves to the given 1-based row, column 1.
func MoveToRow(row int) string {
	return fmt.Sprintf("\x1b[%dH", row)
}

// EraseToEOL clears from the cursor to the end of the line.
func EraseToEOL() string { return "\x1b[K" }

// SelectedLine renders one list row with normal foreground text on the
// SelectedBg gray bar: the bar fills to the right margin (the trailing
// cells pick up the current background) and the style is reset after. It
// is used for the > (active/selected) row of the model and session
// lists.
func SelectedLine(text string) string {
	return SelectedBg() + text + EraseToEOL() + ColorReset
}

// ApprovalBar renders the §11.3 approval prompt as a highlighted row that
// fills to the right margin: the amber background survives EraseToEOL, so the
// bar is unmistakable rather than a short colored run of text. The caller owns
// the row — the bar must start at column 1 of its own line, never glued after
// streamed reply text or painted over by the inline spinner.
func ApprovalBar(text string) string {
	return approvalStyle() + text + EraseToEOL() + ColorReset
}

// ScrollRegion returns the DECSTBM sequence confining scrolling to rows
// 1..n, protecting the status row below it. It also homes the cursor.
func ScrollRegion(n int) string { return fmt.Sprintf("\x1b[1;%dr", n) }

// ResetScrollRegion clears the scroll region (full-screen scroll).
func ResetScrollRegion() string { return "\x1b[r" }

// EnterAltScreen switches to the alternate screen: the cursor is saved, the
// alternate buffer is cleared and the cursor homed, and the normal screen's
// content is preserved underneath. Since 主屏方案 rysh never enters it — it
// renders on the main screen from startup to exit (§3.1) — and the sequence is
// only recognized in incoming output by the Detector; this renderer is kept as
// a tested building block.
func EnterAltScreen() string { return "\x1b[?1049h" }

// ExitAltScreen leaves the alternate screen, restoring the normal screen.
// rysh does not send it (see EnterAltScreen); a fullscreen program sends it
// itself and the Detector only watches for it.
func ExitAltScreen() string { return "\x1b[?1049l" }

// ShowCursor re-enables the cursor. Reset sends it on handoff, so a program
// that hid the cursor while running cannot leave the terminal unusable.
func ShowCursor() string { return "\x1b[?25h" }

// HideCursor disables the cursor. AI mode sends it while a task is busy
// (the thinking spinner and the streaming reply are the live indicators, so
// a blinking cursor at the stream tail is redundant) and pairs with the
// ShowCursor sent when the task settles.
func HideCursor() string { return "\x1b[?25l" }

// CursorStyle selects the shell-mode terminal cursor.
type CursorStyle string

const (
	// CursorBar is the default shell-mode cursor: a blinking bar.
	CursorBar CursorStyle = "bar"
	// CursorBlock is a steady solid block.
	CursorBlock CursorStyle = "block"
	// CursorDefault leaves the terminal's own cursor unchanged.
	CursorDefault CursorStyle = "default"
)

// CursorStyleSeq returns the DECSCUSR sequence for a shell-mode cursor
// style. "" maps to CursorBar.
func CursorStyleSeq(s CursorStyle) string {
	switch s {
	case CursorBlock:
		return BlockCursor
	case CursorDefault:
		return "" // leave the terminal's cursor untouched
	default:
		return BarCursor
	}
}

// ShellStatus renders the bottom status row in shell mode: the [SH] chip,
// the active session id, and an optional inline message, all on a light
// status-bar background filling the full row. It saves and restores the
// cursor so the shell's own cursor position is undisturbed.
func ShellStatus(height int, style CursorStyle, session, msg string) string {
	body := promptSH() + statusBarStyle()
	if session != "" {
		body += " " + session
	}
	if msg != "" {
		body += " │ " + msg
	}
	return SaveCursor + MoveToRow(height) + statusBarStyle() + EraseToEOL() + body + ColorReset + CursorStyleSeq(style) + RestoreCursor
}

// AIStatus renders the bottom status row in AI mode: the [AI] chip, the
// active session id, the model, the session state ("idle"/"running"/...),
// an optional spinner glyph, and an optional inline message, all on the
// light status-bar background filling the full row. The block cursor is
// used so the AI mode reads distinctly from the shell's bar cursor. It
// saves and restores the cursor like ShellStatus does.
func AIStatus(height int, session, model, state, spinner, msg string) string {
	body := promptAI() + statusBarStyle()
	if session != "" {
		body += " " + session
	}
	if model != "" {
		body += " " + model
	}
	body += " · " + state
	if spinner != "" {
		body += " " + spinner
	}
	if msg != "" {
		body += " │ " + msg
	}
	return SaveCursor + MoveToRow(height) + statusBarStyle() + EraseToEOL() + body + ColorReset + BlockCursor + RestoreCursor
}

// CursorUp returns the relative cursor-up sequence for n rows. The terminal
// clamps at the top row, so n=0 yields "".
func CursorUp(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dA", n)
}

// CursorDown returns the relative cursor-down sequence for n rows. The
// terminal clamps at the bottom margin, so n=0 yields "".
func CursorDown(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dB", n)
}

// CursorForward returns the relative cursor-forward (right) sequence for n
// cells. n=0 yields "".
func CursorForward(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\x1b[%dC", n)
}

// PSExpand expands the bash-PS-style prompt escapes in s (the [ai] prompt
// config): \u user, \h hostname, \w current directory (HOME shown as ~),
// \t time (HH:MM:SS), \\ literal backslash, \[...\] a non-printing region
// copied verbatim but excluded from the returned visible width, and \x1b or
// \e the ESC byte (so SGR sequences survive config text and the raw-string
// default, which cannot hold a raw control byte). It returns the expanded
// prompt string and its visible width in cells (wide runes count double;
// escape sequences do not count at all).
//
// \m (the active model ref) is not expanded here: PSExpandModel adds it.
func PSExpand(s, dir string) (string, int) {
	return psExpand(s, dir, "")
}

// PSExpandModel expands s like PSExpand, with \m replaced by the active model
// ref (empty when no model is configured, so the token renders nothing).
func PSExpandModel(s, dir, model string) (string, int) {
	return psExpand(s, dir, model)
}

func psExpand(s, dir, model string) (string, int) {
	s = expandEsc(s)
	var vis strings.Builder // visible cells only, for the width
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '[':
				// Non-printing region: copy up to "\]" verbatim, add no width.
				j := strings.Index(s[i+2:], "\\]")
				if j < 0 {
					b.WriteString(s[i+2:])
					i = len(s)
					continue
				}
				b.WriteString(s[i+2 : i+2+j])
				i += 2 + j + 2
				continue
			case 'u':
				b.WriteString(currentUser())
				vis.WriteString(currentUser())
				i += 2
				continue
			case 'h':
				b.WriteString(hostname())
				vis.WriteString(hostname())
				i += 2
				continue
			case 'w':
				d := dir
				if d == "" {
					d = "?"
				}
				if home, err := os.UserHomeDir(); err == nil && home != "" {
					if d == home {
						d = "~"
					} else if strings.HasPrefix(d, home+"/") {
						d = "~" + strings.TrimPrefix(d, home)
					}
				}
				b.WriteString(d)
				vis.WriteString(d)
				i += 2
				continue
			case 'm':
				b.WriteString(model)
				vis.WriteString(model)
				i += 2
				continue
			case 't':
				t := time.Now().Format("15:04:05")
				b.WriteString(t)
				vis.WriteString(t)
				i += 2
				continue
			case '\\':
				b.WriteString("\\")
				vis.WriteString("\\")
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		vis.WriteByte(s[i])
		i++
	}
	return b.String(), ansi.StringWidth(vis.String())
}

// expandEsc converts the \x1b and \e spellings of the ESC byte into a real
// ESC byte (0x1b) for PSExpand. It leaves \\ pairs intact so the main loop
// still emits a literal backslash (and a \[...\] region stays a region), and
// it only rewrites the string when one of the spellings is actually present.
func expandEsc(s string) string {
	if !strings.Contains(s, `\x1b`) && !strings.Contains(s, `\e`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			switch {
			case s[i+1] == '\\':
				b.WriteString(`\\`)
				i += 2
				continue
			case s[i+1] == 'x' && i+3 < len(s) && s[i+2] == '1' && s[i+3] == 'b':
				b.WriteByte(0x1b)
				i += 4
				continue
			case s[i+1] == 'e':
				b.WriteByte(0x1b)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

var (
	userCache  string
	hostCache  string
	userCacheR bool
	hostCacheR bool
)

func currentUser() string {
	if !userCacheR {
		if u, err := user.Current(); err == nil {
			userCache = u.Username
		} else if un := os.Getenv("USER"); un != "" {
			userCache = un
		} else if un := os.Getenv("LOGNAME"); un != "" {
			userCache = un
		}
		userCacheR = true
	}
	return userCache
}

func hostname() string {
	if !hostCacheR {
		if h, err := os.Hostname(); err == nil {
			hostCache = h
		}
		hostCacheR = true
	}
	return hostCache
}

// ClearScreen clears the entire screen and homes the cursor. Since 主屏方案
// rysh sends it nowhere on the exit path — the session stream stays in the
// terminal's scrollback (see Reset) — so it is kept only as a tested building
// block.
func ClearScreen() string { return "\x1b[2J" + MoveTo(1, 1) }

// Reset restores the terminal to a clean state for handoff back to the user
// when rysh exits: colors off, cursor style reset to the terminal default,
// and the cursor shown again. rysh runs on the main screen, so it never
// enters the alternate screen and never clears it — the contents it printed
// stay in the terminal's scrollback (like any other command's output), and
// the user's shell prompt simply follows on the next line.
func Reset() string {
	return "\x1b[0m" + ResetCursor + ShowCursor()
}

// Action tells a caller how the status line / scroll region would need to
// react to a chunk of child output. On the main screen the session driver
// reads only Detector.Alt() — to pause and resume keystroke recording around
// a fullscreen program — and ignores the Action; the values below describe the
// pinned-status-row layout the detector was built for (主屏方案 §7.1).
type Action uint8

const (
	// ActionNone means the output needs no reaction.
	ActionNone Action = iota
	// ActionRedraw means a clear wiped the status row; redraw it.
	ActionRedraw
	// ActionRelease means a fullscreen app entered the alternate screen:
	// keep the scroll region asserted and repaint the status row, so the
	// app's screen is rows 1..h-1 (tmux-style) and the status row stays
	// visible — entering the alt screen cleared the whole alt buffer,
	// status row included.
	ActionRelease
	// ActionReapply means the alternate screen was left: re-own it by
	// clearing, reapplying the scroll region, and redrawing the status row.
	ActionReapply
	// ActionReset means the terminal was reset (RIS) back to the normal
	// screen: re-enter the alternate screen and rebuild rysh's interface.
	ActionReset
)

// seqKind classifies a recognized escape sequence by the reaction it drives.
type seqKind int

const (
	kindEnter seqKind = iota // a fullscreen app takes the alternate screen
	kindExit                 // it leaves the alternate screen
	kindClear                // a clear that wipes the status row
	kindRIS                  // full terminal reset back to the normal screen
)

// The escape sequences the Detector recognizes. A fullscreen app enters and
// leaves the alternate screen with ?1049h/?1049l, or the older ?1047h/?47h
// and ?1047l/?47l forms; a clear wipes the status row; RIS (ESC c) resets
// the whole terminal.
var knownSeqs = []struct {
	kind seqKind
	str  string
	raw  []byte
}{
	{kindEnter, "\x1b[?1049h", []byte("\x1b[?1049h")},
	{kindEnter, "\x1b[?1047h", []byte("\x1b[?1047h")},
	{kindEnter, "\x1b[?47h", []byte("\x1b[?47h")},
	{kindExit, "\x1b[?1049l", []byte("\x1b[?1049l")},
	{kindExit, "\x1b[?1047l", []byte("\x1b[?1047l")},
	{kindExit, "\x1b[?47l", []byte("\x1b[?47l")},
	{kindClear, "\x1b[2J", []byte("\x1b[2J")},
	{kindClear, "\x1b[3J", []byte("\x1b[3J")},
	{kindClear, "\x1b[J", []byte("\x1b[J")},
	{kindRIS, "\x1b c", []byte("\x1b c")},
}

// Detector inspects child output and reports the reaction the session loop
// must take. Rysh forwards every pty byte to the terminal untouched, so a
// recognized sequence anywhere in a chunk actually takes effect on the
// screen — the Detector therefore scans whole chunks in stream order and
// carries a trailing partial sequence into the next chunk, mirroring the
// terminal's own byte stream. It is stateful: it tracks whether a fullscreen
// app is currently occupying the alternate screen. Feed runs on the output
// loop while Reset runs on the driver loop, so all methods are mutex-guarded.
type Detector struct {
	mu   sync.Mutex
	pend []byte // trailing partial sequence awaiting the next chunk
	alt  bool
}

// NewDetector returns a Detector starting outside the alternate screen.
func NewDetector() *Detector {
	return &Detector{}
}

// Alt reports whether a fullscreen (alternate-screen) app is active.
func (d *Detector) Alt() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alt
}

// Feed hands the latest chunk of child output to the detector and returns
// the reaction for that chunk and the sequence that decided it ("" for
// ActionNone and ActionRedraw). cut is the byte offset of that sequence
// within p, or -1 when the sequence completed from a partial tail carried
// over from an earlier chunk.
func (d *Detector) Feed(p []byte) (Action, string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	held := len(d.pend)
	buf := append(d.pend, p...)
	d.pend = nil
	if len(buf) == 0 {
		return ActionNone, "", -1
	}

	// Scan the whole chunk: the bytes are forwarded to the terminal, so a
	// sequence mid-chunk (e.g. a fullscreen app's exit followed by the
	// shell's prompt in one read) takes effect just like one at the tail.
	startAlt := d.alt
	relSeq, relPos := "", -1 // the enter that turned the alt screen on
	repSeq, repPos := "", -1 // the exit that turned it off
	resSeq, resPos := "", -1 // a full terminal reset
	clearPlain := false      // a clear that wipes the status row

scan:
	for i := 0; i < len(buf); i++ {
		if buf[i] != 0x1b {
			continue
		}
		for j := range knownSeqs {
			ks := &knownSeqs[j]
			if len(buf)-i < len(ks.raw) || !bytes.Equal(buf[i:i+len(ks.raw)], ks.raw) {
				continue
			}
			switch ks.kind {
			case kindEnter:
				if !d.alt {
					d.alt = true
					if relPos < 0 {
						relSeq, relPos = ks.str, i
					}
				}
			case kindExit:
				if d.alt {
					d.alt = false
					repSeq, repPos = ks.str, i
				}
			case kindClear:
				// A clear wipes the status row — on the normal screen and
				// on the alternate screen alike. rysh always owns row h,
				// so a fullscreen app's erase-display must not be allowed
				// to blank it; the session loop repaints it the way tmux
				// redraws its status bar after a fullscreen program clears
				// the screen.
				clearPlain = true
			case kindRIS:
				d.alt = false
				resSeq, resPos = ks.str, i
			}
			i += len(ks.raw)
			continue scan
		}
	}

	// A recognized sequence may end exactly at this chunk's end: keep its
	// partial tail so the next chunk is examined as the terminal sees it.
	if n := carryLen(buf); n > 0 {
		d.pend = append(d.pend, buf[len(buf)-n:]...)
	}

	toCut := func(pos int) int {
		if pos < held {
			return -1
		}
		return pos - held
	}
	switch {
	case resSeq != "":
		// A full reset wins: the terminal is back on the normal screen,
		// so rysh re-enters the alternate screen and rebuilds everything.
		return ActionReset, resSeq, toCut(resPos)
	case d.alt && !startAlt:
		// A fullscreen app now owns the alternate screen: the alt-screen
		// switch cleared the status row, so repaint it; the scroll
		// region stays asserted so its screen stays rows 1..h-1.
		return ActionRelease, relSeq, toCut(relPos)
	case !d.alt && (startAlt || relSeq != ""):
		// The alternate screen was left (or claimed and released within
		// this chunk): re-own the screen and repaint the status row.
		return ActionReapply, repSeq, toCut(repPos)
	case clearPlain:
		// A clear wiped the status row — on the normal screen or inside a
		// fullscreen app's alternate screen alike — so repaint it.
		return ActionRedraw, "", -1
	default:
		return ActionNone, "", -1
	}
}

// carryLen returns the length of the longest suffix of buf that is a proper
// prefix of a recognized sequence — the bytes a following chunk must be
// re-examined with. 0 when no recognized sequence can still complete.
func carryLen(buf []byte) int {
	max := len(buf)
	if max > 7 { // the longest recognized sequence is 8 bytes
		max = 7
	}
	for n := max; n > 0; n-- {
		tail := buf[len(buf)-n:]
		for j := range knownSeqs {
			ks := &knownSeqs[j]
			if len(ks.raw) > n && bytes.Equal(ks.raw[:n], tail) {
				return n
			}
		}
	}
	return 0
}

// Reset drops the detector back to its initial state.
func (d *Detector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pend = nil
	d.alt = false
}
