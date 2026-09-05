package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
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

// memoriesScreen is the memories tab of one project: the files of the
// memory directory Claude would load for it.
type memoriesScreen struct {
	svc     *services
	root    transcripts.Root
	slug    string
	project string
	list    list.Model
	mem     *transcripts.Memory
	dir     string // the directory shown, or the one that would be used
	busy    bool
	width   int
	height  int
}

func newMemoriesScreen(svc *services, root transcripts.Root, slug, project string) *memoriesScreen {
	s := &memoriesScreen{svc: svc, root: root, slug: slug, project: project, list: newList(nil, "file", "files"), busy: true}
	s.list.Title = memoriesHeader(0)
	return s
}

func (s *memoriesScreen) Init() tea.Cmd {
	if mems, ok := s.svc.memories[s.root.Dir]; ok {
		return func() tea.Msg { return memoriesLoadedMsg{rootDir: s.root.Dir, mems: mems} }
	}
	return loadMemories(s.svc.ctx, s.root)
}

func (s *memoriesScreen) Title() string {
	return s.projectLabel() + " › memories"
}

func (s *memoriesScreen) projectLabel() string {
	if s.project != "" {
		return transcripts.Sanitize(shortPath(s.project))
	}
	return transcripts.Sanitize(s.slug)
}

func (s *memoriesScreen) loading() bool        { return s.busy }
func (s *memoriesScreen) capturingInput() bool { return s.list.SettingFilter() }

func (s *memoriesScreen) Keys() []key.Binding {
	ks := []key.Binding{keys.Up, keys.Down, keys.Open, keys.Tab, keys.ScanPaths, keys.Filter}
	ks = append(ks, actionKeys(false)...)
	return append(ks, filterKeys(s.list)...)
}

// target is the whole project: its sessions and its memory.
func (s *memoriesScreen) target() actionTarget {
	return actionTarget{root: s.root, slug: s.slug, project: s.project}
}

// pick chooses the memory directory of the project among the root's:
// the one Claude would use for the decoded cwd (keyed by the git root),
// else the slug directory's own.
func (s *memoriesScreen) pick(mems []transcripts.Memory) {
	want := memoryDirFor(s.root, s.slug, s.project)
	if want == "" {
		want = filepath.Join(s.root.Dir, s.slug, transcripts.MemorySubdir)
		if s.project != "" {
			if dir, err := transcripts.MemoryDirFor(s.root, s.project); err == nil {
				want = dir
			}
		}
	}
	s.dir = want
	for i := range mems {
		if filepath.Clean(mems[i].Dir) == filepath.Clean(want) {
			s.mem = &mems[i]
			return
		}
	}
}

func (s *memoriesScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width, s.height = msg.Width, msg.Height
		s.list.SetSize(msg.Width, msg.Height-1)
		s.list.Title = memoriesHeader(msg.Width - 2)
		return s, nil

	case refreshMsg:
		s.busy = true
		s.mem = nil
		delete(s.svc.memories, s.root.Dir)
		return s, loadMemories(s.svc.ctx, s.root)

	case memoriesLoadedMsg:
		if msg.rootDir != s.root.Dir {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			return s, statusError(msg.err)
		}
		s.svc.memories[s.root.Dir] = msg.mems
		s.pick(msg.mems)
		if s.mem == nil {
			return s, nil
		}
		items := make([]list.Item, 0, len(s.mem.Files))
		for _, f := range s.mem.Files {
			items = append(items, &memoryFileRow{f: f, dir: s.mem.Dir, svc: s.svc})
		}
		return s, s.list.SetItems(items)

	case tea.KeyPressMsg:
		if s.list.SettingFilter() {
			break
		}
		switch {
		case key.Matches(msg, keys.Open):
			if r, ok := s.list.SelectedItem().(*memoryFileRow); ok {
				return s, pushScreen(newFileScreen(r.svc, filepath.Join(r.dir, filepath.FromSlash(r.f.Name)), r.f.Name))
			}
			return s, nil
		case key.Matches(msg, keys.Tab):
			return s, replaceScreen(newSessionsScreen(s.svc, s.root, s.slug, s.project))
		case key.Matches(msg, keys.ScanPaths):
			if s.mem == nil {
				return s, status("no memory dir for " + s.projectLabel())
			}
			return s, pushScreen(newScanPathsScreen(s.svc, s.mem.Dir))
		case key.Matches(msg, keys.Export):
			return s, pushScreen(newExportScreen(s.svc, s.target()))
		case key.Matches(msg, keys.Send):
			sc, err := newServeScreen(s.svc, s.target())
			if err != nil {
				return s, statusError(err)
			}
			return s, pushScreen(sc)
		case key.Matches(msg, keys.Receive):
			return s, receiveInto(s.svc, s.root)
		case key.Matches(msg, keys.Copy):
			return s, pushScreen(newCopyScreen(s.svc, s.target()))
		case key.Matches(msg, keys.Trust):
			if s.project == "" {
				return s, status("no cwd recorded for this project; nothing to trust")
			}
			return s, pushScreen(newTrustScreen(s.svc, s.project))
		}
	}
	var cmd tea.Cmd
	s.list, cmd = s.list.Update(msg)
	return s, cmd
}

func (s *memoriesScreen) View(width, height int) string {
	switch {
	case s.busy:
		return styleFaint.Render(truncate("memory  loading…", width))
	case s.mem == nil:
		return styleFaint.Render(truncate(fmt.Sprintf("no memory dir for %s  (would be %s)", s.projectLabel(), shortPath(s.dir)), width))
	}
	head := fmt.Sprintf("memory %s  (%s)", shortPath(s.mem.Dir), memoryVisibility(s.root))
	return styleFaint.Render(truncate(head, width)) + "\n" + s.list.View()
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
