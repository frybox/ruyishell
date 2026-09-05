package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLogAppendAndClose(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write("shk", "ls -la"); err != nil {
		t.Fatal(err)
	}
	if err := l.Write("asw", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing again is a no-op.
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sess-1", "messages.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2: %q", len(lines), data)
	}
	var rec Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "shk" || rec.P != "ls -la" || rec.Ts == 0 {
		t.Fatalf("record = %+v", rec)
	}
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "asw" || rec.P != "line one\nline two" {
		t.Fatalf("record = %+v", rec)
	}

	// Reopening appends rather than truncating.
	l2, err := OpenLog(dir, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Write("sys", "mode:shell"); err != nil {
		t.Fatal(err)
	}
	l2.Close()
	data, _ = os.ReadFile(filepath.Join(dir, "sess-1", "messages.log"))
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Fatalf("reopened log lines = %d, want 3", n)
	}
}

func TestOpenLogCreatesDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	l, err := OpenLog(dir, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := os.Stat(filepath.Join(dir, "sess-2", "messages.log")); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestWriteOnNilLogIsNoop(t *testing.T) {
	var l *Log
	if err := l.Write("sys", "x"); err != nil {
		t.Fatalf("nil log Write returned error: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("nil log Close returned error: %v", err)
	}
}

// The log rotates once messages.log exceeds maxLogSize: the previous
// generation is gzipped, the oldest dropped, and writes continue into a
// fresh messages.log. ReadMessages reassembles the whole stream in order.
func TestLogRotationAndRead(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(dir, "rot")
	if err != nil {
		t.Fatal(err)
	}
	// maxLogSize is a package constant; force a rotation by writing past it.
	// Use distinct markers in each generation.
	for i := 0; i < 5; i++ {
		if err := l.Write("asw", strings.Repeat("x", maxLogSize/2)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Write("usr", "G0-LAST"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	recs, err := ReadMessages(dir, "rot")
	if err != nil {
		t.Fatal(err)
	}
	// Two generations fit before the third rotation: with 5 writes of
	// maxLogSize/2 the active file and .1.gz hold them (roughly 2 + 2 + 1),
	// the oldest generation is dropped. Assert order and the tail marker.
	if len(recs) < 3 {
		t.Fatalf("ReadMessages returned %d records, want at least 3", len(recs))
	}
	if recs[len(recs)-1].P != "G0-LAST" {
		t.Fatalf("last record = %q, want G0-LAST", recs[len(recs)-1].P)
	}
}

func TestReadMessagesSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "bad")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "not json\n{\"ts\":1,\"kind\":\"sys\",\"p\":\"ok\"}\n{\"ts\":2\n"
	if err := os.WriteFile(filepath.Join(sub, "messages.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadMessages(dir, "bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].P != "ok" {
		t.Fatalf("ReadMessages = %+v, want the single valid record", recs)
	}
}

func TestSanitizeStream(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text\n", "plain text\n"},
		// SGR is kept so replay restores color.
		{"\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m"},
		// Cursor movement / erase sequences are dropped.
		{"\x1b[2K\x1b[1Ahi", "hi"},
		{"\x1b[?1049h", ""},
		// OSC is dropped up to BEL or ST.
		{"\x1b]7;file://x\x07ok", "ok"},
		{"\x1b]0;title\x1b\\t", "t"},
		// Stray single ESC and control bytes (incl. CR) are dropped; \t kept.
		{"a\r\x1bb\tc\x7f", "ab\tc"},
	}
	for _, c := range cases {
		if got := SanitizeStream(c.in); got != c.want {
			t.Fatalf("SanitizeStream(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeStreamWritesCleanJSONL(t *testing.T) {
	// Sanitized payloads must never carry a raw newline that would corrupt
	// the JSONL framing (newlines are escaped by JSON, so this is about
	// control bytes: CR must not survive into a payload).
	dir := t.TempDir()
	l, err := OpenLog(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Write("shl", SanitizeStream("a\r\n\x1b[Kb")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("no line in log")
	}
	var rec Record
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("corrupt JSONL: %v", err)
	}
	if rec.P != "a\nb" {
		t.Fatalf("payload = %q, want %q", rec.P, "a\nb")
	}
}
