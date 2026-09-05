package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Screen is one level of the stack. Update returns the screen to keep
// (usually itself) and navigates only by emitting messages; View draws
// into the content area the app hands it, never the chrome; Title is the
// breadcrumb; Keys lists the bindings the help line shows, in order.
type Screen interface {
	Init() tea.Cmd
	Update(tea.Msg) (Screen, tea.Cmd)
	View(width, height int) string
	Title() string
	Keys() []key.Binding
}

// inputCapturer is implemented by a screen that is typing into a text
// field (a list filter): every key but ctrl+c is then its own.
type inputCapturer interface {
	capturingInput() bool
}

// loader is implemented by a screen that has data outstanding; the app
// shows it in the status line.
type loader interface {
	loading() bool
}
