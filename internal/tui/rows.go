package tui

import (
	"fmt"
	"io"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// row is a list item that lays itself out as one plain-text line for a
// given width. Styling is the delegate's: the cursor row is rendered as
// a whole with the selection style; other rows may colour their
// segments through styledRow.
type row interface {
	list.Item
	render(width int) string
}

// styledRow is a row that can render itself with per-segment colours
// (a state glyph, a muted age); the plain render stays the layout.
type styledRow interface {
	renderStyled(width int) string
}

// rowDelegate renders one-line rows with a two-cell cursor gutter.
type rowDelegate struct{}

func (rowDelegate) Height() int                             { return 1 }
func (rowDelegate) Spacing() int                            { return 0 }
func (rowDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

// Render satisfies list.ItemDelegate.
func (rowDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	r, ok := item.(row)
	if !ok {
		return
	}
	width := m.Width()
	if index == m.Index() {
		fmt.Fprint(w, styleCursor.Render("> "+pad(r.render(width-2), width-2)))
		return
	}
	if sr, ok := item.(styledRow); ok {
		fmt.Fprint(w, "  "+cell(sr.renderStyled(width-2), width-2))
		return
	}
	fmt.Fprint(w, "  "+pad(r.render(width-2), width-2))
}

// newList builds a list with the browser's conventions: no built-in
// help or quit keys (the app owns both), a one-line title bar that
// doubles as the filter input while typing, no padding anywhere.
func newList(items []list.Item, singular, plural string) list.Model {
	l := list.New(items, rowDelegate{}, 0, 0)
	l.SetShowTitle(true)
	l.SetShowHelp(false)
	l.SetShowStatusBar(true)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)
	l.SetShowFilter(true)
	l.SetStatusBarItemName(singular, plural)
	l.DisableQuitKeybindings()
	l.KeyMap = listKeyMap()
	l.FilterInput.Prompt = "/ "
	// A static cursor: a blinking one schedules a tick per blink, which
	// a model test would have to run.
	st := l.FilterInput.Styles()
	st.Cursor.Blink = false
	l.FilterInput.SetStyles(st)
	l.Styles.TitleBar = lipgloss.NewStyle()
	l.Styles.Title = styleHeader
	l.Styles.StatusBar = styleFaint
	l.Styles.StatusEmpty = styleFaint
	l.Styles.StatusBarFilterCount = styleFaint
	l.Styles.PaginationStyle = styleFaint
	l.Styles.NoItems = styleFaint
	return l
}

// listKeyMap keeps the list's browsing keys off the letters the action
// screens will use (its defaults bind d, u, f, b and v).
func listKeyMap() list.KeyMap {
	return list.KeyMap{
		CursorUp:             keys.Up,
		CursorDown:           keys.Down,
		PrevPage:             key.NewBinding(key.WithKeys("left", "h", "pgup"), key.WithHelp("←/pgup", "prev page")),
		NextPage:             key.NewBinding(key.WithKeys("right", "l", "pgdown"), key.WithHelp("→/pgdown", "next page")),
		GoToStart:            key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g/home", "first")),
		GoToEnd:              key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G/end", "last")),
		Filter:               keys.Filter,
		ClearFilter:          keys.ClearFilter,
		CancelWhileFiltering: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		AcceptWhileFiltering: key.NewBinding(key.WithKeys("enter", "tab", "up", "down", "ctrl+j", "ctrl+k"), key.WithHelp("enter", "apply")),
		ShowFullHelp:         key.NewBinding(key.WithDisabled()),
		CloseFullHelp:        key.NewBinding(key.WithDisabled()),
		Quit:                 key.NewBinding(key.WithDisabled()),
		ForceQuit:            key.NewBinding(key.WithDisabled()),
	}
}

// filterKeys is the help entry a list screen adds while a filter is
// applied: esc then clears it instead of going back.
func filterKeys(l list.Model) []key.Binding {
	if l.FilterState() == list.FilterApplied {
		return []key.Binding{keys.ClearFilter}
	}
	return nil
}
