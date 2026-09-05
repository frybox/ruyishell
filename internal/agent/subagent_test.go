package agent

// Subagent manager tests (§16): the worker lifecycle — spawn, status,
// report, kill — the concurrency cap, eviction of finished workers, the
// bounded report, the guard wrap flag, and the anti-spin poll hint.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ruyishell/internal/provider"
)

// stuckClient streams nothing until its context is cancelled, then
// reports the cancellation error — a worker caught mid-round when killed.
type stuckClient struct{}

func (stuckClient) ChatStream(ctx context.Context, msgs []provider.ChatMessage, opts ...provider.ChatOption) (<-chan provider.StreamToken, error) {
	ch := make(chan provider.StreamToken, 1)
	go func() {
		<-ctx.Done()
		ch <- provider.StreamToken{Err: ctx.Err()}
		close(ch)
	}()
	return ch, nil
}

// finalOnlyClient answers with one final text — the fastest possible
// worker.
func finalOnlyClient(report string) *mockClient {
	return &mockClient{rounds: []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return report, nil, nil, nil
		},
	}}
}

func subagentSnap(client StreamClient, dir string) SubagentSnapshot {
	return SubagentSnapshot{
		Client:      client,
		NativeTools: true,
		Base:        []TurnMsg{{Msg: provider.ChatMessage{Role: "system", Content: WorkerInstructions}}},
		Cfg:         fastCfg(dir),
	}
}

// TestSubagentLifecycle drives one worker to completion and checks the
// state machine: running → done, the report in the status, the terminal
// line, and the manager's running counter settling.
func TestSubagentLifecycle(t *testing.T) {
	worker := &mockClient{t: t}
	worker.rounds = []scriptRound{
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "w0", Name: "bash", Arguments: `{"command":"echo WORKER_STEP_ONE"}`}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "① 已完成：运行了第一步。② 无修改。③ 无。", nil, nil, nil
		},
	}
	sm := NewSubagentManager()
	id, err := sm.Spawn("测试子任务", "运行一个命令并汇报", subagentSnap(worker, t.TempDir()))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != 1 {
		t.Fatalf("first subagent id = %d, want 1", id)
	}
	sub := sm.Get(id)
	if sub == nil {
		t.Fatal("Get(spawned id) = nil")
	}
	if sm.Running() != 1 {
		t.Fatalf("Running() = %d, want 1", sm.Running())
	}

	running := sub.Status()
	if !strings.Contains(running, "运行中") && !strings.Contains(running, "已完成") {
		t.Fatalf("status before settle = %q, want running or already done", running)
	}
	<-sub.Wait()

	st := sub.Status()
	for _, want := range []string{"已完成", "① 已完成：运行了第一步"} {
		if !strings.Contains(st, want) {
			t.Fatalf("finished status missing %q:\n%s", want, st)
		}
	}
	// The worker's intermediate command/output stays in the worker's own
	// context — it must not appear in the manager-facing status.
	if strings.Contains(st, "WORKER_STEP_ONE") {
		t.Fatalf("worker's intermediate output leaked into the status:\n%s", st)
	}
	if !strings.Contains(sub.TerminalLine(), "done") {
		t.Fatalf("terminal line = %q, want done", sub.TerminalLine())
	}
	if sm.Running() != 0 {
		t.Fatalf("Running() after finish = %d, want 0", sm.Running())
	}
	// The worker's own context carried the intermediate output; the status
	// (the only thing crossing to the manager) carries the report.
	lastWorkerReq := worker.requests()[len(worker.requests())-1]
	workerBody := flattenReqs(lastWorkerReq)
	if !strings.Contains(workerBody, "WORKER_STEP_ONE") {
		t.Fatal("worker's own request view missing its tool result")
	}
}

// flattenReqs renders a request view for substring assertions.
func flattenReqs(reqs []provider.ChatMessage) string {
	var b strings.Builder
	for _, m := range reqs {
		b.WriteString(m.Role)
		b.WriteString("\x00")
		b.WriteString(m.Content)
		b.WriteString("\x00")
	}
	return b.String()
}

// TestSubagentCap rejects a spawn beyond the concurrent cap and accepts
// it again once a worker is killed.
func TestSubagentCap(t *testing.T) {
	sm := NewSubagentManager()
	snap := subagentSnap(stuckClient{}, t.TempDir())
	for i := 0; i < subagentMax; i++ {
		if _, err := sm.Spawn("忙", "忙", snap); err != nil {
			t.Fatalf("spawn %d: %v", i+1, err)
		}
	}
	if _, err := sm.Spawn("满员", "满员", snap); err == nil {
		t.Fatalf("spawn beyond cap %d succeeded", subagentMax)
	} else if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("cap error = %q, want 上限", err.Error())
	}
	sm.KillRunning()
	for i := 0; i < 20 && sm.Running() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if id, err := sm.Spawn("补位", "补位", snap); err != nil {
		t.Fatalf("spawn after kill: %v", err)
	} else {
		sm.Get(id).Kill()
	}
	sm.StopAll()
}

// TestSubagentKill stops a running worker: the state becomes killed, the
// partial exchange is settled, a second kill reports false, and the
// terminal line names the stop.
func TestSubagentKill(t *testing.T) {
	sm := NewSubagentManager()
	id, err := sm.Spawn("卡住的任务", "永远跑不完", subagentSnap(stuckClient{}, t.TempDir()))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sub := sm.Get(id)
	if !sub.Kill() {
		t.Fatal("first kill reports false, want true (was running)")
	}
	<-sub.Wait()
	st := sub.Status()
	if !strings.Contains(st, "已终止") {
		t.Fatalf("killed status = %q, want 已终止", st)
	}
	if !strings.Contains(sub.TerminalLine(), "已终止") {
		t.Fatalf("terminal line = %q, want 已终止", sub.TerminalLine())
	}
	if sub.Kill() {
		t.Fatal("second kill reports true, want false")
	}
	if sm.Running() != 0 {
		t.Fatalf("Running() = %d, want 0", sm.Running())
	}
}

// TestSubagentReportCap bounds the report fed to the manager: a 30KB
// report comes back head-over-tail under the cap with an elision marker.
func TestSubagentReportCap(t *testing.T) {
	report := strings.Repeat("报", 30*1024)
	sm := NewSubagentManager()
	id, err := sm.Spawn("长报告", "汇报", subagentSnap(finalOnlyClient(report), t.TempDir()))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sub := sm.Get(id)
	<-sub.Wait()
	st := sub.Status()
	if !strings.Contains(st, "已省略") {
		t.Fatal("oversized report not elided")
	}
	if len(st) > taskReportHead+taskReportTail+1024 {
		t.Fatalf("status with capped report = %d bytes, want under ~%d", len(st), taskReportHead+taskReportTail+512)
	}
	if !strings.HasPrefix(st, "[task 1] 已完成") {
		t.Fatalf("status head = %q", st[:20])
	}
}

// TestSubagentWrapFlag marks a worker that its own guards force-wrapped:
// the report survives (it is the forced wrap-up summary) but the status
// flags it as suspect for acceptance.
func TestSubagentWrapFlag(t *testing.T) {
	worker := &mockClient{t: t}
	for i := 0; i < loopBreakConsec; i++ {
		worker.rounds = append(worker.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{ID: "w", Name: "bash", Arguments: `{"command":"echo STUCK"}`}}, nil, nil
		})
	}
	worker.rounds = append(worker.rounds, func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
		return "① 重复执行了 echo STUCK。② 无。③ 卡住了。", nil, nil, nil
	})
	sm := NewSubagentManager()
	id, err := sm.Spawn("空转的任务", "做点什么", subagentSnap(worker, t.TempDir()))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sub := sm.Get(id)
	<-sub.Wait()
	st := sub.Status()
	for _, want := range []string{"强制收尾", wrapReasonLoop, "验收需格外严格"} {
		if !strings.Contains(st, want) {
			t.Fatalf("wrapped status missing %q:\n%s", want, st)
		}
	}
}

// TestSubagentPollHint: a fresh poll with no step progress tells the
// manager to back off instead of tight-polling.
func TestSubagentPollHint(t *testing.T) {
	sm := NewSubagentManager()
	id, err := sm.Spawn("慢任务", "慢慢做", subagentSnap(stuckClient{}, t.TempDir()))
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sub := sm.Get(id)
	first := sub.Status()
	if strings.Contains(first, "无新进展") {
		t.Fatalf("first poll already carries the hint:\n%s", first)
	}
	again := sub.Status()
	if !strings.Contains(again, "无新进展") {
		t.Fatalf("second same-second poll missing the hint:\n%s", again)
	}
	sm.StopAll()
}

// TestSubagentEviction keeps the newest finished workers reachable while
// the oldest fall off the table past the retention cap.
func TestSubagentEviction(t *testing.T) {
	sm := NewSubagentManager()
	total := subagentMax + subagentKeepDone + 5
	var lastID int
	for i := 1; i <= total; i++ {
		id, err := sm.Spawn("批处理", "快速完成", subagentSnap(finalOnlyClient("① 完成。② 无。③ 无。"), t.TempDir()))
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		lastID = id
		<-sm.Get(id).Wait()
	}
	if sm.Get(1) != nil {
		t.Fatal("oldest finished subagent survived eviction")
	}
	if sm.Get(lastID) == nil {
		t.Fatal("newest finished subagent was evicted")
	}
}

// TestSubagentStopAll kills every running worker (shutdown path).
func TestSubagentStopAll(t *testing.T) {
	sm := NewSubagentManager()
	snap := subagentSnap(stuckClient{}, t.TempDir())
	for i := 0; i < 2; i++ {
		if _, err := sm.Spawn("收尾", "收尾", snap); err != nil {
			t.Fatalf("spawn: %v", err)
		}
	}
	sm.StopAll()
	deadline := time.Now().Add(2 * time.Second)
	for sm.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sm.Running() != 0 {
		t.Fatalf("Running() after StopAll = %d, want 0", sm.Running())
	}
}

// TestSubagentWorkerGateFailsClosed pins the §16 approval seam: the
// worker gets the session gate's fail-closed view — a gated call is
// denied with the worker text (the ask callback must never fire, that
// would be the prompt the user is not watching), fed back, and the step
// lands in the manager's status for a foreground re-run.
func TestSubagentWorkerGateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	asked := 0
	ap := NewApproval("ask", func(ctx context.Context, req AskRequest) Answer {
		asked++
		return AnswerAllow
	})
	worker := &mockClient{t: t}
	worker.rounds = []scriptRound{
		// The redirect takes the command out of the §7.1 safe shape, so
		// the call would ask — and must be denied in the worker.
		bashRound("w1", "echo x > worker-out.txt"),
		finalRound("① 完成了其余工作。② 无。③ 被拒：echo x > worker-out.txt（需用户批准）。"),
	}
	sm := NewSubagentManager()
	snap := subagentSnap(worker, dir)
	snap.Cfg.Approval = ap
	id, err := sm.Spawn("写文件", "写一个文件并汇报", snap)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sub := sm.Get(id)
	<-sub.Wait()
	if asked != 0 {
		t.Fatalf("the worker prompted the user %d times, want 0 (workers never ask)", asked)
	}
	if _, err := os.Stat(filepath.Join(dir, "worker-out.txt")); !os.IsNotExist(err) {
		t.Fatal("denied command executed anyway")
	}
	st := sub.Status()
	for _, want := range []string{"被拒", "echo x > worker-out.txt", "主任务在前台执行"} {
		if !strings.Contains(st, want) {
			t.Fatalf("finished status missing %q:\n%s", want, st)
		}
	}
	if !strings.Contains(st, "① 完成了其余工作") {
		t.Fatalf("finished status missing the report:\n%s", st)
	}
}
