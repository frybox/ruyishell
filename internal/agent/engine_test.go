package agent

// Engine tests (M7.2 验收 §12): a scripted StreamClient drives the full
// agent loop — long tasks, weak-argument resilience, the loop guards,
// fence synthesis, tools degradation, retries and usage accounting.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ruyishell/internal/provider"
)

// scriptRound is one scripted model turn: the mock derives its response
// from the request view and the advertised options, so tests can condition
// on tools presence (degradation) or wire shape.
type scriptRound func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (text string, calls []provider.ToolCall, usage *provider.Usage, err error)

// mockClient plays the scripted rounds in order; extra rounds after the
// script fail the test via the trailing panic-free error path, unless
// repeat is set, in which case the last round is replayed (dynamic
// scripts that branch on the request, e.g. the manager's task_output
// polling loop).
type mockClient struct {
	t      *testing.T
	dead   bool
	repeat bool
	mu     struct {
		n    int
		reqs [][]provider.ChatMessage
	}
	rounds []scriptRound
}

func (m *mockClient) ChatStream(ctx context.Context, msgs []provider.ChatMessage, opts ...provider.ChatOption) (<-chan provider.StreamToken, error) {
	o := &provider.ChatOptions{}
	for _, op := range opts {
		op(o)
	}
	m.mu.n++
	n := m.mu.n
	m.mu.reqs = append(m.mu.reqs, msgs)
	idx := n
	if n > len(m.rounds) {
		if !m.repeat {
			m.t.Fatalf("unexpected request #%d (script has %d rounds)", n, len(m.rounds))
		}
		// repeat: replay the last round, but with the TRUE request
		// number — dynamic scripts branch on it.
		idx = len(m.rounds)
	}
	text, calls, usage, err := m.rounds[idx-1](n, msgs, o)
	ch := make(chan provider.StreamToken, 1+len(calls)+1)
	if err != nil {
		close(ch)
		return ch, err
	}
	if text != "" {
		ch <- provider.StreamToken{Text: text}
	}
	for i := range calls {
		ch <- provider.StreamToken{ToolCall: &calls[i]}
	}
	if usage != nil {
		ch <- provider.StreamToken{Usage: usage}
	}
	close(ch)
	return ch, nil
}

func (m *mockClient) requests() [][]provider.ChatMessage { return m.mu.reqs }

// recSink records everything the run reports.
type recSink struct {
	NopSink
	text     strings.Builder
	notices  []string
	status   []string
	steps    []int
	ends     []provider.ToolCall
	compacts []string
}

func (s *recSink) OnText(delta string, reasoning bool) { s.text.WriteString(delta) }
func (s *recSink) OnNotice(msg string)                 { s.notices = append(s.notices, msg) }
func (s *recSink) OnStatus(msg string)                 { s.status = append(s.status, msg) }
func (s *recSink) OnStep(n int)                        { s.steps = append(s.steps, n) }
func (s *recSink) OnToolEnd(call provider.ToolCall, res Result) {
	s.ends = append(s.ends, call)
}
func (s *recSink) OnCompact(text string) { s.compacts = append(s.compacts, text) }

func fastCfg(dir string) Config {
	return Config{Cwd: dir, RetryBase: time.Millisecond, RetryCap: 10 * time.Millisecond}
}

func toolMsgs(msgs []TurnMsg) (n int) {
	for _, tm := range msgs {
		if tm.Msg.Role == "tool" {
			n++
		}
	}
	return n
}

// A 12-round task: one bash call per round, then the final answer. Covers
// the §12 验收 "10+ steps end to end" plus usage accumulation.
func TestRunLongTaskEndToEnd(t *testing.T) {
	mc := &mockClient{t: t}
	for i := 1; i <= 11; i++ {
		step := i
		mc.rounds = append(mc.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{
				ID: fmt.Sprintf("call_%d", step), Name: "bash",
				Arguments: fmt.Sprintf(`{"command":"echo step %d"}`, step),
			}}, &provider.Usage{TotalTokens: step}, nil
		})
	}
	mc.rounds = append(mc.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "all done", nil, nil, nil
	})

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "do everything",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 12 {
		t.Fatalf("Steps = %d, want 12", res.Steps)
	}
	if got := toolMsgs(res.Messages); got != 11 {
		t.Fatalf("tool messages = %d, want 11", got)
	}
	if res.Messages[len(res.Messages)-1].Msg.Content != "all done" {
		t.Fatalf("final message = %q", res.Messages[len(res.Messages)-1].Msg.Content)
	}
	if res.WrapReason != "" {
		t.Fatalf("WrapReason = %q, want none", res.WrapReason)
	}
	// Rounds 1-11 report usage chunks (sum 66); the final round has no
	// usage chunk and falls back to the chars/4 wire estimate, so the
	// total must exceed the reported sum but stay in a sane range.
	if res.UsageTotal < 78 || res.UsageTotal > 400 {
		t.Fatalf("UsageTotal = %d, want 66 reported + a small estimate", res.UsageTotal)
	}
	if want := "step 11"; !strings.Contains(sink.text.String(), want) && sink.text.Len() != 0 {
		// text sink only carries model text; tool output is not streamed
		_ = want
	}
	if len(sink.steps) != 12 {
		t.Fatalf("OnStep events = %d, want 12", len(sink.steps))
	}
}

// Weak models misfire: unknown tools, broken arguments and duplicated
// calls must be fed back as ordinary results, never kill the task (§5.1).
func TestRunResilienceWeakCalls(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{
				{ID: "c1", Name: "nosuchtool", Arguments: `{}`},
				{ID: "c2", Name: "bash", Arguments: `{broken json`},
				{ID: "c3", Name: "bash", Arguments: `{"command":"echo FIRST"}`},
				{ID: "c4", Name: "bash", Arguments: `{"command":"echo FIRST"}`}, // duplicate
			}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "recovered", nil, nil, nil
		},
	)

	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "go",
		Cfg: fastCfg(t.TempDir()),
	}, nil)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 2 {
		t.Fatalf("Steps = %d, want 2", res.Steps)
	}
	if got := toolMsgs(res.Messages); got != 4 {
		t.Fatalf("tool messages = %d, want 4 (every call answered)", got)
	}
	var fed []string
	for _, tm := range res.Messages {
		if tm.Msg.Role == "tool" {
			fed = append(fed, tm.Msg.Content)
		}
	}
	if !strings.Contains(fed[0], "未知工具") {
		t.Fatalf("unknown tool must be reported on the fed result, got %q", fed[0])
	}
	if !strings.Contains(fed[1], "错误: ") {
		t.Fatalf("broken arguments must be reported on the fed result, got %q", fed[1])
	}
	if !strings.Contains(fed[2], "FIRST") || !strings.Contains(fed[3], "FIRST") {
		t.Fatalf("duplicated calls must both execute, got %q / %q", fed[2], fed[3])
	}
	if !strings.Contains(fed[3], "第 2 次重复") {
		t.Fatalf("the repeated result must carry the loop warning, got %q", fed[3])
	}
}

// Six identical runs trip loopBreakConsec: a wrap-up round is injected,
// a disobedient model gets its calls answered with notRunWrap, the task ends.
func TestRunLoopGuardStopsAtSix(t *testing.T) {
	stuck := func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", []provider.ToolCall{{ID: "c", Name: "bash", Arguments: `{"command":"echo STUCK"}`}}, nil, nil
	}
	mc := &mockClient{t: t, rounds: []scriptRound{stuck, stuck, stuck, stuck, stuck, stuck, stuck}}

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "loop",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != wrapReasonLoop {
		t.Fatalf("WrapReason = %q, want %q", res.WrapReason, wrapReasonLoop)
	}
	if res.Steps != 7 {
		t.Fatalf("Steps = %d, want 7 (6 stuck rounds + wrap-up)", res.Steps)
	}
	if got := toolMsgs(res.Messages); got != 7 {
		t.Fatalf("tool messages = %d, want 7 (6 executed + wrap answer)", got)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Msg.Content != notRunWrap {
		t.Fatalf("wrap-up round must answer the disobedient call with %q, got %q", notRunWrap, last.Msg.Content)
	}
	joined := strings.Join(sink.notices, "\n")
	if !strings.Contains(joined, "wrapping up (loop guard)") {
		t.Fatalf("wrap notice missing, got %q", joined)
	}
	// Warnings ride on executed results from the second consecutive run
	// on; the seventh is the wrap round's notRun marker, exempt.
	fed := 0
	for _, tm := range res.Messages {
		if tm.Msg.Role == "tool" {
			fed++
			if fed >= 2 && fed <= 6 && !strings.Contains(tm.Msg.Content, "重复") {
				t.Fatalf("fed result #%d must carry the repetition warning", fed)
			}
		}
	}
}

// Same read target over and over with fresh output each time: the
// read-churn guard warns on the 3rd, regroups on the 4th, wraps on the 5th.
func TestRunReadChurnRegroupThenWrap(t *testing.T) {
	var seq int
	probe := Tool{
		Name:        "probe",
		Description: "read a changing probe",
		ReadOnly:    true,
		Schema:      map[string]any{"type": "object"},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			seq++
			return Result{Meta: "ok", Output: fmt.Sprintf("sample %d", seq), Display: "probe"}, nil
		},
	}
	call := func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", []provider.ToolCall{{ID: "c", Name: "probe", Arguments: `{"target":"x"}`}}, nil, nil
	}
	mc := &mockClient{t: t}
	for i := 0; i < 5; i++ {
		mc.rounds = append(mc.rounds, call)
	}
	mc.rounds = append(mc.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		// The wrap-up round: the request view must contain the regroup
		// reminder and the wrap instruction.
		found := false
		for _, m := range req {
			if m.Role == "user" && strings.Contains(m.Content, "已掌握") {
				found = true
			}
		}
		if !found {
			t.Fatalf("wrap-up request lacks the regroup reminder: %+v", req)
		}
		return "wrapping up now", nil, nil, nil
	})

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Tools: []Tool{probe}, Prompt: "probe",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != wrapReasonReadChurn {
		t.Fatalf("WrapReason = %q, want %q", res.WrapReason, wrapReasonReadChurn)
	}
	if res.Steps != 6 {
		t.Fatalf("Steps = %d, want 6 (5 churn rounds + wrap-up)", res.Steps)
	}
	if !strings.Contains(strings.Join(sink.notices, "\n"), "wrapping up (read churn)") {
		t.Fatalf("wrap notice missing, got %q", sink.notices)
	}
	fed := 0
	for _, tm := range res.Messages {
		if tm.Msg.Role != "tool" {
			continue
		}
		fed++
		if fed == 3 && !strings.Contains(tm.Msg.Content, "同一内容已第 3 次读取") {
			t.Fatalf("third read must carry the churn warning, got %q", tm.Msg.Content)
		}
	}
}

// A healthy mix — distinct reads with a write every few steps — must run
// to completion without any guard tripping (no false kills on long tasks).
func TestRunHealthyExplorationNotKilled(t *testing.T) {
	mc := &mockClient{t: t}
	// 8 distinct probes, then a write, then the answer.
	for i := 1; i <= 8; i++ {
		step := i
		mc.rounds = append(mc.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{
				ID: fmt.Sprintf("c%d", step), Name: "bash",
				Arguments: fmt.Sprintf(`{"command":"echo unique probe %d"}`, step),
			}}, nil, nil
		})
	}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "w", Name: "bash", Arguments: `{"command":"echo done > out.txt"}`}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "explored enough", nil, nil, nil
		},
	)

	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "explore",
		Cfg: fastCfg(t.TempDir()),
	}, nil)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.WrapReason != "" {
		t.Fatalf("healthy task wrapped up: %q", res.WrapReason)
	}
	if res.Steps != 10 {
		t.Fatalf("Steps = %d, want 10", res.Steps)
	}
}

// A code block in the reply is display text, never executed: the former
// fence protocol is gone, so the run ends after the (call-free) reply.
func TestRunFenceNotExecuted(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "to run this yourself:\n```bash\necho NEVER_RUN\n```", nil, nil, nil
		},
	)

	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "show me a command",
		Cfg: fastCfg(t.TempDir()),
	}, nil)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	for _, tm := range res.Messages {
		if tm.Msg.Role == "tool" {
			t.Fatalf("fence block in the reply was executed: %+v", tm.Msg)
		}
	}
	if got := toolMsgs(res.Messages); got != 0 {
		t.Fatalf("tool messages = %d, want 0", got)
	}
}

// An endpoint that rejects the tools parameter is fatal now: the fence
// fallback is gone, so the run ends with an error instead of degrading.
func TestRunToolsRejectedIsFatal(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			if len(o.Tools) == 0 {
				t.Fatal("the request must advertise the registry")
			}
			return "", nil, nil, &provider.ToolsUnsupportedError{Status: "400 Bad Request", Body: "tools are not supported"}
		},
	)

	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "reject my tools",
		Cfg: fastCfg(t.TempDir()),
	}, nil)

	if res.Err == nil {
		t.Fatal("a tools rejection must fail the run")
	}
	if got := toolMsgs(res.Messages); got != 0 {
		t.Fatalf("tool messages = %d, want 0", got)
	}
}

// Transport-style failures back off and retry; a broken stream after the
// first token continues with the partial reply.
func TestRunRetryAndPartialStream(t *testing.T) {
	t.Run("retries request failure", func(t *testing.T) {
		mc := &mockClient{t: t}
		mc.rounds = append(mc.rounds,
			func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
				return "", nil, nil, &provider.StatusError{Code: 503, Status: "503 Service Unavailable", Body: "later"}
			},
			func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
				return "recovered", nil, nil, nil
			},
		)
		sink := &recSink{}
		res := Run(context.Background(), Input{Client: mc, Prompt: "hi", Cfg: fastCfg(t.TempDir())}, sink)
		if res.Err != nil {
			t.Fatalf("run failed: %v", res.Err)
		}
		if len(mc.requests()) != 2 {
			t.Fatalf("requests = %d, want 2 (initial + retry)", len(mc.requests()))
		}
		if len(sink.status) == 0 || !strings.Contains(sink.status[0], "重试") {
			t.Fatalf("retry status missing, got %q", sink.status)
		}
	})

	t.Run("retries silent stream break", func(t *testing.T) {
		inner := &mockClient{t: t}
		inner.rounds = append(inner.rounds,
			func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
				return "after break", nil, nil, nil
			},
		)
		raw := &breakClient{t: t, breakOn: 1, inner: inner}
		res := Run(context.Background(), Input{Client: raw, Prompt: "hi", Cfg: fastCfg(t.TempDir())}, nil)
		if res.Err != nil {
			t.Fatalf("run failed: %v", res.Err)
		}
		if len(raw.calls) != 2 {
			t.Fatalf("ChatStream calls = %d, want 2 (silent break retried)", len(raw.calls))
		}
		if res.Messages[len(res.Messages)-1].Msg.Content != "after break" {
			t.Fatalf("final = %q", res.Messages[len(res.Messages)-1].Msg.Content)
		}
	})

	t.Run("keeps partial reply after mid-stream break", func(t *testing.T) {
		inner := &mockClient{t: t}
		raw := &breakClient{t: t, breakOn: 1, partial: "partial ans"}
		sink := &recSink{}
		res := Run(context.Background(), Input{Client: raw, Prompt: "hi", Cfg: fastCfg(t.TempDir())}, sink)
		if res.Err != nil {
			t.Fatalf("run failed: %v", res.Err)
		}
		if len(raw.calls) != 1 || len(inner.requests()) != 0 {
			t.Fatalf("requests = %d/%d, want 1/0 (no retry once tokens were emitted)",
				len(raw.calls), len(inner.requests()))
		}
		if got := res.Messages[len(res.Messages)-1].Msg.Content; got != "partial ans" {
			t.Fatalf("partial reply must become the assistant message, got %q", got)
		}
		if len(sink.notices) == 0 || !strings.Contains(sink.notices[0], "流式响应中断") {
			t.Fatalf("break notice missing, got %q", sink.notices)
		}
	})

	t.Run("gives up after the retry budget", func(t *testing.T) {
		raw := &breakClient{t: t, breakOn: 0} // break every request
		res := Run(context.Background(), Input{Client: raw, Prompt: "hi", Cfg: fastCfg(t.TempDir())}, nil)
		if res.Err == nil {
			t.Fatal("a persistently breaking stream must fail the run")
		}
		// 1 initial + RetryAttempts retries, then the round fails.
		if want := 1 + 3; len(raw.calls) != want {
			t.Fatalf("ChatStream calls = %d, want %d (retry budget exhausted)", len(raw.calls), want)
		}
	})
}

// breakClient breaks one (or every) stream: breakOn selects the 1-based
// call number to fail, 0 breaks all. A broken call emits only an error
// token, optionally after one partial text delta.
type breakClient struct {
	t       *testing.T
	breakOn int
	partial string
	inner   StreamClient
	calls   [][]provider.ChatMessage
}

func (b *breakClient) ChatStream(ctx context.Context, msgs []provider.ChatMessage, opts ...provider.ChatOption) (<-chan provider.StreamToken, error) {
	b.calls = append(b.calls, msgs)
	if b.breakOn != 0 && len(b.calls) != b.breakOn {
		return b.inner.ChatStream(ctx, msgs, opts...)
	}
	ch := make(chan provider.StreamToken, 2)
	if b.partial != "" {
		ch <- provider.StreamToken{Text: b.partial}
	}
	ch <- provider.StreamToken{Err: errors.New("connection reset by peer")}
	close(ch)
	return ch, nil
}

// A fatal non-retryable request error ends the run with Err set and the
// partial exchange preserved for history.
func TestRunFatalRequestError(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", nil, nil, &provider.StatusError{Code: 400, Status: "400 Bad Request", Body: "bad model"}
	})
	res := Run(context.Background(), Input{Client: mc, Prompt: "doomed", Cfg: fastCfg(t.TempDir())}, nil)
	if res.Err == nil {
		t.Fatal("run must fail with the request error")
	}
	if len(res.Messages) == 0 || res.Messages[0].Msg.Content != "doomed" {
		t.Fatal("the user prompt must stay in Messages on failure")
	}
}

// ^C during a tool run: the running call's result lands, the run stops
// before another request, and no error is surfaced (a deliberate stop).
func TestRunCancelDuringTool(t *testing.T) {
	runCtx, cancel := context.WithCancel(context.Background())
	probe := Tool{
		Name:   "canceller",
		Schema: map[string]any{"type": "object"},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			cancel() // simulates ^C landing mid-execution
			return Result{}, runCtx.Err()
		},
	}
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "c1", Name: "canceller", Arguments: `{}`}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			t.Fatal("no request may follow a cancelled tool run")
			return "", nil, nil, nil
		},
	)
	res := Run(runCtx, Input{
		Client: mc, NativeTools: true, Tools: []Tool{probe}, Prompt: "stop mid-way",
		Cfg: fastCfg(t.TempDir()),
	}, nil)
	if res.Err != nil {
		t.Fatalf("a deliberate stop must not surface as Err, got %v", res.Err)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Msg.Role != "tool" || !strings.Contains(last.Msg.Content, "context canceled") {
		t.Fatalf("the failed tool result must land before the stop, got %+v", last.Msg)
	}
}

// §8.3 两级 ^C: firing the interrupt handle kills the in-flight tool while
// the task survives — the interrupted result is fed back, the round's
// remaining calls are answered without running, and the next request goes
// out so the model picks the next step.
func TestRunToolInterruptContinues(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	slow := Tool{Name: "slow", Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return Result{Meta: "中断", Code: -1, Output: "[被用户中断] slow 的输出"}, nil
	}}
	after := Tool{Name: "after", Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
		t.Error("the call after an interrupt must not run")
		return Result{}, nil
	}}
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{
				{ID: "c1", Name: "slow", Arguments: `{}`},
				{ID: "c2", Name: "after", Arguments: `{}`},
			}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "interrupted but alive", nil, nil, nil
		},
	)

	intr := &ToolInterrupt{}
	type outcome struct{ res RunResult }
	out := make(chan outcome, 1)
	go func() {
		res := Run(context.Background(), Input{
			Client: mc, NativeTools: true, Prompt: "go",
			Tools:     []Tool{slow, after},
			Interrupt: intr,
			Cfg:       fastCfg(t.TempDir()),
		}, &recSink{})
		out <- outcome{res}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow tool never started")
	}
	time.Sleep(20 * time.Millisecond) // let the tool block on its ctx
	if !intr.Fire() {
		t.Fatal("Fire must report a tool in flight")
	}
	var res RunResult
	select {
	case o := <-out:
		res = o.res
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish after the interrupt")
	}
	if res.Err != nil || res.Steps != 2 {
		t.Fatalf("task must continue after a tool interrupt: err=%v steps=%d", res.Err, res.Steps)
	}
	last := res.Messages[len(res.Messages)-1]
	if !strings.Contains(last.Msg.Content, "interrupted but alive") {
		t.Fatalf("the run must reach the next round, got %+v", last.Msg)
	}
	var fed []provider.ChatMessage
	for _, tm := range res.Messages {
		if tm.Msg.Role == "tool" {
			fed = append(fed, tm.Msg)
		}
	}
	if len(fed) != 2 {
		t.Fatalf("both calls must be answered, got %d tool results", len(fed))
	}
	if fed[0].ToolCallID != "c1" || !strings.Contains(fed[0].Content, "[被用户中断] slow") {
		t.Fatalf("interrupted result must be fed back, got %+v", fed[0])
	}
	if fed[1].ToolCallID != "c2" || fed[1].Content != notRunInterrupt {
		t.Fatalf("the skipped call must carry the not-run mark, got %+v", fed[1])
	}
	if intr.Fire() {
		t.Fatal("the slot must be empty once the tool ended")
	}
}

// M8.7: RunResult must report the token splits (from reported usage
// chunks only) and the executed tool-call count, so the driver can draw
// the task-stats footer.
func TestRunResultStats(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"echo a"}`}},
				&provider.Usage{PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105, CachedTokens: 60}, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "c2", Name: "bash", Arguments: `{"command":"echo b"}`}},
				&provider.Usage{PromptTokens: 200, CompletionTokens: 10, TotalTokens: 210, CachedTokens: 150}, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "done", nil, &provider.Usage{PromptTokens: 300, CompletionTokens: 1, TotalTokens: 301, CachedTokens: 250}, nil
		},
	)
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "two steps",
		Cfg: fastCfg(t.TempDir()),
	}, &recSink{})
	if res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}
	if res.Steps != 3 {
		t.Fatalf("Steps = %d, want 3", res.Steps)
	}
	if res.ToolCalls != 2 {
		t.Fatalf("ToolCalls = %d, want 2", res.ToolCalls)
	}
	if res.PromptTokens != 600 || res.CompletionTokens != 16 {
		t.Fatalf("splits = %d/%d, want 600/16", res.PromptTokens, res.CompletionTokens)
	}
	if res.CachedTokens != 460 {
		t.Fatalf("CachedTokens = %d, want 460", res.CachedTokens)
	}
	if res.UsageTotal != 616 {
		t.Fatalf("UsageTotal = %d, want 616", res.UsageTotal)
	}
}
