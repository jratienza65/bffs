package porter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// ImportOptions drives Import. Dest is the destination root (from
// transcripts.Roots/RootFor; orphans are refused); Account the account
// chosen for the import (recorded, the BFFS_ACCOUNT prefix of the verify
// line when the root has no owner, and the owner of the .claude.json
// --carry-trust and --set-last-session write); OnConflict, Memory (skip,
// overwrite or merge; empty = merge for a confirmed placement, skip
// otherwise), TrustMemory, AsIs, PreserveMtimes, Force, DryRun, Limits
// (zero = bundle.DefaultLimits), StagingDir (default
// <cfgDir>/staging/<bundle-id>), ExpectManifestSHA256 (the LAN path), Now
// (zero = time.Now()), Progress, Live (transcripts.Live of the destination
// config dirs) and LaunchEnv (the environment claude is launched with
// here, for transcripts.ProjectDirFor) are honoured.
//
// Placement (plan §9.3): Map holds prefix rules (rehome.ApplyMappings) —
// a derived directory that does not exist here falls back to as-is with
// a warning; Into is the single-project shorthand (an error when the
// bundle holds more than one project, or when both are given); Place is
// asked about every entry no rule or identity decided. CarryTrust copies
// the source's three trust answers into the account's .claude.json for
// confirmed mapped placements only (an error with AsIs); SetLastSession
// points lastSessionId at the newest landed session of every directory;
// NoRewriteMemory keeps old-machine paths in merged memory as they are;
// ForceStamp stamps a mapped transcript whose last line is torn (a live
// session exported mid-write); without it such a session lands as-is,
// pending, with a warning (plan §9.8). ExcludeRoot is accepted and
// ignored. StreamMode says the reader stays open after the
// archive (a LAN body), so its end is not required.
type ImportOptions struct {
	Dest                 transcripts.Root
	Account              string
	OnConflict           ConflictPolicy
	Memory               rehome.MemoryMode
	TrustMemory          bool
	Map                  []rehome.Mapping
	Into                 string
	AsIs                 bool
	Place                Placer
	CarryTrust           bool
	SetLastSession       bool
	PreserveMtimes       bool
	Force                bool
	ForceStamp           bool
	NoRewriteMemory      bool
	DryRun               bool
	Limits               bundle.Limits
	StagingDir           string
	ExpectManifestSHA256 string
	Now                  time.Time
	Progress             func(bundle.Progress)
	Live                 map[string]transcripts.LiveSession
	ExcludeRoot          string
	LaunchEnv            []string
	StreamMode           bool
}

// Report is what Import did (or, with DryRun, would do). Session lists
// hold session ids: Imported landed under a directory of this machine —
// by identity, or mapped with a relocated record (Rehomed, a subset of
// Imported, lists the mapped ones) — Pending as-is, Overwritten (a subset
// of Imported and Pending) displaced an existing session, Skipped were not
// written, Held were refused because a running claude owns them; Reasons
// explains every Skipped and Held id. MemoryDirs are the memory
// directories written and MemoryReview, keyed the same way, how many
// lines of each still mention absolute paths from the source machine
// (`bffs memory scan-paths`); a directory with none is absent. MtimeRaised
// lists the sessions whose transcript mtime was raised to the retention
// floor; SweepDate is when Claude will sweep them unless resumed and
// SweptCount how many. TrustCarried lists the project keys whose trust
// answers were carried (--carry-trust) and LastSession maps each
// directory to the session lastSessionId now points at
// (--set-last-session). Verify holds one rehome.VerifyCommand line per
// distinct destination directory. Bytes is the size of the bundle's
// payload. StagingDir is set only when the staging directory was kept
// after a failure.
type Report struct {
	BundleID     string
	Imported     []string
	Rehomed      []string
	Skipped      []string
	Overwritten  []string
	Pending      []string
	Held         []string
	Reasons      map[string]string
	MemoryDirs   []string
	MemoryReview map[string]int
	MtimeRaised  []string
	SweepDate    time.Time
	SweptCount   int
	HistoryLines int
	TrustCarried []string
	LastSession  map[string]string
	Warnings     []string
	Verify       []string
	Bytes        int64
	StagingDir   string
}

// commitSession is rehome.CommitSession behind a variable so tests can
// inject a failure for one session and watch the rollback.
var commitSession = rehome.CommitSession

// Import reads a bundle from r and lands it in o.Dest under plan §9.1:
//
//	0 rehome.Recover over the destination and every leftover staging dir
//	1–2 envelope + manifest (bundle.PeekManifest, Validate, the expected
//	    manifest sha256, the bundle_id collision check against the import
//	    records — refused without Force)
//	3 preflight: destination sanity by identity, placement (identity,
//	    mapped or as-is — plan §9.3), collision scan scoped to the
//	    destination root, liveness, memory-dir override; DryRun returns
//	    the plan here
//	4 staging under <cfgDir>/staging/<bundle-id>/ (bundle.Unpack)
//	5 completeness: every staged transcript's head sessionId equals its name
//	6 commit: memories (rehome.MergeMemory, old paths rewritten for
//	    mapped placements), then sessions one by one (rehome.CommitSession,
//	    journalled; a mapped session is stamped with a relocated record),
//	    then the import record, trust carry and lastSessionId
//	7 report and cleanup
//
// A failure in steps 0–5 removes the staging directory Import created and
// leaves the destination untouched. A failure inside step 6 rolls back the
// session in flight; sessions already committed stay, the staging
// directory is kept (Report.StagingDir), the record lists what landed,
// and the Report is returned alongside the error.
func Import(ctx context.Context, cfgDir string, r io.Reader, o ImportOptions) (Report, error) {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	if err := validateImportOptions(o); err != nil {
		return Report{}, err
	}
	limits := o.Limits
	if limits == (bundle.Limits{}) {
		limits = bundle.DefaultLimits
	}
	dest, err := checkDestination(cfgDir, o.Dest)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Reasons: map[string]string{}}
	warn := func(format string, args ...any) {
		rep.Warnings = append(rep.Warnings, sanitize(fmt.Sprintf(format, args...)))
	}

	// 0 — recover.
	stale, err := StaleStaging(cfgDir)
	if err != nil {
		return rep, err
	}
	restored, kept, err := recoverAll(dest, stale)
	if err != nil {
		return rep, err
	}
	for _, p := range restored {
		warn("recovered %s from an interrupted import", p)
	}
	for _, p := range kept {
		warn("left %s in place (an interrupted copy; a session with that id already exists or its source is still staged)", p)
	}
	for _, p := range stale {
		warn("leftover staging directory %s (remove with --clean-staging)", p)
	}

	// 1–2 — envelope and manifest.
	m, raw, rest, err := bundle.PeekManifest(r)
	if err != nil {
		return rep, fmt.Errorf("bundle: %w", err)
	}
	if err := m.Validate(limits); err != nil {
		return rep, fmt.Errorf("bundle: %w", err)
	}
	if o.ExpectManifestSHA256 != "" {
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, o.ExpectManifestSHA256) {
			return rep, fmt.Errorf("bundle: manifest sha256 %s does not match the expected %s", got, sanitize(o.ExpectManifestSHA256))
		}
	}
	rep.BundleID = m.BundleID
	rep.Bytes = m.Totals.Bytes
	if rec, ok := imports.Exists(cfgDir, m.BundleID); ok && !o.Force {
		return rep, fmt.Errorf("bundle %s was imported on %s into %s; --force to import again", short8(m.BundleID), rec.ImportedAt.Format("2006-01-02"), rec.DestRoot)
	}

	// 3 — preflight.
	plan, err := planEntries(ctx, m, o, dest, now)
	if err != nil {
		return rep, err
	}
	rep.Warnings = append(rep.Warnings, plan.warnings...)
	days, _ := transcripts.CleanupPeriodDays(dest.ConfigDir)
	policy := rehome.MtimePolicy{CleanupPeriodDays: days, Now: now, PreserveSidecars: o.PreserveMtimes}
	if floor, ok := rehome.TranscriptFloor(policy); ok {
		rep.SweepDate = floor.Add(time.Duration(days) * 24 * time.Hour)
	}
	for _, sp := range plan.sessions {
		rep.note(sp)
	}
	for _, mp := range plan.memories {
		if mp.status == planSkip {
			warn("memory for %s not imported: %s", memoryLabel(mp), mp.reason)
		}
	}
	if o.DryRun {
		for _, sp := range plan.sessions {
			if sp.status == planImport {
				rep.record(sp)
			}
		}
		for _, mp := range plan.memories {
			if mp.status == planImport {
				rep.MemoryDirs = append(rep.MemoryDirs, mp.dir)
			}
		}
		rep.Verify = verifyLines(cfgDir, dest, o.Account, plan.sessions, func(sp sessionPlan) bool { return sp.status == planImport })
		return rep, nil
	}

	// 4 — staging.
	stagingDir := o.StagingDir
	if stagingDir == "" {
		stagingDir = StagingDir(cfgDir, m.BundleID)
	}
	if err := createStaging(stagingDir); err != nil {
		return rep, err
	}
	discard := func() { _ = os.RemoveAll(stagingDir) }
	up, err := bundle.Unpack(ctx, rest, stagingDir, o.ExpectManifestSHA256, limits, !o.StreamMode, o.Progress)
	if err != nil {
		discard()
		return rep, fmt.Errorf("bundle: %w", err)
	}
	for _, n := range up.Notes {
		warn("%s", n)
	}
	staged := func(p string) string { return filepath.Join(stagingDir, filepath.FromSlash(p)) }

	// 5 — completeness beyond the allow-list, and the stamp check.
	for i := range plan.sessions {
		sp := &plan.sessions[i]
		if sp.status != planImport {
			continue
		}
		sid := sp.entry.SessionID
		tr := staged(joinSlash("projects", sp.entry.Slug, sid+transcripts.TranscriptExt))
		head, err := transcripts.ReadHead(tr)
		if err != nil {
			discard()
			return rep, fmt.Errorf("bundle: staged transcript for %s: %w", sid, err)
		}
		if head.SessionID != sid {
			discard()
			return rep, fmt.Errorf("bundle: transcript projects/%s/%s.jsonl records sessionId %q, not its file name; refusing the bundle", sanitize(sp.entry.Slug), sid, sanitize(head.SessionID))
		}
		// A mapped session gets a relocated record appended. A torn last
		// line — a live session exported mid-write — is not stamped over
		// unless ForceStamp (plan §9.8): the session lands as-is instead,
		// pending, and `bffs rehome --force-stamp` can move it later.
		if sp.place.Mode != PlaceMapped || o.ForceStamp {
			continue
		}
		if ok, err := rehome.CheckLastLine(tr); err != nil || !ok {
			reason := "incomplete last line (exported from a running claude?)"
			if err != nil {
				reason = err.Error()
			}
			if sp.demoteToAsIs(dest) {
				warn("session %s: %s; imported as-is under projects/%s, pending — export it again once it is closed, or move it with bffs rehome --bundle %s --force-stamp", short8(sid), reason, sanitize(sp.slug), short8(m.BundleID))
				continue
			}
			sp.status, sp.reason = planSkip, reason+"; pass --force-stamp to stamp it anyway"
			rep.note(*sp)
		}
	}

	// 6 — commit.
	id8 := m.BundleID[:8]
	root, err := os.OpenRoot(dest.ConfigDir)
	if err != nil {
		discard()
		return rep, fmt.Errorf("open destination %s: %w", dest.ConfigDir, err)
	}
	defer root.Close()

	rec := newRecord(m, o, dest, plan, now)
	fail := func(err error) (Report, error) {
		rep.StagingDir = stagingDir
		if len(rep.Imported)+len(rep.Pending)+len(rep.MemoryDirs) > 0 {
			if serr := imports.Save(cfgDir, rec); serr != nil {
				warn("import record not written: %v", serr)
			}
		}
		return rep, fmt.Errorf("%w; staging kept at %s", err, stagingDir)
	}

	localHome, _ := os.UserHomeDir()
	for i, mp := range plan.memories {
		if mp.status != planImport {
			rec.Memories[i].Status = imports.StatusSkipped
			continue
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		srcDir := staged(joinSlash("memory", mp.entry.Slug))
		mv, err := rehome.MergeMemory(mp.dir, srcDir, memoryMode(o, mp.place), id8, mp.place.Confirmed, o.TrustMemory, now)
		if err != nil {
			return fail(fmt.Errorf("memory for %s: %w", memoryLabel(mp), err))
		}
		for _, w := range mv.Warnings {
			warn("memory for %s: %s", memoryLabel(mp), w)
		}
		if len(mv.Added)+len(mv.Renamed) == 0 {
			rec.Memories[i].Status = imports.StatusSkipped
			warn("memory for %s: nothing to write", memoryLabel(mp))
			continue
		}
		rec.Memories[i].Status = placedStatus(mp.place)
		rep.MemoryDirs = append(rep.MemoryDirs, mp.dir)
		if mp.place.Mode == PlaceMapped && mp.place.Confirmed && !o.NoRewriteMemory {
			pairs := rewritePairs(o.Map, mp.entry.Cwd, mp.place.NewCwd, m.Source.Home, localHome)
			if _, remaining, err := rehome.RewriteMemoryPaths(mp.dir, pairs); err != nil {
				warn("memory for %s: old paths not rewritten: %v", memoryLabel(mp), err)
			} else {
				mv.Remaining = remaining
			}
		}
		if n := len(mv.Remaining); n > 0 {
			if rep.MemoryReview == nil {
				rep.MemoryReview = map[string]int{}
			}
			rep.MemoryReview[mp.dir] = n
			warn("%s: %d memory line(s) mention absolute paths from the source machine (bffs memory scan-paths)", mp.dir, n)
		}
	}

	for i, sp := range plan.sessions {
		if sp.status != planImport {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		sid := sp.entry.SessionID
		req, err := commitRequest(sp, staged, id8)
		if err != nil {
			return fail(err)
		}
		journal := func(step string) error {
			return rehome.WriteJournal(stagingDir, rehome.Journal{
				BundleID:  m.BundleID,
				SessionID: sid,
				Origin:    req.Transcript,
				Target:    filepath.Join(dest.Dir, sp.slug, sid+transcripts.TranscriptExt),
				Step:      step,
			})
		}
		var aside *setAside
		if sp.overwrite {
			aside, err = setAsideSession(root, sp, now)
			if err != nil {
				return fail(fmt.Errorf("session %s: %w", sid, err))
			}
		}
		res, err := commitSession(ctx, root, req, policy, journal)
		if err != nil {
			if aside != nil {
				if rerr := aside.restore(root); rerr != nil {
					warn("session %s: set-aside not restored: %v", sid, rerr)
				}
			}
			return fail(err)
		}
		rep.record(sp)
		rep.Warnings = append(rep.Warnings, res.Warnings...)
		if res.MtimeRaised {
			rep.MtimeRaised = append(rep.MtimeRaised, sid)
		}
		rep.HistoryLines += res.HistoryAdded
		rec.Sessions[i].Status = placedStatus(sp.place)
	}
	rep.SweptCount = len(rep.MtimeRaised)
	if err := imports.Save(cfgDir, rec); err != nil {
		return fail(fmt.Errorf("import record: %w", err))
	}
	landed := map[string]bool{}
	for _, sid := range rep.Imported {
		landed[sid] = true
	}
	if carry := len(trustTargets(plan.sessions, landed)) > 0; carry || o.SetLastSession {
		jsonPath, err := accountJSON(cfgDir, o, dest)
		if err != nil {
			warn("trust and lastSessionId not written: %v", err)
		} else {
			if carry {
				rep.TrustCarried = carryTrust(jsonPath, stagingDir, plan.sessions, landed, warn)
			}
			if o.SetLastSession {
				rep.LastSession = setLastSessions(jsonPath, plan.sessions, landed, warn)
			}
		}
	}

	// 7 — report.
	rep.Verify = verifyLines(cfgDir, dest, o.Account, plan.sessions, func(sp sessionPlan) bool {
		return sp.status == planImport
	})
	if err := os.RemoveAll(stagingDir); err != nil {
		warn("staging directory %s not removed: %v", stagingDir, err)
		rep.StagingDir = stagingDir
	}
	return rep, nil
}

// note classifies a planned-but-not-imported session in the report.
func (rep *Report) note(sp sessionPlan) {
	sid := sp.entry.SessionID
	switch sp.status {
	case planSkip:
		rep.Skipped = append(rep.Skipped, sid)
		rep.Reasons[sid] = sanitize(sp.reason)
	case planHeld:
		rep.Held = append(rep.Held, sid)
		rep.Reasons[sid] = sanitize(sp.reason)
	}
}

// record lists a committed (or, dry-run, planned) session.
func (rep *Report) record(sp sessionPlan) {
	sid := sp.entry.SessionID
	switch sp.place.Mode {
	case PlaceIdentity:
		rep.Imported = append(rep.Imported, sid)
	case PlaceMapped:
		rep.Imported = append(rep.Imported, sid)
		rep.Rehomed = append(rep.Rehomed, sid)
	default:
		rep.Pending = append(rep.Pending, sid)
	}
	if sp.overwrite {
		rep.Overwritten = append(rep.Overwritten, sid)
	}
}

func validateImportOptions(o ImportOptions) error {
	switch o.OnConflict {
	case "", ConflictSkip, ConflictOverwrite:
	default:
		return fmt.Errorf("invalid --on-conflict %q: must be %q or %q", string(o.OnConflict), ConflictSkip, ConflictOverwrite)
	}
	switch o.Memory {
	case "", rehome.MemorySkip, rehome.MemoryOverwrite, rehome.MemoryMerge:
	default:
		return fmt.Errorf("invalid --memory %q: must be %q, %q or %q", string(o.Memory), rehome.MemorySkip, rehome.MemoryOverwrite, rehome.MemoryMerge)
	}
	if o.Into != "" && len(o.Map) > 0 {
		return errors.New("--into and --map are mutually exclusive")
	}
	if o.CarryTrust && o.AsIs {
		return errors.New("--carry-trust needs a mapped placement (--map, --into or an interactive answer); it does nothing with --as-is")
	}
	return nil
}

// checkDestination verifies dest by identity (plan §9.2): it must be a
// root transcripts.Roots knows for this bffs home — or the home root of a
// config dir the caller chose — never an orphan; its config dir must
// exist, and its projects/ directory must exist or be creatable directly
// under the config dir.
func checkDestination(cfgDir string, dest transcripts.Root) (transcripts.Root, error) {
	if dest.Dir == "" || dest.ConfigDir == "" {
		return transcripts.Root{}, errors.New("destination root is not set")
	}
	if dest.Orphan {
		return transcripts.Root{}, fmt.Errorf("destination %s belongs to no account (orphan session dir); orphans are never destinations", dest.ConfigDir)
	}
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return transcripts.Root{}, err
	}
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return transcripts.Root{}, err
	}
	homeClaudeDir := ""
	if dest.Owner == "" {
		homeClaudeDir = dest.ConfigDir
	}
	roots, err := transcripts.Roots(cfgDir, homeClaudeDir, accs, state)
	if err != nil {
		return transcripts.Root{}, err
	}
	found := false
	for _, r := range roots {
		if r.Orphan {
			continue
		}
		if canonicalPath(r.Dir) == canonicalPath(dest.Dir) && canonicalPath(r.ConfigDir) == canonicalPath(dest.ConfigDir) {
			found = true
			break
		}
	}
	if !found {
		return transcripts.Root{}, fmt.Errorf("destination %s is not a Claude config dir bffs knows", dest.ConfigDir)
	}
	if !dirExists(dest.ConfigDir) {
		return transcripts.Root{}, fmt.Errorf("destination config dir %s does not exist", dest.ConfigDir)
	}
	if !dirExists(dest.Dir) && canonicalPath(filepath.Dir(dest.Dir)) != canonicalPath(dest.ConfigDir) {
		return transcripts.Root{}, fmt.Errorf("destination %s does not exist and is not directly under %s", dest.Dir, dest.ConfigDir)
	}
	return dest, nil
}

// recoverAll runs rehome.Recover over dest once per leftover staging
// directory (each journal informs one session) — once with no journal
// when there is none.
func recoverAll(dest transcripts.Root, stale []string) (restored, kept []string, err error) {
	dirs := stale
	if len(dirs) == 0 {
		dirs = []string{""}
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		r, k, err := rehome.Recover(dest, d)
		if err != nil {
			return restored, kept, err
		}
		for _, p := range r {
			if !seen[p] {
				seen[p] = true
				restored = append(restored, p)
			}
		}
		for _, p := range k {
			if !seen[p] {
				seen[p] = true
				kept = append(kept, p)
			}
		}
	}
	return restored, kept, nil
}

// commitRequest assembles the CommitRequest of a planned session from the
// staged files.
func commitRequest(sp sessionPlan, staged func(string) string, id8 string) (rehome.CommitRequest, error) {
	e := sp.entry
	sid := e.SessionID
	req := rehome.CommitRequest{
		SessionID:  sid,
		Slug:       sp.slug,
		Transcript: staged(joinSlash("projects", e.Slug, sid+transcripts.TranscriptExt)),
		ID8:        id8,
	}
	if sp.place.Mode == PlaceMapped {
		req.Stamp = rehome.RelocatedRecord(sid, sp.place.NewCwd)
		req.HistoryProject = sp.place.NewCwd
	}
	var history string
	for _, f := range e.Files {
		kind, _, _, err := bundle.ClassifyName(f.Path)
		if err != nil {
			return rehome.CommitRequest{}, fmt.Errorf("session %s: %w", sid, err)
		}
		switch kind {
		case bundle.NameSidecar:
			req.SidecarDir = staged(joinSlash("projects", e.Slug, sid))
		case bundle.NameFileHistory:
			req.FileHistory = append(req.FileHistory, staged(f.Path))
		case bundle.NamePlans:
			req.Plans = append(req.Plans, staged(f.Path))
		case bundle.NameTasks:
			req.TasksDir = staged(joinSlash("tasks", sid))
		case bundle.NameHistory:
			history = staged(f.Path)
		case bundle.NameTranscript:
			req.OrigMtime = f.ModTime
		}
	}
	if history != "" {
		lines, err := readLines(history)
		if err != nil {
			return rehome.CommitRequest{}, fmt.Errorf("session %s: %w", sid, err)
		}
		req.History = lines
	}
	return req, nil
}

// readLines returns the newline-separated lines of path, without their
// terminators and without empty lines.
func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, rerr := r.ReadBytes('\n')
		if t := bytes.TrimRight(line, "\r\n"); len(bytes.TrimSpace(t)) > 0 {
			out = append(out, append([]byte(nil), t...))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return out, nil
			}
			return nil, rerr
		}
	}
}

// setAside is an existing session moved out of the way for an overwrite:
// the paths (relative to the config dir) it was renamed from and to.
type setAside struct {
	moves [][2]string
}

// setAsideSession renames the existing transcript and sidecar of the
// session sp displaces to "<name>.bffs-replaced-<epochms>" through root.
// Nothing is deleted; a set-aside name that is already taken moves the
// stamp forward one millisecond at a time.
func setAsideSession(root *os.Root, sp sessionPlan, now time.Time) (*setAside, error) {
	sid := sp.entry.SessionID
	slugDir := filepath.Join(transcripts.ProjectsSubdir, sp.slug)
	names := []string{sid + transcripts.TranscriptExt}
	if sp.sidecarExists {
		names = append(names, sid)
	}
	a := &setAside{}
	for _, name := range names {
		from := filepath.Join(slugDir, name)
		if _, err := root.Lstat(from); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var to string
		stamp := now
		for tries := 0; ; tries++ {
			to = filepath.Join(slugDir, rehome.SetAsideName(name, stamp))
			if _, err := root.Lstat(to); errors.Is(err, fs.ErrNotExist) {
				break
			}
			if tries > 10_000 {
				return nil, fmt.Errorf("no free set-aside name for %s", filepath.ToSlash(from))
			}
			stamp = stamp.Add(time.Millisecond)
		}
		if err := root.Rename(from, to); err != nil {
			_ = a.restore(root)
			return nil, fmt.Errorf("set aside %s: %w", filepath.ToSlash(from), err)
		}
		a.moves = append(a.moves, [2]string{from, to})
	}
	return a, nil
}

// restore puts the set-aside entries back when the commit that displaced
// them failed; it never renames over something that appeared meanwhile.
func (a *setAside) restore(root *os.Root) error {
	var errs []error
	for i := len(a.moves) - 1; i >= 0; i-- {
		from, to := a.moves[i][0], a.moves[i][1]
		if _, err := root.Lstat(from); err == nil {
			errs = append(errs, fmt.Errorf("%s reappeared; %s kept", filepath.ToSlash(from), filepath.ToSlash(to)))
			continue
		}
		if err := root.Rename(to, from); err != nil {
			errs = append(errs, err)
		}
	}
	a.moves = nil
	return errors.Join(errs...)
}

// newRecord starts the import record for m from the preflight plan:
// every session and memory in manifest order, initially skipped, updated
// as entries land. Sessions and memories carry the slug and directory the
// plan chose, so a pending session records its original slug and an
// identity placement the directory it was placed against.
func newRecord(m *bundle.Manifest, o ImportOptions, dest transcripts.Root, plan importPlan, now time.Time) imports.Record {
	rec := imports.Record{
		BundleID:   m.BundleID,
		Kind:       imports.KindImport,
		ImportedAt: now,
		Account:    o.Account,
		DestRoot:   dest.Dir,
		Source: imports.Source{
			Hostname:      m.Source.Hostname,
			User:          m.Source.User,
			Home:          m.Source.Home,
			OS:            m.Source.OS,
			Account:       m.Source.Account,
			BFFSVersion:   m.BFFSVersion,
			ClaudeVersion: m.ClaudeVersion,
		},
	}
	for _, sp := range plan.sessions {
		e := sp.entry
		s := imports.Session{ID: e.SessionID, OldCwd: e.Cwd, OldSlug: e.Slug, NewCwd: sp.place.NewCwd, Slug: sp.slug, Title: e.Title, GitRemote: e.GitRemote, Status: imports.StatusSkipped}
		for _, f := range e.Files {
			if kind, _, _, err := bundle.ClassifyName(f.Path); err == nil && kind == bundle.NameTranscript {
				s.OrigMtime = f.ModTime
			}
		}
		rec.Sessions = append(rec.Sessions, s)
	}
	for _, mp := range plan.memories {
		rec.Memories = append(rec.Memories, imports.Memory{OldCwd: mp.entry.Cwd, Dir: mp.dir, Status: imports.StatusSkipped})
	}
	rec.Mapping = effectiveMappings(o, plan)
	return rec
}

// effectiveMappings lists the prefix rules the placement used: every
// --map rule, plus one old→new pair per confirmed mapped directory the
// rules did not produce (--into, an interactive answer).
func effectiveMappings(o ImportOptions, plan importPlan) []imports.Mapping {
	out := append([]imports.Mapping(nil), o.Map...)
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Old+"\x00"+m.New] = true
	}
	add := func(p Placement) {
		if p.Mode != PlaceMapped || !p.Confirmed || p.Entry == nil || p.Entry.Cwd == "" {
			return
		}
		if newCwd, _, ok := rehome.ApplyMappings(o.Map, p.Entry.Cwd, p.Entry.ProjectKey); ok && newCwd == p.NewCwd {
			return
		}
		key := p.Entry.Cwd + "\x00" + p.NewCwd
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, imports.Mapping{Old: p.Entry.Cwd, New: p.NewCwd})
	}
	for _, sp := range plan.sessions {
		add(sp.place)
	}
	for _, mp := range plan.memories {
		add(mp.place)
	}
	return out
}

// placedStatus is the import-record status of a landed entry.
func placedStatus(p Placement) string {
	switch p.Mode {
	case PlaceIdentity:
		return imports.StatusPlaced
	case PlaceMapped:
		return imports.StatusRehomed
	}
	return imports.StatusPending
}

// rewritePairs builds the [old, new] prefix pairs a mapped memory
// directory is rewritten with: every --map rule, the entry's own old→new
// directory, and the source home → this home.
func rewritePairs(maps []rehome.Mapping, oldCwd, newCwd, oldHome, newHome string) [][2]string {
	var pairs [][2]string
	for _, m := range maps {
		pairs = append(pairs, [2]string{m.Old, m.New})
	}
	if oldCwd != "" && newCwd != "" {
		pairs = append(pairs, [2]string{oldCwd, newCwd})
	}
	if oldHome != "" && newHome != "" {
		pairs = append(pairs, [2]string{oldHome, newHome})
	}
	return pairs
}

// verifyLines renders one rehome.VerifyCommand per distinct destination
// directory of the sessions include admits — identity directories sorted
// first, the pending group (no directory) last — each with the newest
// session of that group.
func verifyLines(cfgDir string, dest transcripts.Root, account string, plans []sessionPlan, include func(sessionPlan) bool) []string {
	newest := map[string]sessionPlan{}
	for _, sp := range plans {
		if !include(sp) {
			continue
		}
		cwd := sp.place.NewCwd
		if cur, ok := newest[cwd]; !ok || sp.entry.Last.After(cur.entry.Last) {
			newest[cwd] = sp
		}
	}
	if len(newest) == 0 {
		return nil
	}
	cwds := make([]string, 0, len(newest))
	for cwd := range newest {
		cwds = append(cwds, cwd)
	}
	sort.Slice(cwds, func(i, j int) bool {
		if (cwds[i] == "") != (cwds[j] == "") {
			return cwds[i] != ""
		}
		return cwds[i] < cwds[j]
	})
	var out []string
	for _, cwd := range cwds {
		out = append(out, rehome.VerifyCommand(cwd, newest[cwd].entry.SessionID, resumeAccount(cfgDir, dest, account, cwd)))
	}
	return out
}

// resumeAccount is the BFFS_ACCOUNT prefix for a verify line: the root's
// owner, else the account the import was asked for, else what the
// resolver picks for cwd.
func resumeAccount(cfgDir string, dest transcripts.Root, account, cwd string) string {
	if dest.Owner != "" {
		return dest.Owner
	}
	if account != "" && account != transcripts.HomeName {
		return account
	}
	return ResumeAccount(cfgDir, dest, cwd)
}

// memoryLabel names a memory entry in messages.
func memoryLabel(mp memoryPlan) string {
	if mp.entry.Cwd != "" {
		return sanitize(mp.entry.Cwd)
	}
	return "memory/" + sanitize(mp.entry.Slug)
}
