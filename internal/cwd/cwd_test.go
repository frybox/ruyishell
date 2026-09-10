package cwd

import (
	"strings"
	"testing"
)

func TestTrackerParsesOSC7(t *testing.T) {
	tr := &Tracker{}
	// Standard ESC \ terminated report.
	tr.Feed([]byte("\x1b]7;file://myhost/home/user/projects\x1b\\"))
	if got := tr.CWD(); got != "/home/user/projects" {
		t.Fatalf("CWD() = %q, want %q", got, "/home/user/projects")
	}
}

func TestTrackerBELTerminated(t *testing.T) {
	tr := &Tracker{}
	// Some shells terminate OSC sequences with BEL instead of ESC \.
	tr.Feed([]byte("\x1b]7;file://host/tmp/some\x07"))
	if got := tr.CWD(); got != "/tmp/some" {
		t.Fatalf("CWD() = %q, want %q", got, "/tmp/some")
	}
}

func TestTrackerPrefersLatest(t *testing.T) {
	tr := &Tracker{}
	tr.Feed([]byte("\x1b]7;file://h/a\x1b\\tail \x1b]7;file://h/b\x1b\\"))
	if got := tr.CWD(); got != "/b" {
		t.Fatalf("CWD() = %q, want %q (latest report wins)", got, "/b")
	}
}

func TestTrackerAcrossChunkBoundaries(t *testing.T) {
	tr := &Tracker{}
	// A report split across three Feed calls must still be parsed.
	parts := []string{
		"prompt \x1b]7;file://my",
		"host/ho",
		"me/user/ws\x1b\\$ ",
	}
	for _, p := range parts {
		tr.Feed([]byte(p))
	}
	if got := tr.CWD(); got != "/home/user/ws" {
		t.Fatalf("CWD() = %q, want %q (split report)", got, "/home/user/ws")
	}
}

func TestTrackerNoOSC7(t *testing.T) {
	tr := &Tracker{}
	tr.Feed([]byte("ls -la\nfile1\nfile2\r\n$ "))
	if got := tr.CWD(); got != "" {
		t.Fatalf("CWD() = %q, want empty with no OSC 7 report", got)
	}
}

func TestTrackerURLDecoding(t *testing.T) {
	tr := &Tracker{}
	// Paths with spaces are URL-encoded in the URI.
	tr.Feed([]byte("\x1b]7;file://h/My%20Documents/Notes\x1b\\"))
	if got := tr.CWD(); got != "/My Documents/Notes" {
		t.Fatalf("CWD() = %q, want %q (decoded)", got, "/My Documents/Notes")
	}
}

func TestTrackerIgnoresOtherOSC(t *testing.T) {
	tr := &Tracker{}
	// OSC 10/11 color queries must not be mistaken for cwd reports.
	tr.Feed([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[6n"))
	if got := tr.CWD(); got != "" {
		t.Fatalf("CWD() = %q, want empty (non-7 OSC ignored)", got)
	}
}

func TestParseURI(t *testing.T) {
	cases := []struct{ uri, want string }{
		{"file:///tmp/x", "/tmp/x"},                              // empty authority
		{"file://host/tmp/x", "/tmp/x"},                          // with authority
		{"file://host/", "/"},                                    // root
		{"", ""},                                                 // empty
		{"not-a-file-uri", ""},                                   // wrong scheme
		{"file://host", ""},                                      // no path
		{"file://host/a%20b", "/a b"},                            // encoded space
		{"file://host/D:/foo", "D:\\foo"},                        // Windows drive-letter path
		{"file://host/d:/projects/rysh", "D:\\projects\\rysh"},   // lower-case drive
		{"file://host/C:/Users/My%20Docs", "C:\\Users\\My Docs"}, // Windows + encoded space
	}
	for _, c := range cases {
		if got := parseURI(c.uri); got != c.want {
			t.Errorf("parseURI(%q) = %q, want %q", c.uri, got, c.want)
		}
	}
}

func TestFindTerminator(t *testing.T) {
	cases := []struct {
		in           string
		end, termLen int
	}{
		{"abc\x1b\\", 3, 2},
		{"abc\x07", 3, 1},
		{"abc", -1, 0},
		{"\x1b", -1, 0}, // lone ESC, incomplete ST
	}
	for _, c := range cases {
		end, termLen := findTerminator([]byte(c.in))
		if end != c.end || termLen != c.termLen {
			t.Errorf("findTerminator(%q) = (%d,%d), want (%d,%d)",
				c.in, end, termLen, c.end, c.termLen)
		}
	}
}

func TestTrackerManyReportsInStream(t *testing.T) {
	tr := &Tracker{}
	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("\x1b]7;file://h/dir")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteString("\x1b\\")
	}
	tr.Feed([]byte(sb.String()))
	if got := tr.CWD(); got != "/dir9" {
		t.Fatalf("CWD() = %q, want %q (last of 50)", got, "/dir9")
	}
}

// Reset clears the tracked cwd and any partial buffer, so a restarted shell
// starts tracking from scratch.
func TestTrackerReset(t *testing.T) {
	tr := &Tracker{}
	// A partial report split across chunks, then a complete one.
	tr.Feed([]byte("\x1b]7;file://h/partial\x1b"))
	tr.Feed([]byte("\\"))
	tr.Feed([]byte("\x1b]7;file://h/real\x1b\\"))
	if got := tr.CWD(); got != "/real" {
		t.Fatalf("CWD() = %q, want /real", got)
	}
	tr.Reset()
	if got := tr.CWD(); got != "" {
		t.Fatalf("CWD() after Reset = %q, want empty", got)
	}
	tr.Feed([]byte("\x1b]7;file://h/again\x1b\\"))
	if got := tr.CWD(); got != "/again" {
		t.Fatalf("CWD() after Reset+report = %q, want /again", got)
	}
}
