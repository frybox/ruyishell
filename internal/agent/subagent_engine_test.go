package agent

// Engine-level tests for the §16 manager model: delegation keeps the
// worker's transcript out of the manager's context (the report crosses,
// the intermediate output does not), the self-check injections fire at
// the right context weights, the delegation tools stay out of the
// read-churn guard's same-target counting, and the concurrency cap is
// visible to the manager through a tool error.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"ruyishell/internal/provider"
)

// TestRunSelfCheckWindow drives both self-check levels against a
// configured context window: a reported prompt size at 50% injects the
// vigilance nudge, at 75% the consolidation order. Each fires once, at a
// round boundary, and lands in the next request.
func TestRunSelfCheckWindow(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		// r1: report a prompt size at exactly half the 400-token window.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"echo a"}`}},
				&provider.Usage{PromptTokens: 200, CompletionTokens: 10, TotalTokens: 210}, nil
		},
		// r2: the level-1 note must be in the request; report 80%.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			if !strings.Contains(body, "[rysh 自我检查] 你的上下文正在变重") {
				t.Errorf("r2 request missing the level-1 self-check note")
			}
			return "", []provider.ToolCall{{ID: "c2", Name: "bash", Arguments: `{"command":"echo b"}`}},
				&provider.Usage{PromptTokens: 320, CompletionTokens: 10, TotalTokens: 330}, nil
		},
		// r3: the level-2 note must be in the request; final answer.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			if !strings.Contains(body, "[rysh 自我检查] 你的上下文接近上限") {
				t.Errorf("r3 request missing the level-2 self-check note")
			}
			if strings.Count(body, "[rysh 自我检查]") != 2 {
				t.Errorf("request carries %d self-check notes, want exactly the two levels", strings.Count(body, "[rysh 自我检查]"))
			}
			return "done", nil, nil, nil
		},
	}
	cfg := fastCfg(t.TempDir())
	cfg.ContextWindow = 400
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "干活", Cfg: cfg,
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 3 {
		t.Fatalf("Steps = %d, want 3", res.Steps)
	}
	checks := 0
	for _, note := range sink.notices {
		if strings.Contains(note, "self-check") {
			checks++
		}
	}
	if checks != 2 {
		t.Fatalf("self-check notices = %d, want 2 (%v)", checks, sink.notices)
	}
}

// TestRunSelfCheckEstimate covers the window-unset path: the fixed
// token-estimate threshold trips on a huge tool result (a registry tool
// returns 400KB; the wire estimate passes 96k tokens).
func TestRunSelfCheckEstimate(t *testing.T) {
	big := Tool{
		Name:        "big",
		Description: "test tool",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			return Result{Output: strings.Repeat("x", 400*1024), Meta: "big", Display: "big", Code: 0}, nil
		},
	}
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "b1", Name: "big"}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			if !strings.Contains(body, "[rysh 自我检查] 你的上下文正在变重") {
				t.Error("r2 request missing the estimate-based self-check note")
			}
			return "done", nil, nil, nil
		},
	}
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Tools: []Tool{big}, Prompt: "干活", Cfg: fastCfg(t.TempDir()),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 2 {
		t.Fatalf("Steps = %d, want 2", res.Steps)
	}
}

// TestManagerDelegatesIsolation is the §16 core property: the manager
// spawns a worker, polls it, and receives its report — while the
// worker's intermediate tool output never enters any manager request,
// and the manager's prompt never enters any worker request.
func TestManagerDelegatesIsolation(t *testing.T) {
	dir := t.TempDir()
	const secret = "SECRET_INTERMEDIATE_77"

	worker := &mockClient{t: t}
	worker.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "w0", Name: "bash", Arguments: fmt.Sprintf(`{"command":"echo %s"}`, secret)}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "① 已完成：运行了标记命令。② 无修改。③ 无。", nil, nil, nil
		},
	}
	sm := NewSubagentManager()
	snap := subagentSnap(worker, dir)

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			switch {
			case n == 1:
				return "", []provider.ToolCall{{ID: "m1", Name: "task",
					Arguments: `{"description":"探索","prompt":"运行标记命令并汇报"}`}}, nil, nil
			case strings.Contains(body, "已完成 ·"):
				// The finished status header (the report body uses 已完成：).
				return "验收通过", nil, nil, nil
			default:
				// Pace the polls across elapsed-second buckets so the
				// exact-repeat guard never sees identical status output
				// (a real model round takes longer than 1s anyway).
				time.Sleep(1100 * time.Millisecond)
				return "", []provider.ToolCall{{ID: fmt.Sprintf("m%d", n), Name: "task_output", Arguments: `{"id":1}`}}, nil, nil
			}
		},
	}
	tools := ManagerTools(dir, ToolOpts{}, sm, snap)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true, Tools: tools,
		Prompt: "请探索并汇报", Cfg: fastCfg(dir),
	}, sink)
	if res.Err != nil {
		t.Fatalf("manager run failed: %v", res.Err)
	}
	if got := res.Messages[len(res.Messages)-1].Msg.Content; got != "验收通过" {
		t.Fatalf("manager final = %q, want 验收通过", got)
	}

	mgrReqs := mgr.requests()
	for i, req := range mgrReqs {
		body := flattenReqs(req)
		if strings.Contains(body, secret) {
			t.Fatalf("manager request #%d carries the worker's intermediate output — isolation broken", i+1)
		}
	}
	last := flattenReqs(mgrReqs[len(mgrReqs)-1])
	if !strings.Contains(last, "① 已完成：运行了标记命令") {
		t.Fatal("manager's final request missing the worker's report")
	}
	if !strings.Contains(last, "已完成 ·") {
		t.Fatal("manager's final request missing the finished status header")
	}

	wReqs := worker.requests()
	if len(wReqs) < 2 {
		t.Fatalf("worker made %d requests, want >= 2", len(wReqs))
	}
	if body := flattenReqs(wReqs[1]); !strings.Contains(body, secret) {
		t.Fatal("worker's own request missing its tool output")
	}
	for _, req := range wReqs {
		if body := flattenReqs(req); strings.Contains(body, "请探索并汇报") {
			t.Fatal("worker request carries the manager's prompt — one-way isolation broken")
		}
	}
	sm.StopAll()
}

// TestManagerTaskPollingNotReadChurn pins the guard classification:
// polling the same finished worker repeatedly is status monitoring, not
// same-target reading — the read-churn warning must never fire, while
// the exact-repeat warning (identical command and output) still does.
func TestManagerTaskPollingNotReadChurn(t *testing.T) {
	dir := t.TempDir()
	worker := finalOnlyClient("① 完成。② 无。③ 无。")
	sm := NewSubagentManager()
	id, err := sm.Spawn("已完成的任务", "汇报", subagentSnap(worker, dir))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	<-sm.Get(id).Wait()

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			if n >= 5 {
				return "polling done", nil, nil, nil
			}
			return "", []provider.ToolCall{{ID: fmt.Sprintf("p%d", n), Name: "task_output", Arguments: fmt.Sprintf(`{"id":%d}`, id)}}, nil, nil
		},
	}
	tools := ManagerTools(dir, ToolOpts{}, sm, subagentSnap(worker, dir))
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true, Tools: tools, Prompt: "查状态", Cfg: fastCfg(dir),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != "" {
		t.Fatalf("WrapReason = %q, want none (4 polls < break threshold)", res.WrapReason)
	}
	last := flattenReqs(mgr.requests()[len(mgr.requests())-1])
	if strings.Contains(last, "同一内容已第") {
		t.Fatal("read-churn warning fired on task_output polling — wrong classification")
	}
	if !strings.Contains(last, "[rysh] 警告：这是完全相同的命令与输出的第") {
		t.Error("exact-repeat warning missing on identical polls")
	}
	sm.StopAll()
}

// TestManagerSpawnCapThroughTool: a fourth concurrent spawn fails with a
// tool error the manager can act on; the first three run. The manager
// settles the workers with task_kill before ending — with a live
// SubagentManager the run cannot end while its own workers run, so the
// cap test must use the manager's real path for exactly this.
func TestManagerSpawnCapThroughTool(t *testing.T) {
	dir := t.TempDir()
	sm := NewSubagentManager()
	snap := subagentSnap(stuckClient{}, dir)

	cfg := fastCfg(dir)
	cfg.Subs = sm

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			switch {
			case n <= 4:
				return "", []provider.ToolCall{{ID: fmt.Sprintf("s%d", n), Name: "task",
					Arguments: fmt.Sprintf(`{"description":"忙%d","prompt":"卡住"}`, n)}}, nil, nil
			case n <= 7:
				return "", []provider.ToolCall{{ID: fmt.Sprintf("k%d", n), Name: "task_kill",
					Arguments: fmt.Sprintf(`{"id":%d}`, n-4)}}, nil, nil
			default:
				return "cap confirmed", nil, nil, nil
			}
		},
	}
	tools := ManagerTools(dir, ToolOpts{}, sm, snap)
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true, Tools: tools, Prompt: "派四个", Cfg: cfg,
	}, nil)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 8 {
		t.Fatalf("Steps = %d, want 8 (4 spawns, 3 kills, final)", res.Steps)
	}
	last := flattenReqs(mgr.requests()[len(mgr.requests())-1])
	if !strings.Contains(last, "上限") {
		t.Fatal("fourth spawn did not report the concurrency cap")
	}
	// The kills settle the workers (stuckClient answers the cancellation);
	// the run itself only ended once none of them were running.
	deadline := time.Now().Add(2 * time.Second)
	for sm.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sm.Running() != 0 {
		t.Fatalf("Running() = %d, want 0", sm.Running())
	}
	sm.StopAll()
}

// releaseSink is a recSink that closes release after the `after`-th
// notice containing 等待子任务 — the end-gate tests gate a worker's final
// report on the engine's wait, so the worker is guaranteed to be running
// when the gate checks and to settle while the engine waits.
type releaseSink struct {
	recSink
	release chan struct{}
	after   int // close after this many 等待子任务 notices
	seen    int
}

func (s *releaseSink) OnNotice(msg string) {
	s.recSink.OnNotice(msg)
	if strings.Contains(msg, "等待子任务") {
		s.seen++
		if s.seen == s.after {
			close(s.release)
		}
	}
}

// gatedWorker returns a worker client that runs one bash step, then holds
// its final report until release closes — the report crosses only once
// the engine's end gate waits for it. The 30s bound keeps a failed test
// from leaking the worker forever.
func gatedWorker(t *testing.T, release <-chan struct{}) *mockClient {
	return &mockClient{t: t, rounds: []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "w0", Name: "bash", Arguments: `{"command":"echo WORK_DONE"}`}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
			return "① 已完成：干了活。② 无。③ 无。", nil, nil, nil
		},
	}}
}

// TestRunCannotEndWhileOwnSubRunning is the regression for the orphaned
// report: the manager's first text-only round while its own worker runs
// used to end the run (fresh prompt drawn), and the worker's later
// "[task N] done" landed with no manager to accept it. The engine now
// refuses to end: a nudge tells the model to keep polling, and the final
// answer only lands after the report crossed.
func TestRunCannotEndWhileOwnSubRunning(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	sink := &releaseSink{release: release, after: 1}
	worker := gatedWorker(t, release)

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			switch {
			case n == 1:
				return "", []provider.ToolCall{{ID: "m1", Name: "task",
					Arguments: `{"description":"干活","prompt":"干活并汇报"}`}}, nil, nil
			case n == 2:
				// The model's first try to end while the worker runs.
				return "等它跑，我隔几步查进展。", nil, nil, nil
			case strings.Contains(body, "已完成 ·"):
				return "验收通过", nil, nil, nil
			default:
				if n >= 3 && !strings.Contains(body, "子任务仍在后台运行") {
					t.Errorf("request #%d missing the sub-wait nudge", n)
				}
				// Pace the poll past the worker's finish so the first
				// status already carries the finished header.
				time.Sleep(1100 * time.Millisecond)
				return "", []provider.ToolCall{{ID: fmt.Sprintf("m%d", n), Name: "task_output", Arguments: `{"id":1}`}}, nil, nil
			}
		},
	}

	sm := NewSubagentManager()
	cfg := fastCfg(dir)
	cfg.Subs = sm
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true,
		Tools:  ManagerTools(dir, ToolOpts{}, sm, subagentSnap(worker, dir)),
		Prompt: "派个活并验收", Cfg: cfg,
	}, sink)
	sm.StopAll()
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != "" {
		t.Fatalf("WrapReason = %q, want none", res.WrapReason)
	}
	if res.Steps != 4 {
		t.Fatalf("Steps = %d, want 4 (spawn, nudge, poll, final)", res.Steps)
	}
	if got := res.Messages[len(res.Messages)-1].Msg.Content; got != "验收通过" {
		t.Fatalf("manager final = %q, want 验收通过", got)
	}
	last := flattenReqs(mgr.requests()[len(mgr.requests())-1])
	if !strings.Contains(last, "① 已完成：干了活") {
		t.Fatal("final request missing the worker's report")
	}
	if !strings.Contains(last, "已完成 ·") {
		t.Fatal("final request missing the finished status header")
	}
	waited := 0
	for _, note := range sink.notices {
		if strings.Contains(note, "等待子任务") {
			waited++
		}
	}
	if waited != 1 {
		t.Fatalf("等待子任务 notices = %d, want 1 (the nudge only)", waited)
	}
}

// TestRunCollectsSubReportsWhenModelKeepsEnding: the model that keeps
// trying to end gets one nudge, then the engine stops asking — it waits
// for the worker itself and feeds the report back, so the final answer
// lands after the evidence.
func TestRunCollectsSubReportsWhenModelKeepsEnding(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	// The nudge notice is the first 等待子任务 line, the collect the
	// second; the worker's report crosses only when the collect fires.
	sink := &releaseSink{release: release, after: 2}
	worker := gatedWorker(t, release)

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			if n == 1 {
				return "", []provider.ToolCall{{ID: "m1", Name: "task",
					Arguments: `{"description":"干活","prompt":"干活并汇报"}`}}, nil, nil
			}
			return "done", nil, nil, nil
		},
	}

	sm := NewSubagentManager()
	cfg := fastCfg(dir)
	cfg.Subs = sm
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true,
		Tools:  ManagerTools(dir, ToolOpts{}, sm, subagentSnap(worker, dir)),
		Prompt: "派个活", Cfg: cfg,
	}, sink)
	sm.StopAll()
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != "" {
		t.Fatalf("WrapReason = %q, want none", res.WrapReason)
	}
	if res.Steps != 4 {
		t.Fatalf("Steps = %d, want 4 (spawn, nudge, collect-wait, final)", res.Steps)
	}
	if got := res.Messages[len(res.Messages)-1].Msg.Content; got != "done" {
		t.Fatalf("manager final = %q, want done", got)
	}
	last := flattenReqs(mgr.requests()[len(mgr.requests())-1])
	if !strings.Contains(last, "以上子任务已全部结束") {
		t.Fatal("final request missing the collect message")
	}
	if !strings.Contains(last, "① 已完成：干了活") {
		t.Fatal("final request missing the worker's report")
	}
	waited := 0
	for _, note := range sink.notices {
		if strings.Contains(note, "等待子任务") {
			waited++
		}
	}
	if waited != 2 {
		t.Fatalf("等待子任务 notices = %d, want 2 (nudge + collect)", waited)
	}
}

// TestRunLoopGuardWrapsButCollectsSubsFirst: the guard's forced wrap-up
// must not orphan a running worker either — the wrap round collects the
// report first (the wait costs no tokens), and the run ends only after
// the model wraps up with the evidence.
func TestRunLoopGuardWrapsButCollectsSubsFirst(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	sink := &releaseSink{release: release, after: 1}
	worker := gatedWorker(t, release)

	sm := NewSubagentManager()
	snap := subagentSnap(worker, dir)

	// Instant tool with identical args/output: six consecutive runs trip
	// the loop guard (Consec 6) well inside the worker's release window.
	stuck := Tool{
		Name:        "stuck",
		Description: "test tool",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			return Result{Output: "STUCK", Code: 0}, nil
		},
	}

	mgr := &mockClient{t: t, repeat: true}
	mgr.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			body := flattenReqs(req)
			switch {
			case n == 1:
				return "", []provider.ToolCall{{ID: "m1", Name: "task",
					Arguments: `{"description":"干活","prompt":"干活并汇报"}`}}, nil, nil
			case strings.Contains(body, "执行预算已到极限"):
				// The wrap-up instruction is in: text-only.
				return "wrapping up now", nil, nil, nil
			default:
				return "", []provider.ToolCall{{ID: fmt.Sprintf("m%d", n), Name: "stuck"}}, nil, nil
			}
		},
	}
	tools := append(ManagerTools(dir, ToolOpts{}, sm, snap), stuck)
	cfg := fastCfg(dir)
	cfg.Subs = sm
	res := Run(context.Background(), Input{
		Client: mgr, NativeTools: true, Tools: tools,
		Prompt: "干活", Cfg: cfg,
	}, sink)
	sm.StopAll()
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != wrapReasonLoop {
		t.Fatalf("WrapReason = %q, want %q", res.WrapReason, wrapReasonLoop)
	}
	if res.Steps != 9 {
		t.Fatalf("Steps = %d, want 9 (spawn, 6 loops, wrap, collect-wait, final)", res.Steps)
	}
	if got := res.Messages[len(res.Messages)-1].Msg.Content; got != "wrapping up now" {
		t.Fatalf("manager final = %q, want wrapping up now", got)
	}
	last := flattenReqs(mgr.requests()[len(mgr.requests())-1])
	if !strings.Contains(last, "以上子任务已全部结束") {
		t.Fatal("final request missing the collect message")
	}
	if !strings.Contains(last, "① 已完成：干了活") {
		t.Fatal("final request missing the worker's report")
	}
	notices := strings.Join(sink.notices, "\n")
	if !strings.Contains(notices, "wrapping up (loop guard)") {
		t.Fatalf("wrap notice missing, got %q", notices)
	}
	if !strings.Contains(notices, "等待子任务") {
		t.Fatalf("sub-wait notice missing, got %q", notices)
	}
}
