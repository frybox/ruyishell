package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ResponsesClient streams from an OpenAI Responses API endpoint
// (api = "openai-responses"). The Responses API is a newer, flatter variant of
// chat completions: system prompt goes in the top-level "instructions"
// field, the conversation is a flat "input" array of message / function_call
// / function_call_output items, and streaming emits named events (text
// deltas, output_item.done for complete tool calls, a terminal
// response.completed carrying usage).
type ResponsesClient struct {
	baseURL string
	apiKey  string
	headers map[string]string
	model   string
	maxOut  int
	client  *http.Client
}

// NewResponsesClient returns a client bound to the given spec.
func NewResponsesClient(spec *Spec) *ResponsesClient {
	return &ResponsesClient{
		baseURL: strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:  spec.APIKey,
		headers: spec.Headers,
		model:   spec.Model.ID,
		maxOut:  spec.MaxTokens,
		client:  &http.Client{},
	}
}

// respTool is one Responses-API tool (type "function", name/description/
// parameters at top level — flatter than the chat-completions shape).
type respTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// respMessage is a plain user/assistant text item in the input array.
type respMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// respFunctionCallItem is one tool call the model made earlier in the
// conversation (input history).
type respFunctionCallItem struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// respFunctionCallOutputItem is one tool result fed back to the model.
type respFunctionCallOutputItem struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesRequest is the request body sent to /responses.
type responsesRequest struct {
	Model           string     `json:"model"`
	Instructions    string     `json:"instructions,omitempty"`
	Input           []any      `json:"input"`
	Tools           []respTool `json:"tools,omitempty"`
	Stream          bool       `json:"stream"`
	MaxOutputTokens int        `json:"max_output_tokens,omitempty"`
}

// respEvent is one SSE data payload from a Responses streaming response. Only
// the fields each event type carries are read; the rest stay zero.
type respEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"` // response.output_text.delta
	Item  struct {
		Type      string `json:"type"` // "message" or "function_call"
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"` // response.output_item.done
	Response struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"` // response.completed / response.incomplete
	} `json:"response"`
}

// buildInput flattens the internal timeline into the Responses input array
// (systems lifted into instructions, assistant text + its tool calls as
// separate items, tool results as function_call_output items) in timeline
// order.
func buildResponsesInput(msgs []ChatMessage) (instructions string, input []any) {
	var sys []string
	input = []any{}
	for _, m := range msgs {
		switch m.Role {
		case "system":
			sys = append(sys, m.Content)
		case "user":
			input = append(input, respMessage{Role: "user", Content: m.Content})
		case "assistant":
			if m.Content != "" {
				input = append(input, respMessage{Role: "assistant", Content: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input = append(input, respFunctionCallItem{
					Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
				})
			}
		case "tool":
			input = append(input, respFunctionCallOutputItem{
				Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content,
			})
		}
	}
	instructions = strings.Join(sys, "\n\n")
	return
}

// ChatStream posts the messages to /responses and returns a channel of
// stream tokens (content deltas, complete tool calls, final usage). The
// channel is closed when the stream ends; a non-2xx response or a malformed
// stream aborts with the returned error.
func (c *ResponsesClient) ChatStream(ctx context.Context, messages []ChatMessage, opts ...ChatOption) (<-chan StreamToken, error) {
	o := ChatOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	// Fold role:"shell" events (terminal shell events) into the next user
	// message so they keep their timeline position on the wire; only the
	// leading system run reaches buildResponsesInput, which lifts it into
	// the instructions field.
	messages = MergeContextIntoNextUser(messages)
	instructions, input := buildResponsesInput(messages)
	var tools []respTool
	for _, d := range o.Tools {
		tools = append(tools, respTool{Type: "function", Name: d.Name, Description: d.Description, Parameters: d.Parameters})
	}
	reqBody := responsesRequest{
		Model:           c.model,
		Instructions:    instructions,
		Input:           input,
		Tools:           tools,
		Stream:          true,
		MaxOutputTokens: c.maxOut,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(body))
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
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
	stream:
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var ev respEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta":
				if ev.Delta != "" {
					select {
					case ch <- StreamToken{Text: ev.Delta}:
					case <-ctx.Done():
						return
					}
				}
			case "response.output_item.done":
				// Tool calls arrive complete on this event; text message
				// items were already streamed via deltas, so skip them.
				if ev.Item.Type == "function_call" {
					tc := ToolCall{ID: ev.Item.CallID, Name: ev.Item.Name, Arguments: ev.Item.Arguments}
					select {
					case ch <- StreamToken{ToolCall: &tc}:
					case <-ctx.Done():
						return
					}
				}
			case "response.completed", "response.incomplete":
				if ev.Response.Usage.InputTokens != 0 || ev.Response.Usage.OutputTokens != 0 || ev.Response.Usage.TotalTokens != 0 {
					lastUsage = &Usage{
						PromptTokens:     ev.Response.Usage.InputTokens,
						CompletionTokens: ev.Response.Usage.OutputTokens,
						TotalTokens:      ev.Response.Usage.TotalTokens,
					}
					if lastUsage.TotalTokens == 0 {
						lastUsage.TotalTokens = lastUsage.PromptTokens + lastUsage.CompletionTokens
					}
				}
				// Terminal event: the stream is over (no [DONE] sentinel).
				break stream
			}
		}
		if lastUsage != nil {
			select {
			case ch <- StreamToken{Usage: lastUsage}:
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			select {
			case ch <- StreamToken{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}
