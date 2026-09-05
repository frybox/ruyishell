package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ToolCall is one function call requested by the model via native tool
// calling. Arguments is the raw JSON object text; it arrives split across
// streamed deltas and is assembled by the SSE reader.
type ToolCall struct {
	ID        string // provider-assigned id, echoed back on the tool result
	Name      string // function name
	Arguments string // raw JSON arguments
}

// wireToolCall is the OpenAI wire shape for a complete tool call (assistant
// tool_calls entries and non-streaming responses). Streaming deltas use a
// different shape with an index and partial fields; the SSE reader handles
// those itself (deltaToolCall in openai.go).
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// MarshalJSON renders the OpenAI function-calling wire shape.
func (t ToolCall) MarshalJSON() ([]byte, error) {
	w := wireToolCall{ID: t.ID, Type: "function"}
	w.Function.Name = t.Name
	w.Function.Arguments = t.Arguments
	return json.Marshal(w)
}

// UnmarshalJSON accepts the OpenAI function-calling wire shape.
func (t *ToolCall) UnmarshalJSON(b []byte) error {
	var w wireToolCall
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	t.ID, t.Name, t.Arguments = w.ID, w.Function.Name, w.Function.Arguments
	return nil
}

// ToolDef is one tool advertised to the model in the request's tools field.
// The engine (M7.2) builds these from its tool registry; the parameters are
// the JSON Schema of the arguments object.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// MarshalJSON renders the OpenAI wire shape
// ({"type":"function","function":{...}}).
func (t ToolDef) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string `json:"type"`
		Function struct {
			Name        string         `json:"name"`
			Description string         `json:"description,omitempty"`
			Parameters  map[string]any `json:"parameters,omitempty"`
		} `json:"function"`
	}{
		Type: "function",
		Function: struct {
			Name        string         `json:"name"`
			Description string         `json:"description,omitempty"`
			Parameters  map[string]any `json:"parameters,omitempty"`
		}{t.Name, t.Description, t.Parameters},
	})
}

// Usage is the token accounting reported by the final usage chunk of a
// streamed response (stream_options.include_usage).
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the OpenAI-line prompt cache hit count
	// (usage.prompt_tokens_details.cached_tokens) when the endpoint
	// reports it; 0 means unknown, not "no cache".
	CachedTokens int `json:"-"`
}

// UnmarshalJSON extends the wire shape with the OpenAI prompt-cache detail
// block (usage.prompt_tokens_details.cached_tokens), which the plain fields
// do not carry.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var aux struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	u.PromptTokens = aux.PromptTokens
	u.CompletionTokens = aux.CompletionTokens
	u.TotalTokens = aux.TotalTokens
	u.CachedTokens = aux.PromptTokensDetails.CachedTokens
	return nil
}

// ToolsUnsupportedError reports that the endpoint rejected the request's
// tools parameter (400 with a tool-related message). The agent treats it
// as fatal — without function calling the model can only answer
// questions, not drive the tool loop.
type ToolsUnsupportedError struct {
	Status string
	Body   string
}

func (e *ToolsUnsupportedError) Error() string {
	return fmt.Sprintf("chat completions: %s: tools not supported: %s", e.Status, e.Body)
}

// isToolsRejection reports whether a 400 body reads as a tools rejection
// (OpenAI "Invalid parameter: tools", ollama "tool calls are not supported",
// vLLM "\"tools\" is not supported" etc.).
func isToolsRejection(body string) bool {
	return strings.Contains(strings.ToLower(body), "tool")
}

// OverflowError reports that the request exceeded the model's context
// window (a 400 whose body names the context limit). The engine answers
// it by compacting the context once and retrying (M7.6), not by backing
// off.
type OverflowError struct {
	Status string
	Body   string
}

func (e *OverflowError) Error() string {
	return fmt.Sprintf("chat completions: %s: context overflow: %s", e.Status, e.Body)
}

// overflowPhrases are the body fragments that read as a context-length
// rejection (OpenAI "maximum context length", the context_length_exceeded
// error code, vLLM "max context length", ollama "context window",
// Anthropic-style "prompt is too long").
var overflowPhrases = []string{
	"context length",
	"maximum context",
	"max context",
	"context_length",
	"context window",
	"too many tokens",
	"prompt is too long",
	"reduce the length",
}

// isContextOverflow reports whether a 400 body reads as a context-length
// rejection. Checked before isToolsRejection, which matches loosely.
func isContextOverflow(body string) bool {
	b := strings.ToLower(body)
	for _, s := range overflowPhrases {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

// toolCallAccumulator assembles streamed delta.tool_calls entries: fragments
// arrive split across chunks, keyed by index (id/name on the first fragment,
// arguments appended incrementally).
type toolCallAccumulator struct {
	order []int
	byIdx map[int]*ToolCall
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIdx: map[int]*ToolCall{}}
}

// add folds one wire delta into the accumulator.
func (a *toolCallAccumulator) add(idx int, id, name, args string) {
	tc, ok := a.byIdx[idx]
	if !ok {
		tc = &ToolCall{}
		a.byIdx[idx] = tc
		a.order = append(a.order, idx)
	}
	if id != "" {
		tc.ID = id
	}
	if name != "" {
		tc.Name = name
	}
	tc.Arguments += args
}

// take returns the assembled calls in index order and resets the accumulator.
func (a *toolCallAccumulator) take() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	sort.Ints(a.order)
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		out = append(out, *a.byIdx[idx])
	}
	a.order = nil
	a.byIdx = map[int]*ToolCall{}
	return out
}
