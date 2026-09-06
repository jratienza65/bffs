package tui

import (
	"context"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// sessionRow is one transcript of the project. It starts as what the
// fast path knows (id, mtime, size, subagents, liveness, import record,
// root/lastSessionId attribution) and is completed when its windows are
// read; sel is the screen's shared selection set.
type sessionRow struct {
	s        transcripts.Session
	resolved bool
	sel      map[string]bool
	now      func() time.Time
}

func (r *sessionRow) FilterValue() string {
	return r.s.Title + " " + r.s.ID + " " + r.s.Account + " " + r.s.GitBranch
}

const sessionAgeW = 8

// shortID is the eight-character prefix the tables print.
func shortID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// sessionState is the STATE word the preview prints: live,
// imported·pending (an import whose directory does not exist here),
// imported, or nothing.
func sessionState(s transcripts.Session) string {
	switch {
	case s.Live:
		return "live"
	case s.Import != nil && !s.CwdExists:
		return "imported·pending"
	case s.Import != nil:
		return "imported"
	default:
		return ""
	}
}

// sessionGlyph is the one-cell state of a row: ● live, ↓ imported,
// ! cwd missing on this machine, nothing otherwise.
func sessionGlyph(s transcripts.Session) string {
	switch {
	case s.Live:
		return "●"
	case s.Import != nil:
		return "↓"
	case s.Cwd != "" && !s.CwdExists:
		return "!"
	}
	return " "
}

// title is the TITLE cell: the sanitised title, "-" when the windows
// held none, the id while they are still being read.
func (r *sessionRow) title() string {
	if !r.resolved {
		return "(" + shortID(r.s.ID) + ")"
	}
	return dashIfEmpty(transcripts.Sanitize(r.s.Title))
}

// render lays the row out for the side column: a mark for a selected
// row, the state glyph, the title and the age. Account, size and
// branch belong to the preview.
func (r *sessionRow) render(width int) string {
	mark := " "
	if r.sel[r.s.ID] {
		mark = "*"
	}
	prefix := mark + sessionGlyph(r.s) + " "
	titleW := width - 3 - 1 - sessionAgeW
	if titleW < 8 {
		return truncate(prefix+r.title(), width)
	}
	return prefix + pad(r.title(), titleW) + " " + pad(humanizeAgo(r.s.LastTS, r.now()), sessionAgeW)
}

// loadSessions is the fast path for one project: liveness once, then
// ReadDir + Info per transcript. No transcript is opened; titles come
// later, page by page.
func loadSessions(ctx context.Context, svc *services, root transcripts.Root, slug string) tea.Cmd {
	configDirs := svc.configDirs()
	var attr transcripts.Attributor
	if svc.attributor != nil {
		attr = svc.attributor
	}
	imp := svc.imports
	now := svc.now
	return func() tea.Msg {
		msg := sessionsPageMsg{rootDir: root.Dir, slug: slug}
		live, err := transcripts.Live(ctx, configDirs)
		if err != nil {
			live = nil // liveness is enrichment
		}
		ss, err := transcripts.List(ctx, root, transcripts.ListOptions{
			Slug:       slug,
			Live:       live,
			Imports:    imp,
			Attributor: attr,
			Now:        now(),
		})
		if err != nil {
			msg.err = err
			return msg
		}
		msg.items = ss
		return msg
	}
}

func memoryDirFor(root transcripts.Root, slug, project string) string {
	if project != "" {
		if dir, err := transcripts.MemoryDirFor(root, project); err == nil && isDir(dir) {
			return dir
		}
	}
	dir := filepath.Join(root.Dir, slug, transcripts.MemorySubdir)
	if isDir(dir) {
		return dir
	}
	return ""
}

// receiveInto opens the Receive screen for root, unless the root is an
// orphan session dir (read-only: no account would ever read the import).
func receiveInto(svc *services, root transcripts.Root) tea.Cmd {
	if root.Orphan {
		return status("orphan session dir " + transcripts.Sanitize(root.Owner) + " is read-only; receive into an account's root instead")
	}
	return pushScreen(newReceiveScreen(svc, root))
}
