// Session log: each session's stream — shell input/output and the AI
// conversation, in arrival order — is appended to an append-only JSONL file
// that is the session's source of truth on disk. The screen is only the
// live window over this log. Every write happens under the driver's writeMu,
// so the Log mutex is a secondary guard rather than the primary one.
package session

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Record is one JSONL line: a timestamped, typed payload. kinds: shl (shell
// output), shk (submitted shell command), usr (AI user prompt), rea
// (reasoning block), asw (AI reply), tool (tool call/result), noti (system
// notice), sys (meta event: mode switch, task start/end/terminate),
// approval (permission ask/decision), compact (a context compaction's
// checkpoint full text, M7.6), and the s* family for worker-subagent
// events (srea/sasw/stool/snoti/sub/scomp) — reconstructHistory rebuilds
// conversation context from usr/asw/tool only, so notices and checkpoints
// never re-enter a rebuilt history.
type Record struct {
	// Ts is nanoseconds since the epoch (UnixNano), matching the
	// nanosecond ShellEvent times the timeline merge compares against;
	// ms-granularity stamps let a same-millisecond shell command sort on
	// the wrong side of a rebuilt AI message.
	Ts   int64  `json:"ts"`
	Kind string `json:"kind"`
	P    string `json:"p"`
	// CallID names the tool call a "tool" record answers (empty for every
	// other kind). The asw record carries no CallID; the call requests it
	// pairs with travel inside Calls on the preceding asw record.
	CallID string `json:"call_id,omitempty"`
	// Calls lists the native tool calls a finalized assistant turn
	// requested (set on "asw" records only). It lets a rebuilt history
	// pair each tool result with the call that requested it.
	Calls []ToolCallRecord `json:"calls,omitempty"`
}

// ToolCallRecord is the session-log shape of one native tool call: enough
// to rebuild the assistant message's ToolCalls verbatim. It mirrors
// provider.ToolCall without importing the provider package (the session
// package sits below provider on the dependency graph).
type ToolCallRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// maxLogSize bounds messages.log before it rotates (see Log.Write).
const maxLogSize = 1 << 20 // ~1MB

// Log is an append-only JSONL writer for one session, stored at
// <dir>/<id>/messages.log with rotation.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	dir  string
	id   string
	Path string
	size int64 // bytes written to f since it was opened (rotation trigger)
}

// OpenLog creates (or appends to) <dir>/<id>/messages.log, creating any
// missing parent directories. A concurrent rysh on the same session id is
// not expected; O_APPEND makes overlapping appends safe anyway.
func OpenLog(dir, id string) (*Log, error) {
	sub := filepath.Join(dir, id)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, fmt.Errorf("session log: mkdir %s: %w", sub, err)
	}
	path := filepath.Join(sub, "messages.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session log: open %s: %w", path, err)
	}
	l := &Log{f: f, dir: dir, id: id, Path: path}
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return l, nil
}

// Write appends one JSONL record, rotating the log first when it has grown
// past maxLogSize so the session stream never grows without bound.
func (l *Log) Write(kind, payload string) error {
	return l.WriteRecord(Record{Ts: time.Now().UnixNano(), Kind: kind, P: payload})
}

// WriteRecord appends one JSONL record (kind, payload, and optional tool
// pairing fields), rotating first when the log has grown past maxLogSize.
func (l *Log) WriteRecord(rec Record) error {
	if l == nil || l.f == nil {
		return nil
	}
	if rec.Ts == 0 {
		rec.Ts = time.Now().UnixNano()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(b)) > maxLogSize {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return err
}

// rotateLocked closes the current messages.log, rolls the previous
// generations back (messages.log.1 gzipped to messages.log.1.gz, the oldest
// dropped — two generations are kept), and reopens a fresh messages.log.
// The caller must hold l.mu.
func (l *Log) rotateLocked() error {
	active := filepath.Join(l.dir, l.id, "messages.log")
	gen1 := active + ".1"
	gen1gz := gen1 + ".gz"
	// Drop the oldest generation first.
	_ = os.Remove(gen1gz)
	// Compress the previous generation if present.
	if _, err := os.Stat(gen1); err == nil {
		if err := gzipFile(gen1, gen1gz); err != nil {
			return err
		}
		_ = os.Remove(gen1)
	}
	// Roll the active file back one generation.
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	if err := os.Rename(active, gen1); err != nil {
		return err
	}
	f, err := os.OpenFile(active, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	l.size = 0
	return nil
}

// Close flushes and closes the log file. It is idempotent.
func (l *Log) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.f.Close()
	l.f = nil
	return err
}

// ReadMessages reconstructs the full stream of a session from its rotation
// chain (oldest to newest): messages.log.1.gz, messages.log.1, then
// messages.log. Unparseable lines are skipped.
func ReadMessages(dir, id string) ([]Record, error) {
	var recs []Record
	// Oldest generation first: the gzipped .1, then the plain .1, then the
	// active messages.log.
	base := filepath.Join(dir, id)
	paths := []string{
		filepath.Join(base, "messages.log.1.gz"),
		filepath.Join(base, "messages.log.1"),
		filepath.Join(base, "messages.log"),
	}
	for _, p := range paths {
		if err := readMessageFile(p, &recs); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return recs, nil
}

func readMessageFile(path string, recs *[]Record) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err == nil {
			*recs = append(*recs, rec)
		}
	}
	return sc.Err()
}

// gzipFile compresses src into dst.
func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Keep-SGR stream sanitizer. Only SGR ("ESC [ ... m") survives, so replay
// restores colors/styles; cursor-movement and erase sequences, OSC, other
// single-ESC sequences, and stray control bytes are dropped so replay never
// disturbs the cursor position or the screen's state.
var (
	// escapeSeq matches any CSI sequence (including SGR), OSC sequence up
	// to BEL or ST, and other single-ESC introducers.
	escapeSeq = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]|\x1b\][^\x07]*(?:\x07|\x1b\\)|\x1b[@-_]`)
)

// SanitizeStream keeps SGR color/style sequences from s and strips every
// other escape sequence plus control bytes (except \n and \t). The result
// can be replayed to the screen without disturbing the cursor or relying on
// the screen's current state.
func SanitizeStream(s string) string {
	// Wrap kept SGR sequences in NUL sentinels so the control-byte pass
	// below can copy them verbatim instead of dropping their leading ESC.
	s = escapeSeq.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasSuffix(m, "m") {
			return "\x00" + m + "\x00"
		}
		return ""
	})
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == 0 {
			in = !in
			continue
		}
		if in {
			b.WriteRune(r) // inside a kept SGR: copy verbatim
			continue
		}
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// drop other control chars (incl. CR)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeStreamKeepCR is SanitizeStream with one difference: carriage
// returns are kept as well. It is used to flush deferred shell output back
// to the screen on return from AI mode, where the terminal is in raw mode
// and a bare LF alone would not return the cursor to column 1 (so the
// replay would stagger). Cursor-movement and erase sequences are still
// dropped, which also consumes the erase echo of the ^U sent to clear the
// shell's residual input on entry.
func SanitizeStreamKeepCR(s string) string {
	s = escapeSeq.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasSuffix(m, "m") {
			return "\x00" + m + "\x00"
		}
		return ""
	})
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == 0 {
			in = !in
			continue
		}
		if in {
			b.WriteRune(r) // inside a kept SGR: copy verbatim
			continue
		}
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// drop other control chars (incl. backspace)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
