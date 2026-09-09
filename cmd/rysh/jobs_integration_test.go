package main

// Integration coverage for §8 long commands and background jobs: a bash
// call outliving the auto-background window comes back as a job notice,
// the model inspects it with job_output and terminates it with job_kill,
// all through the real engine/provider/tool stack driven by a mock
// OpenAI-compatible endpoint.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// streamToolChunkAt builds one SSE data payload carrying a native
// tool_call delta at an explicit index — one response's tool calls each
// get their own index, or the provider's accumulator folds them into one
// call (same fragment shape the provider SSE reader unit tests use).
func streamToolChunkAt(idx int, id, name, args string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{
			"tool_calls": []map[string]any{{
				"index": idx, "id": id, "type": "function",
				"name":      name,
				"arguments": args,
			}},
		}}},
	})
	return string(b)
}

func streamToolChunk(id, name, args string) string {
	return streamToolChunkAt(0, id, name, args)
}

// Scenario: `echo bg-live; sleep 20` exceeds the 1s auto-background
// window → the fed-back bash result names job 1; job_output reports it
// still running with the captured output; job_kill terminates it; the
// final answer closes the task (§8.1/§8.2 acceptance).
func TestAgentAutoBackgroundJobs(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()

		var chunks []string
		ans := "checking the background job"
		switch n {
		case 0:
			ans = "starting the long command"
			chunks = []string{bashCallChunk("call_bg", "echo bg-live; sleep 20")}
		case 1:
			// The bash result must name the job and point at job_output.
			if !strings.Contains(string(body), "已转后台 job 1") || !strings.Contains(string(body), "job_output") {
				http.Error(w, "auto-background notice missing from context", http.StatusBadRequest)
				return
			}
			ans = "looking at job 1"
			chunks = []string{streamToolChunk("call_1", "job_output", `{"id":1}`)}
		case 2:
			// job_output must report a running job with its output so far.
			if !strings.Contains(string(body), "仍在运行") || !strings.Contains(string(body), "bg-live") {
				http.Error(w, "job_output result missing running status/output", http.StatusBadRequest)
				return
			}
			ans = "terminating job 1"
			chunks = []string{streamToolChunk("call_2", "job_kill", `{"id":1}`)}
		case 3:
			if !strings.Contains(string(body), "已终止") {
				http.Error(w, "job_kill result missing", http.StatusBadRequest)
				return
			}
			ans = "JOBS_ALL_DONE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"

[agent]
approval = "always"
bash_timeout = 30
auto_background_after = 1
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("run the long command\r"))
	out := r.readUntil(t, "JOBS_ALL_DONE", waitTimeout)
	assertContains(t, out, "后台 job 1")
	assertContains(t, out, "job_output")
	assertContains(t, out, "job_kill")
}

// §8.3: ^C while a foreground command executes interrupts only that
// command — the interrupted result is fed back and the task continues to
// its final answer (the streaming phases keep the M4 whole-task cancel).
func TestAgentToolInterruptKeepsTaskAlive(t *testing.T) {
	requireLinuxProc(t)
	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()

		ans := "INTERRUPT_ALIVE"
		var chunks []string
		switch n {
		case 0:
			ans = "starting the slow command"
			chunks = []string{bashCallChunk("call_slow", "sleep 20")}
		case 1:
			if !strings.Contains(string(body), "被用户中断") {
				http.Error(w, "interrupted tool result missing from context", http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", streamChunk(ans))
		fl.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	// A 30s auto-background window and 60s timeout keep sleep 20 in the
	// foreground exec phase for the whole test — only ^C can end it.
	cfg := fmt.Sprintf(`
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = %q
api = "openai-completions"

[[providers.ollama.models]]
id = "llama3.1:8b"

[agent]
approval = "always"
bash_timeout = 60
auto_background_after = 30
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("start the slow command\r"))
	// Wait for the fence reply to render, then give the engine a moment
	// to enter the exec phase before pressing ^C.
	r.readUntil(t, "starting the slow command", waitTimeout)
	time.Sleep(1200 * time.Millisecond)
	p.Write([]byte("\x03"))

	out := r.readUntil(t, "INTERRUPT_ALIVE", waitTimeout)
	assertContains(t, out, "前台命令已中断，任务继续")
}
