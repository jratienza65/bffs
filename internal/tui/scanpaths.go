package tui

import (
	"fmt"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// scanPathsScreen lists every absolute path and @-reference inside a
// memory directory — `bffs memory scan-paths` in a viewport.
type scanPathsScreen struct {
	svc  *services
	dir  string
	vp   viewport.Model
	busy bool
	refs int
}

func newScanPathsScreen(svc *services, dir string) *scanPathsScreen {
	return &scanPathsScreen{svc: svc, dir: dir, vp: newViewport(), busy: true}
}

func loadScanPaths(dir string) tea.Cmd {
	return func() tea.Msg {
		refs, err := transcripts.ScanAbsolutePaths(dir)
		return scanPathsLoadedMsg{dir: dir, refs: refs, err: err}
	}
}

func (s *scanPathsScreen) Init() tea.Cmd       { return loadScanPaths(s.dir) }
func (s *scanPathsScreen) Title() string       { return "scan paths" }
func (s *scanPathsScreen) loading() bool       { return s.busy }
func (s *scanPathsScreen) Keys() []key.Binding { return viewportKeys() }

// scanPathLines renders one `file:line: path` per reference, @-references
// prefixed "@ref ", every field sanitised.
func scanPathLines(refs []transcripts.PathRef) []string {
	lines := make([]string, 0, len(refs))
	for _, r := range refs {
		prefix := ""
		if r.Kind == transcripts.PathKindAt {
			prefix = "@ref "
		}
		lines = append(lines, fmt.Sprintf("%s%s:%d: %s", prefix, transcripts.Sanitize(r.File), r.Line, transcripts.Sanitize(r.Path)))
	}
	return lines
}

func (s *scanPathsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(max(0, msg.Height-1))
		return s, nil
	case scanPathsLoadedMsg:
		if msg.dir != s.dir {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			return s, statusError(msg.err)
		}
		s.refs = len(msg.refs)
		lines := scanPathLines(msg.refs)
		if len(lines) == 0 {
			lines = []string{"no absolute paths or @-references"}
		}
		s.vp.SetContentLines(lines)
		return s, nil
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *scanPathsScreen) View(width, height int) string {
	head := fmt.Sprintf("%s in %s", countNoun(s.refs, "reference"), shortPath(s.dir))
	if s.busy {
		head = "scanning…"
	}
	return styleFaint.Render(truncate(head, width)) + "\n" + s.vp.View()
}
