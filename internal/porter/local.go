package porter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// ErrSameRoot is returned by CopyLocal when the source and the destination
// are one projects/ pool — two partial-isolation accounts, or an account
// and "home" under partial isolation (plan §7.1). There is nothing to copy:
// the transcripts and the memory are already the same files, and what
// differs per account is trust (`bffs trust sync`). `bffs copy` exits 0
// with that hint.
var ErrSameRoot = errors.New("nothing to copy: source and destination share one projects pool (partial isolation)")

// ErrLiveSource is wrapped by CopyLocal when a move would take a session a
// running claude owns (plan §9.5). The whole move is refused up front.
var ErrLiveSource = errors.New("open in a running claude")

// ErrMoveVerify is wrapped by CopyLocal when a file of a landed session
// does not read back from the destination with the sha256 the manifest
// recorded. Nothing is removed from the source; the copies stay.
var ErrMoveVerify = errors.New("move refused: the destination does not match the source")

// errCopyDone is the error the copy hands the pipe once Import has
// returned, so a writer Import left blocked (a dry run, an early refusal)
// stops instead of waiting forever.
var errCopyDone = errors.New("copy: import finished")

// copyLanded is a test seam: when set, CopyLocal calls it after Import has
// landed the copies and before a move verifies them, so a test can corrupt
// a landed file and watch the move refuse.
var copyLanded func(cfgDir string, rep Report)

// SameRoot reports whether two roots are one projects/ pool — the same
// directory once symlinks are resolved.
func SameRoot(a, b transcripts.Root) bool {
	if a.Dir == "" || b.Dir == "" {
		return false
	}
	return canonicalPath(a.Dir) == canonicalPath(b.Dir)
}

// CopyLocal copies (or, with move, moves) sel — sessions and memory of one
// root on this machine — into o.Dest through the same path a bundle takes
// (plan §4.4, §7.1): BuildManifest describes the selection, Write streams
// it uncompressed through an io.Pipe into Import, and Import stages and
// commits it under plan §9 with the collision scan told to ignore the
// source pool (o.ExcludeRoot) and the reader in StreamMode. Every project
// directory of the selection that exists here is confirmed as its own
// placement (an identity rule), so the transcripts land under the same
// slug without a relocated record and the memory keeps its file names and
// its MEMORY.md — the way a same-machine copy should read. A memory
// directory whose sessions Claude has already swept is matched to its
// directory through the source's .claude.json project keys.
//
// The import record is written with Kind "copy". A source and destination
// that are one pool is ErrSameRoot before anything is read; a dry run
// returns the plan and writes nothing.
//
// With move, a selected session a running claude owns refuses the whole
// move before anything is written (ErrLiveSource). After Import every file
// of every landed session — Report.Imported and Report.Pending — is read
// back from the destination and checked against the manifest's sha256
// (a file-history backup or plan file a collision renamed
// "*.imported-<id8>" is accepted under that name); one mismatch is
// ErrMoveVerify and nothing is removed. Then the source transcript, the
// sidecar files, file-history/<sid>, the plan files and tasks/<sid> files
// the manifest listed are removed through an os.Root over the source
// config dir — never anything else in those directories, never
// history.jsonl — and directories left empty are pruned. A selected memory
// directory is removed the same way only when every one of its files
// reads back byte-identical at the destination (under its own name or its
// "*.imported-<id8>.md" side name); a merge that appended to MEMORY.md or
// a skip that kept a different file leaves the source memory in place with
// a warning naming it. Sessions the import skipped or held keep their
// source too.
func CopyLocal(ctx context.Context, cfgDir string, sel Selection, o ImportOptions, move bool) (Report, error) {
	src, dst := sel.Root, o.Dest
	if src.Dir == "" || src.ConfigDir == "" {
		return Report{}, errors.New("source root is not set")
	}
	if dst.Dir == "" || dst.ConfigDir == "" {
		return Report{}, errors.New("destination root is not set")
	}
	if SameRoot(src, dst) {
		return Report{}, ErrSameRoot
	}
	if move {
		for _, s := range sel.Sessions {
			ls, live := o.Live[s.ID]
			if !live && !s.Live {
				continue
			}
			if ls.PID > 0 {
				return Report{}, fmt.Errorf("session %s is %w (pid %d); close it before moving", s.ID, ErrLiveSource, ls.PID)
			}
			return Report{}, fmt.Errorf("session %s is %w; close it before moving", s.ID, ErrLiveSource)
		}
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	o.Now = now
	o.ExcludeRoot = canonicalPath(src.Dir)
	o.StreamMode = true

	eo := copyExportOptions(src, now)
	m, opener, warnings, err := BuildManifest(ctx, sel, eo)
	if err != nil {
		return Report{}, err
	}
	fillMemoryCwds(m, func() map[string]string { return memoryCwdIndex(cfgDir, src) })
	if !o.AsIs && o.Into == "" && len(o.Map) == 0 {
		o.Map = identityRules(m)
	}

	rep, err := importPiped(ctx, cfgDir, m, opener, eo, o)
	rep.Warnings = append(warnings, rep.Warnings...)
	if err != nil {
		return rep, err
	}
	if o.DryRun {
		return rep, nil
	}
	rec, err := markCopy(cfgDir, rep.BundleID)
	if err != nil {
		if move {
			return rep, fmt.Errorf("%v; nothing was removed from the source", err)
		}
		rep.Warnings = append(rep.Warnings, sanitize(err.Error()))
		return rep, nil
	}
	if !move {
		return rep, nil
	}
	if copyLanded != nil {
		copyLanded(cfgDir, rep)
	}
	return rep, removeSource(ctx, &rep, src, dst, m, rec)
}

// copyExportOptions describes the source side of a copy: uncompressed, the
// root's owner as the account when it has one.
func copyExportOptions(src transcripts.Root, now time.Time) ExportOptions {
	eo := ExportOptions{Compression: bundle.CompNone, Now: now}
	switch {
	case src.Orphan:
	case src.Owner != "":
		eo.Account = store.Account{Name: src.Owner, Type: store.TypeOAuth}
		eo.Isolation = store.IsolationFull
	case src.Shared:
		eo.Isolation = store.IsolationPartial
	}
	return eo
}

// importPiped runs Write and Import on the two ends of a pipe. The writer
// is released once Import returns, whatever it consumed; when Import
// succeeded every staged file was verified against the manifest, so a
// writer error after that point carries no information.
func importPiped(ctx context.Context, cfgDir string, m *bundle.Manifest, opener bundle.Opener, eo ExportOptions, o ImportOptions) (Report, error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := Write(ctx, pw, m, opener, eo)
		pw.CloseWithError(err) // nil closes plainly: the reader sees EOF
		done <- err
	}()
	rep, err := Import(ctx, cfgDir, pr, o)
	pr.CloseWithError(errCopyDone)
	<-done
	return rep, err
}

// identityRules confirms every directory of the manifest that exists here
// as its own placement (plan §9.3: a rule is a confirmation; an identity
// rule never stamps). Directories come back sorted.
func identityRules(m *bundle.Manifest) []imports.Mapping {
	seen := map[string]bool{}
	var dirs []string
	for i := range m.Entries {
		cwd := m.Entries[i].Cwd
		if cwd == "" || seen[cwd] || !filepath.IsAbs(cwd) || !dirExists(cwd) {
			continue
		}
		seen[cwd] = true
		dirs = append(dirs, cwd)
	}
	sort.Strings(dirs)
	rules := make([]imports.Mapping, 0, len(dirs))
	for _, d := range dirs {
		rules = append(rules, imports.Mapping{Old: d, New: d})
	}
	return rules
}

// fillMemoryCwds gives a memory entry without a cwd — its slug directory
// holds no transcript any more — the directory the source's .claude.json
// knows under that slug, so the copy can confirm it. index is computed
// lazily: most selections need no lookup.
func fillMemoryCwds(m *bundle.Manifest, index func() map[string]string) {
	var idx map[string]string
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Kind != bundle.EntryMemory || e.Cwd != "" {
			continue
		}
		if idx == nil {
			idx = index()
		}
		if cwd, ok := idx[e.Slug]; ok {
			e.Cwd, e.ProjectKey = cwd, cwd
		}
	}
}

// memoryCwdIndex maps memory slugs to the project directories the source
// root's .claude.json files know — the root's own file and, for a shared
// pool, every attached account's — keeping only directories that exist
// here. Keys are Claude's project keys; the memory slug of each is
// transcripts.MemorySlug (the git root's slug), so a session that ran in
// a subdirectory still finds its memory.
func memoryCwdIndex(cfgDir string, root transcripts.Root) map[string]string {
	files := []string{root.ClaudeJSON}
	if root.Shared {
		for _, acc := range root.Accounts {
			files = append(files, filepath.Join(sessions.Dir(cfgDir, acc), claudejson.Filename))
		}
	}
	idx := map[string]string{}
	for _, f := range files {
		if f == "" {
			continue
		}
		flags, err := claudejson.ReadProjectFlags(f)
		if err != nil {
			continue
		}
		keys := make([]string, 0, len(flags))
		for k := range flags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !filepath.IsAbs(k) || !dirExists(k) {
				continue
			}
			dir, err := transcripts.ProjectKey(k)
			if err != nil {
				continue
			}
			slug, err := transcripts.Slug(dir)
			if err != nil {
				continue
			}
			if _, ok := idx[slug]; !ok {
				idx[slug] = dir
			}
		}
	}
	return idx
}

// markCopy rewrites the import record Import wrote for bundleID as a copy:
// Kind "copy", the identity rules dropped from the mapping (they said
// "stay where you are", which the record's statuses already say).
func markCopy(cfgDir, bundleID string) (imports.Record, error) {
	recs, err := imports.Load(cfgDir)
	if err != nil {
		return imports.Record{}, fmt.Errorf("import record: %w", err)
	}
	for _, rec := range recs {
		if rec.BundleID != bundleID {
			continue
		}
		rec.Kind = imports.KindCopy
		var maps []imports.Mapping
		for _, mp := range rec.Mapping {
			if mp.Old != mp.New {
				maps = append(maps, mp)
			}
		}
		rec.Mapping = maps
		if err := imports.Save(cfgDir, rec); err != nil {
			return imports.Record{}, fmt.Errorf("import record: %w", err)
		}
		return rec, nil
	}
	return imports.Record{}, fmt.Errorf("import record %s not found under %s", short8(bundleID), filepath.Join(cfgDir, imports.Subdir))
}

// excludeUnderRoot drops the sessions whose transcript lies under dir —
// the source copies of a CopyLocal, which the destination's collision scan
// must not count (plan §9.4: presence in other roots never matters). dir
// is compared canonically, as is each transcript's directory.
func excludeUnderRoot(ss []transcripts.Session, dir string) []transcripts.Session {
	root := canonicalPath(dir)
	if root == "" {
		return ss
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	out := ss[:0:0]
	for _, s := range ss {
		if s.Path != "" {
			// The directory is canonicalised, not the file: a transcript
			// compaction renamed away between the listing and this check
			// still belongs to the pool it was listed in.
			p := filepath.Join(canonicalPath(filepath.Dir(s.Path)), filepath.Base(s.Path))
			if strings.HasPrefix(p, prefix) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// removal is one session's or memory directory's source files to remove
// (relative to the source config dir, slash-separated, in removal order)
// and the directories to prune afterwards when they are left empty.
type removal struct {
	label string
	files []string
	prune []string
}

// removeSource verifies every landed file of m at the destination and then
// removes the listed source files (plan §7.1). Session mismatches refuse
// the move; memory mismatches keep that memory directory with a warning.
func removeSource(ctx context.Context, rep *Report, src, dst transcripts.Root, m *bundle.Manifest, rec imports.Record) error {
	landed := map[string]bool{}
	for _, sid := range rep.Imported {
		landed[sid] = true
	}
	for _, sid := range rep.Pending {
		landed[sid] = true
	}
	id8 := short8(m.BundleID)
	var removals []removal
	var bad []string
	sessIdx, memIdx := 0, 0
	for i := range m.Entries {
		e := &m.Entries[i]
		switch e.Kind {
		case bundle.EntrySession:
			idx := sessIdx
			sessIdx++
			if !landed[e.SessionID] {
				continue
			}
			slug := e.Slug
			if idx < len(rec.Sessions) && rec.Sessions[idx].ID == e.SessionID && rec.Sessions[idx].Slug != "" {
				slug = rec.Sessions[idx].Slug
			}
			r, mismatches, err := planSessionRemoval(src, dst, e, slug, id8)
			if err != nil {
				return fmt.Errorf("session %s: %w; nothing was removed from the source", short8(e.SessionID), err)
			}
			bad = append(bad, mismatches...)
			removals = append(removals, r)
		case bundle.EntryMemory:
			idx := memIdx
			memIdx++
			dir := filepath.Join(dst.Dir, e.Slug, transcripts.MemorySubdir)
			if idx < len(rec.Memories) && rec.Memories[idx].Dir != "" {
				dir = rec.Memories[idx].Dir
			}
			r, mismatches, err := planMemoryRemoval(src, e, dir, id8)
			if err != nil {
				return fmt.Errorf("memory %s: %w; nothing was removed from the source", sanitize(memoryEntryLabel(e)), err)
			}
			if len(mismatches) > 0 {
				rep.Warnings = append(rep.Warnings, sanitize(fmt.Sprintf("memory for %s kept at %s: %s does not read back identical at %s (merged, renamed or kept as it was) — remove it by hand after review", memoryEntryLabel(e), filepath.Join(src.Dir, e.Slug, transcripts.MemorySubdir), strings.Join(mismatches, ", "), dir)))
				continue
			}
			removals = append(removals, r)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s; the copies stay in %s and nothing was removed from %s", ErrMoveVerify, sanitize(strings.Join(bad, ", ")), dst.Dir, src.Dir)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w; nothing was removed from the source", err)
	}
	root, err := os.OpenRoot(src.ConfigDir)
	if err != nil {
		return fmt.Errorf("open source %s: %w; nothing was removed", src.ConfigDir, err)
	}
	defer root.Close()
	var failed []string
	for _, r := range removals {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w; %s and later originals were not removed", err, r.label)
		}
		if err := applyRemoval(root, r); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.label, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("originals not fully removed from %s: %s", src.ConfigDir, sanitize(strings.Join(failed, "; ")))
	}
	return nil
}

// planSessionRemoval verifies a landed session's files at the destination
// and lists their source counterparts: the transcript first, then the
// sidecar, file-history, plan and tasks files the manifest carried.
// mismatches names every file that did not read back.
func planSessionRemoval(src, dst transcripts.Root, e *bundle.Entry, slug, id8 string) (removal, []string, error) {
	sid := e.SessionID
	r := removal{label: "session " + short8(sid)}
	var mismatches []string
	var rest []string
	for _, f := range e.Files {
		kind, fsid, fslug, err := bundle.ClassifyName(f.Path)
		if err != nil {
			return removal{}, nil, err
		}
		var target string
		var alts []string
		switch kind {
		case bundle.NameHistory:
			continue // synthesised from history.jsonl: nothing landed as a file, nothing to remove
		case bundle.NameTranscript:
			target = filepath.Join(dst.Dir, slug, sid+transcripts.TranscriptExt)
		case bundle.NameSidecar:
			rel := strings.TrimPrefix(f.Path, joinSlash("projects", fslug, fsid)+"/")
			target = filepath.Join(dst.Dir, slug, sid, filepath.FromSlash(rel))
		case bundle.NameFileHistory:
			base := path.Base(f.Path)
			target = filepath.Join(dst.ConfigDir, transcripts.FileHistorySubdir, sid, base)
			alts = []string{filepath.Join(dst.ConfigDir, transcripts.FileHistorySubdir, sid, base+importedInfix+id8)}
		case bundle.NamePlans:
			base := path.Base(f.Path)
			target = filepath.Join(dst.ConfigDir, transcripts.PlansSubdir, base)
			alts = []string{filepath.Join(dst.ConfigDir, transcripts.PlansSubdir, importedMarkdownName(base, id8))}
		case bundle.NameTasks:
			rel := strings.TrimPrefix(f.Path, joinSlash("tasks", fsid)+"/")
			target = filepath.Join(dst.ConfigDir, transcripts.TasksSubdir, sid, filepath.FromSlash(rel))
		default:
			return removal{}, nil, fmt.Errorf("unexpected bundle entry %q", f.Path)
		}
		if !readsBack(f, target, alts) {
			mismatches = append(mismatches, f.Path)
			continue
		}
		srcRel, err := sourceRel(src, f.Path)
		if err != nil {
			return removal{}, nil, err
		}
		if kind == bundle.NameTranscript {
			r.files = append([]string{srcRel}, r.files...)
		} else {
			rest = append(rest, srcRel)
		}
	}
	r.files = append(r.files, rest...)
	for _, d := range []string{
		joinSlash("projects", e.Slug, sid),
		joinSlash(transcripts.FileHistorySubdir, sid),
		joinSlash(transcripts.TasksSubdir, sid),
	} {
		if rel, err := sourceRel(src, d); err == nil {
			r.prune = append(r.prune, rel)
		}
	}
	return r, mismatches, nil
}

// planMemoryRemoval verifies a memory entry's files at dir (each under its
// own name or its "*.imported-<id8>.md" side name) and lists the source
// files to remove when all of them read back.
func planMemoryRemoval(src transcripts.Root, e *bundle.Entry, dir, id8 string) (removal, []string, error) {
	r := removal{label: "memory " + memoryEntryLabel(e)}
	var mismatches []string
	prefix := joinSlash("memory", e.Slug) + "/"
	for _, f := range e.Files {
		kind, _, _, err := bundle.ClassifyName(f.Path)
		if err != nil {
			return removal{}, nil, err
		}
		if kind != bundle.NameMemory || !strings.HasPrefix(f.Path, prefix) {
			return removal{}, nil, fmt.Errorf("unexpected bundle entry %q", f.Path)
		}
		rel := strings.TrimPrefix(f.Path, prefix)
		target := filepath.Join(dir, filepath.FromSlash(rel))
		alt := filepath.Join(dir, filepath.FromSlash(path.Join(path.Dir(rel), importedMarkdownName(path.Base(rel), id8))))
		if !readsBack(f, target, []string{alt}) {
			mismatches = append(mismatches, rel)
			continue
		}
		srcRel, err := sourceRel(src, f.Path)
		if err != nil {
			return removal{}, nil, err
		}
		r.files = append(r.files, srcRel)
	}
	if rel, err := sourceRel(src, joinSlash("projects", e.Slug, transcripts.MemorySubdir)); err == nil {
		r.prune = append(r.prune, rel)
	}
	return r, mismatches, nil
}

// memoryEntryLabel names a memory entry: its directory, else its slug.
func memoryEntryLabel(e *bundle.Entry) string {
	if e.Cwd != "" {
		return e.Cwd
	}
	return "memory/" + e.Slug
}

// readsBack reports whether target, or one of alts, is a regular file of
// f's size whose sha256 is f's.
func readsBack(f bundle.File, target string, alts []string) bool {
	for _, p := range append([]string{target}, alts...) {
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() != f.Size {
			continue
		}
		sum, err := sha256File(p)
		if err == nil && strings.EqualFold(sum, f.SHA256) {
			return true
		}
	}
	return false
}

func sha256File(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sourceRel maps a bundle name to the source file it was read from,
// relative to the source config dir (slash-separated) so an os.Root over
// that dir can remove it: "projects/…" and "memory/<slug>/…" live under
// the root's projects/ directory, everything else directly under the
// config dir. A path outside the config dir is refused.
func sourceRel(src transcripts.Root, name string) (string, error) {
	parts := strings.SplitN(name, "/", 2)
	var abs string
	switch {
	case parts[0] == "projects" && len(parts) == 2:
		abs = filepath.Join(src.Dir, filepath.FromSlash(parts[1]))
	case parts[0] == "memory" && len(parts) == 2:
		slug, rest, _ := strings.Cut(parts[1], "/")
		abs = filepath.Join(src.Dir, slug, transcripts.MemorySubdir, filepath.FromSlash(rest))
	default:
		abs = filepath.Join(src.ConfigDir, filepath.FromSlash(name))
	}
	rel, err := filepath.Rel(src.ConfigDir, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s lies outside %s; refusing to remove it", abs, src.ConfigDir)
	}
	return filepath.ToSlash(rel), nil
}

// applyRemoval removes r's files in order, then prunes its directories.
// A file already gone is not an error; nothing unlisted is touched.
func applyRemoval(root *os.Root, r removal) error {
	var errs []error
	for i, rel := range r.files {
		err := root.Remove(filepath.FromSlash(rel))
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}
		errs = append(errs, err)
		if i == 0 {
			return errors.Join(errs...) // the transcript stays: leave the session whole
		}
	}
	for _, rel := range r.prune {
		if err := pruneEmpty(root, filepath.FromSlash(rel)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pruneEmpty removes rel when it is a directory whose subtree holds no
// file — recursing into subdirectories first. A missing rel is fine;
// a directory with anything left in it stays.
func pruneEmpty(root *os.Root, rel string) error {
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	d, err := root.Open(rel)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := pruneEmpty(root, filepath.Join(rel, e.Name())); err != nil {
				return err
			}
		}
	}
	d, err = root.Open(rel)
	if err != nil {
		return err
	}
	left, err := d.ReadDir(1)
	d.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(left) > 0 {
		return nil
	}
	err = root.Remove(rel)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// importedInfix and importedMarkdownName mirror rehome's collision names:
// "<name>.imported-<id8>" for a file-history backup, "<stem>.imported-
// <id8>.md" for a markdown file (plans, memory topics).
const importedInfix = ".imported-"

func importedMarkdownName(base, id8 string) string {
	if stem, ok := strings.CutSuffix(base, ".md"); ok {
		return stem + importedInfix + id8 + ".md"
	}
	return base + importedInfix + id8
}
