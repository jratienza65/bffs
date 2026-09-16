package tui

import (
	"fmt"
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
	title   string
	items   []menuItem
	cursor  int
	offset  int // first item drawn: the wheel moves this, not the cursor
	lastCur int
	avail   int
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

// mouse picks the item under the pointer; the wheel scrolls the list's
// text and leaves the selection alone.
func (s *menuScreen) mouse(msg tea.MouseMsg, _, y int) tea.Cmd {
	switch e := msg.(type) {
	case tea.MouseWheelMsg:
		switch e.Button {
		case tea.MouseWheelUp:
			s.offset -= wheelLines
		case tea.MouseWheelDown:
			s.offset += wheelLines
		}
		s.clamp()
	case tea.MouseClickMsg:
		if e.Button != tea.MouseLeft {
			return nil
		}
		i := s.offset + y - menuTop
		if i < 0 || i >= len(s.items) {
			return nil
		}
		s.cursor = i
		run := s.items[i].run
		return func() tea.Msg { return menuChoiceMsg{run: run} }
	}
	return nil
}

// clamp keeps the view inside the items.
func (s *menuScreen) clamp() {
	if s.offset > len(s.items)-max(1, s.avail) {
		s.offset = len(s.items) - max(1, s.avail)
	}
	if s.offset < 0 {
		s.offset = 0
	}
}

func (s *menuScreen) View(width, height int) string {
	lines := []string{styleFaint.Render(truncate(s.title, width)), ""}
	if len(s.items) == 0 {
		lines = append(lines, "nothing to do here")
	}
	// Two lines of chrome above, two below (the hint and its blank).
	s.avail = max(1, height-menuTop-2)
	if s.cursor != s.lastCur {
		s.lastCur = s.cursor
		if s.cursor < s.offset {
			s.offset = s.cursor
		}
		if s.cursor >= s.offset+s.avail {
			s.offset = s.cursor - s.avail + 1
		}
	}
	s.clamp()
	for i := s.offset; i < len(s.items) && i < s.offset+s.avail; i++ {
		line := pad("  "+pad(s.items[i].key, 4)+s.items[i].label, max(0, width-2))
		if i == s.cursor {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	hint := "enter or the key runs it · esc closes"
	if len(s.items) > s.avail {
		hint = fmt.Sprintf("items %d-%d of %d · ↑/↓ or the wheel · enter runs it · esc closes", s.offset+1, min(len(s.items), s.offset+s.avail), len(s.items))
	}
	lines = append(lines, "", styleFaint.Render(truncate(hint, width)))
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
