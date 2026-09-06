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

const (
	sessionAccountW = 10
	sessionAgeW     = 8
	sessionSizeW    = 8
	sessionBranchW  = 12
	sessionStateW   = 16
	sessionFixedW   = 4 + 1 + sessionAccountW + 1 + sessionAgeW + 1 + sessionSizeW + 1 + sessionBranchW + 1 + sessionStateW
)

// shortID is the eight-character prefix the tables print.
func shortID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// sessionState is the STATE cell: live, imported·pending (an import
// whose directory does not exist here), imported, or nothing.
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

// title is the TITLE cell: the sanitised title, "-" when the windows
// held none, the id while they are still being read.
func (r *sessionRow) title() string {
	if !r.resolved {
		return "(" + shortID(r.s.ID) + ")"
	}
	return dashIfEmpty(transcripts.Sanitize(r.s.Title))
}

func (r *sessionRow) render(width int) string {
	mark := "[ ] "
	if r.sel[r.s.ID] {
		mark = "[x] "
	}
	age := humanizeAgo(r.s.LastTS, r.now())
	titleW := width - sessionFixedW
	if titleW < 16 {
		// Narrow terminal: title, age and state only.
		titleW = width - 4 - 1 - sessionAgeW - 1 - sessionStateW
		if titleW < 8 {
			return truncate(mark+r.title(), width)
		}
		return mark + pad(r.title(), titleW) + " " + pad(age, sessionAgeW) + " " + pad(sessionState(r.s), sessionStateW)
	}
	return mark + pad(r.title(), titleW) +
		" " + pad(dashIfEmpty(transcripts.Sanitize(r.s.Account)), sessionAccountW) +
		" " + pad(age, sessionAgeW) +
		" " + pad(formatSize(r.s.Size), sessionSizeW) +
		" " + pad(dashIfEmpty(transcripts.Sanitize(r.s.GitBranch)), sessionBranchW) +
		" " + pad(sessionState(r.s), sessionStateW)
}

// sessionsHeader is the column header laid out like the rows.
func sessionsHeader(width int) string {
	titleW := width - sessionFixedW
	if titleW < 16 {
		titleW = width - 4 - 1 - sessionAgeW - 1 - sessionStateW
		if titleW < 8 {
			return "TITLE"
		}
		return "    " + pad("TITLE", titleW) + " " + pad("LAST", sessionAgeW) + " " + pad("STATE", sessionStateW)
	}
	return "    " + pad("TITLE", titleW) + " " + pad("ACCOUNT", sessionAccountW) + " " + pad("LAST", sessionAgeW) +
		" " + pad("SIZE", sessionSizeW) + " " + pad("BRANCH", sessionBranchW) + " " + pad("STATE", sessionStateW)
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
