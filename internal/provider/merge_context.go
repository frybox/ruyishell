package provider

import "strings"

// contextLeadLine marks context that rysh auto-captured (a terminal shell
// event) and folded into the next user message, so the timeline position
// survives on the wire while the message stays a user message.
const contextLeadLine = "〔rysh 上下文：以下为自动捕获的终端命令执行记录，供理解上下文〕"

// MergeContextIntoNextUser folds each role:"shell" message (a terminal
// shell event captured by the recorder) into the content of the NEXT user
// message, so the context keeps the timeline position it had internally
// while the wire message stays a user message. The next user message
// becomes:
//
//	<contextLeadLine>\n<buffered shell contents, in order>\n\n<original user content>
//
// role:"system" messages are never folded: they are the true system run
// (cwd, env, instructions) that leads every request and must reach the
// provider as system/instructions. Every shell message is folded
// regardless of position — a shell event before the first user message is
// part of that user's turn context, and mid-array events keep the timeline
// position they happened in. If a run of shell messages is never followed
// by a user message (defensive: not produced by the current request
// shapes), it is kept as its own marked user message rather than dropped.
// The caller's slice is never mutated.
func MergeContextIntoNextUser(msgs []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	var buf []string
	for _, m := range msgs {
		if m.Role == "shell" {
			// Shell event: buffer it; it is folded into the next user
			// message below.
			buf = append(buf, m.Content)
			continue
		}
		if m.Role == "user" && len(buf) > 0 {
			preface := contextLeadLine + "\n" + strings.Join(buf, "\n") + "\n\n"
			buf = nil
			out = append(out, ChatMessage{Role: "user", Content: preface + m.Content})
		} else {
			out = append(out, m)
		}
	}
	if len(buf) > 0 {
		// Defensive: these shell messages never get a following user
		// message — keep them as their own marked user message.
		out = append(out, ChatMessage{Role: "user", Content: contextLeadLine + "\n" + strings.Join(buf, "\n")})
	}
	return out
}
