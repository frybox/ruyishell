package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aymanbagabas/go-pty"
	"golang.org/x/term"

	"ruyishell/internal/agent"
	"ruyishell/internal/aiui"
	"ruyishell/internal/config"
	"ruyishell/internal/cwd"
	"ruyishell/internal/i18n"
	"ruyishell/internal/keys"
	"ruyishell/internal/markdown"
	"ruyishell/internal/mode"
	"ruyishell/internal/provider"
	"ruyishell/internal/screen"
	"ruyishell/internal/session"
	"ruyishell/internal/shell"
	"ruyishell/internal/theme"
)

// spinnerFrames is the braille spinner cycled inline on the last line of the
// stream while a task waits for its first token (one frame per 250ms tick).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// uiT is the active UI translator, set from [tui] locale (plus the LANG
// environment) once the config loads; subcommands set it from the
// environment alone before dispatching.
var uiT i18n.T

// spinnerElapsed formats the spinner's elapsed wait grok-style: whole
// seconds below a minute (1s, 2s), then minutes and seconds (1m56s).
func spinnerElapsed(d time.Duration) string {
	s := int(d / time.Second)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%ds", s/60, s%60)
}

// maxPending bounds how much shell output is buffered while AI mode defers
// it; beyond this the oldest bytes are dropped (keep-head) and the overflow
// is reported when the buffer is flushed back to the screen.
const maxPending = 1 << 20

// maxLineLog bounds a partial (newline-less) line buffered for session-log
// flushing, so a pathological output cannot grow it without limit.
const maxLineLog = 64 << 10

// quitFlag signals that rysh should exit (used by /quit and /exit commands).
var quitFlag atomic.Bool

// insideEnv is injected into the environment of rysh's child shell (see
// childEnv), so a rysh started from within rysh can recognize the nesting
// and refuse interactive mode before it would create a pty.
const insideEnv = "RYSH_INSIDE"

// sessionEnv names the active session: exported to the child shell so rysh
// subprocesses (rysh ai / rysh ls) read or write the same session this
// instance owns, and honored at startup for deterministic tests.
const sessionEnv = "RYSH_SESSION_ID"

// ctlEnv names this interactive rysh's control channel file: exported to
// the child shell so a rysh subprocess (rysh new / rysh resume) can append a
// switch request that this instance polls and acts on.
const ctlEnv = "RYSH_CTL"

// pidEnv names the interactive rysh process's pid: exported to the child
// shell so a rysh subprocess can tell its own attachment record (the one
// this instance owns) apart from another live rysh's when scanning
// ~/.rysh/ctl/*.sess. Without it, rysh ls would mark the current session
// as attached by "another" rysh and rysh resume <current> would be refused.
const pidEnv = "RYSH_PID"

// sessionPageSize is how many sessions one /ls or rysh ls page shows.
const sessionPageSize = 10

// switchReq is a deferred session switch: the target meta plus the notice
// printed when the switch lands. It is produced by /new and /resume and by the
// control-channel poll, and executed by switchToMeta.
type switchReq struct {
	meta   *session.Meta
	notice string
}

// pendingSwitchT is a session switch deferred because a task was streaming;
// the mainloop executes it (via switchToMeta) once finalize settles the
// stream. landInAI records whether the request originated from AI mode, so
// the landing mode matches the initiating context.
type pendingSwitchT struct {
	req      *switchReq
	landInAI bool
}

func run(targetID string, startInAI bool) int {
	// M3: provider/model config. The default model ref is named on the AI
	// prompt line and switched with /model. A malformed config only warns
	// to stderr; the shell stays usable and /model surfaces the error
	// again. Loaded first because the shell override below is applied once
	// at startup (the shell process is spawned once).
	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rysh: config path: %v\n", err)
		cfgPath = "~/.rysh/config.toml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rysh: read config %s: %v\n", cfgPath, err)
		cfg = &config.Config{}
	}
	// UI language: [tui] locale wins, then an en* LANG, else Chinese.
	uiT = i18n.New(i18n.Resolve(cfg.TUI.Locale, os.Getenv("LANG")))
	// Prompt marker (主屏方案 §3.2 修订): while rysh runs it owns the
	// terminal title and, for a bash child, prepends a dim "(rysh)" tag to
	// the prompt via an exported PROMPT_COMMAND one-liner. [tui]
	// prompt_marker = "off" restores the original pure passthrough (no
	// title, no injection). Read once at
	// startup; prevTitle holds the terminal's prior window title for
	// restore on exit ("" when the probe could not read it).
	markerOn := cfg.TUI.PromptMarkerEnabled()
	var prevTitle string

	p, err := pty.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rysh: open pty: %v\n", err)
		return 1
	}
	defer p.Close()

	// Resolve the session this instance starts attached to. An explicit
	// target (rysh new / rysh resume on the command line) wins; otherwise
	// RYSH_SESSION_ID (deterministic tests) is honored; otherwise the most
	// recently updated session on disk that no live rysh holds; otherwise a
	// brand-new session. Without a home directory there is no persistence,
	// so the session stays in-memory with a nil log.
	store := sessionStore()
	var activeMeta *session.Meta
	if targetID == "" {
		targetID = os.Getenv(sessionEnv)
	}
	switch {
	case targetID != "" && store != nil:
		m, err := store.EnsureSession(targetID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rysh: %v\n", err)
			return 1
		}
		activeMeta = m
	case targetID != "":
		activeMeta = &session.Meta{ID: targetID, Created: time.Now().UnixMilli()}
	case store != nil:
		m, err := selectActiveSession(store, os.Getpid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "rysh: %v\n", err)
			return 1
		}
		activeMeta = m
	default:
		activeMeta = &session.Meta{ID: session.NewID(), Created: time.Now().UnixMilli()}
	}

	// Per-instance control channel (<pid>.cmd, polled for switch requests
	// from rysh subprocesses) and attachment record (<pid>.sess, naming the
	// session this instance owns, for rysh ls / rysh kill / switch refusal).
	// Both are removed when this instance exits.
	ctlPath := ""
	attachPath := ""
	if store != nil {
		ctlDir := store.CtlDir()
		if err := os.MkdirAll(ctlDir, 0o755); err == nil {
			pid := os.Getpid()
			ctlPath = filepath.Join(ctlDir, fmt.Sprintf("%d.cmd", pid))
			attachPath = filepath.Join(ctlDir, fmt.Sprintf("%d.sess", pid))
			_ = os.WriteFile(ctlPath, nil, 0o644)
			writeSessionAttach(attachPath, activeMeta.ID)
			defer func() {
				_ = os.Remove(ctlPath)
				_ = os.Remove(attachPath)
			}()
		}
	}

	sh, args := shell.For(cfg.Shell)
	// Prompt marker, zsh half: point the child at a private ZDOTDIR whose
	// single .zshenv registers the tag hook (see prompt_marker.go). shellEnv
	// is shared by the first launch and every session-switch re-launch, so
	// the wrapper outlives a /new / /resume shell restart; the directory is
	// removed only at exit.
	var zshMarkerEnv []string
	if markerOn && strings.EqualFold(filepath.Base(sh), "zsh") {
		var zshMarkerCleanup func()
		zshMarkerEnv, zshMarkerCleanup = setupZshMarker()
		if zshMarkerEnv != nil {
			defer zshMarkerCleanup()
		}
	}
	// Prompt marker, PowerShell half: run a temp .ps1 shim that loads the
	// user's profiles and wraps prompt to prepend the dim "(rysh)" tag (see
	// setupPwshMarker). The args outlive session-switch shell restarts
	// because shellEnv and the re-launch both reuse the same args slice;
	// the temp dir is removed only at exit.
	if markerOn && isPowershell(sh) {
		if pwshArgs, pwshCleanup := setupPwshMarker(); len(pwshArgs) > 0 {
			args = append(args, pwshArgs...)
			defer pwshCleanup()
		}
	}
	// Prompt marker, bash half: export the PROMPT_COMMAND one-liner that
	// prepends the dim "(rysh)" tag to the prompt (see prompt_marker.go —
	// a login bash has no after-rc hook of its own).
	shellEnv := func(sessionID string) []string {
		env := childEnv(sessionID, ctlPath)
		switch {
		case markerOn && strings.EqualFold(filepath.Base(sh), "bash"):
			env = append(env, "PROMPT_COMMAND="+promptMarkerCmd)
		case markerOn && isCmd(sh):
			// cmd expands $E to ESC inside PROMPT, so prepend a dim
			// "(rysh) " to the user's existing prompt (default "$P$G").
			// The wrapper never overrides it because we set it env-side.
			prompt := os.Getenv("PROMPT")
			if prompt == "" {
				prompt = "$P$G"
			}
			env = append(env, "PROMPT=$E[2m(rysh)$E[0m "+prompt)
		case len(zshMarkerEnv) > 0:
			// zsh honors the first of duplicate envp entries, so a ZDOTDIR
			// exported by the outer shell — or a nested rysh's wrapper
			// vars — must not precede the marker's own.
			kept := make([]string, 0, len(env))
			for _, kv := range env {
				if strings.HasPrefix(kv, "ZDOTDIR=") || strings.HasPrefix(kv, "RYSH_ZSH_SRC_DIR=") {
					continue
				}
				kept = append(kept, kv)
			}
			env = append(kept, zshMarkerEnv...)
		}
		return env
	}
	c := p.Command(sh, args...)
	c.Env = shellEnv(activeMeta.ID)
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "rysh: start %s: %v\n", sh, err)
		return 1
	}
	// The parent keeps its copy of the pty slave open for the whole run so
	// the same pty can host a fresh shell after a session switch; it is
	// closed just before exit (see the drain below), letting the master
	// reach EOF if the child is still alive.

	// Raw mode on our own terminal so keyboard input (Ctrl-C, arrows,
	// etc.) passes through to the pty as bytes instead of killing rysh.
	restores := rawTerminal()
	// Crash safety: if anything below panics, restore raw mode first (this
	// defer runs after restoreAll, i.e. later), then clear colors/scroll
	// region and re-show the cursor so the user's terminal is usable, then
	// let the panic propagate (exit code 2). Registered before restoreAll
	// so the terminal is normalized before the cleanup bytes are written.
	defer func() {
		if r := recover(); r != nil {
			crashCleanup()
			// The terminal is about to be handed back over a panic: put
			// the window title back the way rysh found it (best effort,
			// same lock-free caveat as crashCleanup).
			if markerOn && prevTitle != "" {
				os.Stdout.WriteString(screen.SetTitle(prevTitle))
			}
			panic(r)
		}
	}()
	defer restoreAll(restores)

	// Test hook: panic right after startup so the crash-cleanup path is
	// exercised deterministically (used by TestPanicCleanup).
	if os.Getenv("RYSH_TEST_PANIC") == "1" {
		panic("rysh: test panic")
	}

	// Mode state machine (shell input routing) and the fullscreen-app
	// detector for shell-mode output. Mode switching never touches the
	// screen: the persistent AI editor (internal/aiui) and the shell share
	// the same terminal stream.
	st := mode.New()
	det := screen.NewDetector()

	// writeMu serializes every write to our stdout: the output loop, the
	// inline spinner ticks, resize, the final cleanup, and the AI-mode rendering
	// all go through it. It also guards modelRef/cfg/history/cancelStream/
	// streamDone/aiPhase/pendingShellOut/height/width,
	// which the AI stream goroutine and the repaint ticker share.
	var writeMu sync.Mutex

	// height/width are the physical terminal size in rows/cells. modelRef
	// is the active "provider/model" reference, named on the AI prompt line
	// (§3.3) and on the reply header. All are only read or written while
	// holding writeMu.
	height := 0
	width := 0
	modelRef := provider.Default(cfg)

	// rec records the user's shell activity (commands + output + cwd) into
	// a bounded event ring (M4 unified event stream). The input loop feeds
	// keystrokes, the output loop feeds terminal output; runStream reads
	// Events() while holding writeMu. Recording pauses while a fullscreen
	// program owns the terminal (see the output loop's alt-screen tracking
	// and the driver's key gating).
	rec := session.New()

	// nlEpoch counts newlines seen in child output; fwdEpoch records the
	// value of nlEpoch at the moment of the most recent forwarded keystroke.
	// They are equal while the user has typed since the shell's last
	// newline, i.e. the cursor is not at a fresh line start. fwdEpoch starts
	// at -1 so the very first line counts as fresh.
	var nlEpoch, fwdEpoch atomic.Int64
	fwdEpoch.Store(-1)

	// childPasteArmed is set once the child shell's own startup output has
	// enabled bracketed paste (DEC 2004, the `\x1b[?2004h` mode-set its
	// readline prints). While unset, the child has not turned the mode on,
	// so pasted bytes reach it without `200~`/`201~` markers and the
	// recorder must not expect them; while set, a multi-line paste arrives
	// wrapped and is recorded as one block instead of a run of submits.
	var childPasteArmed atomic.Bool

	// shellOutAt is the nanotime of the most recent child pty output, stored
	// by the output loop and read by leaveAI, which waits for the shell's
	// resync repaint to settle before it re-injects the shared line.
	var shellOutAt atomic.Int64

	// shellOutTap mirrors the child's output stream so leaveAI can check the
	// echo of a re-injected line against it. See outputTap.
	var shellOutTap outputTap

	// active is the session this instance is attached to: its on-disk meta,
	// the reconstructed AI conversation (active.hist), and the open log
	// writer. meta and hist are guarded by writeMu; the log pointer is
	// additionally guarded by activeLogMu so logWrite is safe from any
	// goroutine (its callers also hold writeMu). The session stream is
	// appended to an append-only JSONL log
	// (~/.rysh/sessions/<id>/messages.log) that is the session's source of
	// truth on disk; the screen is only the live window over it.
	var active = struct {
		meta *session.Meta
		hist []ctxMsg
		log  *session.Log
	}{meta: activeMeta}
	var activeLogMu sync.Mutex

	// sessionsDir is the per-session log directory, or "" when no home
	// directory is available (no persistence: the log stays nil).
	sessionsDir := ""
	if store != nil {
		sessionsDir = store.SessionsDir()
	} else if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		sessionsDir = filepath.Join(home, ".rysh", "sessions")
	}
	// openLog opens (or creates) the JSONL log for a session id, or returns
	// nil when persistence is unavailable.
	openLog := func(id string) *session.Log {
		if sessionsDir == "" {
			return nil
		}
		lg, lerr := session.OpenLog(sessionsDir, id)
		if lerr != nil {
			return nil
		}
		return lg
	}
	active.log = openLog(active.meta.ID)

	// logWrite appends one record to the active session's log (nil-safe:
	// without persistence it is a no-op). See active's doc comment for the
	// locking contract.
	logWrite := func(kind, payload string) {
		activeLogMu.Lock()
		l := active.log
		activeLogMu.Unlock()
		l.Write(kind, payload)
	}
	defer func() {
		activeLogMu.Lock()
		l := active.log
		activeLogMu.Unlock()
		_ = l.Close()
	}()
	logWrite("sys", "session:start")

	// readSessionHistory rebuilds a session's AI conversation from its log
	// records on disk (the source of truth), used to restore context after
	// a switch or on entry to AI mode.
	readSessionHistory := func(id string) []ctxMsg {
		return readHistory(sessionsDir, id)
	}

	// userInputsFor lists a session's recorded user inputs (usr events,
	// oldest first): the recall source for the AI editor's ↑/↓ and
	// Ctrl-P/Ctrl-N.
	userInputsFor := func(id string) []string {
		recs, err := session.ReadMessages(sessionsDir, id)
		if err != nil {
			return nil
		}
		var out []string
		for _, rec := range recs {
			if rec.Kind == "usr" {
				out = append(out, rec.P)
			}
		}
		return out
	}

	// ai is the persistent AI input editor; it lives for the whole process
	// and survives mode switches, so the draft and conversation history
	// carry across AI⇄shell round trips. cancelStream/streamDone are the
	// in-flight task handle (guarded by writeMu); aiPhase is the busy text
	// ("thinking"/"writing"/"exec") of the streaming task, which the two-phase
	// ^C reads to tell a running tool from a streaming reply. jobMgr owns
	// the session's background jobs (§8.2); toolIntr is the two-phase ^C
	// handle (§8.3) the engine holds while a tool executes. pendingAsk is
	// the open §7 approval prompt's answer channel (nil = none); the
	// engine goroutine blocks in askApproval until the driver routes a
	// y/n/a key or the task is cancelled.
	ai := aiui.New()
	jobMgr := agent.NewJobManager()
	// subMgr owns the session's worker subagents (§16): the manager run
	// delegates non-trivial work to them and polls/kills them through the
	// task tools; their events render through a per-subagent sink that
	// logs under the s* kinds so the reconstructed manager history never
	// absorbs a worker's transcript.
	subMgr := agent.NewSubagentManager()
	subMgr.SinkFor = func(id int) agent.Sink {
		return &subSink{id: id, writeMu: &writeMu, md: markdown.New(), logWrite: logWrite}
	}
	toolIntr := &agent.ToolInterrupt{}
	var (
		cancelStream context.CancelFunc
		streamDone   chan struct{}
		aiPhase      string
		pendingAsk   chan agent.Answer
	)

	// runStream is the one-task executor (declared first so submitAI, which
	// spawns it, can reference it; the function body is assigned below).
	var runStream func(ctx context.Context, cancel context.CancelFunc, done chan struct{}, text string)

	// switchToMeta switches the active session and is only ever called on
	// the mainloop goroutine (see its doc comment at the assignment).
	// pendingSwitch holds a switch deferred while a task streams; the
	// mainloop's ctlTick consumes it.
	var switchToMeta func(meta *session.Meta, notice string, landInAI bool)
	var pendingSwitch *pendingSwitchT

	// spinIdx advances the streaming spinner; preOutput reports that a task
	// is streaming but no output token has landed yet, so the ticker animates
	// an inline spinner on the reply line (the last line of the stream).
	// preOutputLabel is the spinner's caption ("正在思考..." while a model
	// round is silent, "执行中..." while a tool runs);
	// spinStart is when the spinner was last armed, so the elapsed time on
	// the line counts the current wait. Guarded by writeMu.
	var (
		spinIdx        int
		preOutput      bool
		preOutputLabel = uiT.Get("spinner_thinking")
		spinStart      time.Time
	)
	// startupSpin is true while the shell-startup spinner is up: the login
	// shell is still booting (its profile runs before the first prompt), so
	// the ticker animates the 启动中 spinner even though no task streams.
	// The first child output clears it (output loop), and enterAI does so
	// earlier if the user enters AI mode before any output. Guarded by
	// writeMu; startupSpin is only true while the spinner is armed.
	var startupSpin bool

	// spinnerLineLocked formats the pre-output spinner row: the current
	// frame, the task caption, and the elapsed time since the spinner was
	// armed (1s, 2s, 1m56s). writeMu must be held by the caller.
	spinnerLineLocked := func() string {
		return "\r" + screen.EraseToEOL() + screen.DimGray() +
			spinnerFrames[spinIdx%len(spinnerFrames)] + " " + preOutputLabel + " " +
			spinnerElapsed(time.Since(spinStart)) + screen.ColorReset
	}

	// armSpinnerLocked draws the inline pre-output spinner (a braille glyph,
	// a task note, and the elapsed wait) on the line where the output will
	// land, bracketed by blank rows — the first arm breaks to a fresh row
	// so a blank row sits between the spinner and the content above, and
	// the row below stays empty while the spinner is the stream's last
	// line — and lets the ticker animate it, so the wait for the first
	// token is visible exactly where the output will arrive. label replaces
	// the
	// caption ("正在思考..." while a model round is silent, "执行中..."
	// while a tool runs); an empty label keeps the current
	// one, so a repaint mid-busy (a subagent line) spins on with the caption
	// it had. It also hides the terminal cursor: the spinner is the live
	// indicator while the task is busy, so a blinking cursor at the stream
	// tail is redundant (the cursor is shown again in finalize when the task
	// settles, or when the 启动中 spinner is cleared by the shell's first
	// output or the AI prompt). writeMu must be held by the caller.
	armSpinnerLocked := func(label string) {
		first := !preOutput
		preOutput = true
		if label != "" {
			preOutputLabel = label
		}
		spinStart = time.Now()
		if first {
			// The spinner block is bracketed by blank rows: the break
			// leaves the row above intact, and the row below stays empty
			// (the spinner is the stream's last line), so the spinner
			// never glues to the content around it.
			os.Stdout.WriteString("\r\n")
		}
		os.Stdout.WriteString(screen.HideCursor())
		os.Stdout.WriteString(spinnerLineLocked())
	}

	// clearSpinnerLocked erases the inline pre-output spinner in place
	// (cursor stays on that line at column 1); callers follow with the
	// content that replaces it. It is a no-op when no spinner is armed and
	// reports whether it cleared, so a caller can issue the one-time row
	// break that precedes the first output token. writeMu must be held by
	// the caller.
	clearSpinnerLocked := func() bool {
		if !preOutput {
			return false
		}
		preOutput = false
		os.Stdout.WriteString("\r" + screen.EraseToEOL())
		return true
	}

	// pendingLineBuf holds pty output not yet flushed to the session log as
	// complete shl lines (a trailing partial line stays here until the next
	// newline, a mode switch, or a flush). Guarded by writeMu.
	var pendingLineBuf []byte

	// pendingShellOut holds raw shell output produced while AI mode is
	// active, deferred until return to shell mode (when it is flushed to the
	// screen so the session stream stays complete). It keeps the head and
	// sets pendingOverflow once it exceeds maxPending. Guarded by writeMu.
	var (
		pendingShellOut []byte
		pendingOverflow bool
	)

	// fullscreenApp mirrors the Detector's alt-screen state; it is stored by
	// the output loop and read by the driver (mode-switch guard) and by
	// resize.
	var fullscreenApp atomic.Bool

	// noticeLine prints a transient dim notice line to the terminal. It is
	// the main-screen replacement for the removed bottom status row: the
	// message scrolls like any other output instead of being pinned. It
	// takes writeMu itself.
	noticeLine := func(msg string) {
		writeMu.Lock()
		os.Stdout.WriteString(screen.DimGray() + msg + screen.ColorReset + "\r\n")
		writeMu.Unlock()
	}

	// askApproval shows the §11.3 prompt as a highlighted amber bar on a row
	// of its own, narrows the streaming lock to the y/n/a keys, and blocks
	// the engine goroutine until the driver routes an answer or the task is
	// cancelled (^C answers deny). Runs on the engine goroutine; every
	// shared touch goes through writeMu.
	askApproval := func(ctx context.Context, req agent.AskRequest) agent.Answer {
		ch := make(chan agent.Answer, 1)
		writeMu.Lock()
		pendingAsk = ch
		ai.SetApproval(true)
		// The bar owns its row: a streamed reply leaves the cursor mid-line
		// (deltas flush without a trailing newline) and a call-only round
		// leaves the pre-output spinner armed on the row where the reply
		// would land — whose ticker would otherwise repaint over anything
		// glued there. Erase the spinner (that also stops it and lands the
		// cursor at column 1) or break the line first, so the ask prompt is
		// never appended to the tail of the answer. finalize() breaks the
		// line the same way before the fresh prompt.
		if !clearSpinnerLocked() {
			os.Stdout.WriteString("\r\n")
		}
		os.Stdout.WriteString(screen.ApprovalBar(uiT.Get("approval_bar", req.Display)) + "\r\n")
		logWrite("approval", "asked: "+aiui.Sanitize(req.Display))
		writeMu.Unlock()
		defer func() {
			writeMu.Lock()
			if pendingAsk == ch {
				pendingAsk = nil
			}
			ai.SetApproval(false)
			// The answer is in: the call now really runs (deny included —
			// its [tool] line lands on this row). Arm the 执行中 spinner
			// below the bar so the wait for the result is visible and the
			// spinner's appearance acknowledges the key.
			armSpinnerLocked(uiT.Get("spinner_executing"))
			writeMu.Unlock()
		}()
		var ans agent.Answer
		select {
		case ans = <-ch:
		case <-ctx.Done():
			ans = agent.AnswerDeny
		}
		writeMu.Lock()
		logWrite("approval", "decided: "+aiui.Sanitize(req.Display)+" → "+ans.String())
		writeMu.Unlock()
		return ans
	}

	// approval is the session's §7 permission gate, process-level like
	// jobMgr: the engine consults it before every tool call, and the
	// mode is re-read from the config at each task start (hot reload).
	approval := agent.NewApproval("ask", askApproval)

	// logLinesLocked buffers raw pty output and flushes complete lines to
	// the session log as shl records (SGR kept, control sequences stripped).
	// writeMu must be held by the caller. A pathological newline-less output
	// is flushed wholesale once it exceeds maxLineLog.
	logLinesLocked := func(chunk []byte) {
		pendingLineBuf = append(pendingLineBuf, chunk...)
		if len(pendingLineBuf) > maxLineLog {
			logWrite("shl", session.SanitizeStream(string(pendingLineBuf)))
			pendingLineBuf = pendingLineBuf[:0]
		}
		for {
			idx := bytes.IndexByte(pendingLineBuf, '\n')
			if idx < 0 {
				break
			}
			line := pendingLineBuf[:idx+1]
			logWrite("shl", session.SanitizeStream(string(line)))
			pendingLineBuf = pendingLineBuf[idx+1:]
		}
	}

	// flushAllLocked logs the pending partial line (no trailing newline) to
	// the session log as shl. writeMu must be held by the caller.
	flushAllLocked := func() {
		if len(pendingLineBuf) > 0 {
			logWrite("shl", session.SanitizeStream(string(pendingLineBuf)))
			pendingLineBuf = pendingLineBuf[:0]
		}
	}

	// runCommand dispatches a submitted slash command. It re-reads the
	// config from disk on every /model so edits take effect without
	// restarting rysh (hot reload, matching pi's /model). It must be called
	// while holding writeMu because it updates modelRef and active.hist.
	// The returned lines are printed into the stream (no bullet prefix); a
	// non-nil switchReq asks the caller to run a session switch (executed
	// after writeMu is released). /new and /resume return their confirmation as
	// the switchReq's notice, printed by switchToMeta on landing.
	runCommand := func(text string) ([]string, *switchReq) {
		fields := strings.Fields(text)
		if len(fields) == 0 {
			return nil, nil
		}
		switch fields[0] {
		case "/model":
			fresh, err := config.Load(cfgPath)
			if err != nil {
				return []string{"rysh: read config: " + err.Error()}, nil
			}
			cfg = fresh
			refs := provider.List(cfg)
			if len(refs) == 0 {
				return []string{"no models configured (edit " + cfgPath + ")"}, nil
			}
			if len(fields) == 1 {
				// No argument: numbered list (the number is what /model <N>
				// accepts), the active model marked.
				return modelListLines(cfg, modelRef, cfgPath), nil
			}
			// Argument: a bare number picks the Nth listed model, anything
			// else is resolved as a "provider/model" reference.
			target := fields[1]
			if n, err := strconv.Atoi(target); err == nil {
				if n < 1 || n > len(refs) {
					return []string{uiT.Get("index_out_of_range", len(refs), target)}, nil
				}
				target = refs[n-1]
			}
			spec, err := provider.Resolve(cfg, target)
			if err != nil {
				return []string{"no such model: " + target}, nil
			}
			modelRef = spec.Ref()
			if markerOn {
				os.Stdout.WriteString(screen.SetTitle(ryshTitle(modelRef, active.meta.ID)))
			}
			return []string{"model: " + modelRef}, nil
		case "/new":
			// Create a fresh session and switch to it. The switch (shell
			// restart + repaint) runs after writeMu is released.
			if store == nil {
				return []string{uiT.Get("no_session_store")}, nil
			}
			m, err := store.CreateSession()
			if err != nil {
				return []string{"rysh: " + err.Error()}, nil
			}
			_ = store.TouchUpdated(m.ID)
			return nil, &switchReq{meta: m, notice: uiT.Get("created_session", m.ID)}
		case "/ls":
			page := 1
			if len(fields) > 1 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					page = n
				}
			}
			attached := map[string]int{}
			if store != nil {
				attached = scanAttached(store.CtlDir(), os.Getpid())
			}
			return renderSessionList(store, active.meta.ID, page, attached), nil
		case "/history":
			// User-input history for the current session (usr records,
			// oldest first, newest last), paginated like /ls. -v pairs each
			// input with the model's reply that followed it.
			page := 1
			verbose := false
			for _, a := range fields[1:] {
				if a == "-v" {
					verbose = true
				} else if n, err := strconv.Atoi(a); err == nil {
					page = n
				}
			}
			return renderHistory(sessionsDir, active.meta.ID, page, verbose), nil
		case "/resume":
			if len(fields) < 2 {
				return []string{uiT.Get("usage_resume_slash")}, nil
			}
			meta, err := resolveSession(store, fields[1])
			if err != nil {
				return []string{err.Error()}, nil
			}
			if pid, ok := attachedByOther(store, meta.ID, os.Getpid()); ok {
				return []string{uiT.Get("attached_session", meta.ID, pid, meta.ID)}, nil
			}
			return nil, &switchReq{meta: meta, notice: uiT.Get("switched_session", fmt.Sprint(meta.Num))}
		case "/help":
			return []string{
				uiT.Get("help_commands"),
				uiT.Get("help_new"),
				uiT.Get("help_ls"),
				uiT.Get("help_history"),
				uiT.Get("help_resume"),
				uiT.Get("help_model"),
				uiT.Get("help_quit"),
				uiT.Get("help_help"),
				uiT.Get("help_tab"),
				uiT.Get("help_paste"),
				uiT.Get("help_bang"),
			}, nil
		default:
			return []string{"unknown command: " + fields[0]}, nil
		}
	}

	// applySize resizes the child pty to the full terminal. rysh runs on the
	// main screen (no reserved status row, no scroll region), so the pty owns
	// every row and the terminal's own scrollback handles history.
	applySize := func() {
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			writeMu.Lock()
			height = h
			width = w
			_ = p.Resize(w, h)
			ai.SetSize(w, h)
			ai.ResetInputState()
			writeMu.Unlock()
		}
	}
	// rysh runs on the main screen (it never enters the alternate screen):
	// print a startup banner — the session identity plus one hint line —
	// so the user can tell rysh is running, since the shell prompt below
	// it looks exactly like their own. With the prompt marker on, the
	// title probe reads the terminal's current window title first (for
	// restore on exit) and rysh then takes the title over. Then size the
	// pty to the full terminal. rysh's output stays in the terminal's
	// scrollback after exit, like any other command's.
	// Probe the terminal's background polarity (OSC 11) before any themed
	// output is drawn, so the whole UI is colored for the right background
	// polarity instead of assuming dark. Runs before the key reader starts
	// (like the title probe), with a short deadline; a terminal that does
	// not answer leaves polarity unknown and the theme falls back to dark.
	// The integration tests run rysh as a pty child (RYSH_TEST_CHILD=1); they
	// skip the probe so the child is deterministic (dark polarity, no query
	// bytes written into the pty stream the assertions read).
	if os.Getenv("RYSH_TEST_CHILD") == "" {
		theme.SetPolarity(probeTerminalBg(150 * time.Millisecond))
	}
	if markerOn {
		prevTitle = probeTerminalTitle(150 * time.Millisecond)
	}
	writeMu.Lock()
	banner := ryshTitle(modelRef, active.meta.ID)
	// History the attached session already carries (a continued session,
	// or one named by rysh new / rysh resume on the command line) has never
	// been shown in this terminal: print a continuation separator plus
	// the tail first — the same shape switchToMeta uses — and the banner
	// right under it, so the two hint lines always land on the visible
	// screen above the first prompt: a replay longer than the screen
	// (up to replayTailLines) would otherwise scroll the banner off the
	// top. Not at the first AI entry: on the main screen a mid-session
	// replay pushes the live prompts off the visible screen, and a plain
	// mode switch must stay an in-place prompt swap.
	if sessionsDir != "" {
		if count := sessionMessageCount(sessionsDir, active.meta.ID); count > 0 {
			os.Stdout.WriteString(screen.DimGray() + uiT.Get("resume_session", active.meta.ID, count) + screen.ColorReset)
			if replay := renderUnifiedReplay(sessionsDir, active.meta.ID, replayTailLines); replay != "" {
				os.Stdout.WriteString(strings.ReplaceAll(replay, "\n", "\r\n"))
			}
		}
	}
	os.Stdout.WriteString(screen.DimGray() + banner + screen.ColorReset + "\r\n")
	os.Stdout.WriteString(screen.DimGray() + "  " + uiT.Get("banner_tail", modeSwitchLabel(cfg.Keys.ModeSwitch)) + screen.ColorReset + "\r\n")
	if markerOn {
		os.Stdout.WriteString(screen.SetTitle(banner))
	}
	// The login shell is still booting (its profile runs before the first
	// prompt): until the first child output lands, the 启动中 spinner owns
	// the row that output will take, so the startup wait is visible like
	// the model's is. The output loop clears it on the first chunk,
	// enterAI takes the row over if the user switches first.
	startupSpin = true
	armSpinnerLocked(uiT.Get("spinner_startup"))
	writeMu.Unlock()
	applySize()

	// Resize the pty when the outer terminal changes size.
	winch := make(chan os.Signal, 1)
	if sigs := winchSignals(); len(sigs) > 0 {
		signal.Notify(winch, sigs...)
		defer signal.Stop(winch)
	}
	go func() {
		for range winch {
			applySize()
		}
	}()

	// drawAIPromptLocked writes the AI input line (PS-style prompt + draft)
	// at the cursor's current position. The prompt names the active model
	// (§3.3), so it is re-drawn whenever the model or the session changes.
	// writeMu must be held by the caller.
	drawAIPromptLocked := func() {
		dir := currentCWD(c.Process.Pid)
		prompt, pw := screen.PSExpandModel(cfg.AIPrompt(modelRef), dir, modelRef)
		ai.BeginInput(prompt, pw)
		ai.RenderInput(func(s string) { os.Stdout.WriteString(s) }, width)
	}

	// Tab completion (first word only). /-candidates come from the static
	// command list. /model and /resume with the cursor right after the
	// space show their numbered list (model list / session list) instead
	// of inserting, so the user can type the number directly. Multiple
	// matches get one dim notice line and then silent cycling; any draft
	// edit resets the cycle, and the inserted text is undoable.
	slashCmds := []string{"/exit", "/help", "/history", "/ls", "/model", "/new", "/quit", "/resume"}
	// printStreamLinesLocked prints notice lines into the stream where the
	// input line stood, then redraws the prompt below them. writeMu must be
	// held by the caller. List rows are plain text in normal color; a row
	// whose first cell is > (the selected/active item) gets the gray
	// SelectedBg bar so it reads as "this one is current".
	printStreamLinesLocked := func(lines ...string) {
		ai.EraseInput(func(s string) { os.Stdout.WriteString(s) })
		for _, ln := range lines {
			if strings.HasPrefix(ln, ">") {
				os.Stdout.WriteString(screen.SelectedLine(ln) + "\r\n")
			} else {
				os.Stdout.WriteString(ln + screen.ColorReset + "\r\n")
			}
			logWrite("noti", aiui.Sanitize(ln))
		}
		drawAIPromptLocked()
	}
	// listShown remembers the last draft for which a selection list (the
	// model list or the session list) was printed, so a repeated Tab on the
	// same draft does not reprint it. Each list also remembers its source
	// on disk (the config file / the sessions dir): an edit to the config
	// (hot reload) or a new session reprints even on the same draft, so a
	// just-added model cannot stay hidden under the repeat no-op.
	var listShown string
	var shownCfg, shownSessions os.FileInfo
	sourceChanged := func(fi, prev os.FileInfo) bool {
		if fi == nil || prev == nil {
			return fi != prev
		}
		return !fi.ModTime().Equal(prev.ModTime()) || fi.Size() != prev.Size()
	}
	showModelList := func(draft string) {
		fi, _ := os.Stat(cfgPath)
		if draft == listShown && !sourceChanged(fi, shownCfg) {
			return
		}
		listShown = draft
		shownCfg = fi
		fresh, err := config.Load(cfgPath)
		if err != nil {
			printStreamLinesLocked("rysh: read config: " + err.Error())
			return
		}
		printStreamLinesLocked(modelListLines(fresh, modelRef, cfgPath)...)
	}
	// showSessionList prints the first page of the session list (the /ls
	// view), so the number / id to type after /resume can be picked.
	showSessionList := func(draft string) {
		var fi os.FileInfo
		if store != nil {
			fi, _ = os.Stat(store.SessionsDir())
		}
		if draft == listShown && !sourceChanged(fi, shownSessions) {
			return
		}
		listShown = draft
		shownSessions = fi
		attached := map[string]int{}
		if store != nil {
			attached = scanAttached(store.CtlDir(), os.Getpid())
		}
		printStreamLinesLocked(renderSessionList(store, active.meta.ID, 1, attached)...)
	}
	ai.SetCompleter(func(draft string, pos int) []string {
		firstSpace := strings.IndexByte(draft, ' ')
		if firstSpace < 0 || pos <= firstSpace {
			// First word: only / completes.
			word := draft[:pos]
			var cands []string
			switch {
			case word == "":
				// nothing to complete
			case strings.HasPrefix(word, "/"):
				for _, c := range slashCmds {
					if strings.HasPrefix(c, word) {
						cands = append(cands, c)
					}
				}
			}
			if len(cands) > 1 {
				shown := make([]string, 0, len(cands))
				for _, c := range cands {
					shown = append(shown, c)
					if len(shown) == 8 {
						break
					}
				}
				tail := ""
				if len(cands) > 8 {
					tail = "…"
				}
				printStreamLinesLocked(uiT.Get("completions", len(cands), strings.Join(shown, " "), tail))
			}
			return cands
		}
		// Second word: /model and /resume take a selection argument; with
		// the cursor right after the space, print the list to pick from.
		if firstWord := draft[:firstSpace]; pos == firstSpace+1 {
			switch firstWord {
			case "/model":
				showModelList(draft)
			case "/resume":
				showSessionList(draft)
			}
		}
		return nil
	})

	// enterAI hands the stream to the AI editor: the shell keeps running
	// but its output is deferred (logged, flushed on return). The shell's
	// unsubmitted input becomes the AI editor's draft — one shared input
	// line that follows the mode switch both ways — and the shell's own
	// copy of it is cleared on the child pty (^A ^K for a shell with a line
	// editor, ^U for one without); the AI prompt replaces the shell
	// prompt line in place. The session's timeline is not re-rendered on
	// a plain switch — the shell and AI streams display every record as
	// it is logged, a session switch replays the target's tail, and
	// startup-attached history was already shown under the banner.
	enterAI := func() {
		writeMu.Lock()
		flushAllLocked()
		logWrite("sys", "mode:ai")
		shared := rec.Line()
		cur := rec.Cursor()
		rec.Abort()
		// Rebuild the conversation context from the session log so a
		// subprocess (rysh ai) that wrote to this session, or a switch,
		// is reflected in what the model sees next.
		active.hist = readSessionHistory(active.meta.ID)
		ai.SetHistory(userInputsFor(active.meta.ID))
		writeMu.Unlock()
		if shared != "" {
			// Clear the shell's input buffer so the shared line is not
			// duplicated when the draft is re-injected on return. A plain
			// shell (dash) kills the whole line with ^U; a line-editor
			// shell (bash/zsh/fish/ksh) would only kill from the cursor to
			// the line start, leaving the tail behind when the cursor is
			// mid-line, so move to the start first and then kill to the
			// end. The erase echo is stripped from the deferred output on
			// return.
			if lineEditorShell(sh) {
				_, _ = p.Write([]byte{0x01, 0x0b}) // ^A ^K
			} else {
				_, _ = p.Write([]byte{0x15}) // ^U
			}
		}
		writeMu.Lock()
		// The shell may still be booting: the AI prompt takes over the
		// 启动中 spinner's row, which the shell's first output would
		// otherwise land on. The AI prompt is a fresh prompt, so the
		// cursor the startup arm hid is shown again.
		startupSpin = false
		if clearSpinnerLocked() {
			os.Stdout.WriteString(screen.ShowCursor())
		}
		ai.SetDraft(shared, cur)
		os.Stdout.WriteString("\r" + screen.EraseToEOL())
		drawAIPromptLocked()
		writeMu.Unlock()
	}

	// leaveAI hands the stream back to the shell: the deferred shell output
	// is flushed to the screen, the shell's own prompt is redrawn onto the
	// line the AI prompt occupied (see the resync below), and the current
	// AI draft is re-injected as the shell's input line, so the one shared
	// line survives the round trip.
	leaveAI := func() {
		st.ToShell()
		writeMu.Lock()
		flushAllLocked()
		logWrite("sys", "mode:shell")
		ai.EraseInput(func(s string) { os.Stdout.WriteString(s) })
		if len(pendingShellOut) > 0 {
			os.Stdout.WriteString(session.SanitizeStreamKeepCR(string(pendingShellOut)))
			pendingShellOut = pendingShellOut[:0]
		}
		if pendingOverflow {
			os.Stdout.WriteString(screen.DimGray() + "... shell output truncated" + screen.ColorReset + "\r\n")
			pendingOverflow = false
		}
		// The real cursor is at the top of the erased AI block — where the
		// input line was — so the resync below lands the shell prompt in
		// that same spot. The input line does not jump across the screen
		// when switching back to the shell.
		writeMu.Unlock()
		// Resync: bring the shell's own prompt back onto the line the AI
		// prompt occupied, so switching back changes only the PS1 text and
		// the cursor shape, never the input line's position. Shells with a
		// line editor (bash, zsh, fish, ksh) repaint their prompt in place
		// when their controlling terminal is resized, so rysh nudges the
		// child pty width and the shell redraws its prompt on the erased
		// block's row. Plain shells (dash) have no line editor: a bare \r
		// makes them execute the empty line and print the next prompt one
		// row below, so rysh first moves the real cursor one row up to keep
		// that prompt on the erased block's row too.
		resyncStart := time.Now()
		if lineEditorShell(sh) && width > 2 {
			_ = p.Resize(width-1, height)
			// A line-editor shell coalesces back-to-back SIGWINCHes and
			// only re-reads the size once, so the width would come back
			// unchanged and no repaint would happen. Pausing between the
			// two resizes lets the shell process the first change and
			// redraw its prompt before the width is restored.
			time.Sleep(resizeRepaintWait)
			_ = p.Resize(width, height)
			// Re-anchor the settle wait on this second resize, so the wait
			// covers the repaint the width restore triggers instead of
			// settling on the first repaint while the second is still due.
			resyncStart = time.Now()
		} else {
			os.Stdout.WriteString(screen.CursorUp(1))
			_, _ = p.Write([]byte("\r"))
		}
		if shared := ai.Draft(); shared != "" {
			// A `!`-leading draft is a shell command the user mistyped in AI
			// mode (see submitAI): switching over strips the `!` so the
			// command lands ready to run; a bare `!` leaves an empty line.
			if strings.HasPrefix(shared, "!") {
				shared = shared[1:]
			}
			if shared != "" {
				// Wait for the shell's resync repaint to land before
				// re-injecting the saved line, so the line is echoed after
				// (not before) the prompt. The wait is only an
				// optimisation: it keeps the common case to a single
				// injection. Whether the line actually landed is settled by
				// its echo, because a byte written into a pty that is still
				// being resized can vanish without the shell ever seeing it.
				waitShellOutputSettled(&shellOutAt, resyncStart,
					resyncOutputIdle, resyncPromptWait)
				runes := []rune(shared)
				for _, rn := range runes {
					rec.Type(rn)
				}
				injectResidual(p, &shellOutTap, shared, lineEditorShell(sh))
				// The text was re-injected wholesale, so the shell's cursor
				// sits at the end of the line; if the user had parked it
				// mid-draft, send one Left arrow per rune to bring it back,
				// and mirror the move in the recorder so the next switch
				// carries the same column. The `!` stripped above sits one
				// column before the draft's first rune, so the parking
				// column shifts by it too. Plain shells (dash) have no line
				// editor and would garble the arrow bytes, so only
				// line-editor shells get repositioned.
				back := len(runes) - ai.Pos()
				if strings.HasPrefix(ai.Draft(), "!") {
					back--
				}
				if back > 0 && lineEditorShell(sh) {
					_, _ = p.Write([]byte(strings.Repeat("\x1b[D", back)))
					rec.SetCursor(ai.Pos() - 1)
				}
			}
		}
		// The line the shell comes back to counts as fresh for
		// mode-switching, so the user's next space switches to AI whether
		// or not a residual was re-injected; a typed character arms
		// forwarding the usual way and re-disarms the gesture.
		fwdEpoch.Store(-1)
	}

	// submitAI handles a submitted AI prompt and reports whether the user
	// asked to quit. Slash commands are dispatched locally and never reach
	// the model. A `!`-leading input is a shell command mistyped in AI mode:
	// it is not sent to the model — the draft is kept, a notice offers the
	// two ways to the shell (the mode-switch key hands the line over with
	// the `!` stripped, see leaveAI; a leading space on an empty draft
	// switches after clearing), and the prompt is redrawn. Anything else
	// starts a streaming task (mode switching is locked while it runs). The
	// task handle (cancel/streamDone) is published under writeMu before the
	// goroutine launches, closing the ^C-during-setup race.
	submitAI := func(text string) bool {
		// The submitted input joins the recall list (↑/↓, ^P/^N) whether
		// it is a slash command or a natural-language task.
		writeMu.Lock()
		ai.Remember(text)
		writeMu.Unlock()
		if strings.HasPrefix(text, "/") {
			if f := strings.Fields(text); len(f) > 0 && (f[0] == "/quit" || f[0] == "/exit") {
				logWrite("usr", aiui.Sanitize(text))
				return true
			}
			writeMu.Lock()
			lines, req := runCommand(text)
			logWrite("usr", aiui.Sanitize(text))
			if store != nil {
				_ = store.TouchUpdated(active.meta.ID)
			}
			for _, ln := range lines {
				if strings.HasPrefix(ln, ">") {
					os.Stdout.WriteString(screen.SelectedLine(ln) + "\r\n")
				} else {
					os.Stdout.WriteString(ln + screen.ColorReset + "\r\n")
				}
				logWrite("noti", aiui.Sanitize(ln))
			}
			drawAIPromptLocked()
			writeMu.Unlock()
			if req != nil {
				// The switch (shell kill/wait/restart) must not hold writeMu.
				switchToMeta(req.meta, req.notice, true)
			}
			return false
		}
		if strings.HasPrefix(text, "!") {
			// A `!`-leading input is a shell command the user mistyped in
			// AI mode (the old inline-`!` escape was removed): do not send
			// it to the model. Keep it in the draft so the mode-switch key
			// can hand it to the shell (leaveAI strips the `!`), and point
			// the user at both escape routes.
			writeMu.Lock()
			ai.SetDraft(text, len([]rune(text)))
			logWrite("usr", aiui.Sanitize(text))
			if store != nil {
				_ = store.TouchUpdated(active.meta.ID)
			}
			logWrite("noti", "AI 模式下 ! 开头输入不执行，提示切换到 shell 模式")
			printStreamLinesLocked(
				uiT.Get("bang_hint_1"),
				uiT.Get("bang_hint_2", modeSwitchLabel(cfg.Keys.ModeSwitch)),
				uiT.Get("bang_hint_3"),
			)
			drawAIPromptLocked()
			writeMu.Unlock()
			return false
		}
		logWrite("usr", aiui.Sanitize(text))
		if store != nil {
			_ = store.TouchUpdated(active.meta.ID)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		writeMu.Lock()
		ai.StartStream()
		aiPhase = ""
		cancelStream = cancel
		streamDone = done
		logWrite("sys", "task:start")
		writeMu.Unlock()
		go runStream(ctx, cancel, done, text)
		return false
	}

	// cancelAndWait aborts the in-flight task on ^C. The deferred finalize
	// in runStream (which runs before close(done)) clears the inline spinner
	// and redraws the AI prompt, so waiting on streamDone guarantees a clean
	// state before the driver reads the next key.
	cancelAndWait := func() {
		writeMu.Lock()
		if cancelStream != nil {
			cancelStream()
			logWrite("sys", "task:terminate")
		}
		done := streamDone
		writeMu.Unlock()
		if done != nil {
			<-done
		}
	}

	// finalize settles the driver state after a task ends (any path: reply
	// finished, cancelled, or an error): the editor unlocks, the busy phase
	// clears, and a fresh AI prompt is drawn in AI mode. It also re-shows the
	// terminal cursor, which armSpinnerLocked hid for the duration of the
	// busy task (thinking + streaming); from here the user is back at the
	// prompt and the cursor marks the input position again.
	finalize := func() {
		writeMu.Lock()
		ai.EndStream()
		// Safety net: any task that ends before its first output token
		// leaves the inline spinner armed; erase it so the fresh prompt is
		// not preceded by a dead spinner line.
		clearSpinnerLocked()
		aiPhase = ""
		cancelStream = nil
		streamDone = nil
		logWrite("sys", "task:end")
		if st.Mode() == mode.AI {
			// Draw the fresh prompt right after the reply, where the shell
			// prompt would sit after equivalent output, so the input line
			// never moves across a mode switch. The blank line separates the
			// prompt from the previous reply's last rendered output.
			os.Stdout.WriteString("\r\n\r\n")
			drawAIPromptLocked()
		}
		os.Stdout.WriteString(screen.ShowCursor())
		writeMu.Unlock()
	}

	// runStream runs one AI task: it snapshots the request state and hands
	// the prompt to the agent engine, which streams reasoning/reply tokens
	// and runs the tool loop — executing the reply's native function calls
	// and feeding the captured output back until the model answers without
	// tool calls, the guards
	// wrap it up, or ^C stops it (executed commands never touch the shared
	// pty). It runs in its own goroutine; all shared state goes through
	// writeMu (via the sink and the final merge), and the tool executions
	// happen outside it so a slow command cannot stall the driver. Defer
	// order matters: finalize runs before close(done) so cancelAndWait
	// observes the settled state.

	runStream = func(ctx context.Context, cancel context.CancelFunc, done chan struct{}, text string) {
		defer cancel()
		defer close(done)
		defer finalize()
		// Task-stats footer (M8.7): one dim system-message line, separated
		// from the reply by a blank line, right before the fresh prompt —
		// duration, LLM rounds, tool calls, and the token split (the
		// cached-hit count only when the endpoint reported it). Declared
		// after finalize and before the markdown flush so, in LIFO order,
		// it lands after the reply's held lines and before finalize draws
		// the prompt. Skipped when no LLM round ran (e.g. no models).
		start := time.Now()
		var res agent.RunResult
		defer func() {
			if res.Steps == 0 {
				return
			}
			d := time.Since(start)
			line := uiT.Get("task_stats", agent.HumanDuration(d), res.Steps, res.ToolCalls, commaInt(res.PromptTokens), commaInt(res.CompletionTokens))
			if res.CachedTokens > 0 {
				line += uiT.Get("task_stats_cache", commaInt(res.CachedTokens))
			}
			logWrite("sys", fmt.Sprintf("task:stats ms=%d rounds=%d tools=%d prompt=%d completion=%d cached=%d",
				d.Milliseconds(), res.Steps, res.ToolCalls, res.PromptTokens, res.CompletionTokens, res.CachedTokens))
			writeMu.Lock()
			os.Stdout.WriteString("\r\n" + screen.DimGray() + line + screen.ColorReset + "\r\n")
			writeMu.Unlock()
		}()
		// md renders the reply's markdown into styled terminal output,
		// holding back lines with an open construct so raw markers never
		// flash. The flush defer runs before finalize (declared after it),
		// so a held line lands above the fresh prompt; it is task-local and
		// single-goroutine, so it needs no lock of its own.
		md := markdown.New()
		defer func() {
			if held := md.Close(); held != "" {
				writeMu.Lock()
				os.Stdout.WriteString(strings.ReplaceAll(held, "\n", "\r\n"))
				writeMu.Unlock()
			}
		}()

		// Snapshot the request state under writeMu, then release it before
		// any network call or command execution.
		writeMu.Lock()
		cfgNow := cfg
		pathNow := cfgPath
		refNow := modelRef
		cwd := currentCWD(c.Process.Pid)
		env := currentEnv(c.Process.Pid, envAllowlistFor(cfgNow))
		events := rec.Events()
		// hist is the verbatim record of past turns; the engine L1-slims
		// its own request view per round (newest tool results verbatim,
		// older ones placeholders).
		hist := active.hist
		writeMu.Unlock()

		write := func(s string) {
			writeMu.Lock()
			defer writeMu.Unlock()
			os.Stdout.WriteString(s)
		}

		if refNow == "" {
			write(screen.DimGray() + "no models configured (edit " + pathNow + ")" + screen.ColorReset + "\r\n")
			logWrite("noti", aiui.Sanitize("no models configured (edit "+pathNow+")"))
			return
		}
		spec, err := provider.Resolve(cfgNow, refNow)
		if err != nil {
			write(screen.DimGray() + "rysh: " + err.Error() + screen.ColorReset + "\r\n")
			logWrite("noti", aiui.Sanitize("rysh: "+err.Error()))
			return
		}

		// Build the conversation: the current directory, the shell's
		// environment, and the agent instructions lead every request; then
		// the unified timeline — recent shell events and the prior AI
		// turns merged chronologically so their interleaving is preserved
		// (each shell event is its own message, in the place it happened).
		// Internally the events are role "system" — the strictest
		// OpenAI-compatible endpoints reject a system message after the
		// leading run (sglang: "System message must be at the
		// beginning"), so provider.compliantSystem demotes them to marked
		// user messages on the wire. base is fixed for the task and
		// carried as TurnMsgs so the engine's L1 slimming folds old tool
		// results across the whole request view; the in-flight turn grows
		// inside the engine.
		base := []agent.TurnMsg{}
		if cwd != "" {
			base = append(base, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: "cwd: " + cwd}})
		}
		if env != "" {
			base = append(base, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: "env:\n" + env}})
		}
		base = append(base, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: agent.Instructions}})
		shellVerbatim, shellFolded := foldShellEvents(events)
		for _, it := range buildTimeline(shellVerbatim, hist, shellFolded) {
			if it.ev != nil {
				base = append(base, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: it.ev.Format()}})
			} else if it.folded != "" {
				base = append(base, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: "以下 shell 命令较早，已折叠为一行（cwd · 命令 · 输出大小），需要细节可重跑：\n" + it.folded + "\n---\n"}})
			} else {
				base = append(base, it.msg.TurnMsg)
			}
		}
		client := provider.NewOpenAIClient(spec)
		sink := &streamSink{
			writeMu:      &writeMu,
			md:           md,
			logWrite:     logWrite,
			armSpinner:   armSpinnerLocked,
			clearSpinner: clearSpinnerLocked,
			aiPhase:      &aiPhase,
		}

		// The engine owns the agent loop: request rounds with retry and
		// tools fallback, tool execution (bash -c in the shell's cwd via
		// the tool registry — never the shared pty, so output cannot
		// corrupt the shell screen), role:"tool" feedback, loop guards
		// and the wrap-up round. There is no round bound (architecture
		// §3.9): a stuck model is caught by repetition, read churn and L1
		// slimming. Events reach the screen and the session log through
		// the sink.
		// The approval mode is re-read per task from the snapshotted
		// config, so a config edit applies to the next run (§7.2).
		approval.SetMode(cfgNow.Agent.ApprovalMode())
		runCfg := agent.Config{
			Cwd:            cwd,
			BashTimeout:    cfgNow.Agent.BashTimeoutDur(),
			BashMaxTimeout: cfgNow.Agent.BashMaxTimeoutDur(),
			AutoBackground: cfgNow.Agent.AutoBackgroundDur(),
			Jobs:           jobMgr,
			Subs:           subMgr,
			Approval:       approval,
			ContextWindow:  spec.Model.ContextWindow,
		}
		// §16 manager model: the top-level run carries the delegation
		// tools. A worker's base is the lean twin of the manager's — cwd,
		// env, worker prompt; the brief (the task prompt) carries the
		// rest. A worker never sees the manager's history or the shell
		// timeline, and its transcript never crosses back: the manager
		// receives only compact status and the bounded report.
		subBase := []agent.TurnMsg{}
		if cwd != "" {
			subBase = append(subBase, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: "cwd: " + cwd}})
		}
		if env != "" {
			subBase = append(subBase, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: "env:\n" + env}})
		}
		subBase = append(subBase, agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: agent.WorkerInstructions}})
		snap := agent.SubagentSnapshot{Client: client, NativeTools: spec.ToolsEnabled(), Base: subBase, Cfg: runCfg}
		tools := agent.ManagerTools(cwd, agent.ToolOpts{
			BashTimeout:    cfgNow.Agent.BashTimeoutDur(),
			BashMaxTimeout: cfgNow.Agent.BashMaxTimeoutDur(),
			Jobs:           jobMgr,
			AutoBackground: cfgNow.Agent.AutoBackgroundDur(),
		}, subMgr, snap)
		res = agent.Run(ctx, agent.Input{
			Client:      client,
			NativeTools: spec.ToolsEnabled(),
			Tools:       tools,
			Base:        base,
			Prompt:      text,
			Interrupt:   toolIntr,
			Cfg:         runCfg,
		}, sink)

		if res.Err != nil {
			write(screen.DimGray() + "rysh: " + res.Err.Error() + screen.ColorReset + "\r\n")
			logWrite("noti", aiui.Sanitize("rysh: "+res.Err.Error()))
		}

		// Remember the whole turn (prompt, replies, tool results) so the
		// model can reference it on a later prompt. Partial exchanges are
		// kept too: an aborted task's context is still worth carrying.
		// After a compaction (M7.6) the checkpoint stands in for the
		// absorbed prefix — the prior history and this task's head — so
		// the in-memory history shrinks to the checkpoint (one user
		// message) plus the tail past the cut point. The disk log already
		// carries the compact event and stays the untouched source of
		// truth: a session reload rebuilds the full record from it.
		turn := make([]ctxMsg, len(res.Messages))
		now := time.Now()
		for i, tm := range res.Messages {
			turn[i] = ctxMsg{TurnMsg: tm, ts: now}
		}
		writeMu.Lock()
		if res.Compacted != nil {
			kept := turn
			if res.Compacted.Cut < len(kept) {
				kept = kept[res.Compacted.Cut:]
			} else {
				kept = nil
			}
			active.hist = append(active.hist[:0], ctxMsg{
				TurnMsg: agent.TurnMsg{Msg: agent.CheckpointMessage(res.Compacted.Checkpoint)},
				ts:      now,
			})
			active.hist = append(active.hist, kept...)
		} else {
			active.hist = append(active.hist, turn...)
		}
		active.hist = trimHistory(active.hist, maxHistory)
		writeMu.Unlock()
	}

	// pty master -> screen / session log. In shell mode output is forwarded
	// to the screen and the detector only decides whether a fullscreen
	// program currently owns the terminal, in which case recording pauses;
	// in AI mode output is deferred (buffered for the
	// return flush) while still being logged, so the session stream stays
	// complete. Complete lines are logged as shl in both modes. With the
	// prompt marker on, every chunk first passes the title filter, which
	// drops the shell's own OSC 0/2 title escapes (rysh owns the title)
	// and lets everything else — OSC 7 cwd reports included — through
	// untouched; nil otherwise, keeping the passthrough byte-exact.
	var titleStrip *titleFilter
	if markerOn {
		titleStrip = &titleFilter{}
	}
	out := make(chan struct{})
	go func() {
		defer close(out)
		buf := make([]byte, 4096)
		prevAlt := false
		for {
			n, err := p.Read(buf)
			if n > 0 {
				// The first child output ends the shell startup: clear the
				// 启动中 spinner on the row this output lands (a no-op when
				// enterAI already took the row over) and show the cursor
				// the startup arm hid, so the shell's prompt lands with a
				// visible cursor.
				if startupSpin {
					writeMu.Lock()
					startupSpin = false
					if clearSpinnerLocked() {
						os.Stdout.WriteString(screen.ShowCursor())
					}
					writeMu.Unlock()
				}
				chunk := buf[:n]
				// Detect the child shell enabling bracketed paste in its
				// own startup (bash/zsh readline print the mode-set when
				// they go raw); from then on, pastes reach the child wrapped
				// in 200~/201~ markers and the recorder can handle them.
				if !childPasteArmed.Load() && bytes.Contains(chunk, []byte("\x1b[?2004h")) {
					childPasteArmed.Store(true)
				}
				if titleStrip != nil {
					chunk = titleStrip.Filter(chunk)
				}
				shellOutAt.Store(time.Now().UnixNano())
				shellOutTap.write(chunk)
				if bytes.ContainsRune(chunk, '\n') {
					nlEpoch.Add(1)
				}
				det.Feed(chunk)
				alt := det.Alt()
				transitioned := alt != prevAlt
				if transitioned {
					if alt {
						// A fullscreen program owns the terminal: pause
						// recording so its keystrokes and screen repaints
						// are not captured as shell activity.
						rec.Pause()
					} else {
						// Back at the shell: resume recording. No capture is
						// open, so the program's exit repaint is not
						// attributed to any command.
						rec.Resume()
					}
					prevAlt = alt
				}
				// Capture terminal output into the open shell event (M4
				// unified event stream); no-op when no command is open. The
				// transition chunk itself is skipped: on enter it holds the
				// shell's own echo (already recorded as the command), on
				// exit the program's final repaint.
				if !alt && !transitioned {
					rec.Output(chunk)
				}
				// Track the shell's cwd from OSC 7 reports the shell emits
				// (cross-platform; /proc is the Linux fallback).
				cwdTracker.Feed(chunk)
				if st.Mode() == mode.AI {
					// Defer to the pending buffer (keep-head) and log the
					// complete lines; nothing touches the screen in AI mode.
					writeMu.Lock()
					fullscreenApp.Store(alt)
					if len(pendingShellOut) < maxPending {
						pendingShellOut = append(pendingShellOut, chunk...)
					} else {
						pendingOverflow = true
					}
					logLinesLocked(chunk)
					writeMu.Unlock()
					continue
				}
				// Forward the chunk in one critical section with the detector's
				// alt-screen state, so the driver (which takes writeMu to switch
				// modes) only ever observes the fullscreen state after it has
				// been stored.
				writeMu.Lock()
				os.Stdout.Write(chunk)
				logLinesLocked(chunk)
				fullscreenApp.Store(alt)
				writeMu.Unlock()
			}
			if err != nil {
				// The child closed the pty: stop the startup spinner (it
				// would otherwise keep animating past the exit).
				writeMu.Lock()
				startupSpin = false
				clearSpinnerLocked()
				writeMu.Unlock()
				return
			}
		}
	}()

	// Keyboard/input -> events. The driver loop routes each event to the
	// shell (shell mode) or into the AI editor (AI mode). keyCh is buffered
	// so the reader goroutine never drops input while the driver is busy.
	// The mode-switch binding (default Shift+Tab) is resolved once here.
	keyCh := make(chan keys.Event, 64)
	modeSwitch := keys.CtrlTabEncodings(cfg.Keys.ModeSwitch)
	go func() {
		rd := keys.NewReaderWith(stdinReader(), modeSwitch)
		for {
			ev, err := rd.Next()
			if err != nil {
				close(keyCh)
				return
			}
			keyCh <- ev
		}
	}()

	// Spinner ticker: while the inline pre-output spinner is armed — a
	// streaming task waiting for its first token, or the shell still
	// booting — advance its frame every 250ms so the wait is visible on
	// the line where output will land, counting up (1s, 2s, 1m56s). On
	// the main screen there is no status row to repaint, so nothing else
	// runs here.
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	go func() {
		for range tick.C {
			writeMu.Lock()
			if preOutput && (ai.Streaming() || startupSpin) {
				spinIdx++
				os.Stdout.WriteString(spinnerLineLocked())
			}
			writeMu.Unlock()
		}
	}()

	// Forward termination signals to the child. Keyboard ^C/^\ travel as
	// bytes through the pty and need no forwarding here.
	childExited := make(chan struct{})
	waitErr := new(error)
	// reap waits on exactly the command it was handed and reports into
	// exactly the slot it was handed. Both are passed by value on purpose:
	// a session switch replaces c, childExited and waitErr, and a goroutine
	// that read those variables instead would wait on the *next* shell and
	// close the *next* channel, so the switch would assume the old shell was
	// already reaped while it still held the pty and could steal the bytes
	// typed right after the switch.
	reap := func(cmd *pty.Cmd, done chan struct{}, slot *error) {
		*slot = cmd.Wait()
		close(done)
	}
	go reap(c, childExited, waitErr)

	fwd := make(chan os.Signal, 1)
	if sigs := forwardSignals(); len(sigs) > 0 {
		signal.Notify(fwd, sigs...)
		defer signal.Stop(fwd)
	}
	go forwardTermSignals(fwd, c, childExited)

	// switchToMeta switches the active session to meta: it validates that no
	// other live rysh holds the target, kills the current shell, restarts a
	// fresh shell on the same pty (go-pty needs a new *Cmd per Start; the
	// parent's slave fd stays open), re-attaches the wait and signal
	// forwarding, redraws the full screen, resets the per-session state, and
	// lands in AI mode (replaying the target history) or shell mode per
	// landInAI. It runs only on the mainloop goroutine — called from
	// submitAI (AI-mode /new /resume) and from the ctlTick poll (shell-mode
	// rysh new / rysh resume) — never from a task goroutine, so it cannot race
	// the mainloop's select. While a task streams the request is pended and
	// executed by the next ctlTick after finalize settles the stream.
	switchToMeta = func(meta *session.Meta, notice string, landInAI bool) {
		// Reject a target another live rysh holds, before touching anything.
		if store != nil {
			if pid, ok := attachedByOther(store, meta.ID, os.Getpid()); ok {
				writeMu.Lock()
				os.Stdout.WriteString(screen.DimGray() + uiT.Get("attached_session", meta.ID, pid, meta.ID) + screen.ColorReset + "\r\n")
				if st.Mode() == mode.AI {
					drawAIPromptLocked()
				}
				writeMu.Unlock()
				return
			}
		}
		// Commit the switch up front: this instance is now attached to meta,
		// so rysh ls / rysh resume (which compare against the current session id)
		// stay correct and a control-channel request for the session we just
		// left is no longer treated as a no-op self-target.
		active.meta = meta
		writeMu.Lock()
		if ai.Streaming() {
			// A task is streaming and only ^C can stop it; defer the switch
			// and let the next ctlTick execute it once finalize settles.
			if pendingSwitch == nil {
				pendingSwitch = &pendingSwitchT{req: &switchReq{meta: meta, notice: notice}, landInAI: landInAI}
			}
			writeMu.Unlock()
			return
		}
		writeMu.Unlock()

		logWrite("sys", "session:switch")
		// Take the old shell generation down with everything it forked, and
		// wait until it is really gone rather than merely signalled: a login
		// shell started through a launcher stub (Git for Windows runs
		// bin\sh.exe, which forks the real usr\bin\bash.exe) survives a kill
		// aimed at the stub alone, and the orphan keeps the terminal open and
		// reads whatever is typed next, answering as the session it was
		// started for. Bounded: a background job may still hold the pty open.
		if c.Process != nil {
			_, survivors := killShellTree(c.Process.Pid)
			if len(survivors) > 0 {
				logWrite("sys", fmt.Sprintf("session:shelltree survivors=%v", survivors))
			}
		}
		select {
		case <-childExited:
		case <-time.After(2 * time.Second):
		}

		// The SIGKILLed generation never restores the terminal: a shell
		// sitting at its prompt (line editor in raw mode) leaves the pty
		// without canonical mode or echo, and the replacement shell would
		// inherit that and swallow typed input. Put the pty back the way a
		// freshly opened one is before the new generation starts.
		if up, ok := p.(pty.UnixPty); ok {
			resetPtyTermios(up.Slave().Fd())
		}

		// Restart a fresh shell on the same pty and re-attach the wait and
		// signal-forwarding goroutines to it. The new generation gets its own
		// channel and its own error slot, and both reapers are handed the
		// values rather than the variables they could outlive.
		c2 := p.Command(sh, args...)
		c2.Env = shellEnv(meta.ID)
		startErr := c2.Start()
		c = c2
		childExited = make(chan struct{})
		waitErr = new(error)
		go reap(c2, childExited, waitErr)
		go forwardTermSignals(fwd, c2, childExited)
		if startErr != nil {
			writeMu.Lock()
			os.Stdout.WriteString(screen.DimGray() + "rysh: start " + sh + ": " + startErr.Error() + screen.ColorReset + "\r\n")
			writeMu.Unlock()
		}
		// Track the new session in the in-memory state: rebuild the
		// conversation history and point the open log at the new session so
		// AI turns after the switch land in the right log, and rewrite the
		// attachment record so rysh ls no longer reports the old session as
		// attached here.
		active.hist = readSessionHistory(meta.ID)
		ai.SetHistory(userInputsFor(meta.ID))
		activeLogMu.Lock()
		active.log = openLog(meta.ID)
		activeLogMu.Unlock()
		if attachPath != "" {
			writeSessionAttach(attachPath, meta.ID)
		}
		// The landing switch is user activity on the target session: record
		// it in the updated ledger so `rysh ls` orders the session that was
		// just switched to most recently. The one-shot `rysh new` writes its
		// own row before handing the switch over; `rysh resume` does not, so
		// without this the ledger would still name the session we left.
		if store != nil {
			_ = store.TouchUpdated(meta.ID)
		}

		// Per-session state reset: the separator and the tail replay below
		// continue the stream where the dead shell left the cursor.
		writeMu.Lock()
		// Both modes: print a separator and replay the unified timeline so
		// the terminal scrollback carries the session's conversation.
		dir := ""
		if store != nil {
			dir = store.SessionsDir()
		}
		count := sessionMessageCount(dir, meta.ID)
		os.Stdout.WriteString(replaySeparator(meta, count))
		if replay := renderUnifiedReplay(dir, meta.ID, replayTailLines); replay != "" {
			os.Stdout.WriteString(strings.ReplaceAll(replay, "\n", "\r\n"))
		}
		if notice != "" {
			os.Stdout.WriteString(screen.DimGray() + notice + screen.ColorReset + "\r\n")
			logWrite("noti", aiui.Sanitize(notice))
		}
		// The title carries the session id: follow the switch.
		if markerOn {
			os.Stdout.WriteString(screen.SetTitle(ryshTitle(modelRef, active.meta.ID)))
		}
		// The notice is part of the streamed history, so it is printed before
		// the AI input line is drawn: the input line must always be the last
		// thing on the screen, or the user types below a prompt that is no
		// longer where the cursor is.
		if landInAI {
			st.ToAI()
			os.Stdout.WriteString("\r" + screen.EraseToEOL())
			drawAIPromptLocked()
		} else {
			st.ToShell()
		}
		logWrite("sys", "session:switch:done")
		writeMu.Unlock()
	}

	// ctlTick polls the per-instance control channel every 250ms for switch
	// requests written by rysh subprocesses (rysh new / rysh resume run from
	// the shell), and executes a switch pended while a task was streaming.
	ctlTick := time.NewTicker(250 * time.Millisecond)
	defer ctlTick.Stop()

	// Direct startup in AI mode (rysh ai without a message): hand the
	// stream to the AI editor immediately. The shell's own prompt is
	// deferred like any other AI-mode output and flushed on return.
	if startInAI {
		st.ToAI()
		enterAI()
	}

	// Driver loop. In shell mode every key is routed through the mode state
	// machine and forwarded to the pty; a mode change (the mode-switch key,
	// default Shift+Tab, or a space at a fresh prompt) hands the stream to
	// the AI editor. In AI mode keys
	// go into the persistent editor; a submitted prompt streams in place,
	// and while a task runs mode switching is locked.
mainloop:
	for {
		select {
		case ev, ok := <-keyCh:
			if !ok {
				// stdin EOF: return to shell mode, send EOT so the shell
				// sees a clean EOF too, and wait for it to exit.
				if st.Mode() == mode.AI {
					leaveAI()
				}
				_, _ = p.Write([]byte{4})
				keyCh = nil // stop selecting on the closed channel
				continue
			}
			if st.Mode() == mode.AI {
				writeMu.Lock()
				act := ai.HandleKey(ev)
				if ai.Streaming() && ev.Kind == keys.CtrlTab {
					os.Stdout.WriteString(screen.DimGray() + uiT.Get("switch_blocked_task") + screen.ColorReset + "\r\n")
				} else if act == aiui.ActionNone && ai.Dirty() {
					// Redraw only when the committed draft or cursor moved:
					// keys the IME forwards while composing often change
					// nothing, and an erase/rewrite on those would detach
					// the candidate window from the cursor.
					ai.RenderInput(func(s string) { os.Stdout.WriteString(s) }, width)
				}
				writeMu.Unlock()
				switch act {
				case aiui.ActionSubmit:
					writeMu.Lock()
					text := ai.Submit(func(s string) { os.Stdout.WriteString(s) }, width)
					os.Stdout.WriteString("\r\n")
					writeMu.Unlock()
					if submitAI(text) {
						// /quit or /exit: settle any in-flight task, then
						// end the child shell. The cancel is taken under
						// writeMu but awaited outside it, so the stream's
						// finalize (which takes writeMu) cannot deadlock.
						writeMu.Lock()
						cancel := cancelStream
						done := streamDone
						cancelStream = nil
						streamDone = nil
						writeMu.Unlock()
						if cancel != nil {
							cancel()
						}
						if done != nil {
							<-done
						}
						quitFlag.Store(true)
						logWrite("sys", "session:quit")
						writeMu.Lock()
						os.Stdout.WriteString("bye\r\n")
						writeMu.Unlock()
						// End the child shell: EOF exits an idle shell
						// cleanly. Interactive shells ignore SIGTERM at
						// the prompt, so a shell still running a command
						// (or holding its breath) is killed outright —
						// along with everything it forked, which would
						// otherwise outlive rysh and keep the terminal
						// open as an orphan.
						_, _ = p.Write([]byte{4})
						select {
						case <-childExited:
						case <-time.After(300 * time.Millisecond):
							if c.Process != nil {
								killShellTree(c.Process.Pid)
							}
							select {
							case <-childExited:
							case <-time.After(2 * time.Second):
							}
						}
						break mainloop
					}
				case aiui.ActionCancel:
					// Two-phase ^C (§8.3): while a foreground command
					// executes, ^C interrupts only that command — the run
					// continues and the model decides the next step. While
					// worker subagents run (§16), ^C stops the subagents —
					// the manager keeps going and sees the stopped state
					// on its next task_output. In the remaining phases ^C
					// cancels the whole task (the M4 semantics). An open
					// approval prompt counts as a streaming phase: nothing
					// is executing, so the pending call is denied and the
					// whole task stops.
					writeMu.Lock()
					exec := aiPhase == "exec"
					ch := pendingAsk
					writeMu.Unlock()
					if ch != nil {
						ch <- agent.AnswerDeny
						cancelAndWait()
					} else if exec && toolIntr.Fire() {
						logWrite("sys", "task:tool-interrupt")
						noticeLine(uiT.Get("interrupted_fg"))
					} else if n := subMgr.KillRunning(); n > 0 {
						logWrite("sys", fmt.Sprintf("task:subagent-interrupt ×%d", n))
						noticeLine(uiT.Get("stopped_subtasks", n))
					} else {
						cancelAndWait()
					}
				case aiui.ActionApproveYes, aiui.ActionApproveNo, aiui.ActionApproveAlways:
					// §7.3: the y/n/a answer for the open approval
					// prompt, routed to the engine goroutine blocked in
					// askApproval. A stray key whose prompt was already
					// answered (or cancelled) is dropped.
					writeMu.Lock()
					ch := pendingAsk
					writeMu.Unlock()
					if ch == nil {
						continue
					}
					ans := agent.AnswerAllow
					switch act {
					case aiui.ActionApproveNo:
						ans = agent.AnswerDeny
					case aiui.ActionApproveAlways:
						ans = agent.AnswerAlways
					}
					ch <- ans
				case aiui.ActionLeave:
					leaveAI()
				}
				continue
			}
			// Shell mode. fwdSinceNewline is true while the user has typed
			// since the shell's last newline, so a space is forwarded, not
			// a mode switch.
			fwdSinceNewline := fwdEpoch.Load() == nlEpoch.Load()
			fs := fullscreenApp.Load()
			// fg is live: the shell owns the pty's foreground process
			// group, i.e. the user's keys reach the shell, not a program
			// the shell launched (vi, ssh, less, pipelines). It gates the
			// mode-switch gestures and keystroke recording; the
			// alternate-screen flag alone would miss foreground programs
			// that never enter the alternate screen.
			fg := !fs && shellInForeground(p.Fd(), c.Process.Pid)
			isSwitch := ev.Kind == keys.CtrlTab || (ev.Kind == keys.Rune && ev.R == ' ' && !fwdSinceNewline && !rec.InPaste())
			// On Windows the foreground process group is not detectable, so fg
			// falls back to "not in the alternate screen". There a fresh prompt
			// (fwdSinceNewline false) always allows the switch so a stale
			// alternate-screen reading can never strand the user; a line the
			// user has already typed keeps the gate so typed spaces forward
			// normally.
			if foregroundBlocksSwitch(isSwitch, fg, fwdSinceNewline, runtime.GOOS == "windows") {
				noticeLine(uiT.Get("switch_blocked_fg"))
				continue
			}
			// Feed activity to the recorder before forwarding, so output
			// capture opens before the shell runs the submitted command.
			// While a foreground child owns the terminal (!fg), the
			// keystrokes go to it instead of the shell's line editor, so
			// they are not recorded.
			res := st.Handle(ev, fwdSinceNewline || rec.InPaste())
			if res.ModeChanged {
				enterAI()
				continue
			}
			if len(res.Forward) > 0 {
				_, _ = p.Write(res.Forward)
				if ev.Kind == keys.Enter {
					// The line was submitted, so the next prompt is a
					// fresh one: clear the "typed since newline" epoch.
					// Without this, a command whose output lacks a
					// trailing newline leaves fwdEpoch == nlEpoch and the
					// next prompt is misread as non-fresh — which both
					// forwards the leading space (no switch) and, on
					// Windows, keeps the foreground gate shut so Shift+Tab
					// is blocked too.
					fwdEpoch.Store(-1)
				} else {
					fwdEpoch.Store(nlEpoch.Load())
				}
			}
			if fg {
				switch ev.Kind {
				case keys.Rune:
					if rec.InPaste() && childPasteArmed.Load() {
						// Inside a bracketed-paste block: the content (and
						// its embedded newlines) is inserted as one block,
						// not typed key by key.
						rec.PasteText(string(ev.R))
					} else {
						rec.Type(ev.R)
					}
				case keys.PasteStart:
					if childPasteArmed.Load() {
						rec.PasteStart()
					}
				case keys.PasteEnd:
					if childPasteArmed.Load() {
						rec.PasteEnd()
					}
				case keys.Backspace:
					rec.Backspace()
				case keys.ArrowLeft:
					rec.Left()
				case keys.ArrowRight:
					rec.Right()
				case keys.Home:
					rec.CursorStart()
				case keys.End:
					rec.CursorEnd()
				case keys.ArrowUp, keys.ArrowDown, keys.Tab:
					// History recall (↑/↓) and tab completion rewrite the
					// line with text the recorder never sees; flag the line
					// so Enter does not log a reconstruction.
					rec.MarkUnreliable()
				case keys.PageUp, keys.PageDown, keys.Esc:
					// History scroll, Alt bindings (Esc+key) and other
					// escape sequences the recorder cannot reconstruct.
					rec.MarkUnreliable()
				case keys.Enter:
					if rec.InPaste() && childPasteArmed.Load() {
						// An embedded newline inside a paste block is
						// literal content (readline holds it in the line
						// buffer), not a submit.
						rec.PasteText("\n")
						break
					}
					cmd := rec.Enter(currentCWD(c.Process.Pid))
					if cmd != "" {
						writeMu.Lock()
						logWrite("shk", aiui.Sanitize(cmd))
						if store != nil {
							_ = store.TouchUpdated(active.meta.ID)
						}
						writeMu.Unlock()
					}
				case keys.Other:
					if rec.Control(ev.Raw) && !rec.InPaste() {
						// An unhandled control binding (^R search, ^Y
						// yank, ...) edited the line in a way the recorder
						// cannot see.
						rec.MarkUnreliable()
					}
				}
			}

		case <-ctlTick.C:
			// Execute a switch pended while a task was streaming, now that
			// the stream has settled.
			writeMu.Lock()
			var ps *pendingSwitchT
			if pendingSwitch != nil && !ai.Streaming() {
				ps = pendingSwitch
				pendingSwitch = nil
			}
			writeMu.Unlock()
			if ps != nil {
				switchToMeta(ps.req.meta, ps.req.notice, ps.landInAI)
				continue
			}
			// Poll the control channel for switch requests written by rysh
			// subprocesses (rysh new / rysh resume run from the shell). Each
			// request is validated before switching.
			if ctlPath != "" {
				for _, req := range readControlRequests(ctlPath) {
					if req.id == active.meta.ID {
						continue
					}
					meta, err := resolveSession(store, req.id)
					if err != nil {
						writeMu.Lock()
						os.Stdout.WriteString(screen.DimGray() + "rysh: " + err.Error() + screen.ColorReset + "\r\n")
						writeMu.Unlock()
						continue
					}
					if pid, ok := attachedByOther(store, meta.ID, os.Getpid()); ok {
						writeMu.Lock()
						os.Stdout.WriteString(screen.DimGray() + uiT.Get("attached_session", meta.ID, pid, meta.ID) + screen.ColorReset + "\r\n")
						writeMu.Unlock()
						continue
					}
					switchToMeta(meta, req.notice, st.Mode() == mode.AI)
				}
			}

		case <-childExited:
			// The shell is gone; leave AI mode if needed so the stream is
			// settled, then exit the loop.
			if st.Mode() == mode.AI {
				leaveAI()
			}
			break mainloop
		}
	}

	if *waitErr != nil && c.ProcessState == nil {
		fmt.Fprintf(os.Stderr, "rysh: wait: %v\n", *waitErr)
		return 1
	}

	// Worker subagents and background jobs die with rysh: the subagents'
	// runs cancel first (§16), then their process groups and any shared
	// jobs are killed so no stray children outlive the session (§8.2).
	subMgr.StopAll()
	jobMgr.StopAll()

	// Close the pty slave so the master reaches EOF and the output reader
	// stops even if a background process still holds the slave open (the
	// slave is kept open for the whole run so shell respawns on a session
	// switch share the same pty).
	if up, ok := p.(pty.UnixPty); ok {
		_ = up.Slave().Close()
	}

	// Drain remaining output; bounded so rysh still exits if a background
	// process keeps the pty master busy.
	select {
	case <-out:
	case <-time.After(time.Second):
	}

	// Hand the terminal back in a clean state without clearing it: rysh ran
	// on the main screen, so its output stays in the scrollback and the
	// user's shell prompt simply follows on the next line. We also leave any
	// alternate screen a child may have been left in (e.g. a fullscreen
	// program killed mid-run never emits the exit sequence itself), reset
	// colors/cursor style/visibility, and add a trailing newline so the
	// prompt lands on a fresh line rather than under a mid-line cursor. The
	// window title goes back to what the probe read at startup, when it could
	// read one.
	writeMu.Lock()
	if markerOn && prevTitle != "" {
		os.Stdout.WriteString(screen.SetTitle(prevTitle))
	}
	// Leave the alternate screen only if a fullscreen child actually left us
	// in it: rysh itself runs on the main screen and never enters ?1049h, so
	// sending ?1049l unconditionally would swap the parent console back to a
	// blank saved buffer and garble the screen on exit.
	if det.Alt() {
		os.Stdout.WriteString("\x1b[?1049l")
	}
	os.Stdout.WriteString("\r\n" + screen.Reset())
	writeMu.Unlock()

	if c.ProcessState != nil {
		// /quit terminates the child on our behalf; report a clean exit
		// instead of the child's signal-death status.
		if quitFlag.Load() {
			return 0
		}
		return exitCodeOf(c.ProcessState)
	}
	return 0
}

func main() {
	os.Exit(ryshMain())
}

// ryshMain is main's testable body. It dispatches on the first argument:
// the pty-creating interactive forms (bare rysh, rysh ai without a message,
// rysh new, rysh resume) only run at top level (RYSH_INSIDE unset); inside a
// running rysh the same commands are one-shot subprocesses that never spawn
// a second interactive rysh — rysh new / rysh resume switch the current
// instance via its control channel, and everything else prints and exits.
func ryshMain() int {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	inside := os.Getenv(insideEnv) != ""
	// One-shot subcommands do not read ~/.rysh/config.toml, so the UI
	// language resolves from the environment alone.
	uiT = i18n.New(i18n.Resolve("", os.Getenv("LANG")))

	switch cmd {
	case "-v", "--version", "-version":
		fmt.Println("rysh", version)
		return 0
	case "ai":
		if len(args) < 2 {
			if inside {
				fmt.Fprintln(os.Stderr, uiT.Get("already_inside_ai"))
				fmt.Fprintln(os.Stderr, subcommandUsage())
				return 1
			}
			return run("", true)
		}
		return chatOnce(strings.Join(args[1:], " "))
	case "new":
		return newSubcommand(inside)
	case "resume":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, uiT.Get("usage_resume"))
			fmt.Fprintln(os.Stderr, subcommandUsage())
			return 1
		}
		return resumeSubcommand(inside, args[1])
	case "ls":
		page := 1
		if len(args) > 1 {
			if n, err := strconv.Atoi(args[1]); err == nil {
				page = n
			}
		}
		store := sessionStore()
		attached := map[string]int{}
		if store != nil {
			attached = scanAttached(store.CtlDir(), selfPID())
		}
		for _, ln := range renderSessionList(store, os.Getenv(sessionEnv), page, attached) {
			fmt.Println(ln)
		}
		return 0
	case "kill":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, uiT.Get("usage_kill"))
			fmt.Fprintln(os.Stderr, subcommandUsage())
			return 1
		}
		return killSubcommand(args[1])
	case "":
		if inside {
			fmt.Fprintln(os.Stderr, uiT.Get("already_inside"))
			fmt.Fprintln(os.Stderr, subcommandUsage())
			return 1
		}
		return run("", false)
	default:
		fmt.Fprintln(os.Stderr, uiT.Get("unknown_subcommand", cmd))
		fmt.Fprintln(os.Stderr, subcommandUsage())
		return 1
	}
}

// ryshTitle is the session identity string: the startup banner line and,
// while the prompt marker is on, the terminal window title.
func ryshTitle(model, session string) string {
	t := "rysh"
	if model != "" {
		t += " · " + model
	}
	return t + " · session " + session
}

// modelListLines formats the numbered model list shown by /model with no
// argument and printed on Tab after /model: every configured model numbered
// so /model <N> can pick it, the active model marked with a leading >, the
// session list's way.
func modelListLines(cfg *config.Config, cur, path string) []string {
	refs := provider.List(cfg)
	if len(refs) == 0 {
		return []string{"no models configured (edit " + path + ")"}
	}
	lines := []string{uiT.Get("models_available")}
	for i, ref := range refs {
		marker := " "
		if ref == cur {
			marker = ">"
		}
		lines = append(lines, fmt.Sprintf("%s %d. %s", marker, i+1, ref))
	}
	return lines
}

// modeSwitchLabel renders a configured mode_switch binding as a display
// name for the startup hint line; the keys package only knows the byte
// sequences, not labels. Unset/unknown names mean the default Shift+Tab.
func modeSwitchLabel(name string) string {
	switch name {
	case "ctrl-tab":
		return "Ctrl+Tab"
	case "ctrl-space":
		return "Ctrl+Space"
	case "ctrl-backslash":
		return "Ctrl+\\"
	default:
		return "Shift+Tab"
	}
}

// foregroundBlocksSwitch reports whether a mode-switch gesture must be
// suppressed because a foreground program currently owns the terminal.
//
// isSwitch is true for the mode-switch gesture (the mode-switch key, or a
// space at a fresh prompt). fg is true when the shell — not a child it
// launched — owns the keyboard. fwdSinceNewline is true once the user has
// typed on the current line. On Windows the foreground process group is not
// detectable, so fg falls back to "not in the alternate screen"; there the
// switch is always allowed at a fresh prompt (fwdSinceNewline false) so a
// stale alternate-screen reading can never strand the user, but a line the
// user has already typed keeps the gate so typed spaces forward normally.
func foregroundBlocksSwitch(isSwitch, fg, fwdSinceNewline, windows bool) bool {
	if !isSwitch {
		return false
	}
	blocked := !fg
	if windows && !fwdSinceNewline {
		blocked = false
	}
	return blocked
}

// childEnv builds the environment for a child shell (or one-shot rysh
// subprocess): the current environment with any pre-existing RYSH_INSIDE /
// RYSH_SESSION_ID / RYSH_CTL / RYSH_PID entries removed, then a single
// marker for each that applies. RYSH_INSIDE tells a nested rysh it must not
// create a pty; RYSH_SESSION_ID names the session the shell's rysh
// subprocesses act on; RYSH_CTL is the per-instance control channel a
// subprocess writes switch requests to; RYSH_PID is this instance's pid so
// a subprocess can tell its own attachment record apart from another rysh's.
func childEnv(sessionID, ctlPath string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, insideEnv+"=") ||
			strings.HasPrefix(kv, sessionEnv+"=") ||
			strings.HasPrefix(kv, ctlEnv+"=") ||
			strings.HasPrefix(kv, pidEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, insideEnv+"=1")
	if sessionID != "" {
		env = append(env, sessionEnv+"="+sessionID)
	}
	if ctlPath != "" {
		env = append(env, ctlEnv+"="+ctlPath)
	}
	env = append(env, pidEnv+"="+strconv.Itoa(os.Getpid()))
	return env
}

// version is the build version, overridable with -ldflags "-X main.version=...".
var version = "dev"

// maxHistory caps the conversation history sent to the model, in messages
// (user + assistant + tool results). Oldest turns are trimmed first. The
// cap is generous because agentic turns are message-dense — every engine
// step adds an assistant and a tool message — while byte volume stays
// bounded by the L1 collapse of old tool results.
const maxHistory = 120

// resyncPromptWait caps how long leaveAI waits for the shell to redraw its
// prompt after the resync, before re-injecting the saved residual, so the
// residual is echoed after (not before) the new prompt. The wait usually ends
// early, as soon as the repaint has come and gone.
const resyncPromptWait = 250 * time.Millisecond

// resyncOutputIdle is the quiet period in child pty output that counts as
// "the repaint is over".
const resyncOutputIdle = 25 * time.Millisecond

// waitShellOutputSettled returns once the child pty wrote something after
// start and then stayed quiet for idle, or once limit has elapsed. It keeps the
// re-injected line from being echoed into the middle of a prompt repaint; the
// limit bounds a mode switch with a shell that never repaints at all. Whether
// the line reached the shell is settled separately, by its echo (injectResidual).
func waitShellOutputSettled(at *atomic.Int64, start time.Time, idle, limit time.Duration) {
	deadline := start.Add(limit)
	startNs := start.UnixNano()
	for {
		if last := at.Load(); last > startNs && time.Since(time.Unix(0, last)) >= idle {
			return // the repaint landed and has finished
		}
		if !time.Now().Before(deadline) {
			return // bounded: a shell that never repaints must not stall the switch
		}
		time.Sleep(outputPollInterval)
	}
}

// outputPollInterval is how often waitShellOutputSettled re-checks the child
// pty's last-output timestamp.
const outputPollInterval = 2 * time.Millisecond

// resizeRepaintWait is the pause between the two pty resizes in leaveAI's
// line-editor resync. It must be long enough for the shell to process the
// first SIGWINCH and repaint its prompt, but it is imperceptible as part of
// a mode switch.
const resizeRepaintWait = 40 * time.Millisecond

// lineEditorShell reports whether the login shell repaints its prompt in
// place when its controlling terminal is resized. Shells with a line editor
// (bash, zsh, fish, ksh, mksh) do; plain shells such as dash do not, so
// leaveAI uses a resize-triggered repaint for the former and a carriage
// return for the latter. The same binaries are named bash.exe on Windows, so
// the suffix is trimmed before comparing (an extensionless sh stays a plain
// shell there too).
func lineEditorShell(path string) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
	switch name {
	case "bash", "zsh", "fish", "ksh", "mksh":
		return true
	}
	return false
}

// isPowershell reports whether the shell binary is PowerShell (pwsh or
// Windows PowerShell).
func isPowershell(path string) bool {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".exe"))
	return name == "pwsh" || name == "powershell"
}

// isCmd reports whether the shell binary is cmd.exe.
func isCmd(path string) bool {
	return strings.EqualFold(filepath.Base(path), "cmd.exe") || strings.EqualFold(filepath.Base(path), "cmd")
}

// ctxMsg is one AI-mode message with the time it was created, so shell
// events and AI turns can be merged into one chronological timeline at
// request time (unified event stream). The embedded agent.TurnMsg carries
// the wire message plus, on tool results, the metadata the L1 collapse
// needs.
type ctxMsg struct {
	agent.TurnMsg
	ts time.Time
}

// chatMessages drops the timestamps, for the wire request.
func chatMessages(cs []ctxMsg) []provider.ChatMessage {
	out := make([]provider.ChatMessage, len(cs))
	for i := range cs {
		out[i] = cs[i].Msg
	}
	return out
}

// timelineItem is one element of the merged shell/AI context stream: either
// a shell event (rendered as its own system message), a shell-fold summary
// (the one-line placeholders for older events, emitted first), or an
// AI-mode message.
type timelineItem struct {
	ev     *session.ShellEvent
	msg    *ctxMsg
	folded string // shell-event L1 summary, "" when this is not the summary slot
}

// buildTimeline merges the shell events and the AI-mode history into one
// chronological stream, oldest first, so commands the user ran between AI
// turns sit between those turns instead of in a lump at the front. On equal
// timestamps the shell event goes first (a command was entered before the
// turn started). Both inputs are in chronological order. folded is the
// one-line summary of the shell events older than verbatim (L1 slimming);
// it is emitted as a single leading item so it sits before everything, in
// the place those commands happened.
func buildTimeline(events []session.ShellEvent, hist []ctxMsg, folded string) []timelineItem {
	items := make([]timelineItem, 0, len(events)+len(hist)+1)
	if folded != "" {
		items = append(items, timelineItem{folded: folded})
	}
	i, j := 0, 0
	for i < len(events) || j < len(hist) {
		if i < len(events) && (j >= len(hist) || !events[i].Time.After(hist[j].ts)) {
			items = append(items, timelineItem{ev: &events[i]})
			i++
		} else {
			items = append(items, timelineItem{msg: &hist[j]})
			j++
		}
	}
	return items
}

// foldShellEvents applies L1 slimming to the shell ring (the shell-event
// twin of agent.CollapseOldTools): the newest events whose formatted total
// fits the per-request budget (session.MaxContext) stay verbatim — the very
// newest always does — and every older event folds to a one-line placeholder
// (ShellEvent.Fold), so an earlier command is never silently dropped, only
// summarized. It returns the verbatim suffix (chronological, for the
// timeline) and the folded summary of the older prefix ("" when none).
func foldShellEvents(events []session.ShellEvent) ([]session.ShellEvent, string) {
	n := len(events)
	if n == 0 {
		return nil, ""
	}
	keep := 0 // index of the oldest verbatim event; 0 when the whole ring fits
	total := 0
	for i := n - 1; i >= 0; i-- {
		size := len(events[i].Format())
		if i < n-1 && total+size > session.MaxContext {
			keep = i + 1
			break
		}
		total += size
	}
	verbatim := events[keep:]
	if keep == 0 {
		return verbatim, ""
	}
	lines := make([]string, 0, keep)
	for i := 0; i < keep; i++ {
		lines = append(lines, events[i].Fold())
	}
	return verbatim, strings.Join(lines, "\n")
}

// trimHistory keeps only the most recent max messages, trimming whole turns
// from the front so the retained history stays coherent. A turn is a user
// message, the assistant reply it produced, and any tool-result messages
// that follow (the engine feeds results back as role:"tool"); trimming
// removes all of them together.
func trimHistory(h []ctxMsg, max int) []ctxMsg {
	for len(h) > max {
		// Drop everything up to (but excluding) the next user message,
		// i.e. one full turn. Leading system messages are skipped.
		i := 0
		for i < len(h) && h[i].Msg.Role != "user" {
			i++
		}
		if i >= len(h) {
			break
		}
		j := i + 1
		for j < len(h) && h[j].Msg.Role != "user" {
			j++
		}
		if j <= i {
			break
		}
		h = h[j:]
	}
	return h
}

// streamSink adapts the agent engine's events to the interactive TUI: it
// renders streamed tokens through the shared markdown writer, prints tool
// observation lines, and mirrors notices into the session log. The func
// fields are the ryshMain closures over the driver's locked state; every
// method takes writeMu itself, so the engine may call it from the task
// goroutine while the driver holds the lock elsewhere.
type streamSink struct {
	writeMu  *sync.Mutex
	md       *markdown.Renderer
	logWrite func(kind, payload string)
	// armSpinner/clearSpinner require writeMu to be held by the caller (the
	// *Locked closures from ryshMain). armSpinner's label is "" to keep the
	// current caption.
	armSpinner   func(label string)
	clearSpinner func() bool
	aiPhase      *string

	prevPhase string // phase of the last emitted token, sink-local
}

// OnText streams one token: reasoning dim on its own phase row, content
// through the markdown writer. A phase change gets its own row break even
// mid-stream; consecutive tokens of one phase keep flowing on the same
// row. The screen gets the styled stream; the log keeps the raw text.
// Reasoning content is rendered through the markdown renderer but de-emphasized
// with dim+italic so it reads as "thinking" rather than the final answer.
func (s *streamSink) OnText(delta string, reasoning bool) {
	phase := "writing"
	if reasoning {
		phase = "thinking"
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.clearSpinner() || s.prevPhase != phase {
		os.Stdout.WriteString("\r\n")
	}
	s.prevPhase = phase
	*s.aiPhase = phase
	if reasoning {
		// Render reasoning through the markdown renderer, then wrap with
		// dim+italic for de-emphasis: the thinking reads as secondary text.
		styled := s.md.Write(delta)
		os.Stdout.WriteString(screen.DimItalic() + strings.ReplaceAll(styled, "\n", "\r\n") + screen.ColorReset)
		s.logWrite("rea", session.SanitizeStream(delta))
		return
	}
	os.Stdout.WriteString(strings.ReplaceAll(s.md.Write(delta), "\n", "\r\n"))
	s.logWrite("asw", session.SanitizeStream(delta))
}

// OnToolBegin brackets tool execution on screen: while the tool runs the
// inline spinner carries the 执行中 caption so a slow execution (the
// approved command, a job_output or task_output wait) shows a live line
// instead of a dead stream. The spinner owns the row the [tool] line will
// take (the M4 observation-line shape). It flips the status phase to "exec"
// so the driver's ^C handler knows a foreground command owns the interrupt
// (§8.3). The approval gate builds here with M7.4.
func (s *streamSink) OnToolBegin(call provider.ToolCall) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	*s.aiPhase = "exec"
	if !s.clearSpinner() {
		// No spinner owns the row: a streamed reply may have landed without
		// a trailing newline (cursor mid-line), so break to a fresh row
		// before arming — the same rule the approval bar follows, and the
		// reason a bare EraseToEOL would not eat the reply's last line.
		os.Stdout.WriteString("\r\n")
	}
	s.armSpinner(uiT.Get("spinner_executing"))
}

// OnToolEnd prints the dim observation line on the spinner's row (clearing
// it) and logs the fed-back result (the same text the model sees, minus
// guard warnings that are appended after this call).
func (s *streamSink) OnToolEnd(call provider.ToolCall, res agent.Result) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	*s.aiPhase = ""
	s.clearSpinner()
	if res.Display == "" {
		return
	}
	os.Stdout.WriteString(screen.DimGray() + "[tool] " + res.Display + screen.ColorReset + "\r\n")
	s.logWrite("tool", agent.FedContent(call.Name, res))
}

// OnTodo prints the model-maintained task list progress line. It lands
// mid todo-tool execution, so like OnNotice it takes the spinner's row and
// re-arms it when a spinner was up.
func (s *streamSink) OnTodo(todos []agent.Todo) {
	done := 0
	cur := ""
	for _, td := range todos {
		if td.Status == "completed" {
			done++
		}
		if td.Status == "in_progress" && cur == "" {
			cur = td.Content
		}
	}
	if cur == "" && len(todos) > 0 {
		cur = todos[0].Content
	}
	line := uiT.Get("todo_status", done, len(todos), cur)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wasSpinning := s.clearSpinner()
	os.Stdout.WriteString(screen.DimGray() + line + screen.ColorReset + "\r\n")
	s.logWrite("noti", aiui.Sanitize(line))
	if wasSpinning {
		s.armSpinner("")
	}
}

// OnStep re-arms the inline pre-output spinner for the new request round
// (the wait for that round's first token), mirroring the per-round arming
// the inline loop used to do; the caption is reset to 正在思考, so a
// spinner from the previous tool (执行中) never leaks into the model wait.
func (s *streamSink) OnStep(n int) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.armSpinner(uiT.Get("spinner_thinking"))
}

// OnNotice prints an out-of-band event line (wrap-up banner, tools
// fallback, subagent header/terminal line, the subagent-wait banner) and
// logs it. A spinner may be armed when the notice lands (subagent header
// mid tool call, the wait banner after a call-only round, a terminal line
// between rounds), so the line takes the spinner's row and the spinner
// re-arms below it — the ticker must never repaint over a notice. When no
// spinner is armed the line just prints, as before.
func (s *streamSink) OnNotice(text string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wasSpinning := s.clearSpinner()
	os.Stdout.WriteString(screen.DimGray() + text + screen.ColorReset + "\r\n")
	s.logWrite("noti", aiui.Sanitize(text))
	if wasSpinning {
		s.armSpinner("")
	}
}

// OnStatus reports transient request state (retry countdowns). The bottom
// status row it once fed is gone on the main screen, so it is a no-op now.
func (s *streamSink) OnStatus(text string) {}

// OnCompact logs the finished compaction's checkpoint full text into the
// session log (compact kind); the screen only saw the engine's notice
// lines. The log record is forensic — reconstructHistory ignores the
// compact kind, so the checkpoint never re-enters a rebuilt history on
// its own.
func (s *streamSink) OnCompact(text string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.logWrite("compact", aiui.Sanitize(text))
}

// subSink renders one worker subagent's events on the shared stream
// (§16): the markdown text flows plain (the ─── task N ─── header and the
// [task N] tool lines carry the identity), and every record lands under a
// s* kind — reconstructHistory rebuilds the manager's context from
// usr/asw/tool only, so a worker's transcript never re-enters the
// manager's history after a session switch.
type subSink struct {
	id       int
	writeMu  *sync.Mutex
	md       *markdown.Renderer
	logWrite func(kind, payload string)
}

// Close flushes the worker's held markdown lines above the terminal line
// (called by the subagent manager at run end).
func (s *subSink) Close() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if held := s.md.Close(); held != "" {
		os.Stdout.WriteString(strings.ReplaceAll(held, "\n", "\r\n"))
	}
}

// OnText is intentionally a no-op: the subagent's reasoning and streaming
// text are internal to the worker — the manager surface is what the user
// sees (the task_output tool results and the manager's final answer). The
// text is still logged under srea/sasw for session reconstruction.
func (s *subSink) OnText(delta string, reasoning bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if reasoning {
		// consume the markdown renderer but do not display
		s.md.Write(delta)
		s.logWrite("srea", session.SanitizeStream(delta))
		return
	}
	s.md.Write(delta)
	s.logWrite("sasw", session.SanitizeStream(delta))
}

func (s *subSink) OnToolBegin(call provider.ToolCall) {}

// OnToolEnd is a no-op: individual tool executions are internal to the
// worker subagent. The manager surface (task_output + manager's OnToolEnd)
// is what the user sees.
func (s *subSink) OnToolEnd(call provider.ToolCall, res agent.Result) {
	// intentionally no-op
}

// OnTodo is a no-op: todo progress is internal to the worker subagent.
// The manager sees the summary via task_output.
func (s *subSink) OnTodo(todos []agent.Todo) {
	// intentionally no-op
}

func (s *subSink) OnStep(n int) {}

// OnNotice carries the lifecycle lines (header and terminal line, plus
// the engine's wrap-up banner) and logs them as sub records.
func (s *subSink) OnNotice(text string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	os.Stdout.WriteString(screen.DimGray() + text + screen.ColorReset + "\r\n")
	s.logWrite("sub", aiui.Sanitize(text))
}

func (s *subSink) OnStatus(text string) {}

// OnCompact logs a worker's checkpoint full text as an s-family record
// (scomp): the worker compacted its own context. Like every s* record it
// never re-enters the manager's rebuilt history.
func (s *subSink) OnCompact(text string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.logWrite("scomp", aiui.Sanitize(text))
}

// cwdTracker holds the shell's current directory as reported via OSC 7,
// which the output loop parses out of the child's terminal output. It is the
// primary cwd source on platforms without /proc (macOS, Windows); on Linux
// currentCWD falls back to /proc until the first report arrives.
var cwdTracker cwd.Tracker

// currentCWD returns the shell process's current working directory, or ""
// when it cannot be determined. It prefers the most recent OSC 7 report the
// shell emitted (accurate on every platform), then falls back to reading
// /proc/<pid>/cwd where that exists (Linux). The shell's cwd follows the
// user's cd, so this is how the agent senses the directory it would run
// commands in.
func currentCWD(pid int) string {
	if cwd := cwdTracker.CWD(); cwd != "" {
		return cwd
	}
	if pid > 0 {
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			return target
		}
	}
	// Fallback for platforms without /proc (Windows, macOS before the first
	// OSC 7 report): use rysh's own working directory so the AI prompt's \w
	// and the agent's tool cwd resolve somewhere real instead of an empty
	// string — PSExpand renders an empty \w as "?", which is what the user
	// saw on Windows where /proc does not exist.
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// defaultEnvAllowlist lists the environment variables the agent sees in
// context unless the config overrides them. Only these are exposed because
// a full dump of the shell's environment could leak secrets (API keys,
// tokens) to the model.
var defaultEnvAllowlist = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "TERM", "HOSTNAME",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "EDITOR", "VISUAL", "PAGER", "PATH",
}

// envAllowlistFor resolves the effective allowlist for a request: a
// configured [agent] env_allowlist replaces the default exactly (even when
// empty), while an absent one falls back to the built-in default.
func envAllowlistFor(cfg *config.Config) []string {
	if cfg != nil && cfg.Agent.EnvAllowlist != nil {
		return cfg.Agent.EnvAllowlist
	}
	return defaultEnvAllowlist
}

// currentEnv returns the shell process's environment as "KEY=VALUE" lines
// for the variables in allowlist, or "" when it cannot be determined
// (e.g. the process is gone, or /proc is unavailable on non-Linux). /proc
// exposes the environment the shell started with; later in-session exports
// are not visible through this mechanism (a shell hook / OSC reporting
// would be needed, out of scope for M4).
func currentEnv(pid int, allowlist []string) string {
	if pid <= 0 {
		return ""
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	got := make(map[string]string)
	for _, kv := range bytes.Split(raw, []byte{0}) {
		if k, v, ok := strings.Cut(string(kv), "="); ok {
			got[k] = v
		}
	}
	var lines []string
	for _, k := range allowlist {
		if v, ok := got[k]; ok {
			lines = append(lines, k+"="+v)
		}
	}
	return strings.Join(lines, "\n")
}

// forwardTermSignals relays termination signals to the child. For hard
// signals (SIGTERM/SIGHUP) it force-kills the child if it does not exit
// within 3 seconds, so rysh never hangs after being asked to terminate.
func forwardTermSignals(fwd <-chan os.Signal, c *pty.Cmd, childExited <-chan struct{}) {
	for {
		select {
		case <-childExited:
			// This shell is gone. Retire: a session switch starts a fresh
			// forwarder for the new shell, and a leftover one would otherwise
			// compete for the signal channel and relay a termination signal to
			// a dead process instead of the current shell.
			return
		case s, ok := <-fwd:
			if !ok {
				return
			}
			if c.Process == nil {
				continue
			}
			_ = c.Process.Signal(s)
			if isHardSignal(s) {
				select {
				case <-childExited: // child exited on its own
				case <-time.After(3 * time.Second):
					_ = c.Process.Kill()
				}
			}
		}
	}
}

func rawTerminal() []func() {
	var restores []func()
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
			restores = append(restores, func() { _ = term.Restore(int(os.Stdin.Fd()), old) })
		}
	}
	// Make stdout raw too: on Unix this clears output post-processing
	// (OPOST) so the pty's \r\n sequences aren't doubled. On Windows this
	// call fails harmlessly and output processing is left alone.
	if term.IsTerminal(int(os.Stdout.Fd())) {
		if old, err := term.MakeRaw(int(os.Stdout.Fd())); err == nil {
			restores = append(restores, func() { _ = term.Restore(int(os.Stdout.Fd()), old) })
		}
		// Enable DEC 2004 (bracketed paste) so multi-line pastes arrive
		// wrapped in CSI 200~/201~ markers instead of being typed key by
		// key, which would submit each embedded line as its own draft.
		// Offending terminals simply never emit the markers and pastes
		// degrade to the previous per-key behavior.
		if _, err := os.Stdout.WriteString("\x1b[?2004h"); err == nil {
			restores = append(restores, func() { _, _ = os.Stdout.WriteString("\x1b[?2004l") })
		}
	}
	return restores
}

func restoreAll(restores []func()) {
	for i := len(restores) - 1; i >= 0; i-- {
		restores[i]()
	}
}

// crashCleanup leaves the terminal in a usable state after a panic: clear
// colors/scroll region and re-show the cursor. It writes without writeMu:
// the panic may have fired while the mutex was held, so taking it here
// could deadlock. Best-effort — the process is about to crash either way.
func crashCleanup() {
	fmt.Fprintln(os.Stderr, "rysh: internal error; terminal restored")
	os.Stdout.WriteString(screen.Reset() + screen.ShowCursor())
}

// commaInt renders n with thousands separators (1234567 -> "1,234,567")
// for the task-stats footer's token counts.
func commaInt(n int) string {
	s := strconv.Itoa(n)
	start := 0
	if n < 0 {
		start = 1 // skip the sign
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
