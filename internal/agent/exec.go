package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxCapture caps how much of each output stream is kept for context, so a
// chatty command cannot blow up the conversation.
const maxCapture = 32 * 1024

// ExecResult is the captured outcome of one executed command.
type ExecResult struct {
	Dir       string // working directory the command ran in
	Command   string
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool // the run was killed after the timeout
	Truncated bool // output exceeded maxCapture and was cut
}

// Execute runs command through the platform shell in dir, capturing stdout,
// stderr and the exit code. The command gets no stdin (non-interactive).
// timeout bounds the run; on expiry the whole process group is killed and
// TimedOut is set.
func Execute(dir, command string, timeout time.Duration) ExecResult {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return ExecuteCtx(ctx, dir, command)
}

// ExecuteCtx is Execute with an explicit context: the run is cancelled when
// ctx is done, killing the whole process group. The bash tool uses it so
// that ^C aborts a foreground command instead of waiting for the timeout.
func ExecuteCtx(ctx context.Context, dir, command string) ExecResult {
	name, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out := &cappedBuffer{}
	errOut := &cappedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = errOut
	configureProcessGroup(cmd)

	err := cmd.Run()
	res := ExecResult{
		Dir:       dir,
		Command:   command,
		Stdout:    out.String(),
		Stderr:    errOut.String(),
		Truncated: out.truncated || errOut.truncated,
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
		return res
	}
	res.ExitCode = 0
	return res
}

// Content formats the result as the context message fed back to the model.
func (r ExecResult) Content() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[tool] $ %s\n", r.Command)
	if r.Dir != "" {
		fmt.Fprintf(&b, "cwd: %s\n", r.Dir)
	}
	fmt.Fprintf(&b, "exit code: %d\n", r.ExitCode)
	if r.TimedOut {
		b.WriteString("[timed out; killed]\n")
	}
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		fmt.Fprintf(&b, "stderr: %s", r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Truncated {
		b.WriteString("[output truncated]\n")
	}
	return b.String()
}

// cappedBuffer accumulates output up to maxCapture bytes, remembering
// whether more arrived, without ever blocking the child's writes.
type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := maxCapture - b.buf.Len()
	if room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	if room < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
