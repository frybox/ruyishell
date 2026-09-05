package agent

// The bash tool (§5.3, §8.1): one fresh shell per call, in workdir or the
// shell's cwd, bounded by a per-call timeout the model may raise up to the
// configured maximum. State (cd, exports) does not survive between calls —
// that is stated in the tool description. With a job manager and a
// positive auto-background window, a call still running when the window
// closes becomes a background job (§8.2) and the tool result points the
// model at job_output instead of blocking.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// bashTool builds the bash executor rooted at the task's default
// directory, with the timeout knobs and the auto-background face.
func bashTool(cwd string, o ToolOpts) Tool {
	return Tool{
		Name: "bash",
		Description: "Run a shell command non-interactively and return its captured output. " +
			"Every call is a brand-new shell: cd/export do NOT persist between calls — pass workdir to run somewhere else. " +
			"Filter long output in the command itself (grep/head); prefer the read tool for files. " +
			"A command still running when the auto-background window closes keeps running as a background job — " +
			"poll job_output instead of re-running or waiting.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to run (required)"},
				"workdir": map[string]any{"type": "string", "description": "Working directory for this command (optional, default: current directory)"},
				"timeout": map[string]any{"type": "integer", "description": fmt.Sprintf("Timeout in seconds (optional, default %d, max %d)", int(o.BashTimeout.Seconds()), int(o.BashMaxTimeout.Seconds()))},
			},
			"required": []string{"command"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			cmd, err := argString(args, "command")
			if err != nil || strings.TrimSpace(cmd) == "" {
				return Result{}, fmt.Errorf("缺少参数 command")
			}
			dir := cwd
			if wd := argOptString(args, "workdir"); wd != "" {
				dir = resolvePath(cwd, wd)
			}
			timeout := o.BashTimeout
			if secs, ok := argOptInt(args, "timeout"); ok {
				if secs < 1 {
					secs = 1
				}
				if max := int(o.BashMaxTimeout.Seconds()); secs > max {
					secs = max
				}
				timeout = time.Duration(secs) * time.Second
			}

			// runCtx derives from the task ctx, so an engine-level tool
			// interrupt (§8.3 ^C during exec) kills the command while the
			// task itself survives.
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			// render assembles the fed-back result; it is the single
			// shape for the inline, timeout and interrupted exits.
			render := func(stdout, stderr string, code int, dur time.Duration, timedOut, interrupted bool) Result {
				body := stdout
				if stderr != "" {
					if body != "" && !strings.HasSuffix(body, "\n") {
						body += "\n"
					}
					body += "stderr: " + stderr
				}
				body = truncateMid(body, 48*1024, 16*1024)
				meta := fmt.Sprintf("exit %d · %.1fs · cwd %s", code, dur.Seconds(), dir)
				if timedOut {
					meta = fmt.Sprintf("exit %d · 超时（%ds）已终止 · cwd %s", code, int(timeout.Seconds()), dir)
					body += fmt.Sprintf("\n[timed out; killed after %ds]", int(timeout.Seconds()))
				} else if interrupted {
					meta += " · 被用户中断"
					body += "\n[被用户中断]"
				}
				return Result{
					Output:  body,
					Meta:    meta,
					Display: "$ " + strings.Join(strings.Fields(cmd), " ") + fmt.Sprintf(" (exit %d)", code),
					Code:    code,
				}
			}

			if o.Jobs == nil || o.AutoBackground <= 0 {
				start := time.Now()
				res := ExecuteCtx(runCtx, dir, cmd)
				timedOut := res.TimedOut
				interrupted := !timedOut && runCtx.Err() != nil
				return render(res.Stdout, res.Stderr, res.ExitCode, time.Since(start), timedOut, interrupted), nil
			}

			job := o.Jobs.startProcess(dir, cmd)
			timer := time.NewTimer(o.AutoBackground)
			defer timer.Stop()
			select {
			case <-job.DoneCh():
				// Finished inside the window (or the window and the
				// process expired together — the expiry shapes win).
				output := job.Output()
				if err := runCtx.Err(); errors.Is(err, context.DeadlineExceeded) {
					return render(output, "", -1, job.Elapsed(), true, false), nil
				} else if err != nil {
					return render(output, "", -1, job.Elapsed(), false, true), nil
				}
				return render(output, "", job.ExitCode(), job.Elapsed(), false, false), nil
			case <-runCtx.Done():
				job.Kill()
				<-job.DoneCh()
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
					return render(job.Output(), "", -1, job.Elapsed(), true, false), nil
				}
				return render(job.Output(), "", -1, job.Elapsed(), false, true), nil
			case <-timer.C:
				// Still running: hand the job to the manager and point
				// the model at job_output (§8.1).
				id := o.Jobs.register(job)
				return Result{
					Output: fmt.Sprintf("命令仍在运行，已转后台 job %d：用 job_output 查看进度（job_kill 终止）。\n$ %s", id, cmd),
					Meta:   fmt.Sprintf("转后台 job %d · cwd %s", id, dir),
					Display: "$ " + strings.Join(strings.Fields(cmd), " ") +
						fmt.Sprintf(" (后台 job %d)", id),
					Code: 0,
				}, nil
			}
		},
	}
}
