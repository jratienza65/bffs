package tui

import "charm.land/lipgloss/v2"

// Styles are semantic tokens set by applyTheme (theme.go): the values
// here are the monochrome fallback — bold, faint and reverse video read
// the same on every palette, and bubbletea honours NO_COLOR itself.
// Rows are laid out as plain text and styled as a whole (the cursor
// row) or by segment (renderStyled); nested styles would reset each
// other mid-line.
var (
	styleHeader  = lipgloss.NewStyle().Bold(true)
	styleFaint   = lipgloss.NewStyle().Faint(true)
	styleCursor  = lipgloss.NewStyle().Reverse(true)             // the cursor row of the focused panel
	styleMuted   = lipgloss.NewStyle().Faint(true).Reverse(true) // the selection of an unfocused panel
	styleSection = lipgloss.NewStyle().Faint(true)               // preview section headers (uppercase)
	styleError   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	styleStatus  = lipgloss.NewStyle()

	styleBorder      = lipgloss.NewStyle()
	styleBorderFocus = lipgloss.NewStyle()
	styleTitle       = lipgloss.NewStyle().Faint(true)
	styleTitleFocus  = lipgloss.NewStyle().Bold(true)
	styleCounter     = lipgloss.NewStyle().Faint(true)
	styleKey         = lipgloss.NewStyle().Bold(true)
	styleDesc        = lipgloss.NewStyle().Faint(true)

	styleLive     = lipgloss.NewStyle()
	styleImported = lipgloss.NewStyle()
	styleMissing  = lipgloss.NewStyle()
	styleMark     = lipgloss.NewStyle().Bold(true)
	stylePin      = lipgloss.NewStyle()
	styleOK       = lipgloss.NewStyle()
	styleBad      = lipgloss.NewStyle()
	styleWarn     = lipgloss.NewStyle()
	styleAccent   = lipgloss.NewStyle().Bold(true)
	styleInfo     = lipgloss.NewStyle()
	stylePrompt   = lipgloss.NewStyle().Bold(true)
)
