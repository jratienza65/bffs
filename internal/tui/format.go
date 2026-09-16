package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

const ellipsis = "…"

// humanizeAgo renders a past instant relative to now the way the tables
// do: "never", "just now", "5m ago", "3h ago", "2d ago".
func humanizeAgo(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// formatSize renders bytes as the tables do: 12 KB, 0.4 MB, 42.6 MB,
// 1.3 GB (decimal units).
func formatSize(n int64) string {
	switch {
	case n < 100_000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
}

// countNoun renders "1 session" / "2 sessions" / "3 entries".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") {
		noun = noun[:len(noun)-1] + "ie"
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// shortPath renders a path relative to $HOME for readability. The result
// is display-only and sanitised: directory names under projects/ come
// from disk and a decoded cwd from a transcript.
func shortPath(p string) string {
	if p == "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return transcripts.Sanitize(p)
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return transcripts.Sanitize(filepath.Join("~", rel))
	}
	return transcripts.Sanitize(p)
}

// truncate cuts s to width terminal cells, ending in an ellipsis when it
// had to. width <= 0 is an empty string. s must be plain text (no escape
// sequences): rows are styled as a whole after they are laid out.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return ellipsis
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if used+w > width-1 {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return strings.TrimRight(b.String(), " ") + ellipsis
}

// pad fits s into exactly width cells: truncated when longer, padded
// with spaces on the right when shorter.
func pad(s string, width int) string {
	s = truncate(s, width)
	if n := width - lipgloss.Width(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// dashIfEmpty is the table convention for an empty cell.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// yesOrDash renders a boolean cell the way `bffs memory` does.
func yesOrDash(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// fitLines pads or cuts a rendered block to exactly height lines, each
// cut at width cells, so the chrome around it never shifts.
func fitLines(s string, width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = lipgloss.NewStyle().MaxWidth(width).Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
