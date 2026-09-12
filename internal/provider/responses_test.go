package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"ruyishell/internal/config"
)

func TestResponsesChatStream(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})

	c := NewResponsesClient(&Spec{BaseURL: base, APIKey: "k", MaxTokens: 2048, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(),
		[]ChatMessage{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hi"},
		}, WithTools([]ToolDef{{Name: "bash", Description: "run", Parameters: map[string]any{"type": "object"}}}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var out, toolArgs string
	var toolName string
	var usage *Usage
	for tok := range ch {
		switch {
		case tok.ToolCall != nil:
			toolName = tok.ToolCall.Name
			toolArgs = tok.ToolCall.Arguments
		case tok.Usage != nil:
			usage = tok.Usage
		case tok.Err != nil:
			t.Fatalf("unexpected stream err: %v", tok.Err)
		default:
			out += tok.Text
		}
	}
	if out != "Hello" {
		t.Fatalf("stream = %q, want Hello", out)
	}
	if toolName != "bash" || toolArgs != `{"command":"ls"}` {
		t.Fatalf("tool = %q/%q, want bash/{\"command\":\"ls\"}", toolName, toolArgs)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want 10/5/15", usage)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"instructions":"you are helpful"`) {
		t.Fatalf("body missing instructions: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"model":"m"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if !strings.Contains(gotBody, `"max_output_tokens":2048`) {
		t.Fatalf("body missing max_output_tokens: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"name":"bash"`) || !strings.Contains(gotBody, `"type":"function"`) || !strings.Contains(gotBody, `"parameters"`) {
		t.Fatalf("body missing tools: %s", gotBody)
	}
}

func TestResponsesInputFlattening(t *testing.T) {
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c := NewResponsesClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys A"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "calling", ToolCalls: []ToolCall{{ID: "c1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
		{Role: "tool", Content: "/tmp", ToolCallID: "c1"},
		{Role: "assistant", Content: "done"},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}
	// system lifted to instructions; the rest flattened in timeline order.
	for _, want := range []string{
		`"instructions":"sys A"`,
		`"role":"user"`,
		`"type":"function_call"`,
		`"call_id":"c1"`,
		`"type":"function_call_output"`,
		`"role":"assistant"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestResponsesMidSystemFoldsIntoUserMessage(t *testing.T) {
	// A mid-array system (shell event) must NOT be lifted into the
	// instructions field — it folds into the next user input item, and
	// only the leading run (sys A) reaches instructions.
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c := NewResponsesClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys A"},
		{Role: "user", Content: "do it"},
		{Role: "shell", Content: "$ ls\nok\n---"},
		{Role: "user", Content: "what was that"},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}
	// Exact match: instructions is the leading run only.
	if !strings.Contains(gotBody, `"instructions":"sys A"`) {
		t.Fatalf("instructions = wrong (shell event leaked in?): %s", gotBody)
	}
	// The shell event sits inside the following user input item.
	if !strings.Contains(gotBody, "$ ls\\nok\\n---\\n\\nwhat was that") {
		t.Fatalf("shell event not folded into the next user item: %s", gotBody)
	}
}

func TestResponsesToolsRejection(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"Invalid parameter: tools is not supported"}}`)
	})
	c := NewResponsesClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	if err == nil {
		t.Fatal("expected error for 400 tools rejection")
	}
	var toolsErr *ToolsUnsupportedError
	if !errors.As(err, &toolsErr) {
		t.Fatalf("err = %v, want *ToolsUnsupportedError", err)
	}
}
