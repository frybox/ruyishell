package main

// A mode switch must leave the shell's input line where the prompt actually
// is. That is only observable through the shell's own repaint, so these tests
// run a real PowerShell child shell and judge the screen: the outer pty stream
// goes through a minimal ANSI screen model, and the row the prompt ends up on
// is compared with the row the line editor paints the typed text on.
//
// The offset guarded against: PSReadLine caches the row it wrote its prompt on
// and re-emits the input line at that absolute row on every keystroke. A
// resync that moves the real cursor without moving that cache (a row-up plus a
// carriage return, which is what a plain shell needs) leaves the two a row
// apart, so every keystroke after the switch paints one row off: typing "exit"
// shows "ex" on the prompt row and "exi" one row below it.

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func psChildCfg(t *testing.T) string {
	t.Helper()
	ps, ok := findProgram("powershell")
	if !ok {
		t.Skip("powershell not found")
	}
	return "shell = " + strconv.Quote(ps) + "\n"
}

// TestPowerShellSwitchBackPaintsInPlace types a fresh token into a PowerShell
// child shell and requires the shell's own repaint to land on the prompt's
// row, both with and without an AI round trip first. Each case gets its own
// rysh process and its own token: a repaint that lands a row off must not be
// able to hide behind text an earlier case left on the screen.
func TestPowerShellSwitchBackPaintsInPlace(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the repaint cache this guards is a ConPTY/PSReadLine behaviour")
	}
	for _, tc := range []struct {
		name      string
		roundTrip bool
		token     string
	}{
		{"no-round-trip", false, "dir"},
		{"after-round-trip", true, "xyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, r := startRyshEnv(t, "", psChildCfg(t))
			sc := newTermScreen(24, 80)
			consume := func() { sc.feed(r.readQuiet(t, 1500*time.Millisecond, 10*time.Second)) }

			sc.feed(r.readUntil(t, "PS", startupTimeout))
			consume()

			if tc.roundTrip {
				enterAI(t, p, r)
				consume()
				p.Write(defaultSwitchSeq) // back to the shell, resync included
				consume()
			}

			for _, k := range []byte(tc.token) {
				p.Write([]byte{k})
				time.Sleep(300 * time.Millisecond)
			}
			consume()

			promptRows := sc.findAll("PS D:")
			inputRows := sc.findAll(tc.token)
			sc.dump(t, tc.name)
			if len(promptRows) == 0 {
				t.Fatalf("the screen model lost the prompt (token %q on rows %v)", tc.token, inputRows)
			}
			if len(inputRows) != 1 {
				t.Fatalf("the typed %q painted on rows %v, want one row: a repaint split across rows is the jump itself", tc.token, inputRows)
			}
			if want := promptRows[len(promptRows)-1]; inputRows[0] != want {
				t.Errorf("the shell repainted the input on row %d, the prompt is on row %d (off by %d)",
					inputRows[0], want, inputRows[0]-want)
			}
		})
	}
}

// TestPowerShellSwitchBackKeepsPromptRow checks the other half of the same
// promise: the prompt comes back on the row it was on, so a switch does not
// push the shell down the screen.
func TestPowerShellSwitchBackKeepsPromptRow(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the repaint cache this guards is a ConPTY/PSReadLine behaviour")
	}
	p, _, r := startRyshEnv(t, "", psChildCfg(t))
	sc := newTermScreen(24, 80)
	consume := func() { sc.feed(r.readQuiet(t, 1500*time.Millisecond, 10*time.Second)) }

	sc.feed(r.readUntil(t, "PS", startupTimeout))
	consume()
	before := sc.findAll("PS D:")
	if len(before) != 1 {
		t.Fatalf("the PowerShell prompt is on rows %v before the switch, want exactly one", before)
	}

	enterAI(t, p, r)
	consume()
	p.Write(defaultSwitchSeq)
	consume()

	after := sc.findAll("PS D:")
	if len(after) == 0 {
		sc.dump(t, "no-prompt")
		t.Fatal("the shell prompt did not come back after switching to the shell")
	}
	if got := after[len(after)-1]; got != before[0] {
		sc.dump(t, "prompt-moved")
		t.Errorf("the prompt moved from row %d to row %d across a mode switch", before[0], got)
	}
}

// termScreen is a minimal ANSI screen model: it tracks the cursor and the text
// on each row. Sequences it does not model are consumed and ignored, so it can
// be wrong about position but never about the moves it does accept. Bytes of an
// escape sequence split across reads are carried to the next feed, because a
// pty hands over whatever chunks the console happens to emit.
type termScreen struct {
	rows, cols     int
	curR, curC     int // 0-based
	lines          []string
	savedR, savedC int
	pending        string // an incomplete escape sequence, carried across feeds
}

func newTermScreen(rows, cols int) *termScreen {
	return &termScreen{rows: rows, cols: cols, lines: make([]string, rows)}
}

func (s *termScreen) feed(str string) {
	str = s.pending + str
	s.pending = ""
	for i := 0; i < len(str); {
		c := str[i]
		switch {
		case c == 0x1b && i+1 >= len(str):
			s.pending = str[i:]
			return
		case c == 0x1b && str[i+1] == ']': // OSC ... BEL
			k := strings.IndexByte(str[i+2:], 0x07)
			if k < 0 {
				s.pending = str[i:]
				return
			}
			i += 2 + k + 1
		case c == 0x1b && str[i+1] == '[':
			j := i + 2
			for j < len(str) && (str[j] == ';' || str[j] == '?' || (str[j] >= '0' && str[j] <= '9')) {
				j++
			}
			if j >= len(str) { // parameters still on their way
				s.pending = str[i:]
				return
			}
			s.csi(str[i+2:j], str[j])
			i = j + 1
		case c == '\r':
			s.curC = 0
			i++
		case c == '\n':
			s.down(1)
			i++
		case c == '\b':
			if s.curC > 0 {
				s.curC--
			}
			i++
		case c >= 0x20:
			s.put(c)
			i++
		default:
			i++
		}
	}
}

func (s *termScreen) put(c byte) {
	if s.curC >= s.cols { // no autowrap modelled: the row is already known
		return
	}
	row := s.lines[s.curR]
	if s.curC > len(row) { // the cursor sat past the text: pad with blanks
		row += strings.Repeat(" ", s.curC-len(row))
	}
	if s.curC < len(row) {
		row = row[:s.curC] + string(c) + row[s.curC+1:] // overwrite, like a cell
	} else {
		row += string(c)
	}
	s.lines[s.curR] = row
	s.curC++
}

func (s *termScreen) down(n int) {
	s.curR += n
	if s.curR >= s.rows { // scroll up, as the real terminal would
		s.curR = s.rows - 1
		s.lines = append(s.lines[1:], "")
	}
}

func (s *termScreen) up(n int) {
	if s.curR-n < 0 {
		s.curR = 0
		return
	}
	s.curR -= n
}

func (s *termScreen) csi(params string, final byte) {
	var nums []int
	for _, p := range strings.Split(params, ";") {
		p = strings.TrimPrefix(p, "?")
		v, err := strconv.Atoi(p)
		if err != nil {
			v = 0
		}
		nums = append(nums, v)
	}
	count := func() int { // CUU/CUD/CUF/CUB: absent or 0 means 1
		if len(nums) == 0 || nums[0] <= 0 {
			return 1
		}
		return nums[0]
	}
	pos := func(i int) int { // CUP/VPA/CHA: 1-based, absent means 1
		if len(nums) <= i || nums[i] <= 0 {
			return 1
		}
		return nums[i]
	}
	mode := func() int {
		if len(nums) == 0 {
			return 0
		}
		return nums[0]
	}
	switch final {
	case 'A':
		s.up(count())
	case 'B':
		s.down(count())
	case 'C':
		s.curC += count()
		if s.curC > s.cols {
			s.curC = s.cols
		}
	case 'D':
		s.curC -= count()
		if s.curC < 0 {
			s.curC = 0
		}
	case 'H', 'f':
		s.curR = clampScreen(pos(0)-1, s.rows)
		s.curC = clampScreen(pos(1)-1, s.cols)
	case 'd':
		s.curR = clampScreen(pos(0)-1, s.rows)
	case 'G':
		s.curC = clampScreen(pos(0)-1, s.cols)
	case 'J':
		switch mode() {
		case 2, 3:
			for r := range s.lines {
				s.lines[r] = ""
			}
		default:
			row := s.lines[s.curR]
			if s.curC < len(row) {
				s.lines[s.curR] = row[:s.curC]
			}
			for r := s.curR + 1; r < s.rows; r++ {
				s.lines[r] = ""
			}
		}
	case 'K':
		row := s.lines[s.curR]
		switch mode() {
		case 0:
			if s.curC < len(row) {
				s.lines[s.curR] = row[:s.curC]
			}
		case 1:
			if s.curC < len(row) {
				s.lines[s.curR] = strings.Repeat(" ", s.curC) + row[s.curC:]
			}
		case 2:
			s.lines[s.curR] = ""
		}
	case 's':
		s.savedR, s.savedC = s.curR, s.curC
	case 'u':
		s.curR, s.curC = clampScreen(s.savedR, s.rows), clampScreen(s.savedC, s.cols)
	}
}

func clampScreen(v, limit int) int {
	if v < 0 {
		return 0
	}
	if v >= limit {
		return limit - 1
	}
	return v
}


func (s *termScreen) findAll(sub string) []int {
	var out []int
	for r, ln := range s.lines {
		if strings.Contains(ln, sub) {
			out = append(out, r+1) // 1-based, as the terminal reports rows
		}
	}
	return out
}


func (s *termScreen) dump(t *testing.T, tag string) {
	t.Helper()
	var b strings.Builder
	for r, ln := range s.lines {
		if trimmed := strings.TrimRight(ln, " "); trimmed != "" {
			b.WriteString(strconv.Itoa(r+1) + "|" + trimmed + "\n")
		}
	}
	t.Logf("== SCREEN %s (cursor at row %d col %d)\n%s", tag, s.curR+1, s.curC+1, b.String())
}
