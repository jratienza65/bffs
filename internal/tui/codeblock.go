package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A fenced code block, lifted out of a document by renderMarkdown and
// drawn as a panel: the language named in the top border the way a diff
// viewer labels a hunk, and every line that does not fit folded rather
// than reflowed. An unmarked fold reads as two separate commands, and
// shell snippets are what gets copied out of a memory file.
type codeBlock struct {
	lang string
	body string
}

// fenceOpen reports whether the line opens a fence, and with what.
// Fences are recognised at an indent of 0-3 columns, opened by three or
// more backticks or tildes — the CommonMark rule for a top-level fence.
func fenceOpen(line string) (char byte, count, indent int, info string, ok bool) {
	indent = len(line) - len(strings.TrimLeft(line, " "))
	if indent > 3 {
		return 0, 0, 0, "", false
	}
	rest := line[indent:]
	if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, 0, "", false
	}
	char = rest[0]
	for count = 0; count < len(rest) && rest[count] == char; count++ {
	}
	if count < 3 {
		return 0, 0, 0, "", false
	}
	info = strings.TrimSpace(rest[count:])
	// An info string on a backtick fence may not itself contain a backtick.
	if char == '`' && strings.Contains(info, "`") {
		return 0, 0, 0, "", false
	}
	if i := strings.IndexAny(info, " \t"); i >= 0 {
		info = info[:i]
	}
	return char, count, indent, info, true
}

// fenceCloses reports whether the line closes a fence opened with char/count.
func fenceCloses(line string, char byte, count int) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == char {
		n++
	}
	return n >= count && strings.TrimSpace(trimmed[n:]) == ""
}

// trimIndent removes up to n leading spaces, the fence's own indentation.
func trimIndent(line string, n int) string {
	for i := 0; i < n && strings.HasPrefix(line, " "); i++ {
		line = line[1:]
	}
	return line
}

// codePanel draws one block. Two border columns and a space of padding
// on each side; below that there is no room for a panel, so the code
// falls back to a plain indented block rather than a box drawn in two
// cells.
func codePanel(b codeBlock, width int) []string {
	inner := width - 4
	if inner < 8 {
		var out []string
		for _, l := range foldMarked(b.body, max(1, width-2)) {
			out = append(out, l.mark+styleFaint.Render(l.text))
		}
		return out
	}
	label := ""
	if b.lang != "" {
		label = " " + b.lang + " "
	}
	if lipgloss.Width(label) > inner {
		label = ""
	}
	// The label names the language, so it is text and takes a text
	// token; only the box characters are drawn with the border.
	out := []string{styleBorder.Render(glyph.tl+glyph.h) + styleFaint.Render(label) +
		styleBorder.Render(strings.Repeat(glyph.h, max(0, inner+1-lipgloss.Width(label)))+glyph.tr)}
	for _, line := range foldMarked(b.body, inner) {
		pad := max(0, inner-lipgloss.Width(line.mark)-lipgloss.Width(line.text))
		out = append(out, styleBorder.Render(glyph.v)+" "+line.mark+
			styleCodeSpan.Render(line.text)+strings.Repeat(" ", pad)+" "+styleBorder.Render(glyph.v))
	}
	return append(out, styleBorder.Render(glyph.bl+strings.Repeat(glyph.h, inner+2)+glyph.br))
}

// markedLine is one rendered code row: the text, and the already-styled
// prefix in front of it — two blank columns, or the fold marker.
type markedLine struct {
	mark string
	text string
}

// foldMarked lays a block out in width columns and marks every folded
// row. The marker is only paid for when something folds: the block is
// wrapped at the full width first, and re-wrapped two columns narrower
// only when that produced a continuation.
func foldMarked(body string, width int) []markedLine {
	lines := wrapHard(body, width)
	folded := false
	for _, l := range lines {
		folded = folded || l.cont
	}
	if !folded {
		out := make([]markedLine, len(lines))
		for i, l := range lines {
			out[i] = markedLine{text: l.text}
		}
		return out
	}
	lines = wrapHard(body, max(1, width-2))
	out := make([]markedLine, len(lines))
	for i, l := range lines {
		mark := "  "
		if l.cont {
			mark = styleFaint.Render(glyph.fold + " ")
		}
		out[i] = markedLine{mark: mark, text: l.text}
	}
	return out
}

// codeLine is one row of a laid-out block: its text, and whether it is
// the tail of the row above rather than a line of the block's own.
type codeLine struct {
	text string
	cont bool
}

// wrapHard breaks lines at exactly width cells. Code is not prose:
// breaking on spaces would silently reflow it, so a long line is
// continued rather than rewrapped, and nothing is dropped the way
// truncation would drop it. Each row says whether it is such a
// continuation, so the panel can mark the fold.
//
// The break itself is ansi.Hardwrap, which walks grapheme clusters: a
// combining mark rides along with the cell it modifies and a wide rune
// counts as the two cells it occupies, so a cluster that will not fit
// can never stall the loop. Tabs are expanded here rather than left to
// it — a tab is one cell to the wrapper and four columns on screen.
func wrapHard(s string, width int) []codeLine {
	if width < 1 {
		width = 1
	}
	var out []codeLine
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		line = strings.ReplaceAll(line, "\t", "    ")
		if line == "" {
			out = append(out, codeLine{})
			continue
		}
		for i, part := range strings.Split(ansi.Hardwrap(line, width, true), "\n") {
			out = append(out, codeLine{text: part, cont: i > 0})
		}
	}
	return out
}
