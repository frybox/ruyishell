package agent

// Subagents and the manager model (§16): the top-level run is a manager;
// every non-trivial piece of work goes to a worker subagent — one more
// engine run with its own context. The manager's context sees only the
// subagent's compact status (task_output) and its final bounded report;
// the worker's transcript — its tool results, its dead ends — never
// crosses the seam. Subagents are managed like background jobs: spawned
// with an id, polled for progress, killed when they run wild.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ruyishell/internal/provider"
)

const (
	// subagentMax caps concurrent worker runs (per session).
	subagentMax = 3
	// subagentKeepDone retains finished subagents for late task_output
	// queries; the oldest finished one is evicted past the cap.
	subagentKeepDone = 20
	// taskReportCap bounds the report fed back to the manager: head over
	// tail, since the report leads with 已完成 (what matters for
	// acceptance). The worker is instructed to stay under 500 words; this
	// is the safety net, not the working limit.
	taskReportHead = 7 * 1024
	taskReportTail = 1024
	// pollHintWindow: a task_output poll this soon after the previous one,
	// with no step progress, gets a back-off hint instead of fresh data.
	pollHintWindow = 15 * time.Second
	// lastToolMax caps the "last action" line inside Status.
	lastToolMax = 120
)

// SubagentSnapshot is the manager task's frozen view a spawn inherits:
// the provider client, the worker's request base (cwd/env/worker prompt)
// and the run knobs (shared job manager; the approval gate, re-cut as
// the fail-closed worker view at spawn). Each spawned subagent keeps its
// own snapshot, so a later task's model switch or cwd does not touch
// workers that are already running.
type SubagentSnapshot struct {
	Client      StreamClient
	NativeTools bool
	Base        []TurnMsg
	Cfg         Config
}

// SubagentManager tracks a session's worker subagents. Spawn is the only
// entry; ids are monotonic and process-wide. SinkFor must return the
// per-subagent screen/log sink (the TUI adapter in main.go; nil in
// headless runs, which get a NopSink).
type SubagentManager struct {
	mu      sync.Mutex
	nextID  int
	running int
	order   []int // insertion order, for finished eviction
	subs    map[int]*Subagent
	SinkFor func(id int) Sink
}

// NewSubagentManager builds the session-wide manager (process-level, like
// the job manager and the approval gate).
func NewSubagentManager() *SubagentManager {
	return &SubagentManager{subs: make(map[int]*Subagent)}
}

// Spawn starts one worker for the given brief under the snapshot's
// client and base. The brief becomes the subagent's first user message;
// description is the short screen label. Spawning beyond the concurrent
// cap is an error the manager model can react to.
func (m *SubagentManager) Spawn(desc, brief string, snap SubagentSnapshot) (int, error) {
	m.mu.Lock()
	if m.running >= subagentMax {
		m.mu.Unlock()
		return 0, fmt.Errorf("已有 %d 个子任务在运行（上限 %d）；等其中一个结束后再派遣，或先 task_kill 停掉一个", m.running, subagentMax)
	}
	m.nextID++
	id := m.nextID
	m.running++
	sub := &Subagent{id: id, desc: desc, brief: brief, state: "running", started: time.Now(), doneCh: make(chan struct{})}
	m.subs[id] = sub
	m.order = append(m.order, id)
	var sink Sink
	if m.SinkFor != nil {
		sink = m.SinkFor(id)
	}
	m.mu.Unlock()
	// §16: the worker never prompts. The snapshot carries the session's
	// interactive gate; the worker gets its fail-closed view — same mode
	// and session rules, read live — so a background run can never block
	// on an answer the user is not watching. The denial is fed back like
	// any refusal; the manager re-runs the step in the foreground.
	if snap.Cfg.Approval != nil {
		snap.Cfg.Approval = snap.Cfg.Approval.WorkerGate()
	}
	if sink == nil {
		sink = NopSink{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	sub.mu.Lock()
	sub.cancel = cancel
	sub.mu.Unlock()

	sink.OnNotice(fmt.Sprintf("─── task %d: %s ───", id, desc))
	go m.runSub(sub, snap, ctx, sink)
	return id, nil
}

// runSub drives one worker run and settles its state. The terminal line
// (done/stopped, with step and duration) lands through the same sink, so
// the screen order stays: header, streamed work, terminal line.
func (m *SubagentManager) runSub(sub *Subagent, snap SubagentSnapshot, ctx context.Context, sink Sink) {
	pg := &progressSink{sub: sub, inner: sink}
	res := Run(ctx, Input{
		Client:      snap.Client,
		NativeTools: snap.NativeTools,
		Base:        snap.Base,
		Prompt:      sub.brief,
		Cfg:         snap.Cfg,
	}, pg)
	sub.finish(res, ctx.Err() != nil)
	if c, ok := sink.(interface{ Close() }); ok {
		c.Close()
	}
	sink.OnNotice(sub.TerminalLine())

	m.mu.Lock()
	m.running--
	m.evictFinishedLocked()
	m.mu.Unlock()
}

// evictFinishedLocked drops the oldest finished subagents until the table
// fits the cap of running + subagentKeepDone entries. Callers hold m.mu.
func (m *SubagentManager) evictFinishedLocked() {
	limit := subagentMax + subagentKeepDone
	for len(m.order) > limit {
		for i, id := range m.order {
			s := m.subs[id]
			s.mu.Lock()
			finished := s.state != "running"
			s.mu.Unlock()
			if finished {
				delete(m.subs, id)
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
}

// Get returns the subagent with the given id, or nil when unknown or
// evicted.
func (m *SubagentManager) Get(id int) *Subagent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subs[id]
}

// Running counts the workers still executing.
func (m *SubagentManager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// RunningIDs lists the ids of workers still executing, ascending. The
// engine's end gate uses it to tell which of a run's own workers are
// still mid-brief.
func (m *SubagentManager) RunningIDs() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	for _, id := range m.order {
		s := m.subs[id]
		s.mu.Lock()
		running := s.state == "running"
		s.mu.Unlock()
		if running {
			out = append(out, id)
		}
	}
	return out
}

// SnapshotLines renders one compact line per tracked subagent (insertion
// order), for the checkpoint's mechanical state section.
func (m *SubagentManager) SnapshotLines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.order))
	for _, id := range m.order {
		if s := m.subs[id]; s != nil {
			out = append(out, s.snapshotLine())
		}
	}
	return out
}

// KillRunning stops every running worker and reports how many there
// were. It is the ^C path: the manager task keeps going, and the next
// task_output shows the stopped state.
func (m *SubagentManager) KillRunning() int {
	m.mu.Lock()
	var running []*Subagent
	for _, s := range m.subs {
		s.mu.Lock()
		isRunning := s.state == "running"
		s.mu.Unlock()
		if isRunning {
			running = append(running, s)
		}
	}
	m.mu.Unlock()
	for _, s := range running {
		s.Kill()
	}
	return len(running)
}

// StopAll kills every running worker (rysh shutdown path).
func (m *SubagentManager) StopAll() {
	m.KillRunning()
}

// Subagent is one delegated worker: its compact state (for task_output)
// and its cancellation handle (for task_kill, ^C and StopAll). The full
// transcript lives only in its own engine run.
type Subagent struct {
	id    int
	desc  string
	brief string

	mu       sync.Mutex
	state    string // "running" | "done" | "killed"
	started  time.Time
	finished time.Time
	steps    int
	lastTool string
	todoLine string
	denied   string // last worker-gate refusal display (§16), for Status
	wrap     string
	err      string
	report   string
	cancel   context.CancelFunc

	lastPoll      time.Time
	lastPollSteps int

	doneCh chan struct{} // closed once (in finish)
}

// Kill stops the worker; it reports whether the worker was running and is
// now stopped (a second kill of a finished worker reports false). The run
// context cancels and the engine returns its partial exchange, which
// finish records as a partial report when there is one.
func (s *Subagent) Kill() bool {
	s.mu.Lock()
	if s.state != "running" {
		s.mu.Unlock()
		return false
	}
	s.state = "killed"
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// Wait returns a channel closed when the run has settled (done or
// killed).
func (s *Subagent) Wait() <-chan struct{} { return s.doneCh }

// noteStep records the newest round number (guard progress).
func (s *Subagent) noteStep(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = n
}

// noteTool records the last executed action's screen line, one line and
// bounded.
func (s *Subagent) noteTool(display string) {
	line := strings.TrimSpace(display)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	r := []rune(line)
	if len(r) > lastToolMax {
		line = string(r[:lastToolMax]) + "…"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTool = line
}

// noteDenied records the worker gate's newest refusal (the display line,
// bounded like lastTool): Status shows it so the manager can re-run the
// step in the foreground.
func (s *Subagent) noteDenied(display string) {
	line := strings.TrimSpace(display)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	r := []rune(line)
	if len(r) > lastToolMax {
		line = string(r[:lastToolMax]) + "…"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied = line
}

// noteTodo records the worker's current plan line (acceptance aid).
func (s *Subagent) noteTodo(todos []Todo) {
	done, cur := 0, ""
	for _, td := range todos {
		if td.Status == "completed" {
			done++
		}
		if td.Status == "in_progress" && cur == "" {
			cur = td.Content
		}
	}
	if cur == "" && len(todos) > 0 {
		cur = todos[0].Content
	}
	line := fmt.Sprintf("todo %d/%d · %s", done, len(todos), cur)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.todoLine = line
}

// finish settles the state from the finished run. It always records the
// results; the state is "killed" when Kill won the race, "done" otherwise
// (a ctx-cancelled run that was not explicitly killed also lands in
// killed — its driver went away).
func (s *Subagent) finish(res RunResult, cancelled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report = res.Report()
	s.steps = res.Steps
	s.wrap = res.WrapReason
	if res.Err != nil && !cancelled {
		s.err = res.Err.Error()
	}
	s.finished = time.Now()
	if s.state == "running" {
		if cancelled {
			s.state = "killed"
		} else {
			s.state = "done"
		}
	}
	close(s.doneCh)
}

// TerminalLine renders the dim closing screen line (also the session-log
// sub record).
func (s *Subagent) TerminalLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dur := s.durationLocked()
	switch s.state {
	case "done":
		line := fmt.Sprintf("[task %d] done · %d 步 · %s", s.id, s.steps, HumanDuration(dur))
		if s.wrap != "" {
			line += fmt.Sprintf(" · 被强制收尾（%s）", s.wrap)
		}
		if s.report != "" {
			line += " · " + humanSize(len(s.report)) + " 报告"
		}
		return line
	case "killed":
		return fmt.Sprintf("[task %d] 已终止 · %d 步 · %s", s.id, s.steps, HumanDuration(dur))
	default:
		return fmt.Sprintf("[task %d] 运行中 · %d 步 · %s", s.id, s.steps, HumanDuration(time.Since(s.started)))
	}
}

func (s *Subagent) durationLocked() time.Duration {
	if s.finished != (time.Time{}) {
		return s.finished.Sub(s.started)
	}
	return time.Since(s.started)
}

// snapshotLine is the one-line compact state the checkpoint's mechanical
// section carries for this worker.
func (s *Subagent) snapshotLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	switch state {
	case "running":
		state = "运行中"
	case "done":
		state = "已完成"
	case "killed":
		state = "已终止"
	}
	return fmt.Sprintf("- task %d %s: %s（第 %d 步 · %s）",
		s.id, state, s.desc, s.steps, HumanDuration(s.durationLocked()))
}

// Status renders the compact state task_output feeds the manager: header
// line plus the last action (and plan) while running, plus the bounded
// report once finished, plus the worker gate's last refusal whenever the
// worker hit a call that needs the user's approval (so the manager can
// re-run that step in the foreground). The anti-spin hint lands when a
// fresh poll shows no step progress.
func (s *Subagent) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	switch s.state {
	case "running":
		head := fmt.Sprintf("[task %d] 运行中 · 第 %d 步 · 已 %s", s.id, s.steps, HumanDuration(now.Sub(s.started)))
		if s.lastTool != "" {
			head += "\n最近动作: " + s.lastTool
		}
		if s.todoLine != "" {
			head += "\n" + s.todoLine
		}
		head += s.deniedLineLocked()
		if now.Sub(s.lastPoll) < pollHintWindow && s.steps == s.lastPollSteps {
			head += "\n（自上次检查无新进展；稍后再查，或先做别的事——子任务在后台自己推进）"
		}
		s.lastPoll = now
		s.lastPollSteps = s.steps
		return head
	case "killed":
		head := fmt.Sprintf("[task %d] 已终止 · 第 %d 步 · %s", s.id, s.steps, HumanDuration(s.durationLocked()))
		if s.report != "" {
			head += "\n── 中断时的部分报告 ──\n" + capReport(s.report)
		} else {
			head += "\n（无报告；工作现场留在工作区，检查后修正提示词重新派遣）"
		}
		head += s.deniedLineLocked()
		return head
	default:
		head := fmt.Sprintf("[task %d] 已完成 · %d 步 · %s", s.id, s.steps, HumanDuration(s.durationLocked()))
		if s.wrap != "" {
			head += fmt.Sprintf("\n注意：该子任务被守卫强制收尾（%s），报告可疑，验收需格外严格", s.wrap)
		}
		if s.err != "" {
			head += "\n（请求错误: " + s.err + "）"
		}
		if s.report != "" {
			head += "\n── 报告 ──\n" + capReport(s.report)
		} else {
			head += "\n（无报告）"
		}
		head += s.deniedLineLocked()
		return head
	}
}

// deniedLineLocked renders the worker gate's last refusal for the
// manager: which step the worker cannot run (it needs the user's
// approval) and the way to act — re-run it in the foreground. Empty
// when the worker never hit a gated call.
func (s *Subagent) deniedLineLocked() string {
	if s.denied == "" {
		return ""
	}
	return "\n被拒（需用户批准，子任务内不可执行）: " + s.denied + " —— 需要它的结果就由主任务在前台执行"
}

// capReport bounds the report fed to the manager (head over tail).
func capReport(r string) string {
	return truncateMid(r, taskReportHead, taskReportTail)
}

// HumanDuration renders a duration as its non-zero units from the largest
// down, so a run is readable at any scale: "45.2s" under a minute, then
// e.g. "2m3s", "5h12m" or "2d1h23m4s" — zero units are omitted.
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	totalSec := int(d.Seconds())
	days := totalSec / 86400
	totalSec %= 86400
	hours := totalSec / 3600
	totalSec %= 3600
	mins := totalSec / 60
	secs := totalSec % 60
	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	if mins > 0 {
		fmt.Fprintf(&b, "%dm", mins)
	}
	if secs > 0 {
		fmt.Fprintf(&b, "%ds", secs)
	}
	return b.String()
}

// progressSink forwards the worker's events to the TUI sink while
// distilling the compact state task_output reports.
type progressSink struct {
	sub   *Subagent
	inner Sink
}

func (p *progressSink) OnText(delta string, reasoning bool) { p.inner.OnText(delta, reasoning) }
func (p *progressSink) OnToolBegin(call provider.ToolCall)  { p.inner.OnToolBegin(call) }
func (p *progressSink) OnAssistant(text string, calls []provider.ToolCall) {
	p.inner.OnAssistant(text, calls)
}
func (p *progressSink) OnToolEnd(call provider.ToolCall, res Result) {
	if res.Meta == "已拒绝" {
		p.sub.noteDenied(strings.TrimPrefix(res.Display, "已拒绝: "))
	}
	p.sub.noteTool(res.Display)
	p.inner.OnToolEnd(call, res)
}
func (p *progressSink) OnTodo(todos []Todo) {
	p.sub.noteTodo(todos)
	p.inner.OnTodo(todos)
}
func (p *progressSink) OnStep(n int) {
	p.sub.noteStep(n)
	p.inner.OnStep(n)
}
func (p *progressSink) OnNotice(text string) { p.inner.OnNotice(text) }
func (p *progressSink) OnStatus(text string) { p.inner.OnStatus(text) }
func (p *progressSink) OnCompact(text string) {
	p.inner.OnCompact(text)
}

// Report returns the run's final assistant answer — the last assistant
// message without pending tool calls; empty when the run never produced a
// final answer (killed mid-tool, or the model went quiet). On a wrapped
// run this is the forced wrap-up summary, which Status flags.
func (r RunResult) Report() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		m := r.Messages[i].Msg
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Content) != "" {
			return m.Content
		}
	}
	return ""
}
