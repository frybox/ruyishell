package agent

// Loop guards for the agent engine (architecture §6.4). Two guards share
// one sliding window of tool-execution entries:
//
//   - LoopGuard (ported verbatim from the pre-engine cmd/rysh/loopguard.go,
//     2026-08-27 定案): catches exact repetition — the same command, exit
//     code and output head. 2 consecutive → warning rides along on the
//     fed-back result; 6 consecutive, or one signature filling >60% of the
//     saturated window → wrap-up round.
//   - Read-churn guard (§6.4 注二): catches "high-frequency reads, zero
//     writes, same target over and over" churn that never builds a
//     consecutive run — the compression→forget→re-read failure mode. The
//     third occurrence of a read signature in the window warns; a long
//     write stall with a read-dominated window first injects one
//     regroup reminder, then wraps up.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// loopWindow is how many recent tool executions the guard remembers.
	loopWindow = 20
	// loopWarnConsec: from this many consecutive identical runs on, every
	// fed-back result carries a nudge telling the model to change course.
	loopWarnConsec = 2
	// loopBreakConsec: this many consecutive identical runs end the task.
	loopBreakConsec = 6
	// loopShareLimit: one signature filling this share of the full window
	// also ends the task, catching interleaved loops (A A A B A A B…)
	// that never build a long consecutive run. share stays 0 until the
	// window saturates at loopWindow, because below that the ratio is
	// noise — a lone first command would otherwise fill 100%.
	loopShareLimit = 0.6
	// sigPrefixBytes caps how much output feeds the signature hash, so a
	// chatty command costs a bounded amount of hashing.
	sigPrefixBytes = 8 * 1024

	// readChurnRepeat: the Nth occurrence of one read signature inside the
	// window trips the repeat-read warning (not necessarily consecutive).
	readChurnRepeat = 3
	// writeStallSteps / writeStallWindow: with no write-class action for
	// this many steps or this long, and the window read-dominated, the
	// engine first injects a regroup reminder; if the stall persists after
	// the reminder, it wraps up (reason read churn).
	writeStallSteps  = 30
	writeStallWindow = 15 * time.Minute
	// readShareLimit: read-class entries must fill this share of the
	// saturated window for the stall to count as read churn.
	readShareLimit = 0.7
)

// Wrap reasons; they appear on screen, in the session log and inside the
// injected wrap-up instruction.
const (
	wrapReasonLoop      = "loop guard"
	wrapReasonReadChurn = "read churn"
)

// loopWarnFormat rides along on every repeated result once the consecutive
// run reaches loopWarnConsec (%d is the current run length). Warnings are
// never shown on screen nor logged — they only ride on the next request.
const loopWarnFormat = "\n[rysh] 警告：这是完全相同的命令与输出的第 %d 次重复——环境没有因它改变。请改变方法，或直接基于已有信息给出结论。"

// readWarnFormat rides on a tool result whose read signature has now been
// seen readChurnRepeat times inside the window.
const readWarnFormat = "\n[rysh] 提示：同一内容已第 %d 次读取，结果应仍在上下文中；若已丢失，请基于现有信息推进，或先用 todo 工具整理进度。"

// regroupInstruction is injected once as a user message when the write
// stall first trips — a free self-reorganize for weak models. Unlike the
// wrap-up it does not forbid tools.
const regroupInstruction = "[rysh] 检测到长时间只读探索且没有产出：请先输出三行——「已掌握 / 待办 / 下一步」——重整思路后再继续任务。"

// wrapUpInstruction is injected as a user message to force the final,
// text-only answer when the task must stop. Its Chinese wording doubles as
// the marker integration mocks watch for.
const wrapUpInstruction = "[rysh] 本任务的执行预算已到极限（原因：%s）。请立即停止调用工具，不要再输出任何 shell 围栏代码块。基于以上全部信息，直接输出：① 已完成的工作；② 关键发现与修改；③ 未完成的部分与下一步建议。"

// entry kinds for the shared window. Read/write classification: registry
// ReadOnly tools are reads, write/edit are writes, and bash is classified
// by bashLooksReadOnly (the §7.1 safe-command shape; the approval wiring
// itself lands with M7.4).
type entryKind int

const (
	kindOther entryKind = iota
	kindRead
	kindWrite
)

// loopEntry is one tool execution in the guard window. sig is the
// repetition signature (command+exit+output head); readSig — set on
// read-class entries only — hashes just the tool and normalized arguments,
// so a re-read of the same target counts even when its output changed.
type loopEntry struct {
	sig     string
	readSig string
	kind    entryKind
}

// toolSignature hashes the parts of a tool execution that must all repeat
// for the run to count as stuck. Binding the output head into the
// signature lets legitimate polling (log tailing, progress checks) vary
// freely. For bash, args is the whitespace-normalized command and out is
// stdout+stderr — identical to the pre-engine formula.
func toolSignature(name, args string, exit int, out string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(strings.Fields(args), " ")))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", exit)
	h.Write([]byte{0})
	if len(out) > sigPrefixBytes {
		out = out[:sigPrefixBytes]
	}
	h.Write([]byte(out))
	return hex.EncodeToString(h.Sum(nil))
}

// readSignature hashes tool + normalized arguments (no output): the
// read-churn guard's "same target again" measure.
func readSignature(name, args string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(strings.Fields(args), " ")))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeArgs canonicalizes tool arguments for signature hashing: the
// JSON is re-marshaled (Go sorts map keys) so key order differences do not
// change the signature. Broken JSON falls back to whitespace squashing.
func normalizeArgs(raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return strings.Join(strings.Fields(raw), " ")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return strings.Join(strings.Fields(raw), " ")
	}
	return string(b)
}

// loopResult is what record reports for one execution.
type loopResult struct {
	Consec    int     // how many times the newest signature now ends the window
	Share     float64 // its share of the saturated window (0 while unsaturated)
	ReadCount int     // occurrences of the read signature in the window (reads only)
}

// loopGuard tracks recent tool executions in a sliding window and reports
// how repetitive — and how read-bound — the model's actions have become.
type loopGuard struct {
	window []loopEntry // oldest first, capped at loopWindow

	lastWriteStep int       // step of the most recent write-class entry, 0 = none
	lastWriteAt   time.Time // wall clock of the same
}

// record adds one entry and reports the repetition measures for it.
func (g *loopGuard) record(e loopEntry, step int, now time.Time) loopResult {
	g.window = append(g.window, e)
	if len(g.window) > loopWindow {
		g.window = g.window[len(g.window)-loopWindow:]
	}
	if e.kind == kindWrite {
		g.lastWriteStep = step
		g.lastWriteAt = now
	}
	var res loopResult
	for i := len(g.window) - 1; i >= 0 && g.window[i].sig == e.sig; i-- {
		res.Consec++
	}
	if len(g.window) == loopWindow {
		count := 0
		for _, w := range g.window {
			if w.sig == e.sig {
				count++
			}
		}
		res.Share = float64(count) / float64(len(g.window))
	}
	if e.kind == kindRead && e.readSig != "" {
		for _, w := range g.window {
			if w.kind == kindRead && w.readSig == e.readSig {
				res.ReadCount++
			}
		}
	}
	return res
}

// writeStalled reports whether the write stall half of the read-churn
// guard is active: no write-class action for writeStallSteps steps or
// writeStallWindow, with the saturated window read-dominated. Like the
// loop share, the read share stays silent until the window saturates —
// early in a task everything is reads, and that must not read as churn.
func (g *loopGuard) writeStalled(step int, now time.Time) bool {
	if len(g.window) < loopWindow {
		return false
	}
	lastStep := g.lastWriteStep
	if lastStep == 0 {
		lastStep = 1 // window full and never a write: stall counts from step 1
	}
	if step-lastStep < writeStallSteps && now.Sub(g.lastWriteAt) < writeStallWindow {
		return false
	}
	reads := 0
	for _, w := range g.window {
		if w.kind == kindRead {
			reads++
		}
	}
	return float64(reads)/float64(len(g.window)) > readShareLimit
}

// bashLooksReadOnly reports whether a bash command matches the §7.1
// safe-command shape: a known read-only head word and no shell metachars
// (redirection or composition makes it a write/composite action). This
// first version only classifies for the read-churn guard; the approval
// gate built on it lands with M7.4.
func bashLooksReadOnly(cmd string) bool {
	squashed := strings.Join(strings.Fields(cmd), " ")
	if squashed == "" {
		return false
	}
	if !safeShape(squashed) {
		return false
	}
	head := strings.SplitN(squashed, " ", 2)[0]
	switch head {
	case "ls", "cat", "head", "tail", "less", "grep", "rg", "find", "pwd", "echo",
		"which", "file", "stat", "du", "df", "ps", "wc", "sort", "uniq", "diff":
		return true
	case "git":
		sub := strings.Fields(squashed)
		if len(sub) >= 2 {
			switch sub[1] {
			case "status", "diff", "log", "show", "branch", "blame", "rev-parse", "describe":
				return true
			}
		}
		return false
	case "go":
		sub := strings.Fields(squashed)
		return len(sub) >= 2 && (sub[1] == "list" || sub[1] == "env" || sub[1] == "version")
	}
	return false
}

// safeShape reports whether a whitespace-squashed command carries the
// shape half of the §7.1 classifier: no shell metachars (composition,
// redirection, substitution) and none of the known write-class flags.
// Shared by bashLooksReadOnly and the approval policy's configurable
// safe-head extension.
func safeShape(squashed string) bool {
	if strings.ContainsAny(squashed, ";|&$`(){}<>\n") {
		return false
	}
	for _, banned := range []string{"-delete", "-exec", "-execdir", "rm ", "mv ", "cp ", ">", ">>"} {
		if strings.Contains(squashed, banned) {
			return false
		}
	}
	return true
}
