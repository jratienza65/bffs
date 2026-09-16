package tui

import (
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// app is the root model: the workspace (panels + preview), a stack of
// overlays drawn in the main pane (action screens, results, the menu,
// the keys), the terminal size, a one-line status and the help bubble.
// Navigation between overlays happens only through messages
// (push/pop/replace); the overlay on top gets every message the app
// does not consume itself, the workspace gets the data messages.
type app struct {
	svc    *services
	ws     *workspace
	stack  []Screen // overlays, bottom to top
	width  int
	height int

	status     string
	statusKind noteKind
	statusAt   time.Time
	toast      toast
	toastSeq   int
	help       help.Model
	quitting   bool
	tracer     *os.File // BFFS_DEBUG, nil when unset (trace.go)
}

func newApp(svc *services) *app {
	// Before anything that bakes a glyph into a value: the help
	// bubble's separator, the key map, the fixed prompts.
	resolveGlyphs()
	h := help.New()
	h.ShortSeparator = sepDot
	h.Ellipsis = glyph.ellipsis
	name, note := resolveThemeName(svc.state.Theme, osEnv)
	if note != "" {
		svc.warnings = append(svc.warnings, note)
	}
	svc.theme, svc.isDark = name, true
	p, _ := paletteByName(name)
	themeProfile = detectProfile()
	applyTheme(p, true)
	return &app{svc: svc, ws: newWorkspace(svc), help: h, tracer: openTrace()}
}

// Init hands the app the roots the services already enumerated (the
// message path is the same one a later reload would take) and asks the
// terminal for its background colour so the theme can pick its light or
// dark variants.
func (a *app) Init() tea.Cmd {
	svc := a.svc
	return tea.Batch(func() tea.Msg { return rootsLoadedMsg{roots: svc.roots, warnings: svc.warnings} }, tea.RequestBackgroundColor)
}

// top is the visible overlay, nil when the workspace has the keys.
func (a *app) top() Screen {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1]
}

// busy reports whether the top overlay runs a long operation:
// navigation is then blocked and every key is the overlay's (esc
// cancels, q asks whether to quit anyway).
func (a *app) busy() bool {
	r, ok := a.top().(running)
	return ok && r.running()
}

// contentHeight is what remains for the frame after the header, the
// status line and the help line.
func (a *app) contentHeight() int { return max(0, a.height-3) }

// mainSize is the size an overlay draws into: the main pane's inside.
func (a *app) mainSize() tea.WindowSizeMsg {
	w := a.ws.mainInner()
	if w == 0 && a.top() != nil {
		// Below the breakpoint the side column takes the width, but an
		// overlay is always drawn in the main pane alone (workspace.View
		// widens it there), so it is as wide as the frame. Telling it
		// zero is how every head and hint came out empty.
		w = max(0, a.ws.width-2-2*padX)
	}
	return tea.WindowSizeMsg{Width: w, Height: a.ws.bodyHeight()}
}

// forward sends msg to the top overlay and stores what it returns.
func (a *app) forward(msg tea.Msg) tea.Cmd {
	s := a.top()
	if s == nil {
		return nil
	}
	next, cmd := s.Update(msg)
	if next != nil {
		a.stack[len(a.stack)-1] = next
	}
	return cmd
}

// push puts s on top, tells it the size and runs its Init.
func (a *app) push(s Screen) tea.Cmd {
	a.stack = append(a.stack, s)
	a.clearNote()
	init := s.Init()
	if a.width > 0 {
		return tea.Batch(init, a.forward(a.mainSize()))
	}
	return init
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that cannot report its size (a bare pty) reads as
		// 0×0; draw for 80×24 rather than nothing.
		if msg.Width <= 0 || msg.Height <= 0 {
			msg.Width, msg.Height = 80, 24
		}
		a.width, a.height = msg.Width, msg.Height
		a.help.SetWidth(msg.Width)
		cmd := a.ws.setSize(msg.Width, a.contentHeight())
		return a, tea.Batch(cmd, a.forward(a.mainSize()))

	case tea.BackgroundColorMsg:
		if dark := msg.IsDark(); dark != a.svc.isDark {
			a.svc.isDark = dark
			if p, ok := paletteByName(a.svc.theme); ok {
				applyTheme(p, dark)
			}
			a.ws.previewKey = "" // rebuild the preview in the new variant
			return a, a.ws.sync()
		}
		return a, nil

	case rootsLoadedMsg:
		var cmds []tea.Cmd
		if len(msg.warnings) > 0 {
			cmds = append(cmds, status("warning: "+strings.Join(msg.warnings, "; ")))
		}
		cmds = append(cmds, a.ws.setRoots(msg.roots))
		return a, tea.Batch(cmds...)

	case pushScreenMsg:
		cmd := a.push(msg.screen)
		a.trace("push")
		return a, cmd

	case replaceScreenMsg:
		if len(a.stack) > 0 {
			a.stack = a.stack[:len(a.stack)-1]
		}
		cmd := a.push(msg.screen)
		a.trace("replace")
		return a, cmd

	case popScreenMsg:
		if len(a.stack) == 0 {
			return a, nil
		}
		a.stack = a.stack[:len(a.stack)-1]
		a.clearNote()
		cmd := a.forward(a.mainSize())
		if msg.refresh {
			cmd = tea.Batch(cmd, a.ws.Update(refreshMsg{}))
		}
		a.trace("pop")
		return a, cmd

	case menuChoiceMsg:
		if _, ok := a.top().(*menuScreen); ok {
			a.stack = a.stack[:len(a.stack)-1]
		}
		return a, msg.run()

	case statusMsg:
		if msg.err != nil {
			return a, a.post(noteBad, msg.err.Error())
		}
		return a, a.post(msg.kind, msg.text)

	case statusOutMsg:
		// Only the note this timer was started for: a newer one has a
		// later timestamp and its own tick.
		if msg.at.Equal(a.statusAt) {
			a.clearNote()
		}
		return a, nil

	case toastMsg:
		return a, a.notify(msg.kind, msg.title, msg.body...)

	case toastOutMsg:
		if msg.seq == a.toast.seq {
			a.toast = toast{}
		}
		return a, nil

	case opDoneMsg:
		var note tea.Cmd
		if msg.err != nil {
			note = a.post(noteBad, msg.err.Error())
		}
		return a, tea.Batch(note, a.forward(msg))

	case quitMsg:
		a.quitting = true
		return a, tea.Quit

	case tea.KeyPressMsg:
		cmd := a.handleKey(msg)
		a.trace("key:" + msg.String())
		return a, cmd

	case tea.MouseMsg:
		cmd := a.handleMouse(msg)
		a.trace(traceMouse(msg))
		return a, cmd
	}
	// Data messages: the workspace's and the overlay's are disjoint
	// types, so both see everything else.
	return a, tea.Batch(a.ws.Update(msg), a.forward(msg))
}

// modeLabel leads the footer with the mode the keys are in. The one
// question a key press asks is whether letters act or type, and only
// the mode answers it: the same letter exports a bundle in the browser
// and lands in a filename in an input. The second return says which,
// so the label can be painted accordingly.
func (a *app) modeLabel() (string, bool) {
	if a.busy() {
		return "RUNNING", false
	}
	if s := a.top(); s != nil {
		if c, ok := s.(inputCapturer); ok && c.capturingInput() {
			return "TYPE", true
		}
		switch s.(type) {
		case *helpScreen:
			return "KEYS", false
		case *menuScreen:
			return "MENU", false
		case *transcriptScreen:
			return "READ", false
		case *driftScreen:
			return "DIFF", false
		}
		if l, ok := s.(loader); ok && l.loading() {
			return "LOADING", false
		}
		return "ACTION", false
	}
	if a.ws.panels[a.ws.focus].filtering() {
		return "FILTER", true
	}
	if a.ws.mainFocus {
		return "READ", false
	}
	return "BROWSE", false
}

// copyOverlay puts the text an overlay is showing on the clipboard, as
// drawn and with the styling stripped.
func (a *app) copyOverlay(s Screen) tea.Cmd {
	size := a.mainSize()
	text := strings.TrimRight(ansi.Strip(s.View(size.Width, size.Height)), " \n")
	if strings.TrimSpace(text) == "" {
		return statusWarn("nothing to copy here")
	}
	return tea.Batch(tea.SetClipboard(text), statusDone(copiedNote(text)))
}

// post puts a note on the status line and starts the timer that takes
// it down: a note left standing is read as the answer to the next key.
func (a *app) post(kind noteKind, text string) tea.Cmd {
	a.status, a.statusKind, a.statusAt = text, kind, time.Now()
	if text == "" {
		return nil
	}
	return expireNote(a.statusAt)
}

// clearNote drops the note now (a key was pressed, an overlay opened or
// closed) without disturbing a later note's timer.
func (a *app) clearNote() {
	a.status, a.statusKind, a.statusAt = "", noteInfo, time.Time{}
}

// handleKey is the key policy: while an operation runs every key is the
// overlay's (ctrl+c and esc cancel it, q asks before quitting);
// otherwise ctrl+c always quits; an overlay or panel typing into a text
// field owns everything else; then q quits, ? opens or closes the keys,
// esc closes an overlay, a reserved action key an overlay does not bind
// answers with the hint, and the rest is the overlay's or the
// workspace's.
func (a *app) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	if a.busy() {
		return a.forward(msg)
	}
	if msg.String() == "ctrl+c" {
		a.quitting = true
		return tea.Quit
	}
	if s := a.top(); s != nil {
		if c, ok := s.(inputCapturer); ok && c.capturingInput() {
			return a.forward(msg)
		}
		screenKeys := s.Keys()
		switch {
		case key.Matches(msg, keys.Quit) && !key.Matches(msg, screenKeys...):
			a.quitting = true
			return tea.Quit
		case key.Matches(msg, keys.Help) && !key.Matches(msg, screenKeys...):
			if _, ok := s.(*helpScreen); ok {
				return popScreen()
			}
			return a.push(newHelpScreen(a.overlayHelpGroups(s)))
		case key.Matches(msg, keys.Back) && !key.Matches(msg, screenKeys...):
			return popScreen()
		case key.Matches(msg, keys.Yank) && !key.Matches(msg, screenKeys...):
			// An overlay is not a viewport, so a drag cannot select in
			// it; y takes what it is showing instead. A confirmation
			// binds y for "yes", and the guard above leaves it alone.
			return a.copyOverlay(s)
		case key.Matches(msg, reservedKeys) && !key.Matches(msg, screenKeys...):
			return statusWarn(reservedHint)
		}
		return a.forward(msg)
	}
	if a.ws.capturing() {
		return a.ws.handleKey(msg)
	}
	switch {
	case key.Matches(msg, keys.Quit):
		a.quitting = true
		return tea.Quit
	case key.Matches(msg, keys.Help):
		return a.push(newHelpScreen(a.ws.helpGroups()))
	}
	// A status message is feedback for the last key; the next one
	// clears it (an action that has something to say sets a new one).
	a.clearNote()
	return a.ws.handleKey(msg)
}

// handleMouse routes a mouse event: to the overlay when one is open
// (in its own content coordinates), else to the workspace. The mouse
// only ever accelerates what the keyboard can already do.
func (a *app) handleMouse(msg tea.MouseMsg) tea.Cmd {
	m := msg.Mouse()
	y := m.Y - 1 // the header line
	if s := a.top(); s != nil {
		h, ok := s.(mouser)
		if !ok {
			return nil
		}
		x := m.X - (1 + padX)
		if side := a.ws.sideWidth(); side > 0 && a.ws.mainWidth() > 0 {
			x = m.X - (side + 2 + paneGap + 1 + padX)
		}
		return h.mouse(msg, x, y-1) // the pane's top border
	}
	return a.ws.mouse(msg, y)
}

// overlayHelpGroups lists an overlay's keys and the globals.
func (a *app) overlayHelpGroups(s Screen) [][]key.Binding {
	return [][]key.Binding{s.Keys(), globalKeys(), {reservedKeys}}
}

// globalKeys close the ? overlay's listing.
func globalKeys() []key.Binding {
	return []key.Binding{keys.Menu, keys.Help, keys.Quit}
}

func (a *app) helpView() string {
	st := a.help.Styles
	st.ShortKey, st.ShortDesc, st.ShortSeparator = styleKey, styleDesc, styleDesc
	st.FullKey, st.FullDesc, st.FullSeparator = styleKey, styleDesc, styleDesc
	// The bubble's own ellipsis colour downsamples to black, which is
	// the background of most terminals that have sixteen colours.
	st.Ellipsis = styleFaint
	a.help.Styles = st
	var ks []key.Binding
	if s := a.top(); s != nil {
		ks = append(ks, s.Keys()...)
		if !key.Matches(tea.KeyPressMsg{Code: tea.KeyEscape}, ks...) {
			ks = append(ks, keys.Back) // an overlay that binds esc itself names it
		}
	} else {
		ks = a.ws.keys()
	}
	return a.help.ShortHelpView(append(ks, keys.Menu, keys.Help, keys.Quit))
}

// breadcrumb joins the overlays' titles.
func (a *app) breadcrumb() string {
	parts := make([]string, 0, len(a.stack))
	for _, s := range a.stack {
		parts = append(parts, s.Title())
	}
	return strings.Join(parts, " "+glyph.crumb+" ")
}

func (a *app) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}
	if a.ws.tooSmall() {
		return tea.NewView(strings.Join(tooSmallLines(a.width, a.height), "\n"))
	}
	header := styleHeader.Render("bffs "+a.svc.version) + "  " + styleFaint.Render(a.ws.crumb())
	if a.svc.theme != "" && a.svc.theme != "default" {
		header += "  " + styleFaint.Render("theme "+a.svc.theme)
	}
	header = cell(header, a.width)

	var main []string
	var mainTitle string
	if s := a.top(); s != nil {
		size := a.mainSize()
		main = strings.Split(fitLines(s.View(size.Width, size.Height), size.Width, size.Height), "\n")
		mainTitle = a.breadcrumb()
	}
	body := a.ws.View(a.width, a.contentHeight(), main, mainTitle, main != nil)
	body = fitLines(body, a.width, a.contentHeight())

	// Errors reach the status line from every engine; sanitised like
	// everything else that is rendered. A note leads with the glyph of
	// its kind, so a refusal does not read as a success on a terminal
	// without colour.
	text := transcripts.Sanitize(a.status)
	style := a.statusKind.style()
	mark := ""
	switch {
	case text != "":
		mark = a.statusKind.mark()
	case a.top() != nil:
		if l, ok := a.top().(loader); ok && l.loading() {
			text, style = "loading"+glyph.ellipsis, styleStatus
		}
	case a.ws.previewBusy:
		text, style = "loading"+glyph.ellipsis, styleStatus
	default:
		text, style = a.ws.hint(), styleFaint
	}
	statusLine := style.Render(truncate(mark+text, a.width))

	if a.toast.title != "" {
		bodyLines := strings.Split(body, "\n")
		body = strings.Join(placeToast(bodyLines, a.toastBox(a.width), a.width), "\n")
	}

	label, typing := a.modeLabel()
	labelStyle := styleFaint
	if typing {
		labelStyle = styleAccent.Bold(true)
	}
	footer := labelStyle.Render(label) + "  " + a.helpView()

	v := tea.NewView(strings.Join([]string{header, body, statusLine, truncate(footer, a.width)}, "\n"))
	v.AltScreen = true
	// Clicking focuses a panel and picks a row, the wheel scrolls what
	// is under the pointer; hold shift for the terminal's own text
	// selection while this is on.
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "bffs"
	return v
}
