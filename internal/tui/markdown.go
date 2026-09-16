package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Auto-memory files are Markdown — headings, lists, fenced code with
// paths and commands — and the preview used to show them raw. This is
// the renderer.
//
// It is bffs's own rather than Glamour because the browser ships in the
// same binary as the claude shim, so every package linked here pays its
// init on every claude launch: Glamour measured +6.8 ms p50 (7 → 14 ms)
// against a budget of +5 ms, almost all of it chroma's lexer and style
// registries plus bluemonday. What Glamour would add over this is
// syntax highlighting — which the reference TUI's own Glamour style turns off,
// because a hardcoded hex theme fights the terminal's — and pipe
// tables, which pass through here as the aligned text they already are.
//
// Everything is laid out for a pane of a given width and nothing is
// ever wider than it (clampLines is the backstop).

// renderMarkdown lays a document out for a pane width cells wide.
func renderMarkdown(src string, width int) []string {
	if width < 4 {
		width = 4
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if char, count, indent, info, ok := fenceOpen(line); ok {
			if body, next, closed := fenceBody(lines, i, char, count, indent); closed {
				out = append(out, codePanel(codeBlock{lang: info, body: body}, width)...)
				i = next
				continue
			}
			// An unterminated fence is a malformed document, not a
			// block: render the line as text rather than swallowing
			// the rest of the file into a panel.
		}

		switch {
		case strings.TrimSpace(line) == "":
			out = append(out, "")
		case isRule(line):
			out = append(out, styleFaint.Render(strings.Repeat(glyph.ruleH, min(width, 12))))
		case headingLevel(line) > 0:
			out = append(out, heading(line, width))
		case strings.HasPrefix(strings.TrimLeft(line, " "), ">"):
			block, next := gather(lines, i, func(l string) bool { return strings.HasPrefix(strings.TrimLeft(l, " "), ">") })
			for _, q := range block {
				q = strings.TrimPrefix(strings.TrimLeft(q, " "), ">")
				q = strings.TrimPrefix(q, " ")
				out = append(out, wrapSpans(inlineSpans(q), width, styleBorder.Render(glyph.quote)+" ", styleBorder.Render(glyph.quote)+" ")...)
			}
			i = next
		case listMarker(line) != "":
			item, next := listItem(lines, i, width)
			out = append(out, item...)
			i = next
		default:
			block, next := gather(lines, i, func(l string) bool { return isParagraphLine(l) })
			out = append(out, wrapSpans(inlineSpans(strings.Join(block, " ")), width, "", "")...)
			i = next
		}
	}
	return clampLines(out, width)
}

// isParagraphLine reports whether a line continues a paragraph rather
// than starting a block of its own.
func isParagraphLine(l string) bool {
	if strings.TrimSpace(l) == "" || isRule(l) || headingLevel(l) > 0 || listMarker(l) != "" {
		return false
	}
	if _, _, _, _, ok := fenceOpen(l); ok {
		return false
	}
	return !strings.HasPrefix(strings.TrimLeft(l, " "), ">")
}

// gather collects the run of consecutive lines from i that belong to
// one block, and returns the index of its last line.
func gather(lines []string, i int, keep func(string) bool) ([]string, int) {
	j := i + 1
	for j < len(lines) && keep(lines[j]) {
		j++
	}
	return lines[i:j], j - 1
}

// fenceBody collects a fenced block's lines, reporting whether the
// fence was ever closed.
func fenceBody(lines []string, i int, char byte, count, indent int) (body string, last int, closed bool) {
	var got []string
	for j := i + 1; j < len(lines); j++ {
		if fenceCloses(lines[j], char, count) {
			return strings.Join(got, "\n"), j, true
		}
		got = append(got, trimIndent(lines[j], indent))
	}
	return "", i, false
}

// headingLevel is the ATX heading level of a line, 0 when it is not one.
func headingLevel(line string) int {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0
	}
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n == len(t) || t[n] != ' ' {
		return 0
	}
	return n
}

// heading renders one heading. Three rungs with two signals each: H1
// accent, bold and upper-cased; H2 accent and bold; H3 the section
// colour and bold; deeper ones plain, so a document that nests six
// levels does not read as six shouts.
func heading(line string, width int) string {
	level := headingLevel(line)
	text := strings.TrimSpace(strings.TrimLeft(strings.TrimLeft(line, " "), "#"))
	text = strings.TrimRight(text, " #")
	plain := plainSpans(inlineSpans(text))
	switch level {
	case 1:
		return styleHeader.Render(truncate(strings.ToUpper(plain), width))
	case 2:
		return styleHeader.Render(truncate(plain, width))
	case 3:
		return styleSection.Render(truncate(plain, width))
	default:
		return styleFaint.Render(truncate(plain, width))
	}
}

// isRule reports whether a line is a thematic break.
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	return strings.Trim(t, string(c)+" ") == ""
}

// listMarker returns the bullet or number a line opens a list item
// with, "" when it opens none.
func listMarker(line string) string {
	t := strings.TrimLeft(line, " ")
	switch {
	case len(t) > 1 && (t[0] == '-' || t[0] == '*' || t[0] == '+') && t[1] == ' ':
		return t[:1]
	}
	for i := 0; i < len(t) && i < 9; i++ {
		if t[i] >= '0' && t[i] <= '9' {
			continue
		}
		if i > 0 && (t[i] == '.' || t[i] == ')') && i+1 < len(t) && t[i+1] == ' ' {
			return t[:i+1]
		}
		break
	}
	return ""
}

// listItem renders one item and the lines that continue it: the marker
// in the accent colour, the text wrapped with a hanging indent so a
// second line lines up under the first rather than under the bullet.
// Nesting is the item's own indentation, two columns per level.
func listItem(lines []string, i, width int) ([]string, int) {
	line := lines[i]
	marker := listMarker(line)
	indent := (len(line) - len(strings.TrimLeft(line, " "))) / 2 * 2
	text := strings.TrimSpace(strings.TrimLeft(line, " ")[len(marker):])

	// Continuation lines are indented further and are not themselves a
	// new item or block.
	j := i + 1
	for j < len(lines) && listMarker(lines[j]) == "" && isParagraphLine(lines[j]) &&
		strings.TrimSpace(lines[j]) != "" && len(lines[j])-len(strings.TrimLeft(lines[j], " ")) > indent {
		text += " " + strings.TrimSpace(lines[j])
		j++
	}

	bullet := glyph.bullet
	if marker != "-" && marker != "*" && marker != "+" {
		bullet = marker // an ordered list keeps its own number
	}
	pad := strings.Repeat(" ", indent)
	first := pad + styleAccent.Render(bullet) + " "
	cont := pad + strings.Repeat(" ", lipgloss.Width(bullet)+1)
	return wrapSpans(inlineSpans(text), width, first, cont), j - 1
}

// span is a run of text with one style. Inline markup is laid out as
// spans so a line can be wrapped by its cells and styled per run —
// wrapping already-styled text would count escape bytes as cells.
type span struct {
	text  string
	style lipgloss.Style
}

// inlineSpans splits one line into styled runs: `code`, **strong**,
// *emphasis*, [text](url) and escapes. An unmatched marker stays
// literal, which is what a memory file full of globs, asterisks and
// snake_case identifiers needs.
func inlineSpans(s string) []span {
	var spans []span
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			spans = append(spans, span{text: buf.String()})
			buf.Reset()
		}
	}
	add := func(text string, style lipgloss.Style) {
		flush()
		spans = append(spans, span{text: text, style: style})
	}
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_[]()#+-.!>", s[i+1]) >= 0:
			buf.WriteByte(s[i+1])
			i += 2
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				add(s[i+1:i+1+j], styleCodeSpan)
				i += j + 2
				continue
			}
			buf.WriteByte(s[i])
			i++
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "__"):
			marker := s[i : i+2]
			if j := strings.Index(s[i+2:], marker); j > 0 {
				add(s[i+2:i+2+j], styleStrong)
				i += j + 4
				continue
			}
			buf.WriteString(marker)
			i += 2
		case s[i] == '*', s[i] == '_' && (i == 0 || s[i-1] == ' '):
			if j := strings.IndexByte(s[i+1:], s[i]); j > 0 && !strings.Contains(s[i+1:i+1+j], " \n") {
				add(s[i+1:i+1+j], styleEmph)
				i += j + 2
				continue
			}
			buf.WriteByte(s[i])
			i++
		case s[i] == '[':
			if text, target, n, ok := inlineLink(s[i:]); ok {
				add(text, styleLink)
				if target != "" && target != text {
					add(" ("+target+")", styleFaint)
				}
				i += n
				continue
			}
			buf.WriteByte(s[i])
			i++
		default:
			buf.WriteByte(s[i])
			i++
		}
	}
	flush()
	return spans
}

// inlineLink parses "[text](target)" at the head of s.
func inlineLink(s string) (text, target string, n int, ok bool) {
	close := strings.IndexByte(s, ']')
	if close < 0 || close+1 >= len(s) || s[close+1] != '(' {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[close+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	return s[1:close], s[close+2 : close+2+end], close + 2 + end + 1, true
}

// plainSpans is the unstyled text of a run of spans.
func plainSpans(spans []span) string {
	var b strings.Builder
	for _, sp := range spans {
		b.WriteString(sp.text)
	}
	return b.String()
}

// wrapSpans lays spans out in width cells, styling each run as it goes.
// first prefixes the first line and cont every line after it (already
// styled, and counted by its cells), which is what gives a list item
// its hanging indent.
func wrapSpans(spans []span, width int, first, cont string) []string {
	prefix, prefixW := first, lipgloss.Width(first)
	budget := max(4, width-prefixW)
	var out []string
	var line strings.Builder
	used := 0
	newline := func() {
		out = append(out, prefix+line.String())
		line.Reset()
		used = 0
		prefix, prefixW = cont, lipgloss.Width(cont)
		budget = max(4, width-prefixW)
	}
	for _, sp := range spans {
		for _, word := range splitWords(sp.text) {
			w := lipgloss.Width(word)
			switch {
			case word == " ":
				if used > 0 && used < budget {
					line.WriteString(" ")
					used++
				}
			case used+w <= budget || used == 0:
				line.WriteString(sp.style.Render(word))
				used += w
			default:
				newline()
				line.WriteString(sp.style.Render(word))
				used = w
			}
		}
	}
	if used > 0 || len(out) == 0 {
		out = append(out, prefix+line.String())
	}
	return out
}

// splitWords splits text into words and the single spaces between
// them, so a wrap point is always between two words.
func splitWords(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], ' ')
		switch {
		case j < 0:
			out = append(out, s[i:])
			i = len(s)
		case j == 0:
			out = append(out, " ")
			i++
		default:
			out = append(out, s[i:i+j], " ")
			i += j + 1
		}
	}
	return out
}

// clampLines is the backstop every rendered document passes through:
// prose is wrapped, but a long URL or an unbreakable token can still
// run past the pane, and nothing in the frame is allowed to.
func clampLines(lines []string, width int) []string {
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			l = ansi.Truncate(l, width, glyph.ellipsis)
		}
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}
