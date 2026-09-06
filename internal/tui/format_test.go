package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func TestTruncateAndPad(t *testing.T) {
	if got := truncate("short", 40); got != "short" {
		t.Errorf("short: %q", got)
	}
	long := strings.Repeat("ab ", 20)
	got := truncate(long, 40)
	if len([]rune(got)) > 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("long: %q (%d runes)", got, len([]rune(got)))
	}
	if got := truncate("ééééé", 3); got != "éé…" {
		t.Errorf("runes: %q", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Errorf("zero width: %q", got)
	}
	if got := pad("ab", 5); got != "ab   " {
		t.Errorf("pad: %q", got)
	}
	if got := pad("abcdefgh", 5); got != "abcd…" {
		t.Errorf("pad cuts: %q", got)
	}
	// Wide runes count as two cells.
	if got := pad("日本", 6); got != "日本  " {
		t.Errorf("wide pad: %q", got)
	}
}

func TestFormatHelpers(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if got := humanizeAgo(now.Add(-2*time.Hour), now); got != "2h ago" {
		t.Errorf("humanizeAgo = %q", got)
	}
	if got := humanizeAgo(time.Time{}, now); got != "never" {
		t.Errorf("humanizeAgo zero = %q", got)
	}
	for n, want := range map[int64]string{0: "0 KB", 400_000: "0.4 MB", 42_600_000: "42.6 MB", 1_300_000_000: "1.3 GB"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
	if got := countNoun(1, "entry"); got != "1 entry" {
		t.Errorf("countNoun = %q", got)
	}
	if got := countNoun(3, "entry"); got != "3 entries" {
		t.Errorf("countNoun = %q", got)
	}
	if got := fitLines("a\nb\nc", 10, 2); got != "a\nb" {
		t.Errorf("fitLines cut = %q", got)
	}
	if got := fitLines("a", 10, 3); got != "a\n\n" {
		t.Errorf("fitLines pad = %q", got)
	}
}

// A hostile title is sanitised in the row and in the detail lines: the
// OSC 52 clipboard write never reaches the rendered text.
func TestRowsSanitize(t *testing.T) {
	const osc52 = "\x1b]52;c;aGVsbG8=\x07"
	now := func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	s := transcripts.Session{ID: "1e005053-380a-4245-a145-52c2715afa73", Title: osc52 + "Plan: session export", GitBranch: osc52 + "main", Account: "aviate", Size: 42_600_000, LastTS: now().Add(-2 * time.Hour)}
	r := &sessionRow{s: s, resolved: true, sel: map[string]bool{}, now: now}
	line := r.render(60)
	for _, want := range []string{"Plan: session export", "2h ago"} {
		if !strings.Contains(line, want) {
			t.Errorf("row missing %q: %q", want, line)
		}
	}
	if strings.Contains(line, "\x1b") || strings.Contains(line, "52;c;") {
		t.Errorf("escape reached the row: %q", line)
	}
	// Account, size and branch belong to the preview, not the row; a
	// marked live row carries its two glyphs.
	if strings.Contains(line, "aviate") || strings.Contains(line, "42.6 MB") {
		t.Errorf("row carries preview fields: %q", line)
	}
	r.sel[s.ID] = true
	r.s.Live = true
	if marked := r.render(60); !strings.HasPrefix(marked, "*● Plan") {
		t.Errorf("marked live row = %q", marked)
	}
	// Unresolved rows show the id until the windows are read.
	r.resolved = false
	if !strings.Contains(r.render(120), "(1e005053)") {
		t.Errorf("placeholder missing: %q", r.render(120))
	}

	s.Cwd = osc52 + "/tmp/x"
	s.Root = transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Owner: "work"}
	lines := strings.Join(detailLines(sessionDetail{Session: s}, now()), "\n")
	if strings.Contains(lines, "\x1b") {
		t.Errorf("escape reached the detail: %q", lines)
	}
	if !strings.Contains(lines, "resume:       cd /tmp/x && BFFS_ACCOUNT=work claude --resume "+s.ID) {
		t.Errorf("resume line:\n%s", lines)
	}
	if !strings.Contains(lines, "title:        Plan: session export  (") {
		t.Errorf("title line:\n%s", lines)
	}
}

func TestScanPathLines(t *testing.T) {
	refs := []transcripts.PathRef{
		{File: "notes.md", Line: 3, Path: "/Users/jonas/x", Kind: transcripts.PathKindAbs},
		{File: "MEMORY.md", Line: 1, Path: "@~/notes.md", Kind: transcripts.PathKindAt},
		{File: "\x1b[2Jevil.md", Line: 9, Path: "/tmp/\x1b]52;c;x\x07y", Kind: transcripts.PathKindAbs},
	}
	lines := scanPathLines(refs)
	if lines[0] != "notes.md:3: /Users/jonas/x" || lines[1] != "@ref MEMORY.md:1: @~/notes.md" {
		t.Errorf("lines = %q", lines)
	}
	if strings.Contains(lines[2], "\x1b") || lines[2] != "evil.md:9: /tmp/y" {
		t.Errorf("hostile line = %q", lines[2])
	}
}

func TestShellWord(t *testing.T) {
	if got := shellWord("/a/b"); got != "/a/b" {
		t.Errorf("plain: %q", got)
	}
	if got := shellWord("/a/my project's"); got != `'/a/my project'\''s'` {
		t.Errorf("quoted: %q", got)
	}
	if got := shellWord(""); got != "''" {
		t.Errorf("empty: %q", got)
	}
}

// Every row type fills exactly the width it is given, at every width:
// a row one cell too wide loses its last column to the cursor gutter.
func TestRowsFillWidth(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	rows := []row{
		&sessionRow{s: transcripts.Session{ID: "1e005053-380a-4245-a145-52c2715afa73", Title: "Plan", Account: "aviate", GitBranch: "main", LastTS: now().Add(-time.Hour)}, resolved: true, sel: map[string]bool{}, now: now},
		&projectRow{slug: "-Users-x", cwd: "/Users/x", sessions: 3, hasMemory: true, newest: now().Add(-time.Hour), now: now},
		&memoryFileRow{f: transcripts.MemoryFile{Name: "notes.md", Size: 10, ModTime: now(), Pinned: true, AbsolutePaths: []string{"/a"}, AtRefs: []string{"@/b"}}, svc: &services{now: now}},
		&accountRow{name: "aviate", kind: "partial", active: true},
	}
	for _, r := range rows {
		for _, w := range []int{20, 40, 60, 118, 298} {
			if got := pad(r.render(w), w); lipglossWidth(got) != w {
				t.Errorf("%T at width %d renders %d cells: %q", r, w, lipglossWidth(got), got)
			}
			if got := r.render(w); lipglossWidth(got) > w {
				t.Errorf("%T at width %d overflows to %d cells: %q", r, w, lipglossWidth(got), got)
			}
		}
	}
}
