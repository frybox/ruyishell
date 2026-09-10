package provider

import "strings"

// contextLeadLine marks context that rysh auto-captured (a terminal shell
// event or a tool-call record) and folded into the next user message, so
// the timeline position survives on the wire while the message stays a
// user message.
const contextLeadLine = "〔rysh 上下文：以下为自动捕获的终端命令执行与工具调用记录，供理解上下文〕"

// MergeContextIntoNextUser folds each mid-array system message (a shell
// event or a tool record) into the content of the NEXT user message, so
// the context keeps the timeline position it had internally while the wire
// message stays a user message. Leading systems (the run before the first
// non-system message) are passed through untouched — the providers place
// those natively (one leading system for OpenAI, the top-level system
// field for Anthropic, the instructions field for Responses).
//
// The next user message becomes:
//
//	<contextLeadLine>\n<buffered system contents, in order>\n\n<original user content>
//
// If a run of mid-array systems is never followed by a user message
// (defensive: not produced by the current request shapes), it is kept as
// its own marked user message rather than dropped. The caller's slice is
// never mutated.
func MergeContextIntoNextUser(msgs []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	seenNonSystem := false
	var buf []string
	for _, m := range msgs {
		switch {
		case !seenNonSystem && m.Role == "system":
			// Leading system run: pass through unchanged.
			out = append(out, m)
		case m.Role == "system":
			// Mid-array system (shell event / tool record): buffer it; it is
			// folded into the next user message below.
			buf = append(buf, m.Content)
			seenNonSystem = true
		default:
			if m.Role == "user" && len(buf) > 0 {
				preface := contextLeadLine + "\n" + strings.Join(buf, "\n") + "\n\n"
				buf = nil
				out = append(out, ChatMessage{Role: "user", Content: preface + m.Content})
			} else {
				out = append(out, m)
			}
			seenNonSystem = true
		}
	}
	if len(buf) > 0 {
		// Defensive: these mid-array systems never get a following user
		// message — keep them as their own marked user message.
		out = append(out, ChatMessage{Role: "user", Content: contextLeadLine + "\n" + strings.Join(buf, "\n")})
	}
	return out
}
