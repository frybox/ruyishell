package agent

// Tool system v1 (architecture §5). Tools are plain structs: the registry
// is fixed for M7.2 (bash/read/write/edit/glob/grep/ls/todo); job_* tools
// arrive with the background-job milestone.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ruyishell/internal/provider"
)

// bashTimeoutDefaults are the M7.2 stand-ins for the [agent] config face
// (bash_timeout / bash_max_timeout land with the long-command milestone).
const (
	defaultBashTimeout = 60 * time.Second
	maxBashTimeout     = 600 * time.Second
)

// Result is a tool's outcome. Output is the body fed back to the model
// (already truncated to the tool's limit); Meta is the one-line summary
// that prefixes the fed-back message and can appear on screen; Display is
// the full screen observation line (bash renders M4-style
// "$ cmd (exit N)"); Code carries the exit code for the loop guards.
type Result struct {
	Output  string
	Meta    string
	Display string
	Code    int
}

// Tool is one callable tool. Execute returns an error for tool-level
// failures — the error is fed back to the model as the tool result (the
// model can self-correct); it never kills the task.
type Tool struct {
	Name        string
	Description string         // enters the tools description (with usage discipline)
	Schema      map[string]any // JSON Schema
	ReadOnly    bool           // read-class for the guards (§6.4) and, later, approvals (§7)
	Execute     func(ctx context.Context, task *Task, args map[string]any) (Result, error)
}

// Task is the per-run handle tools get: the working directory and the
// todo state (todo is the only tool that mutates shared task state).
type Task struct {
	cwd   string
	sink  Sink
	todos []Todo
}

// Cwd returns the task's default working directory (the shell's cwd at
// submit time).
func (t *Task) Cwd() string { return t.cwd }

// SetTodos replaces the task's todo list and reports it to the sink.
func (t *Task) SetTodos(todos []Todo) {
	t.todos = todos
	if t.sink != nil {
		t.sink.OnTodo(todos)
	}
}

// Todos returns the current todo list.
func (t *Task) Todos() []Todo { return t.todos }

// Todo is one entry of the model-maintained task list (§9.1).
type Todo struct {
	ID      string
	Content string
	Status  string // pending | in_progress | completed
}

// DefaultTools builds the v1 registry with the built-in bash timeouts and
// no job manager (auto-background disabled).
func DefaultTools(cwd string) []Tool {
	return defaultToolsWith(cwd, ToolOpts{BashTimeout: defaultBashTimeout, BashMaxTimeout: maxBashTimeout})
}

// ToolOpts bundles the registry knobs: the bash timeout pair (§5.3) and
// the §8.1 auto-background face (a job manager plus the foreground window;
// Jobs nil or AutoBackground 0 disables backgrounding).
type ToolOpts struct {
	BashTimeout    time.Duration
	BashMaxTimeout time.Duration
	Jobs           *JobManager
	AutoBackground time.Duration
}

// defaultToolsWith builds the registry with explicit knobs (the engine
// forwards Config overrides here). job_* tools join only with a manager.
func defaultToolsWith(cwd string, o ToolOpts) []Tool {
	tools := []Tool{
		bashTool(cwd, o),
		readTool(cwd),
		writeTool(cwd),
		editTool(cwd),
		globTool(cwd),
		grepTool(cwd),
		lsTool(cwd),
		todoTool(),
	}
	if o.Jobs != nil {
		tools = append(tools, jobOutputTool(o.Jobs), jobKillTool(o.Jobs))
	}
	return tools
}

// toolNames renders the registry names for the unknown-tool error.
func toolNames(tools []Tool) string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// findTool returns the registry entry with the given name, or nil.
func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// toolDefs renders the registry as the provider's function-calling
// description list.
func toolDefs(tools []Tool) []provider.ToolDef {
	defs := make([]provider.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, provider.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Schema})
	}
	return defs
}

// argString fetches a required string argument.
func argString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("缺少参数 %s", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %s 必须是字符串", key)
	}
	return s, nil
}

// argOptString fetches an optional string argument ("" when absent).
func argOptString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// argInt fetches an integer-ish argument (JSON numbers decode as float64).
func argInt(args map[string]any, key string) (int, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("缺少参数 %s", key)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("参数 %s 必须是数字", key)
	}
	return int(f), nil
}

// argOptInt fetches an optional integer argument (ok=false when absent).
func argOptInt(args map[string]any, key string) (int, bool) {
	f, ok := args[key].(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// argBool fetches an optional boolean argument.
func argBool(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// truncateMid caps s at head+tail bytes: everything in the middle becomes
// a one-line elision marker naming the omitted size (§5.3 bash shape).
func truncateMid(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	omitted := len(s) - head - tail
	return s[:head] + fmt.Sprintf("\n[… 已省略 %d 字节 …]\n", omitted) + s[len(s)-tail:]
}

// truncateLines caps a line-oriented output at maxLines lines and maxBytes
// bytes, appending a marker naming what was cut.
func truncateLines(s string, maxLines int, maxBytes int) string {
	lines := strings.Split(s, "\n")
	cut := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		cut = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		cut = true
	}
	if cut {
		out += fmt.Sprintf("\n[… 已截断：上限 %d 行 / %d 字节 …]", maxLines, maxBytes)
	}
	return out
}

// parseToolArgs decodes the model's arguments JSON into a map. Broken or
// non-object JSON is a tool error (fed back, task continues — §5.1 弱模型韧性).
func parseToolArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
