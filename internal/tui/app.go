package tui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

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

	status    string
	statusErr bool
	help      help.Model
	quitting  bool
}

func newApp(svc *services) *app {
	h := help.New()
	h.ShortSeparator = " · "
	return &app{svc: svc, ws: newWorkspace(svc), help: h}
}

// Init hands the app the roots the services already enumerated; the
// message path is the same one a later reload would take.
func (a *app) Init() tea.Cmd {
	svc := a.svc
	return func() tea.Msg { return rootsLoadedMsg{roots: svc.roots, warnings: svc.warnings} }
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
	return tea.WindowSizeMsg{Width: a.ws.mainWidth(), Height: a.ws.bodyHeight()}
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
	a.status, a.statusErr = "", false
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
		a.ws.setSize(msg.Width, a.contentHeight())
		return a, a.forward(a.mainSize())

	case rootsLoadedMsg:
		var cmds []tea.Cmd
		if len(msg.warnings) > 0 {
			cmds = append(cmds, status("warning: "+strings.Join(msg.warnings, "; ")))
		}
		cmds = append(cmds, a.ws.setRoots(msg.roots))
		return a, tea.Batch(cmds...)

	case pushScreenMsg:
		return a, a.push(msg.screen)

	case replaceScreenMsg:
		if len(a.stack) > 0 {
			a.stack = a.stack[:len(a.stack)-1]
		}
		return a, a.push(msg.screen)

	case popScreenMsg:
		if len(a.stack) == 0 {
			return a, nil
		}
		a.stack = a.stack[:len(a.stack)-1]
		a.status, a.statusErr = "", false
		cmd := a.forward(a.mainSize())
		if msg.refresh {
			cmd = tea.Batch(cmd, a.ws.Update(refreshMsg{}))
		}
		return a, cmd

	case menuChoiceMsg:
		if _, ok := a.top().(*menuScreen); ok {
			a.stack = a.stack[:len(a.stack)-1]
		}
		return a, msg.run()

	case statusMsg:
		if msg.err != nil {
			a.status, a.statusErr = msg.err.Error(), true
		} else {
			a.status, a.statusErr = msg.text, false
		}
		return a, nil

	case opDoneMsg:
		if msg.err != nil {
			a.status, a.statusErr = msg.err.Error(), true
		}
		return a, a.forward(msg)

	case quitMsg:
		a.quitting = true
		return a, tea.Quit

	case tea.KeyPressMsg:
		return a, a.handleKey(msg)
	}
	// Data messages: the workspace's and the overlay's are disjoint
	// types, so both see everything else.
	return a, tea.Batch(a.ws.Update(msg), a.forward(msg))
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
		case key.Matches(msg, reservedKeys) && !key.Matches(msg, screenKeys...):
			return status(reservedHint)
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
	return a.ws.handleKey(msg)
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
	var ks []key.Binding
	if s := a.top(); s != nil {
		ks = append(ks, s.Keys()...)
		ks = append(ks, keys.Back)
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
	return strings.Join(parts, " › ")
}

func (a *app) View() tea.View {
	if a.quitting {
		return tea.NewView("")
	}
	header := styleHeader.Render("bffs "+a.svc.version) + "  " + styleFaint.Render(a.ws.crumb())
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
	// everything else that is rendered.
	statusLine := transcripts.Sanitize(a.status)
	if statusLine == "" {
		if l, ok := a.top().(loader); ok && l != nil && l.loading() {
			statusLine = "loading…"
		} else if a.top() == nil && a.ws.previewBusy {
			statusLine = "loading…"
		}
	}
	if a.statusErr {
		statusLine = styleError.Render(truncate(statusLine, a.width))
	} else {
		statusLine = styleStatus.Render(truncate(statusLine, a.width))
	}

	v := tea.NewView(strings.Join([]string{header, body, statusLine, truncate(a.helpView(), a.width)}, "\n"))
	v.AltScreen = true
	v.WindowTitle = "bffs"
	return v
}
