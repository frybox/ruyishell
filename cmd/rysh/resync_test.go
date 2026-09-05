package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOutputTapSinceMark checks that a mark delimits the recorded stream: bytes
// written before the mark stay out of the tail taken after it, and a mark older
// than the kept tail is clamped to the front of the tail instead of reading
// past it.
func TestOutputTapSinceMark(t *testing.T) {
	var tap outputTap
	tap.write([]byte("old prompt $ "))
	mark := tap.mark()
	tap.write([]byte("echo hi"))

	if got := tap.since(mark); got != "echo hi" {
		t.Fatalf("since(mark) = %q, want %q", got, "echo hi")
	}
	if got := tap.since(mark - 5); !strings.HasPrefix(got, "pt $ echo hi") {
		t.Fatalf("since before the mark = %q, want it to start inside the old text", got)
	}
	if got := tap.since(mark + int64(len("echo hi"))); got != "" {
		t.Fatalf("since(end) = %q, want empty", got)
	}

	// Overrun the ring: the mark taken before is now older than the tail, so
	// since() must return the retained tail rather than slice out of range.
	big := strings.Repeat("x", 3*tapBytes)
	tap.write([]byte(big))
	tail := tap.since(mark)
	if len(tail) != tapBytes {
		t.Fatalf("after an overrun the tail holds %d bytes, want the %d-byte bound", len(tail), tapBytes)
	}
	if !strings.HasSuffix(tail, "x") || !strings.Contains(tail, strings.Repeat("x", 64)) {
		t.Fatalf("overrun tail is not the last %d bytes: %q", tapBytes, tail[:32])
	}
}

// fakeShell is a stand-in for the child pty in tests of the re-injection: every
// write is echoed into a tap the way a shell echoes typed input, and the echo
// can be made to drop bytes the way a pty mid-resize does.
type fakeShell struct {
	mu       sync.Mutex
	tap      *outputTap
	writeLog []string
	// drop is how many leading bytes of the next echo to swallow.
	drop int
	// prompt is echoed ahead of every line echo, the way a shell repaints.
	prompt string
	silent bool
}

func (f *fakeShell) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := string(b)
	f.writeLog = append(f.writeLog, s)
	if f.silent || f.tap == nil {
		return len(b), nil
	}
	echo := s
	if f.drop > 0 {
		echo = echo[f.drop:]
		f.drop = 0
	}
	f.tap.write([]byte(f.prompt + echo))
	return len(b), nil
}

func (f *fakeShell) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writeLog...)
}

// The re-injection is verified against the shell's echo: a line that comes back
// short is cleared and typed again, so a dropped byte costs latency instead of
// silently truncating the command the user is about to run.
func TestInjectResidualRetypesShortEcho(t *testing.T) {
	var tap outputTap
	sh := &fakeShell{tap: &tap, prompt: "\r\n$ ", drop: 1}
	clear := string(residualClear(true))

	injectResidual(sh, &tap, "echo ABXCDE", true)

	// line, clear, line: the leftover fragment is erased before the retry.
	want := []string{"echo ABXCDE", clear, "echo ABXCDE"}
	if got := sh.writes(); len(got) != len(want) || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %q, want %q", got, want)
	}
}

// A shell that echoes nothing must not stall the mode switch: the retry is
// bounded, so the line is typed at most residualAttempts times.
func TestInjectResidualBoundedWhenSilent(t *testing.T) {
	var tap outputTap
	sh := &fakeShell{tap: &tap, silent: true}
	clear := string(residualClear(false))

	start := time.Now()
	injectResidual(sh, &tap, "echo hi", false)
	elapsed := time.Since(start)

	want := strings.Repeat("echo hi|"+clear+"|", residualAttempts-1) + "echo hi"
	if got := strings.Join(sh.writes(), "|"); got != want {
		t.Fatalf("writes = %q, want %q", got, want)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("silent shell took %v, want a bounded mode switch", elapsed)
	}
}

// A line that lands whole on the first try must not be re-typed, and the check
// must not wait out its full timeout.
func TestInjectResidualSingleAttemptWhenEchoed(t *testing.T) {
	var tap outputTap
	sh := &fakeShell{tap: &tap, prompt: "\r\n$ "}

	start := time.Now()
	injectResidual(sh, &tap, "echo hi", false)

	if got := sh.writes(); len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("writes = %q, want the line once", got)
	}
	if elapsed := time.Since(start); elapsed > echoWait {
		t.Fatalf("clean re-injection took %v, want well under the %v echo bound", elapsed, echoWait)
	}
}

// TestResidualClearSurvivesALostByte checks the point of the line-editor clear
// carrying three bytes: whichever one of them the terminal drops, what is left
// still empties the line, so a re-typed line cannot be appended to a fragment.
func TestResidualClearSurvivesALostByte(t *testing.T) {
	const fragment = "cho ABXCDE"
	clear := residualClear(true)
	for lost := range clear {
		line, col := []rune(fragment), len([]rune(fragment))
		for i, b := range clear {
			if i == lost {
				continue
			}
			switch b {
			case 0x15: // ^U: kill what sits before the cursor
				line, col = append([]rune{}, line[col:]...), 0
			case 0x01: // ^A: cursor to the start of the line
				col = 0
			case 0x0b: // ^K: kill what sits after the cursor
				line = append([]rune{}, line[:col]...)
			}
		}
		if len(line) > 0 {
			t.Fatalf("dropping byte %d leaves %q on the line, want it empty", lost, string(line))
		}
	}
}
