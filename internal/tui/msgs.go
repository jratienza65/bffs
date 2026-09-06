package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

// Navigation messages. Screens never reach into the app or into each
// other: they emit one of these and the root switches.
type (
	pushScreenMsg struct{ screen Screen }
	// popScreenMsg removes the top screen; refresh tells the screen
	// underneath to reload (an action changed what it lists).
	popScreenMsg     struct{ refresh bool }
	replaceScreenMsg struct{ screen Screen } // the sessions⇄memories tab
	statusMsg        struct {
		text string
		err  error
	}
	// refreshMsg asks a list screen to reload its data.
	refreshMsg struct{}
	// quitMsg ends the program from a screen (the confirmed quit while an
	// operation runs).
	quitMsg struct{}
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
	// previewLoadedMsg carries the main pane's lines for one selection;
	// key names the selection, gen the reload generation it was built
	// for (a stale one is dropped).
	previewLoadedMsg struct {
		key   string
		gen   int
		lines []string
		err   error
	}
	// transcriptLoadedMsg carries the full viewer's rendering.
	transcriptLoadedMsg struct {
		path      string
		lines     []string
		records   int
		hidden    int
		truncated bool
		err       error
	}
	// pointerDoneMsg ends a last-session pointer write.
	pointerDoneMsg struct {
		account string
		err     error
	}
	// switchedMsg ends an active-account switch (state.toml written).
	switchedMsg struct {
		account string
		err     error
	}
	// themeSavedMsg ends a theme change (state.toml written).
	themeSavedMsg struct {
		name string
		err  error
	}
)

// Operation messages (plan §11 "Long ops"): an action screen's goroutine
// sends progress and transfer events through its op channel and one
// final message of its own; the app learns the outcome from opDoneMsg.
type (
	// progressMsg is one bundle.Progress from a hashing pre-pass, a
	// build or an unpack.
	progressMsg struct{ p bundle.Progress }
	// transferEventMsg is one transfer.Event of a serve or a fetch. Its
	// text never carries the pairing code (transfer guarantees it).
	transferEventMsg struct{ ev transfer.Event }
	// tickMsg drives a countdown; the screen re-arms it while it runs.
	tickMsg struct{ id int }
	// opDoneMsg lets a screen hand the app the outcome of an operation
	// for the status line; the action screens end on a result screen
	// instead, so it is a hook, not a path any of them takes today.
	opDoneMsg struct{ err error }
)

func pushScreen(s Screen) tea.Cmd    { return func() tea.Msg { return pushScreenMsg{screen: s} } }
func popScreen() tea.Cmd             { return func() tea.Msg { return popScreenMsg{} } }
func popRefresh() tea.Cmd            { return func() tea.Msg { return popScreenMsg{refresh: true} } }
func replaceScreen(s Screen) tea.Cmd { return func() tea.Msg { return replaceScreenMsg{screen: s} } }
func status(text string) tea.Cmd     { return func() tea.Msg { return statusMsg{text: text} } }
func statusError(err error) tea.Cmd  { return func() tea.Msg { return statusMsg{err: err} } }
func quit() tea.Cmd                  { return func() tea.Msg { return quitMsg{} } }
