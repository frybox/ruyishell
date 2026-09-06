// Package session tracks the user's shell activity — the commands they run
// in shell mode and the output the terminal shows after each — as a unified
// event stream the AI agent can draw on for context. Each event carries its
// own working directory (cwd-as-data), so the session is not bound to any
// single directory (M4).
package session

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"ruyishell/internal/secrets"
)

// ShellEvent is one command the user ran in shell mode: the command line,
// the directory it ran in (cwd-as-data), and the terminal output that
// followed (truncated, ANSI stripped when formatted).
type ShellEvent struct {
	Dir     string
	Command string
	Output  string
	Time    time.Time
}

// Limits keep the unified event stream bounded so it cannot balloon the
// context sent to the model.
const (
	// MaxEvents is the number of shell events retained in the ring.
	MaxEvents = 20
	// MaxOutput is the per-event output capture cap, in bytes. The cap is
	// split between the head and the tail (each shellOutHalf): like the
	// bash tool's truncateMid, the middle of a long stream is dropped
	// because both ends carry the signal (the command's first lines and
	// its final result/errors).
	MaxOutput = 4 * 1024
	// MaxContext is the total byte budget for the shell events sent in one
	// request's context; when the budget is exceeded the oldest events are
	// collapsed to one-line placeholders (they are interleaved
	// chronologically with the AI conversation, one system message per
	// event), not dropped — see CollapseShellEvents.
	MaxContext = 8 * 1024
	// shellOutHalf is the head/tail of a shell event's output kept verbatim
	// when the stream overruns MaxOutput (MaxOutput/2 each).
	shellOutHalf = MaxOutput / 2
)

// Recorder assembles shell-mode keystrokes into commands and captures the
// output that follows each one into a bounded ring of recent events. It is
// fed from two goroutines (the input and output loops); all methods are
// safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	events []ShellEvent
	line   []rune // command line currently being typed
	cur    int    // cursor offset within line
	// The open event's output stream is kept as a head+tail window so a
	// long stream drops its middle: head holds the first shellOutHalf
	// bytes, tail the last shellOutHalf bytes of the whole stream, and
	// seen counts every byte fed, so the elided middle is derivable.
	head       []byte
	tail       []byte
	seen       int
	curIdx     int  // index of the event capturing output, -1 when none
	paused     bool // true while a fullscreen program owns the terminal
	paste      bool // true while inside a DEC 2004 paste block (PasteStart..End)
	unreliable bool // the line used a readline feature the recorder cannot
	// (tab completion, ↑/↓ history recall, ...), so its reconstructed text is
	// not what the shell actually has and must not be logged as a command.
}

// New returns an empty Recorder.
func New() *Recorder {
	return &Recorder{curIdx: -1}
}

// PasteStart begins a DEC 2004 (bracketed paste) block. From here until
// PasteEnd, pasted content is accumulated and inserted as a literal line
// block instead of being typed key by key; the embedded newlines stay in the
// line (readline holds them under the paste) rather than submitting partial
// commands.
func (r *Recorder) PasteStart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return
	}
	if r.curIdx >= 0 && len(r.line) == 0 {
		r.close()
	}
	r.paste = true
}

// PasteEnd ends a DEC 2004 paste block; the accumulated content is inserted
// as one atomic block at the cursor, so a multi-line paste becomes one line
// (readline's own model) rather than a run of submits.
func (r *Recorder) PasteEnd() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || !r.paste {
		return
	}
	r.paste = false
}

// PasteText appends a chunk of a paste block (rune text). It must be called
// between PasteStart and PasteEnd; the chunk is buffered at the cursor,
// mirroring how readline inserts a pasted block. If the chunk (or the block
// so far) contains a newline the line is flagged unreliable: readline holds
// a multi-line paste in a multi-line buffer and only the first line runs on
// the next Enter, so the recorder cannot reconstruct what the shell will
// actually run and must not log it.
func (r *Recorder) PasteText(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || !r.paste {
		return
	}
	if r.cur > len(r.line) {
		r.cur = len(r.line)
	}
	if strings.ContainsRune(s, '\n') {
		r.unreliable = true
	}
	runes := []rune(s)
	r.line = append(r.line, make([]rune, len(runes))...)
	copy(r.line[r.cur+len(runes):], r.line[r.cur:])
	copy(r.line[r.cur:], runes)
	r.cur += len(runes)
}

// Unreliable reports whether the line currently being typed used a readline
// feature the recorder cannot reconstruct (tab completion, ↑/↓ history
// recall). Such a line's text is not what the shell actually has, so Enter
// must not log it as a command.
func (r *Recorder) Unreliable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unreliable
}

// MarkUnreliable flags the line as reconstructed from a readline feature the
// recorder cannot see (tab completion, ↑/↓ history recall): the real command
// the shell will run differs from r.line, so Enter must not log it. The flag
// is cleared when the line is next submitted or cleared.
func (r *Recorder) MarkUnreliable() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		r.unreliable = true
	}
}

// InPaste reports whether the recorder is inside a DEC 2004 paste block. The
// caller uses it to route the block's content (and its embedded newlines)
// through PasteText instead of typing/submitting key by key.
func (r *Recorder) InPaste() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paste
}

// Type inserts a printable rune at the recorded cursor position and moves
// the cursor past it, mirroring how a line editor inserts mid-line. The
// first keystroke of a new line closes the previous command's output
// capture, so an event's output ends where the next command begins.
func (r *Recorder) Type(rn rune) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return
	}
	if r.curIdx >= 0 && len(r.line) == 0 {
		r.close()
	}
	if r.cur > len(r.line) {
		r.cur = len(r.line)
	}
	r.line = append(r.line, 0)
	copy(r.line[r.cur+1:], r.line[r.cur:])
	r.line[r.cur] = rn
	r.cur++
}

// Backspace deletes the rune left of the recorded cursor position, mirroring
// a line editor's backward delete.
func (r *Recorder) Backspace() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.cur <= 0 {
		return
	}
	r.line = append(r.line[:r.cur-1], r.line[r.cur:]...)
	r.cur--
}

// Control handles a control byte typed in shell mode using common readline
// (emacs) bindings: ^C and ^U abort the line, ^A/^E/^B/^F move the cursor,
// ^W removes the word left of the cursor, ^K truncates the line at the
// cursor. It reports whether the byte was one of the handled bindings; an
// unhandled byte (tab, ^D, ^L, ^R search, ...) leaves the line as-is, and
// the caller may flag the line unreliable, since readline's own binding for
// it edited the line in a way the recorder cannot see.
func (r *Recorder) Control(bs []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || len(bs) == 0 {
		return false
	}
	switch bs[0] {
	case 0x03, 0x15: // ^C interrupt, ^U kill line
		r.line = r.line[:0]
		r.cur = 0
		r.unreliable = false
		return true
	case 0x01: // ^A beginning of line
		r.cur = 0
		return true
	case 0x05: // ^E end of line
		r.cur = len(r.line)
		return true
	case 0x02: // ^B backward char
		if r.cur > 0 {
			r.cur--
		}
		return true
	case 0x06: // ^F forward char
		if r.cur < len(r.line) {
			r.cur++
		}
		return true
	case 0x17: // ^W kill word backward
		for r.cur > 0 && unicode.IsSpace(r.line[r.cur-1]) {
			r.line = append(r.line[:r.cur-1], r.line[r.cur:]...)
			r.cur--
		}
		for r.cur > 0 && !unicode.IsSpace(r.line[r.cur-1]) {
			r.line = append(r.line[:r.cur-1], r.line[r.cur:]...)
			r.cur--
		}
		return true
	case 0x0b: // ^K kill to end of line
		r.line = r.line[:r.cur]
		return true
	default:
		return false
	}
}

// Left moves the cursor one rune left (arrow-left key); no-op at column 0.
func (r *Recorder) Left() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.cur > 0 {
		r.cur--
	}
}

// Right moves the cursor one rune right (arrow-right key); no-op at the end.
func (r *Recorder) Right() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.cur < len(r.line) {
		r.cur++
	}
}

// CursorStart homes the cursor (Home key / ^A).
func (r *Recorder) CursorStart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		r.cur = 0
	}
}

// CursorEnd parks the cursor after the last rune (End key / ^E).
func (r *Recorder) CursorEnd() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.paused {
		r.cur = len(r.line)
	}
}

// SetCursor overrides the recorded cursor offset (clamped into range). It
// is used when rysh itself moves the shell's cursor — after re-injecting
// the shared input line whose text was typed wholesale.
func (r *Recorder) SetCursor(pos int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pos < 0 {
		pos = 0
	}
	if pos > len(r.line) {
		pos = len(r.line)
	}
	r.cur = pos
}

// Line returns the command line currently being typed (its in-progress
// text). It is used to capture the shell's residual input when leaving
// shell mode, so it can be saved and restored across a mode round-trip.
func (r *Recorder) Line() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.line)
}

// Cursor returns the recorded cursor offset within the line (a rune index,
// clamped to its length), so the in-progress cursor position survives mode
// switches together with Line().
func (r *Recorder) Cursor() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur > len(r.line) {
		return len(r.line)
	}
	return r.cur
}

// Enter submits the command line being typed as a shell event and opens
// output capture for it. dir is the shell's cwd at submit time. It returns
// the submitted, trimmed command ("" when blank or all-whitespace, which is
// not recorded; "" for a line flagged Unreliable, since its reconstructed
// text is not what the shell ran; also "" while paused).
func (r *Recorder) Enter(dir string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused {
		return ""
	}
	cmd := strings.TrimSpace(string(r.line))
	unreliable := r.unreliable
	r.line = r.line[:0]
	r.cur = 0
	r.unreliable = false
	r.paste = false
	if cmd == "" {
		return ""
	}
	if unreliable {
		// The line used tab completion or history recall, so r.line is not
		// the command the shell actually ran; do not log it.
		return ""
	}
	r.close()
	// The previous event's head/tail window would otherwise leak into the
	// fresh event's first Output call.
	r.resetOpenOutput()
	r.events = append(r.events, ShellEvent{Dir: dir, Command: cmd, Time: time.Now()})
	r.evict()
	r.curIdx = len(r.events) - 1
	return cmd
}

// Abort clears the line being typed and closes the open output capture. It
// is called when leaving shell mode so output produced in AI mode is not
// attributed to a shell command.
func (r *Recorder) Abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.line = r.line[:0]
	r.cur = 0
	r.unreliable = false
	r.paste = false
	r.close()
}

// Reset clears the event ring and the in-progress line, so a session switch
// starts with a clean slate for the newly restarted shell.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
	r.line = r.line[:0]
	r.cur = 0
	r.curIdx = -1
	r.unreliable = false
	r.paste = false
}

// Pause stops recording while a fullscreen program (vim, tmux attach, ...)
// owns the terminal: it closes the open output capture, clears the
// in-progress line, and makes the input/output feeders no-ops until Resume.
// The event ring is kept as-is.
func (r *Recorder) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.line = r.line[:0]
	r.cur = 0
	r.paused = true
	r.unreliable = false
	r.paste = false
	r.close()
}

// Resume restarts recording after a fullscreen program exits. No capture is
// open on return, so the program's exit redraw is not attributed to any
// command; the next Enter opens a fresh one.
func (r *Recorder) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = false
}

// Output captures a chunk of terminal output into the open event (the most
// recently submitted command). The stream is kept as a head+tail window
// (shellOutHalf each): when it overruns MaxOutput the middle is dropped and
// an elision marker is folded in, so a long stream keeps its first lines and
// its final result/errors (both carry the signal) instead of its head only.
// Output that arrives while no command is open (the initial prompt, or
// echoes while typing) or while paused (a fullscreen program owns the
// terminal) is ignored.
func (r *Recorder) Output(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.curIdx < 0 {
		return
	}
	ev := &r.events[r.curIdx]
	r.appendOut(ev, p)
}

// appendOut folds p into the open event's head+tail window. It must be
// called with r.mu held and r.curIdx >= 0.
func (r *Recorder) appendOut(ev *ShellEvent, p []byte) {
	r.seen += len(p)
	// Head: take the leading bytes until it holds shellOutHalf (once full,
	// later bytes belong to the middle/tail, not the head).
	if len(r.head) < shellOutHalf {
		n := min(shellOutHalf-len(r.head), len(p))
		r.head = append(r.head, p[:n]...)
		p = p[n:]
	}
	// Tail: append the remainder and keep only the last shellOutHalf bytes.
	r.tail = append(r.tail, p...)
	if len(r.tail) > shellOutHalf {
		r.tail = r.tail[len(r.tail)-shellOutHalf:]
	}
	ev.Output = r.renderOut()
}

// renderOut composes the head+tail window into the stored output, folding
// the elided middle into one marker that names how many bytes dropped.
func (r *Recorder) renderOut() string {
	var b strings.Builder
	if len(r.head) > 0 {
		b.Write(r.head)
	}
	if r.seen > MaxOutput {
		dropped := r.seen - len(r.head) - len(r.tail)
		if dropped > 0 {
			b.WriteString("\n[… 已省略 ")
			b.WriteString(strconv.Itoa(dropped))
			b.WriteString(" 字节 …]\n")
		}
	}
	if len(r.tail) > 0 {
		b.Write(r.tail)
	}
	return b.String()
}

// resetOpenOutput clears the head+tail window of the open event. Callers
// must hold r.mu.
func (r *Recorder) resetOpenOutput() {
	r.head = nil
	r.tail = nil
	r.seen = 0
}

// Events returns a copy of the shell events, oldest first, including the
// one whose output is still being captured. It is used by tests.
func (r *Recorder) Events() []ShellEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ShellEvent(nil), r.events...)
}

// close stops capturing output into the open event; the event stays in the
// ring. The caller must hold r.mu.
func (r *Recorder) close() {
	if r.curIdx < 0 {
		return
	}
	r.curIdx = -1
	r.evict()
}

// evict drops the oldest events beyond MaxEvents. The caller must hold r.mu.
func (r *Recorder) evict() {
	if len(r.events) > MaxEvents {
		r.events = append([]ShellEvent(nil), r.events[len(r.events)-MaxEvents:]...)
	}
}

// Fold renders the event as the one-line placeholder used for shell events
// older than the most recent one (L1 slimming, mirroring
// agent.CollapseOldTools): the command and its output size stay, the output
// body does not.
func (e ShellEvent) Fold() string {
	return fmt.Sprintf("[shell] cwd: %s · $ %s（输出 %s）",
		e.Dir, strings.Join(strings.Fields(e.Command), " "), humanSize(len(e.Output)))
}

// CollapseShellEvents splits the ring into the part that stays verbatim and
// a one-line summary of the rest (L1 slimming, the shell-event twin of
// agent.CollapseOldTools): the single most recent event — the one the user
// is most likely asking about — is returned verbatim, while every older
// event is folded to a placeholder line joined by newlines. Older commands
// stay addressable (cwd + command + output size) instead of being dropped,
// so the context is bounded yet nothing silently vanishes.
func CollapseShellEvents(events []ShellEvent) ([]ShellEvent, string) {
	if len(events) == 0 {
		return nil, ""
	}
	folded := make([]string, 0, len(events)-1)
	for i := 0; i < len(events)-1; i++ {
		folded = append(folded, events[i].Fold())
	}
	return []ShellEvent{events[len(events)-1]}, strings.Join(folded, "\n")
}

// humanSize formats a byte count as B or KB for the fold placeholder line.
func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// Format renders the event as the compact system message it is sent to the
// model as: the event's cwd, the command, and its output, terminated by a
// separator line. ANSI escape sequences and leading/trailing whitespace are
// stripped so prompts and cursor movement do not pollute the context, and
// the command and output are run through secrets.Mask so a credential the
// user typed or a key a command printed does not cross into the model.
func (e ShellEvent) Format() string {
	out := strings.TrimSpace(stripANSI(e.Output))
	out = secrets.Mask(out)
	var b strings.Builder
	if e.Dir != "" {
		b.WriteString("cwd: " + e.Dir + "\n")
	}
	b.WriteString("$ " + strings.TrimSpace(secrets.Mask(e.Command)) + "\n")
	if out != "" {
		b.WriteString(out + "\n")
	}
	b.WriteString("---\n")
	return b.String()
}

// ansiSeq matches ANSI escape sequences: CSI (ESC [ ... final byte), OSC
// (ESC ] ... BEL or ST), and other single-byte introducers.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b[@-_]`)

// stripANSI removes ANSI escape sequences from terminal output.
func stripANSI(s string) string {
	return ansiSeq.ReplaceAllString(s, "")
}
