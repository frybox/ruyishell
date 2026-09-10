package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ChatMessage is one message in a chat conversation.
type ChatMessage struct {
	Role    string `json:"role"` // "system", "user", "assistant" or "tool"
	Content string `json:"content"`
	// ToolCalls is set on assistant messages that requested tool calls
	// (native function calling); each is answered by a role:"tool" message
	// carrying the matching ToolCallID.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on role:"tool" messages and names the tool call this
	// message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// OpenAIClient streams chat completions from an OpenAI-compatible endpoint
// (api = "openai-completions").
type OpenAIClient struct {
	baseURL string
	apiKey  string
	headers map[string]string
	model   string
	client  *http.Client
}

// NewOpenAIClient returns a client bound to the given spec.
func NewOpenAIClient(spec *Spec) *OpenAIClient {
	return &OpenAIClient{
		baseURL: strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:  spec.APIKey,
		headers: spec.Headers,
		model:   spec.Model.ID,
		client:  &http.Client{},
	}
}

// chatRequest is the request body sent to /chat/completions.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []ChatMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions carries the stream_options request field.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatOptions carries the per-call knobs beyond the message list.
type ChatOptions struct {
	// Tools advertises native function-calling tools to the model. nil/empty
	// sends no tools field (plain chat — the model then answers without
	// tools, e.g. the one-shot Q&A path).
	Tools []ToolDef
	// IncludeUsage requests the final usage chunk
	// (stream_options.include_usage); callers doing token accounting need it.
	IncludeUsage bool
}

// ChatOption customizes one ChatStream call.
type ChatOption func(*ChatOptions)

// WithTools advertises native function-calling tools to the model.
func WithTools(defs []ToolDef) ChatOption {
	return func(o *ChatOptions) { o.Tools = defs }
}

// WithUsage requests the final usage chunk (stream_options.include_usage).
func WithUsage() ChatOption {
	return func(o *ChatOptions) { o.IncludeUsage = true }
}

// StreamToken is one token delta from a streaming chat response.
type StreamToken struct {
	// Reasoning marks a reasoning_content delta (extended thinking) rather
	// than the visible reply content. Reasoning tokens surface before the
	// reply (deepseek-v4-flash streams a long silent thinking phase).
	Reasoning bool
	// Text is the token text.
	Text string
	// ToolCall is set on a fully assembled tool call (native function
	// calling): streamed argument deltas folded into one call, emitted in
	// index order. Text is empty on tool-call tokens.
	ToolCall *ToolCall
	// Usage is set on the final usage token, after all other tokens.
	Usage *Usage
	// Err is set on a terminal token when the stream itself broke (read
	// error, truncated connection) after the response was already 200.
	// The engine retries a round whose stream broke before its first
	// visible token.
	Err error
}

// StatusError is a non-2xx chat-completions response with its status code
// intact, so callers can classify retries (429/5xx back off, other codes
// are fatal). RetryAfter carries a parsed Retry-After header (seconds
// form) when the endpoint sent one.
type StatusError struct {
	Code       int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("chat completions: %s: %s", e.Status, e.Body)
}

// deltaToolCall is one fragment of a streamed tool call. The id arrives on
// the first fragment of each index; name and arguments stream in
// incrementally. OpenAI nests them under "function", but some compatible
// endpoints emit them at the top level; both are captured and the caller
// prefers the nested shape, falling back to the top level.
type deltaToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatChunk is one SSE data payload from a streaming response.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []deltaToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// compliantSystem rewrites the request for the strictest
// OpenAI-compatible endpoints: servers that enforce the chat spec (sglang
// and peers) answer any request whose system messages are not exactly one
// and at the start with a 400 "System message must be at the beginning" —
// even a leading *run* of systems is rejected (measured: sglang accepts
// one leading system, 400s on two). Lenient endpoints (DeepSeek, OpenAI)
// accept either shape. rysh keeps multiple systems in its internal view
// on purpose — cwd, env and the instructions lead every request, and
// shell events and tool results sit in the timeline where they happened
// (main.go base build, reconstructHistory) — so the wire shape is
// enforced here, at the single outbound point: mid-array systems are
// folded into the next user message (MergeContextIntoNextUser) and the
// leading system run is merged into one system message. The caller's
// slice is never mutated.
func compliantSystem(msgs []ChatMessage) []ChatMessage {
	var lead []string
	out := make([]ChatMessage, 0, len(msgs)+1)
	leadRun := true
	for _, m := range MergeContextIntoNextUser(msgs) {
		if m.Role == "system" && leadRun {
			lead = append(lead, m.Content)
			continue
		}
		leadRun = false
		if len(lead) > 0 {
			out = append(out, ChatMessage{Role: "system", Content: strings.Join(lead, "\n\n")})
			lead = nil
		}
		out = append(out, m)
	}
	if len(lead) > 0 {
		out = append(out, ChatMessage{Role: "system", Content: strings.Join(lead, "\n\n")})
	}
	return out
}

// ChatStream posts the messages and returns a channel of stream tokens
// (reasoning and content deltas, assembled tool calls, final usage — in
// arrival order). The channel is closed when the stream ends; a non-2xx
// response or a malformed stream aborts with the returned error. A 400 that
// reads as a tools rejection returns a *ToolsUnsupportedError so callers
// can surface the fatal "model has no function calling" error.
func (c *OpenAIClient) ChatStream(ctx context.Context, messages []ChatMessage, opts ...ChatOption) (<-chan StreamToken, error) {
	o := ChatOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	messages = compliantSystem(messages)
	// Keep the plain-chat request body identical to the pre-tools wire shape:
	// no tools field, no stream_options unless explicitly requested.
	reqBody := chatRequest{Model: c.model, Messages: messages, Stream: true, Tools: o.Tools}
	if o.IncludeUsage {
		reqBody.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(b))
		// Overflow first: its phrases match more precisely than the
		// loose tools check, and the engine answers it with a
		// compaction + retry, not the fence fallback.
		if resp.StatusCode == http.StatusBadRequest && isContextOverflow(msg) {
			return nil, &OverflowError{Status: resp.Status, Body: msg}
		}
		if resp.StatusCode == http.StatusBadRequest && isToolsRejection(msg) {
			return nil, &ToolsUnsupportedError{Status: resp.Status, Body: msg}
		}
		se := &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: msg}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				se.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, se
	}

	ch := make(chan StreamToken)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		acc := newToolCallAccumulator()
		emit := func(tok StreamToken) bool {
			select {
			case ch <- tok:
				return true
			case <-ctx.Done():
				return false
			}
		}
		flushTools := func() bool {
			for _, tc := range acc.take() {
				if !emit(StreamToken{ToolCall: &tc}) {
					return false
				}
			}
			return true
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		var lastUsage *Usage
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var chunk chatChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				break
			}
			for _, choice := range chunk.Choices {
				delta := choice.Delta
				if delta.ReasoningContent != "" {
					if !emit(StreamToken{Reasoning: true, Text: delta.ReasoningContent}) {
						return
					}
				}
				if delta.Content != "" {
					if !emit(StreamToken{Text: delta.Content}) {
						return
					}
				}
				for _, dtc := range delta.ToolCalls {
					// Prefer the OpenAI-nested function.name/arguments,
					// fall back to top-level for compatible endpoints that
					// flatten the shape.
					name := dtc.Function.Name
					if name == "" {
						name = dtc.Name
					}
					args := dtc.Function.Arguments
					if args == "" {
						args = dtc.Arguments
					}
					acc.add(dtc.Index, dtc.ID, name, args)
				}
			}
			// Buffer usage; emit it last (after the assembled tool
			// calls) at stream end. Some endpoints attach a usage object
			// to every chunk, so flushing tools on each chunk would drain
			// the accumulator incrementally and emit partial tool-call
			// fragments as separate calls.
			if chunk.Usage != nil {
				lastUsage = chunk.Usage
			}
		}
		// Stream end: emit the fully assembled tool calls, then usage
		// last, then surface a broken stream (read error mid-body) as a
		// terminal Err token — unless ^C caused it, which is no error at
		// all.
		flushTools()
		if lastUsage != nil {
			if !emit(StreamToken{Usage: lastUsage}) {
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			emit(StreamToken{Err: err})
		}
	}()
	return ch, nil
}
