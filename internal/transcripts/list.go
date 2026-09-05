package transcripts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
)

const (
	// TranscriptExt is the suffix of a session's transcript file.
	TranscriptExt = ".jsonl"

	// SubagentsSubdir holds a session's subagent transcripts inside its
	// sidecar directory.
	SubagentsSubdir = "subagents"

	// FileHistorySubdir, TasksSubdir and PlansSubdir are the per-session
	// artifact directories under a config dir (disk.md §1.1).
	FileHistorySubdir = "file-history"
	TasksSubdir       = "tasks"
	PlansSubdir       = "plans"

	// liveSiblingWindow is how recently a <sid>.jsonl.superseded-* or
	// <sid>.jsonl.compact.tmp* sibling must have been written for the
	// session to count as live: Claude leaves them behind while
	// relocating or compacting a transcript it has open.
	liveSiblingWindow = 60 * time.Second
)

// Attributor says which account a session belongs to. usage implements
// it (root owner > lastSessionId > import record > launch log); the
// interface keeps transcripts from importing usage. owner is the root's
// Owner ("" for the shared pool). src names the tier that answered.
type Attributor interface {
	Attribute(sid, cwd string, firstTS time.Time, owner string) (account, src string)
}

// Session is one conversation as found on disk. The fast path (List
// without Titles) fills only what a directory listing knows: ID, Slug,
// Path, SidecarDir, Root, LastTS (the transcript's mtime), Size,
// Subagents and Live. Titles adds everything the head and tail windows
// say. Account is filled when an Attributor is given, Import when the
// session has an import record.
type Session struct {
	ID         string
	Slug       string // the projects/ entry the transcript lives in
	Path       string // the transcript
	SidecarDir string // <slugDir>/<sid> — the path, whether or not it exists
	Root       Root

	Cwd       string // effective cwd: RelocatedCwd, else HeadCwd
	HeadCwd   string // the cwd the session started in
	Relocated bool   // a relocated record moved it
	CwdExists bool   // Cwd is a directory on this machine

	PlanSlug, GitBranch, Version string
	Title, TitleSource           string
	FirstTS, LastTS              time.Time
	Size                         int64
	Subagents                    int // <sid>/subagents/*.jsonl files, never opened
	Live                         bool

	Account, AttribSource string
	Import                *imports.SessionRef
}

// ListOptions narrows and enriches List. Zero values mean "no filter":
// every slug, every id, any age, no limit, now = time.Now().
type ListOptions struct {
	Slug       string    // exact projects/ entry name
	IDs        []string  // full session ids (case-insensitive)
	Since      time.Time // transcripts modified at or after this instant
	Titles     bool      // read head/tail windows: cwd, title, version, branch, plan
	History    HistoryIndex
	Attributor Attributor
	Live       map[string]LiveSession
	Imports    map[string]imports.SessionRef
	Limit      int // 0 = all
	Now        time.Time
}

// List enumerates the sessions of root — every <root.Dir>/<slug>/<sid>.jsonl
// whose slug is not reserved and whose name is a UUID — newest transcript
// mtime first. Without Titles no transcript is opened: the listing costs
// one ReadDir per slug (plus one per sidecar dir for the subagent count),
// so a multi-gigabyte pool lists in milliseconds. With Titles the head
// and tail windows of each returned session (after Limit) are read.
//
// A session is Live when opts.Live names it, or when a
// <sid>.jsonl.superseded-* / <sid>.jsonl.compact.tmp* sibling is younger
// than a minute relative to opts.Now. Account comes from opts.Attributor
// (called with the effective cwd and first timestamp when Titles is set,
// empty otherwise); Import from opts.Imports. opts.History is consulted
// for the title fallback; when nil, <root.ConfigDir>/history.jsonl is
// loaded the first time a session needs it. A root whose projects dir does
// not exist lists as empty. ctx is checked between directory entries.
func List(ctx context.Context, root Root, opts ListOptions) ([]Session, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	var match func(sid string) bool
	if len(opts.IDs) > 0 {
		want := make(map[string]bool, len(opts.IDs))
		for _, id := range opts.IDs {
			want[strings.ToLower(id)] = true
		}
		match = func(sid string) bool { return want[strings.ToLower(sid)] }
	}

	entries, err := os.ReadDir(root.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root.Dir, err)
	}
	var all []Session
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if !e.IsDir() || IsReserved(name) || (opts.Slug != "" && name != opts.Slug) {
			continue
		}
		ss, err := listSlug(ctx, root, name, match, opts.Since, opts.Live, now)
		if err != nil {
			return nil, err
		}
		all = append(all, ss...)
	}
	sortNewestFirst(all)
	if opts.Limit > 0 && len(all) > opts.Limit {
		all = all[:opts.Limit]
	}

	hist := historyLoader(root.ConfigDir, opts.History)
	for i := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := &all[i]
		if opts.Titles {
			s.enrich(hist)
		}
		if opts.Attributor != nil {
			s.Account, s.AttribSource = opts.Attributor.Attribute(s.ID, s.Cwd, s.FirstTS, root.Owner)
		}
		if ref, ok := opts.Imports[s.ID]; ok {
			ref := ref
			s.Import = &ref
		}
	}
	return all, nil
}

// listSlug is the fast path for one projects/<slug> directory: ReadDir
// plus Info per transcript, a ReadDir per sidecar dir for the subagent
// count, and an Info per live-marker sibling. No transcript is opened.
func listSlug(ctx context.Context, root Root, slug string, match func(string) bool, since time.Time, live map[string]LiveSession, now time.Time) ([]Session, error) {
	dir := filepath.Join(root.Dir, slug)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	dirs := map[string]bool{}
	recent := map[string]bool{} // sids with a fresh superseded/compact sibling
	var out []Session
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if e.IsDir() {
			dirs[name] = true
			continue
		}
		if sid, ok := liveSibling(name); ok {
			if info, err := e.Info(); err == nil && info.ModTime().After(now.Add(-liveSiblingWindow)) {
				recent[sid] = true
			}
			continue
		}
		sid, ok := strings.CutSuffix(name, TranscriptExt)
		if !ok || !isUUID(sid) || !e.Type().IsRegular() {
			continue
		}
		if match != nil && !match(sid) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // renamed away between ReadDir and Info (compaction)
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			continue
		}
		out = append(out, Session{
			ID:         sid,
			Slug:       slug,
			Path:       filepath.Join(dir, name),
			SidecarDir: filepath.Join(dir, sid),
			Root:       root,
			LastTS:     info.ModTime(),
			Size:       info.Size(),
		})
	}
	for i := range out {
		s := &out[i]
		if dirs[s.ID] {
			s.Subagents = countSubagents(s.SidecarDir)
		}
		if _, ok := live[s.ID]; ok || recent[s.ID] {
			s.Live = true
		}
	}
	return out, nil
}

// liveSibling recognises the files Claude leaves next to a transcript it
// is relocating or compacting and returns the session id they belong to.
func liveSibling(name string) (string, bool) {
	sid, rest, ok := strings.Cut(name, TranscriptExt+".")
	if !ok || !isUUID(sid) {
		return "", false
	}
	if strings.HasPrefix(rest, "superseded-") || strings.HasPrefix(rest, "compact.tmp") {
		return sid, true
	}
	return "", false
}

// countSubagents counts <sidecar>/subagents/*.jsonl without opening any.
func countSubagents(sidecar string) int {
	entries, err := os.ReadDir(filepath.Join(sidecar, SubagentsSubdir))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), TranscriptExt) {
			n++
		}
	}
	return n
}

// enrich reads the head and tail windows of s and fills the fields the
// fast path leaves empty. A transcript that vanished or cannot be read
// leaves them empty rather than failing the listing.
func (s *Session) enrich(hist func() HistoryIndex) {
	h, err := ReadHead(s.Path)
	if err != nil {
		return
	}
	t, err := ReadTail(s.Path)
	if err != nil {
		return
	}
	s.HeadCwd = h.Cwd
	s.Cwd = EffectiveCwd(h, t)
	s.Relocated = t.RelocatedCwd != ""
	if s.Cwd != "" {
		if info, err := os.Stat(s.Cwd); err == nil && info.IsDir() {
			s.CwdExists = true
		}
	}
	s.PlanSlug, s.GitBranch, s.Version, s.FirstTS = h.PlanSlug, h.GitBranch, h.Version, h.FirstTS
	if s.Title, s.TitleSource = Title(h, t, nil); s.Title == "" && hist != nil {
		s.Title, s.TitleSource = Title(h, t, hist())
	}
}

// historyLoader returns a function yielding hist when given, else loading
// <configDir>/history.jsonl once on first use.
func historyLoader(configDir string, hist HistoryIndex) func() HistoryIndex {
	loaded := hist != nil
	return func() HistoryIndex {
		if !loaded {
			loaded = true
			if idx, err := LoadHistory(configDir); err == nil {
				hist = idx
			} else {
				hist = HistoryIndex{}
			}
		}
		return hist
	}
}

func sortNewestFirst(ss []Session) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].LastTS.Equal(ss[j].LastTS) {
			return ss[i].LastTS.After(ss[j].LastTS)
		}
		return ss[i].ID < ss[j].ID
	})
}

// isUUID reports whether s has the 8-4-4-4-12 hex shape of a session id.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// Artifacts are every per-session file Claude keeps besides the
// transcript. Paths are computed, not checked, except PlanFiles which
// lists only files that exist.
type Artifacts struct {
	Transcript     string
	SidecarDir     string   // <root.Dir>/<slug>/<sid>
	FileHistoryDir string   // <root.ConfigDir>/file-history/<sid>
	TasksDir       string   // <root.ConfigDir>/tasks/<sid>
	PlanFiles      []string // existing plans/<planSlug>.md, <planSlug>-agent-*.md, <planSlug>.workshop.md
}

// ArtifactsFor locates the artifacts of s under root (disk.md §1.1).
func ArtifactsFor(root Root, s Session) Artifacts {
	a := Artifacts{
		Transcript:     s.Path,
		SidecarDir:     s.SidecarDir,
		FileHistoryDir: filepath.Join(root.ConfigDir, FileHistorySubdir, s.ID),
		TasksDir:       filepath.Join(root.ConfigDir, TasksSubdir, s.ID),
	}
	if a.Transcript == "" && s.Slug != "" {
		a.Transcript = filepath.Join(root.Dir, s.Slug, s.ID+TranscriptExt)
	}
	if a.SidecarDir == "" && s.Slug != "" {
		a.SidecarDir = filepath.Join(root.Dir, s.Slug, s.ID)
	}
	if s.PlanSlug != "" && safePlanSlug(s.PlanSlug) {
		plans := filepath.Join(root.ConfigDir, PlansSubdir)
		for _, name := range []string{s.PlanSlug + ".md", s.PlanSlug + ".workshop.md"} {
			p := filepath.Join(plans, name)
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				a.PlanFiles = append(a.PlanFiles, p)
			}
		}
		if agents, err := filepath.Glob(filepath.Join(plans, s.PlanSlug+"-agent-*.md")); err == nil {
			for _, p := range agents {
				if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
					a.PlanFiles = append(a.PlanFiles, p)
				}
			}
		}
		sort.Strings(a.PlanFiles)
	}
	return a
}

// safePlanSlug rejects a plan slug — a transcript-supplied string — that
// could name something outside plans/ or act as a glob pattern.
func safePlanSlug(slug string) bool {
	return slug != "." && slug != ".." && !strings.ContainsAny(slug, `/\*?[`)
}
