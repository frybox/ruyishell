// Package config loads ruyishell's provider/model configuration from
// ~/.rysh/config.toml. The layout mirrors the models.json schema used by the
// pi agent: a providers table, each provider carrying a base URL, an API
// type, credentials and a list of models. Every call to Load re-reads the
// file from disk, so editing it takes effect without restarting ruyishell
// (the /model command relies on this).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Model describes one model under a provider.
type Model struct {
	ID        string `toml:"id"`
	Name      string `toml:"name"`
	API       string `toml:"api"`       // per-model API override
	BaseURL   string `toml:"base_url"`  // per-model endpoint override
	Reasoning bool   `toml:"reasoning"` // supports extended thinking
	// Tools opts the model out of native function calling; nil (unset) keeps
	// it enabled. Endpoints that reject tools with a 400 are auto-detected
	// at request time and fall back to the fence protocol.
	Tools         *bool             `toml:"tools"`
	ContextWindow int               `toml:"context_window"`
	MaxTokens     int               `toml:"max_tokens"`
	Headers       map[string]string `toml:"headers"`
}

// Provider describes one provider and the models it exposes.
type Provider struct {
	Name    string            `toml:"name"`
	BaseURL string            `toml:"base_url"`
	API     string            `toml:"api"`     // e.g. "openai-completions"
	APIKey  string            `toml:"api_key"` // literal or $ENV_VAR reference
	Headers map[string]string `toml:"headers"`
	Models  []Model           `toml:"models"`
}

// Config is the parsed configuration file.
type Config struct {
	// Default is the "provider/model" reference selected at startup.
	Default   string               `toml:"default"`
	Providers map[string]*Provider `toml:"providers"`
	// Shell is the shell to launch: "auto" (or empty) probes the platform
	// default; any other value is an explicit shell path. Applies at
	// startup only (the shell process is spawned once).
	Shell string `toml:"shell"`
	// Keys holds keyboard bindings.
	Keys KeyBindings `toml:"keys"`
	// TUI holds terminal-UI preferences.
	TUI TUI `toml:"tui"`
	// Agent holds settings for what the agent sees in context.
	Agent AgentConfig `toml:"agent"`
	// AI holds settings for the AI-mode conversation UI.
	AI AIConfig `toml:"ai"`
}

// AgentConfig holds settings for the agent's view of the shell.
type AgentConfig struct {
	// EnvAllowlist lists the environment variables exposed to the model in
	// context (an "env:" system message). When absent, a built-in default
	// allowlist is used; when present (even empty), it replaces the default
	// exactly, so setting env_allowlist = [] exposes no environment at all.
	// Applies per request (the config is re-read each time, like /model).
	EnvAllowlist []string `toml:"env_allowlist"`
	// BashTimeout is the per-command default timeout in seconds (§8.1).
	// nil → 60; values below 1 clamp to 1.
	BashTimeout *int `toml:"bash_timeout"`
	// BashMaxTimeout caps the model-side `timeout` argument in seconds.
	// nil → 600; values below 1 clamp to 1.
	BashMaxTimeout *int `toml:"bash_max_timeout"`
	// AutoBackground is the §8.1 foreground window in seconds: a bash call
	// still running after this long becomes a background job. nil → 60;
	// 0 disables auto-backgrounding.
	AutoBackground *int `toml:"auto_background_after"`
	// Approval is the §7.2 permission mode: "auto" runs every tool call
	// without asking; anything else (including unset) is "ask" — read-only
	// work runs free, writes and non-safe bash prompt y/n/a first. Re-read
	// per task like the rest of [agent], so edits apply to the next run.
	Approval *string `toml:"approval"`
}

// BashTimeoutDur resolves the §8.1 default timeout.
func (a *AgentConfig) BashTimeoutDur() time.Duration {
	if a.BashTimeout == nil {
		return 60 * time.Second
	}
	if *a.BashTimeout < 1 {
		return time.Second
	}
	return time.Duration(*a.BashTimeout) * time.Second
}

// BashMaxTimeoutDur resolves the model-adjustable timeout cap.
func (a *AgentConfig) BashMaxTimeoutDur() time.Duration {
	if a.BashMaxTimeout == nil {
		return 600 * time.Second
	}
	if *a.BashMaxTimeout < 1 {
		return time.Second
	}
	return time.Duration(*a.BashMaxTimeout) * time.Second
}

// AutoBackgroundDur resolves the auto-background window (0 = disabled).
func (a *AgentConfig) AutoBackgroundDur() time.Duration {
	if a.AutoBackground == nil {
		return 60 * time.Second
	}
	if *a.AutoBackground < 0 {
		return 0
	}
	return time.Duration(*a.AutoBackground) * time.Second
}

// ApprovalMode resolves the §7.2 permission mode: only the exact value
// "auto" enables it; unset or any other value falls back to "ask" (rysh
// runs in the user's real projects, so writes default to asking first).
func (a *AgentConfig) ApprovalMode() string {
	if a.Approval != nil && *a.Approval == "auto" {
		return "auto"
	}
	return "ask"
}

// TUI holds terminal-UI preferences.
type TUI struct {
	// CursorStyle is the shell-mode terminal cursor: "bar" (default,
	// blinking bar), "block" (solid block), or "default" (leave the
	// terminal's cursor unchanged). It is still parsed out of [tui] but
	// never read and never validated: rysh draws on the main screen
	// without a status row, so it no longer sends any DECSCUSR sequence
	// (主屏方案 §6).
	CursorStyle string `toml:"cursor_style"`
	// Locale selects the language of user-visible UI text: "zh" (default)
	// or "en". When empty the language comes from the environment (an
	// en* LANG means English, anything else Chinese). One-shot
	// subcommands (rysh ls/kill/...) do not read this file, so they
	// resolve the language from the environment only.
	Locale string `toml:"locale"`
	// PromptMarker controls the "you are inside rysh" indicators: while
	// rysh runs it owns the terminal title (rysh · model · session, the
	// shell's own OSC 0/2 title escapes are stripped from the stream) and,
	// for bash and zsh child shells, a dim "(rysh)" prefix is prepended to
	// the prompt — bash via a PROMPT_COMMAND one-liner exported into the
	// child env, zsh via a private ZDOTDIR whose .zshenv registers a
	// precmd hook. "off" disables all of it and restores the pure byte
	// passthrough of the original 零侵入 design; any other value
	// (including unset) means "auto". Read once at startup.
	PromptMarker string `toml:"prompt_marker"`
}

// PromptMarkerEnabled reports whether the prompt-marker indicators (terminal
// title, shell prompt prefix) are active: every value other than "off"
// enables them, matching the other TUI keys' silent-ignore-of-unknowns.
func (t *TUI) PromptMarkerEnabled() bool {
	return t.PromptMarker != "off"
}

// AIConfig holds settings for the AI-mode conversation UI.
type AIConfig struct {
	// Prompt is the bash-PS-style prompt rysh draws for the AI input line.
	// Escapes: \u user, \h hostname, \w current directory (HOME shown as
	// ~), \t time (HH:MM:SS), \\ literal backslash, \[...\] a non-printing
	// region. Re-read on every mode entry, so edits take effect without a
	// restart. It only affects rysh's own drawn prompt, not the shell's PS.
	Prompt string `toml:"prompt"`
}

// DefaultAIPrompt is the prompt used when [ai] prompt is unset and no model
// is configured: the current directory in a magenta accent (the AI theme), a
// plain-magenta [AI]: marker so the AI input line reads distinctly and ends in
// a colon — like a natural-language prompt rather than a shell prompt. The
// marker is plain text, not the filled [AI] chip the old pinned bottom status
// row used — the chip on the scrolling prompt line would drift up with the
// reply and read as a status badge.
const DefaultAIPrompt = `\[\x1b[1;35m\]\w\[\x1b[0m\] \x1b[35m[AI]:\x1b[0m `

// DefaultAIPromptModel is the default prompt with the active model ref
// inserted between the directory and the marker, so the AI input line reads
// "cwd · model [AI]:" — the prompt is the only place rysh names the model now
// that the bottom status row is gone. \m expands to the ref.
const DefaultAIPromptModel = `\[\x1b[1;35m\]\w\[\x1b[0m\] \x1b[2m· \m\x1b[0m \x1b[35m[AI]:\x1b[0m `

// AIPrompt returns the effective AI prompt template for the active model ref:
// the configured [ai] prompt as written, or the default — which names the
// model when one is configured and drops the segment entirely when none is, so
// an empty model never leaves a dangling separator on the line.
func (c *Config) AIPrompt(model string) string {
	if c != nil && c.AI.Prompt != "" {
		return c.AI.Prompt
	}
	if model == "" {
		return DefaultAIPrompt
	}
	return DefaultAIPromptModel
}

// KeyBindings holds configurable keyboard bindings.
type KeyBindings struct {
	// ModeSwitch is the binding that toggles shell/AI mode: "shift-tab"
	// (default, CSI Z), "ctrl-tab", "ctrl-space", or "ctrl-backslash".
	// Applies at startup only (the input reader is created once). The
	// default Shift+Tab avoids the Ctrl+Tab that many terminals (e.g.
	// Windows Terminal) reserve for their own tab cycling.
	ModeSwitch string `toml:"mode_switch"`
}

// Path returns the configuration file path: $RYSH_CONFIG if set, otherwise
// ~/.rysh/config.toml.
func Path() (string, error) {
	if p := os.Getenv("RYSH_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: home dir: %w", err)
	}
	return filepath.Join(home, ".rysh", "config.toml"), nil
}

// Load reads and parses the configuration file at path. A missing file
// yields an empty Config (no error); a malformed file yields an error.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	_, err := toml.DecodeFile(path, cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

// ResolveSecret expands environment-variable references the way pi's
// models.json does: $VAR and ${VAR} substitute the named environment
// variable, $$ is a literal dollar sign, and a missing variable is an error.
// Plaintext and any other content pass through unchanged.
func ResolveSecret(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '$':
			b.WriteByte('$')
			i += 2
		case i+1 < len(s) && s[i+1] == '!':
			b.WriteByte('!')
			i += 2
		default:
			name, next, ok := envName(s, i)
			if !ok {
				b.WriteByte('$')
				i++
				continue
			}
			v, present := os.LookupEnv(name)
			if !present {
				return "", fmt.Errorf("config: environment variable %s is not set", name)
			}
			b.WriteString(v)
			i = next
		}
	}
	return b.String(), nil
}

// envName parses an environment-variable reference starting at s[i] == '$'.
// It handles both $NAME and ${NAME}. ok is false when no name follows.
func envName(s string, i int) (name string, next int, ok bool) {
	if i+1 >= len(s) {
		return "", i, false
	}
	if s[i+1] == '{' {
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", i, false
		}
		return s[i+2 : i+2+end], i + 2 + end + 1, true
	}
	j := i + 1
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	if j == i+1 {
		return "", i, false
	}
	return s[i+1 : j], j, true
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
