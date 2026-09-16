package tui

import (
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// menuItem is one entry of the x menu: the key that also triggers it,
// a label and what it does.
type menuItem struct {
	key   string
	label string
	run   func() tea.Cmd
}

// menuChoiceMsg carries the chosen item's action to the app, which pops
// the menu first so the action's own overlay lands on top.
type menuChoiceMsg struct{ run func() tea.Cmd }

// menuScreen lists the actions available for the current selection
// (lazygit's options menu).
type menuScreen struct {
	title  string
	items  []menuItem
	cursor int
}

func newMenuScreen(title string, items []menuItem) *menuScreen {
	return &menuScreen{title: title, items: items}
}

func (s *menuScreen) Init() tea.Cmd { return nil }
func (s *menuScreen) Title() string { return "menu" }
func (s *menuScreen) Keys() []key.Binding {
	letters := make([]string, 0, len(s.items))
	for _, it := range s.items {
		letters = append(letters, it.key)
	}
	ks := []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose"))}
	if len(letters) > 0 {
		ks = append(ks, key.NewBinding(key.WithKeys(letters...), key.WithHelp("letter", "run that action")))
	}
	return append(ks, keys.Cancel)
}

func (s *menuScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	m, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return s, nil
	}
	switch {
	case key.Matches(m, keys.Up):
		s.cursor = max(0, s.cursor-1)
	case key.Matches(m, keys.Down):
		s.cursor = min(len(s.items)-1, s.cursor+1)
	case key.Matches(m, keys.Open):
		if len(s.items) == 0 {
			return s, popScreen()
		}
		run := s.items[s.cursor].run
		return s, func() tea.Msg { return menuChoiceMsg{run: run} }
	case key.Matches(m, keys.Cancel), key.Matches(m, keys.Menu):
		return s, popScreen()
	default:
		for _, it := range s.items {
			if it.key == m.String() {
				run := it.run
				return s, func() tea.Msg { return menuChoiceMsg{run: run} }
			}
		}
	}
	return s, nil
}

// menuTop is the line the first item is drawn on (title, blank).
const menuTop = 2

// mouse picks the item under the pointer; the wheel moves the cursor.
func (s *menuScreen) mouse(msg tea.MouseMsg, _, y int) tea.Cmd {
	switch e := msg.(type) {
	case tea.MouseWheelMsg:
		switch e.Button {
		case tea.MouseWheelUp:
			s.cursor = max(0, s.cursor-1)
		case tea.MouseWheelDown:
			s.cursor = min(len(s.items)-1, s.cursor+1)
		}
	case tea.MouseClickMsg:
		if e.Button != tea.MouseLeft {
			return nil
		}
		i := y - menuTop
		if i < 0 || i >= len(s.items) {
			return nil
		}
		s.cursor = i
		run := s.items[i].run
		return func() tea.Msg { return menuChoiceMsg{run: run} }
	}
	return nil
}

func (s *menuScreen) View(width, height int) string {
	lines := []string{styleFaint.Render(truncate(s.title, width)), ""}
	if len(s.items) == 0 {
		lines = append(lines, "nothing to do here")
	}
	for i, it := range s.items {
		line := pad("  "+pad(it.key, 4)+it.label, max(0, width-2))
		if i == s.cursor {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", styleFaint.Render("enter or the key runs it · esc closes"))
	return strings.Join(lines, "\n")
}

// helpScreen renders every binding, grouped, in a scrollable overlay.
type helpScreen struct {
	vp     viewport.Model
	groups [][]key.Binding
	help   help.Model
}

func newHelpScreen(groups [][]key.Binding) *helpScreen {
	h := help.New()
	return &helpScreen{vp: newViewport(), groups: groups, help: h}
}

func (s *helpScreen) Init() tea.Cmd       { return nil }
func (s *helpScreen) Title() string       { return "keys" }
func (s *helpScreen) Keys() []key.Binding { return append(viewportKeys(), keys.Done) }

func (s *helpScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(msg.Height)
		s.help.SetWidth(msg.Width)
		s.vp.SetContentLines(strings.Split(s.help.FullHelpView(s.groups), "\n"))
		return s, nil
	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Done) || key.Matches(msg, keys.Help) {
			return s, popScreen()
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *helpScreen) mouse(msg tea.MouseMsg, _, _ int) tea.Cmd { return vpMouse(&s.vp, msg) }

func (s *helpScreen) View(width, height int) string { return s.vp.View() }
