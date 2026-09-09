# ruyishell

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[中文版](README.zh-CN.md)

A next-generation interactive shell (command: `rysh`) aimed at enhancing the AI interaction experience of your local shell — your shell by default, AI as the co-pilot, with one keypress to switch modes on the very same input line.

## Why ruyishell

Working with AI agents has become part of the daily routine — codex, claude code, pi, grok. Describing what you want in plain language genuinely feels great. But as a developer, a lot of the time all I need is to run a shell command — say, an `ls` to glance at what changed in the local files. If I ask for that in plain language ("show me the files in the current directory"), the chain is: send to the model → the model reasons → it emits a bash tool call → the agent executes it → the result is fed back to the model for a summary → and only then does the final answer reach me. By the time I see the result, I'm going to be crazy.

The GUI versions of these tools all ship with a built-in terminal window, but "open terminal, run the command, close the terminal" is not a great experience either. And the CLI versions? Either you exit the tool temporarily, run your commands in the shell, then relaunch the tool and restore your session — or you keep two windows open: one for the agent, one for the shell.

So I built ruyishell. It looks exactly like your everyday bash/zsh/fish (cmd/pwsh on Windows) — the only tell is a small dim `(rysh)` mark in front of the prompt. But when there is no keyboard input after the prompt, pressing Space swaps the prompt: ruyishell becomes a plain-language AI agent. And while you're in the AI, with nothing typed after the prompt, pressing Space takes you back to your familiar shell, where you can run `vi`, `ls`, or any other shell command. When you already have text on the command line, `Shift+Tab` performs the mode switch instead.

One more thing: every configuration change in ruyishell is hot — it takes effect immediately, no restart. One of the frustrating things about grok and claude code is that a config change forces you to restart the tool and then `/resume` your session; ruyishell has none of that interruption.

And one scenario that always bugged me: mid-work, I want the current session to move to a different directory (`cd`). With other agent tools, the only way is to exit the session, change directories, and restart — at which point it is a brand-new session. In ruyishell, you can simply switch to shell mode, `cd` to wherever you want, and switch back to AI mode: the current directory has changed, and the session is still the same session.

## Quick Start

`rysh` is your existing shell plus an AI copilot sharing the same screen. It does **not** replace your terminal or your shell: it launches your usual shell (bash/zsh/fish/...) and lets you step into an AI mode on the very same input line. Your half-typed command travels with you across the switch, the AI can read your shell history and current directory, and stepping back returns your draft to the prompt.

```
curl -fsSL https://github.com/frybox/ruyishell/releases/latest/download/install.sh | bash    # prebuilt binary; see "Installation" for all routes
rysh                                            # start — you land in your normal shell
```

Then, with your shell running:

1. Press `Shift+Tab` to enter AI mode (or type a space at the start of a line).
2. Type a task in plain language and press Enter — the reply streams in place and you stay in AI mode to follow up.
3. Press `Shift+Tab` to return to your shell; whatever you had typed comes back.

## Typical Scenarios

- **Ask the AI mid-work without leaving your shell**: halfway through a `grep` and forgotten an option? `Shift+Tab`, describe the problem to the AI, get the full command back, `Shift+Tab` to switch to the shell and run it. No need to leave what you're doing.
- **Hand the AI a multi-step job and watch it work**: "collect all the TODOs in `docs/*.md` into one file" — the AI drives its own tools (read/grep/write, ...) step by step, each command shows up as a dim `[tool]` line, `^C` interrupts at any time.
- **Run several AI sessions in one terminal** (`/new` to create, `/ls` to list, `/resume <n>` to switch back), each with isolated context, cwd and draft; one-shot subcommands like `rysh ls` work in any terminal.
- **Multi-line paste works in both the AI draft and the shell prompt** (bracketed paste): the pasted block keeps its newlines and never accidentally submits; one `Ctrl+Z` reverts the whole block.

## How It Differs from Warp

Warp *replaces your terminal* (a standalone GPU-rendered GUI app, closed source, AI bound to its cloud service); `rysh` *augments your shell* — it runs inside any terminal you already use and hands the terminal back to you on exit. The core differences: model freedom (BYO provider — any OpenAI-compatible endpoint, fully local if you want, hot `/model` switching, no account, no telemetry) and zero-friction mode switching (same input line, one key, content preserved, the AI can read your shell history, one shared screen).

## Documentation

- [Product direction](docs/产品方向.md)
- [Requirements analysis](docs/需求分析.md)
- [Architecture design](docs/架构设计.md)
- [Main-screen design](docs/主屏方案.md) (screen model: main screen only, native scrollback, no status line)
- [Work plan](docs/工作计划.md)

## Status

The entries below record delivered capabilities milestone by milestone, including the UI shape at the time. As of 2026-08-29 the screen model follows the [main-screen design](docs/主屏方案.md): rysh stays on the terminal's main screen the whole time — no bottom status line, no scroll-region cropping, no clear-on-exit — and earlier entries describing those have been superseded.

M2 (mode switching and visual feedback) implemented and verified on Linux: one-key shell/AI switch, bottom status line, cursor-shape linkage; unit + integration tests pass; Windows cross-compile passes (unverified at runtime); macOS unverified.

M3 (AI mode + provider integration) implemented: provider/model configuration (following pi's models.json structure, TOML carrier) + `/model` hot-switch + agent loop (plain language → streaming reply → stay in AI mode to follow up). See the [work plan](docs/工作计划.md).

M4 (tool calls and session context) complete: multi-turn memory for AI conversation + `/new` new session + cwd/env-var tracking + agent tool calls + unified event stream, all shipped (the agent perceives the current working directory and shell environment, read via `/proc` on Linux, environment filtered by an allowlist so no secrets leak; the model may emit `bash` fenced blocks in its reply, which rysh executes and feeds the output back into context; commands the user runs in shell mode and their output also enter the AI context, each tagged with its cwd).

M5+ (TUI enhancement, v4 one-screen-one-stream) implemented: AI and shell share the same screen and the same terminal stream — mode switching does not swap alternate screens, does not clear, does not rebuild the shell view; it only swaps the shell prompt line for the AI prompt line in place (bottom status line on a light-gray full-line background, `[SH]`/`[AI]` badge + cursor-shape linkage, mode switching locked while a task streams in AI mode, only `^C` can terminate; on rysh exit the whole screen is cleared before the terminal is handed back). Shell and AI share the same input line: on Shift+Tab, the half-typed command in the shell carries over into the AI input line, and the AI draft is injected back into the shell command line on the way back — both text and cursor position persist across the switch (the shell cursor column carries into the AI draft; on the way back arrow keys reinject the shell cursor to the same column; only for shells with a line editor such as bash/zsh/fish/ksh — plain shells like dash have no line editor, so the cursor returns to end of line). The AI prompt line supports bash-PS-style configuration (`[ai] prompt`). Reply streaming renders on the shared stream: reasoning shown dim, reply body rendered as markdown (headings bold+underlined, bold/italic, inline code cyan, links underlined with dim URL, lists and blockquotes, fenced code blocks with gray background fill — unclosed constructs do not flash raw markers before they close), tool calls shown as gray `[tool] $ <command> (exit <N>)`. While in AI mode, shell output is buffered and logged to the session log (`~/.rysh/sessions/<id>/messages.log`, append-only JSONL) and flushed in one batch on return to the shell; a bare Enter lets the shell redraw its own prompt to complete re-synchronization. Shell mode stays byte-transparent; the bottom status line's `[SH]` badge and scroll-region protection are unchanged.

Agent budget management, phase one ([architecture design](docs/架构设计.md) §3.9 M1) implemented and verified on Linux: removed the `maxToolRounds=4` round cap (no round limit on the tool loop); added a loop guard — a consecutive run of ≥6 repeats of the same signature, or a saturated-window share >60%, is judged runaway and a wrap-up instruction is injected to force a text summary (screen shows `─── wrapping up (loop guard) ───`; from the 2nd consecutive repeat a warning is prepended); L1 context slimming — at request assembly time, tool output older than the 64KB tail budget is collapsed into a one-line placeholder. Token accounting (M7.2) and L2 summarization (M7.6) landed in later milestones; the `[agent]` configuration surface is phase three. See the [work plan](docs/工作计划.md).

M7.1/M7.2 (the first two steps of the agent execution-flow rework) implemented and verified on Linux (design: [agent execution flow design](docs/agent执行流程设计.md)): tool calls upgraded to provider-native function calling, with the markdown fence protocol kept as a fallback (auto-degrades when the model does not support it or `tools = false` is configured; behavior identical to M4); the execution loop moved out of main.go into a standalone engine (`internal/agent`) driving eight tools — bash (per-call timeout tunable, mid-collapse of large outputs), read (line numbers + continue), write, edit (exact unique match), glob/grep (respects .gitignore inside a git repo), ls, todo (model-maintained task list). The engine has built-in retry (exponential backoff on 429/5xx, server Retry-After preferred), a runaway-loop guard (a repeated identical action: warn first, then force a wrap-up summary), and a read-idling guard (repeated re-reading with no long-term output: prompt a re-plan first, wrap up only if still no progress); old tool results are folded out of the request on a 64KB budget; token usage counts reported values first with estimation as fallback. Errors from weak parameters / unknown tools are fed back to the model for self-correction rather than interrupting the task; `^C` terminates the current task and restores input. Permission approval: see M7.4 below. See the [work plan](docs/工作计划.md).

M7.3 (long commands and background jobs) implemented and verified on Linux: new `[agent]` settings `bash_timeout`/`bash_max_timeout`/`auto_background_after` (defaults 60s/600s/60s). When a foreground command survives its auto-background window without ending, rysh adopts it as a background job (separate process group, cap 50, oldest finished jobs reaped first) and reports back "backgrounded as job N" — the command keeps running; the model inspects status and tail output with `job_output` (default last 200 lines; per-job output keeps 128KB head and tail with a collapsed middle count) and stops it with `job_kill`; a job outlives task end and `^C`, cleaned up only on rysh exit. `^C` gains two-level semantics: during command execution, `^C` only interrupts the current foreground command (result fed back as "interrupted by user", the task continues, the model decides the next step); during streaming reply, `^C` still terminates the whole task.

M7.4 (permission approval) implemented and verified on Linux: before an AI task runs a tool it passes a permission gate — read-only tools (read/glob/grep/ls/todo/job_*) and bash that hits the safety classifier (known read-only command words + no shell metacharacters/redirects in the whole line) pass straight through; write/edit always asks; bash that misses the classifier pops a full-line, amber-highlighted approval bar `? 运行 <命令>（y 是 / n 否 / a 总是）` with the status line showing "awaiting confirmation". `y` allows this one; `n` denies — the denial text is fed back to the model, the task is not interrupted, the model finds another way itself; `a` allows and records a session-level rule (bash by command prefix — approving `git push` does not carry over to `git status`; file tools by tool name — write-heavy tasks ask only once). `[agent] approval` defaults to `ask`; set `auto` for fully automatic, zero interruption (equivalent to pre-M7.3 behavior); config changes take effect on the next task. `^C` while awaiting approval counts as denial and terminates the whole task; every question and decision is traced in the session log (`approval` records). See the [work plan](docs/工作计划.md).

M7.6 (context compaction) implemented and verified on Linux (design: [agent execution flow design §10.3](docs/agent执行流程设计.md)): when the context approaches 85% of the model window (window unconfigured → 144k token estimate fallback) or the provider returns an overflow error, the engine auto-compacts at a turn boundary — the complete turns before the tail-retention line (≤25% of the window) are handed to the current model for a single summary call with tools off, producing a seven-part handoff skeleton (goal quoted verbatim / constraints & preferences / progress / key decisions / next steps / key context / notes), plus a mechanically rendered file list (read / modified) and a live-state snapshot (todo / background jobs / running subtasks) composed into a checkpoint; iterative compaction hands the old checkpoint to the summarizer to merge — no re-summarizing the raw history. Post-compaction request view = first user message verbatim + checkpoint + recent turns after the cut point verbatim (the cut point falls only on whole-turn boundaries, never splitting a tool-call/result pair); session history and on-disk logs are never modified (disk only appends a `compact` event; playback can reconstruct). Compaction can only retrigger after dropping below 50% (hysteresis debounce); an over-short summary is treated as degenerate — one retry, still failing then bail, this round continues with L1 folding only; compaction never kills a task; provider overflow errors compact first and retry once, only then reporting. Screen shows `─── compacting ───` during compaction, then `[已压缩上下文: 保留近 N 轮]` on completion.

M7.7 (subagents and manager model) implemented and verified on Linux (design: [agent execution flow design §16](docs/agent执行流程设计.md)): AI mode is defined as a **manager model** — the manager of a top-level task treats context as the scarcest resource and hoards it, and delegates all heavy work to worker subagents via the `task` tool (in-process engine loops with independent context, reusing the full guard suite: runaway-loop / read-idling / forced wrap-up) except for trivial or self-related tasks. Subtask streaming shows on screen with a `[task N]` prefix in real time and is logged to the session log (`s*` events), but only into the log — it never flows back into the manager's context; the manager polls compact status via `task_output` (step count / elapsed / last action / todo, with an anti-tight-poll hint after 15s of no progress), and on completion takes the 8KB-bounded report for acceptance and summary; a runaway subtask is stopped immediately with `task_kill` (concurrency cap 3; reports from guard-forced wrap-ups carry a "forced wrap-up" marker — acceptance must be extra strict). A task cannot end while its own subtasks are running: if the model wants to wrap up, the engine reminds once first (the engine does not re-wake the model on subtask completion); if it still wants to end, the engine waits for all subtasks to finish, feeds final status and reports back to the model for the acceptance summary; guard-forced wrap-up turns work the same way — reports first, then wrap up. Self-awareness: at 50%/75% of the model window (unconfigured → 96k/144k token estimates, plus a message-count ≥150 criterion) two-level reminders are injected, prompting the manager to inspect bloat and prefer delegation. `^C` gains three-level semantics: awaiting approval → deny and cancel the task; command execution → interrupt only the current command, task continues; subtask running → stop all subtasks first, the manager continues with partial reports, a second press terminates the task. Subtasks do not nest (depth 1) and all stop when rysh exits.

v6 (single-process multi-session) implemented and verified on Linux: one `rysh` run can hold several AI sessions in sequence — each session is one disk stream at `~/.rysh/sessions/<id>/messages.log` (history never overwritten), with global ledgers `~/.rysh/created` (number → session) and `~/.rysh/updated` (last-used order) recording the roster; switching sessions = terminating the current login shell (together with all its descendant processes, including the real shell started via the launcher stub on Windows) and restarting the login shell on the same pty, printing a separator line on screen followed by the session's tail replay (no clear; see the next entry); draft / AI history / event ring are per-session and isolated, never cross-wired. Session operations are reachable by three routes: AI-mode slash commands `/ls` `/new` `/resume`; `rysh ls/new/resume/kill` subcommands in a shell or any terminal (when run inside rysh they are one-shot subprocesses; `new`/`resume` make the current instance switch via the control channel — no second nested pty); and top-level launch forms `rysh new` / `rysh resume <ref>` / `rysh ai` for direct entry. There is also `rysh ai "<message>"` one-shot Q&A, and sessions attached by another rysh process refuse switching (`rysh kill` terminates the attacher). See the "Multi-session" section and the [work plan](docs/工作计划.md).

The main-screen design (approved 2026-08-29, design: [main-screen design](docs/主屏方案.md)) implemented and verified by integration tests under Windows ConPTY: rysh stays on the terminal's **main screen** the whole time — startup prints a two-line dim banner (`rysh · <model> · session <id>` + a mode-switch key hint line `'<mode_switch key> (or leading space) enters AI mode · exit leaves rysh'`, key name follows the `mode_switch` setting), no alternate screen, no scroll-region cropping (DECSTBM), no bottom status line; all shell and AI output lands in the terminal's native scrollback, `Shift+PgUp` / mouse wheel scroll back directly; mode switching still only swaps the prompt line in place, but the AI prompt line becomes the model's only landing spot (`cwd · model · [AI]:`, the default template gains a `\m` escape); streaming wait now shows `⠋ thinking...` / `⠋ executing...` in place on the **last line** of content (one blank line above and below the spinner); from startup to the login shell's first output it is likewise `⠋ starting...` (replaced the moment first output arrives, cursor display restored with it); neither exit nor session switching clears the screen anymore — history stays in the terminal as-is; during full-screen programs (vim, etc.) recording still pauses, but on exit it no longer re-enters the alternate screen or rebuilds the UI — passthrough only; `[tui] cursor_style` is still parsed but no longer sent as DECSCUSR (no status line left to link). The resident identity is carried by the prompt marker (on by default: terminal title takeover + bash/zsh prompt `(rysh)` prefix; see "Prompt marker (resident identity)" below; `[tui] prompt_marker = "off"` restores pure passthrough).

Build:

```
go build ./cmd/rysh
```

or via the Makefile (cross-compile, tests, format checks, etc.):

```
make build      # -> bin/rysh
make cross      # -> 8 platform static binaries + checksums.txt under dist/
make test       # full test suite
```

## Installation

**macOS / Linux — prebuilt binary** (recommended, no Go needed):

```
curl -fsSL https://github.com/frybox/ruyishell/releases/latest/download/install.sh | bash
```

`install.sh` ships in every release; `releases/latest/download/...` always redirects to the newest release, so this line stays valid forever. The script downloads the matching `rysh-<os>-<arch>.tar.gz` / `.zip` from the same release and verifies SHA256 automatically. Installs to `~/.local/bin` (override with `PREFIX=/usr/local`).

**macOS / Linux — from source** (requires Go 1.25+):

```
./scripts/install.sh --source        # installs to ~/.local/bin
PREFIX=/usr/local ./scripts/install.sh --source
```

or directly `go install ./cmd/rysh`.

**Windows**: download `rysh-windows-amd64.zip` from the Release (pick by CPU architecture), unzip and put `rysh.exe` in any directory on your `PATH`.

`rysh -v` / `rysh --version` show the version. Nested interactive use is not supported: running bare `rysh` again inside rysh's shell is refused (the outer rysh exports an `RYSH_INSIDE` marker, the inner one refuses before creating its pty); one-shot forms such as `rysh ai/new/resume/ls/kill/-v` work fine (see "Multi-session").

Test:

```
go test ./...
```

## Configuration

Provider/model configuration lives in `~/.rysh/config.toml` (`$RYSH_CONFIG` can override the path), structured like pi's `models.json`: under the `providers` table each provider carries `base_url` / `api` / `api_key` / `models[]`. Example:

```toml
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"
models = [
    { id = "llama3.1:8b", name = "Llama 3.1 8B" },
    { id = "qwen2.5-coder:7b" }
]

[providers.mycloud]
base_url = "https://api.example.com/v1"
api = "openai-completions"
api_key = "$MY_API_KEY" # env-var reference, not stored in plaintext
```

The `api` field selects the wire protocol the client speaks to that provider (a per-model `api` overrides the provider's value). Three are supported:

- `openai-completions` (default) — OpenAI-compatible chat completions, `POST {base_url}/chat/completions`. This is the shape most OpenAI-compatible endpoints (OpenAI, DeepSeek, Qwen, Kimi, GLM, Grok, Mistral, Ollama, local llama-server, …) expose; no `api` value is needed to use them.
- `openai-responses` — the OpenAI Responses API, `POST {base_url}/responses`. Set `base_url` to the API root (e.g. `https://api.openai.com/v1`); `max_tokens` (the model's `max_tokens`) is sent as `max_output_tokens`.
- `anthropic-messages` — the Anthropic Messages API, `POST {base_url}/v1/messages` with `anthropic-version` and the key sent as `x-api-key`. Set `base_url` to the API root (e.g. `https://api.anthropic.com`). The Messages API requires a `max_tokens` cap: it is taken from the model's `max_tokens`; if unset, ruyishell defaults to `4096`.

Models are referenced as `provider/model` (e.g. `ollama/qwen2.5-coder:7b`). Keys may reference environment variables via `$VAR` / `${VAR}` (`$$` escapes a literal `$`). **Every `/model` re-reads the config file directly**: after editing `config.toml` and saving, no ruyishell restart is needed — the next `/model` picks it up (following pi's `/model` usage). `/model` with no argument lists all available models as a numbered list (the current model's line starts with `>`, consistent with the session list); `/model <number>` or `/model <provider/model>` switches. A model may set `tools = false` to disable native function calling (on by default). When off, that model answers in plain text without executing any tool; if the server returns 400 for the `tools` parameter, the model is treated as not supporting function calling and the AI task fails with a clear error — there is no fallback that executes markdown code blocks (a model without tool support is a question-answering model, not an agent brain).

### Default shell

`shell = "auto"` (default) probes the platform default shell: Linux/macOS takes `$SHELL -l` (login shell, reads `.profile`); Windows probes `pwsh` → `powershell` → `cmd`. An explicit path may also be given:

```toml
shell = "/bin/zsh"
```

This value takes effect **once at startup** (the shell process is created only once); later edits require a ruyishell restart.

### Key bindings

`[keys] mode_switch` changes the shell/AI mode-switch key (default `shift-tab`, i.e. CSI Z). Many terminals (e.g. Windows Terminal) use Ctrl+Tab for tab switching, so the default is Shift+Tab (matching the mode-switch convention of Claude Code / Codex); if your terminal claims Shift+Tab or forwards it unreliably, switch back to `ctrl-tab`, or use `ctrl-space` / `ctrl-backslash`:

```toml
[keys]
mode_switch = "ctrl-space"
```

This value takes effect **at startup** (the input reader is created only once). All Shift+Tab references in the tables below mean whatever key `mode_switch` currently is.

### Prompt marker (resident identity)

Under the main-screen design the shell prompt is drawn by the shell itself; "you are inside rysh" is signaled by a resident marker. `[tui] prompt_marker` defaults to `auto` (on), in two layers:

- **Terminal title** (all shells): during the run the title is always `rysh · <model> · session <id>` — rysh filters out `OSC 0/2` title sequences the child shell itself emits (purely out-of-band; screen and other passthrough bytes are unaffected), updating after `/model` and session switches; on exit it best-effort restores the title read at startup (`OSC 1046` probe; if the terminal doesn't support it, nothing happens; on Windows console it is set but never restored).
- **bash prompt prefix** (bash child shells only): rysh exports a one-liner `PROMPT_COMMAND` into the child shell's environment — bash (login or not) imports `PROMPT_COMMAND` from the environment and runs it before every prompt; that line idempotently prepends a dim `(rysh) ` to PS1 (wrapped in PS1 non-printing markers, so readline cursor math is unaffected). No user config files are modified; startup effect: `(rysh) (base) user@host:~$`.
  - **Best-effort boundary**: if the user's own rc (starship / direnv / nvm, etc.) assigns `PROMPT_COMMAND` itself, the user's value wins and the marker quietly disappears — the user's prompt is never broken. If you want the prefix in that case, configure it in your own rc the `RYSH_INSIDE` way below.
- **zsh prompt prefix** (zsh child shells only): zsh has no rcfile argument and no `PROMPT_COMMAND`; the only environment injection point is `ZDOTDIR` — rysh points the child process's `ZDOTDIR` at a private temp directory containing a single `.zshenv`: that file registers a precmd hook (idempotently prepending a dim `(rysh) ` to the prompt, wrapped in `%{...%}` non-printing markers, cursor math unaffected), then points `ZDOTDIR` back at the user's real dotdir (outer `ZDOTDIR`, or `$HOME` if absent) and runs the user's real `.zshenv` on their behalf (redirection would otherwise shadow it); the user's `.zprofile` / `.zshrc` / `.zlogin` / completion caches parse as usual, no config files are modified, and the temp directory is deleted on rysh exit. The hook is registered before the user's rc, so if the user's own precmd hooks (starship / powerlevel10k, etc.) rewrite the whole prompt, the user wins and the marker quietly disappears.
- **fish / other shells**: no reliable injection point, only the title layer; for a prompt prefix, conditionally modify your own rc on `RYSH_INSIDE` (rysh exports `RYSH_INSIDE=1` when starting the child shell; a bare terminal has no such variable):

  ```bash
  if [ -n "${RYSH_INSIDE:-}" ]; then
    PS1="(rysh) $PS1"
  fi
  ```

Set to `"off"` to restore the original zero-intrusion behavior: no title takeover, `OSC 0/2` passed through as-is, no `PROMPT_COMMAND` injection, no `ZDOTDIR` redirection:

```toml
[tui]
prompt_marker = "off"
```

### TUI preferences

`[tui] cursor_style` used to set the terminal cursor shape in shell mode (DECSCUSR): after the main-screen design removed the bottom status line, rysh no longer rewrites the terminal cursor — the key **is still parsed but has no effect anymore**:

```toml
[tui]
cursor_style = "bar"     # deprecated: rysh no longer sends DECSCUSR
# cursor_style = "block"
# cursor_style = "default"
```

The key is kept only so the documented configuration surface stays unchanged: unknown values have always been, and still are, silently ignored.

### AI prompt line

`[ai] prompt` sets the AI-mode input prompt line (bash-PS style; affects only the prompt line rysh itself draws, not the shell's PS). Escapes: `\u` user, `\h` hostname, `\w` current directory (HOME shown as `~`), `\m` current model (e.g. `hy3/hy3`), `\t` time (HH:MM:SS), `\\` literal backslash, `\[...\]` non-printing region (takes no width), `\x1b` / `\e` escape byte (enables SGR color sequences). The default is a dim-magenta current directory + dim `· <model>` + magenta `[AI]:` marker — the trailing colon reads like a natural-language input box rather than a shell prompt:

```toml
[ai]
prompt = "\\[\\x1b[1;35m\\]\\w\\[\\x1b[0m\\] \\x1b[2m· \\m\\x1b[0m \\x1b[35m[AI]:\\x1b[0m "
```

With no model configured, the default template drops the `· <model>` segment (no dangling separator). Under the main screen there is no resident status line, so this prompt line is the only place rysh shows the current model, updated on the next draw after a `/model` switch. **Note**: in TOML strings, backslashes must be doubled; otherwise `\u` (a unicode escape) and the like will fail to parse.

This value takes effect **every time AI mode is entered** (config re-read on mode switch).

### Session log

Each session corresponds to one disk stream at `~/.rysh/sessions/<id>/messages.log` (append-only JSONL; event types: `sys` session/mode/task events, `shl` shell output lines, `shk` shell commands, `usr` AI questions, `rea`/`asw` reasoning/reply, `tool` tool calls, `noti` notifications, subagent stream `srea`/`sasw`/`stool`/`snoti`/`sub` (M7.7, tagged `[task N]`, log-only — not re-flown into context on history rebuild); single files auto-rotate past ~1MB, two generations kept: `messages.log.1.gz` / `messages.log.1`). Since v6 one run can hold several sessions in sequence (`/new`/`/resume`/`rysh resume` to switch, see "Multi-session"): switching neither deletes nor modifies any session's log; global ledgers `~/.rysh/created` (`<time> <number> <id>`) and `~/.rysh/updated` (`<time> <id>`) record the session roster and last-used order (the numbers shown by `/ls` and `rysh ls` come from `created`). `$RYSH_SESSION_ID` can override the session id (for deterministic scenarios such as tests). The log is the session's source of truth on disk; the screen is only its live window.

### Agent context

`[agent] env_allowlist` configures the shell environment variables exposed to the model (prepended as an `env:` system message on every question). By default the built-in allowlist is used (`HOME` / `USER` / `SHELL` / `TERM` / `LANG` / `EDITOR` / `PATH` and other common entries); once explicitly configured, it **exactly replaces** the default list — variables outside the allowlist (keys, tokens) do not enter the model context:

```toml
[agent]
env_allowlist = ["HOME", "USER", "PATH", "MY_PROJECT_VAR"]
# env_allowlist = []   # empty array = expose no environment variables at all
```

This value takes effect **on every question** (config re-read per request, same as `/model`). Note: environment reading goes through `/proc/<pid>/environ` and can only see the shell's **startup-time** environment; variables `export`ed within the session are not in it (would need shell hooks / OSC reporting, beyond current scope).

### Permission approval

`[agent] approval` controls the permission gate for AI task tool execution, with three values: `"ask"` (default) asks per write operation and per command not listed in the safety classifier; `"always"` is fully automatic, zero interruption (equivalent to ungated behavior); `"never"` refuses every gated operation without asking (the refusal text is fed back to the model, which routes around it; read-only work still runs free):

```toml
[agent]
approval = "ask"   # or "always" / "never"
```

The same values can be applied per session without touching the file: start rysh with `--always-approve` or `--never-approve` (the flag wins over `[agent] approval` for that instance's whole life), or in AI mode use `/approve` — bare `/approve` queries the current mode, and `/approve ask|always|never` sets it (session-level, effective from the next task).

This value takes effect **at the start of every task** (config changes apply to the next task, no restart needed). In ask mode, read-only tools and classifier-passing bash go straight through; everything else pops `? 运行 <命令>（y 是 / n 否 / a 总是）` — `y` allows this one, `n` denies (denial text fed back to the model, the task continues), `a` allows and records a session-level rule (bash by command prefix / file tools by tool name; rules live in memory only, cleared on restart). After answering y/a the command actually runs, and below the bar `⠋ 执行中...` is shown (until the `[tool]` result line lands) — a slow execution keeps it spinning; it is not an unresponsive key.

## Keys (M2 / M3 part)

| Key | Behavior |
| --- | --- |
| `Shift+Tab` | Bidirectional shell / AI mode switch. Shell and AI share the same input line: entering AI carries the shell's unsubmitted command into the AI input line (cleared on the shell side); on exit the AI draft is injected back into the shell command line (without Enter — Enter then executes); submitting or clearing on either side consumes that line. If the draft starts with `!` (a shell command mistyped into AI mode, see below), the switch strips the leading `!` before re-injecting |
| `Space` (line start) | Bidirectional shell / AI mode switch (the leading space is swallowed and never reaches the shell) |
| `Esc` | Clears the draft in AI mode (stay in AI; no longer returns to the shell) |
| `←` / `→` / `Home` / `End` / `Backspace` | Move editing cursor / delete in AI mode |
| `↑` / `↓` (`Ctrl+P` / `Ctrl+N`) | Rotate submitted user inputs in AI mode: from the most recent, select the previous/next in turn and backfill the draft (editable before re-submitting); continuing to edit leaves the rotation; passing the most recent returns to the current draft |
| `Ctrl+Z` | Undo the most recent draft edit in AI mode (insert / delete / clear), repeatable |
| `Enter` (draft starts with `/`) | Submit as a slash command (`/model` `/approve` `/new` `/ls` `/history` `/resume` `/help` `/quit`, see "Multi-session"); draft cleared, stay in AI mode |
| `Enter` (natural-language draft) | Submit as a prompt to the current model; the reply renders streaming on the shared stream; stay in AI mode on completion to follow up |
| `Enter` (draft starts with `!`) | Not sent to the model — this is a shell command mistyped into AI mode: the draft is kept and two exits are offered (press the mode-switch key to go to the shell with the input preserved, executing after stripping the leading `!`; or clear the draft, then switch to the shell with a leading space and retype) |
| `Enter` (empty draft) | no-op |
| `Ctrl+C` (task streaming) | Cancel the current task (the only effective key while streaming; mode switching is locked) |
| `Tab` | First-word completion in AI mode: `/` completes slash commands (multiple matches: list first, then silently cycle); after `/model ` or `/resume ` a numbered list (model list / session list) is shown so you can type the number directly; after the config file changes on disk (hot reload), pressing Tab on the same draft re-lists |

AI and shell share the same screen and the same stream: rysh stays on the terminal's main screen the whole time (no alternate screen, no scroll-region cropping); entering AI mode neither clears nor rebuilds the shell view — only the prompt line is swapped in place; shell and AI share the same input line, and text and cursor persist with the switch (a half-typed shell command goes into the AI draft, and the AI draft is injected back into the shell command line on the way back — see the key table above). A `!`-prefixed command mistyped into AI mode is neither executed nor sent to the model: on submit a hint is shown, and the mode-switch key takes you back to the shell with that command (leading `!` stripped) ready to run. All output lands in the terminal's native scrollback — `Shift+PgUp` / mouse wheel scroll back at any time, and content stays in the terminal after rysh exits. There is no resident status line: the current model is written on the AI prompt line (`cwd · model · [AI]:`); `/model` lists all available models as a numbered list (the current model's line starts with `>`, consistent with the session list), and `/model <number>` or `/model <provider/model>` switches immediately, updating on the next draw; streaming wait states hang on the **last line** of content (one blank line above and below; waiting on model tokens shows `⠋ thinking...`, tool execution shows `⠋ executing...` — likewise between answering y/a on the approval bar and the `[tool]` result line landing); the first token replaces it with the body. During busy periods (startup spinner, from thinking spinner to streaming body) the terminal cursor is hidden, restored when the task lands on a fresh prompt — the spinner and the streaming body are each the dynamic focus; the cursor need not mark focus or input position. In AI mode, pty output is not forwarded to the screen (buffered + logged to the session log, flushed in one batch on return to the shell); in shell mode, while a full-screen program (vim, etc.) runs, rysh pauses recording, and on exit resumes the stream on the main screen as-is, with no UI rebuild. Typing `exit` in the shell (or the child shell exiting) makes rysh do a light reset only (colors and cursor visibility), no clear — the terminal, complete with its history, is handed back to the outer shell.

## AI Conversation (M3 / M4 session context)

In AI mode, type natural language and press Enter, and it is sent to the current model (OpenAI-compatible `chat/completions` SSE streaming interface); the reply renders streaming on the shared stream: first a gray heading `─── <model> ───`, then reasoning content (`reasoning_content`) appended token by token in dim, and the reply body rendered as markdown (headings, bold/italic, inline code, links, lists, blockquotes, fenced code blocks all styled; unclosed markers do not flash during chunked streaming); tool calls appear as gray `[tool] $ <command> (exit <N>)` lines. `Ctrl+C` during generation cancels immediately (mode switching is locked while streaming, only `^C` works): the request is aborted and input is restored (you can follow up). After the reply, the draft is empty again for follow-up questions; after a `/model` switch, the conversation continues with the new model. Multi-turn conversation carries its own memory: every question sends the prior conversation history along (cap 120 messages, oldest whole turns trimmed), memory is per-session — `/new` creates and switches to a brand-new session (empty history, see "Multi-session"), `/resume` returns to an old session and its memory comes back; no switch clears or overwrites disk logs.

Every request also sends the current working directory (cwd) and shell environment as `system` messages (on Linux read via `/proc/<child>/cwd` and `/proc/<child>/environ`; non-Linux degrades gracefully to not sending), so AI mode perceives where you `cd`'d to in shell mode and common environment variables. Only allowlisted environment is exposed (`HOME`/`USER`/`SHELL`/`TERM`/`LANG`/`EDITOR`/`PATH`, etc.); secret-bearing variables (API keys, tokens) never enter the model context; `/proc` can only read the environment from shell startup — `export`s made within the session are not in it.

Commands and output recently executed in shell mode also enter the AI context (M4 unified event stream): rysh records each command (command text + following terminal output + cwd at execution), and on every question these events are **interleaved by occurrence time** with the AI conversation history into one timeline — each event is an independent `system` message (`cwd: <dir>\n$ <cmd>\n<output>\n---`), so a command run between two AI rounds lands between the two rounds, and ordering is not lost (cap 20 events, 4KB per output, 8KB total for the event section, ANSI sequences stripped). Thus after switching to AI mode you can directly ask "what does xxx in the output of my last command mean".

### Agent tool calls (M4)

When the model returns a native `bash` function call, rysh runs that command with a standalone executor at the shell's current cwd (non-interactive, stdin closed), captures stdout/stderr/exit_code, and shows an observation line `[tool] $ <command> (exit <code>)` on screen; the command's output is automatically fed back to the model as a `role:"tool"` message and invoked again, until the model gives a final answer (the same task shares one `─── <model> ───` heading). Only native function calls are executed: code blocks in the reply body (including ` ```bash `) are display-only and are never run, so a model that merely shows a command for the user to copy cannot trigger execution. Tool rounds are **unbounded**; runaway prevention is the loop guard's job: a consecutive run of ≥6 repeats of the same signature (full command + exit code + hash of the first 8KB of output), or a >60% share in the saturated sliding window (20 entries), is judged runaway — the screen shows a dim `─── wrapping up (loop guard) ───` and a wrap-up instruction is injected, forcing the model to produce a text summary from what it already has; from the 2nd consecutive repeat a "change your approach" warning is appended to the fed-back result's tail to prompt self-correction. On the context side, bloat is prevented: at every request assembly, tool output older than the 64KB tail budget is collapsed into a one-line `[已省略] $ …已折叠` placeholder (the newest results and in-budget old results keep their original text), keeping long-task request size bounded while the history does not forget. A single-point runaway fallback also exists: a 32KB cap per stream (truncated), a 30s timeout per command that kills the whole process group.

> Current limitations: command execution uses standalone `bash -c` subprocesses rather than a shared pty (inheriting the login shell environment / interactive execution is future work); the approximation of shell command recording: it records the bytes the user actually typed (the final command from readline history recall/completion is invisible), output includes the following prompt, and shell-mode command exit codes are not captured (tool calls do capture them); while a full-screen program (occupying the alternate screen, e.g. vim) runs, recording pauses — its keystrokes and screen output do not enter the event stream, the redraw at the exit instant belongs to no command, and recording resumes after returning to the shell.

## Multi-session (v6)

`rysh`'s single process runs only one pty, but can hold multiple AI sessions: each session is one disk stream under `~/.rysh/sessions/<id>/`; `/ls` numbers come from the global ledger `~/.rysh/created` (an unnamed new session shows `新会话`). Switching sessions = terminating the current login shell (together with all its descendant processes) → restarting the login shell on the same pty (profile re-played) → printing a separator line followed by that session's tail replay (no clear, no full redraw; see the "main-screen design" entry above) — per-session state such as draft, AI history and the shell event ring is isolated and reset; disk logs are never overwritten.

Three equivalent routes for session operations:

| Entry | Command | Notes |
| --- | --- | --- |
| AI slash commands | `/ls [page]` · `/new` · `/resume <number or id>` | Paged list (`>` marks the current; a `[rysh <pid>]` suffix means it is attached by another rysh process) / create and switch / switch |
| Shell or any terminal | `rysh ls [page]` · `rysh new` · `rysh resume <number or id>` · `rysh kill <number or id>` | When run inside rysh they are one-shot subprocesses (no nested pty); `new`/`resume` make the current instance perform the switch via the control channel |
| Top-level launch forms | `rysh new` · `rysh resume <ref>` · `rysh ai` | Enter directly with a new/specified session, or start straight into AI mode |

Other behaviors:

- **Attachment exclusivity**: a session can be attached by at most one rysh instance at a time; an attached session refuses switching and suggests `rysh kill` (SIGTERM to the attacher → 5s → SIGKILL).
- **One-shot Q&A**: `rysh ai "<message>"` loads the history of the session pointed to by `$RYSH_SESSION_ID`, sends one message, streams the reply, then exits.
- In AI mode `/help` lists all slash commands; `/quit`, `/exit` leave rysh; switch requests received during task streaming land after the task wraps up.

## License

MIT — see [LICENSE](LICENSE).
