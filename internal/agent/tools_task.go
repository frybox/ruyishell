package agent

// The manager's delegation tools (§16): task spawns a worker subagent,
// task_output polls its state or collects its report, task_kill stops a
// worker that runs wild. Only the manager's registry carries them —
// workers get the plain default registry and cannot nest (depth 1).

import (
	"context"
	"fmt"
	"strings"
)

// taskTool spawns one worker under the snapshot of the manager task that
// created the registry.
func taskTool(sm *SubagentManager, snap SubagentSnapshot) Tool {
	return Tool{
		Name: "task",
		Description: "Delegate a piece of work to a worker subagent. The worker runs its own " +
			"agent loop (own context, own tools) and reports back; you see only its compact " +
			"status (task_output) and its final report — your own context stays lean because " +
			"the worker's transcript never enters it. Delegate every non-trivial piece of " +
			"work (exploration, code changes, builds, tests, debugging). The prompt must be a " +
			"self-contained brief: goal, relevant paths, constraints, acceptance criteria " +
			"(what counts as done, how to verify) and what to report — the worker cannot see " +
			"your context, so a brief without acceptance criteria drifts. Up to 3 concurrent " +
			"workers; check progress with task_output, stop a stuck or off-brief worker with " +
			"task_kill and re-dispatch with a corrected prompt.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{"type": "string", "description": "Short label for the screen and logs (a few words)"},
				"prompt": map[string]any{
					"type": "string",
					"description": "Self-contained work brief for the worker: goal, relevant paths, constraints, acceptance criteria (what counts as done and how to verify), and what to report. " +
						"The worker sees only this text — anything not in the brief, it guesses.",
				},
			},
			"required": []string{"description", "prompt"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			desc, err := argString(args, "description")
			if err != nil {
				return Result{}, err
			}
			brief, err := argString(args, "prompt")
			if err != nil {
				return Result{}, err
			}
			desc = oneLine(desc, 60)
			if desc == "" {
				desc = "子任务"
			}
			id, err := sm.Spawn(desc, brief, snap)
			if err != nil {
				return Result{
					Meta:    "派遣失败",
					Code:    1,
					Output:  "错误: " + err.Error(),
					Display: "task 派遣被拒（已达并发上限）",
				}, nil
			}
			return Result{
				Output:  fmt.Sprintf("[task %d] 已派遣：%s\n用 task_output 查看进展（每隔几步一次即可）；卡住或跑偏时用 task_kill 停掉并重新派遣。", id, desc),
				Meta:    fmt.Sprintf("已派遣 task %d", id),
				Display: fmt.Sprintf("task %d: %s", id, desc),
				Code:    0,
			}, nil
		},
	}
}

// taskOutputTool reports a worker's compact state — while running this is
// the manager's monitoring channel (step count, elapsed time, last
// action, plan); once finished it carries the bounded report for
// acceptance.
func taskOutputTool(sm *SubagentManager) Tool {
	return Tool{
		Name: "task_output",
		Description: "Check a worker subagent's status by its task id. Running: step count, " +
			"elapsed time and the last action — poll every few steps, not every step. " +
			"Finished: its report — verify the key claims (run the relevant test, inspect " +
			"the changed files) before accepting the work. Stopped: what it had done before " +
			"it was killed.",
		ReadOnly: true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "The task id from the 已派遣 result"},
			},
			"required": []string{"id"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			id, err := argInt(args, "id")
			if err != nil {
				return Result{}, err
			}
			sub := sm.Get(id)
			if sub == nil {
				return Result{}, fmt.Errorf("task %d 不存在（可能已结束并被清理，或 id 有误）", id)
			}
			st := sub.Status()
			return Result{
				Output:  st,
				Meta:    firstLine(st),
				Display: fmt.Sprintf("task_output %d", id),
			}, nil
		},
	}
}

// taskKillTool stops a worker that the manager judged stuck or off-brief:
// the run context cancels, the partial state is recorded, and the manager
// re-dispatches with a corrected brief.
func taskKillTool(sm *SubagentManager) Tool {
	return Tool{
		Name: "task_kill",
		Description: "Stop a running worker subagent (kills its run; its unfinished work " +
			"stays in the workspace). Use it when a worker stalls, repeats itself, or goes " +
			"off its brief; then re-dispatch with a corrected prompt. Finished workers " +
			"cannot be killed.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "The task id to stop"},
			},
			"required": []string{"id"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			id, err := argInt(args, "id")
			if err != nil {
				return Result{}, err
			}
			sub := sm.Get(id)
			if sub == nil {
				return Result{}, fmt.Errorf("task %d 不存在（可能已结束并被清理，或 id 有误）", id)
			}
			wasRunning := sub.Kill()
			if !wasRunning {
				return Result{
					Output:  fmt.Sprintf("[task %d] 已结束，无需终止；用 task_output 查看其报告", id),
					Meta:    fmt.Sprintf("task %d 已结束", id),
					Display: fmt.Sprintf("task_kill %d", id),
				}, nil
			}
			return Result{
				Output:  fmt.Sprintf("[task %d] 已发出停止信号；它正在终止。稍后用 task_output 确认状态，然后以修正后的提示词重新派遣。", id),
				Meta:    fmt.Sprintf("task %d 已终止", id),
				Display: fmt.Sprintf("task_kill %d", id),
			}, nil
		},
	}
}

// ManagerTools builds the top-level registry: the default tools plus the
// three delegation tools bound to the session's subagent manager and the
// current task's snapshot (§16). Workers get the plain default registry.
func ManagerTools(cwd string, o ToolOpts, sm *SubagentManager, snap SubagentSnapshot) []Tool {
	tools := defaultToolsWith(cwd, o)
	tools = append(tools,
		taskTool(sm, snap),
		taskOutputTool(sm),
		taskKillTool(sm),
	)
	return tools
}

// oneLine squashes s to a single line capped at max runes.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// firstLine returns the first line of s (for the one-line Meta header).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
