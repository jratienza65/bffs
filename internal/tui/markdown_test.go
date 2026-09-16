package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// render is the renderer's output with the styling stripped, which is
// what these tests are about: the layout a memory file gets.
func renderMD(t *testing.T, src string, width int) []string {
	t.Helper()
	out := renderMarkdown(src, width)
	for i, l := range out {
		if got := lipgloss.Width(l); got > width {
			t.Errorf("line %d is %d cells wide, pane is %d: %q", i, got, width, ansi.Strip(l))
		}
		out[i] = ansi.Strip(l)
	}
	return out
}

func TestMarkdownBlocks(t *testing.T) {
	src := strings.Join([]string{
		"# Memory",
		"",
		"Some prose about the project that runs past the pane width and has to wrap somewhere sensible.",
		"",
		"## Decisions",
		"",
		"- first item",
		"- second item that is long enough to need a second line of its own",
		"  and carries a continuation line",
		"  - nested item",
		"1. ordered one",
		"2. ordered two",
		"",
		"> a quoted line",
		"",
		"---",
		"",
		"### Details",
		"#### deeper",
	}, "\n")
	got := renderMD(t, src, 40)
	joined := strings.Join(got, "\n")

	for _, want := range []string{
		"MEMORY",          // h1 is upper-cased
		"Decisions",       // h2 keeps its case
		"• first item",    // a bullet
		"  • nested item", // nesting is the item's own indent
		"1. ordered one",  // an ordered list keeps its numbers
		"│ a quoted line", // the quote bar
		"Details",
		"deeper",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, strings.Repeat("─", 12)) {
		t.Errorf("no horizontal rule in:\n%s", joined)
	}
	// A wrapped item hangs under its own text, not under the bullet.
	for i, l := range got {
		if strings.HasPrefix(l, "• second item") {
			next := got[i+1]
			if !strings.HasPrefix(next, "  ") || strings.HasPrefix(strings.TrimSpace(next), "•") {
				t.Errorf("continuation of a wrapped item is not hanging: %q", next)
			}
			if !strings.Contains(strings.Join(got[i:], " "), "continuation line") {
				t.Errorf("the item's continuation line was dropped:\n%s", joined)
			}
		}
	}
}

func TestMarkdownInline(t *testing.T) {
	got := strings.Join(renderMD(t, "A **strong** word, an *emphasis*, some `code`, a [link](notes.md).", 70), "\n")
	for _, want := range []string{"strong", "emphasis", "code", "link (notes.md)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, marker := range []string{"**", "`", "]("} {
		if strings.Contains(got, marker) {
			t.Errorf("marker %q survived rendering: %q", marker, got)
		}
	}
	// A memory file is full of globs, snake_case and asterisks that open
	// nothing: an unmatched marker is literal text, never swallowed.
	literal := strings.Join(renderMD(t, "use *.bffs files and read snake_case_names", 70), "\n")
	if !strings.Contains(literal, "*.bffs") || !strings.Contains(literal, "snake_case_names") {
		t.Errorf("an unmatched marker was consumed: %q", literal)
	}
}

func TestMarkdownCodePanel(t *testing.T) {
	src := "text\n\n```bash\nbffs export --out ~/a.bffs\nthis-command-is-far-too-long-for-the-panel-and-has-to-fold-somewhere\n```\n\nafter"
	got := renderMD(t, src, 40)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "bash") {
		t.Errorf("the panel does not name its language:\n%s", joined)
	}
	if !strings.Contains(joined, "┌") || !strings.Contains(joined, "└") {
		t.Errorf("the code block is not in a panel:\n%s", joined)
	}
	if !strings.Contains(joined, "bffs export --out ~/a.bffs") {
		t.Errorf("the code was reflowed or lost:\n%s", joined)
	}
	if !strings.Contains(joined, glyph.fold) {
		t.Errorf("a folded code line is not marked:\n%s", joined)
	}
	if !strings.Contains(joined, "after") {
		t.Errorf("the document does not continue after the panel:\n%s", joined)
	}
	// An unterminated fence is a malformed document, not a block: the
	// rest of the file must still render.
	open := strings.Join(renderMD(t, "```\nnot closed\nstill text", 40), "\n")
	if !strings.Contains(open, "still text") {
		t.Errorf("an unterminated fence swallowed the document:\n%s", open)
	}
}

// Nothing the renderer produces may be wider than the pane — a long URL
// or an unbreakable token is truncated rather than allowed to overflow.
func TestMarkdownNeverOverflows(t *testing.T) {
	src := "# A heading that is much wider than any of the panes below\n\n" +
		"https://example.test/" + strings.Repeat("segment/", 20) + "\n\n" +
		"- an item with an unbreakable " + strings.Repeat("x", 80) + " token\n\n" +
		"```go\n" + strings.Repeat("a", 200) + "\n```\n"
	for _, width := range []int{80, 60, 40, 20, 10, 6, 4} {
		renderMD(t, src, width) // the width assertion lives in the helper
	}
}

// BFFS_ASCII swaps every symbol the renderer draws.
func TestMarkdownAsciiMode(t *testing.T) {
	t.Setenv("BFFS_ASCII", "1")
	resolveGlyphs()
	t.Cleanup(func() { t.Setenv("BFFS_ASCII", ""); resolveGlyphs() })
	got := strings.Join(renderMD(t, "- item\n\n```\ncode\n```\n\n---", 40), "\n")
	if strings.ContainsAny(got, "•┌─┘│") {
		t.Errorf("BFFS_ASCII=1 still drew symbols:\n%s", got)
	}
	if !strings.Contains(got, "- item") || !strings.Contains(got, "+") {
		t.Errorf("the ASCII set did not stand in:\n%s", got)
	}
}
