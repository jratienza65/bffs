package rehome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Options drives PlanRehome and Apply (plan §5.3, §9.6, §9.9, §9.10).
//
// Sessions restricts the candidates to these ids or prefixes (at least
// eight hex digits); BundleID to the sessions of that import record (a
// full id or a unique prefix). RewriteMemory rewrites old prefixes in the
// merged memory (RewriteMemoryPaths); Memory is the merge mode (MemoryMerge
// when empty — a mapping is a confirmation); SetLastSession points
// projects[<key>].lastSessionId of ClaudeJSON at the newest moved session
// of every mapped directory; ForceStamp stamps a transcript whose last
// line is incomplete. Mtime is the retention policy Apply restores
// transcript mtimes under (max(original, TranscriptFloor)); OldHome and
// NewHome are an extra rewrite pair for memory. Now is the clock (zero =
// time.Now()); DryRun makes Apply write nothing.
//
// CfgDir is the bffs home (import records for BundleID, and the parent of
// the default StagingDir); StagingDir is where Apply journals under
// rehome-<epochms>/ (<CfgDir>/staging when empty); Account is the
// BFFS_ACCOUNT prefix of the verify lines (Root.Owner when empty); Env is
// the environment claude launches with here, for transcripts.ProjectDirFor
// (nil = os.Environ()).
type Options struct {
	Sessions       []string
	BundleID       string
	RewriteMemory  bool
	SetLastSession bool
	ForceStamp     bool
	Memory         MemoryMode
	Mtime          MtimePolicy
	ClaudeJSON     string
	OldHome        string
	NewHome        string
	Now            time.Time
	DryRun         bool

	CfgDir     string
	StagingDir string
	Account    string
	Env        []string
}

// Move is one session PlanRehome decided to relocate: From and To are the
// transcript's current and future paths, NewSlug the projects/ entry it
// lands in, NewCwd the mapped directory the relocated record names.
// Sidecar says a "<sid>/" directory moves along; SameSlug that the target
// entry is the current one, so only the stamp is appended. PlanFiles are
// the plans/ files of the session (they stay where they are — plans are
// keyed by their own slug) and HistoryLines is always 0: a local session
// keeps its history.jsonl lines. OldCwd, Title and LastTS describe the
// session for the plan printout and the verify lines.
type Move struct {
	SessionID    string
	From, To     string
	NewSlug      string
	NewCwd       string
	Sidecar      bool
	SameSlug     bool
	PlanFiles    []string
	HistoryLines int

	OldCwd string
	Title  string
	LastTS time.Time
}

// Refusal is a session a mapping covers that cannot be moved, and why:
// open in a running claude, a target directory that does not exist, a
// transcript under two slugs, an incomplete last line.
type Refusal struct {
	SessionID, Reason string
}

// Plan is what a rehome would do: the sessions to move, the memory
// directories to merge, the refusals with reasons, and warnings about
// anything skipped. Mappings are the rules the plan was made with (Apply
// uses them as memory rewrite pairs).
type Plan struct {
	Root     transcripts.Root
	Moves    []Move
	Memory   []MemoryMove
	Refusals []Refusal
	Warnings []string

	Mappings []Mapping
}

// PlanRehome decides which sessions of root the prefix rules cover and
// where they go (plan §9.3, §9.6). Every session in root whose effective
// cwd a rule matches is a candidate — restricted to opts.Sessions (ids or
// prefixes) and opts.BundleID (the sessions of that import record) when
// set. Per candidate: the mapped directory must exist here (else a
// Refusal), the session must not be open in a running claude (live), its
// target entry is transcripts.ProjectDirFor(root, newCwd, env), a
// transcript with the same id under another entry is a Refusal (two hits
// make `claude --resume <sid>` ambiguous), a transcript whose last line is
// incomplete is a Refusal unless opts.ForceStamp, and a target entry equal
// to the current one makes the move a stamp-only one (SameSlug). A session
// that already belongs to its mapped directory is skipped with a warning.
// Recovery (Recover over the root, once per leftover rehome-* journal
// under the staging directory) runs first, as step 0 of every rehome —
// with DryRun too, as an import does.
//
// Memory: the memory directory of every mapped old cwd and of its
// ancestors the rules cover (memory is keyed by the git root, which may be
// above the session's cwd — planMemoryChain), plus every memory entry of
// the bundle's import record whose old cwd a rule covers, is planned into
// transcripts.MemoryDirFor(root, newCwd) with opts.Memory (MemoryMerge
// when empty — a mapping is a confirmation) when it exists on disk. The
// old directory is never removed. A session named by opts.Sessions that no
// rule covers is a warning.
func PlanRehome(ctx context.Context, root transcripts.Root, live map[string]transcripts.LiveSession, maps []Mapping, opts Options) (Plan, error) {
	if len(maps) == 0 {
		return Plan{}, errors.New("no mapping given: pass --map OLD=NEW or --into")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	p := Plan{Root: root, Mappings: maps}
	warn := func(format string, args ...any) {
		p.Warnings = append(p.Warnings, transcripts.Sanitize(fmt.Sprintf(format, args...)))
	}
	refuse := func(sid, format string, args ...any) {
		p.Refusals = append(p.Refusals, Refusal{SessionID: sid, Reason: transcripts.Sanitize(fmt.Sprintf(format, args...))})
	}

	var rec *imports.Record
	if opts.BundleID != "" {
		r, err := FindRecord(opts.CfgDir, opts.BundleID)
		if err != nil {
			return Plan{}, err
		}
		rec = &r
	}

	// Step 0 — recovery, before the listing: a sidecar an interrupted
	// rehome left under its target entry would otherwise read as a
	// collision and be refused for good.
	var restored, kept []string
	var rerr error
	if base, err := stagingBase(opts); err == nil {
		restored, kept, rerr = recoverRehomes(root, base)
	} else {
		restored, kept, rerr = Recover(root, "")
	}
	if rerr != nil {
		return Plan{}, rerr
	}
	for _, x := range restored {
		warn("recovered %s from an interrupted operation", x)
	}
	for _, x := range kept {
		warn("left %s in place (an interrupted copy)", x)
	}

	// One fast listing of the root: every transcript by id, for the
	// scope filters and the ambiguity check.
	all, err := transcripts.List(ctx, root, transcripts.ListOptions{Live: live, Now: now})
	if err != nil {
		return Plan{}, err
	}
	slugsOf := map[string][]string{}
	for _, s := range all {
		slugsOf[s.ID] = append(slugsOf[s.ID], s.Slug)
	}
	want, err := scopeIDs(all, rec, opts.Sessions, warn)
	if err != nil {
		return Plan{}, err
	}
	if len(want) == 0 {
		p.memoryFromRecord(rec, maps, opts, warn)
		return p, nil
	}
	sessions, err := transcripts.List(ctx, root, transcripts.ListOptions{IDs: want, Titles: true, Live: live, Now: now})
	if err != nil {
		return Plan{}, err
	}

	// ProjectDirFor scans every entry of the root when the target entry
	// does not exist yet — the usual case here — so ask it once per
	// mapped directory, not once per session.
	type projDir struct {
		dir string
		err error
	}
	projDirs := map[string]projDir{}
	projectDirFor := func(newCwd string) (string, error) {
		if d, ok := projDirs[newCwd]; ok {
			return d.dir, d.err
		}
		dir, err := transcripts.ProjectDirFor(root, newCwd, env)
		projDirs[newCwd] = projDir{dir, err}
		return dir, err
	}

	var memCwds []string
	seenMem := map[string]bool{}
	for _, s := range sessions {
		if s.Cwd == "" {
			warn("session %s records no working directory; skipped", short8(s.ID))
			continue
		}
		oldKey, err := transcripts.ProjectKey(s.Cwd)
		if err != nil {
			oldKey = s.Cwd
		}
		newCwd, _, ok := ApplyMappings(maps, s.Cwd, oldKey)
		if !ok {
			if len(opts.Sessions) > 0 { // named explicitly: say why nothing happens
				warn("session %s belongs to %s, which no rule covers; skipped", short8(s.ID), s.Cwd)
			}
			continue
		}
		if samePath(newCwd, s.Cwd) {
			warn("session %s already belongs to %s; skipped", short8(s.ID), newCwd)
			continue
		}
		if ls, ok := live[s.ID]; ok {
			refuse(s.ID, "open in a running claude (pid %d); close it, or use --fork-session there", ls.PID)
			continue
		}
		if s.Live {
			refuse(s.ID, "open in a running claude (being compacted or relocated); retry in a minute")
			continue
		}
		if !dirExists(newCwd) {
			refuse(s.ID, "target directory %q does not exist", newCwd)
			continue
		}
		dir, err := projectDirFor(newCwd)
		if err != nil {
			refuse(s.ID, "no projects/ entry for %q: %v", newCwd, err)
			continue
		}
		newSlug := filepath.Base(dir)
		if transcripts.IsReserved(newSlug) {
			refuse(s.ID, "projects/%s is not a usable entry", newSlug)
			continue
		}
		if other, ok := otherSlug(slugsOf[s.ID], s.Slug); ok {
			refuse(s.ID, "also exists under projects/%s; two copies would make claude --resume ambiguous", other)
			continue
		}
		sameSlug := canonicalPath(dir) == canonicalPath(filepath.Join(root.Dir, s.Slug))
		if !sameSlug {
			if _, err := os.Lstat(filepath.Join(dir, s.ID)); err == nil {
				refuse(s.ID, "projects/%s/%s already exists", newSlug, s.ID)
				continue
			}
		}
		ok, err = CheckLastLine(s.Path)
		if err != nil {
			refuse(s.ID, "%v", err)
			continue
		}
		if !ok && !opts.ForceStamp {
			refuse(s.ID, "incomplete last line (crashed session?); resume it once in claude or pass --force-stamp")
			continue
		}
		mv := Move{
			SessionID: s.ID,
			From:      s.Path,
			To:        filepath.Join(dir, s.ID+transcripts.TranscriptExt),
			NewSlug:   newSlug,
			NewCwd:    newCwd,
			SameSlug:  sameSlug,
			PlanFiles: transcripts.ArtifactsFor(root, s).PlanFiles,
			OldCwd:    s.Cwd,
			Title:     s.Title,
			LastTS:    s.LastTS,
		}
		if info, err := os.Lstat(s.SidecarDir); err == nil && info.IsDir() {
			mv.Sidecar = true
		}
		p.Moves = append(p.Moves, mv)
		if !seenMem[oldKey] {
			seenMem[oldKey] = true
			memCwds = append(memCwds, s.Cwd)
		}
	}

	mode := opts.Memory
	if mode == "" {
		mode = MemoryMerge
	}
	for _, cwd := range memCwds {
		if !p.planMemoryChain(root, maps, cwd, mode, memoryID8(rec, cwd), warn) {
			break
		}
	}
	p.memoryFromRecord(rec, maps, opts, warn)
	return p, nil
}

// maxMemoryAncestors bounds planMemoryChain's walk up a recorded path.
const maxMemoryAncestors = 64

// planMemoryChain plans the memory directory of cwd and of every ancestor
// the rules still cover. Auto-memory is keyed by the git root, not the
// session's cwd (disk.md §5): a session that ran in a subdirectory of its
// repository keeps its memory under the root's slug — a directory that
// may no longer exist here, so git cannot name it and the walk goes by
// prefix instead. Every ancestor a rule covers maps under that same rule,
// so each existing memory directory lands in the memory of its own mapped
// counterpart (addMemory dedups by source). A mapped ancestor that does
// not exist here is a warning. Returns false when memory placement is
// overridden for this root — nothing more can be planned.
func (p *Plan) planMemoryChain(root transcripts.Root, maps []Mapping, cwd string, mode MemoryMode, id8 string, warn func(string, ...any)) bool {
	dir := cwd
	for range maxMemoryAncestors {
		if dir == "" {
			return true
		}
		newDir, _, ok := ApplyMappings(maps, dir, dir)
		if !ok {
			return true
		}
		from, err := transcripts.MemoryDirFor(root, dir)
		switch {
		case errors.Is(err, transcripts.ErrMemoryDirOverridden):
			warn("memory not merged: %v; bffs cannot place memory there yet", err)
			return false
		case err != nil:
			warn("memory of %s not merged: %v", dir, err)
		case dirExists(from) && !dirExists(newDir):
			warn("memory of %s not merged: %s does not exist here", dir, newDir)
		case dirExists(from):
			p.addMemory(from, dir, newDir, mode, id8, warn)
		}
		dir = parentDir(dir)
	}
	return true
}

// parentDir is the parent of a recorded directory in either separator
// convention: "/a/b" → "/a", "/x" → "/", `C:\x` → `C:\`; "" at a root.
func parentDir(p string) string {
	trimmed := strings.TrimRight(p, `/\`)
	i := strings.LastIndexAny(trimmed, `/\`)
	switch {
	case trimmed == "" || i < 0:
		return ""
	case i == 0:
		return p[:1]
	case i == 2 && isWindowsAbs(p):
		return p[:3]
	}
	return trimmed[:i]
}

// memoryFromRecord plans the memory directories the import record lists
// whose old cwd a rule covers — the case of a session that ran in a
// subdirectory of its repository, whose memory slug bffs cannot derive
// from a directory that does not exist here.
func (p *Plan) memoryFromRecord(rec *imports.Record, maps []Mapping, opts Options, warn func(string, ...any)) {
	if rec == nil {
		return
	}
	mode := opts.Memory
	if mode == "" {
		mode = MemoryMerge
	}
	for _, m := range rec.Memories {
		if m.Status == imports.StatusSkipped || m.Dir == "" || m.OldCwd == "" {
			continue
		}
		newCwd, _, ok := ApplyMappings(maps, m.OldCwd, m.OldCwd)
		if !ok || !dirExists(newCwd) || samePath(newCwd, m.OldCwd) {
			continue
		}
		p.addMemory(m.Dir, m.OldCwd, newCwd, mode, memoryID8(rec, m.OldCwd), warn)
	}
}

// addMemory appends one MemoryMove unless the source is missing, equals
// the destination, or was planned already.
func (p *Plan) addMemory(from, oldCwd, newCwd string, mode MemoryMode, id8 string, warn func(string, ...any)) {
	if !dirExists(from) {
		return
	}
	to, err := transcripts.MemoryDirFor(p.Root, newCwd)
	if err != nil {
		warn("memory of %s not merged: %v", oldCwd, err)
		return
	}
	if canonicalPath(from) == canonicalPath(to) {
		return
	}
	for _, m := range p.Memory {
		if canonicalPath(m.From) == canonicalPath(from) {
			return
		}
	}
	p.Memory = append(p.Memory, MemoryMove{From: from, To: to, OldCwd: oldCwd, ID8: id8, Mode: mode})
}

// FindRecord loads the import record under cfgDir named by a bundle id or
// a unique prefix of at least eight characters — the --bundle argument of
// `bffs rehome`.
func FindRecord(cfgDir, id string) (imports.Record, error) {
	if cfgDir == "" {
		return imports.Record{}, errors.New("--bundle needs the bffs config dir (Options.CfgDir)")
	}
	q := strings.ToLower(strings.TrimSpace(id))
	if len(q) < 8 {
		return imports.Record{}, fmt.Errorf("bundle id %q is too short; give at least eight characters", id)
	}
	recs, err := imports.Load(cfgDir)
	if err != nil {
		return imports.Record{}, err
	}
	var hits []imports.Record
	for _, r := range recs {
		if strings.HasPrefix(strings.ToLower(r.BundleID), q) {
			hits = append(hits, r)
		}
	}
	switch len(hits) {
	case 0:
		return imports.Record{}, fmt.Errorf("no import record for bundle %q (bffs sessions imports)", id)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, 0, len(hits))
	for _, r := range hits {
		ids = append(ids, short8(r.BundleID))
	}
	return imports.Record{}, fmt.Errorf("bundle id %q is ambiguous: %s", id, strings.Join(ids, ", "))
}

// scopeIDs returns the ids of all that the scope filters admit: the
// record's sessions when rec is set, and the ids or prefixes in sessions
// when given (a prefix that matches nothing is a warning). No filter
// admits every id.
func scopeIDs(all []transcripts.Session, rec *imports.Record, sessions []string, warn func(string, ...any)) ([]string, error) {
	inRec := map[string]bool{}
	if rec != nil {
		for _, s := range rec.Sessions {
			if s.Status != imports.StatusSkipped && s.ID != "" {
				inRec[strings.ToLower(s.ID)] = true
			}
		}
	}
	var prefixes []string
	for _, q := range sessions {
		q = strings.ToLower(strings.TrimSpace(q))
		if len(strings.ReplaceAll(q, "-", "")) < 8 || strings.Trim(q, "0123456789abcdef-") != "" {
			return nil, fmt.Errorf("invalid session id %q: expected a UUID or at least eight hex digits of one", q)
		}
		prefixes = append(prefixes, q)
	}
	matched := make([]bool, len(prefixes))
	var out []string
	seen := map[string]bool{}
	for _, s := range all {
		id := strings.ToLower(s.ID)
		if seen[id] {
			continue
		}
		if rec != nil && !inRec[id] {
			continue
		}
		if len(prefixes) > 0 {
			hit := false
			for i, q := range prefixes {
				if strings.HasPrefix(id, q) {
					matched[i] = true
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		seen[id] = true
		out = append(out, s.ID)
	}
	for i, q := range prefixes {
		if !matched[i] {
			warn("session %q not found in this root", q)
		}
	}
	return out, nil
}

// memoryID8 is the side-file suffix for a memory move: the bundle id
// prefix when the rehome is scoped to an import record, else eight hex
// digits of sha256(oldCwd).
func memoryID8(rec *imports.Record, oldCwd string) string {
	if rec != nil && len(rec.BundleID) >= 8 {
		if err := validateID8(rec.BundleID[:8]); err == nil {
			return rec.BundleID[:8]
		}
	}
	sum := sha256.Sum256([]byte(oldCwd))
	return hex.EncodeToString(sum[:4])
}

// otherSlug returns a slug in slugs other than cur.
func otherSlug(slugs []string, cur string) (string, bool) {
	for _, s := range slugs {
		if s != cur {
			return s, true
		}
	}
	return "", false
}

// samePath reports whether two directories normalise to one path.
func samePath(a, b string) bool {
	na, err := store.NormalizePath(a)
	if err != nil {
		return false
	}
	nb, err := store.NormalizePath(b)
	if err != nil {
		return false
	}
	return na == nb
}

// canonicalPath resolves symlinks when the path exists and cleans it
// otherwise.
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

// short8 is the eight-character prefix of an id for messages.
func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
