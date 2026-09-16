package rehome

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// CommitRequest describes one staged session to land in a config dir.
// Transcript, SidecarDir, FileHistory, Plans and TasksDir are SOURCE paths
// (inside staging, outside the destination root); an empty SidecarDir /
// TasksDir and empty slices mean "none". Slug is the projects/ entry the
// session lands in. Stamp, when non-nil, is appended to the transcript
// before it becomes visible (the relocated record of a mapped placement;
// nil for identity and as-is). OrigMtime is the transcript's mtime on the
// source machine (zero = unknown, treated as now). History holds the staged
// history.jsonl lines for this session (nil when none) and HistoryProject
// the project value they are written with ("" keeps each line's own). ID8
// is bundle_id[:8], the suffix of every ".imported-<id8>" name a collision
// produces.
type CommitRequest struct {
	SessionID      string
	Slug           string
	Transcript     string
	SidecarDir     string
	FileHistory    []string
	Plans          []string
	TasksDir       string
	Stamp          []byte
	OrigMtime      time.Time
	History        [][]byte
	HistoryProject string
	ID8            string
}

// CommitResult reports what a successful CommitSession wrote. Placed lists
// every path created, relative to the config dir and slash-separated
// (directories end in "/"). Warnings are the collisions that produced an
// ".imported-<id8>" copy or skipped an existing tasks/ dir, ready to print.
// Mtime is the transcript's final mtime and MtimeRaised whether the
// retention floor raised it above OrigMtime. HistoryAdded counts the
// history.jsonl lines appended.
type CommitResult struct {
	Warnings     []string
	Placed       []string
	MtimeRaised  bool
	Mtime        time.Time
	HistoryAdded int
}

// renameFn performs every rename CommitSession makes through the
// destination root. It is a variable so tests can inject a failure at a
// chosen step and watch the rollback.
var renameFn = func(r *os.Root, oldname, newname string) error {
	return r.Rename(oldname, newname)
}

// CommitSession lands one staged session inside dest, the os.Root opened
// over the destination config dir (transcripts.Root.ConfigDir), in the
// order of plan §9.6 so the session is either whole or absent:
//
//	a  journal("sidecar"); the sidecar tree is copied to
//	   projects/<slug>/<sid>.bffs-tmp/ and renamed to projects/<slug>/<sid>/;
//	   every file inside is stamped p.Now unless p.PreserveSidecars
//	b  file-history/<sid>/<name>, plans/<name> and tasks/<sid>/ under the
//	   collision rules of §9.4: an existing file with the same sha256 is
//	   skipped, a different one keeps both (the import lands as
//	   "<name>.imported-<id8>" / "<plan>.imported-<id8>.md" with a warning),
//	   an existing tasks/<sid>/ is left alone; all stamped like the sidecar
//	c  journal("transcript"); the transcript is copied to
//	   projects/<slug>/<sid>.jsonl.bffs-tmp, Stamp appended (after a "\n"
//	   when the copied bytes lack one), fsynced and renamed to
//	   projects/<slug>/<sid>.jsonl — the session becomes visible only here
//	d  the transcript mtime becomes max(OrigMtime, TranscriptFloor) when a
//	   floor applies, else OrigMtime
//	e  History lines are appended through AppendHistory
//	   (<configDir>/history.jsonl); journal("done")
//
// Every write goes through dest (MkdirAll 0700, OpenFile O_EXCL 0600,
// Rename, Chtimes); sources are read with plain os. A path that already
// exists where the sidecar or transcript must land — a file, a directory or
// a planted symlink — fails the commit before anything is written; so does
// a symlink anywhere on the way that leaves the root, because os.Root
// refuses to follow it. On any failure the steps are undone in reverse:
// everything this call created (its origin is still in staging) is removed
// and the destination is left as it was. journal may be nil.
func CommitSession(ctx context.Context, dest *os.Root, req CommitRequest, p MtimePolicy, journal func(step string) error) (CommitResult, error) {
	if err := validateRequest(req); err != nil {
		return CommitResult{}, fmt.Errorf("commit: %w", err)
	}
	if journal == nil {
		journal = func(string) error { return nil }
	}
	c := &committer{ctx: ctx, dest: dest, req: req, p: p}
	if err := c.run(journal); err != nil {
		if rerr := c.rollback(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rerr))
		}
		return CommitResult{}, fmt.Errorf("commit %s: %w", req.SessionID, err)
	}
	return c.res, nil
}

func validateRequest(req CommitRequest) error {
	if err := validateSessionID(req.SessionID); err != nil {
		return err
	}
	if err := validateSlug(req.Slug); err != nil {
		return err
	}
	if err := validateID8(req.ID8); err != nil {
		return err
	}
	if req.Transcript == "" {
		return errors.New("transcript source path required")
	}
	return nil
}

// committer carries one CommitSession call: the undo stack grows as the
// destination changes and is unwound in reverse on failure.
type committer struct {
	ctx  context.Context
	dest *os.Root
	req  CommitRequest
	p    MtimePolicy
	res  CommitResult
	undo []func() error
}

func (c *committer) run(journal func(string) error) error {
	sid := c.req.SessionID
	slugDir := filepath.Join(transcripts.ProjectsSubdir, c.req.Slug)
	sidecarFinal := filepath.Join(slugDir, sid)
	transcriptFinal := filepath.Join(slugDir, sid+transcripts.TranscriptExt)

	// Preflight: nothing may already stand where the session lands.
	if c.req.SidecarDir != "" {
		if err := c.mustBeAbsent(sidecarFinal); err != nil {
			return err
		}
	}
	if err := c.mustBeAbsent(transcriptFinal); err != nil {
		return err
	}

	// a — sidecar.
	if err := journal(StepSidecar); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	if err := c.ensureDir(slugDir); err != nil {
		return err
	}
	if c.req.SidecarDir != "" {
		tmp, err := c.mkdirTmp(slugDir, sid+tmpSuffix)
		if err != nil {
			return err
		}
		if err := c.copyTree(tmp, c.req.SidecarDir); err != nil {
			return err
		}
		if err := c.rename(tmp, sidecarFinal); err != nil {
			return err
		}
		c.placed(sidecarFinal + "/")
	}

	// b — file-history, plans, tasks.
	if err := c.placeFiles(filepath.Join(transcripts.FileHistorySubdir, sid), c.req.FileHistory, importedFileName); err != nil {
		return err
	}
	if err := c.placeFiles(transcripts.PlansSubdir, c.req.Plans, importedPlanName); err != nil {
		return err
	}
	if err := c.placeTasks(); err != nil {
		return err
	}

	// c — transcript.
	if err := journal(StepTranscript); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	tmp, err := c.copyTranscript(slugDir, sid)
	if err != nil {
		return err
	}
	if err := c.rename(tmp, transcriptFinal); err != nil {
		return err
	}
	syncDir(c.dest, slugDir)
	c.placed(transcriptFinal)

	// d — retention floor.
	mt, raised := transcriptMtime(c.req.OrigMtime, c.p)
	if err := c.dest.Chtimes(transcriptFinal, mt, mt); err != nil {
		return fmt.Errorf("set mtime on %q: %w", transcriptFinal, err)
	}
	c.res.Mtime, c.res.MtimeRaised = mt, raised

	// e — history.
	if len(c.req.History) > 0 {
		added, err := AppendHistory(filepath.Join(c.dest.Name(), transcripts.HistoryFile), sid, c.req.History, c.req.HistoryProject)
		if err != nil {
			return err
		}
		c.res.HistoryAdded = added
	}
	if err := journal(StepDone); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

func (c *committer) rollback() error {
	var errs []error
	for i := len(c.undo) - 1; i >= 0; i-- {
		if err := c.undo[i](); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *committer) placed(name string) {
	c.res.Placed = append(c.res.Placed, filepath.ToSlash(name))
}

func (c *committer) warn(msg string) {
	c.res.Warnings = append(c.res.Warnings, transcripts.Sanitize(msg))
}

// mustBeAbsent fails when anything — file, directory or symlink — exists
// at name, or when the path cannot be examined (a symlink leaving the root).
func (c *committer) mustBeAbsent(name string) error {
	_, err := c.dest.Lstat(name)
	switch {
	case err == nil:
		return fmt.Errorf("%q already exists", filepath.ToSlash(name))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check %q: %w", filepath.ToSlash(name), err)
	}
}

// ensureDir creates name and any missing parent, level by level, so every
// directory this call created is removed again on rollback — each only
// when it is empty by then.
func (c *committer) ensureDir(name string) error {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		dir := filepath.Join(parts[:i+1]...)
		_, err := c.dest.Lstat(dir)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("check %q: %w", filepath.ToSlash(dir), err)
		}
		if err := c.dest.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("create %q: %w", filepath.ToSlash(dir), err)
		}
		c.undo = append(c.undo, func() error { return c.dest.Remove(dir) })
	}
	return nil
}

// mkdirTmp creates <dir>/<base> exclusively, or <dir>/<base>-<rand> when
// that name is taken by an earlier leftover, and registers its removal.
func (c *committer) mkdirTmp(dir, base string) (string, error) {
	name := filepath.Join(dir, base)
	err := c.dest.Mkdir(name, 0o700)
	if errors.Is(err, fs.ErrExist) {
		name = filepath.Join(dir, base+"-"+randSuffix())
		err = c.dest.Mkdir(name, 0o700)
	}
	if err != nil {
		return "", fmt.Errorf("create %q: %w", filepath.ToSlash(name), err)
	}
	c.undo = append(c.undo, func() error { return c.dest.RemoveAll(name) })
	return name, nil
}

// openTmp creates the file <dir>/<base> exclusively, or <dir>/<base>-<rand>
// when that name is taken, and registers its removal.
func (c *committer) openTmp(dir, base string) (string, *os.File, error) {
	name := filepath.Join(dir, base)
	f, err := c.dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		name = filepath.Join(dir, base+"-"+randSuffix())
		f, err = c.dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return "", nil, fmt.Errorf("create %q: %w", filepath.ToSlash(name), err)
	}
	c.undo = append(c.undo, func() error { return c.dest.Remove(name) })
	return name, f, nil
}

func (c *committer) rename(oldname, newname string) error {
	if err := renameFn(c.dest, oldname, newname); err != nil {
		return fmt.Errorf("rename %q to %q: %w", filepath.ToSlash(oldname), filepath.ToSlash(newname), err)
	}
	c.undo = append(c.undo, func() error { return c.dest.RemoveAll(newname) })
	return nil
}

// copyTree copies the directory src (a plain path) into dst, an existing
// directory inside the root. Only directories and regular files may occur:
// staging never holds anything else.
func (c *committer) copyTree(dst, src string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := c.ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if err := c.dest.Mkdir(target, 0o700); err != nil {
				return fmt.Errorf("create %q: %w", filepath.ToSlash(target), err)
			}
			return nil
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return c.copyFile(target, p, artifactMtime(info.ModTime(), c.p))
		default:
			return fmt.Errorf("%q: unsupported file type %s", p, d.Type())
		}
	})
}

// copyFile copies the regular file src (a plain path) to name inside the
// root: O_EXCL, fsynced, stamped mtime. A failed copy removes the partial
// file.
func (c *committer) copyFile(name, src string, mtime time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q: not a regular file", src)
	}
	f, err := c.dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %q: %w", filepath.ToSlash(name), err)
	}
	fail := func(stage string, err error) error {
		_ = f.Close()
		_ = c.dest.Remove(name)
		return fmt.Errorf("%s %q: %w", stage, filepath.ToSlash(name), err)
	}
	if _, err := io.Copy(f, ctxReader{c.ctx, in}); err != nil {
		return fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return fail("fsync", err)
	}
	if err := f.Close(); err != nil {
		_ = c.dest.Remove(name)
		return fmt.Errorf("close %q: %w", filepath.ToSlash(name), err)
	}
	if err := c.dest.Chtimes(name, mtime, mtime); err != nil {
		return fmt.Errorf("set mtime on %q: %w", filepath.ToSlash(name), err)
	}
	return nil
}

// placeFiles lands each source file under dir by its base name, applying
// the §9.4 collision rule with alt naming the ".imported-<id8>" variant.
func (c *committer) placeFiles(dir string, sources []string, alt func(base, id8 string) string) error {
	if len(sources) == 0 {
		return nil
	}
	if err := c.ensureDir(dir); err != nil {
		return err
	}
	for _, src := range sources {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		base := filepath.Base(src)
		target := filepath.Join(dir, base)
		if _, err := c.dest.Lstat(target); err == nil {
			same, err := c.sameContent(target, src)
			if err != nil {
				return err
			}
			if same {
				continue
			}
			imported := filepath.Join(dir, alt(base, c.req.ID8))
			if _, err := c.dest.Lstat(imported); err == nil {
				same, err := c.sameContent(imported, src)
				if err != nil {
					return err
				}
				if same {
					continue
				}
				return fmt.Errorf("%q and %q both exist and differ from the imported file", filepath.ToSlash(target), filepath.ToSlash(imported))
			}
			c.warn(fmt.Sprintf("%s differs from the existing file; kept both, the import is %s", filepath.ToSlash(target), filepath.ToSlash(imported)))
			target = imported
		}
		if err := c.copyFile(target, src, artifactMtime(info.ModTime(), c.p)); err != nil {
			return err
		}
		c.undo = append(c.undo, func() error { return c.dest.Remove(target) })
		c.placed(target)
	}
	return nil
}

// placeTasks copies tasks/<sid>/ unless one already exists.
func (c *committer) placeTasks() error {
	if c.req.TasksDir == "" {
		return nil
	}
	target := filepath.Join(transcripts.TasksSubdir, c.req.SessionID)
	if _, err := c.dest.Lstat(target); err == nil {
		c.warn(fmt.Sprintf("%s already exists; the imported task list was not placed", filepath.ToSlash(target)))
		return nil
	}
	if err := c.ensureDir(transcripts.TasksSubdir); err != nil {
		return err
	}
	if err := c.dest.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create %q: %w", filepath.ToSlash(target), err)
	}
	c.undo = append(c.undo, func() error { return c.dest.RemoveAll(target) })
	if err := c.copyTree(target, c.req.TasksDir); err != nil {
		return err
	}
	c.placed(target + "/")
	return nil
}

// copyTranscript writes the transcript to its .bffs-tmp name, appending
// Stamp when given, and returns that name.
func (c *committer) copyTranscript(slugDir, sid string) (string, error) {
	in, err := os.Open(c.req.Transcript)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q: not a regular file", c.req.Transcript)
	}
	name, f, err := c.openTmp(slugDir, sid+transcripts.TranscriptExt+tmpSuffix)
	if err != nil {
		return "", err
	}
	tw := &tailWriter{w: f}
	if _, err := io.Copy(tw, ctxReader{c.ctx, in}); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write %q: %w", filepath.ToSlash(name), err)
	}
	if c.req.Stamp != nil {
		if tw.n > 0 && tw.last != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				_ = f.Close()
				return "", fmt.Errorf("write %q: %w", filepath.ToSlash(name), err)
			}
		}
		if _, err := f.Write(c.req.Stamp); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("write %q: %w", filepath.ToSlash(name), err)
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("fsync %q: %w", filepath.ToSlash(name), err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %q: %w", filepath.ToSlash(name), err)
	}
	return name, nil
}

// sameContent reports whether the file at name inside the root has the
// same bytes as the plain-path file src (size first, then sha256).
func (c *committer) sameContent(name, src string) (bool, error) {
	a, err := c.dest.Open(name)
	if err != nil {
		return false, err
	}
	defer a.Close()
	b, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer b.Close()
	ai, err := a.Stat()
	if err != nil {
		return false, err
	}
	bi, err := b.Stat()
	if err != nil {
		return false, err
	}
	if !ai.Mode().IsRegular() || ai.Size() != bi.Size() {
		return false, nil
	}
	ha, err := digest(ctxReader{c.ctx, a})
	if err != nil {
		return false, err
	}
	hb, err := digest(ctxReader{c.ctx, b})
	if err != nil {
		return false, err
	}
	return bytes.Equal(ha, hb), nil
}

func digest(r io.Reader) ([]byte, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// importedFileName is the collision name of a file-history backup:
// "<name>.imported-<id8>".
func importedFileName(base, id8 string) string {
	return base + importedInfix + id8
}

// importedPlanName is the collision name of a plan file:
// "<planSlug>.imported-<id8>.md" (the ".md" stays last so Claude still
// sees a markdown file).
func importedPlanName(base, id8 string) string {
	if stem, ok := strings.CutSuffix(base, ".md"); ok {
		return stem + importedInfix + id8 + ".md"
	}
	return base + importedInfix + id8
}

// syncDir fsyncs a directory inside the root on a best-effort basis so a
// rename into it survives a crash; filesystems that refuse are ignored.
func syncDir(dest *os.Root, name string) {
	d, err := dest.Open(name)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// ctxReader fails every Read once ctx is done, so a long copy stops
// promptly on cancellation.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// tailWriter forwards writes and remembers the last byte written.
type tailWriter struct {
	w    io.Writer
	n    int64
	last byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.last = p[n-1]
		t.n += int64(n)
	}
	return n, err
}
