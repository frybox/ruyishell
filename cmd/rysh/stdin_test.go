package main

import (
	"errors"
	"io"
	"testing"
)

// eofOnceReader reports one data-less io.EOF (what a Windows console returns
// for a lone Ctrl+Z) and then delivers the rest of the input normally.
type eofOnceReader struct {
	fired bool
	data  []byte
}

func (r *eofOnceReader) Read(p []byte) (int, error) {
	if !r.fired {
		r.fired = true
		return 0, io.EOF
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestCtrlZReaderTurnsDatalessEOFIntoCtrlZ(t *testing.T) {
	src := &eofOnceReader{data: []byte("hi")}
	c := &ctrlZReader{r: src}

	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if n != 1 || err != nil || buf[0] != 0x1a {
		t.Fatalf("Read after console EOF = (%d, %v, %q), want (1, nil, ^Z)", n, err, buf[:n])
	}
	// Input keeps flowing afterwards: the EOF was a keystroke, not the end of
	// the stream.
	n, err = c.Read(buf)
	if err != nil || n != 2 || string(buf[:n]) != "hi" {
		t.Fatalf("Read = (%d, %v, %q), want (2, nil, \"hi\")", n, err, buf[:n])
	}
	// The source is drained: the next data-less EOF is still reported once as
	// ^Z (a real Ctrl+Z cannot be told apart from a drained buffer here), and
	// the burst cap eventually lets io.EOF through.
	for i := 0; i < maxCtrlZBurst; i++ {
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("Read %d: unexpected error %v before the burst cap", i, err)
		}
	}
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("Read past the burst cap = %v, want io.EOF", err)
	}
}

func TestCtrlZReaderPassesOtherErrorsThrough(t *testing.T) {
	want := errors.New("console gone")
	c := &ctrlZReader{r: errReader{want}}
	buf := make([]byte, 16)
	if _, err := c.Read(buf); !errors.Is(err, want) {
		t.Fatalf("Read error = %v, want %v", err, want)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
