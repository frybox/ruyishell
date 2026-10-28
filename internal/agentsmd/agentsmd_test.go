package agentsmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// noUserConfigDir points os.UserConfigDir at an empty temp dir so the
// user-level layer is absent.
func noUserConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDiscoverRepoRootAGENTS(t *testing.T) {
	noUserConfigDir(t)
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "repo instruction")
	got := Discover(root)
	if !strings.Contains(got, "repo instruction") {
		t.Errorf("got %q, want to contain repo instruction", got)
	}
	wantHeader := "[AGENTS.md " + filepath.Join(root, "AGENTS.md") + "]"
	if !strings.Contains(got, wantHeader) {
		t.Errorf("got %q, want to contain header %q", got, wantHeader)
	}
}

func TestDiscoverFallsBackToCLAUDE(t *testing.T) {
	noUserConfigDir(t)
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "claude convention")
	got := Discover(root)
	if !strings.Contains(got, "claude convention") {
		t.Errorf("got %q, want CLAUDE.md content", got)
	}
	if !strings.Contains(got, "[AGENTS.md "+filepath.Join(root, "CLAUDE.md")+"]") {
		t.Errorf("got %q, want CLAUDE.md header", got)
	}
}

func TestDiscoverNoInstructions(t *testing.T) {
	noUserConfigDir(t)
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".git"))
	if got := Discover(root); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestDiscoverEmptyCWD(t *testing.T) {
	noUserConfigDir(t)
	if got := Discover(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestDiscoverFromSubdir verifies the upward walk: cwd is a subdirectory of
// a repo whose root (but not the root's parent) carries the .git marker.
func TestDiscoverFromSubdir(t *testing.T) {
	noUserConfigDir(t)
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	mustMkdirAll(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "found from subdir")
	sub := filepath.Join(root, "a", "b")
	mustMkdirAll(t, sub)
	got := Discover(sub)
	if !strings.Contains(got, "found from subdir") {
		t.Errorf("got %q, want repo-root AGENTS.md content", got)
	}
	if !strings.Contains(got, "[AGENTS.md "+filepath.Join(root, "AGENTS.md")+"]") {
		t.Errorf("got %q, want repo-root AGENTS.md header", got)
	}
}

// TestDiscoverFollowsCWD pins the shell-cd behavior: the instructions are a
// pure function of the cwd passed in, so switching the shell's working
// directory between agent runs switches the injected AGENTS.md layers.
func TestDiscoverFollowsCWD(t *testing.T) {
	noUserConfigDir(t)
	a := t.TempDir()
	b := t.TempDir()
	mustMkdirAll(t, filepath.Join(a, ".git"))
	mustMkdirAll(t, filepath.Join(b, ".git"))
	mustWrite(t, filepath.Join(a, "AGENTS.md"), "instructions of A")
	mustWrite(t, filepath.Join(b, "AGENTS.md"), "instructions of B")

	if got := Discover(a); !strings.Contains(got, "instructions of A") {
		t.Errorf("cwd=A: got %q, want A instructions", got)
	}
	// Move to repo B: A's instructions must be gone, B's must appear.
	if got := Discover(b); !strings.Contains(got, "instructions of B") ||
		strings.Contains(got, "instructions of A") {
		t.Errorf("cwd=B: got %q, want only B instructions", got)
	}
	// Move back to repo A: A's instructions return.
	if got := Discover(a); !strings.Contains(got, "instructions of A") ||
		strings.Contains(got, "instructions of B") {
		t.Errorf("cwd back to A: got %q, want only A instructions", got)
	}
	// A directory inside a repo with no .git ancestors: repo layer is empty
	// (only the user-level layer, which this test has disabled, remains).
	plain := t.TempDir()
	if got := Discover(plain); got != "" {
		t.Errorf("cwd=%s: got %q, want empty", plain, got)
	}
}

func TestDiscoverUserLevel(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	mustMkdirAll(t, filepath.Join(cfg, "agents"))
	mustWrite(t, filepath.Join(cfg, "agents", "AGENTS.md"), "user instruction")
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "repo instruction")
	got := Discover(root)
	if !strings.Contains(got, "user instruction") {
		t.Errorf("got %q, want user-level content", got)
	}
	if !strings.Contains(got, "[AGENTS.md "+filepath.Join(cfg, "agents", "AGENTS.md")+"]") {
		t.Errorf("got %q, want user-level header", got)
	}
	// User-level section must come before the repo-root section.
	if strings.Index(got, "user instruction") > strings.Index(got, "repo instruction") {
		t.Errorf("user-level section must precede repo-root section in %q", got)
	}
}

func TestDiscoverTruncation(t *testing.T) {
	noUserConfigDir(t)
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".git"))
	// A body larger than the 16 KiB budget, mixing multi-byte UTF-8 so a
	// bad cut would corrupt a character.
	body := strings.Repeat("a指令", 8192) // 48 KB of UTF-8
	if len(body) <= defaultBudget {
		t.Fatalf("test body %d bytes must exceed budget %d", len(body), defaultBudget)
	}
	mustWrite(t, filepath.Join(root, "AGENTS.md"), body)
	got := Discover(root)
	if len(got) == 0 {
		t.Fatalf("Discover returned empty; want repo-root section")
	}
	if !strings.Contains(got, "[... 已截断至 16384 字节]") {
		t.Errorf("got head %q..., want truncation marker", got[:min(len(got), 60)])
	}
	// The section after the header must be valid UTF-8 (no split rune).
	section := strings.TrimPrefix(got, "[AGENTS.md "+filepath.Join(root, "AGENTS.md")+"]\n")
	if !utf8.ValidString(section) {
		t.Errorf("truncated section is not valid UTF-8 (len %d)", len(section))
	}
	if len(section) > defaultBudget+256 {
		t.Errorf("section %d bytes exceeds budget %d by too much", len(section), defaultBudget)
	}
	// The tail must actually be cut off, not just the marker appended.
	if strings.Contains(got, strings.Repeat("a指令", 8000)) {
		t.Errorf("content does not appear truncated")
	}
}
