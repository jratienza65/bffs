package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/textdiff"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// The drift tables say a memory file "differs (newer there)". Whether
// to sync it is a decision about its contents, so `D` shows them: a
// unified diff of this root's copy against every other root that has
// the file, rendered here rather than shelled out to diff.

// diffLoadedMsg carries one rendered comparison.
type diffLoadedMsg struct {
	key   string
	lines []string
	err   error
}

// driftScreen shows the memory drift of one file, or of every differing
// file of a project, against the other roots on this machine.
type driftScreen struct {
	svc   *services
	title string
	key   string
	load  func() ([]string, error)
	vp    viewport.Model
	drag  dragSelect
	busy  bool
	err   error
}

// newDriftScreen compares one memory file across roots. ref is the root
// the browser is looking through; name is "" to compare every file of
// the directory.
func newDriftScreen(svc *services, ref transcripts.Root, slug, cwd, name string) *driftScreen {
	label := "memory of " + shortRootLabel(ref)
	if name != "" {
		label = transcripts.Sanitize(name)
	}
	roots := svc.roots
	return &driftScreen{
		svc: svc, title: label, key: rootID(ref) + "\x00" + slug + "\x00" + name, busy: true, vp: newViewport(),
		load: func() ([]string, error) { return driftDiffLines(roots, ref, slug, cwd, name) },
	}
}

func (s *driftScreen) Init() tea.Cmd {
	k, load := s.key, s.load
	return func() tea.Msg {
		lines, err := load()
		return diffLoadedMsg{key: k, lines: lines, err: err}
	}
}

func (s *driftScreen) Title() string       { return "diff " + s.title }
func (s *driftScreen) loading() bool       { return s.busy }
func (s *driftScreen) Keys() []key.Binding { return append(viewportKeys(), keys.Done) }

func (s *driftScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(max(0, msg.Height-1))
		return s, nil
	case diffLoadedMsg:
		if msg.key != s.key {
			return s, nil
		}
		s.busy, s.err = false, msg.err
		s.vp.SetContentLines(msg.lines)
		return s, nil
	case dragScrollMsg:
		return s, s.drag.step(&s.vp, msg)
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, keys.Yank) && !s.drag.sel.empty():
			return s, s.drag.copy(&s.vp)
		case key.Matches(msg, keys.Done):
			if !s.drag.sel.empty() && msg.String() == "esc" {
				s.drag.clear()
				return s, nil
			}
			return s, popScreen()
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *driftScreen) mouse(msg tea.MouseMsg, x, y int) tea.Cmd {
	return vpSelectMouse(&s.vp, &s.drag, msg, x, y)
}

func (s *driftScreen) View(width, height int) string {
	if s.busy {
		return styleFaint.Render("comparing…")
	}
	s.vp.SetWidth(width)
	s.vp.SetHeight(max(0, height-1))
	body := paintSelection(s.vp.View(), &s.vp, s.drag.sel)
	if s.err != nil {
		body = styleError.Render(truncate(transcripts.Sanitize(s.err.Error()), width))
	}
	return body + "\n" + styleFaint.Render(truncate("esc back · ↑/↓ scroll · S syncs the memory of this project to a full-isolation account", width))
}

// driftDiffLines compares the reference root's memory against every
// other root that has the project, one section per root.
func driftDiffLines(roots []transcripts.Root, ref transcripts.Root, slug, cwd, only string) ([]string, error) {
	refDir := memoryDirFor(ref, slug, cwd)
	if refDir == "" {
		return []string{styleFaint.Render("no memory directory for this project on " + shortRootLabel(ref))}, nil
	}
	refFiles, err := memoryHashes(refDir)
	if err != nil {
		return nil, err
	}
	var lines []string
	compared := 0
	for _, other := range roots {
		if other.Dir == ref.Dir && other.ConfigDir == ref.ConfigDir {
			continue
		}
		dir := memoryDirFor(other, slug, cwd)
		if dir == "" {
			continue
		}
		otherFiles, err := memoryHashes(dir)
		if err != nil {
			lines = append(lines, styleError.Render(shortRootLabel(other)+": "+transcripts.Sanitize(err.Error())), "")
			continue
		}
		compared++
		state, diffs := compareMemory(refFiles, otherFiles)
		lines = append(lines, section(shortRootLabel(other), stateStyled(state)))
		lines = append(lines, diffSections(refDir, dir, diffs, only)...)
		lines = append(lines, "")
	}
	if compared == 0 {
		lines = append(lines, styleFaint.Render("no other root on this machine holds this project's memory"))
	}
	return lines, nil
}

// diffSections renders one root's differing files. only narrows the
// comparison to a single file when the reader opened it from a row.
func diffSections(refDir, otherDir string, diffs []fileDrift, only string) []string {
	var lines []string
	shown := 0
	for _, d := range diffs {
		if only != "" && d.name != only {
			continue
		}
		shown++
		lines = append(lines, "  "+transcripts.Sanitize(d.name)+"  "+stateStyled(d.state))
		if strings.HasPrefix(d.state, "only") {
			// One side has no copy at all: the verdict is the whole story.
			continue
		}
		here, err1 := readMemoryFile(refDir, d.name)
		there, err2 := readMemoryFile(otherDir, d.name)
		if err1 != nil || err2 != nil {
			lines = append(lines, styleFaint.Render("    (cannot read both copies)"))
			continue
		}
		hunks := textdiff.Diff(there, here)
		stat := textdiff.Count(hunks)
		lines = append(lines, styleFaint.Render(fmt.Sprintf("    %d added, %d removed (there → here)", stat.Added, stat.Removed)))
		lines = append(lines, hunkLines(hunks)...)
	}
	if shown == 0 {
		lines = append(lines, styleFaint.Render("  no differing file here"))
	}
	return lines
}

// hunkLines paints a diff: the header faint, removals in the error
// colour, additions in the success one, context muted. The line keeps
// its whole text — the viewport wraps it — because a truncated diff
// line hides exactly the character that differs.
func hunkLines(hunks []textdiff.Hunk) []string {
	var lines []string
	for _, h := range hunks {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("    @@ -%d,%d +%d,%d @@", h.AStart, h.ALen, h.BStart, h.BLen)))
		for _, op := range h.Ops {
			text := "    " + string(op.Kind) + " " + transcripts.Sanitize(op.Text)
			switch op.Kind {
			case '-':
				lines = append(lines, styleBad.Render(text))
			case '+':
				lines = append(lines, styleOK.Render(text))
			default:
				lines = append(lines, styleFaint.Render(text))
			}
		}
	}
	return lines
}

// readMemoryFile reads one file of a memory directory through an
// os.Root, so a name from another machine's tree can never lead out of
// it, and caps it the way the preview caps a file.
func readMemoryFile(dir, name string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, fileReadCap))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
