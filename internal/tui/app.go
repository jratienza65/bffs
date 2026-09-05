package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// app is the root model: a stack of screens, the shared services, the
// terminal size, a one-line status and the help bubble. Navigation
// happens only through messages (push/pop/replace); the screen on top
// gets every message the app does not consume itself.
type app struct {
	svc    *services
	stack  []Screen
	width  int
	height int

	status    string
	statusErr bool
	help      help.Model
	showHelp  bool
	quitting  bool
}

func newApp(svc *services) *app {
	h := help.New()
	h.ShortSeparator = " · "
	return &app{svc: svc, help: h}
}

// Init hands the app the roots the services already enumerated; the
// message path is the same one a later reload would take.
func (a *app) Init() tea.Cmd {
	svc := a.svc
	return func() tea.Msg { return rootsLoadedMsg{roots: svc.roots, warnings: svc.warnings} }
}

// top is the visible screen, nil before the first push.
func (a *app) top() Screen {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1]
}

// busy reports whether the top screen runs a long operation: navigation
// is then blocked and every key is the screen's (esc cancels, q asks
// whether to quit anyway).
func (a *app) busy() bool {
	r, ok := a.top().(running)
	return ok && r.running()
}

// contentHeight is what remains for the screen after the header, the
// status line and the help lines.
func (a *app) contentHeight() int {
	return max(0, a.height-2-lipgloss.Height(a.helpView()))
}

func (a *app) sizeMsg() tea.WindowSizeMsg {
	return tea.WindowSizeMsg{Width: a.width, Height: a.contentHeight()}
}

// forward sends msg to the top screen and stores what it returns.
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
		return tea.Batch(init, a.forward(a.sizeMsg()))
	}
	return init
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.help.SetWidth(msg.Width)
		return a, a.forward(a.sizeMsg())

	case rootsLoadedMsg:
		var cmds []tea.Cmd
		if len(msg.warnings) > 0 {
			cmds = append(cmds, status("warning: "+strings.Join(msg.warnings, "; ")))
		}
		if len(msg.roots) == 1 {
			cmds = append(cmds, a.push(newProjectsScreen(a.svc, msg.roots[0])))
		} else {
			cmds = append(cmds, a.push(newRootsScreen(a.svc, msg.roots)))
		}
		return a, tea.Batch(cmds...)

	case pushScreenMsg:
		return a, a.push(msg.screen)

	case replaceScreenMsg:
		if len(a.stack) > 0 {
			a.stack = a.stack[:len(a.stack)-1]
		}
		return a, a.push(msg.screen)

	case popScreenMsg:
		if len(a.stack) <= 1 {
			return a, nil
		}
		a.stack = a.stack[:len(a.stack)-1]
		a.status, a.statusErr = "", false
		cmd := a.forward(a.sizeMsg())
		if msg.refresh {
			// An action changed the tree: every cached catalog is stale.
			for k := range a.svc.memories {
				delete(a.svc.memories, k)
			}
			cmd = tea.Batch(cmd, a.forward(refreshMsg{}))
		}
		return a, cmd

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
	return a, a.forward(msg)
}

// handleKey is the key policy: while an operation runs every key is the
// screen's (ctrl+c and esc cancel it, q asks before quitting); otherwise
// ctrl+c always quits; a screen typing into a text field owns everything
// else; then q quits, ? toggles help, esc goes back unless the screen
// claims it (a filter to clear, a confirmation to decline), a reserved
// action key the screen does not bind answers with the hint, and the
// rest is the screen's.
func (a *app) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	if a.busy() {
		return a.forward(msg)
	}
	if msg.String() == "ctrl+c" {
		a.quitting = true
		return tea.Quit
	}
	s := a.top()
	if c, ok := s.(inputCapturer); ok && c.capturingInput() {
		return a.forward(msg)
	}
	var screenKeys []key.Binding
	if s != nil {
		screenKeys = s.Keys()
	}
	switch {
	case key.Matches(msg, keys.Quit) && !key.Matches(msg, screenKeys...):
		a.quitting = true
		return tea.Quit
	case key.Matches(msg, keys.Help) && !key.Matches(msg, screenKeys...):
		a.showHelp = !a.showHelp
		a.help.ShowAll = a.showHelp
		return a.forward(a.sizeMsg())
	case key.Matches(msg, keys.Back) && !key.Matches(msg, screenKeys...):
		if len(a.stack) <= 1 {
			return status("q quits")
		}
		return popScreen()
	case key.Matches(msg, reservedKeys) && !key.Matches(msg, screenKeys...):
		return status(reservedHint)
	}
	return a.forward(msg)
}

// globalKeys are appended to every screen's help line.
func globalKeys() []key.Binding {
	return []key.Binding{keys.Back, keys.Help, keys.Quit}
}

func (a *app) helpView() string {
	s := a.top()
	var ks []key.Binding
	if s != nil {
		ks = s.Keys()
	}
	all := append(append([]key.Binding{}, ks...), globalKeys()...)
	if a.showHelp {
		return a.help.FullHelpView([][]key.Binding{ks, globalKeys(), {reservedKeys}})
	}
	return a.help.ShortHelpView(all)
}

// breadcrumb joins the stack's titles.
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
	header := fmt.Sprintf("bffs %s", a.svc.version)
	if crumb := a.breadcrumb(); crumb != "" {
		header += "  " + crumb
	}
	header = styleHeader.Render(truncate(header, a.width))

	body := ""
	if s := a.top(); s != nil {
		body = s.View(a.width, a.contentHeight())
	} else {
		body = styleFaint.Render("loading…")
	}
	body = fitLines(body, a.width, a.contentHeight())

	// Errors reach the status line from every engine; sanitised like
	// everything else that is rendered.
	statusLine := transcripts.Sanitize(a.status)
	if l, ok := a.top().(loader); ok && l != nil && l.loading() && statusLine == "" {
		statusLine = "loading…"
	}
	if a.statusErr {
		statusLine = styleError.Render(truncate(statusLine, a.width))
	} else {
		statusLine = styleStatus.Render(truncate(statusLine, a.width))
	}

	v := tea.NewView(strings.Join([]string{header, body, statusLine, a.helpView()}, "\n"))
	v.AltScreen = true
	v.WindowTitle = "bffs " + a.svc.start
	return v
}
