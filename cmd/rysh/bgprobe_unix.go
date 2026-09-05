//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"ruyishell/internal/theme"
)

// probeTerminalBg requests the terminal's background color (OSC 11 query)
// and returns a polarity hint from the answer. It must run before the key
// reader starts, like probeTerminalTitle: nothing else reads stdin at that
// point and the reads are non-blocking toggles around a single fd. A
// terminal that does not answer within the deadline yields PolUnknown and the
// theme falls back to the dark palette (the common case).
func probeTerminalBg(deadline time.Duration) theme.Polarity {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return theme.PolUnknown
	}
	os.Stdout.WriteString("\x1b]11;?\x1b\\")
	fd := int(os.Stdin.Fd())
	buf := make([]byte, 256)
	var acc []byte
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		setNonblock(fd, true)
		n, _ := unix.Read(fd, buf)
		setNonblock(fd, false)
		if n <= 0 { // EAGAIN (nothing yet) or a transient read error
			time.Sleep(5 * time.Millisecond)
			continue
		}
		acc = append(acc, buf[:n]...)
		if pol := parseBgResponse(acc); pol != theme.PolUnknown {
			return pol
		}
	}
	return theme.PolUnknown
}

// parseBgResponse looks for an OSC 11 answer in the accumulated input:
// \x1b]11;<value><BEL|ST> where <value> is rgb:RR/RR/BB/BB/BB/BB (hex).
// Returns PolUnknown when the answer is not (yet) parseable, so the probe
// keeps waiting until the deadline.
func parseBgResponse(acc []byte) theme.Polarity {
	idx := strings.Index(string(acc), "\x1b]11;")
	if idx < 0 {
		return theme.PolUnknown
	}
	rest := acc[idx+len("\x1b]11;"):]
	for _, term := range []string{"\x1b\\", "\x07"} {
		if i := strings.Index(string(rest), term); i >= 0 {
			return parseBgValue(string(rest[:i]))
		}
	}
	return theme.PolUnknown
}

// parseBgValue turns an OSC 11 value into a polarity. Only an explicit
// rgb:RR/RR/BB/BB/BB/BB answer is trustworthy enough to flip the theme;
// "none", "?", or indexed answers leave polarity unknown.
func parseBgValue(v string) theme.Polarity {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "rgb:") {
		return theme.PolUnknown
	}
	hex := strings.TrimSuffix(strings.TrimPrefix(v, "rgb:"), "/")
	if len(hex) != 12 {
		return theme.PolUnknown
	}
	seg := func(i int) (byte, bool) {
		n, err := strconv.ParseUint(hex[i:i+2], 16, 8)
		if err != nil {
			return 0, false
		}
		return byte(n), true
	}
	r, ok1 := seg(0)
	g, ok2 := seg(4)
	b, ok3 := seg(8)
	if !ok1 || !ok2 || !ok3 {
		return theme.PolUnknown
	}
	return bgPolarity(r, g, b)
}

// bgPolarity maps an RGB background to a polarity by perceived luminance
// (Rec. 709 luma coefficients).
func bgPolarity(r, g, b byte) theme.Polarity {
	lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	if lum >= 128 {
		return theme.PolLight
	}
	return theme.PolDark
}
