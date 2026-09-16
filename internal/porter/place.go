package porter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// ConflictPolicy says what Import does when the destination root already
// holds a transcript with the same session id under the target slug.
type ConflictPolicy string

const (
	// ConflictSkip leaves the existing session alone (the default).
	ConflictSkip ConflictPolicy = "skip"
	// ConflictOverwrite sets the existing transcript and sidecar aside as
	// "<name>.bffs-replaced-<epochms>" (never deleted) and lands the
	// imported one; refused while the session is open in a running claude.
	ConflictOverwrite ConflictPolicy = "overwrite"
)

// Placement modes (Placement.Mode).
const (
	// PlaceIdentity: the entry's cwd exists here as the same directory —
	// the session lands under that directory's projects/ entry with no
	// relocated record.
	PlaceIdentity = "identity"
	// PlaceMapped: a prefix rule, --into or an interactive answer moved
	// the entry to a new directory (M6).
	PlaceMapped = "mapped"
	// PlaceAsIs: the entry keeps its original slug and is flagged pending.
	PlaceAsIs = "as-is"
)

// Placement is where one manifest entry lands: NewCwd is the directory it
// belongs to on this machine ("" for as-is), Mode one of the Place*
// constants, Confirmed whether the user chose the directory (a mapping,
// --into or an interactive answer — never identity or as-is), CarryTrust
// whether the source trust answers may be applied (M6).
type Placement struct {
	Entry      *bundle.Entry
	NewCwd     string
	Mode       string
	Confirmed  bool
	CarryTrust bool
}

// Placer decides placements interactively (M6: cmd supplies it). Nil
// means the M4 rule: identity when the cwd exists here, else as-is.
type Placer func(ctx context.Context, m *bundle.Manifest, suggestions []rehome.Suggestion) ([]Placement, error)

// Plan statuses.
const (
	planImport = "import"
	planSkip   = "skip"
	planHeld   = "held"
)

// sessionPlan is the preflight decision for one session entry.
type sessionPlan struct {
	entry         *bundle.Entry
	place         Placement
	slug          string // target projects/ entry name
	status        string
	reason        string
	overwrite     bool // an existing transcript under slug will be set aside
	sidecarExists bool // …and so will its sidecar directory
}

// memoryPlan is the preflight decision for one memory entry.
type memoryPlan struct {
	entry  *bundle.Entry
	place  Placement
	dir    string // target memory directory
	status string
	reason string
}

// importPlan is the whole preflight (plan §9.3–§9.5, §9.9).
type importPlan struct {
	sessions []sessionPlan
	memories []memoryPlan
	warnings []string
}

// identityPlacement reports whether local — the directory an entry is
// placed against — is the very directory the entry's cwd names: local
// exists as a directory and both normalise to one path (string equality
// of store.NormalizePath results, never slug equality: "build_x" and
// "build-x" share a slug). Both must be absolute on this OS: a manifest
// cwd of "." or "~" would otherwise resolve against the importing
// process's directory, and a POSIX path stats as a drive-relative one on
// Windows — neither names "the same directory here".
func identityPlacement(local, cwd string) bool {
	if local == "" || cwd == "" || !filepath.IsAbs(local) || !filepath.IsAbs(cwd) || !dirExists(local) {
		return false
	}
	a, err := store.NormalizePath(local)
	if err != nil {
		return false
	}
	b, err := store.NormalizePath(cwd)
	if err != nil {
		return false
	}
	return a == b
}

// planEntries decides every entry of m before anything is written.
func planEntries(ctx context.Context, m *bundle.Manifest, o ImportOptions, dest transcripts.Root, now time.Time) (importPlan, error) {
	var p importPlan
	warn := func(format string, args ...any) {
		p.warnings = append(p.warnings, sanitize(fmt.Sprintf(format, args...)))
	}

	// One listing of the destination root, restricted to the incoming
	// ids, gives every existing transcript (any slug) and its liveness.
	var sids []string
	for i := range m.Entries {
		if m.Entries[i].Kind == bundle.EntrySession {
			sids = append(sids, m.Entries[i].SessionID)
		}
	}
	existing := map[string][]transcripts.Session{}
	if len(sids) > 0 {
		ss, err := transcripts.List(ctx, dest, transcripts.ListOptions{IDs: sids, Live: o.Live, Now: now})
		if err != nil {
			return importPlan{}, err
		}
		for _, s := range ss {
			existing[s.ID] = append(existing[s.ID], s)
		}
	}
	policy := o.OnConflict
	if policy == "" {
		policy = ConflictSkip
	}

	memoryOverride := checkMemoryOverride(dest)

	for i := range m.Entries {
		e := &m.Entries[i]
		switch e.Kind {
		case bundle.EntrySession:
			p.sessions = append(p.sessions, planSession(e, o, dest, existing[e.SessionID], policy, warn))
		case bundle.EntryMemory:
			p.memories = append(p.memories, planMemory(e, o, dest, memoryOverride))
		}
	}
	return p, nil
}

func planSession(e *bundle.Entry, o ImportOptions, dest transcripts.Root, existing []transcripts.Session, policy ConflictPolicy, warn func(string, ...any)) sessionPlan {
	sid := e.SessionID
	sp := sessionPlan{entry: e, status: planImport, slug: e.Slug}
	sp.place = Placement{Entry: e, Mode: PlaceAsIs}
	if !o.AsIs && identityPlacement(e.Cwd, e.Cwd) {
		dir, err := transcripts.ProjectDirFor(dest, e.Cwd, o.LaunchEnv)
		if err != nil {
			warn("session %s: %v; importing as-is under projects/%s", short8(sid), err, e.Slug)
		} else {
			sp.place = Placement{Entry: e, Mode: PlaceIdentity, NewCwd: e.Cwd}
			sp.slug = filepath.Base(dir)
		}
	}
	transcriptName := joinSlash("projects", sp.slug, sid+transcripts.TranscriptExt)
	if kind, _, _, err := bundle.ClassifyName(transcriptName); err != nil || kind != bundle.NameTranscript {
		sp.status, sp.reason = planSkip, fmt.Sprintf("target projects/%s is not a usable slug", sp.slug)
		return sp
	}

	for _, ex := range existing {
		if ex.Slug != sp.slug {
			sp.status, sp.reason = planSkip, fmt.Sprintf("exists under %s; would make claude --resume ambiguous", ex.Slug)
			return sp
		}
	}
	for _, ex := range existing {
		// Same slug: the collision policy decides.
		switch policy {
		case ConflictOverwrite:
			if ex.Live {
				sp.status, sp.reason = planHeld, "open in a running claude; close it before overwriting"
				return sp
			}
			sp.overwrite = true
			if info, err := os.Lstat(ex.SidecarDir); err == nil && info != nil {
				sp.sidecarExists = true
			}
		default:
			sp.status, sp.reason = planSkip, "exists (--on-conflict overwrite to replace)"
			return sp
		}
	}
	if !sp.overwrite {
		// Nothing may already stand where the session lands — a planted
		// symlink or a stray directory included.
		slugDir := filepath.Join(dest.Dir, sp.slug)
		if _, err := os.Lstat(filepath.Join(slugDir, sid+transcripts.TranscriptExt)); err == nil {
			sp.status, sp.reason = planSkip, fmt.Sprintf("projects/%s/%s.jsonl already exists", sp.slug, sid)
			return sp
		}
		if hasSidecar(e) {
			if _, err := os.Lstat(filepath.Join(slugDir, sid)); err == nil {
				sp.status, sp.reason = planSkip, fmt.Sprintf("projects/%s/%s already exists", sp.slug, sid)
				return sp
			}
		}
	}
	return sp
}

func planMemory(e *bundle.Entry, o ImportOptions, dest transcripts.Root, override error) memoryPlan {
	mp := memoryPlan{entry: e, status: planImport}
	mp.place = Placement{Entry: e, Mode: PlaceAsIs}
	mp.dir = filepath.Join(dest.Dir, e.Slug, transcripts.MemorySubdir)
	if !o.AsIs && identityPlacement(e.Cwd, e.Cwd) {
		dir, err := transcripts.MemoryDirFor(dest, e.Cwd)
		switch {
		case err == nil:
			mp.place = Placement{Entry: e, Mode: PlaceIdentity, NewCwd: e.Cwd}
			mp.dir = dir
		case errors.Is(err, transcripts.ErrMemoryDirOverridden):
			mp.status, mp.reason = planSkip, err.Error()+"; bffs cannot place memory there yet"
			return mp
		default:
			mp.status, mp.reason = planSkip, err.Error()
			return mp
		}
	}
	if override != nil {
		mp.status, mp.reason = planSkip, override.Error()+"; bffs cannot place memory there yet"
		return mp
	}
	if _, _, _, err := bundle.ClassifyName(joinSlash("memory", filepath.Base(filepath.Dir(mp.dir)), transcripts.MemoryIndexFile)); err != nil {
		mp.status, mp.reason = planSkip, fmt.Sprintf("target %s is not a usable memory directory", mp.dir)
		return mp
	}
	mode := o.Memory
	if mode == "" {
		mode = rehome.MemorySkip
	}
	if mode == rehome.MemorySkip {
		if info, err := os.Lstat(mp.dir); err == nil && info != nil {
			mp.status, mp.reason = planSkip, "memory directory exists (--memory overwrite replaces it)"
			return mp
		}
	}
	return mp
}

// checkMemoryOverride reports the ErrMemoryDirOverridden error that
// applies to dest as a whole (environment or settings), or nil. The probe
// asks transcripts.MemoryDirFor about the root's own projects/ directory,
// whose answer is irrelevant; only the override check matters.
func checkMemoryOverride(dest transcripts.Root) error {
	_, err := transcripts.MemoryDirFor(dest, dest.Dir)
	if err != nil && errors.Is(err, transcripts.ErrMemoryDirOverridden) {
		return err
	}
	return nil
}

// hasSidecar reports whether a session entry carries sidecar files.
func hasSidecar(e *bundle.Entry) bool {
	for _, f := range e.Files {
		if kind, _, _, err := bundle.ClassifyName(f.Path); err == nil && kind == bundle.NameSidecar {
			return true
		}
	}
	return false
}
