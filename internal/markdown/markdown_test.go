package markdown

import (
	"strings"
	"testing"
)

// render all of s at once through Write.
func render(t *testing.T, s string) string {
	t.Helper()
	r := New()
	return r.Write(s) + r.Close()
}

func TestPlainText(t *testing.T) {
	if got := render(t, "hello world\n"); got != "hello world\x1b[0m\n" {
		t.Errorf("plain text = %q, want %q", got, "hello world\x1b[0m\n")
	}
}

func TestBlankLinesKept(t *testing.T) {
	if got := render(t, "a\n\nb\n"); got != "a\x1b[0m\n\nb\x1b[0m\n" {
		t.Errorf("blank lines = %q", got)
	}
}

func TestBold(t *testing.T) {
	if got := render(t, "**bold**\n"); got != "\x1b[1mbold\x1b[0m\n" {
		t.Errorf("bold = %q", got)
	}
	if got := render(t, "a **bold** b\n"); got != "a \x1b[1mbold\x1b[0m b\x1b[0m\n" {
		t.Errorf("bold inline = %q", got)
	}
	if got := render(t, "__bold__\n"); got != "\x1b[1mbold\x1b[0m\n" {
		t.Errorf("underscore bold = %q", got)
	}
}

func TestItalic(t *testing.T) {
	if got := render(t, "*italic*\n"); got != "\x1b[3mitalic\x1b[0m\n" {
		t.Errorf("italic = %q", got)
	}
	if got := render(t, "_italic_\n"); got != "\x1b[3mitalic\x1b[0m\n" {
		t.Errorf("underscore italic = %q", got)
	}
}

func TestBoldAndItalicSameLine(t *testing.T) {
	got := render(t, "**bold** and *italic*\n")
	want := "\x1b[1mbold\x1b[0m and \x1b[3mitalic\x1b[0m\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIntrawordLiteral(t *testing.T) {
	for _, s := range []string{"foo_bar\n", "5*6\n", "a _ b\n"} {
		if got := render(t, s); got != s[:len(s)-1]+"\x1b[0m\n" {
			t.Errorf("intraword %q = %q", s, got)
		}
	}
}

// In CJK text the emphasis markers sit between letters (是**加粗**文) but
// are still intended emphasis: the intraword guard only covers ASCII
// identifiers like foo_bar.
func TestCJKEmphasis(t *testing.T) {
	got := render(t, "这是**加粗**文字和*斜体*\n")
	want := "这是\x1b[1m加粗\x1b[0m文字和\x1b[3m斜体\x1b[0m\n"
	if got != want {
		t.Errorf("CJK emphasis = %q, want %q", got, want)
	}
}

func TestInlineCode(t *testing.T) {
	want := "use " + inlineCode() + "fmt.Println" + reset + " here" + reset + "\n"
	if got := render(t, "use `fmt.Println` here\n"); got != want {
		t.Errorf("inline code = %q, want %q", got, want)
	}
}

func TestLink(t *testing.T) {
	got := render(t, "see [docs](https://x.test)\n")
	want := "see " + underline + "docs" + reset + dim() + " (https://x.test)" + reset + reset + "\n"
	if got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
}

func TestHeading(t *testing.T) {
	if got := render(t, "## Title\n"); got != "\x1b[1;4mTitle\x1b[0m\n" {
		t.Errorf("heading = %q", got)
	}
	// seven hashes is not a heading.
	if got := render(t, "####### nope\n"); got != "####### nope\x1b[0m\n" {
		t.Errorf("non-heading = %q", got)
	}
	// hash without a following space is not a heading either.
	if got := render(t, "#nope\n"); got != "#nope\x1b[0m\n" {
		t.Errorf("hash-no-space = %q", got)
	}
}

func TestList(t *testing.T) {
	if got := render(t, "- item\n"); got != inlineCode()+"- "+reset+"item"+reset+"\n" {
		t.Errorf("dash list = %q", got)
	}
	if got := render(t, "1. first\n"); got != inlineCode()+"1. "+reset+"first"+reset+"\n" {
		t.Errorf("ordered list = %q", got)
	}
	if got := render(t, "* item\n"); got != inlineCode()+"* "+reset+"item"+reset+"\n" {
		t.Errorf("star list = %q", got)
	}
}

func TestBlockquote(t *testing.T) {
	got := render(t, "> quoted text\n")
	want := dim() + ">" + reset + " quoted text" + reset + "\n"
	if got != want {
		t.Errorf("blockquote = %q, want %q", got, want)
	}
}

func TestHorizontalRule(t *testing.T) {
	if got := render(t, "---\n"); got != dim()+"---"+reset+"\n" {
		t.Errorf("hr = %q", got)
	}
	if got := render(t, "** *\n"); got != dim()+"** *"+reset+"\n" {
		t.Errorf("hr spaced = %q", got)
	}
}

func TestEscapes(t *testing.T) {
	if got := render(t, `\*not italic\*`+"\n"); got != "*not italic*\x1b[0m\n" {
		t.Errorf("escaped asterisks = %q", got)
	}
}

func TestFencedCode(t *testing.T) {
	r := New()
	if got := r.Write("```go\n"); got != "" {
		t.Errorf("opener = %q, want empty", got)
	}
	if got := r.Write("func main() {}\n"); got != codeBG()+"func main() {}"+reset+"\n" {
		t.Errorf("code line = %q", got)
	}
	if got := r.Write("```\n"); got != "" {
		t.Errorf("closer = %q, want empty", got)
	}
	if got := r.Write("after\n"); got != "after"+reset+"\n" {
		t.Errorf("after close = %q", got)
	}
	if got := r.Close(); got != "" {
		t.Errorf("close = %q, want empty", got)
	}
}

func TestFencedCodeWholeChunk(t *testing.T) {
	s := "```\ncode\n```\nback\n"
	got := render(t, s)
	want := codeBG() + "code" + reset + "\nback" + reset + "\n"
	if got != want {
		t.Errorf("whole chunk = %q, want %q", got, want)
	}
}

func TestCodeTildeFence(t *testing.T) {
	if got := render(t, "~~~\nx\n~~~\n"); got != codeBG()+"x"+reset+"\n" {
		t.Errorf("tilde fence = %q", got)
	}
}

func TestStreamingHoldsOpenConstructs(t *testing.T) {
	r := New()
	// A bold opener split across chunks must not flash the markers.
	if got := r.Write("**bo"); got != "" {
		t.Errorf("held chunk = %q, want empty", got)
	}
	if got := r.Write("ld**\n"); got != "\x1b[1mbold\x1b[0m\n" {
		t.Errorf("completed = %q", got)
	}
	// An inline code span split across chunks.
	r = New()
	if got := r.Write("run `go test"); got != "" {
		t.Errorf("code split = %q, want empty", got)
	}
	if got := r.Write("` now\n"); got != "run "+inlineCode()+"go test"+reset+" now"+reset+"\n" {
		t.Errorf("code split done = %q", got)
	}
}

func TestStreamingMidWord(t *testing.T) {
	r := New()
	if got := r.Write("hel"); got != "hel" {
		t.Errorf("mid word = %q", got)
	}
	if got := r.Write("lo **bo"); got != "" {
		t.Errorf("mid word 2 = %q, want empty (bold open held)", got)
	}
	if got := r.Write("ld**\n"); got != "lo \x1b[1mbold\x1b[0m\n" {
		t.Errorf("mid word done = %q", got)
	}
}

func TestCloseFlushesHeld(t *testing.T) {
	r := New()
	if got := r.Write("**bo"); got != "" {
		t.Errorf("held = %q", got)
	}
	if got := r.Close(); got != "\x1b[1mbo\x1b[0m" {
		t.Errorf("close flush = %q", got)
	}
}

func TestCloseUnclosedCodeBlock(t *testing.T) {
	r := New()
	r.Write("```\n")
	r.Write("left")
	if got := r.Close(); got != codeBG()+"left"+reset {
		t.Errorf("unclosed code flush = %q", got)
	}
}

func TestFenceOpenerHeldAtChunkEnd(t *testing.T) {
	r := New()
	// A fence opener split across chunks must not flash raw backticks.
	if got := r.Write("```go"); got != "" {
		t.Errorf("fence hold = %q, want empty", got)
	}
	if got := r.Write("\nfunc main() {}\n"); got != codeBG()+"func main() {}"+reset+"\n" {
		t.Errorf("fence done = %q", got)
	}
	if got := r.Write("```\n"); got != "" {
		t.Errorf("fence close = %q, want empty", got)
	}
	if got := r.Write("after\n"); got != "after\x1b[0m\n" {
		t.Errorf("after close = %q", got)
	}
}

func TestNoResetLeakAcrossLines(t *testing.T) {
	got := render(t, "**bold**\nplain\n")
	want := "\x1b[1mbold\x1b[0m\nplain\x1b[0m\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// join renders a stream with a given rune chunk size to simulate token
// boundaries at every position.
func join(t *testing.T, s string, size int) string {
	t.Helper()
	r := New()
	var out strings.Builder
	// Split at rune boundaries: a real stream's deltas are valid UTF-8,
	// so a chunk never cuts a multibyte character in half.
	runes := []rune(s)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		out.WriteString(r.Write(string(runes[i:end])))
	}
	out.WriteString(r.Close())
	return out.String()
}

// stripANSI removes terminal escape sequences so tests can compare the
// visible text of a render regardless of where styles were restarted.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) && (s[i] == ';' || (s[i] >= '0' && s[i] <= '9')) {
				i++
			}
			continue // the loop's i++ consumes the final byte (e.g. 'm')
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// TestChunkBoundaryInvariance renders several markdown samples at every
// possible chunk split point and asserts the visible text is identical to
// a single-chunk render. Styling bytes may differ (a split construct is
// emitted as one piece once it closes), so only the visible text matches.
func TestChunkBoundaryInvariance(t *testing.T) {
	samples := []string{
		"**bold** text\n",
		"*italic* and `code` here\n",
		"see [docs](https://x.test) now\n",
		"# Head\n\npara with **bold**.\n\n- one\n- two\n\n> quote\n\n---\n",
		"```go\nfmt.Println(\"hi\")\n```\ndone\n",
		"plain text without markers\n",
		"foo_bar and 5*6 and _a_ **b**\n",
		"## Streaming Title\n",
		"escaped \\*star\\* and `in code`\n",
		"这是**加粗**文字和*斜体*，还有`内联代码`\n",
		"加**粗**与_强调_混合\n",
	}
	for _, s := range samples {
		ref := stripANSI(render(t, s))
		for size := 1; size <= 8; size++ {
			if got := stripANSI(join(t, s, size)); got != ref {
				t.Errorf("sample %q chunk %d:\n got %q\nwant %q", s, size, got, ref)
			}
		}
	}
}
