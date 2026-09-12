package agent

// Engine (architecture §6): the agent loop that replaced main.go's inline
// streamRound/guard loop. One Run = one user prompt driven to completion:
// stream a reply, execute its native tool calls, feed the results back as
// role:"tool" messages, and repeat until the model answers without calls.
// Code blocks in a reply are display text — only explicit function calls
// are ever executed. Stop conditions: clean final answer, the loop guards
// (§6.4), ^C, or a fatal request error.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"ruyishell/internal/provider"
)

// StreamClient is the engine's request seam: provider.OpenAIClient
// satisfies it, tests substitute a scripted mock.
type StreamClient interface {
	ChatStream(ctx context.Context, messages []provider.ChatMessage, opts ...provider.ChatOption) (<-chan provider.StreamToken, error)
}

// Sink receives everything a run wants to show or log. The TUI
// implementation (cmd/rysh) renders markdown streams, tool lines and
// banners; tests record events. Methods are called from the engine
// goroutine only.
type Sink interface {
	// OnText reports one content or reasoning delta.
	OnText(delta string, reasoning bool)
	// OnToolBegin/OnToolEnd bracket one tool execution.
	OnToolBegin(call provider.ToolCall)
	OnToolEnd(call provider.ToolCall, res Result)
	// OnTodo reports the todo list after the todo tool replaced it.
	OnTodo(todos []Todo)
	// OnStep reports the round number as a new request round starts.
	OnStep(n int)
	// OnNotice reports an out-of-band event: the wrap-up banner, the
	// tools-fallback switch, a broken stream. The text is display-ready.
	OnNotice(text string)
	// OnStatus reports transient request state (retry countdowns).
	OnStatus(text string)
	// OnCompact reports a finished context compaction (M7.6) with the
	// checkpoint full text. The screen sees the engine's notice lines;
	// this hook exists for the session log.
	OnCompact(text string)
	// OnAssistant reports one finalized assistant turn: its text plus the
	// native tool calls it requested, in order. The session log records it
	// atomically so a rebuilt history pairs each call request with its
	// result (a call and its answer are inseparable).
	OnAssistant(text string, calls []provider.ToolCall)
}

// NopSink discards everything; useful as an embed base or for headless
// runs.
type NopSink struct{}

func (NopSink) OnText(string, bool)                 {}
func (NopSink) OnToolBegin(provider.ToolCall)       {}
func (NopSink) OnToolEnd(provider.ToolCall, Result) {}
func (NopSink) OnTodo([]Todo)                       {}
func (NopSink) OnStep(int)                          {}
func (NopSink) OnNotice(string)                     {}
func (NopSink) OnStatus(string)                     {}
func (NopSink) OnCompact(string)                    {}
func (NopSink) OnAssistant(string, []provider.ToolCall) {}

// Config carries the run's knobs. Zero durations fall back to the §6
// defaults; Now is overridable so guard tests do not sleep. Jobs carries
// the session-wide background-job manager (§8.2); nil means the job tools
// stay out of the registry. AutoBackground is the §8.1 foreground window;
// 0 disables backgrounding. Approval is the §7 permission gate; nil means
// no gate at all (pre-M7.4 behavior, unit tests) — the driver always
// supplies one, defaulting to ask mode. ContextWindow (tokens) enables the
// §16 self-check and the M7.6 compaction trigger against real
// percentages; 0 falls back to the fixed token-estimate thresholds. Subs
// carries the session-wide subagent manager (§16); nil means the
// checkpoint's state snapshot omits worker subagents.
type Config struct {
	Cwd            string
	BashTimeout    time.Duration
	BashMaxTimeout time.Duration
	AutoBackground time.Duration
	Jobs           *JobManager
	Subs           *SubagentManager
	Approval       *Approval
	RetryBase      time.Duration // default 2s
	RetryAttempts  int           // default 3
	RetryCap       time.Duration // default 30s
	ContextWindow  int
	Now            func() time.Time
}

// Input is one Run's request. Tools nil means the default registry; Base
// is the fixed prefix (system messages and history) before the in-flight
// turn, carried as TurnMsgs so L1 slimming can fold old tool results
// across the whole request view. Interrupt is the driver's two-phase ^C
// handle (§8.3).
type Input struct {
	Client      StreamClient
	NativeTools bool      // advertise the registry via function calling
	Tools       []Tool    // nil → default registry for Cfg.Cwd
	Base        []TurnMsg // system prefix + prior turns
	Prompt      string
	Cfg         Config
	Interrupt   *ToolInterrupt
}

// ToolInterrupt is the driver's handle on the in-flight tool execution
// (§8.3 两级 ^C): firing it kills the current foreground command while
// the run continues, so the model sees "[被用户中断]" and picks the next
// step. The engine sets the slot before each tool and clears it after;
// Fire on an empty slot reports false so the driver can fall back to
// cancelling the whole task.
type ToolInterrupt struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (ti *ToolInterrupt) Set(cancel context.CancelFunc) {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.cancel = cancel
}

// Fire cancels the in-flight tool and reports whether one was running.
func (ti *ToolInterrupt) Fire() bool {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	if ti.cancel == nil {
		return false
	}
	ti.cancel()
	ti.cancel = nil
	ti.fired = true
	return true
}

// takeFired reports — and clears — whether Fire ran since the last call;
// the engine uses it to skip the rest of a round's calls.
func (ti *ToolInterrupt) takeFired() bool {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	f := ti.fired
	ti.fired = false
	return f
}

// Reset clears the slot and the fired flag (run start).
func (ti *ToolInterrupt) Reset() {
	ti.mu.Lock()
	defer ti.mu.Unlock()
	ti.cancel = nil
	ti.fired = false
}

// RunResult is a finished run: everything said and executed, why it
// stopped, and the token accounting. Messages is kept verbatim even on
// error so the caller can keep the partial exchange in history. Compacted
// is non-nil when the run compacted its context: the driver then shrinks
// its in-memory history over the same checkpoint (M7.6).
type RunResult struct {
	Messages   []TurnMsg
	Steps      int            // streamed request rounds
	WrapReason string         // non-empty when the guards forced the wrap-up round
	Usage      provider.Usage // last reported usage chunk, if any
	UsageTotal int            // cumulative tokens (reported or estimated)
	// Token splits accumulated from the reported usage chunks only (0 when
	// the endpoint never reported usage — the driver must treat those as
	// unknown, not zero); CachedTokens is the prompt-cache hit count.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	ToolCalls        int            // tool invocations executed by this run (top-level)
	Compacted        *CompactResult // the last compaction's checkpoint + cut point
	Err              error
}

// notRun marks tool results synthesized for calls that were never
// executed, keeping the wire legal (every tool_call_id answered).
const (
	notRunCancel    = "[中断: 未执行]"
	notRunWrap      = "[收尾：不再执行工具]"
	notRunInterrupt = "[被用户中断: 未执行]"
)

// Self-check thresholds (§16 自我警觉): the run is the manager of its own
// context and must notice abnormal growth — "任何一部分变得异常多和异常
// 大时，都要看看哪里出问题了". Two escalating one-shot injections ride on
// the request as user messages: a vigilance nudge at half the window and
// a consolidation order at three quarters. Without a configured window
// the fixed token-estimate thresholds apply, and a raw message count
// covers the "异常多" half on either path.
const (
	selfCheckMsgs  = 150
	selfCheckL1Pct = 50
	selfCheckL2Pct = 75
	selfCheckL1Est = 96000 // estimated tokens, window unset
	selfCheckL2Est = 144000
)

// selfCheckNote1 is the vigilance nudge (level 1): find what grew
// abnormally before continuing.
const selfCheckNote1 = "[rysh 自我检查] 你的上下文正在变重（%s）。继续之前先检查：上下文是否还围绕使命（第一条用户消息）？任何一部分变得异常多、异常大——陈旧细节、重复查询、超大输出——先定位来源，再丢弃或压缩。todo 清单与子任务报告是权威的进度记录，已读过的内容不要重读。"

// selfCheckNote2 is the consolidation order (level 2): the context is
// near the wall; condense progress into the todo list now.
const selfCheckNote2 = "[rysh 自我检查] 你的上下文接近上限（%s）。立即整理：重写 todo 清单（已完成/进行中/未开始）把进度压缩进去，丢弃陈旧细节，已做的事情以子任务报告和 todo 为准。整理后继续推进，不要重读已读过的内容。"

// FedContent renders the role:"tool" message body for a result: a
// one-line `[name] meta` header over the output. The sink logs the same
// text, so guard warnings — appended to the message after OnToolEnd —
// never reach screen or session log.
func FedContent(name string, res Result) string {
	head := "[" + name + "]"
	if res.Meta != "" {
		head += " " + res.Meta
	}
	if res.Output == "" {
		return head + "\n(无输出)"
	}
	return head + "\n" + res.Output
}

// Run drives one prompt to completion. It never panics on a nil sink and
// returns partial results on every exit path.
func Run(ctx context.Context, in Input, sink Sink) RunResult {
	if sink == nil {
		sink = NopSink{}
	}
	e := &engine{in: in, sink: sink, native: in.NativeTools}
	e.cfg = in.Cfg
	if e.cfg.RetryBase <= 0 {
		e.cfg.RetryBase = 2 * time.Second
	}
	if e.cfg.RetryAttempts <= 0 {
		e.cfg.RetryAttempts = 3
	}
	if e.cfg.RetryCap <= 0 {
		e.cfg.RetryCap = 30 * time.Second
	}
	if in.Cfg.Now != nil {
		e.now = in.Cfg.Now
	} else {
		e.now = time.Now
	}

	e.tools = in.Tools
	if e.tools == nil {
		bt, bm := defaultBashTimeout, maxBashTimeout
		if e.cfg.BashTimeout > 0 {
			bt = e.cfg.BashTimeout
		}
		if e.cfg.BashMaxTimeout > 0 {
			bm = e.cfg.BashMaxTimeout
		}
		e.tools = defaultToolsWith(in.Cfg.Cwd, ToolOpts{
			BashTimeout:    bt,
			BashMaxTimeout: bm,
			Jobs:           e.cfg.Jobs,
			AutoBackground: e.cfg.AutoBackground,
		})
	}
	e.task = &Task{cwd: in.Cfg.Cwd, sink: sink}
	if in.Interrupt != nil {
		in.Interrupt.Reset()
	}
	if in.Cfg.Subs != nil {
		e.subBase = make(map[int]bool)
		for _, id := range in.Cfg.Subs.RunningIDs() {
			e.subBase[id] = true
		}
	}

	e.turn = []TurnMsg{{Msg: provider.ChatMessage{Role: "user", Content: in.Prompt}}}
	e.compactArmed = true // hysteresis gate starts armed (M7.6)
	e.run(ctx)
	return RunResult{
		Messages:         e.turn,
		Steps:            e.step,
		WrapReason:       e.wrap,
		Usage:            e.usage,
		UsageTotal:       e.usageTotal,
		PromptTokens:     e.promptTokens,
		CompletionTokens: e.completionTokens,
		CachedTokens:     e.cachedTokens,
		ToolCalls:        e.toolCalls,
		Compacted:        compactedResult(e.compact),
		Err:              e.fatal,
	}
}

// compactedResult projects the engine's compaction state into the
// driver-facing result (nil when the run never compacted).
func compactedResult(c *compactState) *CompactResult {
	if c == nil {
		return nil
	}
	return &CompactResult{Checkpoint: c.Checkpoint, Cut: c.Cut}
}

type engine struct {
	in    Input
	cfg   Config
	sink  Sink
	now   func() time.Time
	tools []Tool
	task  *Task

	turn      []TurnMsg
	guard     loopGuard
	native    bool // advertise tools on this run's requests
	regrouped bool // the free regroup reminder was spent
	selfCheck int  // §16 self-vigilance level already injected (0/1/2)
	wrap      string
	step      int

	compact           *compactState // M7.6 projection: nil until first compaction
	compactArmed      bool          // hysteresis gate (re-arms under compactResumePct)
	overflowCompacted bool          // one overflow-driven compaction per run
	readFiles         []string      // successful read targets (checkpoint inventory)
	writtenFiles      []string      // successful write/edit targets

	// §16 end gate: the run must not end while its own workers are still
	// mid-brief — a final answer or a guard wrap-up would orphan their
	// reports (the terminal line lands with no manager to accept it).
	// subBase is the set of worker ids already running when the run
	// started (foreign leftovers; a worker's own engine sees its
	// siblings), so only this run's spawns gate it. subEndStreak counts
	// consecutive text-only rounds while own workers run: the first gets
	// a nudge, the second the engine collects the reports itself.
	subBase      map[int]bool
	subEndStreak int

	wrapRound bool // the next round is the forced wrap-up round

	usage            provider.Usage
	usageTotal       int
	promptTokens     int // cumulative reported prompt tokens (0 when unreported)
	completionTokens int
	cachedTokens     int
	toolCalls        int // tool invocations executed in this run (top-level)
	lastPromptSize   int // current request size in tokens (reported or estimated)
	fatal            error
}

func (e *engine) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// M7.6 soft trigger at the round boundary: past 85% of the
		// window the run compacts once; the view below is then rebuilt
		// over the checkpoint projection.
		e.compactCheck(ctx)
		e.step++
		e.sink.OnStep(e.step)

		// Request view: the fixed base plus an L1-slimmed view of the
		// whole conversation — newest tool results verbatim (within the
		// tail budget), older ones placeholders, history included so a
		// fresh prompt cannot resurrect a prior turn's megabytes. After
		// a compaction the view is re-projected over the checkpoint
		// (requestView); the turn itself keeps full content.
		slim := CollapseOldTools(e.requestView())
		req := make([]provider.ChatMessage, 0, len(slim))
		for _, tm := range slim {
			req = append(req, tm.Msg)
		}
		if e.maybeSelfCheck(req) {
			req = append(req, e.turn[len(e.turn)-1].Msg)
		}

		text, calls, _, fatal := e.streamRound(ctx, req)
		if fatal != nil {
			var oe *provider.OverflowError
			if errors.As(fatal, &oe) && !e.overflowCompacted {
				// M7.6: the provider refused the request as too long —
				// compact once and retry the round on the compacted
				// view; a second overflow stays fatal.
				e.overflowCompacted = true
				if e.compactForOverflow(ctx) {
					continue
				}
			}
			e.fatal = fatal
			return
		}

		// Only explicit native tool calls are ever executed; code blocks
		// in the reply are display text. (The former fence protocol —
		// executing ``` blocks from the reply — is removed: it ran
		// commands the model only meant to show.)
		e.turn = append(e.turn, TurnMsg{
			Msg: provider.ChatMessage{Role: "assistant", Content: text, ToolCalls: calls},
		})
		// Persist the finalized assistant turn atomically (text plus its
		// tool calls) so a rebuilt history pairs each call with its result.
		e.sink.OnAssistant(text, calls)

		if e.wrapRound {
			e.wrapRound = false
			// That was the wrap-up round: answer any disobedient calls
			// without running them.
			for _, call := range calls {
				e.appendNotRun(call, notRunWrap)
			}
			if ids := e.ownRunningSubIDs(); len(ids) > 0 {
				// The guard stopped the model, but this run's workers are
				// still mid-brief: collect their reports first, then let
				// the model wrap up with the evidence in a fresh round.
				// The wrap is consumed: a fresh guard violation later can
				// schedule another one (the workers are settled by then).
				e.collectSubReports(ctx, ids)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			return
		}
		if len(calls) == 0 {
			if ids := e.ownRunningSubIDs(); len(ids) > 0 {
				if e.subEndStreak == 0 {
					// First attempt to end while own workers run: the run
					// cannot end — tell the model to keep polling.
					e.subEndStreak = 1
					e.nudgeSubWait(ids)
					continue
				}
				// The model keeps ending: stop asking — wait for the
				// workers and feed the reports back itself.
				e.collectSubReports(ctx, ids)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			return // final answer
		}
		e.subEndStreak = 0

		regroup := false
		interrupted := false
		guardTripped := false
		for _, call := range calls {
			if ctx.Err() != nil {
				e.appendNotRun(call, notRunCancel)
				continue
			}
			if interrupted {
				// §8.3: the user interrupted one command of the round;
				// the rest are answered without running so the model
				// sees the interruption and picks the next step.
				e.appendNotRun(call, notRunInterrupt)
				continue
			}
			if guardTripped {
				// The guard tripped on an earlier call of this round:
				// answer the rest without running them — the wrap-up
				// round is next, and every tool_call_id needs its
				// answer to keep the wire legal.
				e.appendNotRun(call, notRunWrap)
				continue
			}
			e.sink.OnToolBegin(call)
			toolCtx, tcancel := context.WithCancel(ctx)
			if e.in.Interrupt != nil {
				e.in.Interrupt.Set(tcancel)
			}
			res, guardCmd := e.execCall(toolCtx, call)
			if e.in.Interrupt != nil {
				e.in.Interrupt.Set(nil)
			}
			tcancel()
			interrupted = e.in.Interrupt != nil && e.in.Interrupt.takeFired()

			// Screen and session log first; guard warnings below ride
			// only on the fed-back message.
			e.sink.OnToolEnd(call, res)
			e.turn = append(e.turn, TurnMsg{
				Msg:  provider.ChatMessage{Role: "tool", Content: FedContent(call.Name, res), ToolCallID: call.ID},
				Tool: &ToolMeta{Cmd: guardCmd, Exit: res.Code, Size: len(res.Output)},
			})

			// Guard update (§6.4): the repetition warning rides on the
			// result; wrap decisions and the regroup reminder act at
			// round end so tool results stay contiguous on the wire.
			entry := e.guardEntry(call, res, guardCmd)
			lr := e.guard.record(entry, e.step, e.now())
			warn := ""
			if lr.Consec >= loopWarnConsec {
				warn += fmt.Sprintf(loopWarnFormat, lr.Consec)
			}
			if lr.Consec >= loopBreakConsec || lr.Share >= loopShareLimit {
				e.wrap = wrapReasonLoop
				guardTripped = true
			} else if entry.kind == kindRead && lr.Consec < loopWarnConsec {
				// Read churn is evaluated only outside the exact-repeat
				// streak: loop guard has priority over identical re-reads
				// (§6.4), read churn governs same-target reads whose
				// output changed.
				switch {
				case lr.ReadCount == readChurnRepeat:
					warn += fmt.Sprintf(readWarnFormat, lr.ReadCount)
				case lr.ReadCount == readChurnRepeat+1:
					if e.regrouped {
						e.wrap = wrapReasonReadChurn
						guardTripped = true
					} else {
						regroup = true
					}
				case lr.ReadCount >= readChurnRepeat+2:
					e.wrap = wrapReasonReadChurn
					guardTripped = true
				}
			}
			if warn != "" {
				e.turn[len(e.turn)-1].Msg.Content += warn
			}
		}
		if ctx.Err() != nil {
			return // calls answered, stop before another request
		}
		if guardTripped {
			e.startWrap()
			continue
		}
		if regroup || e.guard.writeStalled(e.step, e.now()) {
			if e.regrouped {
				e.wrap = wrapReasonReadChurn
				e.startWrap()
				continue
			}
			e.regrouped = true
			e.turn = append(e.turn, TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: regroupInstruction}})
		}
	}
}

// streamRound sends one request with retry (§6): transport failures and
// 429/5xx back off exponentially (±25% jitter, Retry-After wins, capped);
// a tools rejection is fatal (no fence fallback anymore). A stream that
// breaks before its first visible token is
// retried like a failed request; a later break continues with the partial
// reply, since text already on screen cannot be re-requested cleanly.
// fatal is set for non-retryable failures and ^C.
func (e *engine) streamRound(ctx context.Context, req []provider.ChatMessage) (text string, calls []provider.ToolCall, usage *provider.Usage, fatal error) {
	attempt := 0
	for {
		opts := make([]provider.ChatOption, 0, 2)
		if e.native && len(e.tools) > 0 {
			opts = append(opts, provider.WithTools(toolDefs(e.tools)))
		}
		opts = append(opts, provider.WithUsage())

		ch, err := e.in.Client.ChatStream(ctx, req, opts...)
		if err != nil {
			// A tools rejection is fatal, not a degradation: the fence
			// fallback is gone, so an endpoint without tool support can
			// only serve a tools=false model, which this run never was.
			var toolsErr *provider.ToolsUnsupportedError
			if errors.As(err, &toolsErr) {
				return "", nil, nil, fmt.Errorf("该模型不支持工具调用（端点拒绝 tools 参数: %s），AI 模式不可用——请换一个支持 function calling 的模型", toolsErr.Error())
			}
			wait, retryable := retryWait(err, attempt, e.cfg)
			if !retryable || attempt >= e.cfg.RetryAttempts {
				return "", nil, nil, err
			}
			attempt++
			e.sink.OnStatus(fmt.Sprintf("请求失败（%s），%.1fs 后重试（%d/%d）",
				briefErr(err), wait.Seconds(), attempt, e.cfg.RetryAttempts))
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", nil, nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}

		var sb strings.Builder
		var midErr error
		emitted := false
		for tok := range ch {
			switch {
			case tok.Err != nil:
				midErr = tok.Err
			case tok.ToolCall != nil:
				calls = append(calls, *tok.ToolCall)
			case tok.Usage != nil:
				usage = tok.Usage
			default:
				e.sink.OnText(tok.Text, tok.Reasoning)
				emitted = true
				if !tok.Reasoning {
					sb.WriteString(tok.Text)
				}
			}
		}
		if midErr != nil {
			if ctx.Err() != nil {
				return "", nil, nil, ctx.Err()
			}
			wait, retryable := retryWait(midErr, attempt, e.cfg)
			if retryable && !emitted && attempt < e.cfg.RetryAttempts {
				attempt++
				e.sink.OnStatus(fmt.Sprintf("流式响应中断（%s），%.1fs 后重试（%d/%d）",
					briefErr(midErr), wait.Seconds(), attempt, e.cfg.RetryAttempts))
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return "", nil, nil, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if !emitted {
				// Nothing arrived before the break: the round failed.
				return "", nil, nil, midErr
			}
			// Part of the reply is already on screen; keep it and
			// let the loop treat the partial content as the round's
			// assistant message.
			e.sink.OnNotice("流式响应中断，按已收到的部分内容继续")
		}
		e.stepUsage(req, sb.Len(), usage)
		return sb.String(), calls, usage, nil
	}
}

// execCall runs one call against the registry. Failures — unknown tool,
// broken arguments, tool-level errors — become the fed-back result; they
// never kill the task (§5.1 弱模型韧性). The second return is the command
// text the guards classify (bash) or a short display line (other tools).
func (e *engine) execCall(ctx context.Context, call provider.ToolCall) (Result, string) {
	e.toolCalls++
	tool := findTool(e.tools, call.Name)
	if tool == nil {
		return Result{
			Meta:   "失败",
			Code:   1,
			Output: fmt.Sprintf("错误: 未知工具 %q；可用工具：%s", call.Name, toolNames(e.tools)),
		}, call.Name
	}
	args, err := parseToolArgs(call.Arguments)
	if err != nil {
		return Result{Meta: "失败", Code: 1, Output: "错误: " + err.Error()}, call.Name
	}
	if e.cfg.Approval != nil {
		if deny := e.cfg.Approval.Gate(ctx, call.Name, args); deny != "" {
			// §7 审批被拒: the refusal is fed back as this call's result —
			// the task continues and the model picks another route; a
			// refusal loop lands in the loop guard like any exact repeat.
			// Display carries the refusal so the [tool] line shows it too.
			display := approvalDisplay(call.Name, args)
			cmd := display
			if call.Name == "bash" {
				cmd = argOptString(args, "command")
			}
			return Result{Meta: "已拒绝", Code: 1, Output: deny, Display: "已拒绝: " + display}, cmd
		}
	}
	if cmd := argOptString(args, "command"); call.Name == "bash" && cmd != "" {
		res, err := tool.Execute(ctx, e.task, args)
		if err != nil {
			return Result{Meta: "失败", Code: 1, Output: "错误: " + err.Error()}, cmd
		}
		return res, cmd
	}
	res, err := tool.Execute(ctx, e.task, args)
	if err != nil {
		return Result{Meta: "失败", Code: 1, Output: "错误: " + err.Error()}, res.Display
	}
	// M7.6: successful file targets feed the checkpoint inventory
	// (noteFile filters to read/write/edit).
	e.noteFile(call.Name, argOptString(args, "path"))
	return res, res.Display
}

// guardEntry builds the guard-window entry for one executed call: bash is
// classified by the §7.1 read-only shape, registry ReadOnly tools are
// reads, write/edit are writes, the rest is other. The delegation tools
// are "other" on purpose: task_output is status polling, not a workspace
// read, so the read-churn guard (which counts same-target reads by
// tool+args alone) must not fire on the manager's normal monitor rhythm.
func (e *engine) guardEntry(call provider.ToolCall, res Result, guardCmd string) loopEntry {
	args := normalizeArgs(call.Arguments)
	var kind entryKind
	switch call.Name {
	case "task", "task_output", "task_kill":
		kind = kindOther
	case "bash":
		if bashLooksReadOnly(guardCmd) {
			kind = kindRead
		} else {
			kind = kindWrite
		}
	case "write", "edit":
		kind = kindWrite
	default:
		if t := findTool(e.tools, call.Name); t != nil && t.ReadOnly {
			kind = kindRead
		}
	}
	entry := loopEntry{sig: toolSignature(call.Name, args, res.Code, res.Output), kind: kind}
	if kind == kindRead {
		entry.readSig = readSignature(call.Name, args)
	}
	return entry
}

func (e *engine) appendNotRun(call provider.ToolCall, mark string) {
	e.turn = append(e.turn, TurnMsg{
		Msg:  provider.ChatMessage{Role: "tool", Content: mark, ToolCallID: call.ID},
		Tool: &ToolMeta{Cmd: call.Name, Exit: -1, Size: len(mark)},
	})
}

// maybeSelfCheck is the §16 self-vigilance injection: when the run's own
// context grows abnormally — past half the window (or the fixed estimate
// when the window is unset), or to an abnormal message count — one
// escalating user message tells the model to look for what grew and
// compress it, keeping the mission (first user message) the anchor. Level
// 1 nudges, level 2 orders a consolidation; each fires once per run. The
// check runs on the CURRENT request being assembled (the reported prompt
// tokens lag a round, so the two are combined with a max); the note is
// appended to the turn and to this request, at a round boundary, so tool
// results stay contiguous on the wire.
func (e *engine) maybeSelfCheck(req []provider.ChatMessage) bool {
	if e.selfCheck >= 2 {
		return false
	}
	est := 0
	for _, m := range req {
		est += len(m.Content)
	}
	size := est / 4
	if e.lastPromptSize > size {
		size = e.lastPromptSize
	}
	level, detail := 0, ""
	switch {
	case e.cfg.ContextWindow > 0:
		pct := size * 100 / e.cfg.ContextWindow
		if e.selfCheck == 0 && pct >= selfCheckL1Pct {
			level, detail = 1, fmt.Sprintf("约 %d%% 的窗口（%d/%d tokens）", pct, size, e.cfg.ContextWindow)
		} else if e.selfCheck == 1 && pct >= selfCheckL2Pct {
			level, detail = 2, fmt.Sprintf("约 %d%% 的窗口（%d/%d tokens）", pct, size, e.cfg.ContextWindow)
		}
	case e.selfCheck == 0 && size >= selfCheckL1Est:
		level, detail = 1, fmt.Sprintf("约 %d tokens", size)
	case e.selfCheck == 1 && size >= selfCheckL2Est:
		level, detail = 2, fmt.Sprintf("约 %d tokens", size)
	}
	if level == 0 && e.selfCheck == 0 {
		if msgs := len(e.in.Base) + len(e.turn); msgs >= selfCheckMsgs {
			level, detail = 1, fmt.Sprintf("%d 条消息", msgs)
		}
	}
	if level == 0 {
		return false
	}
	e.selfCheck = level
	if level == 1 {
		e.sink.OnNotice("─── self-check：上下文变重 ───")
		e.turn = append(e.turn, TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: fmt.Sprintf(selfCheckNote1, detail)}})
	} else {
		e.sink.OnNotice("─── self-check：上下文接近上限 ───")
		e.turn = append(e.turn, TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: fmt.Sprintf(selfCheckNote2, detail)}})
	}
	return true
}

// startWrap announces the forced wrap-up and injects the text-only
// instruction; the next round runs once and ends the task once this
// run's workers are settled. Only a guard trip in the current round
// schedules a wrap, so a stale reason cannot re-inject; after a
// deferred wrap (reports collected first) a fresh trip schedules the
// next wrap round, and that one ends the run — the workers are
// settled by then.
func (e *engine) startWrap() {
	e.sink.OnNotice("─── wrapping up (" + e.wrap + ") ───")
	e.wrapRound = true
	e.turn = append(e.turn, TurnMsg{
		Msg: provider.ChatMessage{Role: "user", Content: fmt.Sprintf(wrapUpInstruction, e.wrap)},
	})
}

// ownRunningSubIDs returns the ids of the subagents THIS run spawned that
// are still executing: the manager's running ids minus the run-start
// baseline, so foreign workers (leftovers, or a worker engine's siblings)
// never gate the run, and a run without a manager never does.
func (e *engine) ownRunningSubIDs() []int {
	if e.cfg.Subs == nil {
		return nil
	}
	var out []int
	for _, id := range e.cfg.Subs.RunningIDs() {
		if !e.subBase[id] {
			out = append(out, id)
		}
	}
	return out
}

// subWaitNudge is the first attempt-to-end nudge: the run cannot end while
// own workers run, and the engine will not wake the model when they
// finish — the model has to stay in the round and poll.
const subWaitNudge = "[rysh] 子任务仍在后台运行（%s）：本任务在全部子任务结束前不能收尾，引擎也不会在子任务完成时重新唤醒你。请继续用 task_output 逐个查询进展（隔几步一次即可），全部结束后再做验收总结。"

// subWaitCollect is the user message that carries the collected final
// states of the run's workers back to the model.
const subWaitCollect = "[rysh] 以上子任务已全部结束，最终状态与报告如下（报告以各子任务为准；验收仍需核查关键论断）："

// subIDsLabel renders ids as "task 1, task 2".
func subIDsLabel(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("task %d", id)
	}
	return strings.Join(parts, ", ")
}

// nudgeSubWait tells the model (on screen and in the request) that the run
// cannot end while its workers run.
func (e *engine) nudgeSubWait(ids []int) {
	e.sink.OnNotice("─── 等待子任务（" + subIDsLabel(ids) + "）完成 ───")
	e.turn = append(e.turn, TurnMsg{
		Msg: provider.ChatMessage{Role: "user", Content: fmt.Sprintf(subWaitNudge, subIDsLabel(ids))},
	})
}

// collectSubReports blocks until every given subagent settles (or the run
// is cancelled), then feeds the manager each worker's final state — report
// included — as one user message, so the run never ends with a worker's
// result unclaimed. The wait costs no tokens (the run holds between
// rounds); ^C still cancels: the driver kills the workers, which settles
// the wait, and the second press cancels this context.
func (e *engine) collectSubReports(ctx context.Context, ids []int) {
	subs := make([]*Subagent, 0, len(ids))
	for _, id := range ids {
		if s := e.cfg.Subs.Get(id); s != nil {
			subs = append(subs, s)
		}
	}
	if len(subs) == 0 {
		return
	}
	e.sink.OnNotice("─── 等待子任务（" + subIDsLabel(ids) + "）完成 ───")
	e.waitSubs(ctx, subs)
	if ctx.Err() != nil {
		return
	}
	var b strings.Builder
	for _, s := range subs {
		b.WriteString(s.Status())
		b.WriteString("\n\n")
	}
	e.turn = append(e.turn, TurnMsg{
		Msg: provider.ChatMessage{Role: "user", Content: subWaitCollect + "\n\n" + strings.TrimSpace(b.String())},
	})
}

// waitSubs blocks until every subagent settles or ctx is cancelled.
func (e *engine) waitSubs(ctx context.Context, subs []*Subagent) {
	settled := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for _, s := range subs {
			ch := s.Wait()
			wg.Add(1)
			go func(ch <-chan struct{}) {
				<-ch
				wg.Done()
			}(ch)
		}
		wg.Wait()
		close(settled)
	}()
	select {
	case <-ctx.Done():
	case <-settled:
	}
}

// stepUsage folds one round's token cost into the totals: the reported
// usage chunk when the endpoint sent one, otherwise a chars/4 estimate of
// what the round actually put on the wire. lastPromptSize keeps the
// CURRENT request's prompt size (not the cumulative cost) — that is the
// number the §16 self-check compares against the context window. The
// prompt/completion/cached splits accumulate per reported chunk (0 when the
// endpoint does not report usage, so callers must treat them as
// "reported only", not estimates).
func (e *engine) stepUsage(req []provider.ChatMessage, completionChars int, usage *provider.Usage) {
	if usage != nil {
		e.usage = *usage
		e.usageTotal += usage.TotalTokens
		e.promptTokens += usage.PromptTokens
		e.completionTokens += usage.CompletionTokens
		e.cachedTokens += usage.CachedTokens
		e.lastPromptSize = usage.PromptTokens
		return
	}
	reqChars := 0
	for _, m := range req {
		reqChars += len(m.Content)
	}
	e.usageTotal += (reqChars + completionChars) / 4
	e.lastPromptSize = reqChars / 4
}

// retryWait classifies a request or stream failure: transport errors and
// 429/5xx status codes back off exponentially with ±25% jitter (capped,
// Retry-After winning over the backoff); everything else — including
// other 4xx, context-overflow refusals (the engine answers those with a
// compaction, M7.6) and context cancellation — is fatal.
func retryWait(err error, attempt int, cfg Config) (time.Duration, bool) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false
	}
	// A tools rejection after degradation is never retryable: the
	// endpoint refuses tool_calls regardless of backoff (§6 降级).
	var tu *provider.ToolsUnsupportedError
	if errors.As(err, &tu) {
		return 0, false
	}
	// A context overflow is answered by compacting, not backing off.
	var oe *provider.OverflowError
	if errors.As(err, &oe) {
		return 0, false
	}
	var se *provider.StatusError
	if errors.As(err, &se) && se.Code != http.StatusTooManyRequests && se.Code < 500 {
		return 0, false
	}
	wait := cfg.RetryBase * time.Duration(1<<min(attempt, 16))
	switch f := rand.Float64(); {
	case f < 0.25:
		wait = wait * 3 / 4
	case f > 0.75:
		wait = wait * 5 / 4
	}
	if wait > cfg.RetryCap {
		wait = cfg.RetryCap
	}
	if se != nil && se.RetryAfter > wait {
		wait = se.RetryAfter
	}
	return wait, true
}

// briefErr renders an error for the retry status line: the HTTP status
// when we have one, else a rune-safe truncated message.
func briefErr(err error) string {
	var se *provider.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	r := []rune(err.Error())
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return string(r)
}
