package main

// Integration coverage for §7 permission approval: the ask-mode gate
// prompts on a highlighted amber bar that owns its row (never inlined after
// streamed reply text) and locks the streaming keys down to y/n/a —
// y executes the command, n feeds the refusal back and ends the task
// without running it, a records the session prefix rule so the second
// same-prefix command runs without a prompt, and auto mode never prompts.
// All through the real engine/provider/tool stack over a mock
// OpenAI-compatible endpoint.

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
)

// approvalHome returns the child rysh's HOME (a per-test temp dir); the
// gated test commands touch files under it, which the test can assert on.
func approvalHome(t *testing.T) string {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME not set for test child")
	}
	return home
}

// bashCallChunk builds the tool_call delta for one bash command (JSON-
// marshalled arguments, so arbitrary paths stay legal on the wire).
func bashCallChunk(id, cmd string) string {
	args, _ := json.Marshal(map[string]any{"command": cmd})
	return streamToolChunk(id, "bash", string(args))
}

// bashCallChunkAt is bashCallChunk with an explicit tool_call index —
// one response's tool calls each get their own index.
func bashCallChunkAt(idx int, id, cmd string) string {
	args, _ := json.Marshal(map[string]any{"command": cmd})
	return streamToolChunkAt(idx, id, "bash", string(args))
}

// writeSSE flushes the chunk list plus one final content chunk as SSE.
func writeSSE(w http.ResponseWriter, chunks []string, ans string) {
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
}

// y path: the approved touch really executes. n path: the refusal is fed
// back, the command never runs, and the task still reaches its final
// answer (§7 审批被拒).
func TestAgentApprovalYesNoPaths(t *testing.T) {
	requireLinuxProc(t)
	home := approvalHome(t)
	yFile := filepath.Join(home, "appr-y.txt")
	nFile := filepath.Join(home, "appr-n.txt")
	_ = os.Remove(yFile)
	_ = os.Remove(nFile)

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
		ans := "…"
		switch n {
		case 0:
			chunks = []string{bashCallChunk("call_0", "touch "+yFile)}
		case 1:
			// Task 1's final answer: the command ran (y), so no refusal
			// may appear in its context, and the tool result must be there.
			if strings.Contains(string(body), "用户拒绝") {
				http.Error(w, "y-approved command was reported as refused", http.StatusBadRequest)
				return
			}
			if !strings.Contains(string(body), "appr-y.txt") {
				http.Error(w, "first touch result missing from context", http.StatusBadRequest)
				return
			}
			ans = "Y_FILE_CREATED"
		case 2:
			chunks = []string{bashCallChunk("call_2", "touch "+nFile)}
		case 3:
			if !strings.Contains(string(body), "用户拒绝了该操作") {
				http.Error(w, "refusal text missing from context", http.StatusBadRequest)
				return
			}
			ans = "DENY_SEEN_TASK_ALIVE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
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

	// --- y path: the approved command really executes ---
	p.Write([]byte("create the first file\r"))
	win := r.readUntil(t, "? 运行 touch ", waitTimeout)
	// The prompt is the highlighted amber bar, not bare text. The SGR state is
	// replayed rather than matched byte for byte, because a Windows pty hands
	// back the terminal's own re-rendered sequences (\x1b[1;30;43m arrives
	// split into \x1b[30m\x1b[43m\x1b[1m).
	if j := strings.Index(win, approvalPrompt); j < 0 || !amberUnderlay(win[:j]) {
		t.Fatalf("approval prompt is not drawn on the amber bar: %q", win)
	}
	p.Write([]byte("y"))
	out := r.readUntil(t, "Y_FILE_CREATED", waitTimeout)
	assertContains(t, out, "[tool] $ touch")
	if _, err := os.Stat(yFile); err != nil {
		t.Fatalf("y-approved command did not execute: %v", err)
	}

	// --- n path: refusal fed back, command never runs, task finishes ---
	// Wait for task 1 to finalize (its AI prompt redraws) before submitting
	// task 2: keys typed mid-stream are dropped by the streaming lock, and a
	// submit burst that straddles the moment an approval prompt opens would
	// have its y/n/a letters misread as answers.
	r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	p.Write([]byte("create the second file\r"))
	r.readUntil(t, "? 运行 touch ", waitTimeout)
	p.Write([]byte("n"))
	out = r.readUntil(t, "DENY_SEEN_TASK_ALIVE", waitTimeout)
	assertContains(t, out, "已拒绝: $ touch")
	if _, err := os.Stat(nFile); err == nil {
		t.Fatal("n-denied command executed anyway")
	}
}

// a path: answering "always" records the `touch` prefix rule, so the
// second same-prefix command runs with no further prompt (§7.2).
func TestAgentApprovalAlwaysSkipsSecondPrompt(t *testing.T) {
	requireLinuxProc(t)
	home := approvalHome(t)
	a1 := filepath.Join(home, "appr-a1.txt")
	a2 := filepath.Join(home, "appr-a2.txt")
	_ = os.Remove(a1)
	_ = os.Remove(a2)

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
		ans := "…"
		switch n {
		case 0:
			chunks = []string{bashCallChunk("call_0", "touch "+a1)}
		case 1:
			if !strings.Contains(string(body), "appr-a1.txt") {
				http.Error(w, "first touch result missing from context", http.StatusBadRequest)
				return
			}
			// Same `touch` prefix: the always rule must let it through
			// without a second prompt.
			chunks = []string{bashCallChunk("call_1", "touch "+a2)}
		case 2:
			if strings.Contains(string(body), "用户拒绝") {
				http.Error(w, "second touch was refused — always rule did not cover it", http.StatusBadRequest)
				return
			}
			ans = "APPROVAL_A_DONE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
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

	p.Write([]byte("create two files\r"))
	r.readUntil(t, "? 运行 touch ", waitTimeout)
	p.Write([]byte("a"))
	out := r.readUntil(t, "APPROVAL_A_DONE", waitTimeout)
	if strings.Contains(out, "? 运行") {
		t.Fatalf("second same-prefix command prompted again: %q", out)
	}
	for _, f := range []string{a1, a2} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
}

// auto mode: the same gated command runs with zero prompts (§7.2).
func TestAgentApprovalAutoModeZeroPrompts(t *testing.T) {
	requireLinuxProc(t)
	home := approvalHome(t)
	f := filepath.Join(home, "appr-auto.txt")
	_ = os.Remove(f)

	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()

		var chunks []string
		ans := "…"
		switch n {
		case 0:
			chunks = []string{bashCallChunk("call_0", "touch "+f)}
		case 1:
			ans = "APPROVAL_AUTO_DONE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
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
approval = "auto"
bash_timeout = 30
`, srv.URL)

	p, _, r := startRyshWithConfig(t, cfg)
	r.readUntil(t, "$ ", startupTimeout)
	enterAI(t, p, r)

	p.Write([]byte("create the file\r"))
	out := r.readUntil(t, "APPROVAL_AUTO_DONE", waitTimeout)
	if strings.Contains(out, "? 运行") {
		t.Fatalf("auto mode prompted for approval: %q", out)
	}
	// No bar in auto mode, but the tool execution is still visible: the
	// 执行中 spinner runs straight from the tool call.
	assertContains(t, out, "执行中")
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("auto-mode command did not execute: %v", err)
	}
}

// While a tool runs, its wait must be visible: after the user answers the
// approval bar, the 执行中 spinner spins on the row below until the [tool]
// line lands, so a slow command does not read as a dead stream (and the
// spinner's appearance acknowledges the y, so the user does not press it
// again in doubt).
func TestToolExecSpinnerAfterApproval(t *testing.T) {
	requireLinuxProc(t)
	home := approvalHome(t)
	f := filepath.Join(home, "appr-exec-spin.txt")
	_ = os.Remove(f)

	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()
		var chunks []string
		ans := "…"
		switch n {
		case 0:
			chunks = []string{bashCallChunk("call_spin", "touch "+f)}
		case 1:
			ans = "EXEC_SPIN_DONE"
		default:
			http.Error(w, "unexpected turn", http.StatusBadRequest)
			return
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

	p.Write([]byte("create the file\r"))
	r.readUntil(t, "? 运行 touch ", waitTimeout)
	p.Write([]byte("y"))
	out := r.readUntil(t, "[tool] $ touch", waitTimeout)
	// The 执行中 spinner ran on the row below the bar, between the answer
	// and the [tool] line.
	assertContains(t, out, "执行中")
	assertContains(t, out, "[tool] $ touch")
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("approved command did not execute: %v", err)
	}
}
