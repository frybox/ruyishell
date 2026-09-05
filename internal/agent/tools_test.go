package agent

// Tool-level tests (§5.3): each tool against a temp workspace. The engine
// tests cover how results feed back; here the tools' own contracts matter —
// line limits, exact-match editing, gitignore-aware enumeration, timeouts.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func runTool(t *testing.T, dir, name string, rawArgs string) (Result, error) {
	t.Helper()
	return runToolWith(t, dir, name, rawArgs, 10*time.Second, 10*time.Second)
}

// runToolWith builds the registry with explicit bash timeouts (whole
// seconds — the schema math truncates to seconds, so sub-second maxes
// would clamp every call to zero).
func runToolWith(t *testing.T, dir, name string, rawArgs string, bashDef, bashMax time.Duration) (Result, error) {
	t.Helper()
	tool := findTool(defaultToolsWith(dir, ToolOpts{BashTimeout: bashDef, BashMaxTimeout: bashMax}), name)
	if tool == nil {
		t.Fatalf("tool %q not in the default registry", name)
	}
	args, err := parseToolArgs(rawArgs)
	if err != nil {
		t.Fatalf("parseToolArgs(%s): %v", rawArgs, err)
	}
	return tool.Execute(context.Background(), &Task{cwd: dir}, args)
}

func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("hello.txt", "alpha\nbeta\ngamma\n")
	mustWrite("src/main.go", "package main\n\nfunc main() {}\n")
	mustWrite("src/util/helper.go", "package util\n")
	return dir
}

func TestReadToolLinesAndLimits(t *testing.T) {
	dir := writeTree(t)

	res, err := runTool(t, dir, "read", `{"path":"hello.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1→alpha", "2→beta", "3→gamma"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output missing %q: %s", want, res.Output)
		}
	}

	// offset/limit windows are 1-based and bounded.
	res, err = runTool(t, dir, "read", `{"path":"hello.txt","offset":2,"limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "2→beta") || strings.Contains(res.Output, "alpha") {
		t.Fatalf("offset/limit window wrong: %s", res.Output)
	}

	// A limit above the cap is clamped, not rejected.
	if _, err := runTool(t, dir, "read", `{"path":"hello.txt","limit":99999}`); err != nil {
		t.Fatalf("oversized limit must clamp: %v", err)
	}

	// Directories and missing files are errors, not empty reads.
	if _, err := runTool(t, dir, "read", `{"path":"src"}`); err == nil {
		t.Fatal("reading a directory must fail")
	}
	if _, err := runTool(t, dir, "read", `{"path":"nope.txt"}`); err == nil {
		t.Fatal("reading a missing file must fail")
	}
}

func TestReadToolByteCap(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 1000) + "\n"
	body := strings.Repeat(big, 60) // 60 KB over the 50 KB read cap
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := runTool(t, dir, "read", `{"path":"big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Output) > readMaxBytes+200 || !strings.Contains(res.Output, "已截断") {
		t.Fatalf("output must truncate at the byte cap with a continuation hint, got %d bytes", len(res.Output))
	}
}

func TestWriteToolRequiresParent(t *testing.T) {
	dir := t.TempDir()

	res, err := runTool(t, dir, "write", `{"path":"new.txt","content":"hi\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(data) != "hi\n" {
		t.Fatalf("write did not land: %v %q", err, data)
	}
	if res.Code != 0 {
		t.Fatalf("successful write must carry code 0, got %d", res.Code)
	}

	// No mkdir: a missing parent directory is an error.
	if _, err := runTool(t, dir, "write", `{"path":"a/b/c.txt","content":"x"}`); err == nil {
		t.Fatal("write into a missing parent must fail (no auto-mkdir)")
	}
}

func TestEditToolFourStates(t *testing.T) {
	dir := writeTree(t)

	// 1) unique match → replaced.
	if _, err := runTool(t, dir, "edit", `{"path":"hello.txt","old_string":"beta","new_string":"BETA"}`); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if !strings.Contains(string(data), "BETA") {
		t.Fatalf("edit did not apply: %s", data)
	}

	// 2) zero matches → error, file untouched.
	if _, err := runTool(t, dir, "edit", `{"path":"hello.txt","old_string":"zzz","new_string":"y"}`); err == nil {
		t.Fatal("missing old_string must fail")
	}
	// 3) multiple matches without replace_all → error.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("same same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(t, dir, "edit", `{"path":"hello.txt","old_string":"same","new_string":"diff"}`); err == nil {
		t.Fatal("ambiguous old_string must fail without replace_all")
	}
	data, _ = os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(data) != "same same\n" {
		t.Fatalf("failed edit must not write: %s", data)
	}
	// 4) replace_all → every occurrence replaced.
	if _, err := runTool(t, dir, "edit", `{"path":"hello.txt","old_string":"same","new_string":"diff","replace_all":true}`); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(data) != "diff diff\n" {
		t.Fatalf("replace_all result = %s", data)
	}
}

func TestLsToolTree(t *testing.T) {
	dir := writeTree(t)
	res, err := runTool(t, dir, "ls", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"src/", "main.go", "helper.go", "hello.txt"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("ls output missing %q: %s", want, res.Output)
		}
	}
}

func TestGlobToolDoubleStar(t *testing.T) {
	dir := writeTree(t)

	res, err := runTool(t, dir, "glob", `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "src/main.go") || !strings.Contains(res.Output, "src/util/helper.go") {
		t.Fatalf("** must match nested paths: %s", res.Output)
	}
	if strings.Contains(res.Output, "hello.txt") {
		t.Fatalf("non-matching files must stay out: %s", res.Output)
	}

	// A single-star pattern stays within its segment.
	res, err = runTool(t, dir, "glob", `{"pattern":"*.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "hello.txt") || strings.Contains(res.Output, ".go") {
		t.Fatalf("*.txt must match only top-level txt: %s", res.Output)
	}

	// The unit-level matcher: ** also matches zero segments.
	if !matchGlobPattern("src/**/helper.go", "src/helper.go") {
		t.Fatal("** must match zero segments")
	}
	if matchGlobPattern("src/*.go", "src/util/helper.go") {
		t.Fatal("* must not cross path separators")
	}
}

func TestGrepToolMatches(t *testing.T) {
	dir := writeTree(t)

	res, err := runTool(t, dir, "grep", `{"pattern":"package","glob":"*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "src/main.go:1:package main") {
		t.Fatalf("rg scan output wrong: %s", res.Output)
	}
	if strings.Contains(res.Output, "hello.txt") {
		t.Fatalf("glob must narrow the file set: %s", res.Output)
	}

	// No matches: a valid empty result.
	res, err = runTool(t, dir, "grep", `{"pattern":"nothing-matches-this"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "" && !strings.Contains(res.Output, "0 处命中") {
		t.Fatalf("no-match run must be empty, got %s", res.Output)
	}

	// An invalid regex is a reported failure, not a crash.
	if _, err := runTool(t, dir, "grep", `{"pattern":"([unclosed"}`); err == nil {
		t.Fatal("invalid regex must fail")
	}
}

// grepScan must agree with rg when rg is unavailable: force the fallback
// by pointing the scan directly at a temp dir (no rg involved there), and
// compare semantics on the same tree via matchGlobPattern filtering.
func TestGrepScanFallback(t *testing.T) {
	dir := writeTree(t)
	re := regexp.MustCompile("func")
	out, err := grepScan(context.Background(), re, dir, "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src/main.go:3:func main() {}") {
		t.Fatalf("fallback scan output wrong: %s", out)
	}
	if strings.Contains(out, "hello.txt") {
		t.Fatalf("fallback must honor the glob filter: %s", out)
	}
}

func TestBashToolTimeout(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	res, err := runToolWith(t, dir, "bash", fmt.Sprintf(`{"command":%q,"timeout":1}`, sleepCmd(2)), time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout not honored, took %v", elapsed)
	}
	if res.Code == 0 || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("a timed-out call must be marked, got code %d out %s", res.Code, res.Output)
	}

	// A model-side timeout above the configured max is clamped to it: the
	// command has to outlive the clamped 2s to prove the clamp fired the
	// timeout rather than the command ending first.
	res, err = runToolWith(t, dir, "bash", fmt.Sprintf(`{"command":%q,"timeout":999}`, sleepCmd(3)), time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Fatalf("clamped timeout must still fire, got %s", res.Output)
	}
}

func TestBashToolWorkdirAndErr(t *testing.T) {
	dir := writeTree(t)

	res, err := runTool(t, dir, "bash", fmt.Sprintf(`{"command":%q,"workdir":"src"}`, pwdCmd()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Output), filepath.FromSlash("/src")) {
		t.Fatalf("workdir not honored: %s", res.Output)
	}

	res, err = runTool(t, dir, "bash", fmt.Sprintf(`{"command":%q}`, stderrExitCmd("oops", 3)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != 3 {
		t.Fatalf("exit code must propagate, got %d", res.Code)
	}
	if !strings.Contains(res.Output, "oops") {
		t.Fatalf("stderr must be captured, got %s", res.Output)
	}
}

// §8.1: a command still running when the auto-background window closes
// becomes a managed job — the tool returns a pointer result at once, and
// the job keeps running until killed.
func TestBashToolAutoBackground(t *testing.T) {
	dir := t.TempDir()
	jm := NewJobManager()
	tool := findTool(defaultToolsWith(dir, ToolOpts{
		BashTimeout:    10 * time.Second,
		BashMaxTimeout: 10 * time.Second,
		Jobs:           jm,
		AutoBackground: 100 * time.Millisecond,
	}), "bash")
	if tool == nil {
		t.Fatal("bash tool missing")
	}

	start := time.Now()
	res, err := runParsed(t, tool, fmt.Sprintf(`{"command":%q}`, sleepCmd(2)))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("auto-background must return at the window, took %v", elapsed)
	}
	if !strings.Contains(res.Output, "已转后台 job 1") || !strings.Contains(res.Output, "job_output") {
		t.Fatalf("backgrounded result must point at job_output: %s", res.Output)
	}
	job := jm.Get(1)
	if job == nil || job.Done() {
		t.Fatalf("the job must be registered and running: %v", job)
	}
	job.Kill()
	waitJob(t, job)

	// A command finishing inside the window renders the normal inline
	// shape (combined output through the job path).
	res, err = runParsed(t, tool, `{"command":"echo inline-fast"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "inline-fast") || strings.Contains(res.Output, "job") {
		t.Fatalf("quick command must finish inline: %s", res.Output)
	}
	if res.Code != 0 {
		t.Fatalf("inline code: %d", res.Code)
	}
}

// §8.1: the per-call timeout still fires with jobs enabled — the job is
// reaped and the result carries the timeout mark.
func TestBashToolTimeoutViaJobs(t *testing.T) {
	dir := t.TempDir()
	jm := NewJobManager()
	tool := findTool(defaultToolsWith(dir, ToolOpts{
		BashTimeout:    time.Second,
		BashMaxTimeout: 10 * time.Second,
		Jobs:           jm,
		AutoBackground: 10 * time.Second, // window wider than the timeout
	}), "bash")
	start := time.Now()
	res, err := runParsed(t, tool, fmt.Sprintf(`{"command":%q,"timeout":1}`, sleepCmd(2)))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout not honored, took %v", elapsed)
	}
	if res.Code == 0 || !strings.Contains(res.Output, "timed out") {
		t.Fatalf("timed-out shape wrong: code %d out %s", res.Code, res.Output)
	}
	// The timeout path kills and reaps the speculatively started job
	// inline (synchronously) — nothing was ever registered with the
	// manager, so no live job is left behind.
	if j := jm.Get(1); j != nil {
		t.Fatalf("the timed-out run must not leave a job behind: %+v", j)
	}
}

func runParsed(t *testing.T, tool *Tool, rawArgs string) (Result, error) {
	t.Helper()
	args, err := parseToolArgs(rawArgs)
	if err != nil {
		t.Fatalf("parseToolArgs(%s): %v", rawArgs, err)
	}
	return tool.Execute(context.Background(), &Task{}, args)
}

func TestTodoToolValidation(t *testing.T) {
	dir := t.TempDir()
	task := &Task{cwd: dir}

	tool := findTool(defaultToolsWith(dir, ToolOpts{BashTimeout: time.Second, BashMaxTimeout: 10 * time.Second}), "todo")
	if tool == nil {
		t.Fatal("todo tool missing from the registry")
	}

	good := `{"todos":[{"id":"1","content":"step one","status":"in_progress"},{"id":"2","content":"step two","status":"pending"}]}`
	args, err := parseToolArgs(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), task, args); err != nil {
		t.Fatal(err)
	}
	if got := task.Todos(); len(got) != 2 || got[0].Status != "in_progress" {
		t.Fatalf("todos not stored: %+v", got)
	}

	bad := []string{
		`{"todos":"nope"}`,
		`{"todos":[{"id":"","content":"x","status":"pending"}]}`,
		`{"todos":[{"id":"1","content":"","status":"pending"}]}`,
		`{"todos":[{"id":"1","content":"x","status":"done-tomorrow"}]}`,
		`{"todos":[{"id":"1","content":"x"}]}`,
	}
	for _, raw := range bad {
		args, err := parseToolArgs(raw)
		if err != nil {
			continue // broken JSON is caught at parse time — also a rejection
		}
		if _, err := tool.Execute(context.Background(), task, args); err == nil {
			t.Fatalf("invalid todos must be rejected: %s", raw)
		}
	}
}
