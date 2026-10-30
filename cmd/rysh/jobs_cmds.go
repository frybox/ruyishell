// The /jobs slash command: a read-mostly view onto the session's
// background jobs (§8.2). A job outlives the turn that started it — it is
// only ever born from a bash call that outran the auto-background window,
// so the prompt badge (`jobs_running`) can say work is still going on
// behind the prompt, and /jobs is where that badge leads: the same jobs,
// with state and exit code, plus the one control a job has (kill) that
// used to exist only as a model tool (job_kill).
//
// The command is a plain function over the manager rather than a closure
// in main's dispatch switch for two reasons: it must not touch writeMu (the
// caller already holds it, see printStreamLinesLocked), and the job list is
// worth testing without the engine — the table test drives a real
// JobManager with real processes.
package main

import (
	"strconv"
	"strings"

	"ruyishell/internal/agent"
	"ruyishell/internal/i18n"
)

// jobsCommandLimit is the command column's rune budget in the list. A bash
// snippet can be an entire heredoc; a row that wraps defeats the point of
// a one-job-per-line table, so long ones are cut with an ellipsis and the
// full text stays available as the job's own output head.
const jobsCommandLimit = 40

// jobsTailLines is how much of a job's output `/jobs <id>` shows. It is
// deliberately shorter than job_output's tail (the model's view): the user
// is glancing at a job, not reading its transcript, and Output() is capped
// at 32KB total, which is far more than one screen.
const jobsTailLines = 20

// jobsCommand formats the /jobs command's output lines, which the caller
// prints with printStreamLinesLocked (writeMu held — so nothing here may
// block on the terminal, and nothing here may take writeMu again).
//
//	/jobs              one row per tracked job, running first (see below)
//	/jobs kill <id>    stop that job, or say it already finished
//	/jobs <id>         the job's output tail, for a job that produced more
//
// Tracked includes finished jobs: a job stays in the manager until the cap
// evicts it, and its exit code is exactly what the user came to read.
func jobsCommand(jm *agent.JobManager, fields []string, t i18n.T) []string {
	if jm == nil {
		// Defensive: the manager is process-level and always built, but a
		// nil here must read as "no jobs" rather than panic the prompt.
		return []string{t.Get("jobs_none")}
	}
	switch len(fields) {
	case 1:
		return jobsListLines(jm, t)
	case 2:
		if fields[1] == "kill" {
			return []string{t.Get("jobs_usage")}
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			return []string{t.Get("jobs_usage")}
		}
		return jobsOutputLines(jm, id, t)
	case 3:
		// kill takes exactly one argument; anything else is a typo the
		// usage line is the fastest fix for (a stray third word would
		// otherwise be silently ignored and an unrelated job killed).
		if fields[1] != "kill" {
			return []string{t.Get("jobs_usage")}
		}
		id, err := strconv.Atoi(fields[2])
		if err != nil {
			return []string{t.Get("jobs_usage")}
		}
		return jobsKillLines(jm, id, t)
	default:
		return []string{t.Get("jobs_usage")}
	}
}

// jobsListLines renders the table: the /jobs header (total and running
// count, which is also the prompt badge's number) followed by one row per
// job. Rows keep List's admission order rather than sorting running jobs
// first: ids are the handle the user types back into `/jobs kill <id>` and
// `/jobs <id>`, and the manager hands them out monotonically, so a
// re-listed job stays where the previous listing put it and the running
// count in the header is what answers "is anything still going?".
func jobsListLines(jm *agent.JobManager, t i18n.T) []string {
	jobs := jm.List()
	if len(jobs) == 0 {
		return []string{t.Get("jobs_none")}
	}
	running := 0
	for _, j := range jobs {
		if !j.Done() {
			running++
		}
	}
	lines := []string{t.Get("jobs_header", len(jobs), running)}
	for _, j := range jobs {
		status, code := jobsStatus(j, t)
		lines = append(lines, t.Get("jobs_row", j.ID(), status, code,
			agent.HumanDuration(j.Elapsed()), j.CommandSummary(jobsCommandLimit)))
	}
	return lines
}

// jobsKillLines stops one job. A finished job is reported, not killed: its
// exit code is the answer the user wanted, and calling Kill on a reaped
// process would only rewrite the recorded state (Stopped() flips) for
// nothing. A running one goes through Job.Kill, which signals the whole
// process group and is idempotent — safe to press twice.
func jobsKillLines(jm *agent.JobManager, id int, t i18n.T) []string {
	j := jm.Get(id)
	if j == nil {
		return []string{t.Get("jobs_no_such", id)}
	}
	if j.Done() {
		return []string{t.Get("jobs_kill_done", id, j.ExitCode())}
	}
	j.Kill()
	return []string{t.Get("jobs_killed", id, j.CommandSummary(jobsCommandLimit))}
}

// jobsOutputLines shows the tail of one job's captured output. Output() is
// a locked read of an in-memory ring buffer (head+tail capped at 32KB), so
// it cannot block on the job — a still-running job shows what it has said
// so far and the header says so.
func jobsOutputLines(jm *agent.JobManager, id int, t i18n.T) []string {
	j := jm.Get(id)
	if j == nil {
		return []string{t.Get("jobs_no_such", id)}
	}
	status, code := jobsStatus(j, t)
	lines := []string{t.Get("jobs_output_head", id, status, code,
		agent.HumanDuration(j.Elapsed()), jobsTailLines)}
	body := strings.Split(strings.TrimRight(j.Output(), "\n"), "\n")
	// A job that has printed nothing yet has no lines to show; "" would
	// otherwise render as one empty row under the header.
	if len(body) == 1 && body[0] == "" {
		return append(lines, t.Get("no_output"))
	}
	if len(body) > jobsTailLines {
		body = body[len(body)-jobsTailLines:]
	}
	return append(lines, body...)
}

// jobsStatus maps a job's state to its display label and exit-code cell.
// A running job has no exit code yet and a stopped one has none worth
// showing (-1 is the kill marker, not a program's own status), so both
// print a dash rather than a number that would read as a real result.
func jobsStatus(j *agent.Job, t i18n.T) (status, code string) {
	switch {
	case !j.Done():
		return t.Get("jobs_status_running"), t.Get("jobs_nocode")
	case j.Stopped():
		return t.Get("jobs_status_stopped"), t.Get("jobs_nocode")
	default:
		return t.Get("jobs_status_done"), strconv.Itoa(j.ExitCode())
	}
}
