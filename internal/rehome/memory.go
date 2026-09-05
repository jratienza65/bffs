package rehome

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// ErrMemoryMergeLater is returned by MergeMemory for MemoryMerge until the
// merge itself ships (M6).
var ErrMemoryMergeLater = errors.New("memory merge arrives in a later milestone")

const (
	pinnedKey         = "pinned"
	pinnedImportedKey = "pinned-imported"
)

// memSrc is one file of the source memory directory.
type memSrc struct {
	rel  string // slash-separated, relative to the memory dir
	abs  string
	info fs.FileInfo
}

// MergeMemory writes the auto-memory directory srcDir (staged from a
// bundle) to dstDir, the memory directory transcripts.MemoryDirFor computed
// for the placement, under plan §9.9. Imported memory is prompt content:
// every topic file (*.md except MEMORY.md, plus logs/**) has its
// frontmatter "pinned:" key rewritten to "pinned-imported:" unless
// trustMemory, so nothing arrives pinned into every future session.
//
// mode decides what happens when dstDir exists: MemorySkip writes only when
// it does not (otherwise every file is reported Unchanged and nothing is
// written); MemoryOverwrite renames the existing directory to
// "<dstDir>.bffs-replaced-<epochms>" — never deleted — and writes fresh.
// MemoryMerge is refused with ErrMemoryMergeLater until M6.
//
// confirmed says the placement was chosen by the user (a mapping, --into
// or an interactive answer). Unconfirmed, in every mode, topic files land
// as "<name>.imported-<id8>.md" and MEMORY.md is neither created nor
// edited, so an as-is or -y import never injects an index into a project.
// Confirmed, files land under their own names and MEMORY.md is copied
// into the fresh directory. Topic files keep their source mtimes (fresh
// ones would displace the user's newest pinned files); a copied MEMORY.md
// gets now. id8 is bundle_id[:8].
//
// The directory is built beside its destination as "<dstDir>.bffs-tmp" and
// renamed into place once complete, so a failure leaves no half-written
// memory. Remaining is the absolute-path scan of dstDir after a write. No
// memory is ever deleted.
//
// dstDir is "<projects>/<slug>/memory" (transcripts.MemoryDirFor's shape);
// every write goes through an os.Root opened over that projects/
// directory, so a symlink planted at "<slug>" or at the memory directory
// that leads out of the pool is refused, never followed — the same rule
// CommitSession applies to sessions.
func MergeMemory(dstDir, srcDir string, mode MemoryMode, id8 string, confirmed, trustMemory bool, now time.Time) (MemoryMove, error) {
	switch mode {
	case MemorySkip, MemoryOverwrite:
	case MemoryMerge:
		return MemoryMove{}, fmt.Errorf("memory %q: %w", dstDir, ErrMemoryMergeLater)
	default:
		return MemoryMove{}, fmt.Errorf("unknown memory mode %q", string(mode))
	}
	if err := validateID8(id8); err != nil {
		return MemoryMove{}, fmt.Errorf("memory: %w", err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	topics, index, err := listMemorySource(srcDir)
	if err != nil {
		return MemoryMove{}, fmt.Errorf("memory: %w", err)
	}
	mv := MemoryMove{From: srcDir, To: dstDir}

	// The root is the projects/ pool two levels up; memRel is the
	// destination relative to it ("<slug>/memory").
	slugDir := filepath.Dir(dstDir)
	poolDir := filepath.Dir(slugDir)
	memRel := filepath.Join(filepath.Base(slugDir), filepath.Base(dstDir))
	if err := os.MkdirAll(poolDir, 0o700); err != nil {
		return MemoryMove{}, fmt.Errorf("memory: create %q: %w", poolDir, err)
	}
	root, err := os.OpenRoot(poolDir)
	if err != nil {
		return MemoryMove{}, fmt.Errorf("memory: open %q: %w", poolDir, err)
	}
	defer root.Close()

	_, lerr := root.Lstat(memRel)
	dstExists := lerr == nil
	if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
		return MemoryMove{}, fmt.Errorf("memory: check %q: %w", dstDir, lerr)
	}
	if dstExists && mode == MemorySkip {
		for _, t := range topics {
			mv.Unchanged = append(mv.Unchanged, t.rel)
		}
		if index != nil {
			mv.Unchanged = append(mv.Unchanged, transcripts.MemoryIndexFile)
		}
		return mv, nil
	}
	if index != nil && !confirmed {
		mv.Unchanged = append(mv.Unchanged, transcripts.MemoryIndexFile)
	}
	if len(topics) == 0 && (index == nil || !confirmed) {
		return mv, nil // nothing to write: no directory is created
	}

	tmp := memRel + tmpSuffix
	if _, err := root.Lstat(tmp); err == nil {
		tmp = memRel + tmpSuffix + "-" + randSuffix()
	}
	if err := root.MkdirAll(tmp, 0o700); err != nil {
		return MemoryMove{}, fmt.Errorf("memory: create %q: %w", filepath.Join(poolDir, tmp), err)
	}
	fail := func(err error) (MemoryMove, error) {
		_ = root.RemoveAll(tmp)
		return MemoryMove{}, fmt.Errorf("memory: %w", err)
	}
	for _, t := range topics {
		rel := t.rel
		if !confirmed {
			rel = path.Join(path.Dir(rel), importedPlanName(path.Base(rel), id8))
		}
		target := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := root.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fail(err)
		}
		if err := writeTopic(root, target, t.abs, !trustMemory); err != nil {
			return fail(err)
		}
		if err := root.Chtimes(target, t.info.ModTime(), t.info.ModTime()); err != nil {
			return fail(err)
		}
		if confirmed {
			mv.Added = append(mv.Added, rel)
		} else {
			mv.Renamed = append(mv.Renamed, rel)
		}
	}
	if index != nil && confirmed {
		target := filepath.Join(tmp, transcripts.MemoryIndexFile)
		if err := writeTopic(root, target, index.abs, false); err != nil {
			return fail(err)
		}
		if err := root.Chtimes(target, now, now); err != nil {
			return fail(err)
		}
		mv.Added = append(mv.Added, transcripts.MemoryIndexFile)
	}

	if dstExists { // MemoryOverwrite: set the existing directory aside first
		aside := SetAsideName(memRel, now)
		if _, err := root.Lstat(aside); err == nil {
			return fail(fmt.Errorf("set-aside %q already exists", filepath.Join(poolDir, aside)))
		}
		if err := root.Rename(memRel, aside); err != nil {
			return fail(err)
		}
	}
	if err := root.Rename(tmp, memRel); err != nil {
		return fail(err)
	}
	if refs, err := transcripts.ScanAbsolutePaths(dstDir); err == nil {
		mv.Remaining = refs
	}
	return mv, nil
}

// listMemorySource lists the topic files of srcDir — *.md at the top
// level except MEMORY.md, and *.md under logs/ (skipping proposals/ and
// index* directories, which Claude never loads) — sorted by name, plus the
// MEMORY.md entry when present.
func listMemorySource(srcDir string) (topics []memSrc, index *memSrc, err error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		m := memSrc{rel: e.Name(), abs: filepath.Join(srcDir, e.Name()), info: info}
		if e.Name() == transcripts.MemoryIndexFile {
			index = &m
			continue
		}
		topics = append(topics, m)
	}
	logs := filepath.Join(srcDir, transcripts.MemoryLogsSubdir)
	err = filepath.WalkDir(logs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == logs && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if p != logs && (d.Name() == transcripts.MemoryProposalsSubdir || strings.HasPrefix(d.Name(), "index")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		topics = append(topics, memSrc{rel: filepath.ToSlash(rel), abs: p, info: info})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk %q: %w", logs, err)
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].rel < topics[j].rel })
	return topics, index, nil
}

// writeTopic copies the memory file src (a plain path in staging) to dst,
// a name inside root (O_EXCL, 0600, fsynced), rewriting the frontmatter
// key "pinned" to "pinned-imported" when neutralise is set. Only the
// leading YAML block (a first line of "---" up to the next "---") is
// examined; the body is copied byte for byte.
func writeTopic(root *os.Root, dst, src string, neutralise bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	fail := func(err error) error {
		_ = out.Close()
		_ = root.Remove(dst)
		return fmt.Errorf("write %q: %w", filepath.ToSlash(dst), err)
	}
	r := bufio.NewReaderSize(in, 64*1024)
	lineNo := 0
	inFront := false
	for {
		line, rerr := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(line)
			switch {
			case lineNo == 1 && trimmed == "---":
				inFront = true
			case inFront && trimmed == "---":
				inFront = false
			case inFront && neutralise:
				line = neutralisePinned(line)
			}
			if _, err := w.WriteString(line); err != nil {
				return fail(err)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return fail(rerr)
		}
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = root.Remove(dst)
		return fmt.Errorf("close %q: %w", filepath.ToSlash(dst), err)
	}
	return nil
}

// neutralisePinned rewrites a frontmatter line whose key is exactly
// "pinned" (the key transcripts reads, whitespace-trimmed) so the key
// becomes "pinned-imported"; any other line is returned unchanged.
func neutralisePinned(line string) string {
	k, _, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok || strings.TrimSpace(k) != pinnedKey {
		return line
	}
	i := strings.Index(line, pinnedKey)
	return line[:i] + pinnedImportedKey + line[i+len(pinnedKey):]
}
