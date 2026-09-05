package agent

// Unit tests for the loop guards, L1 collapse and engine helpers, ported
// from the pre-engine cmd/rysh/loopguard_test.go and adapted to TurnMsg /
// Result (M7.2). The end-to-end guard behavior is covered by the engine
// tests and the cmd/rysh integration tests.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"ruyishell/internal/provider"
)

func entry(cmd string, kind entryKind) loopEntry {
	return loopEntry{sig: toolSignature("bash", cmd, 0, "out"), kind: kind}
}

func TestToolSignatureRepeatsOnIdenticalRuns(t *testing.T) {
	a := toolSignature("bash", "echo hi", 0, "hi\n")
	b := toolSignature("bash", "echo   hi", 0, "hi\n") // whitespace squashed
	if a != b {
		t.Fatalf("whitespace-normalized commands must share a signature:\n%s\n%s", a, b)
	}
	if c := toolSignature("bash", "echo hi", 1, "hi\n"); c == a {
		t.Fatal("exit code must be part of the signature")
	}
	if c := toolSignature("bash", "echo hi", 0, "different\n"); c == a {
		t.Fatal("output head must be part of the signature")
	}
	if c := toolSignature("ls", "echo hi", 0, "hi\n"); c == a {
		t.Fatal("tool name must be part of the signature")
	}
}

func TestToolSignatureIgnoresOutputBeyondPrefix(t *testing.T) {
	filler := strings.Repeat("x", sigPrefixBytes)
	a := toolSignature("bash", "tail -f log", 0, filler+"A")
	b := toolSignature("bash", "tail -f log", 0, filler+"B")
	if a != b {
		t.Fatal("output differences beyond sigPrefixBytes must not change the signature")
	}
}

func TestNormalizeArgsKeyOrderInsensitive(t *testing.T) {
	a := normalizeArgs(`{"command":"ls","workdir":"/tmp"}`)
	b := normalizeArgs(`{"workdir":"/tmp","command":"ls"}`)
	if a != b {
		t.Fatalf("key order must not change normalized args:\n%s\n%s", a, b)
	}
	if c := normalizeArgs(`not json at all`); c != "not json at all" {
		t.Fatalf("broken JSON must fall back to squashed text, got %q", c)
	}
}

func TestReadSignatureIgnoresOutput(t *testing.T) {
	a := readSignature("read", normalizeArgs(`{"path":"/tmp/f"}`))
	b := readSignature("read", normalizeArgs(`{"path":"/tmp/f"}`))
	if a != b {
		t.Fatal("same target must share a read signature")
	}
	if c := readSignature("read", normalizeArgs(`{"path":"/tmp/g"}`)); c == a {
		t.Fatal("different targets must differ")
	}
}

func TestBashLooksReadOnly(t *testing.T) {
	readOnly := []string{
		"ls", "ls -la /tmp", "cat /etc/hostname", "head -5 f", "tail -n 20 log",
		"grep -rn TODO .", "find . -name '*.go'", "pwd", "git status",
		"git log --oneline", "git diff", "go version", "wc -l main.go",
	}
	for _, cmd := range readOnly {
		if !bashLooksReadOnly(cmd) {
			t.Errorf("bashLooksReadOnly(%q) = false, want true", cmd)
		}
	}
	writers := []string{
		"", "rm -rf /", "mv a b", "cp a b", "echo hi > f", "cat f | tee g",
		"ls; rm x", "find . -name x -delete", "git push", "git commit -m x",
		"go build ./...", "make", "curl example.com", "$(rm x)",
	}
	for _, cmd := range writers {
		if bashLooksReadOnly(cmd) {
			t.Errorf("bashLooksReadOnly(%q) = true, want false", cmd)
		}
	}
}

func TestLoopGuardWarnBreakAndShare(t *testing.T) {
	var g loopGuard
	now := time.Unix(0, 0)
	e := entry("echo LOOP_AGENT", kindOther)
	for i := 1; i < loopWarnConsec; i++ {
		if lr := g.record(e, i, now); lr.Consec >= loopWarnConsec {
			t.Fatalf("warn fired early at run %d", lr.Consec)
		}
	}
	lr := g.record(e, 2, now)
	if lr.Consec != loopWarnConsec {
		t.Fatalf("Consec = %d, want %d", lr.Consec, loopWarnConsec)
	}
	if lr.Share != 0 {
		t.Fatalf("Share must stay 0 while the window is unsaturated, got %v", lr.Share)
	}
	var g2 loopGuard
	for i := 1; i <= loopBreakConsec; i++ {
		lr = g2.record(e, i, now)
	}
	if lr.Consec != loopBreakConsec {
		t.Fatalf("Consec = %d, want %d", lr.Consec, loopBreakConsec)
	}
	// Share itself is covered by the interleaved-loop test below; here a
	// six-run streak inside an unsaturated window keeps Share at 0.
	if lr.Share != 0 {
		t.Fatalf("unsaturated window Share = %v, want 0", lr.Share)
	}
}

// An interleaved loop (A A B A A B…) never builds a long consecutive run;
// once the window saturates, the signature share catches it.
func TestLoopGuardShareCatchesInterleavedLoop(t *testing.T) {
	var g loopGuard
	now := time.Unix(0, 0)
	a := entry("echo A", kindOther)
	b := entry("echo B", kindOther)
	var lr loopResult
	for i := 1; i <= loopWindow; i++ {
		if i%3 == 0 {
			lr = g.record(b, i, now)
		} else {
			lr = g.record(a, i, now)
		}
	}
	if lr.Share <= loopShareLimit {
		t.Fatalf("interleaved loop share = %v, want > %v", lr.Share, loopShareLimit)
	}
	if lr.Consec > loopBreakConsec {
		t.Fatalf("Consec = %d, the consecutive run must stay below the break bound", lr.Consec)
	}
}

func TestLoopGuardWindowSlides(t *testing.T) {
	var g loopGuard
	now := time.Unix(0, 0)
	a := entry("echo A", kindOther)
	for i := 1; i <= loopWindow; i++ {
		g.record(a, i, now)
	}
	// loopWindow+1 distinct commands evict every A.
	var lr loopResult
	for i := loopWindow + 1; i <= loopWindow+loopWindow; i++ {
		lr = g.record(entry(fmt.Sprintf("cmd %d", i), kindOther), i, now)
	}
	if lr.Consec != 1 {
		t.Fatalf("after the window slid, Consec = %d, want 1", lr.Consec)
	}
	for _, w := range g.window {
		if w.sig == a.sig {
			t.Fatal("evicted signature still in the window")
		}
	}
}

func TestLoopGuardReadCountNonConsecutive(t *testing.T) {
	var g loopGuard
	now := time.Unix(0, 0)
	read := loopEntry{
		sig:     toolSignature("read", `{"path":"/tmp/f"}`, 0, "v1"),
		readSig: readSignature("read", `{"path":"/tmp/f"}`),
		kind:    kindRead,
	}
	other := entry("ls", kindOther)
	lr := g.record(read, 1, now)
	g.record(other, 2, now)
	// The read signature counts across interleaved entries; the output
	// changed each time, so the repetition signature never accumulates.
	lr = g.record(loopEntry{
		sig:     toolSignature("read", `{"path":"/tmp/f"}`, 0, "v2"),
		readSig: read.readSig,
		kind:    kindRead,
	}, 3, now)
	if lr.Consec != 1 {
		t.Fatalf("changed output must not extend the repetition run, got %d", lr.Consec)
	}
	if lr.ReadCount != 2 {
		t.Fatalf("ReadCount = %d, want 2", lr.ReadCount)
	}
	lr = g.record(read, 4, now)
	if lr.ReadCount != readChurnRepeat {
		t.Fatalf("ReadCount = %d, want %d (churn warn threshold)", lr.ReadCount, readChurnRepeat)
	}
}

func TestWriteStalledNeedsSaturatedWindow(t *testing.T) {
	var g loopGuard
	now := time.Unix(0, 0)
	read := loopEntry{
		sig:     toolSignature("read", `{"path":"/tmp/f"}`, 0, "x"),
		readSig: readSignature("read", `{"path":"/tmp/f"}`),
		kind:    kindRead,
	}
	// Unsaturated window: never stalled, no matter how stale the write.
	for i := 1; i < loopWindow; i++ {
		g.record(read, i, now)
	}
	if g.writeStalled(1000, now.Add(time.Hour)) {
		t.Fatal("stall reported before the window saturated")
	}
	// Saturated, read-dominated, no write since step 1: stalled.
	g.record(read, loopWindow, now)
	if !g.writeStalled(loopWindow+writeStallSteps, now.Add(writeStallWindow)) {
		t.Fatal("read-dominated saturated window without writes must stall")
	}
	// A recent write resets the stall: within writeStallSteps of the
	// write there is no stall, but it returns once the write goes stale.
	var g2 loopGuard
	for i := 1; i < loopWindow; i++ {
		g2.record(read, i, now)
	}
	g2.record(entry("echo > f", kindWrite), loopWindow, now)
	if g2.writeStalled(loopWindow+writeStallSteps-1, now.Add(writeStallWindow-time.Second)) {
		t.Fatal("a recent write must reset the stall")
	}
	if !g2.writeStalled(loopWindow+writeStallSteps, now.Add(writeStallWindow)) {
		t.Fatal("stall must return once the write goes stale")
	}
	// Read share at or below the limit: not churn even with a stale write.
	var g3 loopGuard
	for i := 1; i <= loopWindow; i++ {
		if i%3 == 0 {
			g3.record(entry("echo > f", kindWrite), i, now)
		} else {
			g3.record(read, i, now)
		}
	}
	if g3.writeStalled(loopWindow+writeStallSteps, now.Add(time.Hour)) {
		t.Fatal("window with ≤ readShareLimit reads must not stall")
	}
}

func TestCollapseOldToolsKeepsNewestTail(t *testing.T) {
	small := func(cmd string) TurnMsg {
		return TurnMsg{
			Msg:  provider.ChatMessage{Role: "tool", Content: "body of " + cmd},
			Tool: &ToolMeta{Cmd: cmd, Exit: 0, Size: 1024},
		}
	}
	big := TurnMsg{
		Msg:  provider.ChatMessage{Role: "tool", Content: strings.Repeat("a", 64000)},
		Tool: &ToolMeta{Cmd: "head -c 20000 /dev/zero", Exit: 0, Size: 64000},
	}
	turn := []TurnMsg{
		{Msg: provider.ChatMessage{Role: "user", Content: "go"}},
		big, // folds: the budget behind two 1KB results cannot hold it
		small("c"),
		small("d"),
		{Msg: provider.ChatMessage{Role: "assistant", Content: "done"}},
	}
	orig := []TurnMsg{turn[0], turn[1], turn[2], turn[3], turn[4]}
	out := CollapseOldTools(turn)
	if got := out[1].Msg.Content; !strings.HasPrefix(got, "[已省略] $ head -c 20000 /dev/zero (exit 0)，原始输出 63 KB 已折叠") {
		t.Fatalf("oversized result must fold to the placeholder, got %.80q", got)
	}
	for i, want := range []string{"body of c", "body of d"} {
		if got := out[2+i].Msg.Content; got != want {
			t.Fatalf("newest results must stay verbatim: out[%d] = %q", 2+i, got)
		}
	}
	if out[0].Msg.Role != "user" || out[4].Msg.Content != "done" {
		t.Fatal("non-tool messages must pass through untouched")
	}
	for i := range orig {
		if orig[i].Msg.Content != turn[i].Msg.Content {
			t.Fatalf("CollapseOldTools modified the input at %d", i)
		}
	}
}

func TestCollapseOldToolsFoldsOldestBeyondBudget(t *testing.T) {
	mk := func(body string) TurnMsg {
		return TurnMsg{
			Msg:  provider.ChatMessage{Role: "tool", Content: body},
			Tool: &ToolMeta{Cmd: "cat " + body, Exit: 0, Size: len(body)},
		}
	}
	turn := []TurnMsg{mk(strings.Repeat("a", 40000)), mk(strings.Repeat("b", 40000))}
	out := CollapseOldTools(turn)
	if !strings.HasPrefix(out[0].Msg.Content, "[已省略] $ cat aaaa") {
		t.Fatalf("oldest result beyond the budget must fold, got %.40q", out[0].Msg.Content)
	}
	if out[1].Msg.Content != strings.Repeat("b", 40000) {
		t.Fatal("newest result within the budget must stay verbatim")
	}
}

func TestCollapseOldToolsEmptyAndNoTools(t *testing.T) {
	plain := []TurnMsg{{Msg: provider.ChatMessage{Role: "user", Content: "hi"}}}
	out := CollapseOldTools(plain)
	if len(out) != 1 || out[0].Msg.Content != "hi" {
		t.Fatal("tool-less turn must pass through unchanged")
	}
	if got := CollapseOldTools(nil); len(got) != 0 {
		t.Fatal("nil turn must yield nil/empty")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int]string{0: "0 B", 512: "512 B", 1023: "1023 B", 1024: "1 KB", 20000: "20 KB", 65536: "64 KB"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFedContent(t *testing.T) {
	if got := FedContent("bash", Result{Meta: "exit 0", Output: "hello"}); got != "[bash] exit 0\nhello" {
		t.Fatalf("FedContent with output = %q", got)
	}
	if got := FedContent("bash", Result{Meta: "exit 1"}); got != "[bash] exit 1\n(无输出)" {
		t.Fatalf("FedContent without output = %q", got)
	}
}

func TestRetryWaitClassification(t *testing.T) {
	cfg := Config{RetryBase: time.Second, RetryCap: 10 * time.Second, RetryAttempts: 3}
	nonRetryable := []error{
		context.Canceled,
		context.DeadlineExceeded,
		&provider.StatusError{Code: 400, Status: "400 Bad Request", Body: "bad"},
		&provider.StatusError{Code: 401, Status: "401 Unauthorized", Body: "no"},
		&provider.StatusError{Code: 404, Status: "404 Not Found", Body: "gone"},
		&provider.ToolsUnsupportedError{Status: "400 Bad Request", Body: "no tools"},
		errors.New("some transport weirdness is still retried"), // see retryable below
	}
	for i, err := range nonRetryable[:6] {
		if _, ok := retryWait(err, 0, cfg); ok {
			t.Errorf("nonRetryable[%d] %v must not retry", i, err)
		}
	}
	retryable := []error{
		&provider.StatusError{Code: 429, Status: "429 Too Many Requests", Body: "slow down"},
		&provider.StatusError{Code: 500, Status: "500 Internal Server Error", Body: "boom"},
		&provider.StatusError{Code: 503, Status: "503 Service Unavailable", Body: "later"},
		errors.New("connection reset by peer"),
	}
	for i, err := range retryable {
		if _, ok := retryWait(err, 0, cfg); !ok {
			t.Errorf("retryable[%d] %v must retry", i, err)
		}
	}
}

func TestRetryWaitBackoffAndCaps(t *testing.T) {
	cfg := Config{RetryBase: time.Second, RetryCap: 5 * time.Second}
	err := &provider.StatusError{Code: 500, Status: "500", Body: "x"}
	for attempt := 0; attempt < 10; attempt++ {
		wait, ok := retryWait(err, attempt, cfg)
		if !ok {
			t.Fatalf("attempt %d must be retryable", attempt)
		}
		if wait > cfg.RetryCap {
			t.Fatalf("attempt %d wait %v exceeds cap %v", attempt, wait, cfg.RetryCap)
		}
		// ±25% jitter around base·2^attempt.
		ceiling := cfg.RetryBase * time.Duration(1<<min(attempt, 16)) * 5 / 4
		if wait > min(ceiling, cfg.RetryCap) {
			t.Fatalf("attempt %d wait %v exceeds jitter ceiling %v", attempt, wait, ceiling)
		}
	}
	// Retry-After wins over the backoff when it is larger.
	ra := &provider.StatusError{Code: 429, Status: "429", Body: "x", RetryAfter: 30 * time.Second}
	wait, ok := retryWait(ra, 0, cfg)
	if !ok || wait != 30*time.Second {
		t.Fatalf("Retry-After must win: wait=%v ok=%v", wait, ok)
	}
}

func TestBriefErr(t *testing.T) {
	if got := briefErr(&provider.StatusError{Status: "502 Bad Gateway", Body: "x"}); got != "502 Bad Gateway" {
		t.Fatalf("briefErr(status) = %q", got)
	}
	long := strings.Repeat("字", 100)
	got := briefErr(errors.New(long))
	if n := len([]rune(got)); n > 81 {
		t.Fatalf("briefErr must truncate long messages, got %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated message must end with …, got %q", got)
	}
}

func TestHTTPStatusUsedInClassification(t *testing.T) {
	// Sanity: the 429 special case survives refactors — anything 4xx
	// other than 429 is fatal even though it is a StatusError.
	if _, ok := retryWait(&provider.StatusError{Code: http.StatusConflict, Status: "409"}, 0, Config{}); ok {
		t.Fatal("409 must not retry")
	}
}
