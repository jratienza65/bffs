package tui

import "charm.land/bubbles/v2/key"

// keyMap is every binding the browser uses. The workspace returns the
// subset that applies to the focused panel from keys(); overlays return
// theirs from Keys(); the app adds the global ones.
type keyMap struct {
	Up, Down       key.Binding
	Open           key.Binding
	Back           key.Binding
	Select         key.Binding
	SelectAll      key.Binding
	Filter         key.Binding
	ClearFilter    key.Binding
	ScanPaths      key.Binding
	Help           key.Binding
	Quit           key.Binding
	PageUp, PageDn key.Binding

	// Workspace navigation, lazygit's conventions: panels by number or
	// by cycling, the tabs of the items panel, screen modes, the menu.
	NextPanel, PrevPanel           key.Binding
	Panel1, Panel2, Panel3, Panel4 key.Binding
	NextTab, PrevTab               key.Binding
	ScreenMode, ScreenModePrev     key.Binding
	Menu                           key.Binding

	// The action keys (plan §11, tui-v2 §1): each opens an overlay over
	// the main pane; i (receive) works from every panel.
	Export     key.Binding
	Send       key.Binding
	Receive    key.Binding
	Copy       key.Binding
	Rehome     key.Binding
	Resume     key.Binding
	Trust      key.Binding
	SyncMemory key.Binding
	Pointer    key.Binding

	// Inside an overlay: the [y/N] answer, cancelling a running
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
	Select:      key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "select")),
	SelectAll:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "select all")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	ClearFilter: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
	ScanPaths:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "scan paths")),
	Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keys")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	PageUp:      key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "page up")),
	PageDn:      key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdown", "page down")),

	NextPanel:      key.NewBinding(key.WithKeys("tab", "l", "right"), key.WithHelp("tab/l", "next panel")),
	PrevPanel:      key.NewBinding(key.WithKeys("shift+tab", "h", "left"), key.WithHelp("shift+tab/h", "prev panel")),
	Panel1:         key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "roots")),
	Panel2:         key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "projects")),
	Panel3:         key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "sessions/memory")),
	Panel4:         key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "files")),
	NextTab:        key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next tab")),
	PrevTab:        key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev tab")),
	ScreenMode:     key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "screen mode")),
	ScreenModePrev: key.NewBinding(key.WithKeys("_", "-"), key.WithHelp("_", "screen mode back")),
	Menu:           key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "menu")),

	Export:     key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export to file")),
	Send:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send over LAN")),
	Receive:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "receive")),
	Copy:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy to account")),
	Rehome:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rehome")),
	Resume:     key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "resume in claude")),
	Trust:      key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "trust")),
	SyncMemory: key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "sync memory to account")),
	Pointer:    key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "set last-session pointer")),

	Yes:    key.NewBinding(key.WithKeys("y", "Y", "enter"), key.WithHelp("y", "yes")),
	No:     key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n", "no")),
	Cancel: key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel")),
	Done:   key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "done")),
}

// navKeys are the workspace bindings every panel shares.
func navKeys() []key.Binding {
	return []key.Binding{keys.Up, keys.Down, keys.NextPanel, keys.PrevPanel, keys.Filter, keys.Menu}
}

// reservedKeys are the action slots an overlay does not bind: d (delete
// — deliberately absent from the browser, plan Q7) everywhere, and the
// other action letters where the action has no subject. Anywhere they
// are not bound the key answers with reservedHint.
var reservedKeys = key.NewBinding(key.WithKeys("e", "s", "i", "c", "r", "R", "t", "S", "L", "p", "d"), key.WithHelp("d", "no delete here (bffs sessions rm)"))

const reservedHint = "(not available here; d never deletes — use bffs sessions rm)"
