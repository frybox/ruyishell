package agent

// L1 context slimming (architecture §10.2, ported from the pre-engine
// cmd/rysh/loopguard.go): when assembling a request, the newest tool
// results that fit inside a byte budget stay verbatim; older ones collapse
// to one-line placeholders. turn itself is never modified — only the
// request view is.

import (
	"fmt"
	"strings"

	"ruyishell/internal/provider"
)

// l1PreserveBytes keeps this much of the newest tool results verbatim when
// assembling a request; older ones collapse to placeholders.
const l1PreserveBytes = 64 * 1024

// TurnMsg is one message of a task turn with the metadata the L1 view and
// the loop guards need. Tool is set only on tool-result messages.
type TurnMsg struct {
	Msg  provider.ChatMessage
	Tool *ToolMeta
}

// ToolMeta describes one executed tool call for L1 slimming: the display
// line (for bash, the squashed command), its exit code, and the size of
// the captured output.
type ToolMeta struct {
	Cmd  string
	Exit int
	Size int // captured output size in bytes
}

// CollapseOldTools returns a copy of turn in which the newest tool results
// fitting inside l1PreserveBytes stay verbatim; once that tail budget
// overflows, every older tool result becomes a one-line placeholder.
// Non-tool messages are untouched, and turn itself is never modified.
func CollapseOldTools(turn []TurnMsg) []TurnMsg {
	fold := make([]bool, len(turn))
	kept := 0
	overflowed := false // set once the kept tail exceeds the budget
	for i := len(turn) - 1; i >= 0; i-- {
		t := turn[i].Tool
		if t == nil {
			continue
		}
		if overflowed || (kept > 0 && kept+t.Size > l1PreserveBytes) {
			// Older than the budgeted tail (or the tail itself just
			// overflowed): everything below the newest fit collapses.
			overflowed = true
			fold[i] = true
			continue
		}
		kept += t.Size
	}
	out := append([]TurnMsg(nil), turn...)
	for i := range out {
		if !fold[i] {
			continue
		}
		t := out[i].Tool
		disp := strings.Join(strings.Fields(t.Cmd), " ")
		out[i].Msg.Content = fmt.Sprintf("[已省略] $ %s (exit %d)，原始输出 %s 已折叠；如需详情可重新运行该命令",
			disp, t.Exit, humanSize(t.Size))
	}
	return out
}

// humanSize formats a byte count as B or KB for the placeholder line.
func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}
