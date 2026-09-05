package agent

// File tools (§5.3): read (line-numbered), write (whole-file), edit
// (exact unique-match replacement) and ls (bounded directory tree).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	readDefaultLines = 200
	readMaxLines     = 2000
	readMaxBytes     = 50 * 1024
	lsMaxDepth       = 4
	lsMaxLines       = 500
)

// resolvePath makes p absolute against base when relative.
func resolvePath(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func readTool(cwd string) Tool {
	return Tool{
		Name: "read",
		Description: "Read a text file with line numbers (1-based), optionally from an offset for a limited number of lines. " +
			"Use this instead of cat: the line numbers are what the edit tool anchors on.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File path (relative to current directory unless absolute)"},
				"offset": map[string]any{"type": "integer", "description": "1-based line to start from (optional)"},
				"limit":  map[string]any{"type": "integer", "description": fmt.Sprintf("How many lines to read (optional, default %d, max %d)", readDefaultLines, readMaxLines)},
			},
			"required": []string{"path"},
		},
		ReadOnly: true,
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			path, err := argString(args, "path")
			if err != nil {
				return Result{}, err
			}
			full := resolvePath(task.Cwd(), path)
			info, err := os.Stat(full)
			if err != nil {
				return Result{}, fmt.Errorf("read 失败: %v", err)
			}
			if info.IsDir() {
				return Result{}, fmt.Errorf("read 失败: %s 是目录（用 ls 或 glob）", path)
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return Result{}, fmt.Errorf("read 失败: %v", err)
			}
			offset := 1
			if v, ok := argOptInt(args, "offset"); ok && v > 0 {
				offset = v
			}
			limit := readDefaultLines
			if v, ok := argOptInt(args, "limit"); ok && v > 0 {
				limit = v
			}
			if limit > readMaxLines {
				limit = readMaxLines
			}
			lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(lines) == 1 && lines[0] == "" {
				lines = nil
			}
			var b strings.Builder
			last := offset + limit - 1
			if last > len(lines) {
				last = len(lines)
			}
			size := 0
			truncated := false
			for i := offset; i <= last; i++ {
				line := fmt.Sprintf("%5d→%s", i, lines[i-1])
				if size+len(line) > readMaxBytes {
					truncated = true
					break
				}
				b.WriteString(line)
				b.WriteByte('\n')
				size += len(line)
			}
			out := b.String()
			if truncated {
				out += fmt.Sprintf("[已截断：达到 %d 字节上限，请用 offset=%d 续读]\n", readMaxBytes, offset+limit)
			}
			return Result{
				Output:  out,
				Meta:    fmt.Sprintf("L%d-L%d · %d 行", offset, last, last-offset+1),
				Display: fmt.Sprintf("read %s L%d-L%d", path, offset, last),
			}, nil
		},
	}
}

func writeTool(cwd string) Tool {
	return Tool{
		Name:        "write",
		Description: "Create a file or overwrite it entirely. The parent directory must already exist (no mkdir). Prefer edit for changing parts of an existing file.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "description": "File path (relative to current directory unless absolute)"},
				"content": map[string]any{"type": "string", "description": "The full file content"},
			},
			"required": []string{"path", "content"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			path, err := argString(args, "path")
			if err != nil {
				return Result{}, err
			}
			content, err := argString(args, "content")
			if err != nil {
				return Result{}, err
			}
			full := resolvePath(task.Cwd(), path)
			dir := filepath.Dir(full)
			if _, err := os.Stat(dir); err != nil {
				return Result{}, fmt.Errorf("write 失败: 父目录不存在（不自动创建）: %s", dir)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return Result{}, fmt.Errorf("write 失败: %v", err)
			}
			lines := strings.Count(content, "\n") + 1
			if content != "" && strings.HasSuffix(content, "\n") {
				lines--
			}
			return Result{
				Meta:    fmt.Sprintf("%d 行 · %s", lines, humanSize(len(content))),
				Display: "write " + path,
			}, nil
		},
	}
}

func editTool(cwd string) Tool {
	return Tool{
		Name: "edit",
		Description: "Replace an exact string in a file. old_string must appear exactly once unless replace_all is set; " +
			"on zero or multiple matches nothing is written — read the file again and retry with more context.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "File path (relative to current directory unless absolute)"},
				"old_string":  map[string]any{"type": "string", "description": "The exact text to replace (must be unique in the file unless replace_all)"},
				"new_string":  map[string]any{"type": "string", "description": "The replacement text (may be empty to delete)"},
				"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring uniqueness (optional)"},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			path, err := argString(args, "path")
			if err != nil {
				return Result{}, err
			}
			oldStr, err := argString(args, "old_string")
			if err != nil {
				return Result{}, err
			}
			newStr, err := argString(args, "new_string")
			if err != nil {
				return Result{}, err
			}
			full := resolvePath(task.Cwd(), path)
			data, err := os.ReadFile(full)
			if err != nil {
				return Result{}, fmt.Errorf("edit 失败: %v", err)
			}
			src := string(data)
			count := strings.Count(src, oldStr)
			if count == 0 {
				return Result{}, fmt.Errorf("edit 失败: old_string 在 %s 中未找到（文件可能已变更），请先 read 最新内容后重试", path)
			}
			all := argBool(args, "replace_all")
			if count > 1 && !all {
				return Result{}, fmt.Errorf("edit 失败: old_string 在 %s 中出现 %d 次，请扩大上下文使其唯一，或使用 replace_all", path, count)
			}
			var out string
			var meta string
			if all {
				out = strings.ReplaceAll(src, oldStr, newStr)
				meta = fmt.Sprintf("替换 %d 处", count)
			} else {
				idx := strings.Index(src, oldStr)
				out = src[:idx] + newStr + src[idx+len(oldStr):]
				meta = fmt.Sprintf("替换 1 处（L%d）", 1+strings.Count(src[:idx], "\n"))
			}
			if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
				return Result{}, fmt.Errorf("edit 失败: %v", err)
			}
			return Result{Meta: meta, Display: "edit " + path}, nil
		},
	}
}

func lsTool(cwd string) Tool {
	return Tool{
		Name:        "ls",
		Description: "Show a bounded directory tree with sizes and directory markers. Use this for an overview instead of running ls -R.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string", "description": "Directory to list (optional, default: current directory)"},
				"depth": map[string]any{"type": "integer", "description": fmt.Sprintf("Tree depth (optional, default 2, max %d)", lsMaxDepth)},
			},
		},
		ReadOnly: true,
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			base := task.Cwd()
			if p := argOptString(args, "path"); p != "" {
				base = resolvePath(task.Cwd(), p)
			}
			depth := 2
			if v, ok := argOptInt(args, "depth"); ok && v > 0 {
				depth = v
			}
			if depth > lsMaxDepth {
				depth = lsMaxDepth
			}
			info, err := os.Stat(base)
			if err != nil {
				return Result{}, fmt.Errorf("ls 失败: %v", err)
			}
			if !info.IsDir() {
				return Result{}, fmt.Errorf("ls 失败: %s 不是目录", base)
			}
			var lines []string
			items := 0
			truncated := false
			var walk func(dir string, level int)
			walk = func(dir string, level int) {
				entries, err := os.ReadDir(dir)
				if err != nil {
					return
				}
				sort.Slice(entries, func(i, j int) bool {
					a, b := entries[i].IsDir(), entries[j].IsDir()
					if a != b {
						return a
					}
					return entries[i].Name() < entries[j].Name()
				})
				for _, e := range entries {
					if e.Name() == ".git" {
						continue
					}
					if len(lines) >= lsMaxLines {
						truncated = true
						return
					}
					items++
					indent := strings.Repeat("  ", level)
					if e.IsDir() {
						lines = append(lines, fmt.Sprintf("%s%s/", indent, e.Name()))
						if level < depth {
							walk(filepath.Join(dir, e.Name()), level+1)
						}
					} else {
						size := int64(0)
						if fi, err := e.Info(); err == nil {
							size = fi.Size()
						}
						lines = append(lines, fmt.Sprintf("%s%s  %s", indent, e.Name(), humanSize(int(size))))
					}
				}
			}
			walk(base, 0)
			out := strings.Join(lines, "\n")
			if truncated {
				out += fmt.Sprintf("\n[… 已截断：%d 行上限 …]", lsMaxLines)
			} else if out == "" {
				out = "(空目录)"
			}
			return Result{
				Output:  out,
				Meta:    fmt.Sprintf("%d 项", items),
				Display: "ls " + base,
			}, nil
		},
	}
}
