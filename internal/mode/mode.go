// Package mode implements the shell/AI mode state machine for rysh.
//
// Shell mode forwards every key to the child shell unchanged, except for the
// mode-switch gestures (the mode-switch key — Shift+Tab by default, or a
// space typed at a fresh prompt), which enter AI mode. AI mode itself is not handled here: the session driver
// routes AI-mode keys into the persistent AI input editor (internal/aiui)
// on the same shared screen, and calls ToShell when that editor returns to
// shell mode. Mode switching never changes the screen itself — only which
// keys are forwarded to the shell and which go to the AI input line.
package mode

import (
	"sync"

	"ruyishell/internal/keys"
)

// Mode is the active input routing mode.
type Mode uint8

const (
	// Shell routes every key to the child shell.
	Shell Mode = iota
	// AI means the AI input line owns the keyboard.
	AI
)

// Result describes what the caller should do after handling one event.
type Result struct {
	// Forward carries bytes to send to the child shell (nil when none).
	Forward []byte
	// ModeChanged reports that the gesture entered AI mode; the caller draws
	// the AI input line. Leaving AI mode is driven by the AI editor itself,
	// not by Handle, so this never reports an AI→Shell transition.
	ModeChanged bool
}

// State tracks the current mode. It is safe for concurrent use.
type State struct {
	mu   sync.Mutex
	mode Mode
}

// New returns a State starting in shell mode.
func New() *State {
	return &State{mode: Shell}
}

// Mode returns the current mode.
func (s *State) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// ToShell returns the state to shell mode. It is called when the AI view
// quits (the mode-switch key or a leading space on an empty draft) and when
// the session ends.
func (s *State) ToShell() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = Shell
}

// ToAI puts the state into AI mode. It is used to start rysh directly in AI
// mode (`rysh ai` without a message).
func (s *State) ToAI() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = AI
}

// Handle routes one key event through the state machine.
//
// fwdSinceNewline is true when the user has forwarded bytes to the shell
// since the shell's last newline output, i.e. the cursor is not at a fresh
// line start. In AI mode the driver routes keys to the AI view instead of
// calling Handle, so an AI-mode event is a no-op guard.
func (s *State) Handle(ev keys.Event, fwdSinceNewline bool) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != Shell {
		return Result{}
	}
	switch ev.Kind {
	case keys.CtrlTab:
		// Primary mode switch: always enters AI mode.
		s.mode = AI
		return Result{ModeChanged: true}
	case keys.Rune:
		if ev.R == ' ' && !fwdSinceNewline {
			// A space typed at a fresh prompt is a mode-switch gesture;
			// swallow it so the shell never sees it.
			s.mode = AI
			return Result{ModeChanged: true}
		}
	}
	// Everything else is forwarded to the shell unchanged.
	return Result{Forward: ev.Raw}
}
