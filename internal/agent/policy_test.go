package agent

import (
	"context"
	"strings"
	"testing"
)

// scriptedAsk answers from a fixed script, recording every request.
func scriptedAsk(answers []Answer) (func(ctx context.Context, req AskRequest) Answer, *[]AskRequest) {
	var seen []AskRequest
	i := 0
	return func(ctx context.Context, req AskRequest) Answer {
		seen = append(seen, req)
		ans := AnswerDeny
		if i < len(answers) {
			ans = answers[i]
		}
		i++
		return ans
	}, &seen
}

func TestBashPrefixExtraction(t *testing.T) {
	cases := []struct {
		cmd, want string
	}{
		{"rm -rf build", "rm"},
		{"rm x", "rm"},
		{"git push --force origin main", "git push"},
		{"git status", "git status"},
		{"git", ""}, // bare two-token head: no rule
		{"git -C path push", "git path"},
		{"go test ./...", "go test"},
		{"docker compose up -d", "docker compose"},
		{"make test", "make"},
		{"  npm   install ", "npm install"},
		{"ls -la", "ls"},
		{"", ""},
		{"-x -y foo", "foo"},
		{"pip3 install requests", "pip3 install"},
	}
	for _, tc := range cases {
		if got := bashPrefix(tc.cmd); got != tc.want {
			t.Errorf("bashPrefix(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestGateAllowReadOnlyAndSafeBash(t *testing.T) {
	ask, seen := scriptedAsk(nil)
	a := NewApproval("ask", ask)
	ctx := context.Background()

	// Registry read tools pass without asking.
	for _, tool := range []string{"read", "glob", "grep", "ls", "job_output", "todo", "job_kill"} {
		if got := a.Gate(ctx, tool, map[string]any{}); got != "" {
			t.Errorf("Gate(%s) denied: %q", tool, got)
		}
	}
	// Safe-classifier bash passes without asking.
	if got := a.Gate(ctx, "bash", map[string]any{"command": "ls -la"}); got != "" {
		t.Errorf("Gate(ls -la) denied: %q", got)
	}
	if got := a.Gate(ctx, "bash", map[string]any{"command": "git status"}); got != "" {
		t.Errorf("Gate(git status) denied: %q", got)
	}
	if len(*seen) != 0 {
		t.Fatalf("ask called %d times for read-only work, want 0", len(*seen))
	}
}

func TestGateWriteAndUnsafeBashAsk(t *testing.T) {
	ask, seen := scriptedAsk([]Answer{AnswerAllow, AnswerDeny})
	a := NewApproval("ask", ask)
	ctx := context.Background()

	if got := a.Gate(ctx, "write", map[string]any{"path": "a.txt"}); got != "" {
		t.Fatalf("Gate(write, y) denied: %q", got)
	}
	deny := a.Gate(ctx, "bash", map[string]any{"command": "rm x"})
	if !strings.Contains(deny, "[用户拒绝了该操作]") || !strings.Contains(deny, "rm x") {
		t.Fatalf("Gate(rm x, n) = %q, want refusal text naming the command", deny)
	}
	if len(*seen) != 2 {
		t.Fatalf("ask called %d times, want 2", len(*seen))
	}
	if (*seen)[1].Command != "rm x" || (*seen)[1].Display != "rm x" {
		t.Fatalf("ask request = %+v, want command rm x", (*seen)[1])
	}
}

func TestGateAlwaysBashPrefixRule(t *testing.T) {
	ask, seen := scriptedAsk([]Answer{AnswerAlways})
	a := NewApproval("ask", ask)
	ctx := context.Background()

	if got := a.Gate(ctx, "bash", map[string]any{"command": "touch one"}); got != "" {
		t.Fatalf("Gate(touch one, a) denied: %q", got)
	}
	// Same prefix: allowed without asking again.
	if got := a.Gate(ctx, "bash", map[string]any{"command": "touch two --flag"}); got != "" {
		t.Fatalf("Gate(touch two) denied after always: %q", got)
	}
	// Different prefix: asks (script exhausted → deny).
	if got := a.Gate(ctx, "bash", map[string]any{"command": "rm x"}); !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("Gate(rm x) after always on touch = %q, want a fresh refusal", got)
	}
	if len(*seen) != 2 {
		t.Fatalf("ask called %d times, want 2 (touch two must skip)", len(*seen))
	}
	if want := "bash: touch"; !strings.Contains(a.RuleSummary(), want) {
		t.Fatalf("RuleSummary() = %q, want it to contain %q", a.RuleSummary(), want)
	}
}

func TestGateAlwaysPerToolRule(t *testing.T) {
	ask, seen := scriptedAsk([]Answer{AnswerAlways})
	a := NewApproval("ask", ask)
	ctx := context.Background()

	if got := a.Gate(ctx, "edit", map[string]any{"path": "x.go"}); got != "" {
		t.Fatalf("Gate(edit, a) denied: %q", got)
	}
	// edit is now free; write still asks (per-tool rules, §7.2).
	if got := a.Gate(ctx, "edit", map[string]any{"path": "y.go"}); got != "" {
		t.Fatalf("Gate(edit) denied after always: %q", got)
	}
	if got := a.Gate(ctx, "write", map[string]any{"path": "z.txt"}); !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("Gate(write) after edit-always = %q, want a fresh refusal", got)
	}
	if len(*seen) != 2 {
		t.Fatalf("ask called %d times, want 2", len(*seen))
	}
	if want := "tool: edit"; !strings.Contains(a.RuleSummary(), want) {
		t.Fatalf("RuleSummary() = %q, want it to contain %q", a.RuleSummary(), want)
	}
}

func TestGateAlwaysModeNeverAsks(t *testing.T) {
	ask, seen := scriptedAsk(nil)
	a := NewApproval("always", ask)
	ctx := context.Background()

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{{"bash", map[string]any{"command": "rm -rf /"}},
		{"write", map[string]any{"path": "a"}},
		{"edit", map[string]any{"path": "b"}}} {
		if got := a.Gate(ctx, tc.tool, tc.args); got != "" {
			t.Errorf("Gate(%s) in always denied: %q", tc.tool, got)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("ask called %d times in always mode, want 0", len(*seen))
	}
}

func TestGateNeverModeRefusesWithoutAsking(t *testing.T) {
	ask, seen := scriptedAsk(nil)
	a := NewApproval("never", ask)
	ctx := context.Background()

	// Read-only work still runs free.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{{"read", map[string]any{}}, {"bash", map[string]any{"command": "ls -la"}}} {
		if got := a.Gate(ctx, tc.tool, tc.args); got != "" {
			t.Errorf("Gate(%s) in never refused read-only: %q", tc.tool, got)
		}
	}
	// Every gated call is refused without asking.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{{"bash", map[string]any{"command": "rm -rf /"}},
		{"write", map[string]any{"path": "a"}},
		{"edit", map[string]any{"path": "b"}}} {
		if got := a.Gate(ctx, tc.tool, tc.args); !strings.Contains(got, "[用户拒绝了该操作]") {
			t.Errorf("Gate(%s) in never = %q, want refusal text", tc.tool, got)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("ask called %d times in never mode, want 0", len(*seen))
	}
}

func TestGateNilAskFailsClosed(t *testing.T) {
	a := NewApproval("ask", nil)
	got := a.Gate(context.Background(), "write", map[string]any{"path": "a"})
	if !strings.Contains(got, "[子任务无法请求用户批准]") {
		t.Fatalf("Gate(write) with nil ask = %q, want fail-closed worker refusal", got)
	}
}

func TestWorkerGateSharesCore(t *testing.T) {
	ask, seen := scriptedAsk([]Answer{AnswerAlways})
	a := NewApproval("ask", ask)
	w := a.WorkerGate()
	ctx := context.Background()

	// No rule yet: the worker view fails closed with the worker text and
	// never prompts.
	got := w.Gate(ctx, "bash", map[string]any{"command": "make build"})
	if !strings.Contains(got, "[子任务无法请求用户批准]") {
		t.Fatalf("worker Gate(make build) = %q, want worker refusal", got)
	}
	// The user allows the "make" prefix in the foreground...
	if g := a.Gate(ctx, "bash", map[string]any{"command": "make test"}); g != "" {
		t.Fatalf("parent Gate(make test, a) = %q, want allowed", g)
	}
	// ...the worker sees the rule live, without asking.
	if g := w.Gate(ctx, "bash", map[string]any{"command": "make lint"}); g != "" {
		t.Fatalf("worker Gate(make lint) after the parent's rule = %q, want allowed", g)
	}
	if len(*seen) != 1 {
		t.Fatalf("ask called %d times, want 1 (only the parent's make test)", len(*seen))
	}
	// Mode hot-reload reaches the worker: always lets everything through.
	a.SetMode("always")
	if g := w.Gate(ctx, "write", map[string]any{"path": "x"}); g != "" {
		t.Fatalf("worker Gate(write) in always = %q, want \"\"", g)
	}
	// The safe classifier never needed a rule — still free for the worker.
	if g := w.Gate(ctx, "bash", map[string]any{"command": "git status"}); g != "" {
		t.Fatalf("worker Gate(git status) = %q, want \"\"", g)
	}
	// A rule recorded through the worker view lands in the same core.
	if !strings.Contains(a.RuleSummary(), "bash: make") {
		t.Fatalf("RuleSummary() = %q, want the make rule shared", a.RuleSummary())
	}
}

func TestGateCtxCancelledDenies(t *testing.T) {
	// Mirrors the driver's askApproval: the select over the answer channel
	// and ctx.Done() returns deny on cancellation. Gate must surface that
	// answer as the fed-back refusal.
	ask := func(ctx context.Context, req AskRequest) Answer {
		if ctx.Err() != nil {
			return AnswerDeny
		}
		return AnswerAllow
	}
	a := NewApproval("ask", ask)
	ctx, cancel := context.WithCancel(context.Background())
	got := a.Gate(ctx, "bash", map[string]any{"command": "rm x"})
	if got != "" {
		t.Fatalf("Gate with live ctx = %q, want \"\"", got)
	}
	cancel()
	got = a.Gate(ctx, "bash", map[string]any{"command": "rm x"})
	if !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("Gate with cancelled ctx = %q, want refusal", got)
	}
}

func TestSetModeHotSwitch(t *testing.T) {
	ask, seen := scriptedAsk([]Answer{AnswerAllow, AnswerAllow})
	a := NewApproval("ask", ask)
	ctx := context.Background()

	if got := a.Gate(ctx, "write", map[string]any{"path": "a"}); got != "" {
		t.Fatalf("Gate(write) denied: %q", got)
	}
	a.SetMode("always")
	if got := a.Gate(ctx, "write", map[string]any{"path": "b"}); got != "" {
		t.Fatalf("Gate(write) in always denied: %q", got)
	}
	a.SetMode("never") // never refuses without asking
	if got := a.Gate(ctx, "write", map[string]any{"path": "c2"}); !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("Gate(write) in never = %q, want refusal text", got)
	}
	if got := a.Gate(ctx, "bash", map[string]any{"command": "ls -la"}); got != "" {
		t.Fatalf("Gate(ls -la) in never = %q, want read-only to pass", got)
	}
	if a.Mode() != "never" {
		t.Fatalf("Mode() = %q, want never", a.Mode())
	}
	a.SetMode("ask") // back to ask: rules persist across the switch
	if got := a.Gate(ctx, "edit", map[string]any{"path": "c"}); got != "" {
		t.Fatalf("Gate(edit) back in ask = %q, want \"\" (script allow)", got)
	}
	a.SetMode("nonsense") // unknown values fall back to ask
	if got := a.Gate(ctx, "write", map[string]any{"path": "d"}); !strings.Contains(got, "[用户拒绝了该操作]") {
		t.Fatalf("Gate(write) with nonsense mode = %q, want refusal", got)
	}
	if len(*seen) != 3 {
		t.Fatalf("ask called %d times, want 3", len(*seen))
	}
}

func TestBashLooksSafeExtraHead(t *testing.T) {
	a := NewApproval("ask", nil)
	ctx := context.Background()
	if got := a.Gate(ctx, "bash", map[string]any{"command": "kubectl get pods"}); got == "" {
		t.Fatal("Gate(kubectl get pods) allowed before SetSafeExtra")
	}
	a.SetSafeExtra([]string{"kubectl"})
	if got := a.Gate(ctx, "bash", map[string]any{"command": "kubectl get pods"}); got != "" {
		t.Fatalf("Gate(kubectl get pods) denied after SetSafeExtra: %q", got)
	}
	// The extra head never widens the shape rules.
	if got := a.Gate(ctx, "bash", map[string]any{"command": "kubectl get pods; rm x"}); got == "" {
		t.Fatal("Gate(kubectl …; rm x) allowed — metachars must still ask")
	}
}
