package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecute(t *testing.T) {
	// Basic run captures stdout and exit code 0.
	res := Execute("", "echo hello", 5*time.Second)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("echo: exit %d stdout %q", res.ExitCode, res.Stdout)
	}
	// Non-zero exit code is captured.
	res = Execute("", "exit 7", 5*time.Second)
	if res.ExitCode != 7 {
		t.Fatalf("exit 7: got %d", res.ExitCode)
	}
	// stderr is captured separately.
	res = Execute("", "echo oops 1>&2", 5*time.Second)
	if res.ExitCode != 0 || !strings.Contains(res.Stderr, "oops") {
		t.Fatalf("stderr: exit %d stderr %q", res.ExitCode, res.Stderr)
	}
	// The command runs in the given directory.
	dir := t.TempDir()
	cwd := pwdCmd()
	res = Execute(dir, cwd, 5*time.Second)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != dir {
		t.Fatalf("%s: exit %d stdout %q want %q", cwd, res.ExitCode, res.Stdout, dir)
	}
	// Content mentions command, cwd and exit code.
	if c := res.Content(); !strings.Contains(c, cwd) ||
		!strings.Contains(c, "cwd: "+dir) || !strings.Contains(c, "exit code: 0") {
		t.Fatalf("Content = %q", c)
	}
}

func TestExecuteTimeout(t *testing.T) {
	// Long enough to still be running at the 100ms deadline, short enough
	// that the run cannot stall on a child the kill cannot reach (see
	// sleepCmd).
	res := Execute("", sleepCmd(2), 100*time.Millisecond)
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("timeout: timedOut=%v exit=%d", res.TimedOut, res.ExitCode)
	}
	if !strings.Contains(res.Content(), "timed out") {
		t.Fatalf("Content should note the timeout: %q", res.Content())
	}
}

// A cancelled context aborts the run promptly (the command is killed)
// without reporting a timeout.
func TestExecuteCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ExecResult, 1)
	go func() {
		done <- ExecuteCtx(ctx, "", sleepCmd(1))
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.TimedOut || res.ExitCode == 0 {
			t.Fatalf("cancelled run: timedOut=%v exit=%d", res.TimedOut, res.ExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ExecuteCtx did not return after cancel")
	}
}

func TestExecuteTruncation(t *testing.T) {
	// A file rather than a generator: `yes | head` is two POSIX
	// utilities, and the size is what the test is about.
	dir := t.TempDir()
	const name = "big.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat("y", 100000)), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Execute(dir, printFileCmd(name), 5*time.Second)
	if !res.Truncated {
		t.Fatalf("expected truncation, stdout %d bytes", len(res.Stdout))
	}
	if len(res.Stdout) != maxCapture {
		t.Fatalf("captured %d bytes, want cap %d", len(res.Stdout), maxCapture)
	}
}
