//go:build !windows

package main

import (
	"io"
	"os"
)

// stdinReader returns the byte source for the keyboard reader. Unix pseudo
// terminals report Ctrl+Z as an ordinary 0x1a byte, so no translation is
// needed and io.EOF means the input really is over.
func stdinReader() io.Reader { return os.Stdin }
