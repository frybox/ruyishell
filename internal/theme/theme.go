// Package theme provides rysh's polarity-aware, capability-aware color
// theme, modeled on grok-build's GrokNight/GrokDay palettes.
//
// A terminal's color capability is detected once (truecolor / 256 / basic /
// none) and the terminal's background polarity (dark / light) is probed via
// OSC 11 at startup. Every styled surface in rysh reads its color from the
// global theme (Current), so a single polarity decision re-colors the whole
// UI consistently instead of hard-coded escape strings that only work on one
// background.
package theme

import (
	"os"
	"strings"
	"sync"
)

// RGB is a 24-bit color.
type RGB struct{ R, G, B uint8 }

// Level is the terminal's color capability, low to high.
type Level int

const (
	LNone Level = iota
	LBasic // 16 ANSI colors
	LAnsi256
	LTrueColor
)

// Polarity is the terminal background tone the UI is drawn on.
type Polarity int

const (
	// PolUnknown means no polarity evidence; callers fall back to dark.
	PolUnknown Polarity = iota
	PolDark
	PolLight
)

// Palette names every styled surface rysh draws. Fields hold the canonical
// 24-bit value; rendering quantizes to the active Level (see sgrFg/sgrBg).
type Palette struct {
	// Backgrounds.
	Bg        RGB // main body background
	BgRaised  RGB // raised surface (selected row, hover)
	BgSunken  RGB // sunken surface (code block, paste)
	Selection RGB // selected-row background (distinct from raised)

	// Text.
	FG          RGB // primary text
	FGSecondary RGB // secondary / muted text (still readable)
	FGDim       RGB // faint chrome (faded labels)

	// Semantic accents (content categories / state).
	AccentUser       RGB // user turn, prompt chip
	AccentAssistant  RGB // assistant / thinking
	AccentTool       RGB // tool / notice lines
	AccentSystem     RGB // system lines
	AccentError     RGB
	AccentSuccess   RGB
	AccentRunning   RGB
	AccentWarning   RGB
	AccentModel     RGB

	// Prompt + chips.
	PromptDir    RGB // prompt current-directory color
	PromptModel  RGB // prompt model-ref color (muted)
	PromptTag    RGB // prompt marker tag (dim)
	ChipSH       RGB // [SH] badge background
	ChipSHFg     RGB
	ChipAI       RGB // [AI] badge background
	ChipAIFg     RGB

	// Approval / status bars.
	ApprovalBg   RGB
	ApprovalFg   RGB
	StatusBarBg  RGB
	StatusBarFg  RGB

	// Markdown.
	CodeBG    RGB // fenced code block background
	CodeFg    RGB // fenced code block foreground
	InlineFg  RGB // inline code / list marker
	BlockFg   RGB // blockquote marker / dim elements
	HeadingFg RGB

	// Misc.
	LinkFg    RGB
	DiffDelBg RGB
	DiffDelFg RGB
	DiffInsBg RGB
	DiffInsFg RGB
}

var (
	current     = defaultTheme()
	mu          sync.RWMutex
	polarity    Polarity // PolUnknown until probed
	forceLevel  *Level    // set by Detect override / tests
)

// currentLocked returns the theme for the given polarity, quantized.
func defaultTheme() *Theme {
	return &Theme{}
}

// Theme renders colors; it is a thin handle over the active palette + level.
type Theme struct{}

// Current returns the active theme (safe for concurrent use).
func Current() *Theme {
	mu.RLock()
	t := current
	mu.RUnlock()
	return t
}

// SetLevel overrides the detected capability level (tests, --color flags).
func SetLevel(l Level) { forceLevel = &l }

// SetPolarity records the probed terminal background polarity.
func SetPolarity(p Polarity) {
	mu.Lock()
	polarity = p
	mu.Unlock()
}

// ActivePolarity returns the active polarity (PolUnknown until probed).
func ActivePolarity() Polarity {
	mu.RLock()
	defer mu.RUnlock()
	return polarity
}

func (t *Theme) pol() Polarity {
	p := ActivePolarity()
	if p == PolUnknown {
		return PolDark
	}
	return p
}

func (t *Theme) level() Level {
	if forceLevel != nil {
		return *forceLevel
	}
	return Detect()
}

// dark reports whether the theme renders the dark palette.
func (t *Theme) dark() bool { return t.pol() != PolLight }

// ── Backgrounds ───────────────────────────────────────────────────────────

// Bg is the main body background SGR fragment (often empty: the terminal's
// own background is used for the body).
func (t *Theme) Bg() string { return "" }

// SelectedBg is the raised gray bar behind the active/selected list row.
func (t *Theme) SelectedBg() string {
	return t.bg(t.Selection())
}

func (t *Theme) Selection() RGB {
	if t.dark() {
		return RGB{0x24, 0x24, 0x24} // #242424
	}
	return RGB{0xde, 0xde, 0xde} // #dedede
}

// RaisedBg is a one-step-raised surface (hover / highlight).
func (t *Theme) RaisedBg() string {
	return t.bg(t.Raised())
}

func (t *Theme) Raised() RGB {
	if t.dark() {
		return RGB{0x36, 0x36, 0x36} // #363636
	}
	return RGB{0xcc, 0xcc, 0xcc}
}

func (t *Theme) bg(c RGB) string {
	return sgrBg(t.level(), c)
}

// ── Text ──────────────────────────────────────────────────────────────────

// FG is the primary foreground.
func (t *Theme) FG() RGB {
	if t.dark() {
		return RGB{0xe1, 0xe1, 0xe1} // #e1e1e1
	}
	return RGB{0x26, 0x26, 0x26}
}

// DimGray is the secondary/muted foreground used for the reasoning block and
// tool/notice lines. Unlike the old bare \x1b[2m (which is a low-contrast
// gray on a dark background and unreadable on many screens), it picks a
// readable muted tone per polarity and a capability-appropriate slot.
func (t *Theme) DimGray() string {
	return t.fg(t.FGSecondary())
}

// FGDim is the faint chrome foreground (faded labels).
func (t *Theme) FGDim() RGB {
	if t.dark() {
		return RGB{0x6c, 0x6c, 0x6c}
	}
	return RGB{0x8e, 0x8e, 0x8e}
}

func (t *Theme) FGSecondary() RGB {
	if t.dark() {
		return RGB{0xb0, 0xb0, 0xb0} // readable secondary on dark
	}
	return RGB{0x55, 0x55, 0x55}
}

// DimItalic is the reasoning-block style: muted foreground + italic.
func (t *Theme) DimItalic() string {
	return t.DimGray() + "\x1b[3m"
}

func (t *Theme) fg(c RGB) string {
	return sgrFg(t.level(), c)
}

// ── Prompt + chips ────────────────────────────────────────────────────────

// PromptDir is the prompt current-directory color.
func (t *Theme) PromptDir() string { return t.fg(t.promptAccent()) }

// PromptModel is the prompt model-ref segment (dim).
func (t *Theme) PromptModel() string { return t.fg(t.FGDim()) }

// PromptTag is the prompt marker tag (dim), e.g. "(rysh)".
func (t *Theme) PromptTag() string { return t.fg(t.FGDim()) }

// promptAccent is the AI marker / cwd accent (magenta family).
func (t *Theme) promptAccent() RGB {
	if t.dark() {
		return RGB{0xbb, 0x9a, 0xf7} // #bb9af7
	}
	return RGB{0x7d, 0x4b, 0xc6}
}

func (t *Theme) accentModel() RGB {
	if t.dark() {
		return RGB{0x1a, 0xbc, 0x9c} // teal
	}
	return RGB{0x0a, 0x8e, 0x70}
}

// ChipSH / ChipAI are the filled mode badges: black text on green / magenta.
func (t *Theme) ChipSH() string {
	return t.chip(t.chipSHBg())
}
func (t *Theme) ChipAI() string {
	return t.chip(t.chipAIBg())
}

func (t *Theme) chipBg(c RGB) string { return sgrBg(t.level(), c) }
func (t *Theme) chipFg(c RGB) string { return sgrFg(t.level(), c) }

func (t *Theme) chip(c RGB) string {
	if t.level() == LNone {
		return ""
	}
	return t.chipBg(c) + t.chipFg(RGB{0, 0, 0})
}

func (t *Theme) chipSHBg() RGB {
	if t.dark() {
		return RGB{0x0f, 0x87, 0x0f} // deep green
	}
	return RGB{0x37, 0x8e, 0x23}
}
func (t *Theme) chipAIBg() RGB {
	if t.dark() {
		return RGB{0x7d, 0x4b, 0xc6} // magenta
	}
	return RGB{0x7d, 0x4b, 0xc6}
}

// ── Approval / status bars ────────────────────────────────────────────────

// ApprovalStyle is the §11.3 approval row: bold black text on an amber bar.
func (t *Theme) ApprovalStyle() string {
	if t.level() == LNone {
		return "\x1b[1m"
	}
	return t.chipBg(t.approvalBg()) + t.chipFg(RGB{0, 0, 0}) + "\x1b[1m"
}

func (t *Theme) approvalBg() RGB {
	if t.dark() {
		return RGB{0xe0, 0xaf, 0x68} // amber / yellow
	}
	return RGB{0xa2, 0x76, 0x12}
}

// StatusBarStyle is the pinned status row: high-contrast text on a raised bar.
func (t *Theme) StatusBarStyle() string {
	if t.level() == LNone {
		return ""
	}
	return t.chipBg(t.statusBarBg()) + t.chipFg(t.statusBarFg())
}

func (t *Theme) statusBarBg() RGB {
	if t.dark() {
		return RGB{0x2a, 0x2a, 0x2a}
	}
	return RGB{0xc8, 0xc8, 0xc8}
}
func (t *Theme) statusBarFg() RGB {
	if t.dark() {
		return RGB{0xe1, 0xe1, 0xe1}
	}
	return RGB{0x1e, 0x1e, 0x1e}
}

// ── Markdown ──────────────────────────────────────────────────────────────

// CodeBG is the fenced code block background.
func (t *Theme) CodeBG() string {
	return t.bg(t.codeBG())
}

// CodeFg is the fenced code block foreground.
func (t *Theme) CodeFg() string {
	return t.fg(t.FG())
}

func (t *Theme) codeBG() RGB {
	if t.dark() {
		return RGB{0x1c, 0x1c, 0x1c}
	}
	return RGB{0xe4, 0xe4, 0xe4}
}

// InlineFg is inline code / list-marker color.
func (t *Theme) InlineFg() string { return t.fg(t.inlineFg()) }

func (t *Theme) inlineFg() RGB {
	if t.dark() {
		return RGB{0x7d, 0xcf, 0xff} // cyan
	}
	return RGB{0x00, 0x82, 0xaa}
}

// BlockFg is the blockquote marker / dim element color.
func (t *Theme) BlockFg() string { return t.fg(t.FGSecondary()) }

// HeadingFg is the heading color.
func (t *Theme) HeadingFg() string { return t.fg(t.FG()) }

// LinkFg is link color.
func (t *Theme) LinkFg() string {
	return t.fg(t.linkFg())
}

func (t *Theme) linkFg() RGB {
	if t.dark() {
		return RGB{0x7a, 0xa6, 0xda}
	}
	return RGB{0x2f, 0x64, 0xd2}
}

// ── Detection ─────────────────────────────────────────────────────────────

var detected struct {
	once sync.Once
	lv   Level
}

// Detect reports the terminal's color capability. NO_COLOR and
// RYSH_COLOR_LEVEL are re-read on every call (so a test or a config reload
// takes effect immediately); the COLORTERM/TERM auto-detection is cached
// after the first call. A SetLevel override always wins.
func Detect() Level {
	if forceLevel != nil {
		return *forceLevel
	}
	if l, ok := envOverride(); ok {
		return l
	}
	detected.once.Do(func() {
		detected.lv = detectLevel()
	})
	return detected.lv
}

// envOverride reads the explicit NO_COLOR / RYSH_COLOR_LEVEL pins fresh.
func envOverride() (Level, bool) {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return LNone, true
	}
	if v := strings.ToLower(os.Getenv("RYSH_COLOR_LEVEL")); v != "" {
		switch v {
		case "none":
			return LNone, true
		case "16", "basic":
			return LBasic, true
		case "256":
			return LAnsi256, true
		case "truecolor", "rgb", "24bit":
			return LTrueColor, true
		}
	}
	return LNone, false
}

// detectLevel is the cached auto-detection (no env pins): COLORTERM/TERM.
func detectLevel() Level {
	if ct := strings.ToLower(os.Getenv("COLORTERM")); ct == "truecolor" || ct == "24bit" {
		return LTrueColor
	}
	term := strings.ToLower(os.Getenv("TERM"))
	if term == "" || term == "dumb" {
		return LNone
	}
	if strings.Contains(term, "truecolor") || strings.Contains(term, "24bit") {
		return LTrueColor
	}
	// 256-color is the safe floor for any other colorful terminal: emitting
	// 24-bit codes to a 256-only terminal would silently mangle every color,
	// which is exactly the "unreadable on some screens" failure. We do NOT
	// assume truecolor unless it is advertised, so a 256-color screen still
	// gets a readable quantized palette.
	return LAnsi256
}
