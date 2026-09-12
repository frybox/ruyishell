package provider

import (
	"strings"
	"testing"
)

// Strictest OpenAI-compatible endpoints (sglang and peers) accept exactly
// one system message, at the start — a 400 "System message must be at the
// beginning" on two leading systems or on any system after the first
// non-system message (measured against sglang). compliantSystem is the
// single outbound rewrite that keeps every endpoint working: the leading
// system run is merged into one system message, and every role:"shell"
// message (a terminal shell event) is folded into the next user message by
// MergeContextIntoNextUser, so the context keeps its timeline position
// without a system message appearing anywhere but index 0. role:"tool"
// messages are real protocol messages (chat completions' role:"tool" with
// a tool_call_id) and pass through unchanged.
func TestCompliantSystemMergesLeadingRun(t *testing.T) {
	in := []ChatMessage{
		{Role: "system", Content: "cwd: /x"},
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "first prompt"},
	}
	out := compliantSystem(in)
	if len(out) != 2 {
		t.Fatalf("merged leading run to %d messages, want 2: %+v", len(out), out)
	}
	if out[0].Role != "system" ||
		out[0].Content != "cwd: /x\n\ninstructions" {
		t.Fatalf("merged leading system = %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "first prompt" {
		t.Fatalf("first user message changed: %+v", out[1])
	}
	// The caller's slice is never mutated.
	if in[0].Content != "cwd: /x" || in[1].Role != "system" {
		t.Fatal("compliantSystem mutated its input")
	}
}

func TestCompliantSystemFoldsShellIntoNextUser(t *testing.T) {
	in := []ChatMessage{
		{Role: "system", Content: "cwd: /x"},
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "first prompt"},
		{Role: "shell", Content: "$ cmd\nout\n---"}, // shell event
		{Role: "assistant", Content: "reply"},
		{Role: "tool", Content: "exit 0", ToolCallID: "call_1"}, // tool record
		{Role: "user", Content: "second prompt"},
	}
	out := compliantSystem(in)
	wantRole := []string{"system", "user", "assistant", "tool", "user"}
	if len(out) != len(wantRole) {
		t.Fatalf("output has %d messages, want %d: %+v", len(out), len(wantRole), out)
	}
	for i := range wantRole {
		if out[i].Role != wantRole[i] {
			t.Fatalf("role[%d] = %s, want %s", i, out[i].Role, wantRole[i])
		}
	}
	if out[0].Content != "cwd: /x\n\ninstructions" {
		t.Fatal("leading system run must merge verbatim")
	}
	// "first prompt" has no preceding shell message: untouched.
	if out[1].Content != "first prompt" {
		t.Fatalf("first user message = %q, want verbatim", out[1].Content)
	}
	// The shell event folds into the next user message ("second prompt"),
	// in timeline order, under one lead line.
	want := contextLeadLine + "\n$ cmd\nout\n---\n\nsecond prompt"
	if out[4].Content != want {
		t.Fatalf("folded user message = %q, want %q", out[4].Content, want)
	}
	// The tool record is a real protocol message: it is not folded, it
	// keeps its role and its tool_call_id.
	if out[3].Role != "tool" || out[3].ToolCallID != "call_1" || out[3].Content != "exit 0" {
		t.Fatalf("tool record mangled: %+v", out[3])
	}
}

func TestCompliantSystemConsecutiveShellFold(t *testing.T) {
	// Two consecutive shell messages (no user between them) fold into the
	// same following user message, under one lead line, in order.
	in := []ChatMessage{
		{Role: "user", Content: "q"},
		{Role: "shell", Content: "$ a"},
		{Role: "shell", Content: "$ b"},
		{Role: "user", Content: "next"},
	}
	out := compliantSystem(in)
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "user" {
		t.Fatalf("got %+v", out)
	}
	if out[0].Content != "q" {
		t.Fatalf("first user changed = %q", out[0].Content)
	}
	want := contextLeadLine + "\n$ a\n$ b\n\nnext"
	if out[1].Content != want {
		t.Fatalf("folded = %q, want %q", out[1].Content, want)
	}
}

func TestCompliantSystemShellOnlyAfterNonSystem(t *testing.T) {
	// A shell message with no following user message takes the defensive
	// path: it is kept as its own marked user message (not dropped).
	in := []ChatMessage{
		{Role: "user", Content: "first prompt"},
		{Role: "shell", Content: "$ cmd"},
	}
	out := compliantSystem(in)
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "user" {
		t.Fatalf("got %+v", out)
	}
	if !strings.HasPrefix(out[1].Content, contextLeadLine) || !strings.Contains(out[1].Content, "$ cmd") {
		t.Fatalf("defensive content = %q", out[1].Content)
	}
}

func TestCompliantSystemLeadingRunAndShell(t *testing.T) {
	in := []ChatMessage{
		{Role: "system", Content: "cwd: /x"},
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "first prompt"},
		{Role: "shell", Content: "$ cmd\nout\n---"},
		{Role: "user", Content: "second prompt"},
	}
	out := compliantSystem(in)
	if len(out) != 3 {
		t.Fatalf("output has %d messages, want 3: %+v", len(out), out)
	}
	if out[0].Role != "system" || out[0].Content != "cwd: /x\n\ninstructions" {
		t.Fatalf("leading system run = %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "first prompt" {
		t.Fatalf("first user = %+v", out[1])
	}
	want := contextLeadLine + "\n$ cmd\nout\n---\n\nsecond prompt"
	if out[2].Role != "user" || out[2].Content != want {
		t.Fatalf("second user = %+v, want %q", out[2], want)
	}
}

func TestCompliantSystemShellBeforeFirstUserFoldsIn(t *testing.T) {
	// A shell event before the first user message is that user's context:
	// it folds into the first user message rather than becoming a system
	// or a standalone wire message.
	in := []ChatMessage{
		{Role: "system", Content: "cwd: /x"},
		{Role: "system", Content: "instructions"},
		{Role: "shell", Content: "$ ls\nout\n---"},
		{Role: "user", Content: "what is this"},
	}
	out := compliantSystem(in)
	if len(out) != 2 {
		t.Fatalf("output has %d messages, want 2: %+v", len(out), out)
	}
	if out[0].Role != "system" || out[0].Content != "cwd: /x\n\ninstructions" {
		t.Fatalf("leading system run = %+v", out[0])
	}
	want := contextLeadLine + "\n$ ls\nout\n---\n\nwhat is this"
	if out[1].Role != "user" || out[1].Content != want {
		t.Fatalf("first user = %+v, want %q", out[1], want)
	}
}

func TestCompliantSystemAllSystem(t *testing.T) {
	// No non-system message: everything is the leading run, merges to one.
	in := []ChatMessage{{Role: "system", Content: "a"}, {Role: "system", Content: "b"}}
	out := compliantSystem(in)
	if len(out) != 1 || out[0].Role != "system" || out[0].Content != "a\n\nb" {
		t.Fatalf("got %+v", out)
	}
}

func TestCompliantSystemNoOpOnCleanInput(t *testing.T) {
	in := []ChatMessage{
		{Role: "system", Content: "a"},
		{Role: "user", Content: "c"},
	}
	out := compliantSystem(in)
	for i := range out {
		if out[i].Role != in[i].Role || out[i].Content != in[i].Content {
			t.Fatalf("spec-clean input changed at %d: %+v", i, out)
		}
	}
}
