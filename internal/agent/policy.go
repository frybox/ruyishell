package agent

// Approval policy (design §7): the ask/auto permission gate in front of
// tool execution. Read-only work runs free — registry reads (read/glob/
// grep/ls/job_output) always, and bash when it matches the §7.1
// safe-command classifier. Everything else (write/edit every time, bash
// outside the classifier) prompts y/n/a in "ask" mode; "auto" runs
// everything (the pre-M7.4 behavior). An answer of "a" records a
// session-persistent rule: bash rules are command prefixes (§7.2), file
// tools are remembered per tool name. Denial never kills the task — the
// refusal is fed back and the model picks another route (§7 审批被拒).
// Background workers run the gate's worker view (WorkerGate): the same
// core — mode and session rules read live — but no prompt, so gated
// calls are denied for them instead (§16).

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Answer is the user's decision on one approval prompt.
type Answer int

const (
	AnswerDeny   Answer = iota // n: refuse this call
	AnswerAllow                // y: allow this call once
	AnswerAlways               // a: allow and remember a session rule
)

// String renders the answer for the session log's approval record.
func (a Answer) String() string {
	switch a {
	case AnswerAllow:
		return "allow"
	case AnswerAlways:
		return "always"
	default:
		return "deny"
	}
}

// AskRequest is one pending approval shown to the user (§11.3).
type AskRequest struct {
	Tool    string // "bash", "write" or "edit"
	Command string // bash command line; empty for file tools
	Display string // display line for the prompt, e.g. `rm -rf build/`
}

// approvalCore is the session's mutable permission state: the mode, the
// persistent rules and the safe-extra heads. The interactive gate and
// every worker view (WorkerGate) share one core, so a rule the user
// records mid-session — or the mode hot-reload at each task start —
// applies to workers that are already running.
type approvalCore struct {
	mu           sync.Mutex
	mode         string          // "ask" (default) or "auto"
	bashPrefixes map[string]bool // session rules: "git push" → allow
	alwaysTools  map[string]bool // session rules: per-tool "always"
	safeExtra    []string        // extra read-only head words treated as safe
}

// Approval carries one permission gate over the session core. The driver
// supplies Ask (it blocks on the UI and must honor ctx cancellation by
// returning AnswerDeny); the engine consults Gate before each tool
// execution. Ask nil makes the gate fail closed: everything that would
// have asked is denied with the worker text — WorkerGate returns exactly
// that view of the same core for background workers (§16).
type Approval struct {
	core *approvalCore
	ask  func(ctx context.Context, req AskRequest) Answer
}

// NewApproval builds the gate for one mode and ask callback. Unknown
// modes fall back to ask.
func NewApproval(mode string, ask func(ctx context.Context, req AskRequest) Answer) *Approval {
	a := &Approval{core: &approvalCore{
		bashPrefixes: map[string]bool{},
		alwaysTools:  map[string]bool{},
	}, ask: ask}
	a.SetMode(mode)
	return a
}

// WorkerGate returns the non-interactive view of this session gate for
// background workers (§16): the same core — mode, safe classifier and
// session rules, read live — but no UI to answer, so anything that would
// ask is denied with the worker text instead of blocking. A worker
// blocking on an answer the user is not watching is a hidden deadlock;
// the denial is fed back like any refusal and the manager re-runs the
// step in the foreground, where the same call can prompt the user.
func (a *Approval) WorkerGate() *Approval {
	return &Approval{core: a.core}
}

// SetMode switches ask/auto; only the exact value "auto" enables auto.
// The driver re-reads the mode from the config at each task start, so a
// config edit applies to the next run without a restart.
func (a *Approval) SetMode(mode string) {
	if mode != "auto" {
		mode = "ask"
	}
	a.core.mu.Lock()
	defer a.core.mu.Unlock()
	a.core.mode = mode
}

// SetSafeExtra extends the safe-command classifier with additional
// read-only head words (§7.1 白名单可配增补); they never widen the shape
// rules — a command with shell metachars still asks.
func (a *Approval) SetSafeExtra(heads []string) {
	a.core.mu.Lock()
	defer a.core.mu.Unlock()
	a.core.safeExtra = append([]string(nil), heads...)
}

// bashLooksSafeWith reports whether cmd is read-only under the §7.1
// classifier: the built-in safe shape and head words, or a configured
// extra head word with the same shape. extra is a snapshot of
// Approval.safeExtra taken by the caller.
func bashLooksSafeWith(extra []string, cmd string) bool {
	if bashLooksReadOnly(cmd) {
		return true
	}
	if len(extra) == 0 {
		return false
	}
	squashed := strings.Join(strings.Fields(cmd), " ")
	if squashed == "" || !safeShape(squashed) {
		return false
	}
	head := strings.SplitN(squashed, " ", 2)[0]
	for _, s := range extra {
		if s == head {
			return true
		}
	}
	return false
}

// multiWordHeads are commands whose approval prefix spans two tokens
// (the subcommand): approving `git push` must not swallow `git status`,
// and vice versa.
var multiWordHeads = map[string]bool{
	"git": true, "go": true, "docker": true, "npm": true, "pnpm": true,
	"yarn": true, "cargo": true, "systemctl": true, "service": true,
	"apt": true, "apt-get": true, "brew": true, "pip": true, "pip3": true,
}

// bashPrefix extracts the §7.2 rule prefix of a bash command: the first
// non-option token, extended to the second non-option token when the head
// is a known two-token command. Options are skipped, so `rm -rf build`
// yields "rm" and `git push --force` yields "git push" (opencode parses
// the command with tree-sitter; this token scan is the dependency-free
// simplification the design settled for). A two-token head with only
// options after it yields "" — a bare `git` must not mint an "allow all
// git" rule. Note that options taking a separate argument (git -C path)
// make the argument the second token; the rule is narrower than intended
// but never more permissive than what was approved.
func bashPrefix(cmd string) string {
	var toks []string
	for _, f := range strings.Fields(cmd) {
		if strings.HasPrefix(f, "-") {
			continue
		}
		toks = append(toks, f)
		if len(toks) == 2 {
			break
		}
	}
	if len(toks) == 0 {
		return ""
	}
	if multiWordHeads[toks[0]] {
		if len(toks) < 2 {
			return ""
		}
		return toks[0] + " " + toks[1]
	}
	return toks[0]
}

// denyText is the fed-back result body for a refused call: the model is
// told the user refused and must pick another route (§7 审批被拒).
func denyText(display string) string {
	return "[用户拒绝了该操作] " + display + " —— 请改用其他方案，或基于已有信息直接推进。"
}

// workerDenyText is the fed-back result for a worker gate denial: there
// is no user to prompt in a background worker, so the call is refused
// (fail closed) and the step is reported for the manager to run in the
// foreground, where the same call can prompt the user (§16).
func workerDenyText(display string) string {
	return "[子任务无法请求用户批准] " + display + " —— 后台子任务内该操作不可执行，不要重试。请基于已有信息推进其余工作，并在报告「未完成」中列出这一步，由主任务在前台执行。"
}

// approvalDisplay renders the [tool] observation line for a gated call
// (shown when the call is refused; mirrors the tools' own Display shapes).
func approvalDisplay(tool string, args map[string]any) string {
	switch tool {
	case "bash":
		return "$ " + strings.Join(strings.Fields(argOptString(args, "command")), " ")
	case "write", "edit":
		return tool + " " + strings.TrimSpace(argOptString(args, "path"))
	}
	return tool
}

// Gate decides whether one parsed tool call may execute (§7.1). It
// returns "" to allow, or the deny text to feed back in place of a
// result. Only write/edit (always) and bash outside the safe classifier
// are gated; every other tool — the read family, todo, job_kill — passes
// unconditionally.
func (a *Approval) Gate(ctx context.Context, tool string, args map[string]any) string {
	command, display := "", ""
	switch tool {
	case "bash":
		command = argOptString(args, "command")
		display = strings.Join(strings.Fields(command), " ")
	case "write", "edit":
		display = strings.TrimSpace(argOptString(args, "path"))
	default:
		return ""
	}

	a.core.mu.Lock()
	mode := a.core.mode
	extra := a.core.safeExtra
	always := a.core.alwaysTools[tool]
	var rule string
	if tool == "bash" {
		rule = bashPrefix(command)
		always = always || (rule != "" && a.core.bashPrefixes[rule])
	}
	a.core.mu.Unlock()

	if mode == "auto" {
		return ""
	}
	if tool == "bash" && bashLooksSafeWith(extra, command) {
		return ""
	}
	if always {
		return ""
	}
	if a.ask == nil {
		// Fail closed: no UI attached to answer the prompt (worker view).
		return workerDenyText(display)
	}
	switch a.ask(ctx, AskRequest{Tool: tool, Command: command, Display: display}) {
	case AnswerAllow:
		return ""
	case AnswerAlways:
		a.core.mu.Lock()
		if tool == "bash" {
			if rule != "" {
				a.core.bashPrefixes[rule] = true
			}
		} else {
			a.core.alwaysTools[tool] = true
		}
		a.core.mu.Unlock()
		return ""
	default:
		return denyText(display)
	}
}

// RuleSummary lists the persistent rules, one per line ("bash: git push",
// "tool: write"), sorted for display. Empty when none were recorded.
func (a *Approval) RuleSummary() string {
	a.core.mu.Lock()
	defer a.core.mu.Unlock()
	var lines []string
	for r := range a.core.bashPrefixes {
		lines = append(lines, "bash: "+r)
	}
	for t := range a.core.alwaysTools {
		lines = append(lines, "tool: "+t)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
