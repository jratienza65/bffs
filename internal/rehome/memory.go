package rehome

import (
	"bufio"
	"bytes"
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

// ErrMemoryMergeLater was returned by MergeMemory for MemoryMerge before
// the merge shipped. It is no longer returned; the variable stays so
// callers that compare against it keep compiling.
var ErrMemoryMergeLater = errors.New("memory merge arrives in a later milestone")

const (
	pinnedKey         = "pinned"
	pinnedImportedKey = "pinned-imported"

	// indexMaxLines and indexMaxChars are what Claude loads of MEMORY.md
	// before truncating it with a warning (disk.md §5).
	indexMaxLines = 200
	indexMaxChars = 25_000

	// maxMemoryFile bounds one memory file read into memory.
	maxMemoryFile = 64 << 20
)

// memSrc is one file of the source memory directory.
type memSrc struct {
	rel  string // slash-separated, relative to the memory dir
	abs  string
	info fs.FileInfo
}

// MergeMemory writes the auto-memory directory srcDir (staged from a
// bundle, or the memory of a project being rehomed) to dstDir, the memory
// directory transcripts.MemoryDirFor computed for the placement, under
// plan §9.9. Imported memory is prompt content: every topic file (*.md
// except MEMORY.md, plus logs/**) has its frontmatter "pinned:" key
// rewritten to "pinned-imported:" unless trustMemory, so nothing arrives
// pinned into every future session.
//
// mode decides what happens when dstDir exists: MemorySkip writes only
// when it does not (otherwise every file is reported Unchanged and nothing
// is written); MemoryOverwrite renames the existing directory to
// "<dstDir>.bffs-replaced-<epochms>" — never deleted — and writes fresh;
// MemoryMerge merges file by file: an absent file is added, an identical
// one (same bytes) is Unchanged, a different one lands beside it as
// "<name>.imported-<id8>.md" (Renamed), logs/ files follow the same rule,
// and an existing MEMORY.md gains one "## Imported (bundle <id8>, <date>)"
// section with one "- [title](file.md) - description" line per top-level
// file added or renamed (title and description from the file's
// frontmatter, else its first heading; existing lines are never touched).
// Warnings notes an index that exceeds the 200 lines / 25 000 characters
// Claude loads.
//
// confirmed says the placement was chosen by the user (a mapping, --into
// or an interactive answer). Unconfirmed, in every mode, topic files land
// as "<name>.imported-<id8>.md" and MEMORY.md is neither created nor
// edited, so an as-is or -y import never injects an index into a project.
// Confirmed, files land under their own names and MEMORY.md is copied
// into a fresh directory. Topic files keep their source mtimes (fresh
// ones would displace the user's newest pinned files); a copied or
// appended MEMORY.md gets now. id8 is bundle_id[:8].
//
// A fresh directory is built beside its destination as "<dstDir>.bffs-tmp"
// and renamed into place once complete, so a failure leaves no
// half-written memory; a merge writes each file exclusively and replaces
// MEMORY.md atomically. Remaining is the absolute-path scan of dstDir
// after a write. No memory is ever deleted.
//
// dstDir is "<projects>/<slug>/memory" (transcripts.MemoryDirFor's shape);
// every write goes through an os.Root opened over that projects/
// directory, so a symlink planted at "<slug>" or at the memory directory
// that leads out of the pool is refused, never followed — the same rule
// CommitSession applies to sessions.
func MergeMemory(dstDir, srcDir string, mode MemoryMode, id8 string, confirmed, trustMemory bool, now time.Time) (MemoryMove, error) {
	switch mode {
	case MemorySkip, MemoryOverwrite, MemoryMerge:
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
	if dstExists && mode == MemoryMerge {
		m := &merger{root: root, memRel: memRel, dstDir: dstDir, id8: id8, confirmed: confirmed, trust: trustMemory, now: now, mv: mv}
		if err := m.run(topics, index); err != nil {
			return MemoryMove{}, fmt.Errorf("memory: %w", err)
		}
		return m.mv, nil
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
			rel = importedRel(rel, id8)
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

// merger carries one merge into an existing memory directory.
type merger struct {
	root      *os.Root
	memRel    string
	dstDir    string
	id8       string
	confirmed bool
	trust     bool
	now       time.Time
	mv        MemoryMove
	wrote     bool
	index     []indexLine // lines for the MEMORY.md section
}

// indexLine is one "- [title](file) - description" entry.
type indexLine struct {
	rel, title, description string
}

func (m *merger) run(topics []memSrc, index *memSrc) error {
	for _, t := range topics {
		if err := m.topic(t); err != nil {
			return err
		}
	}
	if index != nil {
		if err := m.mergeIndex(index); err != nil {
			return err
		}
	}
	if m.wrote {
		if refs, err := transcripts.ScanAbsolutePaths(m.dstDir); err == nil {
			m.mv.Remaining = refs
		}
	}
	return nil
}

// topic lands one source file under the §9.9 rule.
func (m *merger) topic(t memSrc) error {
	data, err := readTopic(t.abs, !m.trust)
	if err != nil {
		return err
	}
	rel := t.rel
	if !m.confirmed {
		rel = importedRel(rel, m.id8)
	}
	same, exists, err := m.compare(rel, data)
	if err != nil {
		return err
	}
	switch {
	case !exists:
		if err := m.write(rel, data, t.info.ModTime()); err != nil {
			return err
		}
		m.note(rel, t.rel, data, m.confirmed)
		return nil
	case same:
		m.mv.Unchanged = append(m.mv.Unchanged, rel)
		return nil
	case !m.confirmed:
		m.warn(fmt.Sprintf("%s: kept the existing file; the imported one differs from it", rel))
		m.mv.Unchanged = append(m.mv.Unchanged, rel)
		return nil
	}
	alt := importedRel(t.rel, m.id8)
	same, exists, err = m.compare(alt, data)
	if err != nil {
		return err
	}
	switch {
	case !exists:
		if err := m.write(alt, data, t.info.ModTime()); err != nil {
			return err
		}
		m.note(alt, t.rel, data, false)
	case same:
		m.mv.Unchanged = append(m.mv.Unchanged, alt)
	default:
		m.warn(fmt.Sprintf("%s and %s both exist and differ from the imported file; kept both", rel, alt))
		m.mv.Unchanged = append(m.mv.Unchanged, rel)
	}
	return nil
}

// note records a written file as Added or Renamed and, for a top-level
// topic file, queues its index line.
func (m *merger) note(rel, srcRel string, data []byte, added bool) {
	if added {
		m.mv.Added = append(m.mv.Added, rel)
	} else {
		m.mv.Renamed = append(m.mv.Renamed, rel)
	}
	if !strings.Contains(rel, "/") {
		title, desc := topicTitle(data, srcRel)
		m.index = append(m.index, indexLine{rel: rel, title: title, description: desc})
	}
}

// mergeIndex copies MEMORY.md when the destination has none and the
// placement is confirmed, appends the imported section when it has one
// and files were written, and leaves it alone otherwise.
func (m *merger) mergeIndex(index *memSrc) error {
	const name = transcripts.MemoryIndexFile
	if !m.confirmed {
		m.mv.Unchanged = append(m.mv.Unchanged, name)
		return nil
	}
	target := filepath.Join(m.memRel, name)
	existing, err := m.read(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data, err := readTopic(index.abs, false)
		if err != nil {
			return err
		}
		if err := m.write(name, data, m.now); err != nil {
			return err
		}
		m.mv.Added = append(m.mv.Added, name)
		m.checkIndexSize(data)
		return nil
	case err != nil:
		return err
	}
	lines := m.newIndexLines(existing)
	if len(lines) == 0 {
		m.mv.Unchanged = append(m.mv.Unchanged, name)
		return nil
	}
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		buf.WriteByte('\n')
	}
	fmt.Fprintf(&buf, "\n## Imported (bundle %s, %s)\n", m.id8, m.now.Format("2006-01-02"))
	for _, l := range lines {
		fmt.Fprintf(&buf, "- [%s](%s)", l.title, l.rel)
		if l.description != "" {
			fmt.Fprintf(&buf, " - %s", l.description)
		}
		buf.WriteByte('\n')
	}
	if err := m.replace(target, buf.Bytes(), m.now); err != nil {
		return err
	}
	m.mv.IndexAppended = true
	m.wrote = true
	m.checkIndexSize(buf.Bytes())
	return nil
}

// newIndexLines drops the queued lines whose file the index already
// links, so a repeated merge never lists a file twice.
func (m *merger) newIndexLines(existing []byte) []indexLine {
	var out []indexLine
	for _, l := range m.index {
		if bytes.Contains(existing, []byte("]("+l.rel+")")) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// checkIndexSize warns when the index exceeds what Claude loads.
func (m *merger) checkIndexSize(data []byte) {
	lines := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	chars := len(bytes.Runes(data))
	if lines > indexMaxLines || chars > indexMaxChars {
		m.warn(fmt.Sprintf("%s is %d lines / %d characters; claude loads only the first %d lines / %d characters — move detail into topic files", transcripts.MemoryIndexFile, lines, chars, indexMaxLines, indexMaxChars))
	}
}

func (m *merger) warn(msg string) {
	m.mv.Warnings = append(m.mv.Warnings, transcripts.Sanitize(msg))
}

// compare reports whether rel exists in the destination and whether it
// holds exactly data.
func (m *merger) compare(rel string, data []byte) (same, exists bool, err error) {
	got, err := m.read(filepath.Join(m.memRel, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	return bytes.Equal(got, data), true, nil
}

// read returns the bytes of name inside the root (bounded).
func (m *merger) read(name string) ([]byte, error) {
	f, err := m.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", filepath.ToSlash(name))
	}
	if info.Size() > maxMemoryFile {
		return nil, fmt.Errorf("%q is larger than %d bytes", filepath.ToSlash(name), maxMemoryFile)
	}
	return io.ReadAll(f)
}

// write creates rel (relative to the memory dir) exclusively with data
// and stamps mtime.
func (m *merger) write(rel string, data []byte, mtime time.Time) error {
	target := filepath.Join(m.memRel, filepath.FromSlash(rel))
	if err := m.root.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	if err := writeExcl(m.root, target, data); err != nil {
		return err
	}
	if err := m.root.Chtimes(target, mtime, mtime); err != nil {
		return err
	}
	m.wrote = true
	return nil
}

// replace writes data to a temp file beside name and renames it over
// name, so a reader sees the old index or the new one, never a torn file.
func (m *merger) replace(name string, data []byte, mtime time.Time) error {
	tmp := name + tmpSuffix + "-" + randSuffix()
	if err := writeExcl(m.root, tmp, data); err != nil {
		return err
	}
	if err := m.root.Chtimes(tmp, mtime, mtime); err != nil {
		_ = m.root.Remove(tmp)
		return err
	}
	if err := m.root.Rename(tmp, name); err != nil {
		_ = m.root.Remove(tmp)
		return err
	}
	return nil
}

// importedRel is the side-file name of a memory file:
// "<dir>/<name>.imported-<id8>.md".
func importedRel(rel, id8 string) string {
	return path.Join(path.Dir(rel), importedPlanName(path.Base(rel), id8))
}

// topicTitle picks the index title and description of a topic file: the
// frontmatter "name:" and "description:" values, else the first "# "
// heading, else the file's stem.
func topicTitle(data []byte, rel string) (title, description string) {
	lines := strings.Split(string(data), "\n")
	inFront := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case i == 0 && trimmed == "---":
			inFront = true
			continue
		case inFront && trimmed == "---":
			inFront = false
			continue
		case inFront:
			k, v, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch strings.TrimSpace(k) {
			case "name":
				if title == "" {
					title = v
				}
			case "description":
				if description == "" {
					description = v
				}
			}
			continue
		}
		if title == "" && strings.HasPrefix(trimmed, "# ") {
			title = strings.TrimSpace(trimmed[2:])
		}
		if title != "" && !inFront {
			break
		}
	}
	if title == "" {
		title = strings.TrimSuffix(path.Base(rel), ".md")
	}
	return transcripts.Sanitize(title), transcripts.Sanitize(description)
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

// writeTopic copies the memory file src (a plain path) to dst, a name
// inside root (O_EXCL, 0600, fsynced), rewriting the frontmatter key
// "pinned" to "pinned-imported" when neutralise is set.
func writeTopic(root *os.Root, dst, src string, neutralise bool) error {
	data, err := readTopic(src, neutralise)
	if err != nil {
		return err
	}
	return writeExcl(root, dst, data)
}

// readTopic reads the memory file src, rewriting the frontmatter key
// "pinned" to "pinned-imported" when neutralise is set. Only the leading
// YAML block (a first line of "---" up to the next "---") is examined;
// the body is kept byte for byte.
func readTopic(src string, neutralise bool) ([]byte, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", src)
	}
	if info.Size() > maxMemoryFile {
		return nil, fmt.Errorf("%q is larger than %d bytes", src, maxMemoryFile)
	}
	var out bytes.Buffer
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
			out.WriteString(line)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read %q: %w", src, rerr)
		}
	}
	return out.Bytes(), nil
}

// writeExcl creates dst inside root exclusively (0600) with data, fsynced;
// a failed write removes the partial file.
func writeExcl(root *os.Root, dst string, data []byte) error {
	out, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = out.Close()
		_ = root.Remove(dst)
		return fmt.Errorf("write %q: %w", filepath.ToSlash(dst), err)
	}
	if _, err := out.Write(data); err != nil {
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
