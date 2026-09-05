package agent

// Compaction tests (M7.6 验收): the soft trigger and its hysteresis, the
// whole-round tool-pair-safe cut, the checkpoint projection (mission
// verbatim, checkpoint as one user message), the summarizer request shape
// (system prefix, L1-folded conversation, instruction last, no tools),
// iterative merging, the degenerate-summary fallback, and the
// overflow-compact-retry path.
//
// mockClient consumes one script slot per request — summarizer calls
// included — so every test lists its slots in true request order: model
// round, summarizer slot, post-compaction round, …

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ruyishell/internal/provider"
)

// longSummary passes the non-degenerate gate (minSummaryRunes).
var longSummary = strings.Repeat("接", 600)

// isSummaryReq reports whether a request is a compaction summarizer call
// (the checkpoint instruction rides as the closing user message).
func isSummaryReq(req []provider.ChatMessage) bool {
	if len(req) == 0 {
		return false
	}
	return strings.HasPrefix(req[len(req)-1].Content, "[系统] 以上")
}

// textRound scripts one text-only slot: a summarizer answer or a final
// answer — which is which is the test's slot layout, not the helper's.
func textRound(text string) scriptRound {
	return func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return text, nil, nil, nil
	}
}

// bigUsage points the trigger at a 1000-token window (90%).
func bigUsage() *provider.Usage { return &provider.Usage{PromptTokens: 900, TotalTokens: 950} }

// usageBashRound scripts one bash tool call with the given usage.
func usageBashRound(cmd string, usage *provider.Usage) scriptRound {
	return func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", []provider.ToolCall{{
			ID:        fmt.Sprintf("call_%d", n),
			Name:      "bash",
			Arguments: fmt.Sprintf(`{"command":%q}`, cmd),
		}}, usage, nil
	}
}

// summarySlots lists the indices of the summarizer requests a run made.
func summarySlots(reqs [][]provider.ChatMessage) []int {
	var out []int
	for i, req := range reqs {
		if isSummaryReq(req) {
			out = append(out, i)
		}
	}
	return out
}

// compactCfg is a run config with a tiny window so 900 prompt tokens are
// past the soft trigger. Note: the same window drives the §16 self-check,
// so a run this hot also collects its one-shot notes in the transcript —
// the assertions below are written to not depend on them.
func compactCfg(dir string) Config {
	cfg := fastCfg(dir)
	cfg.ContextWindow = 1000
	return cfg
}

// TestCompactTriggerProjectionAndInvariants: a run crossing 85% of the
// window compacts once; the next request view is re-projected over the
// checkpoint (Base system messages, mission verbatim, checkpoint as one
// user message, whole rounds after the cut), the transcript stays
// untouched, and the result reports the checkpoint + cut.
func TestCompactTriggerProjectionAndInvariants(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		usageBashRound("echo one", bigUsage()), // round 1 → trigger at the next boundary
		textRound(longSummary),                 // summarizer
		usageBashRound("echo two", nil),        // first post-compaction round
		textRound("done"),                      // final answer (never summarized)
	)

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "修复构建",
		Base: []TurnMsg{{Msg: provider.ChatMessage{Role: "system", Content: "cwd: /tmp/x"}}},
		Cfg:  compactCfg(t.TempDir()),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Compacted == nil {
		t.Fatalf("Compacted = nil, want checkpoint result")
	}
	if res.Compacted.Cut != 1 {
		t.Fatalf("Cut = %d, want 1 (mission summarized, tail kept)", res.Compacted.Cut)
	}

	// The summarizer request: instruction last, no tools advertised.
	sumReq := mc.requests()[1]
	if !isSummaryReq(sumReq) {
		t.Fatalf("request #2 is not a summarizer call: last = %q", sumReq[len(sumReq)-1].Content)
	}
	if sumReq[0].Content != "cwd: /tmp/x" {
		t.Fatalf("summarizer request lacks the system prefix: %q", sumReq[0].Content)
	}
	foundMission := false
	for _, m := range sumReq {
		if m.Role == "user" && m.Content == "修复构建" {
			foundMission = true
		}
	}
	if !foundMission {
		t.Fatalf("summarizer request does not carry the mission verbatim")
	}

	// The post-compaction round view: [Base system, mission, checkpoint,
	// kept round (assistant + tool)] plus the §16 self-check note the
	// stale-hot window size appends at the end.
	view := mc.requests()[2]
	if isSummaryReq(view) {
		t.Fatalf("request #3 is another summarizer call")
	}
	if len(view) < 5 {
		t.Fatalf("post-compaction view too short: %d messages", len(view))
	}
	if view[0].Role != "system" || view[0].Content != "cwd: /tmp/x" {
		t.Fatalf("view[0] = %v %q, want the Base system message", view[0].Role, view[0].Content)
	}
	if view[1].Role != "user" || view[1].Content != "修复构建" {
		t.Fatalf("view[1] = %v %q, want the mission verbatim", view[1].Role, view[1].Content)
	}
	if view[2].Role != "user" || !strings.HasPrefix(view[2].Content, "[上下文已压缩]") {
		t.Fatalf("view[2] = %v %q, want the checkpoint user message", view[2].Role, view[2].Content)
	}
	if !strings.Contains(view[2].Content, longSummary) {
		t.Fatalf("checkpoint message does not carry the summary text")
	}
	if !strings.Contains(view[2].Content, "## 当前状态") {
		t.Fatalf("checkpoint message lacks the mechanical section")
	}
	if view[3].Role != "assistant" || len(view[3].ToolCalls) != 1 {
		t.Fatalf("view[3] = %+v, want the kept round's assistant call", view[3])
	}
	if view[4].Role != "tool" {
		t.Fatalf("view[4] = %v, want the kept tool result", view[4].Role)
	}

	// Invariant ①: the run's own transcript is untouched — the full
	// exchange survives verbatim and no checkpoint message leaks into it.
	msgs := res.Messages
	if msgs[0].Msg.Role != "user" || msgs[0].Msg.Content != "修复构建" {
		t.Fatalf("Messages[0] = %v %q, want the mission", msgs[0].Msg.Role, msgs[0].Msg.Content)
	}
	if last := msgs[len(msgs)-1]; last.Msg.Role != "assistant" || last.Msg.Content != "done" {
		t.Fatalf("Messages[last] = %v %q, want the final answer", last.Msg.Role, last.Msg.Content)
	}
	for _, tm := range msgs {
		if strings.HasPrefix(tm.Msg.Content, "[上下文已压缩]") {
			t.Fatalf("checkpoint message leaked into the transcript: %q", tm.Msg.Content)
		}
	}

	// Sink: the compaction banner lines and the checkpoint hook.
	joined := strings.Join(sink.notices, "\n")
	if !strings.Contains(joined, "─── compacting ───") ||
		!strings.Contains(joined, "[已压缩上下文: 保留近") {
		t.Fatalf("notices = %v, want the compaction lines", sink.notices)
	}
	if len(sink.compacts) != 1 || !strings.Contains(sink.compacts[0], longSummary) {
		t.Fatalf("compacts = %v, want the full checkpoint once", sink.compacts)
	}
}

// TestCompactHysteresisAndIteration: while the request stays above the
// resume line no second compaction fires; falling under 50% re-arms the
// gate, and the next trigger merges the previous checkpoint instead of
// re-summarizing the absorbed prefix.
func TestCompactHysteresisAndIteration(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		usageBashRound("a", bigUsage()), // round 1 → compaction #1
		textRound(longSummary),          //   summarizer
		usageBashRound("b", bigUsage()), // round 2 → still hot, gate stays disarmed
		usageBashRound("c", &provider.Usage{PromptTokens: 400, TotalTokens: 420}), // round 3 → re-arms
		usageBashRound("d", bigUsage()),                                           // round 4 → compaction #2 (iterate)
		textRound(longSummary),                                                    //   summarizer
		textRound("done"),                                                         // final answer
	)

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "长任务",
		Cfg: compactCfg(t.TempDir()),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}

	// Exactly two summarizer calls, one right after each trigger — the
	// hot middle rounds (b, d) must not squeeze one out.
	slots := summarySlots(mc.requests())
	if len(slots) != 2 || slots[0] != 1 || slots[1] != 5 {
		t.Fatalf("summarizer request indices = %v, want [1 5]", slots)
	}

	// The second summarizer call carries the first checkpoint and the
	// iterate instruction — not the absorbed prefix again.
	iterReq := mc.requests()[slots[1]]
	if !strings.Contains(iterReq[len(iterReq)-1].Content, "再次压缩") {
		t.Fatalf("second summary instruction = %q, want the iterate variant", iterReq[len(iterReq)-1].Content)
	}
	hasPrev := false
	for _, m := range iterReq {
		if strings.Contains(m.Content, longSummary) {
			hasPrev = true
		}
	}
	if !hasPrev {
		t.Fatalf("second summarizer request lacks the previous checkpoint")
	}
	if res.Compacted == nil || res.Compacted.Cut == 0 {
		// The exact cut depends on where the tail budget lands (the
		// §16 self-check notes are turn messages too); boundary
		// legality is TestChooseCutWholeRounds' job.
		t.Fatalf("Compacted = %+v, want the second checkpoint's cut", res.Compacted)
	}
}

// TestCompactDegenerateSummary: a summary below minSummaryRunes is
// retried once; still short, the trigger gives up, disarms, and the run
// continues un-compacted on L1 folding (不变式④).
func TestCompactDegenerateSummary(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		usageBashRound("echo hi", bigUsage()), // round 1 → trigger
		textRound("太短"),                       // summarizer: degenerate
		textRound("太短"),                       //   the one retry: still degenerate
		textRound("done"),                     // final answer
	)

	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "任务",
		Cfg: compactCfg(t.TempDir()),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Compacted != nil {
		t.Fatalf("Compacted = %+v, want nil after degenerate summaries", res.Compacted)
	}
	if slots := summarySlots(mc.requests()); len(slots) != 2 {
		t.Fatalf("summarizer attempts = %d (%v), want exactly one retry", len(slots), slots)
	}
	if got := len(mc.requests()); got != 4 {
		t.Fatalf("requests = %d, want 4 — a failed compaction must not re-fire at the next boundary", got)
	}
	joined := strings.Join(sink.notices, "\n")
	if !strings.Contains(joined, "上下文压缩失败") {
		t.Fatalf("notices = %v, want the fallback notice", sink.notices)
	}
	// The final round's view keeps the plain shape (no checkpoint message).
	last := mc.requests()[len(mc.requests())-1]
	for _, m := range last {
		if strings.HasPrefix(m.Content, "[上下文已压缩]") {
			t.Fatalf("un-compacted run must not carry a checkpoint message")
		}
	}
}

// TestOverflowCompactsOnceAndTwiceFatal: a context-overflow refusal is
// answered by one compaction + retry; a second overflow stays fatal.
func TestOverflowCompactsOnceAndTwiceFatal(t *testing.T) {
	overflow := func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "", nil, nil, &provider.OverflowError{Status: "400 Bad Request", Body: "maximum context length is 8 tokens"}
	}

	// First overflow: compact, retry, finish.
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		overflow,
		textRound(longSummary),
		textRound("recovered"),
	)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "任务",
		Cfg: compactCfg(t.TempDir()),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Compacted == nil {
		t.Fatalf("Compacted = nil, want the overflow-driven compaction")
	}
	if got := len(mc.requests()); got != 3 {
		t.Fatalf("requests = %d, want overflow + summary + retry", got)
	}
	if !isSummaryReq(mc.requests()[1]) {
		t.Fatalf("request #2 is not a summarizer call")
	}
	if !strings.Contains(strings.Join(sink.notices, "\n"), "─── compacting ───") {
		t.Fatalf("notices = %v, want the compaction banner", sink.notices)
	}

	// Second overflow on the compacted view: fatal, state reported.
	mc2 := &mockClient{t: t}
	mc2.rounds = append(mc2.rounds,
		overflow,
		textRound(longSummary),
		overflow, // the compacted view is refused too → fatal
	)
	res2 := Run(context.Background(), Input{
		Client: mc2, NativeTools: true, Prompt: "任务",
		Cfg: compactCfg(t.TempDir()),
	}, sink)
	var oe *provider.OverflowError
	if !errors.As(res2.Err, &oe) {
		t.Fatalf("Err = %v, want a fatal *OverflowError", res2.Err)
	}
}

// TestCompactFileInventory: successful read/write targets accumulate
// into the checkpoint's mechanical section; failed and unrelated calls
// do not.
func TestCompactFileInventory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{
				{ID: "call_r", Name: "read", Arguments: `{"path":"a.txt"}`},
				{ID: "call_w", Name: "write", Arguments: `{"path":"b.txt","content":"x"}`},
				{ID: "call_m", Name: "read", Arguments: `{"path":"missing.txt"}`}, // fails, not tracked
				{ID: "call_e", Name: "bash", Arguments: `{"command":"echo x"}`},   // not a file tool
			}, bigUsage(), nil
		},
		textRound(longSummary), // summarizer after the trigger
		textRound("done"),      // final answer
	)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "任务",
		Cfg: compactCfg(dir),
	}, sink)
	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if len(sink.compacts) != 1 {
		t.Fatalf("compacts = %d, want 1", len(sink.compacts))
	}
	ckpt := sink.compacts[0]
	if !strings.Contains(ckpt, "已读: a.txt") {
		t.Fatalf("checkpoint lacks the read inventory: %s", ckpt)
	}
	if !strings.Contains(ckpt, "已修改: b.txt") {
		t.Fatalf("checkpoint lacks the write inventory: %s", ckpt)
	}
	if strings.Contains(ckpt, "missing.txt") {
		t.Fatalf("checkpoint tracked a failed read: %s", ckpt)
	}
}

// TestChooseCutWholeRounds: the cut never lands on a tool result (it
// would orphan the pair) and is the oldest boundary whose verbatim tail
// still fits the tail budget.
func TestChooseCutWholeRounds(t *testing.T) {
	e := &engine{cfg: Config{ContextWindow: 1000}}
	big := strings.Repeat("x", 900)
	// turn: user, asst, tool(big), asst, tool(big), asst(final)
	e.turn = []TurnMsg{
		{Msg: provider.ChatMessage{Role: "user", Content: "mission"}},
		{Msg: provider.ChatMessage{Role: "assistant", Content: "one", ToolCalls: []provider.ToolCall{{ID: "a", Name: "bash", Arguments: "{}"}}}},
		{Msg: provider.ChatMessage{Role: "tool", Content: big}, Tool: &ToolMeta{Size: len(big)}},
		{Msg: provider.ChatMessage{Role: "assistant", Content: "two"}},
		{Msg: provider.ChatMessage{Role: "tool", Content: big}, Tool: &ToolMeta{Size: len(big)}},
		{Msg: provider.ChatMessage{Role: "assistant", Content: "final"}},
	}
	cut := e.chooseCut()
	if cut <= 0 || cut >= len(e.turn) {
		t.Fatalf("cut = %d, want an interior boundary", cut)
	}
	if e.turn[cut].Msg.Role == "tool" {
		t.Fatalf("cut = %d lands on a tool result", cut)
	}
	// budget = 1000*25%*4 = 1000 bytes: the tail from cut 3 (~910 bytes)
	// fits, but the tail from cut 1 carries both 900-byte results and
	// does not — so 3 is the oldest fitting boundary.
	if cut != 3 {
		t.Fatalf("cut = %d, want 3 (tail keeps rounds two and three)", cut)
	}
}

// TestRenderStateSnapshot: todo plan, surviving jobs and worker
// subagents render as the mechanical section's live state.
func TestRenderStateSnapshot(t *testing.T) {
	jm := NewJobManager()
	jobCmd := sleepCmd(3)
	j := jm.startProcess(t.TempDir(), jobCmd)
	jm.register(j)
	defer jm.StopAll()

	sm := NewSubagentManager()
	sm.subs[3] = &Subagent{id: 3, desc: "重构 parser", state: "running", started: time.Now(), steps: 5}
	sm.order = append(sm.order, 3)

	out := RenderStateSnapshot([]Todo{
		{ID: "1", Content: "已完成项", Status: "completed"},
		{ID: "2", Content: "进行中项", Status: "in_progress"},
	}, jm, sm)

	if !strings.Contains(out, "- [x] 已完成项") || !strings.Contains(out, "- [~] 进行中项") {
		t.Fatalf("todo lines missing: %s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("job %d 运行中: %s", j.ID(), jobCmd)) {
		t.Fatalf("job line missing: %s", out)
	}
	if !strings.Contains(out, "task 3 运行中: 重构 parser") {
		t.Fatalf("subagent line missing: %s", out)
	}

	empty := RenderStateSnapshot(nil, nil, nil)
	if !strings.Contains(empty, "（无）") {
		t.Fatalf("empty snapshot = %q, want the（无）placeholder", empty)
	}
}
