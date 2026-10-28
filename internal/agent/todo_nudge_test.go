package agent

// The todo end-gate (§16 part 2): a run must not end its final answer while
// its own todo list still has unfinished items — the first text-only round
// gets one nudge, the second passes through. A fully-completed list and a
// run that never planned end immediately (no false positive).

import (
	"context"
	"strings"
	"testing"

	"ruyishell/internal/provider"
)

// nudgePresent reports whether the request carries the one-shot todo nudge
// (a user message whose content is exactly the nudge text).
func nudgePresent(req []provider.ChatMessage) bool {
	for _, m := range req {
		if m.Role == "user" && m.Content == todoNudge {
			return true
		}
	}
	return false
}

// sinkHasTodoNudge reports whether the run surfaced the todo nudge notice.
func sinkHasTodoNudge(sink *recSink) bool {
	for _, n := range sink.notices {
		if strings.Contains(n, "todo") {
			return true
		}
	}
	return false
}

func TestRunTodoNudgeWhileUnfinished(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		// round 1: the model plans with one unfinished item.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{
				ID: "c1", Name: "todo",
				Arguments: `{"todos":[{"id":"1","content":"do the work","status":"pending"}]}`,
			}}, nil, nil
		},
		// round 2: the model tries to end — this is the round the engine
		// vetoes (unfinished todo) and injects the nudge after.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "done", nil, nil, nil
		},
		// round 3: after the nudge the model ends again — this passes.
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "really done", nil, nil, nil
		},
	)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "go",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 3 {
		t.Fatalf("Steps = %d, want 3 (plan, vetoed end, final)", res.Steps)
	}
	reqs := mc.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	if nudgePresent(reqs[1]) {
		t.Fatalf("nudge must not be in the second request (it is injected after)")
	}
	if !nudgePresent(reqs[2]) {
		t.Fatalf("nudge missing from the third request (the round after the veto)")
	}
	if !sinkHasTodoNudge(sink) {
		t.Fatalf("no todo nudge notice surfaced to the sink")
	}
	if res.Messages[len(res.Messages)-1].Msg.Content != "really done" {
		t.Fatalf("final message = %q, want the round after the nudge", res.Messages[len(res.Messages)-1].Msg.Content)
	}
}

func TestRunTodoAllCompletedEndsDirectly(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{
				ID: "c1", Name: "todo",
				Arguments: `{"todos":[{"id":"1","content":"done","status":"completed"}]}`,
			}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "done", nil, nil, nil
		},
	)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "go",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 2 {
		t.Fatalf("Steps = %d, want 2 (plan, final — a done list ends immediately)", res.Steps)
	}
	if got := mc.requests()[1]; nudgePresent(got) {
		t.Fatalf("nudge must not fire for a fully-completed todo list")
	}
	if sinkHasTodoNudge(sink) {
		t.Fatalf("no todo nudge notice expected for a fully-completed list")
	}
}

func TestRunNoTodoNoNudge(t *testing.T) {
	mc := &mockClient{t: t}
	mc.rounds = append(mc.rounds,
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "", []provider.ToolCall{{
				ID: "c1", Name: "bash",
				Arguments: `{"command":"echo hi"}`,
			}}, nil, nil
		},
		func(n int, req []provider.ChatMessage, o *provider.ChatOptions) (string, []provider.ToolCall, *provider.Usage, error) {
			return "done", nil, nil, nil
		},
	)
	sink := &recSink{}
	res := Run(context.Background(), Input{
		Client: mc, NativeTools: true, Prompt: "go",
		Cfg: fastCfg(t.TempDir()),
	}, sink)

	if res.Err != nil {
		t.Fatalf("run failed: %v", res.Err)
	}
	if res.Steps != 2 {
		t.Fatalf("Steps = %d, want 2 (bash, final — a run that never planned ends immediately)", res.Steps)
	}
	if sinkHasTodoNudge(sink) {
		t.Fatalf("a run that never used todo must not get the todo nudge")
	}
	if got := mc.requests()[1]; nudgePresent(got) {
		t.Fatalf("nudge must not fire when the todo list was never created")
	}
}
