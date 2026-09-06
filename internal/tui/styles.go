package tui

import "charm.land/lipgloss/v2"

// Styles are deliberately minimal — bold, faint and reverse video read
// the same on every palette, and bubbletea honours NO_COLOR itself.
// Rows are laid out as plain text and styled as a whole (nested styles
// would reset each other mid-line).
var (
	styleHeader  = lipgloss.NewStyle().Bold(true)
	styleFaint   = lipgloss.NewStyle().Faint(true)
	styleCursor  = lipgloss.NewStyle().Reverse(true)
	styleMuted   = lipgloss.NewStyle().Faint(true).Reverse(true) // the selection of an unfocused panel
	styleSection = lipgloss.NewStyle().Faint(true)               // preview section headers (uppercase)
	styleError   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	styleStatus  = lipgloss.NewStyle()
)
