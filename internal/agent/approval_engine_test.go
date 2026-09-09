package agent

// Engine-level approval tests (M7.4): the gate wired into execCall —
// safe bash runs free, gated calls prompt, y executes, n feeds the
// refusal back and the task continues, a records the session rule, auto
// never asks.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ruyishell/internal/provider"
)

// apprSink records tool results so tests can pin the fed-back refusal.
type apprSink struct {
	NopSink
	ends []Result
}

func (s *apprSink) OnToolEnd(_ provider.ToolCall, res Result) {
	s.ends = append(s.ends, res)
}

// scriptedApproval builds an ask-mode Approval answering from answers in
// order (exhausted script denies), counting every prompt.
func scriptedApproval(answers []Answer) (*Approval, *int) {
	n := 0
	ask := func(ctx context.Context, req AskRequest) Answer {
		ans := AnswerDeny
		if n < len(answers) {
			ans = answers[n]
		}
		n++
		return ans
	}
	return NewApproval("ask", ask), &n
}

func bashRound(id, cmd string) scriptRound {
	return func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", []provider.ToolCall{{ID: id, Name: "bash", Arguments: `{"command":"` + cmd + `"}`}}, nil, nil
	}
}

func finalRound(text string) scriptRound {
	return func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return text, nil, nil, nil
	}
}

// requestView joins one request's message contents for content assertions.
func requestView(msgs []provider.ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func TestRunApprovalSafeBashNoAsk(t *testing.T) {
	ap, asks := scriptedApproval(nil)
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		// echo is §7.1-safe with a shape both shells run — the gated
		// commands below are echo too, with a redirection.
		bashRound("c1", "echo listing"),
		finalRound("done"),
	}
	sink := &apprSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "list",
		Cfg: func() Config { c := fastCfg(t.TempDir()); c.Approval = ap; return c }(),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if *asks != 0 {
		t.Fatalf("ask called %d times for `echo listing`, want 0", *asks)
	}
	if len(sink.ends) != 1 || sink.ends[0].Code != 0 {
		t.Fatalf("the safe command did not run clean: %+v", sink.ends)
	}
}

func TestRunApprovalAllowExecutes(t *testing.T) {
	dir := t.TempDir()
	ap, asks := scriptedApproval([]Answer{AnswerAllow})
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		// A redirect creates the file in both shells and keeps the
		// command out of the safe shape, so the call does ask.
		bashRound("c1", "echo x > gate-y.txt"),
		finalRound("done"),
	}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "create",
		Cfg: func() Config { c := fastCfg(dir); c.Approval = ap; return c }(),
	}, &apprSink{})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if *asks != 1 {
		t.Fatalf("ask called %d times, want 1", *asks)
	}
	if _, err := os.Stat(filepath.Join(dir, "gate-y.txt")); err != nil {
		t.Fatalf("approved command did not execute: %v", err)
	}
}

func TestRunApprovalDenyFeedsBackAndContinues(t *testing.T) {
	dir := t.TempDir()
	ap, asks := scriptedApproval([]Answer{AnswerDeny})
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		bashRound("c1", "echo x > denied.txt"),
		finalRound("gave up"),
	}
	sink := &apprSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "create",
		Cfg: func() Config { c := fastCfg(dir); c.Approval = ap; return c }(),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 2 || !strings.Contains(res.Messages[len(res.Messages)-1].Msg.Content, "gave up") {
		t.Fatalf("task did not reach its final answer after the refusal: steps=%d", res.Steps)
	}
	if *asks != 1 {
		t.Fatalf("ask called %d times, want 1", *asks)
	}
	if _, err := os.Stat(filepath.Join(dir, "denied.txt")); err == nil {
		t.Fatal("denied command executed anyway")
	}
	if len(sink.ends) != 1 || sink.ends[0].Meta != "已拒绝" {
		t.Fatalf("deny result = %+v, want Meta 已拒绝", sink.ends)
	}
	if !strings.Contains(sink.ends[0].Output, "[用户拒绝了该操作]") {
		t.Fatalf("deny output = %q, want the refusal text", sink.ends[0].Output)
	}
	// The refusal reached the model in the next request (it must be able
	// to pick another route).
	if got := requestView(mc.requests()[1]); !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("refusal not fed back on request #2: %q", got)
	}
}

func TestRunApprovalAlwaysPrefixSkipsSecondAsk(t *testing.T) {
	dir := t.TempDir()
	ap, asks := scriptedApproval([]Answer{AnswerAlways})
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		bashRound("c1", "echo x > a1.txt"),
		bashRound("c2", "echo x > a2.txt"),
		finalRound("done"),
	}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "two files",
		Cfg: func() Config { c := fastCfg(dir); c.Approval = ap; return c }(),
	}, &apprSink{})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if *asks != 1 {
		t.Fatalf("ask called %d times, want 1 (a must cover the second echo)", *asks)
	}
	for _, f := range []string{"a1.txt", "a2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
}

func TestRunApprovalAutoModeNeverAsks(t *testing.T) {
	dir := t.TempDir()
	n := 0
	ap := NewApproval("always", func(ctx context.Context, req AskRequest) Answer {
		n++
		return AnswerDeny
	})
	mc := &mockClient{t: t}
	mc.rounds = []scriptRound{
		bashRound("c1", "echo x > auto.txt"),
		finalRound("done"),
	}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "create",
		Cfg: func() Config { c := fastCfg(dir); c.Approval = ap; return c }(),
	}, &apprSink{})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if n != 0 {
		t.Fatalf("ask called %d times in auto, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "auto.txt")); err != nil {
		t.Fatalf("auto-mode command did not execute: %v", err)
	}
}

func TestRunApprovalWriteAlwaysPerTool(t *testing.T) {
	dir := t.TempDir()
	ap, asks := scriptedApproval([]Answer{AnswerAlways})
	mc := &mockClient{t: t}
	writeCall := func(id, path string) scriptRound {
		return func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			b := `{"path":"` + path + `","content":"hi"}`
			return "", []provider.ToolCall{{ID: id, Name: "write", Arguments: b}}, nil, nil
		}
	}
	mc.rounds = []scriptRound{
		writeCall("c1", "w1.txt"),
		writeCall("c2", "w2.txt"),
		finalRound("done"),
	}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "two writes",
		Cfg: func() Config { c := fastCfg(dir); c.Approval = ap; return c }(),
	}, &apprSink{})
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if *asks != 1 {
		t.Fatalf("ask called %d times, want 1 (per-tool always rule)", *asks)
	}
	for _, f := range []string{"w1.txt", "w2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
}
