package agent

// Background jobs (§8): a foreground bash call that outlives
// auto_background_after turns into a job — the command keeps running in
// its own process group, the tool call returns a job id instead of the
// result, and the model inspects progress with job_output or terminates
// the job with job_kill. Jobs outlive the task that started them and die
// with the rysh process (StopAll at shutdown; process groups keep
// grandchildren from straying).

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	jobMax          = 50
	jobHeadBytes    = 128 * 1024
	jobTailBytes    = 128 * 1024
	jobOutMaxBytes  = 32 * 1024
	jobDefaultLines = 200
)

// headTailBuffer keeps a stream's first jobHeadBytes and most recent
// jobTailBytes; bytes squeezed out of the middle are counted and rendered
// as a one-line elision marker, so a chatty job neither grows unbounded
// nor loses its beginning.
type headTailBuffer struct {
	mu   sync.Mutex
	head []byte
	tail []byte
	lost int // bytes between head and tail once overflow began
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.head) < jobHeadBytes {
		room := jobHeadBytes - len(b.head)
		if room >= len(p) {
			b.head = append(b.head, p...)
			return n, nil
		}
		b.head = append(b.head, p[:room]...)
		p = p[room:]
	}
	if len(p) >= jobTailBytes {
		b.lost += len(b.tail) + len(p) - jobTailBytes
		b.tail = append(b.tail[:0], p[len(p)-jobTailBytes:]...)
		return n, nil
	}
	b.tail = append(b.tail, p...)
	if over := len(b.tail) - jobTailBytes; over > 0 {
		b.lost += over
		copy(b.tail, b.tail[over:])
		b.tail = b.tail[:jobTailBytes]
	}
	return n, nil
}

func (b *headTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lost == 0 {
		return string(b.head) + string(b.tail)
	}
	return string(b.head) + fmt.Sprintf("\n[… 中间已省略 %s …]\n", humanSize(b.lost)) + string(b.tail)
}

// Job is one background execution. Output combines stdout and stderr in
// arrival order — job_output is for eyeballing progress, not the
// stdout/stderr separation the inline bash result keeps.
type Job struct {
	Command string
	Dir     string
	Started time.Time

	mu       sync.Mutex
	id       int
	done     bool
	stopped  bool // killed via job_kill / eviction, not a natural exit
	exitCode int
	duration time.Duration
	out      headTailBuffer
	cancel   context.CancelFunc
	doneCh   chan struct{}
}

// ID returns the job's managed id (0 until register admits it).
func (j *Job) ID() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.id
}

func (j *Job) Done() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done
}

func (j *Job) ExitCode() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exitCode
}

func (j *Job) Stopped() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stopped
}

// Duration is the total run time; valid once Done.
func (j *Job) Duration() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.duration
}

// Elapsed is how long the job has been (or was) running.
func (j *Job) Elapsed() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done {
		return j.duration
	}
	return time.Since(j.Started)
}

func (j *Job) Output() string { return j.out.String() }

// DoneCh closes when the job exits; callers use it to reap without
// polling.
func (j *Job) DoneCh() <-chan struct{} { return j.doneCh }

// Kill terminates the whole process group. It is idempotent.
func (j *Job) Kill() { j.cancel() }

// JobManager tracks a session's background jobs.
type JobManager struct {
	mu     sync.Mutex
	jobs   map[int]*Job
	order  []int // insertion order, for cap eviction
	nextID int
}

func NewJobManager() *JobManager {
	return &JobManager{jobs: make(map[int]*Job)}
}

// startProcess launches command under its own process group. The job is
// unmanaged until register admits it: a foreground bash call starts one
// speculatively and either renders its result inline (finished inside the
// auto-background window) or hands the still-running job to the manager.
func (m *JobManager) startProcess(dir, command string) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{Command: command, Dir: dir, Started: time.Now(), cancel: cancel, doneCh: make(chan struct{})}
	name, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = &j.out
	cmd.Stderr = &j.out
	configureProcessGroup(cmd)
	go func() {
		err := cmd.Run()
		j.mu.Lock()
		j.done = true
		j.duration = time.Since(j.Started)
		switch {
		case ctx.Err() != nil:
			j.stopped = true
			j.exitCode = -1
		case err != nil:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				j.exitCode = ee.ExitCode()
			} else {
				j.exitCode = -1
			}
		default:
			j.exitCode = 0
		}
		j.mu.Unlock()
		close(j.doneCh)
	}()
	return j
}

// register admits a started job under an id, evicting the oldest job when
// the cap is hit — the oldest finished one first, else the oldest running
// one is killed (§8.2 names idle eviction; killing a running job is the
// fallback so the cap always holds).
func (m *JobManager) register(j *Job) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.order) >= jobMax {
		var victim *Job
		for _, id := range m.order {
			if m.jobs[id].Done() {
				victim = m.jobs[id]
				break
			}
		}
		if victim == nil {
			victim = m.jobs[m.order[0]]
		}
		m.removeLocked(victim.ID())
		victim.Kill()
	}
	m.nextID++
	j.mu.Lock()
	j.id = m.nextID
	j.mu.Unlock()
	m.jobs[m.nextID] = j
	m.order = append(m.order, m.nextID)
	return m.nextID
}

func (m *JobManager) removeLocked(id int) {
	delete(m.jobs, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

// Get returns the job with the given id, or nil.
func (m *JobManager) Get(id int) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// List returns the tracked jobs in admission order (oldest first) —
// the checkpoint snapshot's source for surviving background jobs.
func (m *JobManager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.jobs[id])
	}
	return out
}

// StopAll kills every running job (rysh shutdown path).
func (m *JobManager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.Kill()
	}
}

// jobOutputTool reports a job's status and recent output; the model polls
// it every few steps instead of blocking on the job (§8.2).
func jobOutputTool(jm *JobManager) Tool {
	return Tool{
		Name: "job_output",
		Description: "Read a background job's status and recent output (tail). " +
			"Use the job id from the 已转后台 notice; poll every few steps — a running job keeps producing new output.",
		ReadOnly: true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":    map[string]any{"type": "integer", "description": "The job id"},
				"lines": map[string]any{"type": "integer", "description": fmt.Sprintf("How many trailing lines to show (optional, default %d)", jobDefaultLines)},
			},
			"required": []string{"id"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			id, err := argInt(args, "id")
			if err != nil {
				return Result{}, err
			}
			lines := jobDefaultLines
			if v, ok := argOptInt(args, "lines"); ok && v > 0 {
				lines = v
				if lines > 1000 {
					lines = 1000
				}
			}
			job := jm.Get(id)
			if job == nil {
				return Result{}, fmt.Errorf("job %d 不存在（可能已结束并被清理，或 id 有误）", id)
			}
			var head string
			code := 0
			if job.Done() {
				if job.Stopped() {
					head = fmt.Sprintf("job %d 已被终止", id)
				} else {
					code = job.ExitCode()
					head = fmt.Sprintf("job %d 已结束 · exit %d · 耗时 %.1fs", id, code, job.Duration().Seconds())
				}
			} else {
				head = fmt.Sprintf("job %d 仍在运行 · 已 %.1fs · 稍后再查可见新输出", id, job.Elapsed().Seconds())
			}
			body := tailLines(job.Output(), lines)
			if body == "" {
				body = "(暂无输出)"
			}
			// Byte cap only, keeping the tail — tailLines already set the
			// line cap, and a second line pass would cut the newest lines.
			return Result{
				Output:  head + "\n" + tailBytes(body, jobOutMaxBytes),
				Meta:    head,
				Display: fmt.Sprintf("job_output %d", id),
				Code:    code,
			}, nil
		},
	}
}

// jobKillTool terminates a job's whole process group.
func jobKillTool(jm *JobManager) Tool {
	return Tool{
		Name:        "job_kill",
		Description: "Terminate a background job (kills its process group). Finished jobs cannot be killed.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "integer", "description": "The job id"},
			},
			"required": []string{"id"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			id, err := argInt(args, "id")
			if err != nil {
				return Result{}, err
			}
			job := jm.Get(id)
			if job == nil {
				return Result{}, fmt.Errorf("job %d 不存在（可能已结束并被清理，或 id 有误）", id)
			}
			if job.Done() {
				return Result{
					Output:  fmt.Sprintf("job %d 已结束（exit %d），无需终止", id, job.ExitCode()),
					Meta:    "已结束",
					Display: fmt.Sprintf("job_kill %d", id),
					Code:    job.ExitCode(),
				}, nil
			}
			job.Kill()
			return Result{
				Output:  fmt.Sprintf("job %d 已终止（$ %s）", id, job.Command),
				Meta:    fmt.Sprintf("job %d 已终止", id),
				Display: fmt.Sprintf("job_kill %d", id),
			}, nil
		},
	}
}

// tailLines keeps the last n lines of s, marking what was cut.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return fmt.Sprintf("…（仅显示尾 %d 行）…\n", n) + strings.Join(lines[len(lines)-n:], "\n")
}

// tailBytes keeps the last max bytes of s, marking what was cut — the
// byte counterpart of tailLines for tail views.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("[… 已省略 %s …]\n", humanSize(len(s)-max)) + s[len(s)-max:]
}
