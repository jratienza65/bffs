package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// screenMode is lazygit's +/_ cycle: the side column at a third, at half,
// or hidden behind the main pane.
type screenMode int

const (
	modeNormal screenMode = iota
	modeHalf
	modeMainOnly
	modeCount
)

// itemsTab is what panel 3 lists for the project.
type itemsTab int

const (
	tabSessions itemsTab = iota
	tabMemory
)

// workspace is the browse layer (tui-v2 §2): four stacked panels in a
// side column — roots, projects, sessions|memory, files — and a main
// pane previewing the focused panel's selection. Panels are lists; the
// chain root → project → item → file is re-derived after every cursor
// move by sync, which loads only what changed. Overlays (action screens,
// the menu, the keys) are the app's; the workspace only draws the frame
// they sit in.
type workspace struct {
	svc       *services
	panels    [panelCount]*panel
	focus     panelID
	mainFocus bool // the main pane scrolls with the keys
	mode      screenMode
	tab       itemsTab
	width     int
	height    int

	roots      []transcripts.Root
	rootKey    string
	root       transcripts.Root
	project    *projectRow
	projectKey string // root + slug the items belong to
	placed     bool   // the cursor was put on the process's project once

	// sessions tab: the rows, their index, the selection and the title
	// reads in flight — as the old sessions screen kept them.
	sessRows      []*sessionRow
	byID          map[string]*sessionRow
	sel           map[string]bool
	pending       map[string]bool
	sessLoadedFor string
	itemsKey      string

	// memory tab
	mem          *transcripts.Memory
	memDir       string
	memLoadedFor string

	filesKey string

	vp          viewport.Model
	previewKey  string
	previewGen  int
	previewBusy bool
	gen         int
}

func newWorkspace(svc *services) *workspace {
	ws := &workspace{svc: svc, focus: panelProjects, byID: map[string]*sessionRow{}, sel: map[string]bool{}, pending: map[string]bool{}, vp: newViewport()}
	ws.panels[panelRoots] = newPanel(panelRoots, "roots", "root", "roots", "no roots")
	ws.panels[panelProjects] = newPanel(panelProjects, "projects", "project", "projects", "no projects")
	ws.panels[panelItems] = newPanel(panelItems, "sessions", "session", "sessions", "no sessions")
	ws.panels[panelFiles] = newPanel(panelFiles, "files", "file", "files", "nothing selected")
	if svc.start == "memories" {
		ws.tab = tabMemory
		ws.focus = panelItems
	}
	ws.applyTab()
	return ws
}

// rootID identifies a root for the selection chain.
func rootID(r transcripts.Root) string { return r.ConfigDir + "\x00" + r.Dir }

// applyTab renames panels 3 and 4 for the active tab.
func (ws *workspace) applyTab() {
	items, files := ws.panels[panelItems], ws.panels[panelFiles]
	switch ws.tab {
	case tabSessions:
		items.name, items.empty = "sessions", "no sessions"
		files.name, files.empty = "files", "select a session"
	case tabMemory:
		items.name, items.empty = "memory", "no memory dir"
		files.name, files.empty = "references", "select a memory file"
	}
	ws.layout()
}

// --- layout -----------------------------------------------------------

func (ws *workspace) setSize(width, height int) {
	ws.width, ws.height = width, height
	ws.layout()
}

// sideWidth is the side column's inner width, 0 when hidden.
func (ws *workspace) sideWidth() int {
	switch ws.mode {
	case modeMainOnly:
		return 0
	case modeHalf:
		return max(0, (ws.width-3)/2)
	}
	w := ws.width / 3
	if ws.width < 90 {
		w = (ws.width - 3) / 2
	}
	return max(0, min(max(w, 30), 64))
}

// mainWidth is the main pane's inner width.
func (ws *workspace) mainWidth() int {
	side := ws.sideWidth()
	if side == 0 {
		return max(0, ws.width-2)
	}
	return max(0, ws.width-3-side)
}

// bodyHeight is what the frame leaves for panel rows.
func (ws *workspace) bodyHeight() int { return max(0, ws.height-2) }

// panelHeights shares the body rows: the focused panel gets what the
// others (one to three rows each) leave.
func (ws *workspace) panelHeights() [panelCount]int {
	var hs [panelCount]int
	h := ws.bodyHeight() - (int(panelCount) - 1) // separators
	u := min(3, max(1, h/8))
	for i := range hs {
		hs[i] = u
	}
	hs[ws.focus] = max(1, h-u*(int(panelCount)-1))
	return hs
}

func (ws *workspace) layout() {
	side := ws.sideWidth()
	hs := ws.panelHeights()
	for i, p := range ws.panels {
		if p == nil {
			continue
		}
		p.setSize(side, hs[i])
	}
	ws.applyHeaders(side)
	ws.vp.SetWidth(ws.mainWidth())
	ws.vp.SetHeight(ws.bodyHeight())
}

// applyHeaders sets the column headers the lists show in their title row.
func (ws *workspace) applyHeaders(width int) {
	inner := max(0, width-2)
	ws.panels[panelRoots].list.Title = "ROOT"
	ws.panels[panelProjects].list.Title = projectsHeader(inner)
	if ws.tab == tabSessions {
		ws.panels[panelItems].list.Title = sessionsHeader(inner)
	} else {
		ws.panels[panelItems].list.Title = memoriesHeader(inner)
	}
	ws.panels[panelFiles].list.Title = "FILE"
}

// --- selection chain --------------------------------------------------

func (ws *workspace) selectedRoot() (transcripts.Root, bool) {
	r, ok := ws.panels[panelRoots].selected().(*rootRow)
	if !ok {
		return transcripts.Root{}, false
	}
	return r.root, true
}

func (ws *workspace) selectedProject() *projectRow {
	r, _ := ws.panels[panelProjects].selected().(*projectRow)
	return r
}

func (ws *workspace) selectedSession() *sessionRow {
	if ws.tab != tabSessions {
		return nil
	}
	r, _ := ws.panels[panelItems].selected().(*sessionRow)
	return r
}

func (ws *workspace) selectedMemFile() *memoryFileRow {
	if ws.tab != tabMemory {
		return nil
	}
	r, _ := ws.panels[panelItems].selected().(*memoryFileRow)
	return r
}

// slug and cwd of the selected project.
func (ws *workspace) projectSlug() string {
	if ws.project == nil {
		return ""
	}
	return ws.project.slug
}

func (ws *workspace) projectCwd() string {
	if ws.project == nil {
		return ""
	}
	return ws.project.cwd
}

func (ws *workspace) projectLabel() string {
	if ws.project == nil {
		return ""
	}
	return ws.project.label()
}

// memoryDir is the project's memory directory when it has one.
func (ws *workspace) memoryDir() string {
	if ws.project == nil {
		return ""
	}
	return memoryDirFor(ws.root, ws.project.slug, ws.project.cwd)
}

// setRoots fills panel 1 and starts the chain.
func (ws *workspace) setRoots(roots []transcripts.Root) tea.Cmd {
	ws.roots = roots
	rows := make([]row, 0, len(roots))
	for _, r := range roots {
		rows = append(rows, &rootRow{root: r, label: rootLabel(r)})
	}
	cmd := ws.panels[panelRoots].setRows(rows)
	return tea.Batch(cmd, ws.sync())
}

// sync re-derives the chain from the cursors and loads what changed.
func (ws *workspace) sync() tea.Cmd {
	var cmds []tea.Cmd
	if r, ok := ws.selectedRoot(); ok && rootID(r) != ws.rootKey {
		ws.rootKey, ws.root = rootID(r), r
		ws.project, ws.projectKey = nil, ""
		ws.clearSessions()
		ws.clearMemory()
		ws.placed = false
		p := ws.panels[panelProjects]
		p.loading = true
		cmds = append(cmds, p.setRows(nil), loadProjects(ws.svc.ctx, r, ws.svc.now))
	}
	if pr := ws.selectedProject(); pr == nil {
		if ws.projectKey != "" {
			ws.project, ws.projectKey = nil, ""
			ws.clearSessions()
			ws.clearMemory()
		}
	} else {
		ws.project = pr
		pk := ws.rootKey + "\x00" + pr.slug
		if pk != ws.projectKey {
			ws.projectKey = pk
			ws.clearSessions()
			ws.clearMemory()
		}
		ik := fmt.Sprintf("%d\x00%s", ws.tab, pk)
		if ik != ws.itemsKey {
			ws.itemsKey = ik
			cmds = append(cmds, ws.loadItems())
		}
	}
	if ws.project == nil && ws.itemsKey != "" {
		ws.itemsKey = ""
		cmds = append(cmds, ws.panels[panelItems].setRows(nil))
	}
	fk, fcmd := ws.filesFor()
	if fk != ws.filesKey {
		ws.filesKey = fk
		f := ws.panels[panelFiles]
		cmds = append(cmds, f.setRows(nil))
		f.loading = fcmd != nil
		if fcmd != nil {
			cmds = append(cmds, fcmd)
		}
	}
	pk, pcmd := ws.previewFor()
	if pk != ws.previewKey {
		ws.previewKey, ws.previewGen = pk, ws.gen
		ws.previewBusy = pcmd != nil
		if pcmd == nil {
			ws.vp.SetContentLines([]string{styleFaint.Render("nothing selected")})
		}
		cmds = append(cmds, pcmd)
	}
	cmds = append(cmds, ws.requestTitles())
	return tea.Batch(cmds...)
}

func (ws *workspace) clearSessions() {
	ws.sessRows = nil
	ws.byID = map[string]*sessionRow{}
	ws.pending = map[string]bool{}
	for id := range ws.sel {
		delete(ws.sel, id)
	}
	ws.sessLoadedFor = ""
	if ws.tab == tabSessions {
		ws.itemsKey = ""
	}
}

func (ws *workspace) clearMemory() {
	ws.mem, ws.memDir, ws.memLoadedFor = nil, "", ""
	if ws.tab == tabMemory {
		ws.itemsKey = ""
	}
}

// loadItems fills panel 3 for the project and tab, from what is already
// loaded when it is.
func (ws *workspace) loadItems() tea.Cmd {
	p := ws.panels[panelItems]
	switch ws.tab {
	case tabSessions:
		if ws.sessLoadedFor == ws.projectKey {
			return tea.Batch(p.setRows(ws.sessionRowsAsRows()), ws.requestTitles())
		}
		p.loading = true
		return tea.Batch(p.setRows(nil), loadSessions(ws.svc.ctx, ws.svc, ws.root, ws.projectSlug()))
	case tabMemory:
		if ws.memLoadedFor == ws.projectKey {
			return p.setRows(ws.memoryRows())
		}
		p.loading = true
		if mems, ok := ws.svc.memories[ws.root.Dir]; ok {
			root := ws.root
			return tea.Batch(p.setRows(nil), func() tea.Msg { return memoriesLoadedMsg{rootDir: root.Dir, mems: mems} })
		}
		return tea.Batch(p.setRows(nil), loadMemories(ws.svc.ctx, ws.root))
	}
	return nil
}

func (ws *workspace) sessionRowsAsRows() []row {
	rows := make([]row, 0, len(ws.sessRows))
	for _, r := range ws.sessRows {
		rows = append(rows, r)
	}
	return rows
}

func (ws *workspace) memoryRows() []row {
	if ws.mem == nil {
		return nil
	}
	rows := make([]row, 0, len(ws.mem.Files))
	for _, f := range ws.mem.Files {
		rows = append(rows, &memoryFileRow{f: f, dir: ws.mem.Dir, svc: ws.svc})
	}
	return rows
}

// pickMemory chooses the project's memory directory among the root's:
// the one Claude would use for the decoded cwd (keyed by the git root),
// else the slug directory's own.
func pickMemory(root transcripts.Root, slug, project string, mems []transcripts.Memory) (*transcripts.Memory, string) {
	want := memoryDirFor(root, slug, project)
	if want == "" {
		want = filepath.Join(root.Dir, slug, transcripts.MemorySubdir)
		if project != "" {
			if dir, err := transcripts.MemoryDirFor(root, project); err == nil {
				want = dir
			}
		}
	}
	for i := range mems {
		if filepath.Clean(mems[i].Dir) == filepath.Clean(want) {
			return &mems[i], want
		}
	}
	return nil, want
}

// filesFor names panel 4's contents and how to load them.
func (ws *workspace) filesFor() (string, tea.Cmd) {
	if r := ws.selectedSession(); r != nil {
		return artifactKey(r.s), loadArtifacts(ws.root, r.s)
	}
	if r := ws.selectedMemFile(); r != nil {
		return refsKey(r.dir, r.f.Name), loadRefs(r.dir, r.f.Name)
	}
	return "", nil
}

// previewFor names the main pane's subject and how to build it.
func (ws *workspace) previewFor() (string, tea.Cmd) {
	focus := ws.focus
	if focus == panelFiles {
		switch r := ws.panels[panelFiles].selected().(type) {
		case *artifactRow:
			k := "artifact:" + r.path
			return k, previewCmd(k, ws.gen, artifactPreview(r))
		case *refRow:
			k := fmt.Sprintf("ref:%s:%d:%s", r.ref.File, r.ref.Line, r.ref.Path)
			return k, previewCmd(k, ws.gen, refPreview(r))
		}
		focus = panelItems
	}
	switch focus {
	case panelRoots:
		if r, ok := ws.selectedRoot(); ok {
			k := "root:" + rootID(r)
			return k, previewCmd(k, ws.gen, rootPreview(ws.svc, r, len(ws.panels[panelProjects].rows())))
		}
		return "", nil
	case panelItems:
		if r := ws.selectedSession(); r != nil {
			k := "session:" + r.s.Path
			return k, previewCmd(k, ws.gen, sessionPreview(ws.svc, r.s, r.resolved))
		}
		if r := ws.selectedMemFile(); r != nil {
			k := "memfile:" + filepath.Join(r.dir, filepath.FromSlash(r.f.Name))
			return k, previewCmd(k, ws.gen, memoryFilePreview(ws.svc, ws.root, ws.projectSlug(), ws.projectCwd(), r.dir, r.f.Name))
		}
		if ws.tab == tabMemory && ws.project != nil && ws.memLoadedFor == ws.projectKey && ws.mem == nil {
			k := "nomem:" + ws.projectKey
			dir := ws.memDir
			label := ws.projectLabel()
			return k, previewCmd(k, ws.gen, func() ([]string, error) {
				return []string{styleHeader.Render("no memory dir for " + label), kvLine("would be", shortPath(dir)), "", styleFaint.Render("Claude creates it the first time it saves a memory for the project")}, nil
			})
		}
	}
	if ws.project != nil {
		k := "drift:" + ws.projectKey
		return k, previewCmd(k, ws.gen, driftPreview(ws.svc, ws.root, ws.project.slug, ws.project.cwd))
	}
	if r, ok := ws.selectedRoot(); ok {
		k := "root:" + rootID(r)
		return k, previewCmd(k, ws.gen, rootPreview(ws.svc, r, len(ws.panels[panelProjects].rows())))
	}
	return "", nil
}

// refresh reloads everything below the root after an action changed the
// tree; cursors stay where they are when the rows still exist.
func (ws *workspace) refresh() tea.Cmd {
	ws.gen++
	for k := range ws.svc.memories {
		delete(ws.svc.memories, k)
	}
	ws.sessLoadedFor, ws.memLoadedFor = "", ""
	ws.itemsKey, ws.filesKey, ws.previewKey = "", "", ""
	ws.pending = map[string]bool{}
	if ws.rootKey == "" {
		return ws.sync()
	}
	ws.panels[panelProjects].loading = true
	return loadProjects(ws.svc.ctx, ws.root, ws.svc.now)
}

// --- messages ---------------------------------------------------------

func (ws *workspace) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		if msg.rootDir != ws.root.Dir {
			return nil
		}
		p := ws.panels[panelProjects]
		p.loading = false
		if msg.err != nil {
			return statusError(msg.err)
		}
		rows := make([]row, 0, len(msg.rows))
		for _, r := range msg.rows {
			rows = append(rows, r)
		}
		cmd := p.setRows(rows)
		if !ws.placed {
			ws.placed = true
			if i := ws.currentProjectIndex(msg.rows); i >= 0 {
				p.list.Select(i)
			}
		}
		ws.projectKey = "" // rows are new objects: re-derive the chain
		if strings.HasPrefix(ws.previewKey, "root:") {
			ws.previewKey = "" // the root preview counts the projects
		}
		return tea.Batch(cmd, ws.sync())

	case sessionsPageMsg:
		if msg.rootDir != ws.root.Dir || msg.slug != ws.projectSlug() {
			return nil
		}
		p := ws.panels[panelItems]
		p.loading = false
		if msg.err != nil {
			return statusError(msg.err)
		}
		ws.sessRows = ws.sessRows[:0]
		ws.byID = map[string]*sessionRow{}
		for _, sess := range msg.items {
			r := &sessionRow{s: sess, sel: ws.sel, now: ws.svc.now}
			if m, ok := ws.svc.titles[keyOf(sess)]; ok {
				ws.applyMeta(r, m)
			}
			ws.sessRows = append(ws.sessRows, r)
			ws.byID[sess.ID] = r
		}
		ws.sessLoadedFor = ws.projectKey
		var cmd tea.Cmd
		if ws.tab == tabSessions {
			cmd = p.setRows(ws.sessionRowsAsRows())
		}
		return tea.Batch(cmd, ws.requestTitles(), ws.sync())

	case titlesResolvedMsg:
		if msg.slug != ws.projectSlug() {
			return nil
		}
		if msg.history != nil {
			ws.svc.history[msg.configDir] = msg.history
		}
		for id, m := range msg.metas {
			delete(ws.pending, id)
			r := ws.byID[id]
			if r == nil {
				continue
			}
			ws.applyMeta(r, m)
			if !m.Failed {
				ws.svc.titles[keyOf(r.s)] = m
			}
		}
		var cmd tea.Cmd
		if l := &ws.panels[panelItems].list; ws.tab == tabSessions && l.FilterState() != list.Unfiltered {
			cmd = l.SetItems(l.Items())
		}
		return tea.Batch(cmd, ws.requestTitles())

	case memoriesLoadedMsg:
		if msg.rootDir != ws.root.Dir {
			return nil
		}
		p := ws.panels[panelItems]
		p.loading = false
		if msg.err != nil {
			return statusError(msg.err)
		}
		ws.svc.memories[ws.root.Dir] = msg.mems
		ws.mem, ws.memDir = pickMemory(ws.root, ws.projectSlug(), ws.projectCwd(), msg.mems)
		ws.memLoadedFor = ws.projectKey
		var cmd tea.Cmd
		if ws.tab == tabMemory {
			cmd = p.setRows(ws.memoryRows())
		}
		return tea.Batch(cmd, ws.sync())

	case filesLoadedMsg:
		if msg.key != ws.filesKey {
			return nil
		}
		p := ws.panels[panelFiles]
		p.loading = false
		if msg.err != nil {
			return statusError(msg.err)
		}
		return tea.Batch(p.setRows(msg.rows), ws.sync())

	case previewLoadedMsg:
		if msg.key != ws.previewKey || msg.gen != ws.previewGen {
			return nil
		}
		ws.previewBusy = false
		lines := msg.lines
		if msg.err != nil {
			lines = append(lines, styleError.Render(transcripts.Sanitize(msg.err.Error())))
		}
		ws.vp.SetContentLines(lines)
		ws.vp.GotoTop()
		return nil

	case refreshMsg:
		return ws.refresh()

	case resumeDoneMsg:
		note := status("claude exited; listing again")
		if msg.err != nil {
			note = statusError(fmt.Errorf("claude --resume %s: %w", shortID(msg.id), msg.err))
		}
		return tea.Batch(note, ws.refresh())

	case tea.WindowSizeMsg, tea.KeyPressMsg:
		return nil
	}
	// Anything else is a list's own (the filter's match results): the
	// focused panel is the one that asked.
	cmd := ws.updatePanel(ws.panels[ws.focus], msg)
	return tea.Batch(cmd, ws.sync())
}

func (ws *workspace) currentProjectIndex(rows []*projectRow) int {
	if ws.svc.cwd == "" {
		return -1
	}
	slug, _ := transcripts.Slug(ws.svc.cwd)
	for i, r := range rows {
		if r.cwd == ws.svc.cwd || (slug != "" && r.slug == slug) {
			return i
		}
	}
	return -1
}

// applyMeta completes a row from its windows and refines attribution.
func (ws *workspace) applyMeta(r *sessionRow, m titleMeta) {
	r.resolved = true
	if m.Failed {
		return
	}
	m.apply(&r.s)
	ws.svc.attribute(&r.s)
}

// requestTitles resolves the titles of the visible page ±1 of the
// sessions panel: cached ones immediately, the rest in one command.
func (ws *workspace) requestTitles() tea.Cmd {
	if ws.tab != tabSessions {
		return nil
	}
	items, start, end := visibleRange(ws.panels[panelItems].list)
	return ws.request(items[start:end])
}

func (ws *workspace) requestAllTitles() tea.Cmd {
	if ws.tab != tabSessions {
		return nil
	}
	return ws.request(ws.panels[panelItems].list.Items())
}

func (ws *workspace) request(items []list.Item) tea.Cmd {
	var reqs []titleReq
	for _, it := range items {
		r, ok := it.(*sessionRow)
		if !ok || r.resolved || ws.pending[r.s.ID] {
			continue
		}
		if m, ok := ws.svc.titles[keyOf(r.s)]; ok {
			ws.applyMeta(r, m)
			continue
		}
		ws.pending[r.s.ID] = true
		reqs = append(reqs, titleReq{id: r.s.ID, path: r.s.Path})
	}
	if len(reqs) == 0 {
		return nil
	}
	hist := ws.svc.history[ws.root.ConfigDir]
	var cmds []tea.Cmd
	for len(reqs) > 0 {
		n := min(titleBatch, len(reqs))
		cmds = append(cmds, resolveTitles(ws.svc.ctx, ws.projectSlug(), ws.root.ConfigDir, reqs[:n], hist))
		reqs = reqs[n:]
	}
	return tea.Batch(cmds...)
}

// --- keys ---------------------------------------------------------------

// capturing reports whether a panel's filter input has the keyboard.
func (ws *workspace) capturing() bool { return ws.panels[ws.focus].filtering() }

func (ws *workspace) setFocus(id panelID) tea.Cmd {
	if id < 0 || id >= panelCount {
		return nil
	}
	ws.focus, ws.mainFocus = id, false
	ws.layout()
	return ws.sync()
}

func (ws *workspace) switchTab(delta int) tea.Cmd {
	if delta > 0 {
		ws.tab = tabMemory
	} else {
		ws.tab = tabSessions
	}
	ws.applyTab()
	ws.panels[panelItems].list.ResetFilter()
	cmds := []tea.Cmd{ws.sync()}
	if ws.focus != panelItems {
		cmds = append(cmds, ws.setFocus(panelItems))
	}
	return tea.Batch(cmds...)
}

// updatePanel forwards msg to the focused panel's list.
func (ws *workspace) updatePanel(p *panel, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return cmd
}

func (ws *workspace) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	p := ws.panels[ws.focus]
	if p.filtering() {
		cmd := ws.updatePanel(p, msg)
		return tea.Batch(cmd, ws.sync(), ws.requestTitles())
	}
	if ws.mainFocus {
		if key.Matches(msg, keys.Back) || key.Matches(msg, keys.PrevPanel) {
			ws.mainFocus = false
			return nil
		}
		var cmd tea.Cmd
		ws.vp, cmd = ws.vp.Update(msg)
		return cmd
	}
	switch {
	case key.Matches(msg, keys.Panel1):
		return ws.setFocus(panelRoots)
	case key.Matches(msg, keys.Panel2):
		return ws.setFocus(panelProjects)
	case key.Matches(msg, keys.Panel3):
		return ws.setFocus(panelItems)
	case key.Matches(msg, keys.Panel4):
		return ws.setFocus(panelFiles)
	case key.Matches(msg, keys.NextPanel):
		return ws.setFocus((ws.focus + 1) % panelCount)
	case key.Matches(msg, keys.PrevPanel):
		return ws.setFocus((ws.focus + panelCount - 1) % panelCount)
	case key.Matches(msg, keys.NextTab):
		return ws.switchTab(1)
	case key.Matches(msg, keys.PrevTab):
		return ws.switchTab(-1)
	case key.Matches(msg, keys.ScreenMode):
		ws.mode = (ws.mode + 1) % modeCount
		ws.layout()
		return nil
	case key.Matches(msg, keys.ScreenModePrev):
		ws.mode = (ws.mode + modeCount - 1) % modeCount
		ws.layout()
		return nil
	case key.Matches(msg, keys.Menu):
		return pushScreen(newMenuScreen(ws.menuTitle(), ws.actions()))
	case key.Matches(msg, keys.Open):
		return ws.open()
	case key.Matches(msg, keys.Select):
		if r := ws.selectedSession(); r != nil && ws.focus == panelItems {
			if ws.sel[r.s.ID] {
				delete(ws.sel, r.s.ID)
			} else {
				ws.sel[r.s.ID] = true
			}
			p.list.CursorDown()
			return ws.sync()
		}
		return nil
	case key.Matches(msg, keys.SelectAll):
		if ws.tab == tabSessions && ws.focus == panelItems {
			visible := p.list.VisibleItems()
			all := len(visible) > 0
			for _, it := range visible {
				if !ws.sel[it.(*sessionRow).s.ID] {
					all = false
					break
				}
			}
			for _, it := range visible {
				id := it.(*sessionRow).s.ID
				if all {
					delete(ws.sel, id)
				} else {
					ws.sel[id] = true
				}
			}
		}
		return nil
	case key.Matches(msg, keys.Filter):
		cmd := ws.updatePanel(p, msg)
		if ws.focus == panelItems {
			return tea.Batch(cmd, ws.requestAllTitles())
		}
		return cmd
	case key.Matches(msg, keys.Back):
		if p.list.FilterState() == list.FilterApplied {
			cmd := ws.updatePanel(p, msg)
			return tea.Batch(cmd, ws.sync())
		}
		return status("q quits · x opens the menu")
	}
	if cmd, ok := ws.action(msg); ok {
		return cmd
	}
	cmd := ws.updatePanel(p, msg)
	return tea.Batch(cmd, ws.sync(), ws.requestTitles())
}

// open is enter: drill down a panel, open a session's transcript, or
// hand the keys to the main pane to scroll a file.
func (ws *workspace) open() tea.Cmd {
	switch ws.focus {
	case panelRoots:
		return ws.setFocus(panelProjects)
	case panelProjects:
		if ws.project == nil {
			return nil
		}
		return ws.setFocus(panelItems)
	case panelItems:
		if r := ws.selectedSession(); r != nil {
			return pushScreen(newTranscriptScreen(ws.svc, r.s))
		}
		if ws.selectedMemFile() != nil {
			ws.mainFocus = true
		}
		return nil
	case panelFiles:
		switch r := ws.panels[panelFiles].selected().(type) {
		case *artifactRow:
			if r.exists && !r.isDir && filepath.Ext(r.path) == ".jsonl" {
				if s := ws.selectedSession(); s != nil && r.path == s.s.Path {
					return pushScreen(newTranscriptScreen(ws.svc, s.s))
				}
			}
			ws.mainFocus = true
		case *refRow:
			ws.mainFocus = true
		}
	}
	return nil
}

// --- actions ------------------------------------------------------------

// target is what an action works on: the root, the project, every
// session in row order and the selected ones.
func (ws *workspace) target() actionTarget {
	t := actionTarget{root: ws.root, slug: ws.projectSlug(), project: ws.projectCwd()}
	for _, r := range ws.sessRows {
		t.rows = append(t.rows, r.s)
		if ws.sel[r.s.ID] {
			t.ids = append(t.ids, r.s.ID)
		}
	}
	return t
}

// chosen are the sessions a rehome works on: the selection, else the
// project's pending imports (an import record and no directory here).
func (ws *workspace) chosen() []transcripts.Session {
	var sel, pending []transcripts.Session
	for _, r := range ws.sessRows {
		switch {
		case ws.sel[r.s.ID]:
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

func (ws *workspace) menuTitle() string {
	if ws.project == nil {
		return "actions for " + shortRootLabel(ws.root)
	}
	return "actions for " + ws.projectLabel() + "  (" + shortRootLabel(ws.root) + ")"
}

// actions lists what the current selection allows, in menu order; the
// same table answers the action keys. Every entry calls the CLI's
// engines through the action screens, never bffs itself.
func (ws *workspace) actions() []menuItem {
	svc, root := ws.svc, ws.root
	var items []menuItem
	add := func(k, label string, run func() tea.Cmd) {
		items = append(items, menuItem{key: k, label: label, run: run})
	}
	if ws.rootKey == "" {
		return items
	}
	add("i", "receive a bundle over the LAN into "+shortRootLabel(root), func() tea.Cmd { return receiveInto(svc, root) })
	if ws.project == nil {
		return items
	}
	tgt := ws.target()
	label := ws.projectLabel()
	add("e", "export "+tgt.what()+" to a file", func() tea.Cmd { return pushScreen(newExportScreen(svc, tgt)) })
	add("s", "send "+tgt.what()+" over the LAN", func() tea.Cmd {
		sc, err := newServeScreen(svc, tgt)
		if err != nil {
			return statusError(err)
		}
		return pushScreen(sc)
	})
	add("c", "copy "+tgt.what()+" to another account", func() tea.Cmd { return pushScreen(newCopyScreen(svc, tgt)) })
	if ws.tab == tabSessions && ws.focus == panelItems {
		if r := ws.selectedSession(); r != nil {
			sess := r.s
			add("R", "resume "+shortID(sess.ID)+" in claude", func() tea.Cmd { return resume(svc, sess) })
			add("L", "point an account's last session at "+shortID(sess.ID), func() tea.Cmd {
				sc, err := newPointerScreen(svc, sess)
				if err != nil {
					return statusError(err)
				}
				return pushScreen(sc)
			})
		}
	}
	if ws.tab == tabSessions {
		if chosen := ws.chosen(); len(chosen) > 0 {
			add("r", "rehome "+countNoun(len(chosen), "session"), func() tea.Cmd { return pushScreen(newRehomeScreen(svc, tgt, chosen)) })
		}
	}
	if cwd := ws.projectCwd(); cwd != "" {
		add("t", "trust matrix for "+label, func() tea.Cmd { return pushScreen(newTrustScreen(svc, cwd)) })
	}
	if dir := ws.memoryDir(); dir != "" {
		mt := tgt.memoryOnly()
		add("S", "sync the memory of "+label+" to a full-isolation account", func() tea.Cmd { return pushScreen(newCopyScreen(svc, mt)) })
		add("p", "scan the memory of "+label+" for paths", func() tea.Cmd { return pushScreen(newScanPathsScreen(svc, dir)) })
	}
	return items
}

// actionHints explain a refused action key.
var actionHints = map[string]string{
	"e": "select a project first", "s": "select a project first", "c": "select a project first",
	"R": "select a session on the sessions tab", "L": "select a session on the sessions tab",
	"r": "nothing to rehome: select sessions with space (no pending imports)",
	"t": "no cwd recorded for this project; nothing to trust",
	"S": "no memory dir for this project", "p": "no memory dir for this project",
	"d": "d never deletes — use bffs sessions rm",
}

// action runs the action behind a key, or explains why it cannot.
func (ws *workspace) action(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	k := msg.String()
	for _, it := range ws.actions() {
		if it.key == k {
			return it.run(), true
		}
	}
	if hint, ok := actionHints[k]; ok {
		return status(hint), true
	}
	return nil, false
}

// keys are the bindings the help line shows for the focused panel.
func (ws *workspace) keys() []key.Binding {
	ks := []key.Binding{keys.Up, keys.Down, keys.NextPanel}
	switch {
	case ws.mainFocus:
		return []key.Binding{keys.Up, keys.Down, keys.PageUp, keys.PageDn, key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back to panels"))}
	case ws.focus == panelItems && ws.tab == tabSessions:
		ks = append(ks, keys.Open, keys.Select, keys.NextTab, keys.Resume, keys.Rehome, keys.Pointer)
	case ws.focus == panelItems:
		ks = append(ks, keys.Open, keys.PrevTab, keys.SyncMemory)
	case ws.focus == panelProjects:
		ks = append(ks, keys.Open, keys.Export, keys.Send, keys.Copy, keys.Trust)
	case ws.focus == panelRoots:
		ks = append(ks, keys.Open, keys.Receive)
	default:
		ks = append(ks, keys.Open)
	}
	if ws.panels[ws.focus].list.FilterState() == list.FilterApplied {
		ks = append(ks, keys.ClearFilter)
	}
	return append(ks, keys.Filter, keys.Menu)
}

// helpGroups are the full key listing for the ? overlay.
func (ws *workspace) helpGroups() [][]key.Binding {
	return [][]key.Binding{
		{keys.Panel1, keys.Panel2, keys.Panel3, keys.Panel4, keys.NextPanel, keys.PrevPanel, keys.NextTab, keys.PrevTab},
		{keys.Up, keys.Down, keys.PageUp, keys.PageDn, keys.Open, keys.Select, keys.SelectAll, keys.Filter},
		{keys.Export, keys.Send, keys.Receive, keys.Copy, keys.Rehome, keys.Resume},
		{keys.Trust, keys.SyncMemory, keys.Pointer, keys.ScanPaths},
		{keys.ScreenMode, keys.ScreenModePrev, keys.Menu, keys.Help, keys.Back, keys.Quit, reservedKeys},
	}
}

// crumb is the header's context.
func (ws *workspace) crumb() string {
	if ws.rootKey == "" {
		return ""
	}
	parts := []string{shortRootLabel(ws.root)}
	if ws.project != nil {
		parts = append(parts, ws.projectLabel())
		if ws.tab == tabMemory {
			parts = append(parts, "memory")
		} else {
			parts = append(parts, "sessions")
		}
	}
	return strings.Join(parts, " › ")
}

// previewTitle names the main pane's subject.
func (ws *workspace) previewTitle() string {
	switch {
	case ws.previewBusy:
		return "preview …"
	case strings.HasPrefix(ws.previewKey, "session:"):
		if r := ws.selectedSession(); r != nil {
			return "session " + shortID(r.s.ID)
		}
	case strings.HasPrefix(ws.previewKey, "drift:"):
		return "project " + ws.projectLabel()
	case strings.HasPrefix(ws.previewKey, "memfile:"):
		if r := ws.selectedMemFile(); r != nil {
			return "memory " + transcripts.Sanitize(r.f.Name)
		}
	case strings.HasPrefix(ws.previewKey, "root:"):
		return "root"
	}
	return "preview"
}

// --- drawing --------------------------------------------------------------

// cell fits a possibly styled line into exactly width cells.
func cell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if lipgloss.Width(s) > width {
		s = ansi.Truncate(s, width, ellipsis)
	}
	if n := width - lipgloss.Width(s); n > 0 {
		s += strings.Repeat(" ", n)
	}
	return s
}

// titled draws a horizontal border of inner cells carrying a title.
func titled(title string, inner int, focused bool) string {
	if inner <= 0 {
		return ""
	}
	t := " " + truncate(title, max(0, inner-3)) + " "
	w := lipgloss.Width(t)
	if w > inner-1 {
		t, w = "", 0
	}
	if focused {
		t = styleHeader.Render(t)
	} else {
		t = styleFaint.Render(t)
	}
	return "─" + t + strings.Repeat("─", max(0, inner-1-w))
}

// panelTitle is the frame title of panel i.
func (ws *workspace) panelTitle(i panelID) string {
	p := ws.panels[i]
	if i == panelItems {
		if ws.tab == tabSessions {
			return p.title("sessions (memory)")
		}
		return p.title("memory (sessions)")
	}
	return p.title("")
}

// View draws the frame: the side column with its panels and the main
// pane. main and mainTitle override the preview (an overlay's view);
// nil draws the preview.
func (ws *workspace) View(width, height int, main []string, mainTitle string, mainFocused bool) string {
	if width != ws.width || height != ws.height {
		ws.setSize(width, height)
	}
	side, mi, h := ws.sideWidth(), ws.mainWidth(), ws.bodyHeight()
	if main == nil {
		main = strings.Split(ws.vp.View(), "\n")
		mainTitle, mainFocused = ws.previewTitle(), ws.mainFocus
	}
	var out []string
	if side == 0 {
		main = fill(main, h)
		out = append(out, "┌"+titled(mainTitle, mi, mainFocused)+"┐")
		for _, l := range main {
			out = append(out, "│"+cell(l, mi)+"│")
		}
		out = append(out, "└"+strings.Repeat("─", mi)+"┘")
		return strings.Join(out, "\n")
	}
	hs := ws.panelHeights()
	need := int(panelCount) - 1
	for _, n := range hs {
		need += n
	}
	main = fill(main, max(h, need))
	row := 0
	next := func() string {
		l := ""
		if row < len(main) {
			l = main[row]
		}
		row++
		return cell(l, mi)
	}
	for i, p := range ws.panels {
		focused := panelID(i) == ws.focus && !ws.mainFocus
		if i == 0 {
			out = append(out, "┌"+titled(ws.panelTitle(panelID(i)), side, focused)+"┬"+titled(mainTitle, mi, mainFocused)+"┐")
		} else {
			out = append(out, "├"+titled(ws.panelTitle(panelID(i)), side, focused)+"┤"+next()+"│")
		}
		for _, l := range p.body(side, hs[i], focused) {
			out = append(out, "│"+cell(l, side)+"│"+next()+"│")
		}
	}
	out = append(out, "└"+strings.Repeat("─", side)+"┴"+strings.Repeat("─", mi)+"┘")
	return strings.Join(out, "\n")
}
