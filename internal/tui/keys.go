package tui

import "charm.land/bubbles/v2/key"

// keyMap is every binding the browser uses. The workspace returns the
// 3–5 that matter for the focused panel from keys(); overlays return
// theirs from Keys(); the app adds the global ones.
type keyMap struct {
	Up, Down       key.Binding
	Open           key.Binding
	Back           key.Binding
	Select         key.Binding
	SelectAll      key.Binding
	Activate       key.Binding // space on the accounts panel
	Filter         key.Binding
	ClearFilter    key.Binding
	ScanPaths      key.Binding
	Help           key.Binding
	Quit           key.Binding
	PageUp, PageDn key.Binding

	// Workspace navigation, lazygit's conventions: panels by number or
	// by cycling, the tabs of the items panel, screen modes, the menu.
	NextPanel, PrevPanel       key.Binding
	Panel1, Panel2, Panel3     key.Binding
	NextTab, PrevTab           key.Binding
	ScreenMode, ScreenModePrev key.Binding
	Menu                       key.Binding
	Theme                      key.Binding
	Wizard                     key.Binding

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
	Select:      key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "mark")),
	SelectAll:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "mark all")),
	Activate:    key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "switch to account")),
	Filter:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	ClearFilter: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
	ScanPaths:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "scan paths")),
	Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keys")),
	Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	PageUp:      key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("pgup", "page up")),
	PageDn:      key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("pgdown", "page down")),

	NextPanel:      key.NewBinding(key.WithKeys("tab", "l", "right"), key.WithHelp("tab", "next panel")),
	PrevPanel:      key.NewBinding(key.WithKeys("shift+tab", "h", "left"), key.WithHelp("shift+tab", "prev panel")),
	Panel1:         key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "accounts")),
	Panel2:         key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "projects")),
	Panel3:         key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "sessions/memory")),
	NextTab:        key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "memory tab")),
	PrevTab:        key.NewBinding(key.WithKeys("["), key.WithHelp("[", "sessions tab")),
	ScreenMode:     key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "bigger preview")),
	ScreenModePrev: key.NewBinding(key.WithKeys("_", "-"), key.WithHelp("_", "smaller preview")),
	Menu:           key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "more")),
	Theme:          key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "theme")),
	Wizard:         key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "transfer wizard")),

	Export:     key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export")),
	Send:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "send")),
	Receive:    key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "receive")),
	Copy:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
	Rehome:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rehome")),
	Resume:     key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "resume")),
	Trust:      key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "trust")),
	SyncMemory: key.NewBinding(key.WithKeys("S"), key.WithHelp("S", "sync memory")),
	Pointer:    key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "last session")),

	Yes:    key.NewBinding(key.WithKeys("y", "Y", "enter"), key.WithHelp("y", "yes")),
	No:     key.NewBinding(key.WithKeys("n", "N", "esc"), key.WithHelp("n", "no")),
	Cancel: key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel")),
	Done:   key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "done")),
}

// reservedKeys are the action slots an overlay does not bind: d (delete
// — deliberately absent from the browser, plan Q7) everywhere, and the
// other action letters where the action has no subject. Anywhere they
// are not bound the key answers with reservedHint.
var reservedKeys = key.NewBinding(key.WithKeys("e", "s", "i", "c", "r", "R", "t", "S", "L", "p", "w", "d"), key.WithHelp("d", "no delete here (bffs sessions rm)"))

const reservedHint = "(not available here; d never deletes — use bffs sessions rm)"
