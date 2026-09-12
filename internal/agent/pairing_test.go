package agent

import (
	"strconv"
	"testing"

	"ruyishell/internal/provider"
)

// pairingInvariant asserts a message slice is wire-legal for native tool
// calling: every tool result answers one of the still-open calls (no
// orphan), no user/system message lands between an open call and its
// result, and no call is left unclosed at the end. This is the compact-side
// half of the "a call request and its result are inseparable" invariant
// (不变式②): a compaction cut that drops a round's assistant message must
// not leave that round's tool results behind.
func pairingInvariant(t *testing.T, name string, view []TurnMsg) {
	t.Helper()
	var open []string // ids of open (declared, not yet answered) calls
	for i, tm := range view {
		m := tm.Msg
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
				t.Fatalf("%s[%d]: %s message splits open call %s from its result",
					name, i, m.Role, open[len(open)-1])
			}
		}
	}
	if len(open) > 0 {
		t.Fatalf("%s: view ends with %d unclosed tool call(s)", name, len(open))
	}
}

// pairingRound is one whole turn: an assistant message with two tool calls
// followed by the two results, in order.
func pairingRound(prefix string) []TurnMsg {
	calls := []provider.ToolCall{
		{ID: prefix + "1", Name: "read", Arguments: `{"path":"a"}`},
		{ID: prefix + "2", Name: "bash", Arguments: `{"command":"ls"}`},
	}
	out := []TurnMsg{{Msg: provider.ChatMessage{Role: "assistant", Content: prefix + " thinks", ToolCalls: calls}}}
	for _, c := range calls {
		out = append(out, TurnMsg{
			Msg:  provider.ChatMessage{Role: "tool", Content: "[" + c.Name + "] ok", ToolCallID: c.ID},
			Tool: &ToolMeta{Cmd: c.Name, Size: 10},
		})
	}
	return out
}

// TestCompactCutKeepsToolPairs locks the compact-side pairing invariant:
// the post-compaction request view stays pairing-balanced at every legal
// cut point, and chooseCut itself never lands on a tool result.
func TestCompactCutKeepsToolPairs(t *testing.T) {
	turn := []TurnMsg{{Msg: provider.ChatMessage{Role: "user", Content: "mission"}}}
	turn = append(turn, pairingRound("r1")...)
	turn = append(turn, pairingRound("r2")...)
	turn = append(turn, pairingRound("r3")...)
	turn = append(turn, TurnMsg{Msg: provider.ChatMessage{Role: "assistant", Content: "final answer"}})

	t.Run("requestViewBalancedAtEveryLegalCut", func(t *testing.T) {
		e := &engine{cfg: Config{ContextWindow: 100000}, turn: turn}
		checked := 0
		for cut := 1; cut < len(turn); cut++ {
			if turn[cut].Msg.Role == "tool" {
				continue // not a legal boundary
			}
			e.compact = &compactState{Checkpoint: "checkpoint", Cut: cut}
			view := e.requestView()
			pairingInvariant(t, "cut "+strconv.Itoa(cut), view)
			if view[0].Msg.Role != "user" || view[0].Msg.Content != "mission" {
				t.Fatalf("cut %d: mission not first verbatim", cut)
			}
			checked++
		}
		if checked == 0 {
			t.Fatal("no legal cut points checked")
		}
	})

	t.Run("chooseCutNeverLandsOnTool", func(t *testing.T) {
		for _, window := range []int{200, 500, 2000, 10000, 100000} {
			e := &engine{cfg: Config{ContextWindow: window}, turn: turn}
			cut := e.chooseCut()
			if cut >= len(turn) {
				continue
			}
			if turn[cut].Msg.Role == "tool" {
				t.Fatalf("window %d: cut %d lands on a tool result", window, cut)
			}
			e.compact = &compactState{Checkpoint: "checkpoint", Cut: cut}
			pairingInvariant(t, "chooseCut window "+strconv.Itoa(window), e.requestView())
		}
	})
}
