package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"ruyishell/internal/config"
)

// readAll drains a stream channel into a slice of tokens.
func readAll(ch <-chan StreamToken) []StreamToken {
	var out []StreamToken
	for tok := range ch {
		out = append(out, tok)
	}
	return out
}

func TestChatStreamToolCallDeltas(t *testing.T) {
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"Let me look. "}}]}`+"\n\n")
		// Tool 0: id/name on the first fragment, arguments split in two.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","name":"bash","arguments":""}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"arguments":"{\"comm"}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"arguments":"and\":\"ls\"}"}]}}]}`+"\n\n")
		// Tool 1: complete in a single fragment.
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","name":"read","arguments":"{\"path\":\"a.go\"}"}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
		io.WriteString(w, `data: [DONE]`+"\n\n")
	})

	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}},
		WithTools([]ToolDef{
			{Name: "bash", Description: "run a shell command", Parameters: map[string]any{"type": "object"}},
			{Name: "read"},
		}),
		WithUsage())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	toks := readAll(ch)

	// Request body: tools advertised + include_usage requested.
	if !strings.Contains(gotBody, `"tools":[{"type":"function","function":{"name":"bash","description":"run a shell command","parameters":{"type":"object"}}},{"type":"function","function":{"name":"read"}}]`) {
		t.Fatalf("tools wire shape = %q", gotBody)
	}
	if !strings.Contains(gotBody, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream_options missing: %q", gotBody)
	}

	// Token sequence: content, then assembled calls in index order, then usage.
	if len(toks) != 4 {
		t.Fatalf("got %d tokens, want 4: %+v", len(toks), toks)
	}
	if toks[0].Text != "Let me look. " || toks[0].Reasoning || toks[0].ToolCall != nil {
		t.Fatalf("tok0 = %+v", toks[0])
	}
	if toks[1].ToolCall == nil || *toks[1].ToolCall != (ToolCall{ID: "call_1", Name: "bash", Arguments: `{"command":"ls"}`}) {
		t.Fatalf("tok1 = %+v", toks[1])
	}
	if toks[2].ToolCall == nil || *toks[2].ToolCall != (ToolCall{ID: "call_2", Name: "read", Arguments: `{"path":"a.go"}`}) {
		t.Fatalf("tok2 = %+v", toks[2])
	}
	if toks[3].Usage == nil || *toks[3].Usage != (Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}) {
		t.Fatalf("tok3 = %+v", toks[3])
	}
}

func TestChatStreamToolCallPlainChatUnchanged(t *testing.T) {
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","name":"bash","arguments":"{}"}]}}]}`+"\n\n")
		io.WriteString(w, `data: [DONE]`+"\n\n")
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})

	// No options: wire shape stays exactly the pre-tools body (fence mode).
	ch, err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	toks := readAll(ch)
	if strings.Contains(gotBody, "tools") || strings.Contains(gotBody, "stream_options") {
		t.Fatalf("plain chat body changed: %q", gotBody)
	}
	// Tool calls without a usage chunk are still flushed at stream end.
	if len(toks) != 1 || toks[0].ToolCall == nil || toks[0].ToolCall.Name != "bash" {
		t.Fatalf("toks = %+v", toks)
	}
}

func TestChatStreamToolsUnsupported(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"tool calls are not supported by this model","type":"invalid_request_error"}}`)
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	var tue *ToolsUnsupportedError
	if !errors.As(err, &tue) {
		t.Fatalf("err = %v, want *ToolsUnsupportedError", err)
	}
	if !strings.Contains(tue.Error(), "400") {
		t.Fatalf("error = %v, want status mention", tue)
	}
}

func TestChatStreamGeneric400NotTools(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"invalid message content"}}`)
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	var tue *ToolsUnsupportedError
	if errors.As(err, &tue) {
		t.Fatalf("err = %v, want generic error for non-tool 400", err)
	}
	if !strings.Contains(err.Error(), "invalid message content") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatStreamContextOverflow(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens."}}`)
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	var oe *OverflowError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *OverflowError", err)
	}
	if oe.Status != "400 Bad Request" {
		t.Fatalf("Status = %q", oe.Status)
	}
	if !strings.Contains(oe.Body, "maximum context length") {
		t.Fatalf("Body = %q, want the refusal body", oe.Body)
	}
	if !strings.Contains(oe.Error(), "context overflow") {
		t.Fatalf("error = %v, want the overflow wording", oe)
	}
}

func TestContextOverflowBeatsToolsRejection(t *testing.T) {
	// A 400 naming the context limit classifies as overflow even with
	// tools advertised: the engine must compact-and-retry, not fall
	// back to fence mode — and the loose tools check ("tool" anywhere)
	// must not shadow it.
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"too many tokens in request"}}`)
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	var oe *OverflowError
	var tue *ToolsUnsupportedError
	if !errors.As(err, &oe) || errors.As(err, &tue) {
		t.Fatalf("err = %v, want *OverflowError and not *ToolsUnsupportedError", err)
	}
}

func TestIsContextOverflowPhrases(t *testing.T) {
	yes := []string{
		"Maximum Context Length is 4096",
		"max context length is 8192 tokens",
		`{"code":"context_length_exceeded"}`,
		"the request used too many tokens",
		"prompt is too long: 9000 tokens > 8192 maximum",
		"please reduce the length of your messages",
		"this model's context window is full",
	}
	no := []string{
		"",
		"invalid api key",
		"tool calls are not supported by this model",
		"internal server error",
	}
	for _, s := range yes {
		if !isContextOverflow(s) {
			t.Fatalf("isContextOverflow(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isContextOverflow(s) {
			t.Fatalf("isContextOverflow(%q) = true, want false", s)
		}
	}
}

func TestChatMessageToolWireShape(t *testing.T) {
	assistant := ChatMessage{
		Role:      "assistant",
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_9", Name: "bash", Arguments: `{"command":"ls"}`}},
	}
	b, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	calls, ok := raw["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant wire = %s", b)
	}
	call := calls[0].(map[string]any)
	if call["type"] != "function" {
		t.Fatalf("tool call type = %v", call["type"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"] != `{"command":"ls"}` {
		t.Fatalf("function = %v", fn)
	}

	toolMsg := ChatMessage{Role: "tool", Content: "out", ToolCallID: "call_9"}
	b, err = json.Marshal(toolMsg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tool_call_id":"call_9"`) || !strings.Contains(string(b), `"role":"tool"`) {
		t.Fatalf("tool wire = %s", b)
	}

	// Round-trip keeps the domain fields.
	var back ChatMessage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Role != "tool" || back.Content != "out" || back.ToolCallID != "call_9" {
		t.Fatalf("round-trip = %+v", back)
	}
	var backAssistant ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","tool_calls":[{"id":"call_9","type":"function","function":{"name":"bash","arguments":"{}"}}]}`), &backAssistant); err != nil {
		t.Fatal(err)
	}
	if len(backAssistant.ToolCalls) != 1 || backAssistant.ToolCalls[0].ID != "call_9" || backAssistant.ToolCalls[0].Name != "bash" {
		t.Fatalf("assistant round-trip = %+v", backAssistant)
	}
}

func TestToolCallAccumulator(t *testing.T) {
	acc := newToolCallAccumulator()
	if got := acc.take(); got != nil {
		t.Fatalf("empty take = %+v", got)
	}
	// Out-of-order arrival still assembles by index.
	acc.add(1, "call_2", "read", `{"path":"a`)
	acc.add(0, "", "", "{\"comm")
	acc.add(0, "call_1", "bash", "")
	acc.add(1, "", "", `nd.go"}`)
	acc.add(0, "", "", `and":"ls"}`)
	got := acc.take()
	if len(got) != 2 {
		t.Fatalf("take = %+v", got)
	}
	if got[0] != (ToolCall{ID: "call_1", Name: "bash", Arguments: `{"command":"ls"}`}) {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1] != (ToolCall{ID: "call_2", Name: "read", Arguments: `{"path":"and.go"}`}) {
		t.Fatalf("got[1] = %+v", got[1])
	}
	// take resets: a fresh round starts empty.
	if got := acc.take(); got != nil {
		t.Fatalf("take after reset = %+v", got)
	}
}

func TestSpecToolsEnabled(t *testing.T) {
	on := true
	off := false
	for _, tc := range []struct {
		m    config.Model
		want bool
	}{{config.Model{}, true}, {config.Model{Tools: &on}, true}, {config.Model{Tools: &off}, false}} {
		spec := &Spec{Model: tc.m}
		if got := spec.ToolsEnabled(); got != tc.want {
			t.Fatalf("ToolsEnabled(%+v) = %v, want %v", tc.m, got, tc.want)
		}
	}
}

// M8.7: the OpenAI prompt-cache detail block must land on Usage, and
// endpoints without it must stay at zero without error.
func TestUsageCachedTokens(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":64}}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 7 || u.TotalTokens != 107 || u.CachedTokens != 64 {
		t.Fatalf("u = %+v", u)
	}
	var v Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.CachedTokens != 0 {
		t.Fatalf("v.CachedTokens = %d, want 0 when unreported", v.CachedTokens)
	}
}
