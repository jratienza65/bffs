package porter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
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
	// the entry to a new directory; the session lands under that
	// directory's projects/ entry with a relocated record.
	PlaceMapped = "mapped"
	// PlaceAsIs: the entry keeps its original slug and is flagged pending.
	PlaceAsIs = "as-is"
)

// Placement is where one manifest entry lands: NewCwd is the directory it
// belongs to on this machine ("" for as-is), Mode one of the Place*
// constants, Confirmed whether the user chose the directory (a mapping,
// --into or an interactive answer — never a bare identity or as-is),
// CarryTrust whether the source trust answers are applied (only for a
// confirmed mapped placement).
type Placement struct {
	Entry      *bundle.Entry
	NewCwd     string
	Mode       string
	Confirmed  bool
	CarryTrust bool
}

// Placer decides placements interactively (cmd supplies it): it is called
// once, with the manifest and rehome.Suggest's candidates for every entry
// no rule, --into or identity decided, and returns one Placement per
// project (matched to entries by Entry pointer, else by the entry's cwd).
// An entry it does not mention lands as-is. Nil means the rule without a
// prompt: identity when the cwd exists here, else as-is.
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

	placements, err := decidePlacements(ctx, m, o, warn)
	if err != nil {
		return importPlan{}, err
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

	// transcripts.ProjectDirFor scans every entry of the destination root
	// when the target entry does not exist yet; one answer per directory
	// serves every session placed there.
	type projDir struct {
		dir string
		err error
	}
	projDirs := map[string]projDir{}
	projectDirFor := func(cwd string) (string, error) {
		if d, ok := projDirs[cwd]; ok {
			return d.dir, d.err
		}
		dir, err := transcripts.ProjectDirFor(dest, cwd, o.LaunchEnv)
		projDirs[cwd] = projDir{dir, err}
		return dir, err
	}

	for i := range m.Entries {
		e := &m.Entries[i]
		switch e.Kind {
		case bundle.EntrySession:
			p.sessions = append(p.sessions, planSession(e, dest, existing[e.SessionID], policy, placements[e], projectDirFor, warn))
		case bundle.EntryMemory:
			p.memories = append(p.memories, planMemory(e, o, dest, memoryOverride, placements[e]))
		}
	}
	return p, nil
}

// decidePlacements applies plan §9.3 to every entry: --as-is; --into
// (the single-project shorthand); the prefix rules (a derived directory
// that does not exist here falls back to as-is with a warning); identity
// when the cwd exists here; the Placer for what is left; as-is otherwise.
// CarryTrust is kept only on confirmed mapped placements.
func decidePlacements(ctx context.Context, m *bundle.Manifest, o ImportOptions, warn func(string, ...any)) (map[*bundle.Entry]Placement, error) {
	out := map[*bundle.Entry]Placement{}
	asIs := func(e *bundle.Entry) Placement { return Placement{Entry: e, Mode: PlaceAsIs} }
	if o.AsIs {
		for i := range m.Entries {
			e := &m.Entries[i]
			out[e] = asIs(e)
		}
		return out, nil
	}
	into := ""
	if o.Into != "" {
		norm, err := store.NormalizePath(o.Into)
		if err != nil {
			return nil, fmt.Errorf("--into %q: %w", o.Into, err)
		}
		if !dirExists(norm) {
			return nil, fmt.Errorf("--into %q is not a directory", o.Into)
		}
		if n := projectCount(m); n > 1 {
			return nil, fmt.Errorf("bundle holds %d projects; --into needs a single-project bundle, use --map", n)
		}
		into = norm
	}

	var undecided []*bundle.Entry
	for i := range m.Entries {
		e := &m.Entries[i]
		switch {
		case into != "":
			out[e] = chosenPlacement(e, into)
		case len(o.Map) > 0:
			newCwd, _, ok := rehome.ApplyMappings(o.Map, e.Cwd, e.ProjectKey)
			if !ok {
				break
			}
			if !dirExists(newCwd) {
				warn("%s: mapped directory %q does not exist; importing as-is under projects/%s", entryLabel(e), newCwd, e.Slug)
				out[e] = asIs(e)
				continue
			}
			out[e] = chosenPlacement(e, newCwd)
		}
		if _, done := out[e]; done {
			continue
		}
		if identityPlacement(e.Cwd, e.Cwd) {
			out[e] = Placement{Entry: e, Mode: PlaceIdentity, NewCwd: e.Cwd}
			continue
		}
		undecided = append(undecided, e)
	}

	if len(undecided) > 0 && o.Place != nil {
		home, _ := os.UserHomeDir()
		answers, err := o.Place(ctx, m, rehome.Suggest(recordForSuggest(m, undecided), home, nil))
		if err != nil {
			return nil, err
		}
		byEntry := map[*bundle.Entry]Placement{}
		byCwd := map[string]Placement{}
		for _, a := range answers {
			if a.Entry != nil {
				byEntry[a.Entry] = a
				if a.Entry.Cwd != "" {
					if _, ok := byCwd[a.Entry.Cwd]; !ok {
						byCwd[a.Entry.Cwd] = a
					}
				}
			}
		}
		for _, e := range undecided {
			a, ok := byEntry[e]
			if !ok {
				a, ok = byCwd[e.Cwd]
			}
			if !ok || a.Mode == PlaceAsIs || a.NewCwd == "" {
				out[e] = asIs(e)
				continue
			}
			norm, err := store.NormalizePath(a.NewCwd)
			if err != nil {
				return nil, fmt.Errorf("%s: chosen directory %q: %w", entryLabel(e), a.NewCwd, err)
			}
			if !dirExists(norm) {
				return nil, fmt.Errorf("%s: chosen directory %q does not exist", entryLabel(e), a.NewCwd)
			}
			p := chosenPlacement(e, norm)
			p.CarryTrust = a.CarryTrust
			out[e] = p
		}
	}
	for _, e := range undecided {
		if _, ok := out[e]; !ok {
			out[e] = asIs(e)
		}
	}
	for e, p := range out {
		p.CarryTrust = (o.CarryTrust || p.CarryTrust) && p.Mode == PlaceMapped && p.Confirmed
		out[e] = p
	}
	return out, nil
}

// chosenPlacement is the placement of an entry the user pointed at dir:
// identity when dir is the entry's own directory (no relocated record),
// mapped otherwise — confirmed either way.
func chosenPlacement(e *bundle.Entry, dir string) Placement {
	if identityPlacement(dir, e.Cwd) {
		return Placement{Entry: e, Mode: PlaceIdentity, NewCwd: dir, Confirmed: true}
	}
	return Placement{Entry: e, Mode: PlaceMapped, NewCwd: dir, Confirmed: true}
}

// projectCount counts the distinct projects of a manifest (by cwd, else
// by slug).
func projectCount(m *bundle.Manifest) int {
	seen := map[string]bool{}
	for i := range m.Entries {
		e := &m.Entries[i]
		key := e.Cwd
		if key == "" {
			key = "projects/" + e.Slug
		}
		seen[key] = true
	}
	return len(seen)
}

// recordForSuggest shapes the undecided entries as the import record
// rehome.Suggest reads: one session per entry with its old cwd and git
// remote, the source home for the relative-path candidate.
func recordForSuggest(m *bundle.Manifest, entries []*bundle.Entry) imports.Record {
	rec := imports.Record{BundleID: m.BundleID, Source: imports.Source{Home: m.Source.Home}}
	for _, e := range entries {
		switch e.Kind {
		case bundle.EntrySession:
			rec.Sessions = append(rec.Sessions, imports.Session{ID: e.SessionID, OldCwd: e.Cwd, OldSlug: e.Slug, GitRemote: e.GitRemote})
		case bundle.EntryMemory:
			rec.Memories = append(rec.Memories, imports.Memory{OldCwd: e.Cwd})
		}
	}
	return rec
}

// entryLabel names an entry in messages.
func entryLabel(e *bundle.Entry) string {
	if e.Kind == bundle.EntrySession {
		return "session " + short8(e.SessionID)
	}
	if e.Cwd != "" {
		return "memory for " + sanitize(e.Cwd)
	}
	return "memory/" + sanitize(e.Slug)
}

func planSession(e *bundle.Entry, dest transcripts.Root, existing []transcripts.Session, policy ConflictPolicy, place Placement, projectDirFor func(string) (string, error), warn func(string, ...any)) sessionPlan {
	sid := e.SessionID
	sp := sessionPlan{entry: e, status: planImport, slug: e.Slug, place: place}
	if place.Mode == "" {
		sp.place = Placement{Entry: e, Mode: PlaceAsIs}
	}
	if sp.place.Mode != PlaceAsIs {
		dir, err := projectDirFor(sp.place.NewCwd)
		if err != nil {
			warn("session %s: %v; importing as-is under projects/%s", short8(sid), err, e.Slug)
			sp.place = Placement{Entry: e, Mode: PlaceAsIs}
		} else {
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

// demoteToAsIs turns a mapped session plan into an as-is one when that
// is safe: nothing may already stand under the original slug, and a
// session that was going to displace an existing one under the mapped
// slug (an overwrite) cannot land elsewhere without leaving two copies.
// Reports whether the plan changed.
func (sp *sessionPlan) demoteToAsIs(dest transcripts.Root) bool {
	e := sp.entry
	if sp.slug != e.Slug {
		if sp.overwrite {
			return false
		}
		slugDir := filepath.Join(dest.Dir, e.Slug)
		if _, err := os.Lstat(filepath.Join(slugDir, e.SessionID+transcripts.TranscriptExt)); err == nil {
			return false
		}
		if hasSidecar(e) {
			if _, err := os.Lstat(filepath.Join(slugDir, e.SessionID)); err == nil {
				return false
			}
		}
	}
	sp.place = Placement{Entry: e, Mode: PlaceAsIs}
	sp.slug = e.Slug
	return true
}

func planMemory(e *bundle.Entry, o ImportOptions, dest transcripts.Root, override error, place Placement) memoryPlan {
	mp := memoryPlan{entry: e, status: planImport, place: place}
	if place.Mode == "" {
		mp.place = Placement{Entry: e, Mode: PlaceAsIs}
	}
	mp.dir = filepath.Join(dest.Dir, e.Slug, transcripts.MemorySubdir)
	if mp.place.Mode != PlaceAsIs {
		dir, err := transcripts.MemoryDirFor(dest, mp.place.NewCwd)
		switch {
		case err == nil:
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
	if memoryMode(o, mp.place) == rehome.MemorySkip {
		if info, err := os.Lstat(mp.dir); err == nil && info != nil {
			mp.status, mp.reason = planSkip, "memory directory exists (--memory merge adds to it, --memory overwrite replaces it)"
			return mp
		}
	}
	return mp
}

// memoryMode is the mode a memory entry is written with: o.Memory, else
// merge for a confirmed placement (a mapping is a confirmation) and skip
// otherwise.
func memoryMode(o ImportOptions, place Placement) rehome.MemoryMode {
	if o.Memory != "" {
		return o.Memory
	}
	if place.Confirmed {
		return rehome.MemoryMerge
	}
	return rehome.MemorySkip
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
