package provider

import (
	"testing"

	"ruyishell/internal/config"
)

func TestNewClientDispatch(t *testing.T) {
	for api, want := range map[string]string{
		"":                   "openai",
		"openai-completions": "openai",
		"openai-responses":   "responses",
		"anthropic-messages": "anthropic",
		"legacy-garbage":     "openai", // unknown api falls back to completions
	} {
		c := NewClient(&Spec{API: api, Model: config.Model{ID: "m"}})
		var got string
		switch c.(type) {
		case *OpenAIClient:
			got = "openai"
		case *ResponsesClient:
			got = "responses"
		case *AnthropicClient:
			got = "anthropic"
		default:
			got = "unknown"
		}
		if got != want {
			t.Fatalf("NewClient(api=%q) = %q, want %q", api, got, want)
		}
	}
}
