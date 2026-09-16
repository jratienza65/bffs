package tui

import "charm.land/bubbles/v2/key"

// keyMap is every binding the browser uses. Screens return the subset
// that applies to them from Keys(); the app adds the global ones.
type keyMap struct {
	Up, Down       key.Binding
	Open           key.Binding
	Back           key.Binding
	Tab            key.Binding
	Select         key.Binding
	SelectAll      key.Binding
	Filter         key.Binding
	ClearFilter    key.Binding
	ScanPaths      key.Binding
	Help           key.Binding
	Quit           key.Binding
	PageUp, PageDn key.Binding
}

var keys = keyMap{
	Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Open:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	Back:        key.NewBinding(key.WithKeys("esc", "backspace"), key.WithHelp("esc", "back")),
	Tab:         key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "sessions⇄memories")),
	Select:      key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "select")),
	SelectAll:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "select all")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	ClearFilter: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
	ScanPaths:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "scan paths")),
	Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	PageUp:      key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("pgup", "page up")),
	PageDn:      key.NewBinding(key.WithKeys("pgdown", "f"), key.WithHelp("pgdown", "page down")),
}

// reservedKeys are the action slots of the next version: export, send,
// receive, copy, rehome, resume, trust, scan paths and delete. A screen
// that binds one of them (p on the sessions and memories screens) gets
// it; anywhere else the key answers with reservedHint.
var reservedKeys = key.NewBinding(key.WithKeys("e", "s", "i", "c", "r", "R", "t", "p", "d"), key.WithHelp("e s i c r R t d", "actions"))

const reservedHint = "(actions arrive in a later version)"
