package main

// Regression coverage for how the §11.3 approval prompt reaches the screen.
// It must own a terminal row of its own — never be appended to the tail of a
// streamed reply (where the cursor is left mid-line), never share the row the
// inline pre-output spinner is animating on — and it must read as the
// highlighted amber bar. Both cases answer n, so no command ever executes.
//
// The assertions replay escape sequences instead of matching them byte for
// byte: the pty hands back ConPTY's re-rendering, which splits and reorders
// the sequences rysh writes (a single \x1b[1;30;43m arrives as
// \x1b[30m\x1b[43m\x1b[1m). What has to hold on every platform is what the
// user reads: a line break before the prompt, nothing else on its row, and
// the prompt's cells carrying the amber background.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/charmbracelet/x/ansi"
)

// approvalBarAsk is the tail of the prompt, after the command it names.
const approvalBarAsk = "（y 是 / n 否 / a 总是）"

// approvalPrompt is the text the bar starts with, unstyled.
const approvalPrompt = "? 运行"

// startApprovalBarCase starts rysh on a mock endpoint whose first answer is
// exactly chunks — no trailing content delta, so the caller decides whether
// the cursor sits mid-reply or on a live spinner row when the gate fires —
// and whose later answers (the refusal round) are the plain text final.
// `hold` delays the first byte, which is how long the pre-output spinner is
// left running: the pty hands back the terminal's re-rendering, so a spinner
// that lives only a few milliseconds is coalesced away and never becomes
// bytes to assert on. It returns the pty with AI mode entered.
func startApprovalBarCase(t *testing.T, hold time.Duration, chunks []string, final string) (pty.Pty, *ptyReader) {
	t.Helper()
	var mu sync.Mutex
	turn := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		n := turn
		turn++
		mu.Unlock()
		if n == 0 && hold > 0 {
			time.Sleep(hold)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		out := chunks
		if n > 0 {
			out = nil
			fmt.Fprintf(w, "data: %s\n\n", streamChunk(final))
			fl.Flush()
		}
		for _, c := range out {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

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
	return p, r
}

// sgrRe matches an SGR sequence and captures its parameter list.
var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// amberUnderlay reports whether the SGR state reached by s puts following
// text on the amber bar background (43). Sequences are replayed in order, so
// a reset anywhere between the style and the text reads as no background.
func amberUnderlay(s string) bool {
	// The approval bar is drawn with a background fill before the prompt.
	// We do NOT assert a specific hue (it is quantized to the terminal's
	// color level — 24-bit, 256, or 16 — by the theme), so this checks for
	// any background SGR fill: a 48;2;R;G;B / 48;5;N / 4X code, surviving
	// until the prompt (i.e. not reset back to default 49 before it).
	hasBg := false
	for _, m := range sgrRe.FindAllStringSubmatch(s, -1) {
		parts := strings.Split(m[1], ";")
		if len(parts) == 1 && (parts[0] == "0" || parts[0] == "") {
			hasBg = false // full reset
			continue
		}
		for i, p := range parts {
			switch {
			case p == "48" && i+2 < len(parts):
				// 48;2;R;G;B or 48;5;N background
				hasBg = true
			case p == "49":
				hasBg = false
			case len(p) == 2 && p[0] == '4' && p[1] >= '1' && p[1] <= '7':
				hasBg = true
			}
		}
	}
	return hasBg
}

// assertApprovalBarRow checks the window that ends at the prompt: the bar is
// the only thing on its row and it is drawn on the amber background. The bar
// takes over an erased row — the row the 执行中 tool spinner owns (OnToolBegin
// arms it before the gate fires), so EraseLineRight lands just ahead of the
// prompt and the prompt starts at column 1, whatever was streamed before.
func assertApprovalBarRow(t *testing.T, win string) {
	t.Helper()
	i := strings.Index(win, approvalPrompt)
	if i < 0 {
		t.Fatalf("no approval prompt on screen: %q", win)
	}
	if !strings.Contains(win[:i], ansi.EraseLineRight) {
		t.Fatalf("bar's row was not erased before the bar: %q", win[:i])
	}
	head := ansi.Strip(win[:i])
	if !strings.HasSuffix(head, "\r") {
		t.Fatalf("bar not on a cleared row of its own: %q", head)
	}
	if !amberUnderlay(win[:i]) {
		t.Fatalf("approval prompt is not highlighted with the amber bar background: %q", win[:i])
	}
	assertContains(t, win[i:], approvalBarAsk)
}

// The reply streams and ends without a newline: the ask prompt must still
// begin on the row below it, not sit at the tail of the answer.
func TestApprovalBarOwnsRowAfterStreamedReply(t *testing.T) {
	target := filepath.Join(t.TempDir(), "appr-bar-tail.txt")
	_ = os.Remove(target)

	p, r := startApprovalBarCase(t, 0, []string{
		streamChunk("BAR_TAIL_WITHOUT_NEWLINE"),
		bashCallChunk("call_bar_tail", "touch "+target),
	}, "BAR_TAIL_DENIED")

	p.Write([]byte("ask something that streams first\r"))
	win := r.readUntil(t, approvalBarAsk, waitTimeout)
	assertContains(t, win, "BAR_TAIL_WITHOUT_NEWLINE")
	// The reply's last line stays complete: the bar lands on the row below
	// it (over the tool spinner), never at its tail.
	if !strings.Contains(ansi.Strip(win), "BAR_TAIL_WITHOUT_NEWLINE\r\n") {
		t.Fatalf("streamed text's last line disturbed: %q", ansi.Strip(win))
	}
	assertApprovalBarRow(t, win)
	// The gate parks the task, so the stream is quiet now: what follows the
	// prompt is the bar's own tail — the erase that carries the amber
	// background out to the right margin.
	assertContains(t, r.readQuiet(t, settle, waitTimeout), ansi.EraseLineRight)

	p.Write([]byte("n"))
	done := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	assertContains(t, done, "BAR_TAIL_DENIED")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("denied command executed anyway")
	}
}

// The round carries only a tool call, so the pre-output spinner is still
// armed on the row where the reply would have landed: the bar takes that row
// over in place (no dead spinner line above it) and the ticker stops
// repainting over it.
func TestApprovalBarReplacesArmedSpinner(t *testing.T) {
	target := filepath.Join(t.TempDir(), "appr-bar-spinner.txt")
	_ = os.Remove(target)

	p, r := startApprovalBarCase(t, 600*time.Millisecond, []string{
		bashCallChunk("call_bar_spin", "touch "+target),
	}, "BAR_SPINNER_DENIED")

	p.Write([]byte("ask something that only calls a tool\r"))
	spinner := r.readUntil(t, "正在思考", waitTimeout)
	if strings.Contains(spinner, approvalPrompt) {
		t.Fatalf("approval prompt appeared before the model could answer: %q", spinner)
	}
	win := r.readUntil(t, approvalBarAsk, waitTimeout)
	assertApprovalBarRow(t, win)

	p.Write([]byte("n"))
	done := r.readUntil(t, "\x1b[35m[AI]:", waitTimeout)
	assertContains(t, done, "BAR_SPINNER_DENIED")
	// The refusal round legitimately re-arms the pre-output spinner (the
	// engine's OnStep does that on every round start), so a frame may sit
	// in `done` ahead of the final answer. What must never happen is the
	// ticker still repainting after the task has ended and the fresh
	// prompt is on screen.
	if q := r.readQuiet(t, settle, waitTimeout); strings.Contains(ansi.Strip(q), "正在思考") {
		t.Fatalf("spinner ticker resumed over the answered prompt: %q", q)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("denied command executed anyway")
	}
}
