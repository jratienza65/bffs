package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// projectRow is one projects/<slug> directory of a root.
type projectRow struct {
	slug      string
	cwd       string // decoded from the newest transcript; "" when none records one
	cwdExists bool
	sessions  int
	hasMemory bool // memory/MEMORY.md exists
	newest    time.Time
	now       func() time.Time
}

func (r *projectRow) FilterValue() string { return r.cwd + " " + r.slug }

// label is the row's name: the decoded cwd, else the slug. Sanitised:
// the cwd comes from a transcript.
func (r *projectRow) label() string {
	if r.cwd != "" {
		return transcripts.Sanitize(shortPath(r.cwd))
	}
	return transcripts.Sanitize(r.slug)
}

const projectAgeW = 8

// render lays the row out for the side column: a ! for a directory
// missing on this machine, the name, the session count, a "mem" tag and
// the newest age.
func (r *projectRow) render(width int) string {
	mark := " "
	if r.cwd != "" && !r.cwdExists {
		mark = "!"
	}
	mem := "   "
	if r.hasMemory {
		mem = "mem"
	}
	age := ""
	if !r.newest.IsZero() {
		age = humanizeAgo(r.newest, r.now())
	}
	right := fmt.Sprintf("%3d %s %s", r.sessions, mem, pad(age, projectAgeW))
	nameW := width - 2 - 1 - len([]rune(right))
	if nameW < 8 {
		return truncate(mark+" "+r.label(), width)
	}
	return mark + " " + pad(r.label(), nameW) + " " + right
}

// loadProjects lists root's projects/ directory: one fast-path List
// over the root for the counts and newest mtimes (no transcript is
// opened), then per slug a stat of memory/MEMORY.md and the decoded cwd
// (the newest transcript's head window — slugs are lossy).
func loadProjects(ctx context.Context, root transcripts.Root, now func() time.Time) tea.Cmd {
	return func() tea.Msg {
		msg := projectsLoadedMsg{rootDir: root.Dir}
		entries, err := os.ReadDir(root.Dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			msg.err = fmt.Errorf("read %s: %w", root.Dir, err)
			return msg
		}
		ss, err := transcripts.List(ctx, root, transcripts.ListOptions{Now: now()})
		if err != nil {
			msg.err = err
			return msg
		}
		count := map[string]int{}
		newest := map[string]time.Time{}
		for _, s := range ss {
			count[s.Slug]++
			if s.LastTS.After(newest[s.Slug]) {
				newest[s.Slug] = s.LastTS
			}
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				msg.err = ctx.Err()
				return msg
			}
			if !e.IsDir() || transcripts.IsReserved(e.Name()) {
				continue
			}
			slugDir := filepath.Join(root.Dir, e.Name())
			r := &projectRow{slug: e.Name(), sessions: count[e.Name()], newest: newest[e.Name()], now: now}
			if info, err := os.Stat(filepath.Join(slugDir, transcripts.MemorySubdir, transcripts.MemoryIndexFile)); err == nil && info.Mode().IsRegular() {
				r.hasMemory = true
			}
			if cwd, err := transcripts.DecodeCwd(slugDir); err == nil {
				r.cwd = cwd
				r.cwdExists = isDir(cwd)
			}
			if r.sessions == 0 && !r.hasMemory && r.cwd == "" {
				continue // an empty directory: nothing to browse
			}
			msg.rows = append(msg.rows, r)
		}
		sort.SliceStable(msg.rows, func(i, j int) bool {
			a, b := msg.rows[i], msg.rows[j]
			if !a.newest.Equal(b.newest) {
				return a.newest.After(b.newest)
			}
			return a.slug < b.slug
		})
		return msg
	}
}
