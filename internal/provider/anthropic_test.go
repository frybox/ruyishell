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

func TestAnthropicChatStream(t *testing.T) {
	var gotPath, gotKey, gotVersion, gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"x\\\"}\"}}\n\n")
		io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	})

	c := NewAnthropicClient(&Spec{BaseURL: base, APIKey: "key", MaxTokens: 1024, Model: config.Model{ID: "claude-x"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}, WithTools([]ToolDef{{Name: "read", Description: "read file", Parameters: map[string]any{"type": "object"}}}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var out, toolName, toolArgs string
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
	if toolName != "read" || toolArgs != `{"path":"x"}` {
		t.Fatalf("tool = %q/%q, want read/{\"path\":\"x\"}", toolName, toolArgs)
	}
	if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 3 || usage.TotalTokens != 10 {
		t.Fatalf("usage = %+v, want 7/3/10", usage)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "key" {
		t.Fatalf("x-api-key = %q, want key", gotKey)
	}
	if gotVersion == "" {
		t.Fatalf("anthropic-version header missing")
	}
	for _, want := range []string{
		`"max_tokens":1024`,
		`"system":"be brief"`,
		`"model":"claude-x"`,
		`"input_schema"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestAnthropicMessagesFlattening(t *testing.T) {
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
	})
	c := NewAnthropicClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys A"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "calling", ToolCalls: []ToolCall{{ID: "toolu_9", Name: "bash", Arguments: `{"command":"pwd"}`}}},
		{Role: "tool", Content: "/tmp", ToolCallID: "toolu_9"},
		{Role: "assistant", Content: "done"},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}
	for _, want := range []string{
		`"system":"sys A"`,
		`"tool_use"`,
		`"toolu_9"`,
		`"tool_result"`,
		`"tool_use_id":"toolu_9"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestBuildAnthropicMessagesParallelGrouping(t *testing.T) {
	// One assistant turn with two parallel tool calls, then the two results.
	// Anthropic requires both tool_results grouped into ONE user message.
	_, messages := buildAnthropicMessages([]ChatMessage{
		{Role: "user", Content: "do two"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "toolu_a", Name: "bash", Arguments: `{"command":"a"}`},
			{ID: "toolu_b", Name: "bash", Arguments: `{"command":"b"}`},
		}},
		{Role: "tool", Content: "A", ToolCallID: "toolu_a"},
		{Role: "tool", Content: "B", ToolCallID: "toolu_b"},
	})
	if len(messages) != 3 {
		t.Fatalf("len = %d, want 3 (user, assistant, grouped user)", len(messages))
	}
	asst, ok := messages[1].Content.([]anthropicContent)
	if !ok || len(asst) != 2 || asst[0].Type != "tool_use" || asst[1].Type != "tool_use" {
		t.Fatalf("assistant = %+v, want 2 tool_use blocks", messages[1].Content)
	}
	grouped, ok := messages[2].Content.([]anthropicToolResult)
	if !ok || len(grouped) != 2 || grouped[0].ToolUseID != "toolu_a" || grouped[1].ToolUseID != "toolu_b" {
		t.Fatalf("grouped user = %+v, want 2 tool_results a,b", messages[2].Content)
	}
}

func TestBuildAnthropicMessagesConsecutiveAssistantMerge(t *testing.T) {
	// A text assistant turn followed by a tool-call assistant turn must merge
	// into one assistant message (no assistant/assistant adjacency).
	_, messages := buildAnthropicMessages([]ChatMessage{
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "thinking out loud"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "t1", Name: "bash", Arguments: `{"command":"ls"}`}}},
		{Role: "tool", Content: "ok", ToolCallID: "t1"},
	})
	if len(messages) != 3 {
		t.Fatalf("len = %d, want 3", len(messages))
	}
	asst, ok := messages[1].Content.([]anthropicContent)
	if !ok || len(asst) != 2 || asst[0].Type != "text" || asst[1].Type != "tool_use" {
		t.Fatalf("assistant = %+v, want text+tool_use merged", messages[1].Content)
	}
}

func TestAnthropicDefaultMaxTokens(t *testing.T) {
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n")
	})
	c := NewAnthropicClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}}) // MaxTokens unset
	ch, err := c.ChatStream(context.Background(), nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}
	if !strings.Contains(gotBody, `"max_tokens":4096`) {
		t.Fatalf("body missing default max_tokens 4096: %s", gotBody)
	}
}

func TestAnthropicMidSystemFoldsIntoUserMessage(t *testing.T) {
	// A mid-array system (shell event) must NOT be hoisted into the
	// top-level system field — it folds into the next user message, and
	// only the leading run (sys A) reaches the top-level system field.
	var gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
		io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n")
	})
	c := NewAnthropicClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{
		{Role: "system", Content: "sys A"},
		{Role: "user", Content: "do it"},
		{Role: "shell", Content: "$ curl -v x\n200\n---"},
		{Role: "user", Content: "what did that hit"},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for range ch {
	}
	// Exact match: the top-level system field is the leading run only.
	if !strings.Contains(gotBody, `"system":"sys A"`) {
		t.Fatalf("top-level system = wrong (shell event leaked in?): %s", gotBody)
	}
	// The shell event sits inside the following user message's text block,
	// JSON-escaped (\n is a literal two-char sequence in the body).
	if !strings.Contains(gotBody, "$ curl -v x\\n200\\n---\\n\\nwhat did that hit") {
		t.Fatalf("shell event not folded into the next user message: %s", gotBody)
	}
}

func TestAnthropicErrorStatus(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`)
	})
	c := NewAnthropicClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want status mention", err)
	}
}

func TestAnthropicToolsRejection(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools is not supported by this model"}}`)
	})
	c := NewAnthropicClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil, WithTools([]ToolDef{{Name: "bash"}}))
	if err == nil {
		t.Fatal("expected error for 400 tools rejection")
	}
	var toolsErr *ToolsUnsupportedError
	if !errors.As(err, &toolsErr) {
		t.Fatalf("err = %v, want *ToolsUnsupportedError", err)
	}
}
