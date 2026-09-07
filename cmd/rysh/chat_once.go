// One-shot AI chat (`rysh ai "message"`): renders the reply with markdown
// to stdout, writes the exchange to the active session log, and exits
// without creating a pty or entering AI mode.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ruyishell/internal/agent"
	"ruyishell/internal/aiui"
	"ruyishell/internal/config"
	"ruyishell/internal/markdown"
	"ruyishell/internal/provider"
	"ruyishell/internal/session"
)

// chatOnce runs one streaming request and prints the markdown-rendered
// reply. It writes the usr prompt and the asw reply into the session's
// log so the conversation continues in the interactive rysh, and updates
// the session's display name from the prompt when it is still unnamed.
// No agent tool loop runs (a one-shot query should not spawn further
// commands from a subprocess).
func chatOnce(text string) int {
	cfgPath, err := config.Path()
	if err != nil {
		cfgPath = "~/.rysh/config.toml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		cfg = &config.Config{}
	}
	ref := provider.Default(cfg)
	if ref == "" {
		fmt.Println("no models configured (edit " + cfgPath + ")")
		return 1
	}
	spec, err := provider.Resolve(cfg, ref)
	if err != nil {
		fmt.Println("rysh: " + err.Error())
		return 1
	}

	store := sessionStore()
	cwd, _ := os.Getwd()

	// Resolve the target session: RYSH_SESSION_ID (inside a rysh: the
	// instance's active session, always writable) wins; otherwise the most
	// recently updated session that no live rysh holds; otherwise a new
	// session. Top-level targets restored from the ledger are rejected when
	// another live rysh is attached to them (concurrent writes to one log).
	var meta *session.Meta
	if id := os.Getenv(sessionEnv); id != "" {
		if store != nil {
			if m, err := store.EnsureSession(id); err == nil {
				meta = m
			}
		}
		if meta == nil {
			meta = &session.Meta{ID: id, Created: time.Now().UnixMilli()}
		}
	} else if store != nil {
		if id := store.LastUpdatedID(); id != "" {
			if _, attached := attachedByOther(store, id, selfPID()); !attached {
				if m, err := resolveSession(store, id); err == nil {
					meta = m
				}
			}
		}
		if meta == nil {
			m, err := store.CreateSession()
			if err != nil {
				fmt.Fprintln(os.Stderr, "rysh: "+err.Error())
				return 1
			}
			meta = m
			_ = store.TouchUpdated(m.ID)
		}
	} else {
		meta = &session.Meta{ID: session.NewID(), Created: time.Now().UnixMilli()}
	}

	// Open the session log and rebuild the conversation context from disk.
	sessionsDir := ""
	if store != nil {
		sessionsDir = store.SessionsDir()
	} else if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		sessionsDir = home + "/.rysh/sessions"
	}
	var log *session.Log
	if sessionsDir != "" {
		log, _ = session.OpenLog(sessionsDir, meta.ID)
		if log != nil {
			defer log.Close()
		}
	}
	hist := readHistory(sessionsDir, meta.ID)

	// Build the request: cwd, allowlisted environment, agent instructions,
	// the session history, and the prompt.
	messages := []provider.ChatMessage{}
	if cwd != "" {
		messages = append(messages, provider.ChatMessage{Role: "system", Content: "cwd: " + cwd})
	}
	if env := filteredEnv(cfg); env != "" {
		messages = append(messages, provider.ChatMessage{Role: "system", Content: "env:\n" + env})
	}
	messages = append(messages, provider.ChatMessage{Role: "system", Content: agent.ChatInstructions})
	for _, m := range hist {
		messages = append(messages, m.Msg)
	}
	messages = append(messages, provider.ChatMessage{Role: "user", Content: text})

	if log != nil {
		log.Write("usr", aiui.Sanitize(text))
	}
	if store != nil {
		_ = store.TouchUpdated(meta.ID)
	}

	// Replay the conversation context, then stream the reply. The output
	// uses \n: the inherited pty slave is cooked (ONLCR), so the shell adds
	// the carriage return.
	if replay := renderHistoryReplay(hist); replay != "" {
		fmt.Print(replay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := provider.NewClient(spec)
	ch, err := client.ChatStream(ctx, messages)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rysh: "+err.Error())
		if log != nil {
			log.Write("noti", "rysh: "+err.Error())
		}
		return 1
	}
	md := markdown.New()
	var reply strings.Builder
	for tok := range ch {
		if tok.Reasoning {
			continue
		}
		fmt.Print(md.Write(tok.Text))
		reply.WriteString(tok.Text)
	}
	if held := md.Close(); held != "" {
		fmt.Print(held)
	}
	fmt.Println()
	if log != nil && reply.Len() > 0 {
		log.Write("asw", session.SanitizeStream(reply.String()))
	}
	if store != nil {
		updateSessionTitle(store, meta, text)
	}
	return 0
}

// filteredEnv returns the allowlisted environment as "KEY=VALUE" lines for
// the agent context (the same allowlist the interactive mode uses).
func filteredEnv(cfg *config.Config) string {
	allow := envAllowlistFor(cfg)
	if len(allow) == 0 {
		return ""
	}
	want := make(map[string]bool, len(allow))
	for _, k := range allow {
		want[k] = true
	}
	var lines []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && want[k] {
			lines = append(lines, kv)
		}
	}
	return strings.Join(lines, "\n")
}
