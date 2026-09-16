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

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
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

const (
	projectSessionsW = 12
	projectMemoryW   = 6
	projectAgeW      = 9
	projectFixedW    = 2 + 1 + projectSessionsW + 1 + projectMemoryW + 1 + projectAgeW
)

func (r *projectRow) render(width int) string {
	mark := "  "
	if r.cwd != "" && !r.cwdExists {
		mark = "! "
	}
	memory := ""
	if r.hasMemory {
		memory = "MEMORY"
	}
	age := ""
	if !r.newest.IsZero() {
		age = humanizeAgo(r.newest, r.now())
	}
	nameW := width - projectFixedW
	if nameW < 12 {
		return truncate(mark+r.label(), width)
	}
	return mark + pad(r.label(), nameW) + " " + pad(countNoun(r.sessions, "session"), projectSessionsW) +
		" " + pad(memory, projectMemoryW) + " " + pad(age, projectAgeW)
}

// projectsHeader is the column header laid out like the rows.
func projectsHeader(width int) string {
	nameW := width - projectFixedW
	if nameW < 12 {
		return "PROJECT"
	}
	return "  " + pad("PROJECT", nameW) + " " + pad("SESSIONS", projectSessionsW) + " " + pad("MEMORY", projectMemoryW) + " " + pad("NEWEST", projectAgeW)
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

// projectsScreen lists the projects of one root, newest first, with the
// cursor on the process's own project when it is there.
type projectsScreen struct {
	svc    *services
	root   transcripts.Root
	list   list.Model
	busy   bool
	err    error
	width  int
	height int
}

func newProjectsScreen(svc *services, root transcripts.Root) *projectsScreen {
	s := &projectsScreen{svc: svc, root: root, list: newList(nil, "project", "projects"), busy: true}
	s.list.Title = projectsHeader(0)
	return s
}

func (s *projectsScreen) Init() tea.Cmd {
	return loadProjects(s.svc.ctx, s.root, s.svc.now)
}

func (s *projectsScreen) Title() string {
	return shortRootLabel(s.root)
}

func (s *projectsScreen) loading() bool        { return s.busy }
func (s *projectsScreen) capturingInput() bool { return s.list.SettingFilter() }

func (s *projectsScreen) Keys() []key.Binding {
	return append([]key.Binding{keys.Up, keys.Down, keys.Open, keys.Filter}, filterKeys(s.list)...)
}

func (s *projectsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.list.SetSize(msg.Width, msg.Height-1)
		s.list.Title = projectsHeader(msg.Width - 2)
		return s, nil

	case projectsLoadedMsg:
		if msg.rootDir != s.root.Dir {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			s.err = msg.err
			return s, statusError(msg.err)
		}
		items := make([]list.Item, 0, len(msg.rows))
		for _, r := range msg.rows {
			items = append(items, r)
		}
		cmd := s.list.SetItems(items)
		if i := s.currentProjectIndex(msg.rows); i >= 0 {
			s.list.Select(i)
		}
		return s, cmd

	case tea.KeyPressMsg:
		if !s.list.SettingFilter() && key.Matches(msg, keys.Open) {
			r, ok := s.list.SelectedItem().(*projectRow)
			if !ok {
				return s, nil
			}
			if s.svc.start == "memories" {
				return s, pushScreen(newMemoriesScreen(s.svc, s.root, r.slug, r.cwd))
			}
			return s, pushScreen(newSessionsScreen(s.svc, s.root, r.slug, r.cwd))
		}
	}
	var cmd tea.Cmd
	s.list, cmd = s.list.Update(msg)
	return s, cmd
}

// currentProjectIndex finds the row of the process's working directory:
// by decoded cwd, else by the slug Claude would compute for it.
func (s *projectsScreen) currentProjectIndex(rows []*projectRow) int {
	if s.svc.cwd == "" {
		return -1
	}
	slug, _ := transcripts.Slug(s.svc.cwd)
	for i, r := range rows {
		if r.cwd == s.svc.cwd || (slug != "" && r.slug == slug) {
			return i
		}
	}
	return -1
}

func (s *projectsScreen) View(width, height int) string {
	head := fmt.Sprintf("%s  (%s)", shortPath(s.root.Dir), shortRootLabel(s.root))
	if s.busy {
		head += "  loading…"
	}
	return styleFaint.Render(truncate(head, width)) + "\n" + s.list.View()
}
