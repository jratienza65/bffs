package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// wizardStep is where the transfer wizard is.
type wizardStep int

const (
	wizStart    wizardStep = iota // send or receive
	wizSendWhat                   // what to send: parts, memory, live sessions
	wizSendHow                    // over the LAN, to a file, through ssh
	wizSendSSH                    // the command to run in a terminal
	wizRecvFrom                   // a machine on the LAN, or a file
	wizRecvFile                   // typing the file's path
)

// wizardScreen walks a transfer between machines step by step: send or
// receive; what to send and which parts to leave out; how — over the
// local network with a pairing code, to a .bffs file, or through ssh —
// or where a received bundle comes from. Every step says what happens
// next and hands off to the screen that does the work (serve, export,
// receive) with the choices made here, so nothing runs before the last
// step.
type wizardScreen struct {
	svc        *services
	tgt        actionTarget
	root       transcripts.Root
	account    string // the perspective an import runs as ("" = the resolver's pick)
	hasProject bool
	step       wizardStep
	cursor     int
	parts      porter.Parts
	live       bool
	input      textinput.Model
	note       string
	copied     bool
	width      int
	height     int

	// The checklist of the "what" step: every session of the project
	// and every file of its memory directory, checked unless the panel
	// had marked a subset.
	sessions []checkItem
	memFiles []checkItem
	rows     []checkRow // the rendered rows, rebuilt on each change
	offset   int        // scroll offset of the checklist
}

// checkItem is one checkable thing: a session or a memory file.
type checkItem struct {
	id    string // session id, or the memory file's slash-relative name
	label string
	meta  string
	on    bool
}

// checkRow is one line of the checklist: a section header (no cursor
// stops there), a checkable item, a part toggle, or the continue row.
type checkRow struct {
	header string
	item   *checkItem
	on     *bool  // a part or option toggle
	label  string // toggle label / continue
	desc   string
	cont   bool
}

func (r checkRow) selectable() bool { return r.header == "" }

func newWizardScreen(svc *services, tgt actionTarget, root transcripts.Root, account string, hasProject bool) *wizardScreen {
	in := newInput("file: ", "path of a .bffs file written by bffs export")
	w := &wizardScreen{svc: svc, tgt: tgt, root: root, account: account, hasProject: hasProject, parts: porter.DefaultParts, live: true, input: in}
	marked := map[string]bool{}
	for _, id := range tgt.ids {
		marked[id] = true
	}
	for _, sess := range tgt.rows {
		title := transcripts.Sanitize(sess.Title)
		if title == "" {
			title = "(" + shortID(sess.ID) + ")"
		}
		meta := humanizeAgo(sess.LastTS, svc.now()) + " · " + formatSize(sess.Size)
		if sess.Live {
			meta += " · live"
		}
		w.sessions = append(w.sessions, checkItem{id: sess.ID, label: sessionGlyph(sess) + " " + title, meta: meta, on: len(marked) == 0 || marked[sess.ID]})
	}
	if tgt.project != "" {
		if dir := memoryDirFor(root, tgt.slug, tgt.project); dir != "" {
			for _, f := range listMemoryFiles(dir) {
				meta := formatSize(f.size)
				if f.pinned {
					meta += " · pinned"
				}
				w.memFiles = append(w.memFiles, checkItem{id: f.name, label: f.name, meta: meta, on: true})
			}
		}
	}
	w.rebuild()
	w.cursor = 0 // step 1 starts on "send"; the checklist places its own cursor
	return w
}

// memoryListing is one exportable memory file.
type memoryListing struct {
	name   string
	size   int64
	pinned bool
}

// listMemoryFiles lists the .md files of a memory directory the way the
// export walks it (top level plus logs/), slash-relative, sorted.
func listMemoryFiles(dir string) []memoryListing {
	var out []memoryListing
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != transcripts.MemoryLogsSubdir && !strings.HasPrefix(rel, transcripts.MemoryLogsSubdir+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(rel, ".md") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		pinned := false
		if f, ferr := os.Open(path); ferr == nil {
			buf := make([]byte, 512)
			n, _ := f.Read(buf)
			f.Close()
			head := string(buf[:n])
			pinned = strings.HasPrefix(head, "---") && strings.Contains(head, "\npinned: true")
		}
		out = append(out, memoryListing{name: rel, size: info.Size(), pinned: pinned})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// rebuild lays the checklist out from its items and toggles.
func (s *wizardScreen) rebuild() {
	var rows []checkRow
	if len(s.tgt.ids) == 0 && s.tgt.project != "" || len(s.sessions) > 0 {
		rows = append(rows, checkRow{header: "SESSIONS"})
	}
	for i := range s.sessions {
		rows = append(rows, checkRow{item: &s.sessions[i]})
	}
	if len(s.sessions) == 0 && s.tgt.project != "" {
		rows = append(rows, checkRow{header: "", label: "(sessions still loading, or none; the export takes what the project has)"})
	}
	if len(s.memFiles) > 0 {
		rows = append(rows, checkRow{header: "MEMORY"})
		for i := range s.memFiles {
			rows = append(rows, checkRow{item: &s.memFiles[i]})
		}
	}
	rows = append(rows, checkRow{header: "PARTS OF EVERY SELECTED SESSION"},
		checkRow{on: &s.parts.ToolResults, label: "tool results", desc: "saved tool outputs — may contain pasted secrets"},
		checkRow{on: &s.parts.FileHistory, label: "file history", desc: "backups of files Claude edited, what /rewind uses"},
		checkRow{on: &s.parts.History, label: "prompt history", desc: "the lines claude shows when you press up"},
		checkRow{on: &s.live, label: "live sessions", desc: "sessions open in a running claude — exported read-only, possibly truncated"},
		checkRow{cont: true, label: "continue →", desc: "choose how to send"})
	s.rows = rows
	if s.cursor < 0 || s.cursor >= len(rows) || !rows[s.cursor].selectable() {
		s.cursor = max(0, s.firstSelectable(0, 1))
	}
}

// firstSelectable is the next selectable row from i in direction d, or
// -1 when there is none that way.
func (s *wizardScreen) firstSelectable(i, d int) int {
	for j := i; j >= 0 && j < len(s.rows); j += d {
		if s.rows[j].selectable() && (s.rows[j].item != nil || s.rows[j].on != nil || s.rows[j].cont) {
			return j
		}
	}
	return -1
}

// move steps the cursor n selectable rows in direction d.
func (s *wizardScreen) move(d, n int) {
	for ; n > 0; n-- {
		i := s.firstSelectable(s.cursor+d, d)
		if i < 0 {
			return
		}
		s.cursor = i
	}
}

// counts summarises the checklist.
func (s *wizardScreen) counts() string {
	on, total := 0, len(s.sessions)
	for _, it := range s.sessions {
		if it.on {
			on++
		}
	}
	parts := []string{fmt.Sprintf("%d of %d sessions", on, total)}
	if len(s.memFiles) > 0 {
		mon := 0
		for _, it := range s.memFiles {
			if it.on {
				mon++
			}
		}
		parts = append(parts, fmt.Sprintf("%d of %d memory files", mon, len(s.memFiles)))
	}
	return strings.Join(parts, " · ")
}

// sectionOf is the items of the section the cursor is in.
func (s *wizardScreen) sectionOf(i int) []checkItem {
	for j := i; j >= 0; j-- {
		switch s.rows[j].header {
		case "SESSIONS":
			return s.sessions
		case "MEMORY":
			return s.memFiles
		case "":
			continue
		default:
			return nil
		}
	}
	return nil
}

// toggleSection checks every item of the cursor's section, or clears
// it when every item is already checked.
func (s *wizardScreen) toggleSection() {
	items := s.sectionOf(s.cursor)
	if len(items) == 0 {
		return
	}
	all := true
	for _, it := range items {
		if !it.on {
			all = false
			break
		}
	}
	for i := range items {
		items[i].on = !all
	}
}

func (s *wizardScreen) Init() tea.Cmd        { return nil }
func (s *wizardScreen) Title() string        { return "transfer wizard" }
func (s *wizardScreen) capturingInput() bool { return s.step == wizRecvFile }

func (s *wizardScreen) Keys() []key.Binding {
	back := key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))
	switch s.step {
	case wizSendWhat:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")), key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "all/none in section")), key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "continue")), back}
	case wizSendSSH:
		return []key.Binding{key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy the command")), back}
	case wizRecvFile:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "review the bundle")), back}
	}
	return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")), back}
}

// wizardOption is one choice of a step: a label, what it means, and
// for the toggles of the "what" step the current state.
type wizardOption struct {
	label string
	desc  string
	on    *bool
}

// options are the choices of the current step.
func (s *wizardScreen) options() []wizardOption {
	switch s.step {
	case wizStart:
		return []wizardOption{
			{label: "Send sessions from this machine", desc: "serve them over the local network with a pairing code, write a .bffs file, or pipe them through ssh"},
			{label: "Receive sessions from another machine", desc: "from a machine running the serve, or from a .bffs file that reached this one"},
		}
	case wizSendHow:
		return []wizardOption{
			{label: "Over the local network", desc: "this machine shows an address and a pairing code; the other one runs bffs import --from <address> (or w → receive) and types the code. Needs an inbound port; macOS asks once whether bffs may accept connections"},
			{label: "To a .bffs file", desc: "written here; carry it however you like and import it on the other machine (w → receive → file, or bffs import --from <file>)"},
			{label: "Through ssh", desc: "no inbound port needed: a one-line command pipes the bundle straight into bffs import on the other machine; shown for you to run in a terminal"},
		}
	case wizRecvFrom:
		return []wizardOption{
			{label: "From another machine on the local network", desc: "it runs bffs export --serve (or w → send there) and shows an address and a pairing code"},
			{label: "From a .bffs file on this machine", desc: "written by bffs export on the other machine"},
		}
	}
	return nil
}

// target is the export target with the choices of the "what" step:
// the parts, the live toggle, and the checklist when it narrows the
// project (a subset of sessions or of memory files).
func (s *wizardScreen) target() actionTarget {
	t := s.tgt
	parts := s.parts
	t.parts = &parts
	t.noLive = !s.live
	if len(s.sessions) > 0 {
		keep := map[string]bool{}
		all := true
		for _, it := range s.sessions {
			if it.on {
				keep[it.id] = true
			} else {
				all = false
			}
		}
		if !all || len(t.ids) > 0 {
			t.keepSessions = keep
			t.ids = nil
		}
	}
	if len(s.memFiles) > 0 {
		keep := map[string]bool{}
		all := true
		for _, it := range s.memFiles {
			if it.on {
				keep[it.id] = true
			} else {
				all = false
			}
		}
		switch {
		case len(keep) == 0:
			t.only = "sessions"
		case !all:
			t.keepMemory = keep
		}
	}
	return t
}

// nothingChecked reports whether the checklist sends nothing.
func (s *wizardScreen) nothingChecked() bool {
	for _, it := range s.sessions {
		if it.on {
			return false
		}
	}
	for _, it := range s.memFiles {
		if it.on {
			return false
		}
	}
	return len(s.sessions)+len(s.memFiles) > 0
}

// sshCommand is the pipe the ssh step shows.
func (s *wizardScreen) sshCommand() string {
	t := s.target()
	var b strings.Builder
	b.WriteString("bffs export")
	switch {
	case t.keepSessions != nil:
		ids := make([]string, 0, len(t.keepSessions))
		for id := range t.keepSessions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b.WriteString(" --session " + shellWord(id))
		}
		if t.keepMemory != nil || (t.only != "sessions" && t.project != "") {
			b.WriteString(" --project " + shellWord(t.project))
		}
	case len(t.ids) > 0:
		for _, id := range t.ids {
			b.WriteString(" --session " + shellWord(id))
		}
	case t.project != "":
		b.WriteString(" --project " + shellWord(t.project))
	}
	if t.only == "sessions" {
		b.WriteString(" --only sessions")
	}
	if !t.parts.ToolResults {
		b.WriteString(" --no-tool-results")
	}
	if !t.parts.FileHistory {
		b.WriteString(" --no-file-history")
	}
	if !t.parts.History {
		b.WriteString(" --no-history")
	}
	if t.noLive {
		b.WriteString(" --no-live")
	}
	if t.root.Owner != "" {
		b.WriteString(" --account " + shellWord(t.root.Owner))
	}
	b.WriteString(" --out - | ssh <other-machine> 'bffs import --from - -y --as-is'")
	return b.String()
}

// canSend says whether there is anything to send.
func (s *wizardScreen) canSend() bool {
	return len(s.tgt.ids) > 0 || s.tgt.project != "" || (s.hasProject && len(s.tgt.rows) > 0)
}

func (s *wizardScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.input.SetWidth(max(10, msg.Width-8))
		return s, nil
	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	if s.step == wizRecvFile {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	return s, nil
}

func (s *wizardScreen) keyPress(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.step == wizRecvFile {
		switch {
		case key.Matches(msg, keys.Cancel):
			s.step, s.cursor, s.note = wizRecvFrom, 1, ""
			return s, nil
		case msg.Code == tea.KeyEnter:
			raw := strings.TrimSpace(s.input.Value())
			path, err := validBundlePath(raw)
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			return s, replaceScreen(newReceiveFileScreen(s.svc, s.root, s.account, path))
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	if s.step == wizSendWhat {
		return s.checklistKey(msg)
	}
	opts := s.options()
	switch {
	case key.Matches(msg, keys.Cancel):
		return s.back()
	case key.Matches(msg, keys.Up):
		s.cursor = max(0, s.cursor-1)
	case key.Matches(msg, keys.Down):
		s.cursor = min(len(opts)-1, s.cursor+1)
	case msg.String() == "c" && s.step == wizSendSSH:
		s.copied = true
		return s, tea.SetClipboard(s.sshCommand())
	case msg.Code == tea.KeyEnter:
		return s.choose()
	}
	return s, nil
}

// checklistKey drives the "what" step: the cursor skips headers, space
// toggles, a toggles a whole section, enter continues.
func (s *wizardScreen) checklistKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Cancel):
		return s.back()
	case key.Matches(msg, keys.Up):
		s.move(-1, 1)
	case key.Matches(msg, keys.Down):
		s.move(1, 1)
	case key.Matches(msg, keys.PageUp):
		s.move(-1, 10)
	case key.Matches(msg, keys.PageDn):
		s.move(1, 10)
	case msg.Code == tea.KeySpace:
		s.toggleRow()
	case msg.String() == "a":
		s.toggleSection()
	case msg.Code == tea.KeyEnter:
		if s.rows[s.cursor].cont {
			return s.choose()
		}
		s.toggleRow()
	}
	s.note = ""
	return s, nil
}

// toggleRow flips the checkable thing under the cursor.
func (s *wizardScreen) toggleRow() {
	r := s.rows[s.cursor]
	switch {
	case r.item != nil:
		r.item.on = !r.item.on
	case r.on != nil:
		*r.on = !*r.on
	}
}

// back is esc: one step up, or close.
func (s *wizardScreen) back() (Screen, tea.Cmd) {
	s.note = ""
	switch s.step {
	case wizStart:
		return s, popScreen()
	case wizSendWhat, wizRecvFrom:
		s.step, s.cursor = wizStart, 0
	case wizSendHow:
		s.step = wizSendWhat
		s.cursor = max(0, s.firstSelectable(0, 1))
	case wizSendSSH:
		s.step, s.cursor, s.copied = wizSendHow, 2, false
	}
	return s, nil
}

// choose is enter on the current step.
func (s *wizardScreen) choose() (Screen, tea.Cmd) {
	s.note = ""
	switch s.step {
	case wizStart:
		if s.cursor == 0 {
			if !s.canSend() {
				s.note = "nothing to send: select a project (2) or mark sessions (space) first"
				return s, nil
			}
			s.step = wizSendWhat
			s.rebuild()
			s.cursor = max(0, s.firstSelectable(0, 1))
		} else {
			s.step, s.cursor = wizRecvFrom, 0
		}
	case wizSendWhat:
		if s.nothingChecked() {
			s.note = "nothing selected: check at least one session or memory file (space)"
			return s, nil
		}
		s.step, s.cursor = wizSendHow, 0
	case wizSendHow:
		switch s.cursor {
		case 0:
			sc, err := newServeScreen(s.svc, s.target())
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			return s, replaceScreen(sc)
		case 1:
			return s, replaceScreen(newExportScreen(s.svc, s.target()))
		default:
			s.step = wizSendSSH
		}
	case wizSendSSH:
		return s, nil
	case wizRecvFrom:
		if s.root.Orphan {
			s.note = "an orphan session dir is read-only; pick an account (1) to receive into"
			return s, nil
		}
		if s.cursor == 0 {
			return s, replaceScreen(newReceiveScreen(s.svc, s.root, s.account))
		}
		s.step = wizRecvFile
		s.input.SetValue("")
		s.input.Focus()
	}
	return s, nil
}

// validBundlePath resolves what was typed: an existing regular file.
func validBundlePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("type the path of the .bffs file")
	}
	path := raw
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist", shortPath(path))
	}
	if err != nil {
		return "", fmt.Errorf("%s: %v", shortPath(path), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", shortPath(path))
	}
	return path, nil
}

// checklistView renders the rows around the cursor within height.
func (s *wizardScreen) checklistView(lines []string, width, height int) string {
	body := make([]string, 0, len(s.rows))
	for i, r := range s.rows {
		var line string
		switch {
		case r.header != "":
			line = section(r.header, "")
		case r.item != nil:
			box := "[ ]"
			if r.item.on {
				box = "[x]"
			}
			metaW := lipglossWidthOf(r.item.meta)
			label := pad(r.item.label, max(8, width-4-2-metaW-3))
			line = box + " " + label + "  " + styleFaint.Render(r.item.meta)
		case r.on != nil:
			state := "include"
			if !*r.on {
				state = "leave out"
			}
			line = pad(r.label, 16) + state + "   " + styleFaint.Render(r.desc)
		case r.cont:
			line = styleHeader.Render(r.label) + "   " + styleFaint.Render(r.desc)
		default:
			line = styleFaint.Render(r.label)
		}
		if i == s.cursor {
			line = styleCursor.Render("> " + cell(line, max(0, width-2)))
		} else {
			line = "  " + cell(line, max(0, width-2))
		}
		body = append(body, line)
	}
	// Keep the cursor visible in the rows the pane has left.
	avail := max(3, height-len(lines)-2)
	if s.cursor < s.offset {
		s.offset = s.cursor
	}
	if s.cursor >= s.offset+avail {
		s.offset = s.cursor - avail + 1
	}
	if s.offset > 0 {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("  ↑ %d more", s.offset)))
	}
	end := min(len(body), s.offset+avail)
	lines = append(lines, body[s.offset:end]...)
	if end < len(body) {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("  ↓ %d more", len(body)-end)))
	}
	if s.note != "" {
		lines = append(lines, "", styleError.Render(truncate(s.note, width)))
	}
	return strings.Join(lines, "\n")
}

func (s *wizardScreen) View(width, height int) string {
	var lines []string
	head := func(step, text string) {
		lines = append(lines, styleFaint.Render(step)+"  "+styleHeader.Render(text), "")
	}
	switch s.step {
	case wizStart:
		head("step 1 of 3", "What do you want to do?")
	case wizSendWhat:
		head("step 2 of 3", "What to send from "+s.tgt.label())
		lines = append(lines, styleFaint.Render(truncate(s.counts()+" · space toggles · a checks or clears a section · enter continues · nothing is read yet", width)), "")
		return s.checklistView(lines, width, height)
	case wizSendHow:
		head("step 3 of 3", "How to send it")
	case wizSendSSH:
		head("send through ssh", "Run this in a terminal on this machine")
		lines = append(lines, "", "  "+s.sshCommand(), "",
			styleFaint.Render("  replace <other-machine> with its ssh host; -y --as-is imports without questions and you rehome there (r) afterwards"))
		if s.copied {
			lines = append(lines, "", styleOK.Render("  copied to the clipboard (OSC 52)"))
		} else {
			lines = append(lines, "", styleFaint.Render("  c copies it to the clipboard · esc goes back"))
		}
		return joinLines(lines, width)
	case wizRecvFrom:
		acct := s.account
		if acct == "" {
			acct = destAccount(s.svc, s.root)
		}
		if acct == "" {
			acct = transcripts.HomeName
		}
		head("step 2 of 2", "Where from?")
		lines = append(lines, styleFaint.Render(truncate(fmt.Sprintf("into %s as account %q — 1 accounts changes that", shortRootLabel(s.root), acct), width)), "")
	case wizRecvFile:
		head("receive from a file", "Which file?")
		lines = append(lines, s.input.View())
		if s.note != "" {
			lines = append(lines, styleError.Render(truncate(s.note, width)))
		} else {
			lines = append(lines, styleFaint.Render("the manifest is shown and confirmed before anything is written"))
		}
		return strings.Join(lines, "\n")
	}
	for i, o := range s.options() {
		label := o.label
		if o.on != nil {
			state := "include"
			if !*o.on {
				state = "leave out"
			}
			label = pad(o.label, 16) + state
		}
		line := pad(label, max(0, width-2))
		if i == s.cursor {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line, styleFaint.Render(truncate("      "+o.desc, width)))
	}
	if s.note != "" {
		lines = append(lines, "", styleError.Render(truncate(s.note, width)))
	}
	return strings.Join(lines, "\n")
}

// lipglossWidthOf is lipgloss.Width, named for the checklist layout.
func lipglossWidthOf(v string) int { return lipgloss.Width(v) }
