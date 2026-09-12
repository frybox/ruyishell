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

// AnthropicClient streams from an Anthropic Messages API endpoint
// (api = "anthropic-messages"). The Messages API differs from the OpenAI
// family in three ways the engine is unaware of: the system prompt is a
// top-level field (not a message), tool results are a distinct message type
// (tool_result) rather than a "tool" role, and the request requires a
// max_tokens cap. Streaming is SSE named events (content_block_delta for
// text, content_block_start/delta for tool calls, message_start/delta for
// usage).
type AnthropicClient struct {
	baseURL   string
	apiKey    string
	headers   map[string]string
	model     string
	maxTokens int
	client    *http.Client
}

// defaultMaxTokens caps Anthropic requests when the model config leaves
// max_tokens unset; the Messages API rejects a request without it.
const defaultMaxTokens = 4096

// NewAnthropicClient returns a client bound to the given spec.
func NewAnthropicClient(spec *Spec) *AnthropicClient {
	mt := spec.MaxTokens
	if mt <= 0 {
		mt = defaultMaxTokens
	}
	return &AnthropicClient{
		baseURL:   strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:    spec.APIKey,
		headers:   spec.Headers,
		model:     spec.Model.ID,
		maxTokens: mt,
		client:    &http.Client{},
	}
}

// anthropicContent is one block in an Anthropic message. A message's content
// is a list of these: text and (for assistant tool calls) tool_use blocks.
type anthropicContent struct {
	Type  string `json:"type"` // "text" or "tool_use"
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`
}

// anthropicToolResult is a tool_result content block fed back to the model.
type anthropicToolResult struct {
	Type      string `json:"type"` // "tool_result"
	ToolUseID string `json:"tool_use_id"`
	Content   any    `json:"content"`
}

// anthropicMessage is one entry in the messages array.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicTool is one tool advertised to the model.
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicRequest is the request body sent to /v1/messages.
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

// anthropicUsage is the Messages-API usage block. It maps to the shared Usage
// (input→prompt, output→completion); a dedicated aux type is needed because
// Usage.UnmarshalJSON reads the OpenAI field names, which Anthropic does not
// send.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (u anthropicUsage) toUsage() *Usage {
	return &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// anthropicEvent is one SSE data payload from an Anthropic streaming response.
type anthropicEvent struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Message struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"` // message_start
	Index *int `json:"index"`
	Block struct {
		Type string `json:"type"` // "tool_use"
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"` // content_block_start
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		JSONPartial string `json:"partial_json"`
	} `json:"delta"` // content_block_delta
	Usage *anthropicUsage `json:"usage"` // message_delta
	// StopReason is set on message_delta.
	StopReason string `json:"stop_reason"`
}

// buildAnthropicMessages flattens the internal timeline into Anthropic
// messages. The internal model is OpenAI-shaped (a "tool" role per result);
// Anthropic instead wants: the system prompt as a top-level field, an
// assistant message whose content is [text?, tool_use, tool_use, …], and the
// results of that assistant's tool calls grouped into ONE following user
// message of tool_result blocks (parallel calls → several tool_result blocks
// in the same message). Consecutive same-role messages (a text assistant
// followed by a tool-call assistant, or wrap-up rounds) are merged so the
// wire stays a strict user/assistant alternation.
func buildAnthropicMessages(msgs []ChatMessage) (system string, messages []anthropicMessage) {
	var sys []string
	var pending []anthropicToolResult
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		messages = append(messages, anthropicMessage{Role: "user", Content: pending})
		pending = nil
	}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sys = append(sys, m.Content)
		case "user":
			flushPending()
			messages = append(messages, anthropicMessage{Role: "user", Content: []anthropicContent{{Type: "text", Text: m.Content}}})
		case "assistant":
			var blocks []anthropicContent
			if m.Content != "" {
				blocks = append(blocks, anthropicContent{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := any(map[string]any{})
				if strings.TrimSpace(tc.Arguments) != "" {
					var v any
					if err := json.Unmarshal([]byte(tc.Arguments), &v); err != nil {
						input = map[string]any{}
					} else {
						input = v
					}
				}
				blocks = append(blocks, anthropicContent{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				continue
			}
			// Group this assistant's tool results before its own turn, so a
			// later assistant round keeps the user/assistant alternation.
			flushPending()
			if n := len(messages); n > 0 && messages[n-1].Role == "assistant" {
				// Merge consecutive assistant messages into one.
				last := messages[n-1].Content.([]anthropicContent)
				messages[n-1].Content = append(last, blocks...)
			} else {
				messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})
			}
		case "tool":
			pending = append(pending, anthropicToolResult{
				Type: "tool_result", ToolUseID: m.ToolCallID,
				Content: []anthropicContent{{Type: "text", Text: m.Content}},
			})
		}
	}
	flushPending()
	system = strings.Join(sys, "\n\n")
	return
}

// ChatStream posts the messages to /v1/messages and returns a channel of
// stream tokens (content deltas, complete tool calls, final usage). The
// channel is closed when the stream ends; a non-2xx response or a malformed
// stream aborts with the returned error.
func (c *AnthropicClient) ChatStream(ctx context.Context, messages []ChatMessage, opts ...ChatOption) (<-chan StreamToken, error) {
	o := ChatOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	// Fold role:"shell" events (terminal shell events) into the next user
	// message so they keep their timeline position on the wire; only the
	// leading system run reaches buildAnthropicMessages, which lifts it
	// into the top-level system field.
	messages = MergeContextIntoNextUser(messages)
	system, msgs := buildAnthropicMessages(messages)
	var tools []anthropicTool
	for _, d := range o.Tools {
		schema := d.Parameters
		if schema == nil {
			schema = map[string]any{}
		}
		tools = append(tools, anthropicTool{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	reqBody := anthropicRequest{
		Model: c.model, System: system, Messages: msgs,
		Tools: tools, MaxTokens: c.maxTokens, Stream: true,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
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
		var lastUsage *Usage
		// toolUse assembles one tool call across its start/delta events,
		// keyed by content-block index (Anthropic streams one block at a
		// time, but index makes the assembly robust).
		type inFlight struct{ ID, Name, Args string }
		tools := map[int]*inFlight{}
		var order []int
		emit := func(tok StreamToken) bool {
			select {
			case ch <- tok:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var ev anthropicEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "message_start":
				if ev.Message.Usage != nil {
					lastUsage = ev.Message.Usage.toUsage()
				}
			case "content_block_start":
				if ev.Block.Type == "tool_use" && ev.Index != nil {
					if _, ok := tools[*ev.Index]; !ok {
						order = append(order, *ev.Index)
					}
					tools[*ev.Index] = &inFlight{ID: ev.Block.ID, Name: ev.Block.Name}
				}
			case "content_block_delta":
				// Two delta shapes share this event type: text_delta (visible
				// text) and input_json_delta (a tool call's arguments JSON,
				// appended incrementally).
				if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
					if !emit(StreamToken{Text: ev.Delta.Text}) {
						return
					}
				} else if ev.Delta.JSONPartial != "" && ev.Index != nil {
					if tf, ok := tools[*ev.Index]; ok {
						tf.Args += ev.Delta.JSONPartial
					}
				}
			case "message_delta":
				// message_start carried the input tokens; message_delta
				// carries the final output/total — merge them.
				if ev.Usage != nil {
					if lastUsage == nil {
						lastUsage = &Usage{}
					}
					lastUsage.CompletionTokens = ev.Usage.OutputTokens
					lastUsage.TotalTokens = lastUsage.PromptTokens + ev.Usage.OutputTokens
				}
			case "error":
				if ev.Error != nil {
					msg := ev.Error.Message
					if ev.Error.Type == "" {
						ev.Error.Type = "api_error"
					}
					if !emit(StreamToken{Err: fmt.Errorf("anthropic: %s: %s", ev.Error.Type, msg)}) {
						return
					}
				}
			}
		}
		// Flush complete tool calls in block order.
		for _, idx := range order {
			tf := tools[idx]
			tc := ToolCall{ID: tf.ID, Name: tf.Name, Arguments: tf.Args}
			if !emit(StreamToken{ToolCall: &tc}) {
				return
			}
		}
		if lastUsage != nil {
			if lastUsage.TotalTokens == 0 {
				lastUsage.TotalTokens = lastUsage.PromptTokens + lastUsage.CompletionTokens
			}
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
