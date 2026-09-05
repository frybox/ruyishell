// Shared session helpers for the in-process AI-mode slash commands, the
// one-shot rysh subcommands (ls/new/resume/kill), and session switching.
// These read and write the on-disk session store; the interactive process
// keeps only its active session in memory.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"ruyishell/internal/agent"
	"ruyishell/internal/markdown"
	"ruyishell/internal/provider"
	"ruyishell/internal/screen"
	"ruyishell/internal/session"
)

// sessionStore returns the on-disk session store, or nil when no home
// directory is available (no persistence).
func sessionStore() *session.Store {
	s, err := session.NewStore()
	if err != nil {
		return nil
	}
	return s
}

// titleFrom derives a session's display name from its first user message:
// the first line, whitespace-collapsed, truncated to 16 runes.
func titleFrom(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) > 16 {
		r = r[:16]
	}
	return string(r)
}

// updateSessionTitle writes the session's display name from its first user
// message when the session has not been named yet (default 新会话).
func updateSessionTitle(store *session.Store, meta *session.Meta, text string) {
	if store == nil || meta == nil || meta.Name != "" {
		return
	}
	if name := titleFrom(text); name != "" {
		if err := store.SaveName(meta.ID, name); err == nil {
			meta.Name = name
		}
	}
}

// resolveSession maps a session identifier — a number (the session number)
// or a raw id — to its meta. Numbers are tried first.
func resolveSession(store *session.Store, ident string) (*session.Meta, error) {
	if store == nil {
		return nil, fmt.Errorf("%s", uiT.Get("no_session_store"))
	}
	metas, err := store.ListSessions()
	if err != nil {
		return nil, err
	}
	if n, err := strconv.Atoi(ident); err == nil {
		for i := range metas {
			if metas[i].Num == n {
				return &metas[i], nil
			}
		}
		return nil, fmt.Errorf("%s", uiT.Get("no_session", ident))
	}
	for i := range metas {
		if metas[i].ID == ident {
			return &metas[i], nil
		}
	}
	return nil, fmt.Errorf("%s", uiT.Get("no_session", ident))
}

// selfPID identifies "this rysh instance" for attachment scanning. Inside
// a running rysh, a one-shot subprocess inherits RYSH_PID (the interactive
// instance's pid) so it treats that instance's attachment record as its
// own; at top level the subprocess is the instance and uses its own pid.
func selfPID() int {
	if s := os.Getenv(pidEnv); s != "" {
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			return pid
		}
	}
	return os.Getpid()
}

// scanAttached reads every <pid>.sess attachment record in ctlDir and
// returns the live ones (owner pid still running, excluding selfPID) as a
// map of session id → owner pid. Stale records left by a killed rysh are
// filtered by the pid liveness check.
func scanAttached(ctlDir string, selfPID int) map[string]int {
	attached := map[string]int{}
	entries, err := os.ReadDir(ctlDir)
	if err != nil {
		return attached
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sess") {
			continue
		}
		pidStr := strings.TrimSuffix(e.Name(), ".sess")
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid == selfPID || !pidAlive(pid) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ctlDir, e.Name()))
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(data)); id != "" {
			attached[id] = pid
		}
	}
	return attached
}

// attachedByOther reports the pid of a live rysh process (not selfPID)
// attached to id, if any.
func attachedByOther(store *session.Store, id string, selfPID int) (int, bool) {
	if store == nil {
		return 0, false
	}
	pid, ok := scanAttached(store.CtlDir(), selfPID)[id]
	return pid, ok
}

// writeSessionAttach atomically records that this rysh process (identified
// by its pid in the filename) is attached to session id.
func writeSessionAttach(path, id string) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// renderSessionList renders one page (sessionPageSize rows) of the session
// list, shared by the in-process /ls command and the rysh ls subprocess.
// activeID marks the current session with a leading >; attached maps
// sessions owned by another live rysh to that process's pid. Each row adds
// the creation time, the last update time (updated ledger), and the rysh
// running the session: 本实例 for the instance this invocation belongs to,
// [rysh <pid>] for another live one.
func renderSessionList(store *session.Store, activeID string, page int, attached map[string]int) []string {
	if store == nil {
		return []string{uiT.Get("no_session_store")}
	}
	metas, err := store.ListSessions()
	if err != nil {
		return []string{"rysh: " + err.Error()}
	}
	if len(metas) == 0 {
		return []string{uiT.Get("session_list_empty")}
	}
	pages := (len(metas) + sessionPageSize - 1) / sessionPageSize
	if page < 1 || page > pages {
		return []string{uiT.Get("no_page", page)}
	}
	start := (page - 1) * sessionPageSize
	end := start + sessionPageSize
	if end > len(metas) {
		end = len(metas)
	}
	updated := store.UpdatedTimes()
	rows := [][]string{{"", uiT.Get("col_num"), "ID", uiT.Get("col_name"), uiT.Get("col_created"), uiT.Get("col_updated"), uiT.Get("col_owner")}}
	for _, m := range metas[start:end] {
		name := m.Name
		if name == "" {
			name = uiT.Get("new_session_name")
		}
		marker := " "
		if m.ID == activeID {
			marker = ">"
		}
		occupied := ""
		if pid, ok := attached[m.ID]; ok {
			occupied = fmt.Sprintf("[rysh %d]", pid)
		} else if m.ID == activeID {
			occupied = uiT.Get("occupied_self")
		}
		rows = append(rows, []string{marker, strconv.Itoa(m.Num), m.ID, name,
			fmtListTime(m.Created), fmtListTime(updated[m.ID]), occupied})
	}
	// Column width = widest cell (CJK counts double), so the header and the
	// fixed-width time columns stay aligned across rows.
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if w := ansi.StringWidth(c); w > widths[i] {
				widths[i] = w
			}
		}
	}
	lines := []string{uiT.Get("session_list", page, pages, len(metas))}
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = c + strings.Repeat(" ", widths[i]-ansi.StringWidth(c))
		}
		lines = append(lines, strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	if pages > 1 {
		lines = append(lines, uiT.Get("list_page_hint"))
	}
	return lines
}

// fmtListTime renders a unix-millisecond timestamp for the session list:
// "01-02 15:04" within the current year, "2006-01-02" otherwise, and "-"
// when there is no record.
func fmtListTime(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	t := time.UnixMilli(ms)
	if t.Year() == time.Now().Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02")
}

// historyItemsFor collects a session's user-input history from its log
// records: every usr event, oldest first, exactly as submitted (no
// filtering — slash and `!`-leading inputs included, matching the recall
// source ↑/↓ uses via userInputsFor). -v pairs each input with the
// contiguous asw segments that follow it before the next usr, merged into
// one reply (the model's answer for that input; reasoning, tool results
// and notices are dropped, the reply is not trimmed to maxHistory).
func historyItemsFor(dir, id string) []historyItem {
	recs, err := session.ReadMessages(dir, id)
	if err != nil {
		return nil
	}
	var items []historyItem
	for _, rec := range recs {
		switch rec.Kind {
		case "usr":
			items = append(items, historyItem{input: rec.P})
		case "asw":
			if len(items) == 0 {
				continue
			}
			last := &items[len(items)-1]
			last.output += rec.P
		}
	}
	return items
}

// historyItem is one user input and, for /history -v, the model's reply
// that followed it (asw segments merged).
type historyItem struct {
	input  string
	output string
}

// renderHistory formats the /history output: a numbered, paginated list of
// the current session's user inputs, newest last. verbose pairs each input
// with its reply (the reply is shown indented under the input); page is
// 1-based and the page size is sessionPageSize for plain mode, 3 for -v
// (a reply can span many lines). Out-of-range pages report the missing page,
// and an empty history reports 共 0 条.
func renderHistory(dir, id string, page int, verbose bool) []string {
	items := historyItemsFor(dir, id)
	if len(items) == 0 {
		return []string{uiT.Get("history_empty")}
	}
	size := sessionPageSize
	if verbose {
		size = 3
	}
	pages := (len(items) + size - 1) / size
	if page < 1 || page > pages {
		return []string{uiT.Get("no_page", page)}
	}
	start := (page - 1) * size
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	lines := []string{uiT.Get("history", page, pages, len(items))}
	for i := start; i < end; i++ {
		// 1-based index; the number is what `history <页号>` does NOT take —
		// it is display-only (there is no per-item jump, like /ls rows).
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, items[i].input))
		if verbose {
			out := strings.TrimSpace(items[i].output)
			if out == "" {
				out = uiT.Get("no_output")
			}
			// The reply is shown under its input, indented: the first line
			// carries the ↳ marker, continuation lines align under it.
			for j, ln := range strings.Split(out, "\n") {
				if j == 0 {
					lines = append(lines, "  ↳ "+ln)
				} else {
					lines = append(lines, "    "+ln)
				}
			}
		}
	}
	if pages > 1 {
		lines = append(lines, uiT.Get("history_page_hint"))
	}
	return lines
}

// selectActiveSession chooses the startup session: the most recently
// updated one on disk (last line of the updated ledger) that is loadable
// and not attached by another live rysh. When none qualifies, a new session
// is created.
func selectActiveSession(store *session.Store, selfPID int) (*session.Meta, error) {
	if id := store.LastUpdatedID(); id != "" {
		if _, attached := attachedByOther(store, id, selfPID); !attached {
			if m, err := resolveSession(store, id); err == nil {
				return m, nil
			}
		}
	}
	return store.CreateSession()
}

// readHistory rebuilds a session's AI conversation from its on-disk log
// records, or nil when the log cannot be read (no persistence, missing
// files). It is the disk-side counterpart of the in-memory active.hist.
func readHistory(dir, id string) []ctxMsg {
	if dir == "" || id == "" {
		return nil
	}
	recs, err := session.ReadMessages(dir, id)
	if err != nil {
		return nil
	}
	return reconstructHistory(recs)
}

// reconstructHistory rebuilds the AI conversation context from a session's
// log records: usr prompts become user messages, contiguous asw segments
// merge into one assistant message (rea reasoning is skipped), tool records
// become system messages, and everything else (noti/sys/shl/shk) is
// dropped. The result is trimmed to maxHistory. The internal roles are the
// session's own shape (the display replay keys off them); the wire shape
// is enforced at the provider boundary, where a system message after the
// leading run is demoted to a marked user message (strict OpenAI-
// compatible endpoints reject the other arrangement).
func reconstructHistory(recs []session.Record) []ctxMsg {
	var hist []ctxMsg
	var asw strings.Builder
	var aswTS int64
	flush := func() {
		if asw.Len() == 0 {
			return
		}
		hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "assistant", Content: asw.String()}}, ts: time.Unix(0, aswTS)})
		asw.Reset()
		aswTS = 0
	}
	for _, rec := range recs {
		switch rec.Kind {
		case "usr":
			flush()
			text := strings.TrimSpace(rec.P)
			if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "!") {
				continue
			}
			hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "user", Content: text}}, ts: time.Unix(0, rec.Ts)})
		case "asw":
			if aswTS == 0 {
				aswTS = rec.Ts
			}
			asw.WriteString(rec.P)
		case "tool":
			flush()
			hist = append(hist, ctxMsg{TurnMsg: agent.TurnMsg{Msg: provider.ChatMessage{Role: "system", Content: rec.P}}, ts: time.Unix(0, rec.Ts)})
		}
	}
	flush()
	return trimHistory(hist, maxHistory)
}

// renderHistoryReplay renders a conversation as a sequence of dim message
// headers and markdown-styled bodies, for the AI-mode landing replay and
// the one-shot chatOnce. Output uses \n line separators: raw interactive
// callers convert to \r\n, while the cooked chatOnce stdout needs plain \n
// (ONLCR supplies the carriage return).
func renderHistoryReplay(hist []ctxMsg) string {
	var b strings.Builder
	for _, m := range hist {
		switch m.Msg.Role {
		case "user":
			b.WriteString(screen.DimGray() + uiT.Get("user_label") + "\n" + screen.ColorReset)
			b.WriteString(m.Msg.Content + "\n")
		case "assistant":
			b.WriteString("\n" + screen.DimGray() + uiT.Get("assistant_label") + "\n" + screen.ColorReset)
			md := markdown.New()
			b.WriteString(md.Write(m.Msg.Content))
			if held := md.Close(); held != "" {
				b.WriteString(held)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// replayTailLines is the maximum number of lines the unified session replay
// keeps from the tail of the log. The tail is bounded by lines (not records)
// and is cut block-atomically, so a reasoning run, an answer, or a shell
// output block is never split mid-unit.
const replayTailLines = 200

// renderUnifiedReplay renders the tail of a session's unified timeline — the
// shell command/output interleaved with the AI conversation — as plain text
// for scrolling-back review on the main screen. It reads the whole session
// log but coalesces consecutive records of the same kind into atomic blocks
// (a reasoning run = many rea records, an answer = many asw, a command's
// output = many shl) and renders only the most recent blocks whose combined
// line count reaches maxLines — never splitting a block. Returns "" when the
// log is empty or unreadable. Output uses \n line separators; the caller
// converts to \r\n for the raw terminal.
func renderUnifiedReplay(dir, id string, maxLines int) string {
	if dir == "" || id == "" {
		return ""
	}
	recs, err := session.ReadMessages(dir, id)
	if err != nil || len(recs) == 0 {
		return ""
	}
	return renderUnifiedReplayRecords(recs, maxLines)
}

// renderUnifiedReplayRecords renders the tail of exactly the given
// records (renderUnifiedReplay passes the whole log).
func renderUnifiedReplayRecords(recs []session.Record, maxLines int) string {
	if len(recs) == 0 {
		return ""
	}
	// Coalesce consecutive records of the same kind into blocks (order
	// kept). Streaming fragments of one logical unit join verbatim —
	// shl records already end in \n (one per complete output line) and
	// asw deltas are raw slices of the answer — so a "\n" separator would
	// add a blank line between every output line and break the answer at
	// every delta boundary. One-record-per-message kinds keep the "\n".
	type block struct {
		kind string
		text string
	}
	var blocks []block
	for _, rec := range recs {
		if rec.Kind == "" {
			continue
		}
		sep := "\n"
		if rec.Kind == "shl" || rec.Kind == "asw" {
			sep = ""
		}
		if n := len(blocks); n > 0 && blocks[n-1].kind == rec.Kind {
			blocks[n-1].text += sep + rec.P
		} else {
			blocks = append(blocks, block{kind: rec.Kind, text: rec.P})
		}
	}
	// Walk blocks from the tail, accumulating the line count, and keep
	// everything from the first block that pushes the total over maxLines
	// onward (block-atomic: a block is never split).
	start := 0
	lines := 0
	for i := len(blocks) - 1; i >= 0; i-- {
		lines += strings.Count(blocks[i].text, "\n") + 1
		start = i
		if lines >= maxLines {
			break
		}
	}
	blocks = blocks[start:]
	if start > 0 {
		blocks = append([]block{{kind: "more", text: uiT.Get("older_omitted")}}, blocks...)
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.kind {
		case "usr":
			b.WriteString(screen.DimGray() + uiT.Get("user_label") + "\n" + screen.ColorReset)
			b.WriteString(blk.text + "\n")
		case "shk":
			b.WriteString(screen.DimGray() + "─── $ " + firstCommandLine(blk.text) + " ───\n" + screen.ColorReset)
		case "asw":
			b.WriteString(screen.DimGray() + uiT.Get("assistant_label") + "\n" + screen.ColorReset)
			md := markdown.New()
			b.WriteString(md.Write(blk.text))
			if held := md.Close(); held != "" {
				b.WriteString(held)
			}
			b.WriteString("\n")
		case "shl":
			// Shell output: the log kept SGR styling, so replay it with its
			// original colors. Each record terminated in \n, so the block
			// already ends on a newline; ensure one is present.
			b.WriteString(blk.text)
			if !strings.HasSuffix(blk.text, "\n") {
				b.WriteString("\n")
			}
		case "more":
			// Truncation marker: older blocks of the rendered range were
			// dropped by the maxLines cut.
			b.WriteString(screen.DimGray() + blk.text + "\n" + screen.ColorReset)
		default:
			// sys / approval / compact / rea / tool / noti carry no
			// human-facing timeline content worth replaying here.
		}
	}
	return b.String()
}

// firstCommandLine returns the first line of a shell command, so the replay
// header reads "─── $ cmd ───" without a trailing newline.
func firstCommandLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// sessionMessageCount returns the number of message-bearing records in a
// session log (usr/shk/asw/rea/shl/tool/noti), used only for the replay
// separator line.
func sessionMessageCount(dir, id string) int {
	if dir == "" || id == "" {
		return 0
	}
	recs, err := session.ReadMessages(dir, id)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range recs {
		switch r.Kind {
		case "usr", "shk", "asw", "rea", "shl", "tool", "noti":
			n++
		}
	}
	return n
}

// replaySeparator renders the one-line header printed before a session
// replay on the main screen, marking where a session's scrollback begins so
// consecutive sessions in the shared backbuffer stay distinguishable.
func replaySeparator(meta *session.Meta, count int) string {
	return screen.DimGray() + uiT.Get("switched_notice",
		meta.ID, count, time.Now().Format("2006-01-02 15:04:05")) + screen.ColorReset
}

// ctrlRequest is one pending switch request read from the control channel.
type ctrlRequest struct {
	id     string // target session id
	notice string // notice to print when the switch lands
}

// writeControlSwitch appends a switch request for id to the control channel
// named by $RYSH_CTL (inherited from the interactive rysh whose shell ran
// this subprocess). It is a no-op when the variable is unset (not inside a
// rysh shell). notice is printed by the interactive process when the switch
// lands, so the subprocess prints its own confirmation first and the parent
// reports the switch separately.
func writeControlSwitch(id, notice string) {
	path := os.Getenv(ctlEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString("switch " + id + " " + notice + "\n")
	_ = f.Close()
}

// readControlRequests reads the switch requests pending in the control
// channel file and truncates it so they are not processed twice. Each
// returned value is one request (target id plus the notice to print). A
// request appended concurrently by a subprocess between the read and the
// truncate survives: O_APPEND writes land at the (now empty) end of file and
// are picked up on the next poll.
func readControlRequests(path string) []ctrlRequest {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var reqs []ctrlRequest
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "switch" && fields[1] != "" {
			notice := ""
			if len(fields) > 2 {
				notice = strings.Join(fields[2:], " ")
			}
			reqs = append(reqs, ctrlRequest{id: fields[1], notice: notice})
		}
	}
	_ = os.WriteFile(path, nil, 0o644)
	return reqs
}

// newSubcommand handles `rysh new`. Top level it creates a session and
// enters the interactive main program attached to it; inside a running rysh
// it creates the session, prints the confirmation, and asks the current
// instance to switch to it via the control channel (no new pty).
func newSubcommand(inside bool) int {
	store := sessionStore()
	if store == nil {
		fmt.Fprintln(os.Stderr, uiT.Get("no_session_store"))
		return 1
	}
	m, err := store.CreateSession()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rysh: "+err.Error())
		return 1
	}
	_ = store.TouchUpdated(m.ID)
	fmt.Printf("%s\n", uiT.Get("created_session", m.ID))
	if inside {
		writeControlSwitch(m.ID, uiT.Get("created_session", m.ID))
		return 0
	}
	return run(m.ID, false)
}

// resumeSubcommand handles `rysh resume <标识>`. Top level it attaches to
// the target session and enters the interactive main program; inside a
// running rysh it validates the target, prints the confirmation, and asks
// the current instance to switch via the control channel.
func resumeSubcommand(inside bool, ident string) int {
	store := sessionStore()
	if store == nil {
		fmt.Fprintln(os.Stderr, uiT.Get("no_session_store"))
		return 1
	}
	meta, err := resolveSession(store, ident)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if pid, ok := attachedByOther(store, meta.ID, selfPID()); ok {
		fmt.Fprintf(os.Stderr, "%s\n", uiT.Get("attached_session", meta.ID, pid, meta.ID))
		return 1
	}
	fmt.Printf("%s\n", uiT.Get("switched_session", fmt.Sprint(meta.Num)))
	if inside {
		writeControlSwitch(meta.ID, uiT.Get("switched_session", fmt.Sprint(meta.Num)))
		return 0
	}
	return run(meta.ID, false)
}

// killSubcommand handles `rysh kill <标识>`: it terminates the rysh process
// attached to the target session so the session can be entered or switched
// to again. On Unix that is SIGTERM escalating to SIGKILL, and the target
// removes its own ctl records while exiting; on Windows there is no signal
// to deliver, so the process is terminated outright (see terminateProcess)
// and the killer removes the records on its behalf and sweeps the shell tree
// the dead instance left behind.
func killSubcommand(ident string) int {
	store := sessionStore()
	if store == nil {
		fmt.Fprintln(os.Stderr, uiT.Get("no_session_store"))
		return 1
	}
	meta, err := resolveSession(store, ident)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	attached := scanAttached(store.CtlDir(), selfPID())
	pid, ok := attached[meta.ID]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s\n", uiT.Get("no_attachment", meta.ID))
		return 1
	}
	// Snapshot the instance's shell tree while its owner is still alive to
	// parent it: once the rysh process is gone the shell is reparented and can
	// no longer be found from the pid.
	shells := pidsOf(procTree(pid))
	if !terminateProcess(pid) {
		fmt.Fprintf(os.Stderr, "%s\n", uiT.Get("kill_failed", pid))
		return 1
	}
	// An instance that was ended outright never took its login shell with it,
	// so the killer sweeps what is left below it; a graceful Unix target has
	// already shut the shell down and this finds nothing to do.
	if survivors := killPids(shells); len(survivors) > 0 {
		fmt.Fprintf(os.Stderr, "%s\n", uiT.Get("kill_children_left", pid, survivors))
	}
	// A terminated rysh never ran its own cleanup (on Windows it could not:
	// the process is ended outright, with no signal handler to run), so the
	// killer removes the dead instance's control channel and attachment
	// record. Readers already ignore records whose pid is gone; this keeps
	// the ctl directory from accumulating the residue of every kill.
	ctlDir := store.CtlDir()
	_ = os.Remove(filepath.Join(ctlDir, fmt.Sprintf("%d.sess", pid)))
	_ = os.Remove(filepath.Join(ctlDir, fmt.Sprintf("%d.cmd", pid)))
	fmt.Printf("%s\n", uiT.Get("killed_process", pid))
	return 0
}

// subcommandUsage is the one-shot subcommand help shown when a rysh is run
// from inside a running rysh (or with an unknown subcommand). It is a
// function because the active UI language is resolved at dispatch time,
// after package initialization.
func subcommandUsage() string { return uiT.Get("usage_subcommand") }
