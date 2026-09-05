package mode

import (
	"testing"

	"ruyishell/internal/keys"
)

func runeEv(r rune) keys.Event {
	return keys.Event{Kind: keys.Rune, R: r, Raw: []byte(string(r))}
}

func ctrlTab() keys.Event {
	return keys.Event{Kind: keys.CtrlTab, Raw: []byte("\x1b[27;5;9~")}
}

func TestShellForwardsEverything(t *testing.T) {
	cases := []struct {
		name string
		ev   keys.Event
	}{
		{"letter", runeEv('e')},
		{"enter", keys.Event{Kind: keys.Enter, Raw: []byte("\r")}},
		{"backspace", keys.Event{Kind: keys.Backspace, Raw: []byte("\x7f")}},
		{"ctrl-c", keys.Event{Kind: keys.Other, Raw: []byte("\x03")}},
		{"arrow", keys.Event{Kind: keys.ArrowRight, Raw: []byte("\x1b[C")}},
		{"esc", keys.Event{Kind: keys.Esc, Raw: []byte("\x1b")}},
		{"page-up", keys.Event{Kind: keys.PageUp, Raw: []byte("\x1b[5~")}},
		{"ctrl-end", keys.Event{Kind: keys.CtrlEnd, Raw: []byte("\x1b[1;5F")}},
		{"mid-line space", runeEv(' ')}, // forwarded when not at a fresh prompt
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			res := s.Handle(tc.ev, true) // fwdSinceNewline=true: mid-line
			if res.ModeChanged {
				t.Fatalf("expected no mode change, got %+v", res)
			}
			if string(res.Forward) != string(tc.ev.Raw) {
				t.Fatalf("forward = %q, want %q", res.Forward, tc.ev.Raw)
			}
			if s.Mode() != Shell {
				t.Fatalf("still expected shell mode, got %v", s.Mode())
			}
		})
	}
}

func TestShellSpaceAtFreshPromptEntersAI(t *testing.T) {
	s := New()
	// fwdSinceNewline=false means the cursor is at a fresh line start.
	res := s.Handle(runeEv(' '), false)
	if !res.ModeChanged {
		t.Fatalf("expected mode change, got %+v", res)
	}
	if res.Forward != nil {
		t.Fatalf("space should be swallowed, got forward %q", res.Forward)
	}
	if s.Mode() != AI {
		t.Fatalf("expected AI mode, got %v", s.Mode())
	}
}

func TestShellCtrlTabEntersAI(t *testing.T) {
	s := New()
	res := s.Handle(ctrlTab(), true)
	if !res.ModeChanged {
		t.Fatalf("expected mode change, got %+v", res)
	}
	if s.Mode() != AI {
		t.Fatalf("expected AI mode, got %v", s.Mode())
	}
}

func TestToShell(t *testing.T) {
	s := New()
	s.Handle(ctrlTab(), true)
	if s.Mode() != AI {
		t.Fatalf("expected AI mode, got %v", s.Mode())
	}
	// The driver calls ToShell when the AI view quits.
	s.ToShell()
	if s.Mode() != Shell {
		t.Fatalf("expected shell mode after ToShell, got %v", s.Mode())
	}
}

func TestAIEventsAreNoop(t *testing.T) {
	// While in AI mode the driver routes keys to the AI view and does not
	// call Handle; if one slips through it must not affect state.
	s := New()
	s.Handle(ctrlTab(), true)
	res := s.Handle(runeEv('x'), false)
	if res.ModeChanged || res.Forward != nil {
		t.Fatalf("AI-mode handle should be a no-op, got %+v", res)
	}
	if s.Mode() != AI {
		t.Fatalf("expected AI mode unchanged, got %v", s.Mode())
	}
}

func TestModeInitial(t *testing.T) {
	s := New()
	if s.Mode() != Shell {
		t.Fatalf("expected shell mode initially, got %v", s.Mode())
	}
}
