package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// memoryFileRow is the list item for one memory file.
type memoryFileRow struct {
	f   transcripts.MemoryFile
	dir string
	svc *services
}

func (r *memoryFileRow) FilterValue() string { return r.f.Name }

const (
	memorySizeW   = 8
	memoryAgeW    = 9
	memoryPinnedW = 6
	memoryPathsW  = 7
	memoryRefsW   = 7
	memoryFixedW  = 2 + 1 + memorySizeW + 1 + memoryAgeW + 1 + memoryPinnedW + 1 + memoryPathsW + 1 + memoryRefsW
)

func (r *memoryFileRow) render(width int) string {
	nameW := width - memoryFixedW
	name := transcripts.Sanitize(r.f.Name)
	if nameW < 12 {
		return truncate(name, width)
	}
	paths := ""
	if n := len(r.f.AbsolutePaths); n > 0 {
		paths = fmt.Sprintf("paths %d", n)
	}
	refs := ""
	if n := len(r.f.AtRefs); n > 0 {
		refs = fmt.Sprintf("@refs %d", n)
	}
	pinned := ""
	if r.f.Pinned {
		pinned = "pinned"
	}
	return "  " + pad(name, nameW) + " " + pad(formatSize(r.f.Size), memorySizeW) + " " + pad(humanizeAgo(r.f.ModTime, r.svc.now()), memoryAgeW) +
		" " + pad(pinned, memoryPinnedW) + " " + pad(paths, memoryPathsW) + " " + pad(refs, memoryRefsW)
}

func memoriesHeader(width int) string {
	nameW := width - memoryFixedW
	if nameW < 12 {
		return "NAME"
	}
	return "  " + pad("NAME", nameW) + " " + pad("SIZE", memorySizeW) + " " + pad("MODIFIED", memoryAgeW) +
		" " + pad("PINNED", memoryPinnedW) + " " + pad("PATHS", memoryPathsW) + " " + pad("@REFS", memoryRefsW)
}

// loadMemories catalogs every memory directory of root once; the app
// caches the result per root so the tab switch back is free.
func loadMemories(ctx context.Context, root transcripts.Root) tea.Cmd {
	return func() tea.Msg {
		mems, err := transcripts.Memories(ctx, []transcripts.Root{root})
		return memoriesLoadedMsg{rootDir: root.Dir, mems: mems, err: err}
	}
}

// memoryVisibility says which accounts read a memory directory.
func memoryVisibility(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan session dir %s — no account reads it", transcripts.Sanitize(r.Owner))
	case r.Owner != "":
		return "account: " + transcripts.Sanitize(r.Owner)
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool — visible to: " + accountList(r.Accounts)
	default:
		return "home — unmanaged " + shortPath(r.ConfigDir)
	}
}

// fileScreen shows one memory file read-only, in a viewport. The file
// is read once, bounded, and every line is sanitised.
type fileScreen struct {
	svc       *services
	path      string
	name      string
	vp        viewport.Model
	busy      bool
	truncated bool
	err       error
	lines     int
}

// fileReadCap bounds what the viewer reads — far above Claude's own
// 25 000-character memory load limit.
const fileReadCap = 1 << 20

func newFileScreen(svc *services, path, name string) *fileScreen {
	return &fileScreen{svc: svc, path: path, name: name, vp: newViewport(), busy: true}
}

// loadFile reads the first fileReadCap bytes of path, line by line.
func loadFile(path string) tea.Cmd {
	return func() tea.Msg {
		msg := fileLoadedMsg{path: path}
		f, err := os.Open(path)
		if err != nil {
			msg.err = err
			return msg
		}
		defer f.Close()
		r := bufio.NewReaderSize(io.LimitReader(f, fileReadCap+1), 256*1024)
		var total int64
		for {
			line, err := r.ReadString('\n')
			total += int64(len(line))
			if total > fileReadCap {
				msg.truncated = true
				break
			}
			if line != "" {
				msg.lines = append(msg.lines, transcripts.Sanitize(strings.TrimRight(line, "\r\n")))
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					msg.err = err
				}
				break
			}
		}
		return msg
	}
}

func (s *fileScreen) Init() tea.Cmd       { return loadFile(s.path) }
func (s *fileScreen) Title() string       { return transcripts.Sanitize(s.name) }
func (s *fileScreen) loading() bool       { return s.busy }
func (s *fileScreen) Keys() []key.Binding { return viewportKeys() }

func (s *fileScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(max(0, msg.Height-1))
		return s, nil
	case fileLoadedMsg:
		if msg.path != s.path {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			s.err = msg.err
			return s, statusError(msg.err)
		}
		s.lines = len(msg.lines)
		s.truncated = msg.truncated
		s.vp.SetContentLines(msg.lines)
		return s, nil
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *fileScreen) View(width, height int) string {
	head := countNoun(s.lines, "line")
	if s.truncated {
		head += " (first 1 MB)"
	}
	head += "  " + transcripts.Sanitize(shortPath(s.path))
	if s.busy {
		head = "loading…"
	}
	return styleFaint.Render(truncate(head, width)) + "\n" + s.vp.View()
}

// newViewport is a read-only viewport with the browser's keys: the
// defaults bind d, u, f and b, which the action screens will use.
func newViewport() viewport.Model {
	vp := viewport.New()
	vp.SoftWrap = true
	vp.KeyMap = viewport.KeyMap{
		Up:           keys.Up,
		Down:         keys.Down,
		PageUp:       keys.PageUp,
		PageDown:     keys.PageDn,
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		Left:         key.NewBinding(key.WithKeys("left", "h")),
		Right:        key.NewBinding(key.WithKeys("right", "l")),
	}
	return vp
}

func viewportKeys() []key.Binding {
	return []key.Binding{keys.Up, keys.Down, keys.PageUp, keys.PageDn}
}
