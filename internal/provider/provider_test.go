package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"ruyishell/internal/config"
)

const sample = `
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
name = "Llama 3.1 8B (Local)"

[[providers.ollama.models]]
id = "qwen2.5-coder:7b"

[providers.mycloud]
base_url = "https://api.example.com/v1"
api = "openai-completions"
api_key = "$RYSH_PROV_TEST_KEY"

[[providers.mycloud.models]]
id = "gpt-x"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/config.toml"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("RYSH_PROV_TEST_KEY", "sk-cloud")
	cfg, err := config.Load(writeConfig(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestResolve(t *testing.T) {
	cfg := sampleConfig(t)

	spec, err := Resolve(cfg, "ollama/llama3.1:8b")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Ref() != "ollama/llama3.1:8b" {
		t.Fatalf("Ref = %q", spec.Ref())
	}
	if spec.Name != "Llama 3.1 8B (Local)" {
		t.Fatalf("Name = %q", spec.Name)
	}
	if spec.BaseURL != "http://localhost:11434/v1" || spec.APIKey != "ollama" {
		t.Fatalf("spec = %+v", spec)
	}

	// Env-var key resolved at resolve time.
	cloud, err := Resolve(cfg, "mycloud/gpt-x")
	if err != nil {
		t.Fatalf("Resolve mycloud: %v", err)
	}
	if cloud.APIKey != "sk-cloud" {
		t.Fatalf("mycloud api key = %q, want sk-cloud", cloud.APIKey)
	}
}

func TestResolveErrors(t *testing.T) {
	cfg := sampleConfig(t)
	for _, ref := range []string{
		"nope/llama3.1:8b", // unknown provider
		"ollama/nope",      // unknown model
		"ollama",           // missing model
		"ollama/",          // empty model
		"/ollama",          // empty provider
		"ollama/a/b",       // extra slash -> model "a/b" not found
	} {
		if _, err := Resolve(cfg, ref); err == nil {
			t.Fatalf("Resolve(%q): expected error", ref)
		}
	}
}

func TestResolveMissingKeyEnv(t *testing.T) {
	cfg, err := config.Load(writeConfig(t, `
[providers.p]
api_key = "$RYSH_NO_SUCH_PROV_KEY"
[[providers.p.models]]
id = "m"
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(cfg, "p/m"); err == nil {
		t.Fatal("expected error resolving missing env var")
	}
}

func TestList(t *testing.T) {
	cfg := sampleConfig(t)
	refs := List(cfg)
	want := []string{"mycloud/gpt-x", "ollama/llama3.1:8b", "ollama/qwen2.5-coder:7b"}
	if len(refs) != len(want) {
		t.Fatalf("List = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("List = %v, want %v", refs, want)
		}
	}
}

func TestDefault(t *testing.T) {
	cfg := sampleConfig(t)
	if got := Default(cfg); got != "ollama/llama3.1:8b" {
		t.Fatalf("Default = %q, want ollama/llama3.1:8b", got)
	}

	// No default configured: first available (sorted) wins.
	cfg2, err := config.Load(writeConfig(t, `
[providers.p]
[[providers.p.models]]
id = "b"
[[providers.p.models]]
id = "a"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Default(cfg2); got != "p/a" {
		t.Fatalf("Default (no configured default) = %q, want p/a", got)
	}

	// Empty config: no default.
	if got := Default(&config.Config{}); got != "" {
		t.Fatalf("Default (empty) = %q, want empty", got)
	}
}

// fakeServer serves OpenAI-compatible chat completions over SSE.
func fakeServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestOpenAIChatStream(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})

	c := NewOpenAIClient(&Spec{BaseURL: base, APIKey: "k", Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var out strings.Builder
	for tok := range ch {
		if tok.Reasoning {
			t.Fatalf("unexpected reasoning token %q", tok.Text)
		}
		out.WriteString(tok.Text)
	}
	if out.String() != "Hello" {
		t.Fatalf("stream = %q, want Hello", out.String())
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"model":"m"`) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestOpenAIChatStreamReasoning(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" harder\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Answer\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ch, err := c.ChatStream(context.Background(), nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var reas, out string
	for tok := range ch {
		if tok.Reasoning {
			reas += tok.Text
		} else {
			out += tok.Text
		}
	}
	if reas != "think harder" {
		t.Fatalf("reasoning = %q, want %q", reas, "think harder")
	}
	if out != "Answer" {
		t.Fatalf("content = %q, want Answer", out)
	}
}

func TestOpenAIChatStreamErrorStatus(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want status mention", err)
	}
}

func TestOpenAIChatStreamCancel(t *testing.T) {
	base := fakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	c := NewOpenAIClient(&Spec{BaseURL: base, Model: config.Model{ID: "m"}})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.ChatStream(ctx, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	cancel()
	for range ch {
		// draining; the goroutine should exit on ctx cancel
	}
}

func TestOpenAIClientBadBase(t *testing.T) {
	c := NewOpenAIClient(&Spec{BaseURL: "://bad", Model: config.Model{ID: "m"}})
	_, err := c.ChatStream(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for bad base URL")
	}
}
