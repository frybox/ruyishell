package main

import "testing"

// foregroundBlocksSwitch drives the mode-switch gate. Cover the four cases
// that matter: not a switch is never blocked; a genuine fresh prompt always
// allows the switch (even on Windows with a stale alt-screen reading); a
// foreground program blocks on non-fresh lines; and a fresh line with no
// foreground program is allowed.
func TestForegroundBlocksSwitch(t *testing.T) {
	cases := []struct {
		name            string
		isSwitch        bool
		fg              bool
		fwdSinceNewline bool
		windows         bool
		want            bool
	}{
		{"not a switch gesture", false, false, false, true, false},
		{"not a switch, unix", false, false, false, false, false},

		// Fresh prompt on Windows: always allowed, even if fg is false
		// (stale alternate-screen reading must not strand the user).
		{"fresh prompt, win, no fg", true, false, false, true, false},
		{"fresh prompt, win, fg ok", true, true, false, true, false},

		// Non-fresh line on Windows: only blocked while a foreground
		// program owns the terminal (alt screen).
		{"typed line, win, no fg", true, false, true, true, true},
		{"typed line, win, fg ok", true, true, true, true, false},

		// Unix: gated purely by fg (foreground process group).
		{"fresh prompt, unix, no fg", true, false, false, false, true},
		{"fresh prompt, unix, fg ok", true, true, false, false, false},
		{"typed line, unix, no fg", true, false, true, false, true},
		{"typed line, unix, fg ok", true, true, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := foregroundBlocksSwitch(c.isSwitch, c.fg, c.fwdSinceNewline, c.windows); got != c.want {
				t.Fatalf("foregroundBlocksSwitch(%v,%v,%v,%v) = %v, want %v",
					c.isSwitch, c.fg, c.fwdSinceNewline, c.windows, got, c.want)
			}
		})
	}
}
