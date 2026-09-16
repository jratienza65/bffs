package rehome

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

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Result is what Apply did (or, with DryRun, would do). Plan is the plan
// it ran; Moved lists the sessions relocated (in order); Held the sessions
// the plan refused because a running claude owns them and Skipped the
// other refusals; LastSessionSet is the session --set-last-session pointed
// the newest mapped directory at; Verify holds one VerifyCommand line per
// mapped directory. JournalDir names the journal kept after a failure.
// Rewrites lists, per moved transcript that changed, what
// Options.RewriteCwd / RewriteFileHistory replaced.
type Result struct {
	Plan
	Held, Skipped  []string
	LastSessionSet string
	Verify         []string

	Moved      []string
	JournalDir string
	Rewrites   []TranscriptRewrite
}

// Lock parameters for the .claude.json write of --set-last-session:
// Claude's proper-lockfile treats a lock older than 10 s as abandoned.
const (
	claudeJSONLockStale = 10 * time.Second
	claudeJSONLockWait  = 3 * time.Second
)

// Apply carries out p in place (plan §9.6), one session at a time and
// transactionally: with the journal under <StagingDir>/rehome-<epochms>/
// updated at every step, the sidecar is renamed first (<old>/<sid>/ →
// <new>/<sid>.bffs-tmp/ → <new>/<sid>/), then the transcript (<old>/<sid>.jsonl
// → <new>/<sid>.jsonl.bffs-tmp, the relocated record appended, →
// <new>/<sid>.jsonl); the transcript's mtime is captured before the append
// and restored afterwards as max(original, TranscriptFloor(opts.Mtime)),
// so picker order and `--continue` are unchanged. A same-slug move only
// appends the stamp. Every rename is undone in reverse when a step fails.
// Plan files and history lines need no move. Recovery runs first: Recover
// over the root once per leftover rehome-* journal directory under
// StagingDir (an interrupted rehome; a consumed journal directory is
// removed), or once without a journal. Two rehomes running at the same
// time on one root are not supported.
//
// Memory: every planned directory is merged (MergeMemory, confirmed, the
// files' pinned state kept — they already live in this pool) and, with
// opts.RewriteMemory, the plan's mappings plus OldHome→NewHome are
// rewritten in it (RewriteMemoryPaths); the old directory stays in place
// and Warnings says so. --set-last-session writes lastSessionId under
// Claude's lock. Every write into projects/ goes through an os.Root over
// the root's config dir. With DryRun nothing is written and the Result is
// filled from the plan.
func Apply(ctx context.Context, root transcripts.Root, p Plan, opts Options) (Result, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	res := Result{Plan: p}
	for _, r := range p.Refusals {
		if strings.Contains(r.Reason, "running claude") {
			res.Held = append(res.Held, r.SessionID)
		} else {
			res.Skipped = append(res.Skipped, r.SessionID)
		}
	}
	warn := func(format string, args ...any) {
		res.Warnings = append(res.Warnings, transcripts.Sanitize(fmt.Sprintf(format, args...)))
	}
	if opts.DryRun {
		res.Verify = verifyLines(root, p.Moves, opts.Account)
		return res, nil
	}
	if len(p.Moves) == 0 && len(p.Memory) == 0 {
		return res, nil
	}

	base, err := stagingBase(opts)
	if err != nil {
		return res, err
	}
	restored, kept, err := recoverRehomes(root, base)
	if err != nil {
		return res, err
	}
	for _, x := range restored {
		warn("recovered %s from an interrupted operation", x)
	}
	for _, x := range kept {
		warn("left %s in place (an interrupted copy)", x)
	}

	jdir, err := journalDir(base, now)
	if err != nil {
		return res, err
	}
	res.JournalDir = jdir
	dest, err := os.OpenRoot(root.ConfigDir)
	if err != nil {
		return res, fmt.Errorf("open %s: %w", root.ConfigDir, err)
	}
	defer dest.Close()
	projRel, err := filepath.Rel(root.ConfigDir, root.Dir)
	if err != nil || projRel == "." || strings.HasPrefix(projRel, "..") {
		return res, fmt.Errorf("projects directory %s is not inside %s", root.Dir, root.ConfigDir)
	}

	policy := opts.Mtime
	if policy.Now.IsZero() {
		policy.Now = now
	}
	var moved []Move
	var errs []error
	for _, mv := range p.Moves {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := moveSession(dest, projRel, mv, opts, policy, jdir); err != nil {
			return res, fmt.Errorf("session %s: %w; journal kept at %s", mv.SessionID, err, jdir)
		}
		res.Moved = append(res.Moved, mv.SessionID)
		moved = append(moved, mv)
		if !opts.RewriteCwd && !opts.RewriteFileHistory {
			continue
		}
		// The session has landed; the rewrite is a separate, atomic pass
		// over the final file (tmp + rename, mtime restored), so a failure
		// here leaves a moved, stamped, unrewritten transcript.
		rel, err := filepath.Rel(root.ConfigDir, mv.To)
		if err != nil {
			errs = append(errs, err)
			warn("transcript of %s not rewritten: %v", mv.SessionID, err)
			continue
		}
		rules := TranscriptRules{
			OldCwd:      mv.OldCwd,
			NewCwd:      mv.NewCwd,
			Paths:       rewritePairs(p.Mappings, mv.OldCwd, opts),
			Cwd:         opts.RewriteCwd,
			FileHistory: opts.RewriteFileHistory,
		}
		rw, err := rewriteTranscriptIn(dest, rel, rules)
		if err != nil {
			errs = append(errs, err)
			warn("transcript of %s not rewritten: %v", mv.SessionID, err)
			continue
		}
		if rw.CwdRecords > 0 || rw.FileHistoryPaths > 0 {
			res.Rewrites = append(res.Rewrites, rw)
		}
	}

	for i := range res.Memory {
		m := &res.Memory[i]
		mode := m.Mode
		if mode == "" {
			mode = opts.Memory
		}
		if mode == "" {
			mode = MemoryMerge
		}
		got, err := MergeMemory(m.To, m.From, mode, m.ID8, true, true, now)
		if err != nil {
			errs = append(errs, err)
			warn("memory of %s not merged: %v", m.OldCwd, err)
			continue
		}
		m.Added, m.Renamed, m.Unchanged = got.Added, got.Renamed, got.Unchanged
		m.IndexAppended, m.Remaining, m.Warnings = got.IndexAppended, got.Remaining, got.Warnings
		m.Mode = mode
		if opts.RewriteMemory && dirExists(m.To) {
			changed, remaining, err := RewriteMemoryPaths(m.To, rewritePairs(p.Mappings, m.OldCwd, opts))
			if err != nil {
				errs = append(errs, err)
				warn("memory paths in %s not rewritten: %v", m.To, err)
			} else {
				m.Rewritten = changed
				m.Remaining = remaining
			}
		}
		warn("old memory directory %s left in place (merged into %s); remove it yourself once you are happy with the result", m.From, m.To)
	}

	if opts.SetLastSession && len(moved) > 0 {
		if opts.ClaudeJSON == "" {
			warn("lastSessionId not set: no .claude.json for the target account (Options.ClaudeJSON)")
		} else if sid, err := setLastSessions(opts.ClaudeJSON, moved); err != nil {
			errs = append(errs, err)
			warn("lastSessionId not set: %v", err)
		} else {
			res.LastSessionSet = sid
		}
	}
	res.Verify = verifyLines(root, moved, opts.Account)
	if err := os.RemoveAll(jdir); err != nil {
		warn("journal directory %s not removed: %v", jdir, err)
	} else {
		res.JournalDir = ""
	}
	return res, errors.Join(errs...)
}

// rehomeJournalPrefix names the journal directories of in-place rehomes
// under the staging directory: rehome-<epochms>[-<rand>]/.
const rehomeJournalPrefix = "rehome-"

// stagingBase is the directory journals live under: opts.StagingDir, else
// <CfgDir>/staging.
func stagingBase(opts Options) (string, error) {
	if opts.StagingDir != "" {
		return opts.StagingDir, nil
	}
	if opts.CfgDir == "" {
		return "", errors.New("staging directory not set (Options.StagingDir or Options.CfgDir)")
	}
	return filepath.Join(opts.CfgDir, "staging"), nil
}

// recoverRehomes runs Recover over root once per leftover rehome-*
// journal directory under base — each the trace of an interrupted
// in-place rehome — or once without a journal when there is none. A
// journal directory whose pass kept nothing on disk is removed: it holds
// only journal.json, now consumed.
func recoverRehomes(root transcripts.Root, base string) (restored, kept []string, err error) {
	var dirs []string
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), rehomeJournalPrefix) {
				dirs = append(dirs, filepath.Join(base, e.Name()))
			}
		}
	}
	if len(dirs) == 0 {
		return Recover(root, "")
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		r, k, err := Recover(root, d)
		if err != nil {
			return restored, kept, err
		}
		for _, x := range r {
			if !seen[x] {
				seen[x] = true
				restored = append(restored, x)
			}
		}
		for _, x := range k {
			if !seen[x] {
				seen[x] = true
				kept = append(kept, x)
			}
		}
		if len(k) == 0 {
			_ = os.RemoveAll(d)
		}
	}
	return restored, kept, nil
}

// journalDir creates <base>/rehome-<epochms>/ (a random suffix when that
// name exists) and returns it.
func journalDir(base string, now time.Time) (string, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", base, err)
	}
	dir := filepath.Join(base, fmt.Sprintf("%s%d", rehomeJournalPrefix, now.UnixMilli()))
	err := os.Mkdir(dir, 0o700)
	if errors.Is(err, fs.ErrExist) {
		dir += "-" + randSuffix()
		err = os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// moveSession relocates one session inside dest; every rename is undone
// in reverse when a later step fails.
func moveSession(dest *os.Root, projRel string, mv Move, opts Options, policy MtimePolicy, jdir string) (err error) {
	sid := mv.SessionID
	oldDir := filepath.Join(projRel, filepath.Base(filepath.Dir(mv.From)))
	newDir := filepath.Join(projRel, mv.NewSlug)
	oldTr := filepath.Join(oldDir, sid+transcripts.TranscriptExt)
	newTr := filepath.Join(newDir, sid+transcripts.TranscriptExt)
	journal := func(step string) error {
		return WriteJournal(jdir, Journal{BundleID: opts.BundleID, SessionID: sid, Origin: mv.From, Target: mv.To, Step: step})
	}
	var undo []func() error
	defer func() {
		if err == nil {
			return
		}
		var errs []error
		for i := len(undo) - 1; i >= 0; i-- {
			if uerr := undo[i](); uerr != nil && !errors.Is(uerr, fs.ErrNotExist) {
				errs = append(errs, uerr)
			}
		}
		if len(errs) > 0 {
			err = errors.Join(err, fmt.Errorf("rollback: %w", errors.Join(errs...)))
		}
	}()
	rename := func(from, to string) error {
		if err := renameFn(dest, from, to); err != nil {
			return fmt.Errorf("rename %q to %q: %w", filepath.ToSlash(from), filepath.ToSlash(to), err)
		}
		undo = append(undo, func() error { return dest.Rename(to, from) })
		return nil
	}
	stamp := func(name string) (time.Time, error) {
		info, err := dest.Lstat(name)
		if err != nil {
			return time.Time{}, err
		}
		if !info.Mode().IsRegular() {
			return time.Time{}, fmt.Errorf("%q is not a regular file", filepath.ToSlash(name))
		}
		size, mtime := info.Size(), info.ModTime()
		f, err := dest.OpenFile(name, os.O_RDWR|os.O_APPEND, 0)
		if err != nil {
			return time.Time{}, err
		}
		undo = append(undo, func() error {
			g, err := dest.OpenFile(name, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			if err := g.Truncate(size); err != nil {
				_ = g.Close()
				return err
			}
			if err := g.Close(); err != nil {
				return err
			}
			return dest.Chtimes(name, mtime, mtime)
		})
		if err := appendRelocated(f, sid, mv.NewCwd, opts.ForceStamp); err != nil {
			_ = f.Close()
			return time.Time{}, err
		}
		if err := f.Close(); err != nil {
			return time.Time{}, err
		}
		return mtime, nil
	}
	restore := func(name string, orig time.Time) error {
		final, _ := transcriptMtime(orig, policy)
		if err := dest.Chtimes(name, final, final); err != nil {
			return fmt.Errorf("set mtime on %q: %w", filepath.ToSlash(name), err)
		}
		return nil
	}

	if mv.SameSlug {
		if err := journal(StepTranscript); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		orig, err := stamp(oldTr)
		if err != nil {
			return err
		}
		if err := restore(oldTr, orig); err != nil {
			return err
		}
		if err := journal(StepDone); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		return nil
	}

	// a — sidecar.
	if err := journal(StepSidecar); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if _, err := dest.Lstat(newDir); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("check %q: %w", filepath.ToSlash(newDir), err)
		}
		if err := dest.Mkdir(newDir, 0o700); err != nil {
			return fmt.Errorf("create %q: %w", filepath.ToSlash(newDir), err)
		}
		undo = append(undo, func() error { return dest.Remove(newDir) })
	}
	if mv.Sidecar {
		final := filepath.Join(newDir, sid)
		if _, err := dest.Lstat(final); err == nil {
			return fmt.Errorf("%q already exists", filepath.ToSlash(final))
		}
		tmp := freeName(dest, filepath.Join(newDir, sid+tmpSuffix))
		if err := rename(filepath.Join(oldDir, sid), tmp); err != nil {
			return err
		}
		if err := rename(tmp, final); err != nil {
			return err
		}
	}

	// b — transcript.
	if err := journal(StepTranscript); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if _, err := dest.Lstat(newTr); err == nil {
		return fmt.Errorf("%q already exists", filepath.ToSlash(newTr))
	}
	tmp := freeName(dest, filepath.Join(newDir, sid+transcripts.TranscriptExt+tmpSuffix))
	if err := rename(oldTr, tmp); err != nil {
		return err
	}
	orig, err := stamp(tmp)
	if err != nil {
		return err
	}
	if err := rename(tmp, newTr); err != nil {
		return err
	}
	syncDir(dest, newDir)
	if err := restore(newTr, orig); err != nil {
		return err
	}
	if err := journal(StepDone); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

// freeName returns name, or name-<rand> when an earlier leftover holds it.
func freeName(dest *os.Root, name string) string {
	if _, err := dest.Lstat(name); err == nil {
		return name + "-" + randSuffix()
	}
	return name
}

// rewritePairs builds the [old, new] prefix pairs for one memory
// directory: every mapping, the directory's own old→new cwd, and
// OldHome→NewHome.
func rewritePairs(maps []Mapping, oldCwd string, opts Options) [][2]string {
	var pairs [][2]string
	for _, m := range maps {
		pairs = append(pairs, [2]string{m.Old, m.New})
	}
	if newCwd := newCwdOfMemory(maps, oldCwd); newCwd != "" {
		pairs = append(pairs, [2]string{oldCwd, newCwd})
	}
	if opts.OldHome != "" && opts.NewHome != "" {
		pairs = append(pairs, [2]string{opts.OldHome, opts.NewHome})
	}
	return pairs
}

// newCwdOfMemory maps a memory's old cwd with the plan's rules.
func newCwdOfMemory(maps []Mapping, oldCwd string) string {
	newCwd, _, ok := ApplyMappings(maps, oldCwd, oldCwd)
	if !ok {
		return ""
	}
	return newCwd
}

// newestPerCwd picks, per mapped directory, the move with the newest
// transcript; the directories come back sorted.
func newestPerCwd(moves []Move) ([]string, map[string]Move) {
	newest := map[string]Move{}
	for _, mv := range moves {
		if cur, ok := newest[mv.NewCwd]; !ok || mv.LastTS.After(cur.LastTS) {
			newest[mv.NewCwd] = mv
		}
	}
	cwds := make([]string, 0, len(newest))
	for cwd := range newest {
		cwds = append(cwds, cwd)
	}
	sort.Strings(cwds)
	return cwds, newest
}

// setLastSessions points projects[<key>].lastSessionId of claudeJSON at
// the newest moved session of every mapped directory, under Claude's
// lock, and returns the id written for the newest directory overall.
func setLastSessions(claudeJSON string, moved []Move) (string, error) {
	cwds, newest := newestPerCwd(moved)
	release, err := fsutil.Lock(claudeJSON+".lock", claudeJSONLockStale, claudeJSONLockWait)
	if err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			return "", fmt.Errorf("%s is being written by a running claude; retry in a moment (%w)", claudeJSON, err)
		}
		return "", err
	}
	defer release()
	var top Move
	for _, cwd := range cwds {
		mv := newest[cwd]
		key, err := transcripts.ProjectKey(cwd)
		if err != nil {
			return "", err
		}
		if err := claudejson.SetLastSessionID(claudeJSON, key, mv.SessionID); err != nil {
			return "", err
		}
		if top.SessionID == "" || mv.LastTS.After(top.LastTS) {
			top = mv
		}
	}
	return top.SessionID, nil
}

// verifyLines renders one VerifyCommand per mapped directory with its
// newest session; account is opts.Account, else the root's owner.
func verifyLines(root transcripts.Root, moves []Move, account string) []string {
	if account == "" {
		account = root.Owner
	}
	cwds, newest := newestPerCwd(moves)
	var out []string
	for _, cwd := range cwds {
		out = append(out, VerifyCommand(cwd, newest[cwd].SessionID, account))
	}
	return out
}
