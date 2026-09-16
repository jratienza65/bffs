package tui

import (
	"runtime"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// A path in a preview is a link the terminal can open. The URL is built
// from a path that may have been recorded on another machine, so it is
// sanitised and escaped before it is handed to the terminal to act on.
func TestFileURL(t *testing.T) {
	// What counts as a local path is the platform's business: a rooted
	// POSIX path is not absolute on Windows, and a path from another
	// machine is not a file here — so it gets no link either way.
	cases := []struct{ in, want string }{
		{"/home/d/.claude/projects/x", "file:///home/d/.claude/projects/x"},
		{"/home/d/my notes/MEMORY.md", "file:///home/d/my%20notes/MEMORY.md"},
		{"/home/d/a#b?c", "file:///home/d/a%23b%3Fc"},

		// Sanitize drops the whole sequence, payload included, not just
		// the escape byte that opens it.
		{"/home/d/\x1b]52;c;cGF5bG9hZA==\x07evil", "file:///home/d/evil"},
	}
	if runtime.GOOS == "windows" {
		cases = []struct{ in, want string }{
			{`C:\Users\d\.claude\projects\x`, "file:///C:/Users/d/.claude/projects/x"},
			{`C:\Users\d\my notes\MEMORY.md`, "file:///C:/Users/d/my%20notes/MEMORY.md"},
			{"/home/d/.claude/projects/x", ""}, // rooted, but not a path here
		}
	}
	cases = append(cases,
		struct{ in, want string }{"relative/path", ""},
		struct{ in, want string }{"", ""},
	)
	for _, tc := range cases {
		if got := fileURL(tc.in); got != tc.want {
			t.Errorf("fileURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, path := range []string{"/home/d/\x1b]8;;http://evil\x07", "/home/d/\x07bell"} {
		if got := fileURL(path); strings.ContainsAny(got, "\x1b\x07") {
			t.Errorf("fileURL(%q) carried an escape: %q", path, got)
		}
	}
}

func TestLinkRendering(t *testing.T) {
	plain := lipgloss.NewStyle()
	got := link(plain, "MEMORY.md", "file:///home/d/MEMORY.md")
	if !strings.Contains(got, "\x1b]8;;file:///home/d/MEMORY.md") {
		t.Errorf("no OSC 8 target in %q", got)
	}
	if !strings.Contains(got, "MEMORY.md") || !strings.Contains(got, glyph.open) {
		t.Errorf("the label or its mark is missing: %q", got)
	}
	// Without a target the text reads the same, so a row whose path is
	// not on this machine does not look like a broken link.
	if got := link(plain, "MEMORY.md", ""); got != "MEMORY.md" {
		t.Errorf("unlinked text = %q", got)
	}
	// BFFS_ASCII has no mark to spare.
	t.Setenv("BFFS_ASCII", "1")
	resolveGlyphs()
	t.Cleanup(func() { t.Setenv("BFFS_ASCII", ""); resolveGlyphs() })
	if got := link(plain, "x", "file:///x"); strings.Contains(got, "↗") {
		t.Errorf("BFFS_ASCII still drew the link mark: %q", got)
	}
}
