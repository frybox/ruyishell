package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ruyishell/internal/screen"
)

const sample = `
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
name = "Llama 3.1 8B (Local)"
reasoning = false

[[providers.ollama.models]]
id = "qwen2.5-coder:7b"

[providers.mycloud]
base_url = "https://api.example.com/v1"
api = "openai-completions"
api_key = "$MY_KEY"

[[providers.mycloud.models]]
id = "gpt-x"
context_window = 128000
max_tokens = 4096
`

func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Default != "ollama/llama3.1:8b" {
		t.Fatalf("default = %q, want ollama/llama3.1:8b", cfg.Default)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cfg.Providers))
	}
	ollama := cfg.Providers["ollama"]
	if ollama == nil || ollama.BaseURL != "http://localhost:11434/v1" || ollama.API != "openai-completions" {
		t.Fatalf("ollama provider = %+v", ollama)
	}
	if len(ollama.Models) != 2 {
		t.Fatalf("ollama models = %d, want 2", len(ollama.Models))
	}
	if ollama.Models[0].ID != "llama3.1:8b" || ollama.Models[0].Name != "Llama 3.1 8B (Local)" {
		t.Fatalf("ollama model[0] = %+v", ollama.Models[0])
	}
	cloud := cfg.Providers["mycloud"]
	if cloud.APIKey != "$MY_KEY" {
		t.Fatalf("mycloud api_key = %q, want raw $MY_KEY", cloud.APIKey)
	}
}

func TestModelToolsSwitch(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
[providers.p]
[[providers.p.models]]
id = "auto"
[[providers.p.models]]
id = "off"
tools = false
[[providers.p.models]]
id = "on"
tools = true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	models := cfg.Providers["p"].Models
	if models[0].Tools != nil {
		t.Fatalf("unset tools = %+v, want nil (default on)", models[0].Tools)
	}
	if models[1].Tools == nil || *models[1].Tools != false {
		t.Fatalf("tools = false not parsed: %+v", models[1].Tools)
	}
	if models[2].Tools == nil || *models[2].Tools != true {
		t.Fatalf("tools = true not parsed: %+v", models[2].Tools)
	}
}

func TestLoadMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if cfg.Providers != nil || cfg.Default != "" {
		t.Fatalf("expected empty config, got %+v", cfg)
	}
}

func TestLoadMalformed(t *testing.T) {
	_, err := Load(writeConfig(t, t.TempDir(), "[providers\nbroken"))
	if err == nil {
		t.Fatal("expected error for malformed TOML")
	}
}

func TestPathEnvOverride(t *testing.T) {
	t.Setenv("RYSH_CONFIG", "/tmp/alt.toml")
	p, err := Path()
	if err != nil || p != "/tmp/alt.toml" {
		t.Fatalf("Path with RYSH_CONFIG = %q, %v", p, err)
	}
}

func TestPathHomeDefault(t *testing.T) {
	t.Setenv("RYSH_CONFIG", "")
	// Path() resolves the home directory through os.UserHomeDir, which on
	// Windows reads USERPROFILE and ignores HOME.
	homeVar := "HOME"
	if runtime.GOOS == "windows" {
		homeVar = "USERPROFILE"
	}
	home := t.TempDir()
	t.Setenv(homeVar, home)
	want := filepath.Join(home, ".rysh", "config.toml")
	p, err := Path()
	if err != nil || p != want {
		t.Fatalf("Path = %q, %v; want %q", p, err, want)
	}
}

func TestResolveSecret(t *testing.T) {
	t.Setenv("RYSH_TEST_KEY", "sk-secret")
	t.Setenv("RYSH_TEST_PREFIX", "pre")

	cases := []struct {
		in   string
		want string
	}{
		{"ollama", "ollama"},                         // literal
		{"$RYSH_TEST_KEY", "sk-secret"},              // $NAME
		{"${RYSH_TEST_KEY}", "sk-secret"},            // ${NAME}
		{"${RYSH_TEST_PREFIX}_suffix", "pre_suffix"}, // ${NAME} with literal suffix
		{"$$literal", "$literal"},                    // $$ escape
		{"$!bang", "!bang"},                          // $! escape
		{"a$RYSH_TEST_KEY b", "ask-secret b"},        // interpolation inside text
		{"no var here", "no var here"},
	}
	for _, tc := range cases {
		got, err := ResolveSecret(tc.in)
		if err != nil {
			t.Fatalf("ResolveSecret(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ResolveSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveSecretMissingVar(t *testing.T) {
	t.Setenv("RYSH_TEST_MISSING", "")
	_, err := ResolveSecret("$RYSH_TEST_MISSING") // set-but-empty is fine
	if err != nil {
		t.Fatalf("empty set var should resolve: %v", err)
	}
	_, err = ResolveSecret("$RYSH_NO_SUCH_VAR_12345")
	if err == nil || !strings.Contains(err.Error(), "RYSH_NO_SUCH_VAR_12345") {
		t.Fatalf("missing var: got %v, want a not-set error", err)
	}
}

// The shell override and key bindings parse from their own TOML sections.
func TestShellAndKeyBindings(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
shell = "/bin/zsh"

[keys]
mode_switch = "ctrl-space"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Shell != "/bin/zsh" {
		t.Fatalf("shell = %q, want /bin/zsh", cfg.Shell)
	}
	if cfg.Keys.ModeSwitch != "ctrl-space" {
		t.Fatalf("mode_switch = %q, want ctrl-space", cfg.Keys.ModeSwitch)
	}
}

// Absent shell/keys default to auto / ctrl-tab semantics at the call site.
func TestShellAndKeysDefaultToEmpty(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Shell != "" {
		t.Fatalf("shell = %q, want empty (auto)", cfg.Shell)
	}
	if cfg.Keys.ModeSwitch != "" {
		t.Fatalf("mode_switch = %q, want empty (ctrl-tab)", cfg.Keys.ModeSwitch)
	}
}

// prompt_marker is off only when explicitly "off": unset or unknown values
// keep the marker (terminal title + bash/zsh prompt prefix) enabled,
// matching the other [tui] keys' silent-ignore-of-unknowns.
func TestPromptMarker(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  string
		want bool
	}{
		"unset":   {"", true},
		"auto":    {"[tui]\nprompt_marker = \"auto\"\n", true},
		"off":     {"[tui]\nprompt_marker = \"off\"\n", false},
		"unknown": {"[tui]\nprompt_marker = \"bogus\"\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, t.TempDir(), tc.cfg))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.TUI.PromptMarkerEnabled(); got != tc.want {
				t.Fatalf("PromptMarkerEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// The TUI prefs parse from their own [tui] section.
func TestTUIPrefs(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
[tui]
cursor_style = "block"
locale = "en"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TUI.CursorStyle != "block" {
		t.Fatalf("cursor_style = %q, want block", cfg.TUI.CursorStyle)
	}
	if cfg.TUI.Locale != "en" {
		t.Fatalf("locale = %q, want en", cfg.TUI.Locale)
	}
}

// Absent [tui] defaults to empty, which resolves to the bar at the call site.
func TestTUIDefaultsToEmpty(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TUI.CursorStyle != "" {
		t.Fatalf("cursor_style = %q, want empty (bar)", cfg.TUI.CursorStyle)
	}
}

// The agent env allowlist parses from the [agent] section and stays nil
// when absent, so the call site can tell "unset" (built-in default) apart
// from "explicitly empty" (expose no environment).
func TestAgentEnvAllowlist(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
[agent]
env_allowlist = ["HOME", "PATH"]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.EnvAllowlist == nil || len(cfg.Agent.EnvAllowlist) != 2 ||
		cfg.Agent.EnvAllowlist[0] != "HOME" || cfg.Agent.EnvAllowlist[1] != "PATH" {
		t.Fatalf("env_allowlist = %#v, want [HOME PATH]", cfg.Agent.EnvAllowlist)
	}
}

func TestAgentEnvAllowlistDefaultsToNil(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.EnvAllowlist != nil {
		t.Fatalf("env_allowlist = %#v, want nil (built-in default)", cfg.Agent.EnvAllowlist)
	}
}

func TestAgentEnvAllowlistEmpty(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
[agent]
env_allowlist = []
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.EnvAllowlist == nil {
		t.Fatal("env_allowlist = nil, want non-nil empty (expose no env)")
	}
	if len(cfg.Agent.EnvAllowlist) != 0 {
		t.Fatalf("env_allowlist = %#v, want empty", cfg.Agent.EnvAllowlist)
	}
}

// The bash timeout knobs resolve to the §8.1 defaults when unset, clamp
// to 1s at the floor, and auto_background_after=0 disables the window.
func TestAgentApprovalMode(t *testing.T) {
	// Unset → ask (writes default to asking in real projects).
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.ApprovalMode(); got != "ask" {
		t.Fatalf("default approval = %q, want ask", got)
	}

	// "always" and "never" pass through; unset, empty and unknown values
	// fall back to ask.
	for _, tc := range []struct {
		raw, want string
	}{
		{`approval = "ask"`, "ask"},
		{`approval = "always"`, "always"},
		{`approval = "never"`, "never"},
		{`approval = "auto"`, "ask"},
		{`approval = "yolo"`, "ask"},
		{`approval = ""`, "ask"},
	} {
		cfg, err = Load(writeConfig(t, t.TempDir(), "[agent]\n"+tc.raw+"\n"))
		if err != nil {
			t.Fatalf("Load(%s): %v", tc.raw, err)
		}
		if got := cfg.Agent.ApprovalMode(); got != tc.want {
			t.Fatalf("approval %s = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestAgentTimeoutKnobs(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.BashTimeoutDur(); got != 60*time.Second {
		t.Fatalf("default bash_timeout = %v, want 60s", got)
	}
	if got := cfg.Agent.BashMaxTimeoutDur(); got != 600*time.Second {
		t.Fatalf("default bash_max_timeout = %v, want 600s", got)
	}
	if got := cfg.Agent.AutoBackgroundDur(); got != 60*time.Second {
		t.Fatalf("default auto_background_after = %v, want 60s", got)
	}

	cfg, err = Load(writeConfig(t, t.TempDir(), `
[agent]
bash_timeout = 120
bash_max_timeout = 1800
auto_background_after = 30
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.BashTimeoutDur(); got != 120*time.Second {
		t.Fatalf("bash_timeout = %v, want 120s", got)
	}
	if got := cfg.Agent.BashMaxTimeoutDur(); got != 1800*time.Second {
		t.Fatalf("bash_max_timeout = %v, want 1800s", got)
	}
	if got := cfg.Agent.AutoBackgroundDur(); got != 30*time.Second {
		t.Fatalf("auto_background_after = %v, want 30s", got)
	}

	cfg, err = Load(writeConfig(t, t.TempDir(), `
[agent]
bash_timeout = 0
bash_max_timeout = -5
auto_background_after = 0
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.BashTimeoutDur(); got != time.Second {
		t.Fatalf("bash_timeout floor = %v, want 1s", got)
	}
	if got := cfg.Agent.BashMaxTimeoutDur(); got != time.Second {
		t.Fatalf("bash_max_timeout floor = %v, want 1s", got)
	}
	if got := cfg.Agent.AutoBackgroundDur(); got != 0 {
		t.Fatalf("auto_background_after = %v, want 0 (disabled)", got)
	}
}

// The [ai] prompt parses from its section; an absent one falls back to the
// default.
func TestAIPrompt(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), `
[ai]
prompt = "\\[\\x1b[1;30;45m[AI]\\x1b[0m\\] \\w > "
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AI.Prompt == "" {
		t.Fatal("[ai] prompt parsed empty")
	}
	if !strings.Contains(cfg.AIPrompt("ollama/x:1"), "[AI]") {
		t.Fatalf("AIPrompt = %q, want the configured prompt", cfg.AIPrompt("ollama/x:1"))
	}
	// A configured prompt is used verbatim: the default model segment never
	// overrides what the user wrote.
	if got := cfg.AIPrompt("ollama/x:1"); got != cfg.AI.Prompt {
		t.Fatalf("AIPrompt with a model = %q, want the configured prompt %q", got, cfg.AI.Prompt)
	}
}

func TestAIPromptDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, t.TempDir(), ``))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AIPrompt(""); got != DefaultAIPrompt {
		t.Fatalf("AIPrompt default = %q, want %q", got, DefaultAIPrompt)
	}
	if got := (&Config{}).AIPrompt(""); got != DefaultAIPrompt {
		t.Fatalf("empty config AIPrompt = %q, want default", got)
	}
	// With an active model the default names it (§3.3); the segment is part
	// of the default only, and an empty model never selects it.
	if got := (&Config{}).AIPrompt("ollama/llama3.1:8b"); got != DefaultAIPromptModel {
		t.Fatalf("AIPrompt with a model = %q, want %q", got, DefaultAIPromptModel)
	}
}

// The default prompt renders the current directory (never a literal \w)
// in a magenta accent and a plain-magenta [AI]: text marker on the main
// screen (rysh no longer pins a bottom status row).
// (Regression: \w must sit outside the \[...\] regions.)
func TestDefaultAIPromptExpansion(t *testing.T) {
	s, w := screen.PSExpand(DefaultAIPrompt, "/home/u/proj")
	if strings.Contains(s, `\w`) {
		t.Fatalf("default prompt leaked literal \\w: %q", s)
	}
	if !strings.Contains(s, "\x1b[1;35m/home/u/proj\x1b[0m") {
		t.Fatalf("default prompt did not expand the magenta cwd: %q", s)
	}
	if !strings.Contains(s, "\x1b[35m[AI]:\x1b[0m") {
		t.Fatalf("default prompt lost its plain-magenta [AI]: marker: %q", s)
	}
	if strings.Contains(s, "\x1b[1;30;45m[AI]\x1b[0m") {
		t.Fatalf("default prompt uses the old filled [AI] status chip: %q", s)
	}
	if w != len("/home/u/proj")+7 { // cwd + " [AI]: "
		t.Fatalf("default prompt visible width = %d, want %d", w, len("/home/u/proj")+7)
	}

	// The default with an active model renders "cwd · model [AI]:" (§3.3)
	// and counts the model's cells in the visible width.
	ms, mw := screen.PSExpandModel(DefaultAIPromptModel, "/home/u/proj", "ollama/x:1")
	if !strings.Contains(ms, "\x1b[2m· ollama/x:1\x1b[0m") {
		t.Fatalf("model prompt lost its dim model segment: %q", ms)
	}
	if strings.Contains(ms, `\m`) {
		t.Fatalf("model prompt leaked literal \\m: %q", ms)
	}
	// The width is counted in cells, not bytes: the separator "·" is a single
	// cell that utf8 encodes as 2 bytes.
	wantMW := utf8.RuneCountInString("/home/u/proj · ollama/x:1 [AI]: ")
	if mw != wantMW {
		t.Fatalf("model prompt visible width = %d, want %d", mw, wantMW)
	}

	// \m with no model renders nothing, so a prompt can never show a stray
	// escape or an empty segment's content.
	if es, _ := screen.PSExpandModel(`\m[AI]:`, "", ""); es != "[AI]:" {
		t.Fatalf(`PSExpandModel with no model = %q, want the token dropped`, es)
	}
}
