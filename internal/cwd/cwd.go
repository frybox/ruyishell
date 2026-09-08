// Package cwd tracks the shell's current working directory via OSC 7
// hyperlink reports. Shells and prompts can emit an OSC 7 sequence
//
//	ESC ] 7 ; file://<host>/<path> ESC \
//
// (or BEL-terminated) whenever the directory changes; terminals use it to
// update the working-directory URL. rysh parses these reports out of the
// child's terminal output, giving it the shell's cwd on every platform,
// including macOS and Windows where /proc is unavailable. On Linux, /proc
// remains a fallback when no OSC 7 report has arrived yet.
package cwd

import (
	"bytes"
	"net/url"
	"strings"
	"sync"
)

// osc7Start is the leading bytes of an OSC 7 report: ESC ] 7 ;
var osc7Start = []byte("\x1b]7;")

// Tracker parses OSC 7 cwd reports out of a stream of terminal output.
// Feed is safe for concurrent use (called from the output loop while CWD is
// read from the driver loop).
type Tracker struct {
	mu  sync.Mutex
	cwd string // latest parsed cwd, "" until the first report
	buf []byte // bytes not yet scanned (may hold a partial sequence)
}

// Feed scans data for OSC 7 cwd reports and records the most recent one.
// The stream may arrive in arbitrary chunks, so partial sequences are kept
// in a bounded buffer across calls.
func (t *Tracker) Feed(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Keep only the tail of the stream: OSC 7 reports are short, and a
	// report split across many tiny reads still lands inside this window.
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	t.buf = append(t.buf, data...)
	for {
		idx := bytes.Index(t.buf, osc7Start)
		if idx < 0 {
			break // no report start in the window
		}
		start := idx + len(osc7Start)
		end, termLen := findTerminator(t.buf[start:])
		if end < 0 {
			break // report not complete yet; keep the tail for the next read
		}
		uri := string(t.buf[start : start+end])
		t.buf = t.buf[start+end+termLen:]
		if c := parseURI(uri); c != "" {
			t.cwd = c
		}
	}
}

// CWD returns the most recent cwd reported via OSC 7, or "" if none has
// been seen yet.
func (t *Tracker) CWD() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

// Reset clears the tracker state, so a restarted shell's first OSC 7 report
// re-establishes the cwd for the new session.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cwd = ""
	t.buf = nil
}

// findTerminator locates the end of an OSC payload (BEL or ESC \), returning
// the offset of the terminator and its length. A missing terminator returns
// -1, 0.
func findTerminator(p []byte) (int, int) {
	for i := 0; i < len(p); i++ {
		switch {
		case p[i] == '\x07': // BEL
			return i, 1
		case p[i] == '\x1b' && i+1 < len(p) && p[i+1] == '\\': // ST
			return i, 2
		}
	}
	return -1, 0
}

// parseURI extracts and URL-decodes the path from a file:// URI. On Windows
// the URI arrives as file://host/D:/foo/bar (a drive-letter path with a
// leading slash); the leading slash is stripped and forward slashes are
// converted to backslashes so the result is a native Windows path. On
// POSIX the path is returned as-is (absolute, leading slash kept).
func parseURI(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	rest := uri[len("file://"):]
	// Skip the authority (hostname); the path begins at the first '/'.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return ""
	}
	path := rest[slash:]
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return ""
	}
	return normalizePath(decoded)
}

// normalizePath converts a decoded file:// path to the platform's native
// form. A Windows drive-letter path arrives with a leading slash
// (file://host/D:/foo -> "/D:/foo"); strip it and flip slashes. POSIX
// paths are already native and returned unchanged.
func normalizePath(decoded string) string {
	// Windows drive-letter path: "/D:..." or "/d:..." -> "D:\...".
	if len(decoded) >= 3 && decoded[0] == '/' && decoded[2] == ':' {
		// Drop the leading slash, keep the drive letter + colon + rest.
		p := decoded[1:]
		// Normalize drive letter to uppercase for consistency.
		if p[0] >= 'a' && p[0] <= 'z' {
			p = string(p[0]-32) + p[1:]
		}
		return strings.ReplaceAll(p, "/", "\\")
	}
	return decoded
}
