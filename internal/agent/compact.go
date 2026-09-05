package agent

// Context compaction (M7.6, 架构 §10.3): 压缩 = 状态交接. When the run's
// own request nears the context window — or the provider refuses it as
// too long — everything before a cut point is handed over to a checkpoint:
// one summarizer call produces the seven-section skeleton, the harness
// appends the mechanical section (file inventory, live state snapshot),
// and the request view is re-projected over it. Four invariants hold:
//
// ① history is never rewritten — Base, turn and the disk log stay
//    untouched; only the request view changes (checkpoint lives in the
//    projection, not in the transcript);
// ② the cut point sits on a whole-round boundary and never splits an
//    assistant message from its tool results;
// ③ the summary owns "the past", the mechanical section owns "the now"
//    (todos, files, surviving jobs and subagents, rendered from the live
//    managers);
// ④ compaction never kills the task — a failed or degenerate summary
//    leaves the state untouched and the run continues on L1 folding.

import (
	"context"
	"fmt"
	"strings"

	"ruyishell/internal/provider"
)

const (
	// compactSoftPct: the soft trigger fires when the last measured
	// request passed this share of the context window.
	compactSoftPct = 85
	// compactResumePct: hysteresis — after a compaction the trigger
	// re-arms only once the request falls back under this share.
	compactResumePct = 50
	// compactTailPct: at most this share of the window stays verbatim
	// past the cut point (whole rounds).
	compactTailPct = 25
	// compactEstWindow stands in for an unconfigured window (same fixed
	// estimate the §16 self-check uses).
	compactEstWindow = selfCheckL2Est
	// minSummaryRunes: a shorter checkpoint is a degenerate summary —
	// retried once, then the trigger gives up (不变式④).
	minSummaryRunes = 500
)

// compactState is the projection one compaction installs: the checkpoint
// full text (summary + mechanical section) and the cut point — turn[:Cut]
// is absorbed into the checkpoint, turn[Cut:] stays verbatim in the view.
type compactState struct {
	Checkpoint string
	Cut        int
}

// CompactResult reports a finished compaction to the driver (RunResult):
// the checkpoint full text and the cut point, so the caller can shrink
// its in-memory history the same way — the disk log stays the untouched
// source of truth (the compact event is logged separately).
type CompactResult struct {
	Checkpoint string
	Cut        int
}

// checkpointLead introduces the checkpoint in the request view and in the
// history a later task inherits.
const checkpointLead = "[上下文已压缩] 以下 checkpoint 概括了之前的对话，直接继续任务，不要复述 checkpoint 本身。\n\n"

// CheckpointMessage renders a checkpoint as the user message the request
// view carries (engine) and the shrunk history inherits (driver).
func CheckpointMessage(text string) provider.ChatMessage {
	return provider.ChatMessage{Role: "user", Content: checkpointLead + text}
}

// windowTokens is the window the percentage checks run against: the
// configured one, or the fixed estimate.
func (e *engine) windowTokens() int {
	if e.cfg.ContextWindow > 0 {
		return e.cfg.ContextWindow
	}
	return compactEstWindow
}

// compactCheck is the soft trigger, evaluated at a round boundary: when
// the last measured request passed compactSoftPct of the window, the run
// compacts once; the hysteresis gate re-arms only after the request
// falls back under compactResumePct. It never fires while wrapping up.
func (e *engine) compactCheck(ctx context.Context) {
	if e.wrap != "" {
		return
	}
	win := e.windowTokens()
	if !e.compactArmed {
		if e.lastPromptSize*100/win < compactResumePct {
			e.compactArmed = true
		}
		return
	}
	if e.lastPromptSize == 0 || e.lastPromptSize*100/win < compactSoftPct {
		return
	}
	e.sink.OnNotice("─── compacting ───")
	if e.doCompact(ctx) {
		e.sink.OnNotice(fmt.Sprintf("[已压缩上下文: 保留近 %d 轮]", e.tailRounds()))
	} else {
		e.sink.OnNotice("上下文压缩失败，本轮继续（旧内容仍受 L1 折叠保护）")
	}
}

// compactForOverflow answers a context-overflow refusal: compact once and
// let the caller retry the round on the compacted view. Reports whether
// the retry may proceed.
func (e *engine) compactForOverflow(ctx context.Context) bool {
	e.sink.OnNotice("─── compacting ───")
	if e.doCompact(ctx) {
		e.sink.OnNotice(fmt.Sprintf("[已压缩上下文: 保留近 %d 轮]", e.tailRounds()))
		return true
	}
	e.sink.OnNotice("上下文压缩失败，无法继续")
	return false
}

// doCompact summarizes everything before the cut point into a checkpoint
// (merging the previous checkpoint on a second compaction) and installs
// the projection. On failure — request error or a degenerate summary
// after one retry — the state stays untouched and the run continues on
// L1 folding alone (不变式④).
func (e *engine) doCompact(ctx context.Context) bool {
	cut := e.chooseCut()
	text, err := e.summarize(ctx, cut)
	if err == nil && len([]rune(text)) < minSummaryRunes {
		text, err = e.summarize(ctx, cut)
	}
	if err == nil && len([]rune(text)) < minSummaryRunes {
		err = fmt.Errorf("checkpoint 仅 %d 字，视为退化", len([]rune(text)))
	}
	if err != nil {
		// Disarm on failure: a broken summarizer must not be re-asked
		// at every boundary; the gate re-arms under the resume line as
		// usual (不变式④ — the run falls back to L1 folding).
		e.compactArmed = false
		return false
	}
	e.compact = &compactState{
		Checkpoint: strings.TrimSpace(text) + "\n\n" + e.mechanicalSection(),
		Cut:        cut,
	}
	e.compactArmed = false
	e.sink.OnCompact(e.compact.Checkpoint)
	return true
}

// requestView assembles what this round's request sees. Without a
// compaction it is Base plus the whole turn. After one, the checkpoint
// stands in for the summarized prefix: the Base system messages, the
// mission (turn[0]) verbatim, the checkpoint as one user message, then
// the whole rounds past the cut point. Base and turn are never modified —
// only the projection changes (不变式①).
func (e *engine) requestView() []TurnMsg {
	if e.compact == nil {
		view := make([]TurnMsg, 0, len(e.in.Base)+len(e.turn))
		view = append(view, e.in.Base...)
		return append(view, e.turn...)
	}
	view := make([]TurnMsg, 0, len(e.in.Base)+2+len(e.turn)-e.compact.Cut)
	for _, tm := range e.in.Base {
		if tm.Msg.Role == "system" {
			view = append(view, tm)
		}
	}
	view = append(view, e.turn[0], TurnMsg{Msg: CheckpointMessage(e.compact.Checkpoint)})
	return append(view, e.turn[e.compact.Cut:]...)
}

// chooseCut picks the compaction cut point: the oldest whole-round
// boundary whose verbatim tail fits in compactTailPct of the window —
// or, when nothing fits, the shortest legal tail (at least one round
// survives). A boundary is any non-tool message index ≥ 1: assistant
// messages and their tool results are adjacent, so a cut on anything
// but a tool result keeps every tool_call/result pair whole (不变式②).
func (e *engine) chooseCut() int {
	budget := e.windowTokens() * compactTailPct / 100 * 4 // bytes ≈ 4 per token
	best, fallback := -1, -1
	cum := 0
	for i := len(e.turn) - 1; i >= 1; i-- {
		cum += msgSize(e.turn[i])
		if e.turn[i].Msg.Role == "tool" {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if cum <= budget {
			best = i
		}
	}
	switch {
	case best >= 0:
		return best
	case fallback >= 0:
		return fallback
	default:
		return len(e.turn)
	}
}

// summarize runs one summarizer request: the system prefix verbatim, then
// the conversation to absorb — the Base history plus the turn head on the
// first compaction, the previous checkpoint standing in for the already
// absorbed prefix plus the newly accumulated rounds on a later one —
// L1-folded either way, and the checkpoint instruction as the closing
// user message. No tools are advertised; the summary text never reaches
// the screen sink.
func (e *engine) summarize(ctx context.Context, cut int) (string, error) {
	msgs := make([]provider.ChatMessage, 0, len(e.in.Base)+cut+2)
	for _, tm := range e.in.Base {
		if tm.Msg.Role == "system" {
			msgs = append(msgs, tm.Msg)
		}
	}
	var conv []TurnMsg
	inst := CompactInstructions
	if e.compact == nil {
		for _, tm := range e.in.Base {
			if tm.Msg.Role != "system" {
				conv = append(conv, tm)
			}
		}
		conv = append(conv, e.turn[:cut]...)
	} else {
		msgs = append(msgs, CheckpointMessage(e.compact.Checkpoint))
		conv = append(conv, e.turn[e.compact.Cut:cut]...)
		inst = CompactIterateInstructions
	}
	for _, tm := range CollapseOldTools(conv) {
		msgs = append(msgs, tm.Msg)
	}
	msgs = append(msgs, provider.ChatMessage{Role: "user", Content: inst})

	ch, err := e.in.Client.ChatStream(ctx, msgs)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var midErr error
	for tok := range ch {
		switch {
		case tok.Err != nil:
			midErr = tok.Err
		case tok.Usage != nil:
			e.usageTotal += tok.Usage.TotalTokens
		case !tok.Reasoning:
			sb.WriteString(tok.Text)
		}
	}
	if midErr != nil {
		return "", midErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// mechanicalSection renders the checkpoint's machine-built tail: the
// session file inventory (tracked at execution time, accumulated across
// compactions) and the live state snapshot — facts that must not depend
// on the summary model's memory (不变式③).
func (e *engine) mechanicalSection() string {
	var b strings.Builder
	b.WriteString("## 会话文件清单\n")
	if len(e.readFiles) == 0 && len(e.writtenFiles) == 0 {
		b.WriteString("（无）\n")
	} else {
		if len(e.readFiles) > 0 {
			b.WriteString("已读: " + strings.Join(e.readFiles, ", ") + "\n")
		}
		if len(e.writtenFiles) > 0 {
			b.WriteString("已修改: " + strings.Join(e.writtenFiles, ", ") + "\n")
		}
	}
	b.WriteString("\n" + RenderStateSnapshot(e.task.Todos(), e.cfg.Jobs, e.cfg.Subs))
	return b.String()
}

// RenderStateSnapshot renders the checkpoint's live-state section: the
// todo plan, the surviving background jobs and the worker subagents with
// their compact state. Pure: it reads the managers, it changes nothing.
func RenderStateSnapshot(todos []Todo, jobs *JobManager, subs *SubagentManager) string {
	var b strings.Builder
	b.WriteString("## 当前状态\n### Todo\n")
	if len(todos) == 0 {
		b.WriteString("（无）\n")
	}
	for _, td := range todos {
		mark := "[ ]"
		switch td.Status {
		case "completed":
			mark = "[x]"
		case "in_progress":
			mark = "[~]"
		}
		b.WriteString("- " + mark + " " + td.Content + "\n")
	}
	if jobs != nil {
		if list := jobs.List(); len(list) > 0 {
			b.WriteString("### 后台任务\n")
			for _, j := range list {
				state := "运行中"
				switch {
				case j.Stopped():
					state = "已终止"
				case j.Done():
					state = fmt.Sprintf("已结束 (exit %d)", j.ExitCode())
				}
				b.WriteString(fmt.Sprintf("- job %d %s: %s（已 %s）\n",
					j.ID(), state, j.Command, HumanDuration(j.Elapsed())))
			}
		}
	}
	if subs != nil {
		if lines := subs.SnapshotLines(); len(lines) > 0 {
			b.WriteString("### 子任务\n")
			for _, ln := range lines {
				b.WriteString(ln + "\n")
			}
		}
	}
	return b.String()
}

// tailRounds counts the assistant rounds kept verbatim past the cut
// point, for the 已压缩上下文 notice line.
func (e *engine) tailRounds() int {
	if e.compact == nil {
		return 0
	}
	n := 0
	for _, tm := range e.turn[e.compact.Cut:] {
		if tm.Msg.Role == "assistant" {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// noteFile records a successful read/write/edit target for the
// checkpoint's file inventory: reads and writes accumulate across
// compactions in first-seen order.
func (e *engine) noteFile(name, path string) {
	switch name {
	case "read":
		e.readFiles = appendUnique(e.readFiles, path)
	case "write", "edit":
		e.writtenFiles = appendUnique(e.writtenFiles, path)
	}
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// msgSize estimates one message's on-the-wire size in bytes (content
// plus the tool-call envelope).
func msgSize(tm TurnMsg) int {
	n := len(tm.Msg.Content)
	for _, c := range tm.Msg.ToolCalls {
		n += len(c.ID) + len(c.Name) + len(c.Arguments)
	}
	return n
}
