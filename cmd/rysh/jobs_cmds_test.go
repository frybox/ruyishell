package main

// Table tests for the /jobs slash command. The four commands share one
// formatted function over a real JobManager and real processes, so the
// tests spell each acceptance case as a table row: an empty manager, a live
// job, a finished job, a kill, and the two error paths. No output is
// printed by the code under test (it returns lines), which is what makes
// the listing assertable at all.

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"ruyishell/internal/agent"
	"ruyishell/internal/i18n"
)

// jobsSleepCmd is the sleep fixture for the shell the job will actually
// run under; unlike the agent package's helper (unexported to tests beyond
// it), this one only has to exist on the platforms these tests run on.
func jobsSleepCmd(secs int) string {
	if runtime.GOOS == "windows" {
		return "ping -n " + strconv.Itoa(secs+1) + " 127.0.0.1 >nul 2>&1"
	}
	return "sleep " + strconv.Itoa(secs)
}

func waitJobDone(t *testing.T, j *agent.Job) {
	t.Helper()
	select {
	case <-j.DoneCh():
	case <-time.After(10 * time.Second):
		t.Fatal("job did not finish in time")
	}
}

// assertLines joins the command's lines so a containment check reads the
// same whether the text landed on the header, a row, or a bare notice.
func joinLines(lines []string) string { return strings.Join(lines, "\n") }

// TestJobsCommandTable covers the list and kill forms against a manager
// holding one live, one finished and one killed job: (a) the empty-state
// notice, (b) the running row, (c) the finished row with its exit code,
// (d) kill on a running job, (e) the two miss paths, plus the usage forms.
func TestJobsCommandTable(t *testing.T) {
	zh := i18n.New(i18n.LangZh)

	// null manager: the empty state, not a panic.
	if got := joinLines(jobsCommand(nil, []string{"/jobs"}, zh)); !strings.Contains(got, "没有后台 job") {
		t.Errorf("nil manager: %q must report no jobs", got)
	}
	if got := joinLines(jobsCommand(agent.NewJobManager(), []string{"/jobs"}, zh)); !strings.Contains(got, "没有后台 job") {
		t.Errorf("empty manager: %q must report no jobs", got)
	}

	jm := agent.NewJobManager()
	defer jm.StopAll()
	live, liveID := jm.StartBackground("", jobsSleepCmd(30))
	liveCmd := live.Command

	finished, finID := jm.StartBackground("", "exit 3")
	waitJobDone(t, finished)

	stopped, stopID := jm.StartBackground("", jobsSleepCmd(30))
	stopped.Kill()
	waitJobDone(t, stopped)

	cases := []struct {
		name string
		args []string
		want []string // every fragment must appear in the joined output
		deny []string // fragments that must NOT appear
	}{
		{
			name: "list: header counts one running of three",
			args: []string{"/jobs"},
			want: []string{"后台 job（共 3 个 · 运行中 1 个）"},
		},
		{
			name: "b: running job row carries id, state and command",
			args: []string{"/jobs"},
			want: []string{"job " + strconv.Itoa(liveID) + "  运行中", liveCmd},
		},
		{
			name: "c: finished job row carries state and exit code",
			args: []string{"/jobs"},
			want: []string{"job " + strconv.Itoa(finID) + "  已结束  退出码 3"},
		},
		{
			name: "killed job reports stopped without a code",
			args: []string{"/jobs"},
			want: []string{"job " + strconv.Itoa(stopID) + "  已停止  退出码 -"},
		},
		{
			name: "usage: bare kill says how to use it",
			args: []string{"/jobs", "kill"},
			want: []string{"用法：/jobs"},
		},
		{
			name: "usage: non-numeric id",
			args: []string{"/jobs", "kill", "abc"},
			want: []string{"用法：/jobs"},
		},
		{
			name: "usage: extra argument",
			args: []string{"/jobs", "kill", "1", "2"},
			want: []string{"用法：/jobs"},
		},
		{
			name: "e: kill an unknown id is an error line, not a panic",
			args: []string{"/jobs", "kill", "999"},
			want: []string{"没有后台 job 999"},
		},
		{
			name: "output view of an unknown id",
			args: []string{"/jobs", "999"},
			want: []string{"没有后台 job 999"},
		},
		{
			name: "kill on a finished job explains, does not relabel it",
			args: []string{"/jobs", "kill", strconv.Itoa(finID)},
			want: []string{"后台 job " + strconv.Itoa(finID) + " 已结束（退出码 3），无需终止"},
		},
		{
			name: "output tail of the finished job",
			args: []string{"/jobs", strconv.Itoa(finID)},
			want: []string{"job " + strconv.Itoa(finID) + " · 已结束 · 退出码 3", "无输出"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := jobsCommand(jm, c.args, zh)
			got := joinLines(lines)
			if len(lines) == 0 {
				t.Fatal("command returned no lines")
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, d := range c.deny {
				if strings.Contains(got, d) {
					t.Errorf("unexpected %q in:\n%s", d, got)
				}
			}
		})
	}

	// (d) the kill form really terminates the job: run it last so the
	// listing rows above still see it alive.
	killLines := jobsCommand(jm, []string{"/jobs", "kill", strconv.Itoa(liveID)}, zh)
	if got := joinLines(killLines); !strings.Contains(got, "已终止后台 job "+strconv.Itoa(liveID)) || !strings.Contains(got, liveCmd) {
		t.Fatalf("kill confirmation = %q", got)
	}
	waitJobDone(t, live)
	if !live.Done() || !live.Stopped() {
		t.Fatalf("job after /jobs kill: done=%v stopped=%v", live.Done(), live.Stopped())
	}
	if n := jm.RunningCount(); n != 0 {
		t.Fatalf("running count after the kill = %d, want 0", n)
	}
	// The manager still tracks the corpse, now listed as stopped.
	if got := joinLines(jobsCommand(jm, []string{"/jobs"}, zh)); !strings.Contains(got, "job "+strconv.Itoa(liveID)+"  已停止") {
		t.Fatalf("listing after kill must show it stopped:\n%s", got)
	}
}

// TestJobsCommandEnglish pins the other language: the same rows must come
// out in English when the locale is, since the command takes the
// translator rather than reaching for the package-level uiT.
func TestJobsCommandEnglish(t *testing.T) {
	en := i18n.New(i18n.LangEn)
	jm := agent.NewJobManager()
	defer jm.StopAll()
	if got := joinLines(jobsCommand(jm, []string{"/jobs"}, en)); got != "no background jobs" {
		t.Errorf("empty en listing = %q", got)
	}
	_, id := jm.StartBackground("", jobsSleepCmd(30))
	got := joinLines(jobsCommand(jm, []string{"/jobs"}, en))
	for _, w := range []string{"background jobs (1 total · 1 running)", "job " + strconv.Itoa(id) + "  running"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if got := joinLines(jobsCommand(jm, []string{"/jobs", "kill", strconv.Itoa(id)}, en)); !strings.Contains(got, "stopped background job") {
		t.Errorf("en kill confirmation = %q", got)
	}
}

// TestJobsCommandOutputTail: the output view is capped to the tail, marked
// as such, and shows a live job's output so far without waiting for it.
func TestJobsCommandOutputTail(t *testing.T) {
	zh := i18n.New(i18n.LangZh)
	jm := agent.NewJobManager()
	defer jm.StopAll()

	// 30 numbered lines then a long sleep: the head must be cut away.
	print := "for i in $(seq 1 30); do echo line$i; done; " + jobsSleepCmd(30)
	if runtime.GOOS == "windows" {
		print = "for /L %i in (1,1,30) do @echo line%i & " + jobsSleepCmd(30)
	}
	j, id := jm.StartBackground("", print)
	defer j.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(j.Output(), "line30") {
		if time.Now().After(deadline) {
			t.Fatalf("job never printed line30: %q", j.Output())
		}
		time.Sleep(10 * time.Millisecond)
	}

	lines := jobsCommand(jm, []string{"/jobs", strconv.Itoa(id)}, zh)
	got := joinLines(lines)
	if !strings.Contains(got, "仅显示尾部 20 行") || !strings.Contains(got, "运行中") {
		t.Fatalf("output head = %q", lines[0])
	}
	if !strings.Contains(got, "line30") || strings.Contains(got, "line1\n") {
		t.Fatalf("output must keep the tail only:\n%s", got)
	}
	// Header + 20 body lines.
	if len(lines) != 1+20 {
		t.Fatalf("line count = %d, want 21 (header + 20)", len(lines))
	}
}

// TestJobCommandSummary: a multi-line, over-long command collapses to one
// row's worth of text, and the ellipsis marks the cut.
func TestJobCommandSummary(t *testing.T) {
	j := agent.NewJobForTest("echo a\n   echo b\n" + strings.Repeat("x", 100))
	got := j.CommandSummary(40)
	if strings.ContainsAny(got, "\n") {
		t.Fatalf("summary must be one line: %q", got)
	}
	if n := len([]rune(got)); n != 41 { // 40 runes + …
		t.Fatalf("summary rune length = %d, want 41: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "echo a echo b ") {
		t.Fatalf("summary = %q", got)
	}
}
