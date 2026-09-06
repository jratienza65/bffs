package tui

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/usage"
)

// services is what every screen shares: the store, the roots, the
// attribution and import indexes, and the caches. Screens read it from
// Update (one goroutine); commands capture what they need by value and
// hand results back as messages, so nothing here is locked.
type services struct {
	ctx           context.Context
	cfgDir        string
	homeClaudeDir string
	version       string
	start         string // "sessions" | "memories": the tab a project opens on
	cwd           string // normalised process cwd, "" when unknown
	accs          store.Accounts
	state         store.State
	roots         []transcripts.Root
	attributor    *usage.Attributor // nil when unavailable
	imports       map[string]imports.SessionRef
	warnings      []string
	now           func() time.Time
	theme         string // the active theme's name (theme.go)
	isDark        bool   // the terminal background, dark until the terminal answers

	// titles caches what the head and tail windows said, keyed by the
	// transcript's identity, so a page scrolled back to costs nothing.
	titles map[titleKey]titleMeta
	// history is history.jsonl per config dir, loaded once on first need.
	history map[string]transcripts.HistoryIndex
	// memories is the memory catalog per root, loaded once per root.
	memories map[string][]transcripts.Memory
}

// loadServices reads the store and enumerates the roots. Attribution
// and import records are enrichment: a problem with them is a warning,
// never a refusal to browse.
func loadServices(ctx context.Context, o Options) (*services, error) {
	accs, err := store.LoadAccounts(o.CfgDir)
	if err != nil {
		return nil, err
	}
	state, err := store.LoadState(o.CfgDir)
	if err != nil {
		return nil, err
	}
	roots, err := transcripts.Roots(o.CfgDir, o.HomeClaudeDir, accs, state)
	if err != nil {
		return nil, err
	}
	svc := &services{
		ctx:           ctx,
		cfgDir:        o.CfgDir,
		homeClaudeDir: o.HomeClaudeDir,
		version:       o.Version,
		start:         o.Start,
		accs:          accs,
		state:         state,
		roots:         roots,
		now:           time.Now,
		titles:        map[titleKey]titleMeta{},
		history:       map[string]transcripts.HistoryIndex{},
		memories:      map[string][]transcripts.Memory{},
	}
	if svc.start != "memories" {
		svc.start = "sessions"
	}
	if cwd, err := os.Getwd(); err == nil {
		if n, err := store.NormalizePath(cwd); err == nil {
			svc.cwd = n
		}
	}
	if attr, err := usage.NewAttributor(o.CfgDir, accs); err != nil {
		svc.warnings = append(svc.warnings, fmt.Sprintf("attribution unavailable: %v", err))
	} else {
		svc.attributor = attr
	}
	if recs, err := imports.Load(o.CfgDir); err != nil {
		svc.warnings = append(svc.warnings, fmt.Sprintf("import records unavailable: %v", err))
	} else {
		svc.imports = imports.BySession(recs)
	}
	for _, r := range roots {
		if r.Orphan {
			svc.warnings = append(svc.warnings, fmt.Sprintf("orphan session dir %s: no account uses it (read-only)", shortPath(r.ConfigDir)))
		}
	}
	return svc, nil
}

// configDirs lists the distinct Claude config dirs behind the roots —
// the dirs whose runtime sessions/ transcripts.Live reads.
func (svc *services) configDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, r := range svc.roots {
		if seen[r.ConfigDir] {
			continue
		}
		seen[r.ConfigDir] = true
		dirs = append(dirs, r.ConfigDir)
	}
	return dirs
}

// attribute names the account of s with whatever evidence is at hand:
// without a resolved head the launch-log tier cannot fire, so the answer
// is refined once the title window has been read.
func (svc *services) attribute(s *transcripts.Session) {
	if svc.attributor == nil {
		return
	}
	s.Account, s.AttribSource = svc.attributor.Attribute(s.ID, s.Cwd, s.FirstTS, s.Root.Owner)
}

// titleKey identifies a transcript's content for the title cache: a
// rewrite (compaction, relocation stamp) changes mtime or size.
type titleKey struct {
	path  string
	mtime int64
	size  int64
}

func keyOf(s transcripts.Session) titleKey {
	return titleKey{path: s.Path, mtime: s.LastTS.UnixNano(), size: s.Size}
}

// titleMeta is what the head and tail windows of a transcript say —
// the fields transcripts.List fills with Titles set — plus the history
// fallback. Every string is Sanitize'd where it is rendered.
type titleMeta struct {
	Title, TitleSource string
	Cwd, HeadCwd       string
	Relocated          bool
	CwdExists          bool
	PlanSlug           string
	GitBranch          string
	Version            string
	FirstTS            time.Time
	SessionID          string // the head's own id, for the history lookup
	Failed             bool   // the transcript could not be read
}

// readMeta reads the two windows of the transcript at path, the way
// transcripts.List does with Titles set. hist may be nil.
func readMeta(path string, hist transcripts.HistoryIndex) titleMeta {
	h, err := transcripts.ReadHead(path)
	if err != nil {
		return titleMeta{Failed: true}
	}
	t, err := transcripts.ReadTail(path)
	if err != nil {
		return titleMeta{Failed: true}
	}
	m := titleMeta{
		HeadCwd:   h.Cwd,
		Cwd:       transcripts.EffectiveCwd(h, t),
		Relocated: t.RelocatedCwd != "",
		PlanSlug:  h.PlanSlug,
		GitBranch: h.GitBranch,
		Version:   h.Version,
		FirstTS:   h.FirstTS,
		SessionID: h.SessionID,
	}
	if m.Cwd != "" {
		m.CwdExists = isDir(m.Cwd)
	}
	m.Title, m.TitleSource = transcripts.Title(h, t, hist)
	return m
}

// apply copies the window fields onto s.
func (m titleMeta) apply(s *transcripts.Session) {
	s.Title, s.TitleSource = m.Title, m.TitleSource
	s.Cwd, s.HeadCwd, s.Relocated, s.CwdExists = m.Cwd, m.HeadCwd, m.Relocated, m.CwdExists
	s.PlanSlug, s.GitBranch, s.Version, s.FirstTS = m.PlanSlug, m.GitBranch, m.Version, m.FirstTS
}

// titleReq is one transcript whose windows a command should read.
type titleReq struct {
	id, path string
}

// titleBatch bounds how many transcripts one command reads; a filter
// over a large project fans out into several commands that run in
// parallel.
const titleBatch = 25

// resolveTitles reads the windows of reqs in one command. hist is the
// config dir's history index when already loaded; when it is nil and a
// title needs the fallback, the command loads it and returns it in the
// message so the app can cache it.
func resolveTitles(ctx context.Context, slug, configDir string, reqs []titleReq, hist transcripts.HistoryIndex) tea.Cmd {
	return func() tea.Msg {
		out := titlesResolvedMsg{slug: slug, configDir: configDir, metas: make(map[string]titleMeta, len(reqs))}
		for _, r := range reqs {
			if ctx.Err() != nil {
				break
			}
			m := readMeta(r.path, hist)
			if m.Title == "" && !m.Failed && hist == nil {
				if idx, err := transcripts.LoadHistory(configDir); err == nil {
					hist = idx
				} else {
					hist = transcripts.HistoryIndex{}
				}
				out.history = hist
				m = readMeta(r.path, hist)
			}
			out.metas[r.id] = m
		}
		return out
	}
}
