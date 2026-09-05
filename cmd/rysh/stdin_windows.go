//go:build windows

package main

import (
	"io"
	"os"

	"golang.org/x/term"
)

// stdinReader returns the byte source for the keyboard reader.
//
// A Windows console never delivers a literal Ctrl+Z to a Go program: the
// console read path stops copying at 0x1a and reports the empty remainder as
// io.EOF, even though the stream itself keeps working afterwards. Taken at
// face value that EOF ends the keyboard reader for good, which turns ^Z into
// "input dies". On a console terminal, translate the data-less EOF back into
// the ^Z byte. Pipe and file stdin keep ordinary EOF semantics, so
// end-of-input still stops rysh there.
func stdinReader() io.Reader {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stdin
	}
	return &ctrlZReader{r: os.Stdin}
}
