package main

import (
	"strconv"
	"testing"

	"ruyishell/internal/agent"
	"ruyishell/internal/provider"
)

// pairingHistory is a multi-prompt conversation of the exact shape the
// driver's active.hist carries: each prompt is one user message followed
// by its rounds (assistant-with-calls, then one tool result per call, in
// order). Multiple user messages let trimHistory drop whole turns, so the
// survivor slice is a non-trivial subset to check.
func pairingHistory(t *testing.T) []ctxMsg {
	t.Helper()
	hist := []ctxMsg{}
	for _, p := range []string{"p1", "p2", "p3"} {
		hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: p}}})
		calls := []provider.ToolCall{
			{ID: p + "a", Name: "read", Arguments: `{"path":"a"}`},
			{ID: p + "b", Name: "bash", Arguments: `{"command":"ls"}`},
		}
		hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "assistant", Content: p + " works", ToolCalls: calls}}})
		for _, c := range calls {
			hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "tool", Content: "[" + c.Name + "] ok", ToolCallID: c.ID}}})
		}
	}
	hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "assistant", Content: "final answer"}}})
	return hist
}

// assertTrimmedPairing checks the trim-side half of the invariant: the
// survivor slice starts on a whole user-message boundary (never mid-round)
// and is pairing-balanced — no orphan tool result, no unclosed call,
// nothing splitting a pair.
func assertTrimmedPairing(t *testing.T, name string, h []ctxMsg) {
	t.Helper()
	if len(h) == 0 {
		return
	}
	if h[0].Msg.Role != "user" {
		t.Fatalf("%s: survivor slice starts on a %s message, not a whole turn", name, h[0].Msg.Role)
	}
	var open []string
	for i, cm := range h {
		m := cm.Msg
		switch m.Role {
		case "assistant":
			for _, c := range m.ToolCalls {
				open = append(open, c.ID)
			}
		case "tool":
			idx := -1
			for j, id := range open {
				if id == m.ToolCallID {
					idx = j
					break
				}
			}
			if idx < 0 {
				t.Fatalf("%s[%d]: orphan tool result %q (no open call answers it)", name, i, m.ToolCallID)
			}
			open = append(open[:idx], open[idx+1:]...)
		default:
			if len(open) > 0 {
				t.Fatalf("%s[%d]: %s message splits open call %s from its result", name, i, m.Role, open[len(open)-1])
			}
		}
	}
	if len(open) > 0 {
		t.Fatalf("%s: history ends with %d unclosed tool call(s)", name, len(open))
	}
}

// TestTrimHistoryKeepsToolPairs locks the trim-side pairing invariant:
// trimming to every cap keeps the survivor slice whole-turn and balanced —
// no tool result outlives its assistant call request or vice versa.
func TestTrimHistoryKeepsToolPairs(t *testing.T) {
	hist := pairingHistory(t)
	assertTrimmedPairing(t, "full", hist)
	for max := 1; max <= len(hist); max++ {
		trimmed := trimHistory(hist, max)
		assertTrimmedPairing(t, "max="+strconv.Itoa(max), trimmed)
		if len(trimmed) > max {
			t.Fatalf("max=%d: trimHistory kept %d messages", max, len(trimmed))
		}
	}
}
