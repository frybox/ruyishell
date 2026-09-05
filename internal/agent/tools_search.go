package agent

// Search tools (§5.3): glob (file enumeration with ** support, .gitignore
// respected via git ls-files inside a repository) and grep (rg first,
// built-in scan as fallback).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	globMaxFiles  = 1000
	grepMaxLines  = 200
	grepMaxBytes  = 50 * 1024
	grepTimeout   = 30 * time.Second
	grepMaxFileSz = 1 << 20 // skip files above 1MB in the fallback scan
)

// globTool builds the glob tool rooted at the task's default directory.
func globTool(cwd string) Tool {
	return Tool{
		Name:        "glob",
		Description: "List files matching a pattern like **/*.go, relative to path (default: current directory). Respects .gitignore inside a git repository.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Glob pattern, ** matches any depth, e.g. **/*.go or internal/**/*.md"},
				"path":    map[string]any{"type": "string", "description": "Base directory (optional, default: current directory)"},
			},
			"required": []string{"pattern"},
		},
		ReadOnly: true,
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			pattern, err := argString(args, "pattern")
			if err != nil {
				return Result{}, err
			}
			base := task.Cwd()
			if p := argOptString(args, "path"); p != "" {
				base = resolvePath(task.Cwd(), p)
			}
			pattern = strings.TrimPrefix(pattern, "./")
			files, err := listFiles(ctx, base)
			if err != nil {
				return Result{}, fmt.Errorf("glob 失败: %v", err)
			}
			var matched []string
			for _, f := range files {
				if matchGlobPattern(pattern, f) {
					matched = append(matched, f)
				}
			}
			sort.Strings(matched)
			total := len(matched)
			truncated := false
			if total > globMaxFiles {
				matched = matched[:globMaxFiles]
				truncated = true
			}
			out := strings.Join(matched, "\n")
			if truncated {
				out += fmt.Sprintf("\n[… 共 %d 个文件，仅显示前 %d 个 …]", total, globMaxFiles)
			}
			if out == "" {
				out = "(无匹配文件)"
			}
			return Result{
				Output:  out,
				Meta:    fmt.Sprintf("%d 个文件", total),
				Display: "glob " + pattern,
			}, nil
		},
	}
}

// listFiles enumerates candidate paths under base, relative to base and
// slash-separated. Inside a git work tree it shells out to git ls-files
// (which applies .gitignore); otherwise it walks, skipping .git.
func listFiles(ctx context.Context, base string) ([]string, error) {
	if _, err := exec.LookPath("git"); err == nil {
		check := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
		check.Dir = base
		if out, err := check.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
			ls := exec.CommandContext(ctx, "git", "ls-files", "--cached", "--others", "--exclude-standard")
			ls.Dir = base
			if out, err := ls.Output(); err == nil {
				var files []string
				for _, line := range strings.Split(string(out), "\n") {
					if line = strings.TrimSpace(line); line != "" {
						files = append(files, filepath.ToSlash(filepath.Clean(line)))
					}
				}
				return files, nil
			}
		}
	}
	var files []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// matchGlobPattern reports whether a slash-separated relative path matches
// a glob pattern where ** matches zero or more path segments and each
// other segment obeys filepath.Match rules.
func matchGlobPattern(pattern, path string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], segs[0])
	return err == nil && ok && matchSegs(pat[1:], segs[1:])
}

func grepTool(cwd string) Tool {
	return Tool{
		Name: "grep",
		Description: "Search file contents with a regular expression, printing path:line:content. " +
			"Search a directory (default: current directory) or a single file; glob narrows the file set.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Regular expression (Go/rg syntax)"},
				"path":    map[string]any{"type": "string", "description": "File or directory to search (optional, default: current directory)"},
				"glob":    map[string]any{"type": "string", "description": "Only search files matching this glob, e.g. *.go (optional)"},
			},
			"required": []string{"pattern"},
		},
		ReadOnly: true,
		Execute: func(ctx context.Context, task *Task, args map[string]any) (Result, error) {
			pattern, err := argString(args, "pattern")
			if err != nil {
				return Result{}, err
			}
			re, err := regexp.Compile(pattern)
			if err != nil {
				return Result{}, fmt.Errorf("grep 失败: 正则无效: %v", err)
			}
			base := task.Cwd()
			if p := argOptString(args, "path"); p != "" {
				base = resolvePath(task.Cwd(), p)
			}
			globPat := argOptString(args, "glob")

			ctx, cancel := context.WithTimeout(ctx, grepTimeout)
			defer cancel()
			var out string
			if lines, err := grepRipgrep(ctx, pattern, base, globPat); err == nil {
				out = lines
			} else {
				lines, gerr := grepScan(ctx, re, base, globPat)
				if gerr != nil {
					return Result{}, fmt.Errorf("grep 失败: %v", gerr)
				}
				out = lines
			}
			count := 0
			if out != "" {
				count = strings.Count(out, "\n")
				if !strings.HasSuffix(out, "\n") {
					count++
				}
			}
			body := truncateLines(out, grepMaxLines, grepMaxBytes)
			return Result{
				Output:  body,
				Meta:    fmt.Sprintf("%d 处命中", count),
				Display: fmt.Sprintf("grep %q %s", pattern, base),
			}, nil
		},
	}
}

// grepRipgrep runs rg --line-number and returns its raw output. It fails
// (so the caller falls back) whenever rg is missing, the target is not
// searchable, or anything else goes wrong.
func grepRipgrep(ctx context.Context, pattern, path, glob string) (string, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		return "", err
	}
	argv := []string{"rg", "--line-number", "--no-heading", "--color", "never", "-e", pattern}
	if glob != "" {
		argv = append(argv, "--glob", glob)
	}
	argv = append(argv, path)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		// Exit 1 means "no matches" — a valid empty result, not a failure.
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return "", err
		}
	}
	return stdout.String(), nil
}

// grepScan is the no-rg fallback: walk the tree (skipping .git, oversized
// and binary files) and collect path:line:content for regex matches.
func grepScan(ctx context.Context, re *regexp.Regexp, base string, glob string) (string, error) {
	var b bytes.Buffer
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > grepMaxFileSz {
			return nil
		}
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if glob != "" && !matchGlobPattern(glob, rel) && !matchGlobPattern(glob, d.Name()) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || bytes.IndexByte(data[:min(800, len(data))], 0) >= 0 {
			return nil // unreadable or binary
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d:%s\n", rel, i+1, line)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}
