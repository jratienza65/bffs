// Package porter moves Claude Code sessions and auto-memory in and out of
// a projects/ pool: Select picks what to export, BuildManifest/Export turn
// the selection into a .bffs bundle (internal/bundle), and Import lands a
// bundle in a destination root through internal/rehome's transactional
// commit. It is the layer every surface shares — `bffs export`, `bffs
// import`, the LAN transfer (M5), `bffs copy` (M7) and the MCP tools.
//
// The rules the package enforces are those of the plan's §6 (what a
// bundle carries and never carries), §9 (placement, collisions, liveness,
// mtimes, memory, records) and §12 (tests). Placement knows three modes:
// identity (the entry's cwd exists here as a directory — no relocated
// record), mapped (a --map prefix rule, --into or an interactive answer
// through the Placer moved the entry to a directory here; the transcript
// is stamped with a relocated record and memory is merged as confirmed),
// and as-is (everything else, landing under the original slug and flagged
// pending for `bffs rehome`). Trust carry and lastSessionId are written
// after the sessions have landed.
//
// Every string taken from a manifest, a transcript or a peer that reaches
// a warning or report passes transcripts.Sanitize before it is stored.
//
// Dependency rule: porter imports bundle, rehome, transcripts, imports,
// store, sessions, resolver, trust, fsutil and claudejson — never cmd,
// usage, mcpserver or transfer.
package porter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Parts says which per-session artifacts an export carries besides the
// transcript. Sidecar is the projects/<slug>/<sid>/ tree; ToolResults
// narrows it (tool-results/ holds saved tool outputs, which may contain
// pasted secrets); Tasks is opt-in because task lists are rarely wanted.
type Parts struct {
	Sidecar, ToolResults, FileHistory, Plans, History, Tasks bool
}

// DefaultParts is what `bffs export` includes without flags (plan §6.4).
var DefaultParts = Parts{Sidecar: true, ToolResults: true, FileHistory: true, Plans: true, History: true}

// Selection is what an export carries: sessions and memory directories of
// one root, with the parts to include.
type Selection struct {
	Root     transcripts.Root
	Sessions []transcripts.Session
	Memories []transcripts.Memory
	Parts    Parts
}

// Selector values for SelectOptions.Only.
const (
	OnlySessions = "sessions"
	OnlyMemories = "memories"
)

// SelectOptions narrows Select. Projects are directories in any form
// store.NormalizePath accepts; Sessions are session ids or prefixes of at
// least eight hex digits; AllProjects takes every non-reserved slug of the
// root. Only is "", "sessions" or "memories". Since keeps only sessions
// whose transcript was modified within that duration (0 = any age).
// IncludeLive keeps sessions a running claude owns (they export read-only,
// possibly truncated). Now is the clock Since and the live-sibling rule
// are measured against; zero means time.Now().
type SelectOptions struct {
	AllProjects bool
	Projects    []string
	Sessions    []string
	Only        string
	Since       time.Duration
	IncludeLive bool
	Now         time.Time
}

// Select resolves o against root: each project directory becomes its
// projects/ entry (transcripts.ProjectDirFor) and its memory directory
// (transcripts.MemoryDirFor — an overridden memory location is an error
// naming the override); each session id is looked up with
// transcripts.Find restricted to root; AllProjects lists every session and
// memory of the root. Titles are resolved for every selected session.
// Sessions are newest first and never listed twice; memories follow root
// order. Selecting nothing (no projects, sessions or AllProjects) is an
// error, so a caller must state the default scope explicitly.
func Select(ctx context.Context, root transcripts.Root, hist transcripts.HistoryIndex, live map[string]transcripts.LiveSession, o SelectOptions) (Selection, error) {
	switch o.Only {
	case "", OnlySessions, OnlyMemories:
	default:
		return Selection{}, fmt.Errorf("invalid --only %q: must be %q or %q", o.Only, OnlySessions, OnlyMemories)
	}
	if !o.AllProjects && len(o.Projects) == 0 && len(o.Sessions) == 0 {
		return Selection{}, errors.New("nothing selected: pass --project, --session or --all-projects")
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	var since time.Time
	if o.Since > 0 {
		since = now.Add(-o.Since)
	}
	wantSessions := o.Only != OnlyMemories
	wantMemories := o.Only != OnlySessions

	sel := Selection{Root: root, Parts: DefaultParts}
	seen := map[string]bool{} // transcript paths already selected
	addSessions := func(ss []transcripts.Session) {
		for _, s := range ss {
			if seen[s.Path] {
				continue
			}
			if !since.IsZero() && s.LastTS.Before(since) {
				continue
			}
			if s.Live && !o.IncludeLive {
				continue
			}
			seen[s.Path] = true
			sel.Sessions = append(sel.Sessions, s)
		}
	}
	listOpts := transcripts.ListOptions{Titles: true, History: hist, Live: live, Since: since, Now: now}

	var memDirs []string // memory directories wanted, in selection order
	if o.AllProjects {
		if wantSessions {
			ss, err := transcripts.List(ctx, root, listOpts)
			if err != nil {
				return Selection{}, err
			}
			addSessions(ss)
		}
	}
	for _, p := range o.Projects {
		dir, err := store.NormalizePath(p)
		if err != nil {
			return Selection{}, fmt.Errorf("project %q: %w", p, err)
		}
		if wantSessions && !o.AllProjects {
			projDir, err := transcripts.ProjectDirFor(root, dir, nil)
			if err != nil {
				return Selection{}, fmt.Errorf("project %q: %w", p, err)
			}
			opts := listOpts
			opts.Slug = filepath.Base(projDir)
			ss, err := transcripts.List(ctx, root, opts)
			if err != nil {
				return Selection{}, err
			}
			addSessions(ss)
		}
		if wantMemories {
			memDir, err := transcripts.MemoryDirFor(root, dir)
			if err != nil {
				return Selection{}, fmt.Errorf("project %q: %w", p, err)
			}
			memDirs = append(memDirs, memDir)
		}
	}
	if wantSessions {
		for _, id := range o.Sessions {
			ss, err := transcripts.Find([]transcripts.Root{root}, id)
			if err != nil {
				return Selection{}, err
			}
			for i := range ss {
				if _, ok := live[ss[i].ID]; ok {
					ss[i].Live = true
				}
			}
			addSessions(ss)
		}
		sortNewestFirst(sel.Sessions)
	}

	if wantMemories && (o.AllProjects || len(memDirs) > 0) {
		mems, err := transcripts.Memories(ctx, []transcripts.Root{root})
		if err != nil {
			return Selection{}, err
		}
		if o.AllProjects {
			sel.Memories = mems
		} else {
			byDir := map[string]transcripts.Memory{}
			for _, m := range mems {
				byDir[canonicalPath(m.Dir)] = m
			}
			added := map[string]bool{}
			for _, d := range memDirs {
				key := canonicalPath(d)
				if m, ok := byDir[key]; ok && !added[key] {
					added[key] = true
					sel.Memories = append(sel.Memories, m)
				}
			}
		}
	}
	return sel, nil
}

// sortNewestFirst orders sessions by transcript mtime, newest first, ties
// by id — the order transcripts.List uses.
func sortNewestFirst(ss []transcripts.Session) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].LastTS.Equal(ss[j].LastTS) {
			return ss[i].LastTS.After(ss[j].LastTS)
		}
		return ss[i].ID < ss[j].ID
	})
}

// canonicalPath resolves symlinks when the path exists and cleans it
// otherwise — the comparison key for directories on this machine.
func canonicalPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// dirExists reports whether p names a directory (following symlinks).
func dirExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// partNames renders the parts an entry actually carries, in grammar order.
func partNames(has map[string]bool) []string {
	var out []string
	for _, name := range []string{"transcript", "sidecar", "file-history", "plans", "history", "tasks"} {
		if has[name] {
			out = append(out, name)
		}
	}
	return out
}

// sanitize is transcripts.Sanitize for report and warning strings.
func sanitize(s string) string { return transcripts.Sanitize(s) }

// short8 is the eight-character prefix of an id for messages.
func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// joinSlash builds a bundle-relative name from components.
func joinSlash(parts ...string) string { return strings.Join(parts, "/") }
