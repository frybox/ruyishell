package theme

import "testing"

// TestDetectLevel pins down the capability ladder, including the Windows
// terminals that advertise nothing: a GUI-launched rysh has neither TERM nor
// COLORTERM, and if that reads as LNone every color is dropped while the
// literally-written style codes (the reasoning block's italic) survive -- so
// the text comes out italic but never muted, and the task-stats footer never
// dimmed.
func TestDetectLevel(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want Level
	}{
		{"COLORTERM truecolor", map[string]string{"COLORTERM": "truecolor"}, LTrueColor},
		{"TERM truecolor beats WT", map[string]string{"TERM": "xterm-truecolor", "WT_SESSION": "x"}, LTrueColor},
		// An advertised TERM wins over the platform markers: this is what the
		// integration suite relies on when it pins TERM to quantize colors.
		{"TERM 256 beats WT_SESSION", map[string]string{"TERM": "xterm-256color", "WT_SESSION": "x"}, LAnsi256},
		{"TERM dumb", map[string]string{"TERM": "dumb", "WT_SESSION": "x"}, LNone},
		{"Windows Terminal, no TERM", map[string]string{"WT_SESSION": "abc"}, LTrueColor},
		{"mintty, no TERM", map[string]string{"MSYSCON": "mintty.exe"}, LAnsi256},
		{"ConEmu, no TERM", map[string]string{"ConEmuANSI": "on"}, LAnsi256},
		// A legacy conhost advertises nothing and sets no marker: staying LNone
		// is correct, since without VT processing the escapes would print raw.
		{"nothing at all", map[string]string{}, LNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Blank every input first: detectLevel only reads these, and an
			// empty value is "absent" for each of its checks.
			for _, k := range []string{"COLORTERM", "TERM", "WT_SESSION", "MSYSCON", "ConEmuANSI"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := detectLevel(); got != tc.want {
				t.Fatalf("detectLevel() with %v = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}
