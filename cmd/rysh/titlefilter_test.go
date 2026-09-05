package main

import (
	"strings"
	"testing"
)

// The prompt marker's stream filter must drop exactly the title-setting OSC
// 0/2 sequences and let everything else through byte for byte — OSC 7 cwd
// reports, hyperlinks, CSI and SGR included — while surviving sequences
// split across chunk boundaries.

func TestTitleFilterStripsTitleOSCs(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"OSC 0 BEL":                {"abc\x1b]0;shell title\x07def", "abcdef"},
		"OSC 2 ST":                 {"a\x1b]2;shell title\x1b\\b", "ab"},
		"two titles":               {"\x1b]2;x\x07mid\x1b]0;y\x07", "mid"},
		"title at edge":            {"\x1b]2;only\x07", ""},
		"OSC 7 passes":             {"x\x1b]7;file:///tmp\x07y", "x\x1b]7;file:///tmp\x07y"},
		"OSC 8 passes":             {"\x1b]8;;https://e\x07link\x1b]8;;\x07", "\x1b]8;;https://e\x07link\x1b]8;;\x07"},
		"SGR passes":               {"\x1b[1;31mred\x1b[0m", "\x1b[1;31mred\x1b[0m"},
		"CSI passes":               {"\x1b[?1049h", "\x1b[?1049h"},
		"lone ESC pair":            {"a\x1b\x1b[1mb", "a\x1b\x1b[1mb"},
		"ESC then ESC then ]":      {"\x1b\x1b]2;x\x07y", "\x1by"},
		"empty params not a title": {"\x1b];x\x07y", "\x1b];x\x07y"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &titleFilter{}
			if got := string(f.Filter([]byte(tc.in))); got != tc.want {
				t.Fatalf("Filter(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Split the input at every possible position across two Filter calls: the
// state machine must carry partial sequences, and the concatenation of the
// two outputs must equal the single-pass result.
func TestTitleFilterChunked(t *testing.T) {
	in := "pre\x1b]2;split title\x07mid\x1b]7;file://c/x\x07post"
	want := "premid\x1b]7;file://c/x\x07post"
	full := &titleFilter{}
	if got := string(full.Filter([]byte(in))); got != want {
		t.Fatalf("single-pass = %q, want %q", got, want)
	}
	for cut := 0; cut <= len(in); cut++ {
		f := &titleFilter{}
		got := string(f.Filter([]byte(in[:cut]))) + string(f.Filter([]byte(in[cut:])))
		if got != want {
			t.Fatalf("cut %d: %q, want %q", cut, got, want)
		}
	}
}

// A partial ST terminator (the ESC already buffered, the backslash in the
// next chunk) must not be passed through early.
func TestTitleFilterSplitST(t *testing.T) {
	f := &titleFilter{}
	if got := string(f.Filter([]byte("a\x1b]2;title\x1b"))); got != "a" {
		t.Fatalf("part 1 = %q, want %q", got, "a")
	}
	if got := string(f.Filter([]byte("\\b"))); got != "b" {
		t.Fatalf("part 2 = %q, want %q", got, "b")
	}
}

// An OSC with no terminator must be flushed after the bound instead of
// being buffered forever, and the filter must resynchronize after.
func TestTitleFilterUnterminated(t *testing.T) {
	f := &titleFilter{}
	long := make([]byte, 0, oscMaxLen+16)
	long = append(long, []byte("\x1b]2;")...)
	long = append(long, strings.Repeat("x", oscMaxLen)...)
	long = append(long, []byte("tail\x07")...)
	out := string(f.Filter(long))
	if !strings.HasPrefix(out, "\x1b]2;") {
		t.Fatalf("unterminated title OSC was swallowed: %q", out)
	}
	if !strings.HasSuffix(out, "tail\x07") {
		t.Fatalf("resync lost the tail: %q", out)
	}
	// The next, well-formed title is stripped again.
	if got := string(f.Filter([]byte("\x1b]2;next\x07"))); got != "" {
		t.Fatalf("post-resync title not stripped: %q", got)
	}
}

func TestParseTitleResponse(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"BEL terminated": {"junk\x1b]11;?;old title\x07", "old title", true},
		"ST terminated":  {"\x1b]11;?;old title\x1b\\", "old title", true},
		"not yet":        {"\x1b]11;?;par", "", false},
		"absent":         {"\x1b[2J", "", false},
		"other osc":      {"\x1b]11;rgb:00/00/00\x07", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := parseTitleResponse([]byte(tc.in))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseTitleResponse(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRyshTitle(t *testing.T) {
	if got := ryshTitle("", "s123"); got != "rysh · session s123" {
		t.Fatalf("ryshTitle(none) = %q", got)
	}
	if got := ryshTitle("ollama/llama3.1:8b", "s123"); got != "rysh · ollama/llama3.1:8b · session s123" {
		t.Fatalf("ryshTitle(model) = %q", got)
	}
}
