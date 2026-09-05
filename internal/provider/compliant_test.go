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
// system run is merged into one system message, later systems are
// demoted to marked user messages.
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

func TestCompliantSystemDemotesMidArray(t *testing.T) {
	in := []ChatMessage{
		{Role: "system", Content: "cwd: /x"},
		{Role: "system", Content: "instructions"},
		{Role: "user", Content: "first prompt"},
		{Role: "system", Content: "$ cmd\nout\n---"}, // shell event
		{Role: "assistant", Content: "reply"},
		{Role: "system", Content: "[tool] exit 0"}, // tool record
		{Role: "user", Content: "second prompt"},
	}
	out := compliantSystem(in)
	wantRole := []string{"system", "user", "user", "assistant", "user", "user"}
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
	if !strings.HasPrefix(out[2].Content, nonSystemLeadMarker) ||
		!strings.Contains(out[2].Content, "$ cmd") {
		t.Fatalf("demoted shell event lost its marker or content: %q", out[2].Content)
	}
	if out[4].Content != nonSystemLeadMarker+"\n[tool] exit 0" {
		t.Fatalf("demoted tool record = %q", out[4].Content)
	}
}

func TestCompliantSystemSystemOnlyAfterNonSystem(t *testing.T) {
	// No leading system at all: a later system is demoted, and no system
	// is invented for the array's start.
	in := []ChatMessage{
		{Role: "user", Content: "first prompt"},
		{Role: "system", Content: "$ cmd"},
	}
	out := compliantSystem(in)
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "user" {
		t.Fatalf("got %+v", out)
	}
	if !strings.HasPrefix(out[1].Content, nonSystemLeadMarker) {
		t.Fatalf("demoted content = %q", out[1].Content)
	}
}

func TestCompliantSystemAllSystem(t *testing.T) {
	// No non-system message: everything merges into one leading system.
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
			t.Fatalf("spec-clean input changed at %d: %+v", i, out[i])
		}
	}
}
