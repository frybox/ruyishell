package main

import "io"

// maxCtrlZBurst bounds how many Ctrl+Z bytes a streak of data-less EOFs may
// synthesize. One phantom EOF consumes one ^Z from the console buffer, so a
// human typing ^Z cannot reach the cap; it exists so a reader stuck at EOF
// cannot spin the keyboard goroutine forever. Past it io.EOF is reported and
// the session ends, which is what the pre-existing EOF path does.
const maxCtrlZBurst = 64

// ctrlZReader turns the data-less io.EOF that a Windows console produces for a
// Ctrl+Z keystroke back into the 0x1a byte the user actually pressed; see
// stdinReader in stdin_windows.go.
type ctrlZReader struct {
	r     io.Reader
	burst int
}

func (c *ctrlZReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.burst = 0
		// Bytes came through, so whatever arrived with them is not the end of
		// the stream; the next read re-derives a real error.
		return n, nil
	}
	if err == io.EOF && len(p) > 0 && c.burst < maxCtrlZBurst {
		c.burst++
		p[0] = 0x1a // ^Z
		return 1, nil
	}
	return n, err
}
