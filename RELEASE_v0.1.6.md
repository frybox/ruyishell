# ruyishell v0.1.6

Session-switch fix, three-state approval, llama.cpp compatibility & Windows ConPTY improvements

---

## 🚀 New Features

- **Three-state permission approval**: `approval = "ask"` (default, reads free, writes ask) / `"always"` / `"never"`, with `/approve` command and `--always-approve`/`--never-approve` startup flags
- **`/history <number>`**: recall a previous user input to the current edit line from shell history
- **llama.cpp context overflow auto-adaptation**: recognize llama.cpp's "exceeds the available context size" error, parse the server's actual `n_ctx` and adjust the window dynamically, no hardcoded limit needed
- **Shell events get their own role**: shell events become an independent `events` role; tool calls are displayed paired with their results
- **HumanDuration smart display**: render duration in its largest non-zero unit(s), no trailing zero segments

## 🐛 Fixes

- **Session switch no longer leaks old shell events**: reset the shell event ring and half-typed line on session switch, preventing old-session context from bleeding into the new session
- **PowerShell input line preserved after mode switch**: keep the PowerShell prompt row intact when switching between shell and AI mode
- **Windows ConPTY window size polling**: actively poll console size on Windows so ConPTY keeps up with resize events
- **Pty slave cross-platform refactor + macOS reconnect**: split reopen logic by platform (unix/windows); on macOS the revoked pty slave can be reopened after a session leader is killed
- **Guarantee reply stream ends on a fresh line**
- **Bash OSC 7 cwd marker now actually emits**
- **`cd` in shell mode syncs rysh cwd**
- **Windows terminal color detection no longer discards all colors**
- **Blank line between thinking and answer on phase switch**

## 📚 Documentation

- `docs/roadmap.md`: task provenance and model A/B evaluation planning
- `README` adds error-in-shell scenario description (both English and Chinese)
- Real-time thread inline design document

## 🔧 Build Fix

- Replace `syscall.Dup2` with `unix.Dup2` in `cmd/rysh/ptyslave_unix.go` to fix cross-compilation on `linux/arm64` (Go 1.25+ deprecated `syscall.Dup2`)

---

**Full changelog**: [v0.1.5...v0.1.6](https://github.com/frybox/ruyishell/compare/v0.1.5...v0.1.6)