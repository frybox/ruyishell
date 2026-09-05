package main

import (
	"io"
	"strings"
	"sync"
	"time"

	"ruyishell/internal/aiui"
)

// outputTap keeps the tail of the child pty's output stream so the driver can
// check what the shell actually echoed. leaveAI re-injects the shared input
// line by writing it into the child's pty, where it is read by the shell's own
// line editor; whether the line landed in full is only observable in the echo.
// The tap records exactly that: the bytes the child wrote, unmodified, so a
// caller can mark a position and later ask whether a string appeared after it.
//
// On Windows the child sits behind ConPTY, and a pty input write issued while
// the pseudo console is still processing a ResizePseudoConsole can be dropped
// outright: the shell never sees the first character, so the re-injected line
// comes back one character short and running it fails. leaveAI therefore
// verifies every re-injection against the echo and re-types a line that did not
// land, instead of trusting a timed wait.
type outputTap struct {
	mu sync.Mutex
	// buf holds the last tapBytes of output; dropped is how many bytes were
	// trimmed from its front, so marks stay comparable across trims.
	buf     []byte
	dropped int64
}

// tapBytes bounds the retained tail. It only has to cover one mode switch.
const tapBytes = 16 << 10

// mark returns a token identifying the current end of the tap. Passing it back
// to since yields everything the child wrote after the mark was taken.
func (t *outputTap) mark() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped + int64(len(t.buf))
}

// since returns the recorded output after a mark. A mark older than the kept
// tail is clamped to the front of the tail.
func (t *outputTap) since(mark int64) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.dropped + int64(len(t.buf))
	if mark < t.dropped {
		mark = t.dropped
	}
	if mark >= end {
		return ""
	}
	return string(t.buf[mark-t.dropped:])
}

// write appends one chunk read from the child pty, keeping only the tail.
func (t *outputTap) write(chunk []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, chunk...)
	if over := int64(len(t.buf) - tapBytes); over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.dropped += over
	}
}

// echoWait and echoPoll bound how long leaveAI waits for the shell to echo back
// a re-injected line before deciding it did not land. A line that lands is
// echoed within a few milliseconds; the bound only delays the retry.
const (
	echoWait = 300 * time.Millisecond
	echoPoll = 5 * time.Millisecond
)

// residualAttempts is how many times leaveAI tries to get the shared line into
// the shell: the first injection plus one re-type after a short wait, which is
// the most that can happen without a visible stutter on a mode switch.
const residualAttempts = 2

// residualClear is the key sequence a retry sends to empty the shell's input
// line before re-typing it, so it cannot append to the fragment it replaces. A
// retry goes in right after a failed verification, which is also when a write
// can lose bytes, so the line-editor form carries two independent kills: ^U
// erases backwards from the cursor, ^A ^K forwards from the line start, and any
// one of the three bytes missing still leaves a working pair. A shell with no
// line editor understands only ^U, where the other two would be literal input.
func residualClear(lineEditor bool) []byte {
	if lineEditor {
		return []byte{0x15, 0x01, 0x0b} // ^U ^A ^K
	}
	return []byte{0x15} // ^U
}

// injectResidual writes a re-injected line into the child pty and confirms the
// shell echoed all of it back. A line that came back short or not at all is
// cleared and typed again, so a dropped write costs a little latency instead of
// a silently truncated command.
func injectResidual(p io.Writer, tap *outputTap, line string, lineEditor bool) {
	clear := residualClear(lineEditor)
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			// Drop whatever partial line the shell did keep before re-typing,
			// so a retry cannot append to a fragment.
			if len(clear) > 0 {
				_, _ = p.Write(clear)
			}
			time.Sleep(echoWait / 2)
		}
		mark := tap.mark()
		_, _ = p.Write([]byte(line))
		if waitResidualEcho(tap, mark, line, echoWait) || attempt >= residualAttempts {
			return
		}
	}
}

// waitResidualEcho polls the tapped output until the echo of want shows up
// whole, and reports whether it did. The echo is compared on visible text:
// ConPTY repaints a line with cursor moves and erase sequences interleaved, so
// the raw bytes of a correctly echoed line are not contiguous. The wanted text
// goes through the same filter, so a draft carrying control bytes of its own
// still matches its own echo.
func waitResidualEcho(tap *outputTap, mark int64, want string, wait time.Duration) bool {
	needle := aiui.Sanitize(want)
	deadline := time.Now().Add(wait)
	for {
		if strings.Contains(aiui.Sanitize(tap.since(mark)), needle) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(echoPoll)
	}
}
