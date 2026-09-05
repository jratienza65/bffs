package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// Navigation messages. Screens never reach into the app or into each
// other: they emit one of these and the root switches.
type (
	pushScreenMsg    struct{ screen Screen }
	popScreenMsg     struct{}
	replaceScreenMsg struct{ screen Screen } // the sessions⇄memories tab
	statusMsg        struct {
		text string
		err  error
	}
)

// Data messages. Each carries the identity of what it was loaded for so
// a screen that was popped or replaced before the load finished ignores
// a message meant for another one.
type (
	rootsLoadedMsg struct {
		roots    []transcripts.Root
		warnings []string
	}
	projectsLoadedMsg struct {
		rootDir string
		rows    []*projectRow
		err     error
	}
	sessionsPageMsg struct {
		rootDir, slug string
		items         []transcripts.Session
		err           error
	}
	titlesResolvedMsg struct {
		slug      string
		configDir string
		metas     map[string]titleMeta     // by session id
		history   transcripts.HistoryIndex // non-nil when the command had to load it
	}
	memoriesLoadedMsg struct {
		rootDir string
		mems    []transcripts.Memory
		err     error
	}
	showLoadedMsg struct {
		id     string
		detail sessionDetail
		err    error
	}
	fileLoadedMsg struct {
		path      string
		lines     []string
		truncated bool
		err       error
	}
	scanPathsLoadedMsg struct {
		dir  string
		refs []transcripts.PathRef
		err  error
	}
	// opDoneMsg ends a long operation; the action screens send it.
	opDoneMsg struct{ err error }
)

func pushScreen(s Screen) tea.Cmd    { return func() tea.Msg { return pushScreenMsg{screen: s} } }
func popScreen() tea.Cmd             { return func() tea.Msg { return popScreenMsg{} } }
func replaceScreen(s Screen) tea.Cmd { return func() tea.Msg { return replaceScreenMsg{screen: s} } }
func status(text string) tea.Cmd     { return func() tea.Msg { return statusMsg{text: text} } }
func statusError(err error) tea.Cmd  { return func() tea.Msg { return statusMsg{err: err} } }
