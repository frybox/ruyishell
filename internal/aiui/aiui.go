// Package aiui is the pure-state editor for the AI-mode input line in the
// unified-stream model. It no longer owns a screen or renders a
// conversation view: the session driver owns the single terminal stream and
// draws everything (AI prompts, streamed replies, inline spinner, notices).
// aiui only tracks the in-progress draft and how the input line was last
// laid out, so it can redraw that one line in place (relative cursor
// movement, no absolute coordinates, no DSR queries) without disturbing
// anything else on the screen. It also implements the streaming lock: while
// a task runs, input is locked and ^C cancels.
package aiui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"ruyishell/internal/keys"
	"ruyishell/internal/screen"
)

// Action is what the driver must do after handling one AI-mode key event.
type Action uint8

const (
	// ActionNone means the event was consumed without changing mode; the
	// driver may need to redraw the input line.
	ActionNone Action = iota
	// ActionSubmit means Enter was pressed with a non-empty draft; the
	// driver runs the submitted text.
	ActionSubmit
	// ActionCancel means ^C was pressed while a task is streaming; the
	// driver aborts the in-flight request.
	ActionCancel
	// ActionLeave means the user wants to return to shell mode.
	ActionLeave
	// ActionApproveYes/No/Always are the §7.3 approval answers: they are
	// only produced while a task streams AND an approval prompt is
	// pending (SetApproval). a additionally records the session's
	// "always allow" rule.
	ActionApproveYes
	ActionApproveNo
	ActionApproveAlways
)

// draftSnapshot is one undoable state of the draft: the text and the cursor
// index, captured before each mutating edit.
type draftSnapshot struct {
	draft []rune
	pos   int
}

// maxUndo bounds the draft undo history so an editing session cannot grow
// memory without limit.
const maxUndo = 100

// Model is the pure-state AI input editor.
type Model struct {
	cols int // terminal width in cells, used as the default for rendering

	draft []rune
	pos   int // cursor index into draft
	undo  []draftSnapshot

	prompt        string // expanded PS-style prompt
	promptWidth   int    // visible width of prompt in cells
	prevRows      int    // rows the last render occupied
	prevCursorRow int    // cursor row within the block after the last render

	// lastDraft is the draft text as drawn on screen by the last render.
	// While the committed draft equals it, the rendered line is still
	// valid and a render needs to move only the cursor (no erase/rewrite),
	// which keeps an IME's preedit/candidate window attached to the cursor.
	lastDraft string
	// dirty reports whether the committed draft or the cursor position
	// changed since the last render. The driver redraws on ActionNone only
	// when dirty, so keys that change nothing (notably keys an IME forwards
	// while composing) do not disturb the line on screen.
	dirty bool

	streaming bool

	// pendingApproval marks an open §7 approval prompt: the streaming
	// lock routes y/n/a (and Y/N/A) to the driver as approval answers
	// instead of ignoring them. Toggled by SetApproval.
	pendingApproval bool

	// completer supplies the Tab completion candidates: the full candidate
	// strings for the word under the cursor (already filtered to prefix
	// it), or nil/empty for no completion. Called once per completion
	// attempt (the first Tab; later Tabs cycle the stored list). The
	// driver owns the policy (which drafts complete, one-time notices).
	completer func(draft string, pos int) []string

	// compActive/candIdx are the Tab cycle state. The first Tab since the
	// last edit inserts the longest common prefix, or the first candidate
	// when that extends nothing; later Tabs take the next candidate.
	// candIdx is -1 right after a common-prefix insert. Any draft edit
	// resets to first-Tab behavior.
	compActive bool
	cands      []string
	candIdx    int

	// pasting captures a DEC 2004 bracketed-paste block: between
	// PasteStart and PasteEnd every rune is appended to pasteBuf instead
	// of editing the draft, so an embedded newline inserts as a literal
	// newline rather than submitting a partial draft.
	pasting  bool
	pasteBuf string

	// history is the session's user inputs, oldest first: the recall
	// source for ↑/Ctrl-P (previous) and ↓/Ctrl-N (next). The driver
	// replaces it on session switches (SetHistory) and appends every
	// submitted input (Remember).
	history []string
	// histIdx is the selected history index; -1 is the live, unrecalled
	// draft. savedDraft/savedPos hold the live draft while a history
	// entry is selected, so ↓ past the newest entry restores it.
	histIdx    int
	savedDraft []rune
	savedPos   int
}

// New returns an empty AI input editor. cols defaults to 80 until SetSize
// arrives.
func New() *Model {
	return &Model{cols: 80}
}

// SetSize records the terminal size. Only the width matters for the input
// line; the height is accepted for symmetry and unused.
func (m *Model) SetSize(w, h int) {
	if w > 0 {
		m.cols = w
	}
}

// Streaming reports whether a task is currently streaming (input locked).
func (m *Model) Streaming() bool { return m.streaming }

// SetApproval opens (true) or closes (false) the §7.3 approval window:
// while a task streams and an approval is pending, y/n/a answer the
// prompt instead of being ignored. Everything else stays locked.
func (m *Model) SetApproval(pending bool) { m.pendingApproval = pending }

// SetCompleter installs the Tab completion source (nil = no completion).
// The Model calls it on the first Tab of an attempt with the current draft
// and cursor position; an empty result means no completion for this draft.
func (m *Model) SetCompleter(f func(draft string, pos int) []string) {
	m.completer = f
}

// Dirty reports whether the committed draft or the cursor position changed
// since the last render. The driver calls RenderInput on ActionNone keys
// only when this is true, so keys the IME forwards during composition that
// change nothing do not erase/rewrite the input line and detach the IME's
// candidate window.
func (m *Model) Dirty() bool { return m.dirty }

// Draft returns the current draft text.
func (m *Model) Draft() string { return string(m.draft) }

// Pos returns the cursor index within the draft.
func (m *Model) Pos() int { return m.pos }

// BeginInput starts a fresh input-line block: the render state is reset so
// the next RenderInput treats the cursor's current position as the block
// top. The draft is kept (it survives mode switches).
func (m *Model) BeginInput(prompt string, promptWidth int) {
	m.prompt = prompt
	m.promptWidth = promptWidth
	m.prevRows = 0
	m.prevCursorRow = 0
	m.lastDraft = ""
	m.dirty = true
	m.resetComplete()
}

// ResetInputState clears the render state while keeping the draft, so a
// redraw after a mode switch does not erase rows belonging to the old
// layout.
func (m *Model) ResetInputState() {
	m.prevRows = 0
	m.prevCursorRow = 0
	m.lastDraft = ""
	m.dirty = true
}

// Clear empties the draft (and its undo history).
func (m *Model) Clear() {
	m.draft = nil
	m.pos = 0
	m.undo = nil
	m.lastDraft = ""
	m.dirty = true
	m.resetRecall()
}

// SetDraft replaces the draft with text and puts the cursor at pos
// (clamped into range), clearing the undo history: the text was composed
// in the shell (the shared input line), so it has no editable history in
// this editor. The next RenderInput does a full redraw.
func (m *Model) SetDraft(text string, pos int) {
	m.draft = []rune(text)
	if pos < 0 {
		pos = 0
	}
	if pos > len(m.draft) {
		pos = len(m.draft)
	}
	m.pos = pos
	m.undo = nil
	m.lastDraft = ""
	m.dirty = true
	m.resetComplete()
	m.resetRecall()
}

// SetHistory replaces the recall list — a session switch or a re-read of
// the session's log — and drops any in-flight recall: the live draft is
// not a recalled entry.
func (m *Model) SetHistory(items []string) {
	m.history = items
	m.resetRecall()
}

// Remember appends one submitted input to the recall list; the submitted
// draft is consumed, so recall state resets with it.
func (m *Model) Remember(text string) {
	m.history = append(m.history, text)
	m.resetRecall()
}

// HandleKey routes one AI-mode key event and returns what the driver must
// do. While streaming, only ^C (cancel) is honored — plus y/n/a when an
// approval prompt is pending (§7.3); the mode-switch key is
// locked and every other key is ignored so the draft cannot be disturbed.
// Otherwise Enter submits a non-empty draft, the mode-switch key (Shift+Tab
// by default, or a leading space on an empty draft) leaves AI mode, Esc
// with a draft clears it (undoable), ↑/Ctrl-P and ↓/Ctrl-N walk the
// recall list of submitted inputs, and the editing keys edit the draft
// (any edit detaches from recall). PageUp/PageDown/Ctrl-End are ignored so
// the terminal's native scrollback is left alone. An ActionNone key marks
// the editor dirty only when it actually changed the committed draft or the
// cursor position, so keys the IME forwards during composition that change
// nothing do not force a redraw.
func (m *Model) HandleKey(ev keys.Event) Action {
	if m.streaming {
		if ev.Kind == keys.Other && len(ev.Raw) == 1 && ev.Raw[0] == 0x03 {
			return ActionCancel
		}
		// §7.3: an open approval prompt lets exactly the three answer
		// keys through the streaming lock (case-insensitive; the draft
		// is never touched).
		if m.pendingApproval && ev.Kind == keys.Rune {
			switch ev.R {
			case 'y', 'Y':
				return ActionApproveYes
			case 'n', 'N':
				return ActionApproveNo
			case 'a', 'A':
				return ActionApproveAlways
			}
		}
		return ActionNone
	}
	beforeDraft := string(m.draft)
	beforePos := m.pos
	act := ActionNone
	switch ev.Kind {
	case keys.PasteStart:
		// DEC 2004: begin capturing a paste block; nothing between the
		// markers edits the draft.
		m.pasting = true
		m.pasteBuf = ""
	case keys.PasteEnd:
		m.pasting = false
		if m.pasteBuf != "" {
			m.insertPaste(m.pasteBuf)
		}
		m.pasteBuf = ""
	case keys.Enter:
		if m.pasting {
			// An embedded newline inside a paste block is literal text,
			// not a submit.
			m.pasteBuf += "\n"
			break
		}
		if strings.TrimSpace(string(m.draft)) != "" {
			act = ActionSubmit
		}
	case keys.CtrlTab:
		act = ActionLeave
	case keys.Esc:
		// Esc only clears a draft (undoable); it no longer leaves AI mode
		// — returning to the shell is a leading space on an empty draft.
		if len(m.draft) > 0 {
			m.pushUndo()
			m.draft = nil
			m.pos = 0
			m.resetComplete()
			m.resetRecall()
		}
	case keys.Rune:
		if m.pasting {
			// Capture the whole block; an embedded newline is stored as a
			// literal \n and never submits the draft.
			m.pasteBuf += string(ev.R)
			break
		}
		if ev.R == ' ' && len(m.draft) == 0 {
			// A leading space at an empty draft returns to shell mode,
			// mirroring the shell-mode switch; the shell never sees it.
			act = ActionLeave
		} else {
			m.insert(ev.R)
		}
	case keys.Backspace:
		m.backspace()
	case keys.Tab:
		m.tabComplete()
	case keys.ArrowLeft:
		if m.pos > 0 {
			m.pos--
		}
	case keys.ArrowRight:
		if m.pos < len(m.draft) {
			m.pos++
		}
	case keys.Home:
		m.pos = 0
	case keys.End:
		m.pos = len(m.draft)
	case keys.ArrowUp: // ↑ / Ctrl-P: previous user input
		m.prevInput()
	case keys.ArrowDown: // ↓ / Ctrl-N: next user input
		m.nextInput()
	case keys.PageUp, keys.PageDown, keys.CtrlEnd:
		// native scrollback
	case keys.Other:
		// Readline-style control bytes that reach the draft editor.
		if len(ev.Raw) == 1 {
			switch ev.Raw[0] {
			case 0x01: // ^A
				m.pos = 0
			case 0x05: // ^E
				m.pos = len(m.draft)
			case 0x1a: // ^Z undo
				m.undoEdit()
			case 0x15: // ^U clear
				m.pushUndo()
				m.draft = nil
				m.pos = 0
				m.resetComplete()
				m.resetRecall()
			case 0x10: // ^P previous user input
				m.prevInput()
			case 0x0e: // ^N next user input
				m.nextInput()
			}
		}
	}
	if act == ActionNone {
		m.dirty = m.dirty || beforeDraft != string(m.draft) || beforePos != m.pos
	}
	return act
}

// RenderInput redraws the input line (prompt + draft) in place, entirely
// with relative cursor movement, and positions the cursor at the typing
// position. write must be called with the driver's writeMu held; it receives
// the exact bytes to emit. cols is the terminal width in cells (0 uses the
// stored size).
func (m *Model) RenderInput(write func(string), cols int) {
	m.renderInput(write, cols)
}

// Submit finalizes the draft for submission: it redraws the line with the
// cursor at the end (so the full text is visible), returns the trimmed
// draft, and clears the draft and its undo history. write must be called
// with the driver's writeMu held.
func (m *Model) Submit(write func(string), cols int) string {
	m.pos = len(m.draft)
	m.renderInput(write, cols)
	text := strings.TrimSpace(string(m.draft))
	m.draft = nil
	m.pos = 0
	m.undo = nil
	m.resetComplete()
	m.resetRecall()
	m.prevRows = 0
	m.prevCursorRow = 0
	m.lastDraft = ""
	m.dirty = false
	return text
}

// StartStream locks the editor for a running task.
func (m *Model) StartStream() { m.streaming = true }

// EndStream unlocks the editor after a task finishes or is cancelled.
func (m *Model) EndStream() { m.streaming = false }

// EraseInput removes the input line (prompt + draft) from the screen and
// leaves the cursor at the block top, so following content (e.g. deferred
// shell output flushed on return to shell mode) continues the stream there.
// It mirrors the erase steps of renderInput and resets the render state.
// write must be called with the driver's writeMu held.
func (m *Model) EraseInput(write func(string)) {
	if m.prevRows == 0 {
		m.lastDraft = ""
		m.dirty = false
		return
	}
	var b strings.Builder
	// 1. Move up to the block top (relative; clamps at row 1).
	b.WriteString(screen.CursorUp(m.prevCursorRow))
	b.WriteString("\r")
	// 2. Erase the rows the input line occupied. The downward steps stop at
	// the block's last row, so the erase never writes past the block: a
	// line-down on the final screen row would scroll the main screen and
	// drag already-committed lines up into history.
	for i := 0; i < m.prevRows; i++ {
		b.WriteString("\x1b[2K")
		if i < m.prevRows-1 {
			b.WriteString("\x1b[1B")
		}
	}
	// 3. Return to the block top so following writes continue there.
	b.WriteString(screen.CursorUp(m.prevRows - 1))
	b.WriteString("\r")
	m.prevRows = 0
	m.prevCursorRow = 0
	m.lastDraft = ""
	m.dirty = false
	write(b.String())
}

// ansiSeq matches ANSI escape sequences: CSI (ESC [ ... final byte), OSC
// (ESC ] ... BEL or ST), and other single-byte introducers.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b[@-_]`)

// Sanitize strips ANSI escape sequences and stray control bytes (except \n
// and \t) from s. It is used to clean payloads destined for the session log
// (submitted prompts, notices) so escape sequences never reach the file.
func Sanitize(s string) string {
	s = ansiSeq.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderInput is the relative redraw. It tracks only where the cursor is
// within the block (prevCursorRow) and how many rows the block occupies
// (prevRows), never absolute coordinates. Between two renders the only
// writes on the screen are input-line redraws, so the block top only moves
// up and erasing prevRows rows from it is always correct; streaming
// happens only after BeginInput resets the state. When the committed draft
// is unchanged since the last render, the line on screen is still valid
// and only the cursor is moved: skipping the erase/rewrite keeps an IME's
// preedit/candidate window attached to the cursor while it moves.
func (m *Model) renderInput(write func(string), cols int) {
	if cols < 1 {
		cols = m.cols
	}
	if cols < 1 {
		cols = 80
	}
	// Compute the visual layout the same way the terminal renders it:
	// rune by rune, wrapping when a cell no longer fits. A plain
	// total/cols modulo would be wrong for wide (CJK) characters crossing
	// a row boundary — a 2-cell rune that does not fit at a row's end
	// wastes one cell there, drifting the linear model one column per
	// wrapped row — and for the deferred-wrap case where the text exactly
	// fills whole rows and the cursor lags on the previous row's last
	// column.
	rows, targetRow, targetCol, postRow := m.draftLayout(cols)
	if targetCol == cols {
		// A draft (or cursor position) ending exactly at a row boundary
		// would sit in the terminal's pending-wrap cell, which is
		// unreachable via CUF (it clamps at the last column). The next
		// cell below is the same write position, so anchor there instead;
		// keeping prevCursorRow equal to the real row stays consistent.
		targetRow++
		targetCol = 0
	}

	var b strings.Builder
	if m.prevRows > 0 && string(m.draft) == m.lastDraft {
		// Cursor-only move: the rendered line is unchanged, so move the
		// cursor from its previous position in the block to the new typing
		// position without erasing or rewriting anything.
		if targetRow > m.prevCursorRow {
			b.WriteString(screen.CursorDown(targetRow - m.prevCursorRow))
		} else {
			b.WriteString(screen.CursorUp(m.prevCursorRow - targetRow))
		}
		b.WriteString("\r")
		b.WriteString(screen.CursorForward(targetCol))
	} else {
		// 1. Move up to the block top (relative; clamps at row 1).
		b.WriteString(screen.CursorUp(m.prevCursorRow))
		b.WriteString("\r")
		// 2. Erase the rows the previous render occupied, never moving past
		// the block's last row: a line-down on the final screen row would
		// scroll the main screen and drag already-committed lines up into
		// history.
		for i := 0; i < m.prevRows; i++ {
			b.WriteString("\x1b[2K")
			if i < m.prevRows-1 {
				b.WriteString("\x1b[1B")
			}
		}
		b.WriteString(screen.CursorUp(m.prevRows - 1))
		b.WriteString("\r")
		// 3. Write the prompt and the full draft.
		b.WriteString(m.prompt)
		b.WriteString(string(m.draft))
		// 4. Move the cursor from the end of the written text to the typing
		// position. Start with CR (to column zero of the written text's
		// row, which is unambiguous even under deferred wrap) and only
		// then move rows, so a full last row lands the cursor on the row
		// below it instead of one row too high.
		b.WriteString("\r")
		if postRow > targetRow {
			b.WriteString(screen.CursorUp(postRow - targetRow))
		} else if postRow < targetRow {
			b.WriteString(screen.CursorDown(targetRow - postRow))
		}
		b.WriteString(screen.CursorForward(targetCol))
		// 5. Remember the new layout for the next render.
		m.prevRows = rows
		if m.prevRows < 1 {
			m.prevRows = 1
		}
	}
	m.prevCursorRow = targetRow
	m.lastDraft = string(m.draft)
	m.dirty = false

	write(b.String())
}

// draftLayout computes the visual layout of prompt+draft when the draft
// holds embedded newlines: the prompt starts visual row 0 at column
// promptWidth, each '\n' starts a new visual line at column zero, and lines
// wrap at cols cells exactly as the terminal renders them. It returns the
// total visual rows, the cursor row/col for position pos, and the visual row
// the terminal's cursor lands on after the text is written (the final visual
// line, since the written '\n' moves the cursor to its start). For drafts
// without '\n' the result matches the single-line wrap math of renderInput.
func (m *Model) draftLayout(cols int) (rows, cursorRow, cursorCol, postRow int) {
	row, col := 0, m.promptWidth
	runes := m.draft
	rowOf := make([]int, len(runes))
	colOf := make([]int, len(runes))
	for i, r := range runes {
		if r == '\n' {
			rowOf[i], colOf[i] = row, col
			row++
			col = 0
			continue
		}
		w := ansi.StringWidth(string(r))
		if col+w > cols {
			row++
			col = 0
		}
		rowOf[i], colOf[i] = row, col
		col += w
	}
	rows = row + 1
	postRow = row
	switch {
	case m.pos == 0:
		cursorRow, cursorCol = 0, m.promptWidth
	case runes[m.pos-1] == '\n':
		// A newline's cursor lands at the start of the next visual line.
		cursorRow, cursorCol = rowOf[m.pos-1]+1, 0
	default:
		cursorRow = rowOf[m.pos-1]
		cursorCol = colOf[m.pos-1] + ansi.StringWidth(string(runes[m.pos-1]))
	}
	return
}

// pushUndo snapshots the current draft and cursor onto the undo stack
// (bounded) before a mutating edit.
func (m *Model) pushUndo() {
	if len(m.undo) >= maxUndo {
		m.undo = append(m.undo[:0], m.undo[1:]...)
	}
	m.undo = append(m.undo, draftSnapshot{
		draft: append([]rune(nil), m.draft...),
		pos:   m.pos,
	})
}

// undoEdit restores the draft to its state before the last mutating edit
// (Ctrl+Z). It copies the snapshot so later edits cannot alias its storage.
func (m *Model) undoEdit() {
	if len(m.undo) == 0 {
		return
	}
	m.resetComplete()
	m.resetRecall()
	i := len(m.undo) - 1
	m.draft = append([]rune(nil), m.undo[i].draft...)
	m.pos = m.undo[i].pos
	m.undo = m.undo[:i]
}

func (m *Model) insert(r rune) {
	m.resetComplete()
	m.resetRecall()
	m.pushUndo()
	m.draft = append(m.draft, 0)
	copy(m.draft[m.pos+1:], m.draft[m.pos:])
	m.draft[m.pos] = r
	m.pos++
}

// insertPaste inserts a bracketed-paste block (DEC 2004) as a single
// undoable edit. Embedded newlines are normalized to \n and stored in the
// draft as literal line breaks; they never reach the Enter handler, so a
// multi-line paste cannot submit a partial draft.
func (m *Model) insertPaste(s string) {
	m.resetComplete()
	m.resetRecall()
	m.pushUndo()
	var text strings.Builder
	for _, r := range s {
		if r == '\r' {
			text.WriteByte('\n')
		} else {
			text.WriteRune(r)
		}
	}
	runes := []rune(text.String())
	m.draft = append(m.draft, make([]rune, len(runes))...)
	copy(m.draft[m.pos+len(runes):], m.draft[m.pos:])
	copy(m.draft[m.pos:], runes)
	m.pos += len(runes)
}

func (m *Model) backspace() {
	if m.pos == 0 {
		return
	}
	m.resetComplete()
	m.resetRecall()
	m.pushUndo()
	m.draft = append(m.draft[:m.pos-1], m.draft[m.pos:]...)
	m.pos--
}

// tabComplete is the Tab handler. The first Tab since the last edit asks
// the completer for the candidates of the word under the cursor and
// inserts their longest common prefix, or the first candidate when that
// extends nothing; later Tabs cycle through that same stored list (the
// draft already carries a candidate, so re-querying would filter the
// others out). The candidate replaces the word [0,pos) — v1 completes the
// first word only, which is the only region the completer serves.
func (m *Model) tabComplete() {
	if m.completer == nil {
		return
	}
	if m.compActive {
		cands := m.cands
		word := string(m.draft[:m.pos])
		i := m.candIdx
		for k := 0; k <= len(cands); k++ {
			i = (i + 1) % len(cands)
			if cands[i] != word {
				break
			}
		}
		m.candIdx = i
		m.setWord(cands[i])
		return
	}
	cands := m.completer(string(m.draft), m.pos)
	if len(cands) == 0 {
		return
	}
	m.cands = cands
	m.compActive = true
	word := string(m.draft[:m.pos])
	if lcp := longestCommonPrefix(cands); lcp != word && strings.HasPrefix(lcp, word) {
		m.candIdx = -1
		m.setWord(lcp)
	} else {
		m.candIdx = 0
		m.setWord(cands[0])
	}
}

// setWord replaces the completed word [0,pos) with the candidate, keeping
// the text after the cursor. Undoable like any edit.
func (m *Model) setWord(cand string) {
	m.resetRecall()
	m.pushUndo()
	m.draft = append([]rune(cand), m.draft[m.pos:]...)
	m.pos = len([]rune(cand))
}

// resetComplete clears the Tab cycle state so the next Tab is a fresh
// first-Tab attempt; every draft edit calls it.
func (m *Model) resetComplete() {
	m.compActive = false
	m.cands = nil
	m.candIdx = 0
}

// resetRecall drops any in-flight recall: the live draft is back in play
// (histIdx -1) and the saved-draft slot is released. Called by every
// draft-mutating point, so editing a recalled entry detaches from the
// recall list, and by SetHistory/Remember/SetDraft.
func (m *Model) resetRecall() {
	m.histIdx = -1
	m.savedDraft = nil
	m.savedPos = 0
}

// prevInput recalls the previous user input (↑ / Ctrl-P). The first
// recall saves the live draft so ↓ past the newest entry restores it;
// at the oldest entry it stays put.
func (m *Model) prevInput() {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx == -1 {
		m.savedDraft = append([]rune(nil), m.draft...)
		m.savedPos = m.pos
		m.histIdx = len(m.history) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	} else {
		return
	}
	m.resetComplete()
	m.draft = []rune(m.history[m.histIdx])
	m.pos = len(m.draft)
}

// nextInput recalls the next user input (↓ / Ctrl-N); past the newest
// entry it restores the live draft saved at the first recall.
func (m *Model) nextInput() {
	if m.histIdx == -1 {
		return
	}
	if m.histIdx < len(m.history)-1 {
		m.histIdx++
		m.resetComplete()
		m.draft = []rune(m.history[m.histIdx])
		m.pos = len(m.draft)
		return
	}
	m.histIdx = -1
	m.draft = m.savedDraft
	m.pos = m.savedPos
	m.savedDraft = nil
}

// longestCommonPrefix returns the longest prefix shared by all candidates
// (the empty string for none or zero candidates).
func longestCommonPrefix(cands []string) string {
	if len(cands) == 0 {
		return ""
	}
	p := cands[0]
	for _, c := range cands[1:] {
		for len(p) > 0 && !strings.HasPrefix(c, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}
