package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// sessionRow is one transcript of the project. It starts as what the
// fast path knows (id, mtime, size, subagents, liveness, import record,
// root/lastSessionId attribution) and is completed when its windows are
// read; sel is the screen's shared selection set.
type sessionRow struct {
	s        transcripts.Session
	resolved bool
	sel      map[string]bool
	now      func() time.Time
}

func (r *sessionRow) FilterValue() string {
	return r.s.Title + " " + r.s.ID + " " + r.s.Account + " " + r.s.GitBranch
}

const (
	sessionAccountW = 10
	sessionAgeW     = 8
	sessionSizeW    = 8
	sessionBranchW  = 12
	sessionStateW   = 16
	sessionFixedW   = 4 + 1 + sessionAccountW + 1 + sessionAgeW + 1 + sessionSizeW + 1 + sessionBranchW + 1 + sessionStateW
)

// shortID is the eight-character prefix the tables print.
func shortID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// sessionState is the STATE cell: live, imported·pending (an import
// whose directory does not exist here), imported, or nothing.
func sessionState(s transcripts.Session) string {
	switch {
	case s.Live:
		return "live"
	case s.Import != nil && !s.CwdExists:
		return "imported·pending"
	case s.Import != nil:
		return "imported"
	default:
		return ""
	}
}

// title is the TITLE cell: the sanitised title, "-" when the windows
// held none, the id while they are still being read.
func (r *sessionRow) title() string {
	if !r.resolved {
		return "(" + shortID(r.s.ID) + ")"
	}
	return dashIfEmpty(transcripts.Sanitize(r.s.Title))
}

func (r *sessionRow) render(width int) string {
	mark := "[ ] "
	if r.sel[r.s.ID] {
		mark = "[x] "
	}
	age := humanizeAgo(r.s.LastTS, r.now())
	titleW := width - sessionFixedW
	if titleW < 16 {
		// Narrow terminal: title, age and state only.
		titleW = width - 4 - 1 - sessionAgeW - 1 - sessionStateW
		if titleW < 8 {
			return truncate(mark+r.title(), width)
		}
		return mark + pad(r.title(), titleW) + " " + pad(age, sessionAgeW) + " " + pad(sessionState(r.s), sessionStateW)
	}
	return mark + pad(r.title(), titleW) +
		" " + pad(dashIfEmpty(transcripts.Sanitize(r.s.Account)), sessionAccountW) +
		" " + pad(age, sessionAgeW) +
		" " + pad(formatSize(r.s.Size), sessionSizeW) +
		" " + pad(dashIfEmpty(transcripts.Sanitize(r.s.GitBranch)), sessionBranchW) +
		" " + pad(sessionState(r.s), sessionStateW)
}

// sessionsHeader is the column header laid out like the rows.
func sessionsHeader(width int) string {
	titleW := width - sessionFixedW
	if titleW < 16 {
		titleW = width - 4 - 1 - sessionAgeW - 1 - sessionStateW
		if titleW < 8 {
			return "TITLE"
		}
		return "    " + pad("TITLE", titleW) + " " + pad("LAST", sessionAgeW) + " " + pad("STATE", sessionStateW)
	}
	return "    " + pad("TITLE", titleW) + " " + pad("ACCOUNT", sessionAccountW) + " " + pad("LAST", sessionAgeW) +
		" " + pad("SIZE", sessionSizeW) + " " + pad("BRANCH", sessionBranchW) + " " + pad("STATE", sessionStateW)
}

// loadSessions is the fast path for one project: liveness once, then
// ReadDir + Info per transcript. No transcript is opened; titles come
// later, page by page.
func loadSessions(ctx context.Context, svc *services, root transcripts.Root, slug string) tea.Cmd {
	configDirs := svc.configDirs()
	var attr transcripts.Attributor
	if svc.attributor != nil {
		attr = svc.attributor
	}
	imp := svc.imports
	now := svc.now
	return func() tea.Msg {
		msg := sessionsPageMsg{rootDir: root.Dir, slug: slug}
		live, err := transcripts.Live(ctx, configDirs)
		if err != nil {
			live = nil // liveness is enrichment
		}
		ss, err := transcripts.List(ctx, root, transcripts.ListOptions{
			Slug:       slug,
			Live:       live,
			Imports:    imp,
			Attributor: attr,
			Now:        now(),
		})
		if err != nil {
			msg.err = err
			return msg
		}
		msg.items = ss
		return msg
	}
}

// sessionsScreen is the sessions tab of one project.
type sessionsScreen struct {
	svc     *services
	root    transcripts.Root
	slug    string
	project string // decoded cwd of the project, "" when unknown
	list    list.Model
	rows    []*sessionRow
	byID    map[string]*sessionRow
	sel     map[string]bool
	pending map[string]bool // title reads in flight, by id
	busy    bool
	err     error
	width   int
	height  int
}

func newSessionsScreen(svc *services, root transcripts.Root, slug, project string) *sessionsScreen {
	s := &sessionsScreen{
		svc: svc, root: root, slug: slug, project: project,
		list:    newList(nil, "session", "sessions"),
		byID:    map[string]*sessionRow{},
		sel:     map[string]bool{},
		pending: map[string]bool{},
		busy:    true,
	}
	s.list.Title = sessionsHeader(0)
	return s
}

func (s *sessionsScreen) Init() tea.Cmd {
	return loadSessions(s.svc.ctx, s.svc, s.root, s.slug)
}

func (s *sessionsScreen) Title() string {
	return s.projectLabel() + " › sessions"
}

func (s *sessionsScreen) projectLabel() string {
	if s.project != "" {
		return transcripts.Sanitize(shortPath(s.project))
	}
	return transcripts.Sanitize(s.slug)
}

func (s *sessionsScreen) loading() bool        { return s.busy }
func (s *sessionsScreen) capturingInput() bool { return s.list.SettingFilter() }

func (s *sessionsScreen) Keys() []key.Binding {
	ks := []key.Binding{keys.Up, keys.Down, keys.Open, keys.Select, keys.SelectAll, keys.Tab, keys.ScanPaths, keys.Filter}
	ks = append(ks, actionKeys(true)...)
	return append(ks, filterKeys(s.list)...)
}

// target is what an action pushed from here works on: the root, the
// project, every session in row order and the selected ones.
func (s *sessionsScreen) target() actionTarget {
	t := actionTarget{root: s.root, slug: s.slug, project: s.project}
	for _, r := range s.rows {
		t.rows = append(t.rows, r.s)
		if s.sel[r.s.ID] {
			t.ids = append(t.ids, r.s.ID)
		}
	}
	return t
}

// chosen are the sessions a rehome works on: the selection, else the
// project's pending imports (an import record and no directory here).
func (s *sessionsScreen) chosen() []transcripts.Session {
	var sel, pending []transcripts.Session
	for _, r := range s.rows {
		switch {
		case s.sel[r.s.ID]:
			sel = append(sel, r.s)
		case r.s.Import != nil && !r.s.CwdExists:
			pending = append(pending, r.s)
		}
	}
	if len(sel) > 0 {
		return sel
	}
	return pending
}

// clearSelection empties the selection set the rows share.
func (s *sessionsScreen) clearSelection() {
	for id := range s.sel {
		delete(s.sel, id)
	}
}

// action opens the screen behind an action key, or explains in the
// status line why it cannot.
func (s *sessionsScreen) action(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keys.Export):
		return pushScreen(newExportScreen(s.svc, s.target())), true
	case key.Matches(msg, keys.Send):
		sc, err := newServeScreen(s.svc, s.target())
		if err != nil {
			return statusError(err), true
		}
		return pushScreen(sc), true
	case key.Matches(msg, keys.Receive):
		return receiveInto(s.svc, s.root), true
	case key.Matches(msg, keys.Copy):
		return pushScreen(newCopyScreen(s.svc, s.target())), true
	case key.Matches(msg, keys.Rehome):
		chosen := s.chosen()
		if len(chosen) == 0 {
			return status("nothing to rehome: select sessions with space (the project has no pending imports)"), true
		}
		return pushScreen(newRehomeScreen(s.svc, s.target(), chosen)), true
	case key.Matches(msg, keys.Resume):
		r, ok := s.list.SelectedItem().(*sessionRow)
		if !ok {
			return nil, true
		}
		return resume(s.svc, r.s), true
	case key.Matches(msg, keys.Trust):
		if s.project == "" {
			return status("no cwd recorded for this project; nothing to trust"), true
		}
		return pushScreen(newTrustScreen(s.svc, s.project)), true
	}
	return nil, false
}

// memoryDir is where this project's auto-memory lives when it has any:
// keyed by the git root, so the cwd's own slug is tried through
// MemoryDirFor first, then the slug directory itself.
func (s *sessionsScreen) memoryDir() string {
	return memoryDirFor(s.root, s.slug, s.project)
}

func memoryDirFor(root transcripts.Root, slug, project string) string {
	if project != "" {
		if dir, err := transcripts.MemoryDirFor(root, project); err == nil && isDir(dir) {
			return dir
		}
	}
	dir := filepath.Join(root.Dir, slug, transcripts.MemorySubdir)
	if isDir(dir) {
		return dir
	}
	return ""
}

func (s *sessionsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.list.SetSize(msg.Width, msg.Height-1)
		s.list.Title = sessionsHeader(msg.Width - 2)
		return s, s.requestTitles()

	case sessionsPageMsg:
		if msg.rootDir != s.root.Dir || msg.slug != s.slug {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			s.err = msg.err
			return s, statusError(msg.err)
		}
		s.rows = s.rows[:0]
		s.byID = map[string]*sessionRow{}
		items := make([]list.Item, 0, len(msg.items))
		for _, sess := range msg.items {
			r := &sessionRow{s: sess, sel: s.sel, now: s.svc.now}
			if m, ok := s.svc.titles[keyOf(sess)]; ok {
				s.applyMeta(r, m)
			}
			s.rows = append(s.rows, r)
			s.byID[sess.ID] = r
			items = append(items, r)
		}
		cmd := s.list.SetItems(items)
		return s, tea.Batch(cmd, s.requestTitles())

	case refreshMsg:
		// An action changed the pool: list again, selection cleared.
		s.clearSelection()
		s.busy = true
		return s, loadSessions(s.svc.ctx, s.svc, s.root, s.slug)

	case resumeDoneMsg:
		// claude ran in this terminal and exited; the transcript changed.
		s.busy = true
		var note tea.Cmd
		if msg.err != nil {
			note = statusError(fmt.Errorf("claude --resume %s: %w", shortID(msg.id), msg.err))
		} else {
			note = status("claude exited; listing again")
		}
		return s, tea.Batch(note, loadSessions(s.svc.ctx, s.svc, s.root, s.slug))

	case titlesResolvedMsg:
		if msg.slug != s.slug {
			return s, nil
		}
		if msg.history != nil {
			s.svc.history[msg.configDir] = msg.history
		}
		for id, m := range msg.metas {
			delete(s.pending, id)
			r := s.byID[id]
			if r == nil {
				continue
			}
			s.applyMeta(r, m)
			if !m.Failed {
				s.svc.titles[keyOf(r.s)] = m
			}
		}
		var cmd tea.Cmd
		if s.list.FilterState() != list.Unfiltered {
			cmd = s.list.SetItems(s.list.Items()) // re-run the filter over the new titles
		}
		return s, tea.Batch(cmd, s.requestTitles())

	case tea.KeyPressMsg:
		if s.list.SettingFilter() {
			var cmd tea.Cmd
			s.list, cmd = s.list.Update(msg)
			return s, tea.Batch(cmd, s.requestTitles())
		}
		switch {
		case key.Matches(msg, keys.Open):
			if r, ok := s.list.SelectedItem().(*sessionRow); ok {
				return s, pushScreen(newShowScreen(s.svc, r.s, r.resolved))
			}
			return s, nil
		case key.Matches(msg, keys.Select):
			if r, ok := s.list.SelectedItem().(*sessionRow); ok {
				if s.sel[r.s.ID] {
					delete(s.sel, r.s.ID)
				} else {
					s.sel[r.s.ID] = true
				}
				s.list.CursorDown()
			}
			return s, nil
		case key.Matches(msg, keys.SelectAll):
			visible := s.list.VisibleItems()
			all := len(visible) > 0
			for _, it := range visible {
				if !s.sel[it.(*sessionRow).s.ID] {
					all = false
					break
				}
			}
			for _, it := range visible {
				id := it.(*sessionRow).s.ID
				if all {
					delete(s.sel, id)
				} else {
					s.sel[id] = true
				}
			}
			return s, nil
		case key.Matches(msg, keys.Tab):
			return s, replaceScreen(newMemoriesScreen(s.svc, s.root, s.slug, s.project))
		case key.Matches(msg, keys.ScanPaths):
			dir := s.memoryDir()
			if dir == "" {
				return s, status("no memory dir for " + s.projectLabel())
			}
			return s, pushScreen(newScanPathsScreen(s.svc, dir))
		case key.Matches(msg, keys.Filter):
			var cmd tea.Cmd
			s.list, cmd = s.list.Update(msg)
			// A filter searches titles, so every title is needed now.
			return s, tea.Batch(cmd, s.requestAllTitles())
		}
		if cmd, ok := s.action(msg); ok {
			return s, cmd
		}
	}
	var cmd tea.Cmd
	s.list, cmd = s.list.Update(msg)
	return s, tea.Batch(cmd, s.requestTitles())
}

// receiveInto opens the Receive screen for root, unless the root is an
// orphan session dir (read-only: no account would ever read the import).
func receiveInto(svc *services, root transcripts.Root) tea.Cmd {
	if root.Orphan {
		return status("orphan session dir " + transcripts.Sanitize(root.Owner) + " is read-only; receive into an account's root instead")
	}
	return pushScreen(newReceiveScreen(svc, root))
}

// applyMeta completes a row from its windows and refines attribution
// with the cwd and first timestamp the launch-log tier needs.
func (s *sessionsScreen) applyMeta(r *sessionRow, m titleMeta) {
	r.resolved = true
	if m.Failed {
		return
	}
	m.apply(&r.s)
	s.svc.attribute(&r.s)
}

// requestTitles resolves the titles of the visible page ±1: cached
// ones immediately, the rest in one command.
func (s *sessionsScreen) requestTitles() tea.Cmd {
	items, start, end := visibleRange(s.list)
	return s.request(items[start:end])
}

// requestAllTitles resolves every title, in parallel batches.
func (s *sessionsScreen) requestAllTitles() tea.Cmd {
	return s.request(s.list.Items())
}

func (s *sessionsScreen) request(items []list.Item) tea.Cmd {
	var reqs []titleReq
	for _, it := range items {
		r, ok := it.(*sessionRow)
		if !ok || r.resolved || s.pending[r.s.ID] {
			continue
		}
		if m, ok := s.svc.titles[keyOf(r.s)]; ok {
			s.applyMeta(r, m)
			continue
		}
		s.pending[r.s.ID] = true
		reqs = append(reqs, titleReq{id: r.s.ID, path: r.s.Path})
	}
	if len(reqs) == 0 {
		return nil
	}
	hist := s.svc.history[s.root.ConfigDir]
	var cmds []tea.Cmd
	for len(reqs) > 0 {
		n := min(titleBatch, len(reqs))
		cmds = append(cmds, resolveTitles(s.svc.ctx, s.slug, s.root.ConfigDir, reqs[:n], hist))
		reqs = reqs[n:]
	}
	return tea.Batch(cmds...)
}

func (s *sessionsScreen) View(width, height int) string {
	head := fmt.Sprintf("project %s  (%s)", s.projectLabel(), shortRootLabel(s.root))
	if n := len(s.sel); n > 0 {
		head += fmt.Sprintf("  %d selected", n)
	}
	if s.busy {
		head += "  loading…"
	}
	return styleFaint.Render(truncate(head, width)) + "\n" + s.list.View()
}
