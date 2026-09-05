package main

// Integration coverage for the §16 manager model, end to end through the
// real binary + engine + mock OpenAI-compatible endpoint: the manager
// spawns a worker subagent (the mock distinguishes the two by the worker
// prompt marker), the worker runs its own loop and reports, the manager
// polls task_output, accepts the report and answers. The core property is
// context isolation: the worker's intermediate output never enters any
// manager request, while the bounded report crosses the seam.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentSubagentDelegation(t *testing.T) {
	requireLinuxProc(t)
	// The intermediate data lives in a file: the worker's command line
	// (visible to the manager as the "last action" while running) carries
	// only the path, the echoed output carries the secret.
	const secret = "SECRET_INTERMEDIATE_77"
	home := approvalHome(t)
	secretFile := filepath.Join(home, "subagent-secret.txt")
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secretFile)

	var mu sync.Mutex
	mgrTurn, workerTurn := 0, 0
	var mgrBodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		b := string(body)

		var chunks []string
		ans := "…"
		if strings.Contains(b, "工人子任务") {
			// Worker subagent's request (the WorkerInstructions marker).
			mu.Lock()
			n := workerTurn
			workerTurn++
			mu.Unlock()
			switch n {
			case 0:
				chunks = []string{bashCallChunk("w0", "cat "+secretFile)}
			case 1:
				if !strings.Contains(b, secret) {
					http.Error(w, "worker's intermediate output missing from its own context", http.StatusBadRequest)
					return
				}
				ans = "① 已完成：运行了标记命令并确认输出。② 无修改。③ 无。"
			default:
				http.Error(w, "unexpected worker turn", http.StatusBadRequest)
				return
			}
		} else {
			mu.Lock()
			n := mgrTurn
			mgrTurn++
			mgrBodies = append(mgrBodies, b)
			mu.Unlock()
			switch {
			case n == 0:
				chunks = []string{streamToolChunk("m0", "task",
					`{"description":"探索","prompt":"在本目录运行一次标记检查并汇报结果"}`)}
			case strings.Contains(b, "[task 1] 已完成 ·"):
				if !strings.Contains(b, "① 已完成：运行了标记命令") {
					http.Error(w, "worker report missing from manager context", http.StatusBadRequest)
					return
				}
				// Include the worker report as reasoning so the screen shows it
				chunks = []string{
					reasoningChunk("report: ① 已完成：运行了标记命令并确认输出"),
					contentChunk("验收通过 SUBAGENT_OK"),
				}
			default:
				chunks = []string{streamToolChunk("m1", "task_output", `{"id":1}`)}
			}
		}
		writeSSE(w, chunks, ans)
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
bash_timeout = 30
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("探索一下并汇报\r"))
	out := r.readUntil(t, "SUBAGENT_OK", 15*time.Second)
	assertContains(t, out, "─── task 1:")   // spawn header
	assertContains(t, out, "[task 1] done") // terminal line
	assertContains(t, out, "① 已完成")         // worker report streamed

	// Isolation: no manager request carries the worker's intermediate
	// output (the brief deliberately never names it).
	mu.Lock()
	bodies := append([]string(nil), mgrBodies...)
	mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("only %d manager requests recorded", len(bodies))
	}
	for i, b := range bodies {
		if strings.Contains(b, secret) {
			t.Fatalf("manager request #%d carries the worker's intermediate output — isolation broken", i+1)
		}
	}
	if !strings.Contains(bodies[len(bodies)-1], "① 已完成：运行了标记命令") {
		t.Fatal("manager's final request missing the worker's report")
	}
}

// reasoningChunk builds one SSE data payload for a reasoning delta.
func reasoningChunk(reasoning string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{
			"reasoning_content": reasoning,
		}}},
	})
	return string(b)
}

// contentChunk builds one SSE data payload for a content delta.
func contentChunk(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{"content": content}}},
	})
	return string(b)
}
