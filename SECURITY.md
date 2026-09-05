# Security Policy

## Supported Versions

All versions from the current `main` branch forward. Pre-release versions
(`v0.x`) get security fixes when feasible, but no guaranteed support window.

## Reporting a Vulnerability

**Do not report security vulnerabilities via public issues.**

Use GitHub's private vulnerability disclosure:
[Report a vulnerability](https://github.com/frybox/ruyishell/security/advisories/new)
(enabled under *Security → Security advisories*). You will get an advisory
number and we will respond within 7 days.

If the private channel is unavailable, email the maintainer at
`juzejian@gmail.com`.

## What Counts as a Vulnerability

`rysh` is an AI terminal that **executes shell commands by design**. The
security model is: the agent proposes, an approval gate decides, the user
is always in the loop. A vulnerability is anything that breaks that model,
for example:

- **Approval bypass** — a path that executes a command without the approval
  gate, or that mis-classifies a destructive command as safe (e.g. a
  `rm -rf` pattern slipping past the classifier).
- **Shell / PTY escape** — bytes from model output, tool results, or a
  child process reaching the terminal as control sequences (OSC/CSI
  injection), or corrupting the user's pty state (termios, PS1, cursor).
- **Path traversal in file tools** — `read`/`write`/`edit`/`grep` escaping
  the working directory or following symlinks to write outside it when the
  user did not intend that.
- **Session/session-file leakage** — secrets in `~/.rysh` session logs or
  config being written world-readable, or echoed back into model context
  unintentionally.
- **Prompt injection escalating to action** — content from file reads,
  command output, or web text steering the agent into an action the
  approval gate should have blocked but does not.

## What Is Not a Vulnerability

- The model itself saying something wrong or a model hallucinating — that is
  a model-quality issue, not a shell issue.
- A user approving a command they should not have approved. The approval
  gate shows the exact command; approving it is the user's decision.
- Risks inherent to running any agent with a shell and giving it your
  `PATH` and your home directory.

## When Fixing

Fixes are backported to the latest release tag; a patched release is cut
within 7 days of a confirmed critical issue. We will publish a GitHub
Security Advisory with a CVE request for anything with real exposure.
