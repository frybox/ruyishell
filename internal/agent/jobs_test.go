package agent

// Job manager tests (§8.2): the head-tail buffer contract, the job
// lifecycle (natural exit, kill, eviction at the cap) and the job_output /
// job_kill tool contracts.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func waitJob(t *testing.T, j *Job) {
	t.Helper()
	select {
	case <-j.DoneCh():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish in time")
	}
}

func TestHeadTailBuffer(t *testing.T) {
	b := &headTailBuffer{}
	if n, err := b.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("write: %d %v", n, err)
	}
	if _, err := b.Write([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "hello world" {
		t.Fatalf("small writes must pass through: %q", got)
	}

	// Head saturates, then the tail keeps only the most recent bytes; the
	// elided middle is named in the marker.
	b2 := &headTailBuffer{}
	_, _ = b2.Write([]byte(strings.Repeat("a", jobHeadBytes)))
	_, _ = b2.Write([]byte(strings.Repeat("b", 200*1024)))
	out := b2.String()
	if !strings.HasPrefix(out, strings.Repeat("a", jobHeadBytes)) {
		t.Fatal("head must keep the first bytes verbatim")
	}
	if !strings.HasSuffix(out, strings.Repeat("b", jobTailBytes)) {
		t.Fatal("tail must keep the most recent bytes")
	}
	if !strings.Contains(out, "已省略 72 KB") {
		t.Fatalf("marker must name the elided size: …%s…", out[jobHeadBytes:jobHeadBytes+60])
	}
}

func TestJobLifecycle(t *testing.T) {
	jm := NewJobManager()
	j := jm.startProcess("", "echo hi")
	waitJob(t, j)
	if !j.Done() || j.Stopped() || j.ExitCode() != 0 {
		t.Fatalf("natural exit: done=%v stopped=%v code=%d", j.Done(), j.Stopped(), j.ExitCode())
	}
	if !strings.Contains(j.Output(), "hi") {
		t.Fatalf("output must capture stdout: %q", j.Output())
	}
	if j.Duration() <= 0 {
		t.Fatal("duration must be recorded")
	}
	if id := jm.register(j); id != 1 || jm.Get(1) != j {
		t.Fatalf("register: id=%d get=%v", id, jm.Get(1))
	}
	if jm.Get(2) != nil {
		t.Fatal("unknown id must miss")
	}
}

func TestJobKill(t *testing.T) {
	jm := NewJobManager()
	j := jm.startProcess("", sleepCmd(2))
	j.Kill()
	waitJob(t, j)
	if !j.Stopped() || j.ExitCode() != -1 {
		t.Fatalf("killed job: stopped=%v code=%d", j.Stopped(), j.ExitCode())
	}
	// Kill is idempotent, and the manager still tracks the corpse.
	j.Kill()
	id := jm.register(j)
	if jm.Get(id) != j {
		t.Fatal("killed job must stay registered")
	}
}

func TestJobManagerCapEviction(t *testing.T) {
	jm := NewJobManager()
	for i := 0; i < jobMax; i++ {
		j := jm.startProcess("", "exit 0")
		waitJob(t, j)
		jm.register(j)
	}
	j := jm.startProcess("", "exit 0")
	waitJob(t, j)
	id := jm.register(j)
	if jm.Get(1) != nil {
		t.Fatal("the oldest job must be evicted at the cap")
	}
	if jm.Get(id) != j {
		t.Fatal("the new job must be registered")
	}

	// No finished job available: the oldest running one is killed so the
	// cap always holds. The jobs have to still be running when the 51st
	// arrives — admitting 50 of them takes a measurable fraction of the
	// block, and waitJob's 5s bound has to cover the reaping.
	jm2 := NewJobManager()
	for i := 0; i < jobMax; i++ {
		jm2.register(jm2.startProcess("", sleepCmd(4)))
	}
	oldest := jm2.Get(1)
	extra := jm2.startProcess("", "exit 0")
	waitJob(t, extra)
	jm2.register(extra)
	waitJob(t, oldest)
	if jm2.Get(1) != nil || !oldest.Stopped() {
		t.Fatalf("oldest running job must be evicted and killed: get=%v stopped=%v",
			jm2.Get(1), oldest.Stopped())
	}
	if jm2.Get(jobMax+1) != extra {
		t.Fatal("the new job must be registered")
	}
	// Don't leak the 49 remaining jobs past the test. On Windows this only
	// waits out their countdown (see sleepCmd), not a kill.
	jm2.StopAll()
	for id := 2; id <= jobMax; id++ {
		waitJob(t, jm2.Get(id))
	}
}

func TestJobOutputTool(t *testing.T) {
	jm := NewJobManager()
	tool := jobOutputTool(jm)
	task := &Task{}

	// A running job reports status plus the output so far.
	running := jm.startProcess("", echoLinesCmd(3, "l1", "l2", "l3"))
	id := jm.register(running)
	deadline := time.Now().Add(3 * time.Second)
	for running.Output() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	res, err := tool.Execute(context.Background(), task, map[string]any{"id": float64(id)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "仍在运行") || !strings.Contains(res.Output, "l3") {
		t.Fatalf("running report wrong: %s", res.Output)
	}

	// A finished job reports the exit code and carries it in Code.
	done := jm.startProcess("", "echo bye")
	id = jm.register(done)
	waitJob(t, done)
	res, err = tool.Execute(context.Background(), task, map[string]any{"id": float64(id)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "已结束 · exit 0") || res.Code != 0 || !strings.Contains(res.Output, "bye") {
		t.Fatalf("done report wrong: %s", res.Output)
	}

	// A killed job says so.
	running.Kill()
	waitJob(t, running)
	res, err = tool.Execute(context.Background(), task, map[string]any{"id": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "已被终止") {
		t.Fatalf("killed report wrong: %s", res.Output)
	}

	// Unknown ids and missing arguments are tool errors.
	if _, err := tool.Execute(context.Background(), task, map[string]any{"id": float64(999)}); err == nil {
		t.Fatal("unknown id must fail")
	}
	if _, err := tool.Execute(context.Background(), task, map[string]any{}); err == nil {
		t.Fatal("missing id must fail")
	}

	// The lines cap trims from the top.
	many := jm.startProcess("", countCmd(500))
	id = jm.register(many)
	waitJob(t, many)
	res, err = tool.Execute(context.Background(), task, map[string]any{"id": float64(id), "lines": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "仅显示尾 10 行") || !strings.Contains(res.Output, "\n500") {
		t.Fatalf("tail cap wrong: %s", res.Output)
	}
}

func TestJobKillTool(t *testing.T) {
	jm := NewJobManager()
	tool := jobKillTool(jm)
	task := &Task{}

	j := jm.startProcess("", sleepCmd(2))
	id := jm.register(j)
	res, err := tool.Execute(context.Background(), task, map[string]any{"id": float64(id)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "已终止") {
		t.Fatalf("kill report wrong: %s", res.Output)
	}
	waitJob(t, j)
	if !j.Stopped() {
		t.Fatal("job_kill must stop the job")
	}

	// A finished job refuses the kill with its exit code.
	done := jm.startProcess("", "exit 7")
	id = jm.register(done)
	waitJob(t, done)
	res, err = tool.Execute(context.Background(), task, map[string]any{"id": float64(id)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "无需终止") || res.Code != 7 {
		t.Fatalf("finished-kill report wrong: %s", res.Output)
	}

	if _, err := tool.Execute(context.Background(), task, map[string]any{"id": float64(999)}); err == nil {
		t.Fatal("unknown id must fail")
	}
}

func TestTailLines(t *testing.T) {
	if got := tailLines("", 5); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := tailLines("a\nb\nc", 5); got != "a\nb\nc" {
		t.Fatalf("under cap: %q", got)
	}
	got := tailLines("1\n2\n3\n4\n5", 3)
	want := fmt.Sprintf("…（仅显示尾 %d 行）…\n3\n4\n5", 3)
	if got != want {
		t.Fatalf("over cap: %q", got)
	}
}
