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

	// The action keys (plan §11): each pushes an action screen from the
	// sessions or memories screen; i (receive) works from every screen.
	Export  key.Binding
	Send    key.Binding
	Receive key.Binding
	Copy    key.Binding
	Rehome  key.Binding
	Resume  key.Binding
	Trust   key.Binding

	// Inside an action screen: the [y/N] answer, cancelling a running
	// operation, closing a result.
	Yes    key.Binding
	No     key.Binding
	Cancel key.Binding
	Done   key.Binding
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

	Export:  key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export to file")),
	Send:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send over LAN")),
	Receive: key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "receive")),
	Copy:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy to account")),
	Rehome:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rehome")),
	Resume:  key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "resume in claude")),
	Trust:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "trust")),

	Yes:    key.NewBinding(key.WithKeys("y", "Y", "enter"), key.WithHelp("y", "yes")),
	No:     key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n", "no")),
	Cancel: key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel")),
	Done:   key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "done")),
}

// actionKeys are the bindings a sessions screen offers; the memories
// screen leaves out rehome and resume, which are about one transcript.
func actionKeys(sessions bool) []key.Binding {
	ks := []key.Binding{keys.Export, keys.Send, keys.Receive, keys.Copy}
	if sessions {
		ks = append(ks, keys.Rehome, keys.Resume)
	}
	return append(ks, keys.Trust)
}

// reservedKeys are the action slots a screen does not bind: d (delete —
// deliberately absent from the browser, plan Q7) everywhere, and the
// other action letters on screens where the action has no subject (a
// project's memory cannot be resumed). Anywhere they are not bound the
// key answers with reservedHint.
var reservedKeys = key.NewBinding(key.WithKeys("e", "s", "i", "c", "r", "R", "t", "p", "d"), key.WithHelp("d", "no delete here (bffs sessions rm)"))

const reservedHint = "(not available on this screen; d never deletes — use bffs sessions rm)"
