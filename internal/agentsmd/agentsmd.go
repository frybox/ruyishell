// Package agentsmd discovers and renders AGENTS.md instruction layers so
// that project instructions can be appended to the leading system-message
// run of an agent request.
//
// Two layers are discovered, in order: the user-level file at
// os.UserConfigDir()/agents/AGENTS.md, then the repository-root file. The
// repository root is located by walking up from the shell cwd until a
// directory containing a .git entry (a directory or a file, the latter for
// git worktrees and submodules) is found; the instruction file is the
// repository root's AGENTS.md, falling back to CLAUDE.md (the Claude Code
// convention). Each discovered file is rendered as a "[AGENTS.md <path>]"
// header followed by its content; the file content is truncated to a byte
// budget on a rune boundary so it never splits a multi-byte UTF-8
// character.
package agentsmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// defaultBudget is the byte budget for one instruction file's content.
const defaultBudget = 16 * 1024

// Discover finds and renders the AGENTS.md instruction layers for the
// given shell cwd, in order: user-level, then repo-root. Returns "" when
// no instruction file is found. The result is ready to be appended to the
// leading system-message run of an agent request.
func Discover(cwd string) string {
	if cwd == "" {
		return ""
	}
	var sections []string
	if s := readUserLevel(); s != "" {
		sections = append(sections, s)
	}
	if root := findRepoRoot(cwd); root != "" {
		if s := readRepoLevel(root); s != "" {
			sections = append(sections, s)
		}
	}
	return strings.Join(sections, "\n\n")
}

// findRepoRoot walks up from cwd until a directory containing a .git
// entry (directory or file) is found, returning that directory's path.
// Returns "" when no such directory exists up to the filesystem root.
func findRepoRoot(cwd string) string {
	dir := cwd
	for {
		if isGitEntry(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isGitEntry reports whether the path exists and is a regular file or a
// directory (a .git directory for an ordinary repo, a .git file for a git
// worktree or submodule).
func isGitEntry(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.IsDir() || fi.Mode().IsRegular()
}

// readUserLevel returns the rendered user-level instruction file, or ""
// when the file is missing or unreadable.
func readUserLevel() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return render(filepath.Join(dir, "agents", "AGENTS.md"), defaultBudget)
}

// readRepoLevel returns the rendered repository-root instruction file,
// preferring AGENTS.md and falling back to CLAUDE.md. Returns "" when
// neither exists.
func readRepoLevel(root string) string {
	if s := render(filepath.Join(root, "AGENTS.md"), defaultBudget); s != "" {
		return s
	}
	return render(filepath.Join(root, "CLAUDE.md"), defaultBudget)
}

// render reads the file at path and renders it as a "[AGENTS.md <path>]"
// header followed by the content, truncated to budget bytes on a rune
// boundary. Returns "" when the file is missing or unreadable.
func render(path string, budget int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := truncateToUTF8(string(data), budget)
	section := fmt.Sprintf("[AGENTS.md %s]\n%s", path, content)
	if len(data) > budget {
		section += fmt.Sprintf("\n[... 已截断至 %d 字节]", budget)
	}
	return section
}

// truncateToUTF8 returns at most the first budget bytes of s, never
// cutting in the middle of a multi-byte UTF-8 character.
func truncateToUTF8(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	for budget > 0 && !utf8.RuneStart(s[budget]) {
		budget--
	}
	return s[:budget]
}
