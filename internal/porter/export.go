package porter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// ExportOptions shapes a bundle. Compression is the envelope byte; Version
// is the bffs version stamped as bffs_version; Account and Isolation
// describe the account the export runs as (source.account,
// source.account_type, source.isolation); Now is the manifest clock (zero
// = time.Now()); Progress, when set, is called during the hashing pre-pass
// (Phase "hash") and by bundle.Build (Phase "build").
type ExportOptions struct {
	Compression bundle.Compression
	Version     string
	Account     store.Account
	Isolation   store.IsolationPreset
	Now         time.Time
	Progress    func(bundle.Progress)
}

const (
	// gitRemoteTimeout bounds the best-effort `git remote get-url origin`.
	gitRemoteTimeout = 2 * time.Second

	// envTransferCode never reaches a child process.
	envTransferCode = "BFFS_TRANSFER_CODE"

	// historyMaxLine and historyMaxLines mirror rehome.AppendHistory's
	// caps so an export never ships lines the importer would drop.
	historyMaxLine  = 1 << 20
	historyMaxLines = 10_000

	// fallbackHostname is used when os.Hostname yields nothing usable.
	fallbackHostname = "host"
)

// BuildManifest turns sel into a manifest (plan §6.3) and the Opener that
// serves its bytes. The pre-pass sizes and sha256-hashes every candidate
// file; every name is checked with bundle.ValidateEntryName and
// bundle.ClassifyName and an out-of-grammar file is skipped with a warning
// ("skipped <path>: <reason>") rather than failing the export. Symlinks
// inside the tree are skipped with a warning; the root's projects/
// directory is resolved once. Live transcripts are opened during the
// pre-pass and the handle is kept, so bundle.Build streams the inode the
// manifest describes even if claude compacts and renames the file
// meanwhile. history/<sid>.jsonl is synthesised from <ConfigDir>/
// history.jsonl and served from memory.
//
// The returned Opener also implements io.Closer: Write and Export close it
// after building; a caller that stops before building must Close it to
// release the live handles.
func BuildManifest(ctx context.Context, sel Selection, o ExportOptions) (*bundle.Manifest, bundle.Opener, []string, error) {
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	id, err := newBundleID()
	if err != nil {
		return nil, nil, nil, err
	}
	b := &builder{
		ctx:      ctx,
		root:     sel.Root,
		parts:    sel.Parts,
		progress: o.Progress,
		src:      &opener{files: map[string]source{}},
		lower:    map[string]string{},
	}

	m := &bundle.Manifest{
		Format:      bundle.FormatVersion,
		BundleID:    id,
		BFFSVersion: o.Version,
		Created:     now,
		Source:      sourceInfo(sel.Root, o),
	}

	sessions := append([]transcripts.Session(nil), sel.Sessions...)
	sortNewestFirst(sessions)
	for _, s := range sessions {
		if s.Version != "" {
			m.ClaudeVersion = sanitize(s.Version)
			break
		}
	}

	wantHistory := map[string]bool{}
	if sel.Parts.History {
		for _, s := range sessions {
			wantHistory[s.ID] = true
		}
	}
	history, err := collectHistory(ctx, filepath.Join(sel.Root.ConfigDir, transcripts.HistoryFile), wantHistory)
	if err != nil {
		b.src.Close()
		return nil, nil, nil, err
	}
	b.history = history

	flags, _ := claudejson.ReadProjectFlags(sel.Root.ClaudeJSON)
	b.flags = flags

	for _, s := range sessions {
		if err := ctx.Err(); err != nil {
			b.src.Close()
			return nil, nil, nil, err
		}
		e, ok, err := b.sessionEntry(s)
		if err != nil {
			b.src.Close()
			return nil, nil, nil, err
		}
		if ok {
			m.Entries = append(m.Entries, e)
		}
	}
	for _, mem := range sel.Memories {
		if err := ctx.Err(); err != nil {
			b.src.Close()
			return nil, nil, nil, err
		}
		var keep map[string]bool
		if names, ok := sel.MemoryFiles[mem.Dir]; ok {
			keep = make(map[string]bool, len(names))
			for _, n := range names {
				keep[n] = true
			}
		}
		e, ok, err := b.memoryEntry(mem, keep)
		if err != nil {
			b.src.Close()
			return nil, nil, nil, err
		}
		if ok {
			m.Entries = append(m.Entries, e)
		}
	}
	m.Totals = bundle.Totals{Entries: len(m.Entries)}
	for _, e := range m.Entries {
		for _, f := range e.Files {
			m.Totals.Files++
			m.Totals.Bytes += f.Size
		}
	}
	if len(m.Entries) == 0 {
		b.src.Close()
		return nil, nil, b.warnings, errors.New("nothing to export: the selection holds no session or memory")
	}
	return m, b.src, b.warnings, nil
}

// Write streams a bundle described by m to w through bundle.Build with
// o.Compression and o.Progress, and returns the manifest bytes written as
// entry 0. src is closed afterwards when it implements io.Closer, so the
// live handles BuildManifest kept are released either way.
func Write(ctx context.Context, w io.Writer, m *bundle.Manifest, src bundle.Opener, o ExportOptions) ([]byte, error) {
	if c, ok := src.(io.Closer); ok {
		defer c.Close()
	}
	return bundle.Build(ctx, w, m, src, o.Compression, o.Progress)
}

// Export is BuildManifest followed by Write: the convenience path for a
// caller that does not need the pre-pass warnings before writing (they
// are dropped here; call BuildManifest and Write separately to show
// them). It returns the manifest and the exact bytes of entry 0.
func Export(ctx context.Context, sel Selection, w io.Writer, o ExportOptions) (*bundle.Manifest, []byte, error) {
	m, src, _, err := BuildManifest(ctx, sel, o)
	if err != nil {
		return nil, nil, err
	}
	raw, err := Write(ctx, w, m, src, o)
	if err != nil {
		return nil, nil, err
	}
	return m, raw, nil
}

// builder accumulates one manifest.
type builder struct {
	ctx      context.Context
	root     transcripts.Root
	parts    Parts
	progress func(bundle.Progress)
	src      *opener
	history  map[string][]byte
	flags    map[string]claudejson.ProjectFlags
	warnings []string
	lower    map[string]string // lower-cased bundle path → path as listed
	hashed   int
	bytes    int64
}

func (b *builder) warn(format string, args ...any) {
	b.warnings = append(b.warnings, sanitize(fmt.Sprintf(format, args...)))
}

func (b *builder) skip(p, reason string) {
	b.warn("skipped %s: %s", p, reason)
}

// admit checks a candidate bundle name against the grammar and the
// case-insensitive uniqueness rule; a rejection is a warning, never an
// error.
func (b *builder) admit(name string) bool {
	if err := bundle.ValidateEntryName(name); err != nil {
		b.skip(name, err.Error())
		return false
	}
	if _, _, _, err := bundle.ClassifyName(name); err != nil {
		b.skip(name, err.Error())
		return false
	}
	low := strings.ToLower(name)
	if prev, dup := b.lower[low]; dup {
		if prev != name {
			b.skip(name, fmt.Sprintf("collides with %q on a case-insensitive filesystem", prev))
		}
		// The same path listed twice — a plan file two sessions share —
		// travels once, with the first session that claimed it.
		return false
	}
	b.lower[low] = name
	return true
}

// addFile hashes the regular file at abs and lists it under name.
// keepOpen keeps the handle for the Opener (live transcripts). A file
// that cannot be read is skipped with a warning unless required.
func (b *builder) addFile(files *[]bundle.File, name, abs string, keepOpen, required bool) (bool, error) {
	info, err := os.Lstat(abs)
	if err != nil {
		if required {
			return false, fmt.Errorf("%s: %w", abs, err)
		}
		b.skip(name, err.Error())
		return false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if required {
			return false, fmt.Errorf("%s is a symlink; bffs never follows links inside the tree", abs)
		}
		b.skip(name, "symlink (never followed)")
		return false, nil
	}
	if !info.Mode().IsRegular() {
		if required {
			return false, fmt.Errorf("%s is not a regular file", abs)
		}
		b.skip(name, "not a regular file")
		return false, nil
	}
	if !b.admit(name) {
		return false, nil
	}
	f, err := os.Open(abs)
	if err != nil {
		if required {
			return false, err
		}
		b.skip(name, err.Error())
		return false, nil
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		if required {
			return false, err
		}
		b.skip(name, err.Error())
		return false, nil
	}
	size := stat.Size()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(&ctxReader{ctx: b.ctx, r: f}, size))
	if err != nil {
		f.Close()
		if required || b.ctx.Err() != nil {
			return false, fmt.Errorf("hash %s: %w", abs, err)
		}
		b.skip(name, err.Error())
		return false, nil
	}
	if n != size {
		// Truncated under us: describe what we could read; Build will
		// re-read the same range from the kept handle or the path.
		size = n
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if keepOpen {
		b.src.files[name] = source{file: f, size: size}
	} else {
		f.Close()
		b.src.files[name] = source{path: abs}
	}
	*files = append(*files, bundle.File{Path: name, Size: size, SHA256: digest, ModTime: stat.ModTime()})
	b.hashed++
	b.bytes += size
	if b.progress != nil {
		b.progress(bundle.Progress{Phase: "hash", Files: b.hashed, Bytes: b.bytes, Current: name})
	}
	return true, nil
}

// addBytes lists an in-memory file.
func (b *builder) addBytes(files *[]bundle.File, name string, data []byte, mtime time.Time) bool {
	if !b.admit(name) {
		return false
	}
	sum := sha256.Sum256(data)
	b.src.files[name] = source{data: data}
	*files = append(*files, bundle.File{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ModTime: mtime})
	b.hashed++
	b.bytes += int64(len(data))
	return true
}

// walkTree lists every regular file under dir as prefix/<rel>; skip
// decides per relative slash path whether a subtree or file is left out
// silently. Symlinks are skipped with a warning.
func (b *builder) walkTree(files *[]bundle.File, prefix, dir string, skip func(rel string, isDir bool) bool) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		b.skip(prefix, err.Error())
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		b.skip(prefix, "symlink (never followed)")
		return nil
	}
	if !info.IsDir() {
		b.skip(prefix, "not a directory")
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				b.skip(prefix, err.Error())
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			b.skip(joinSlash(prefix, filepath.ToSlash(rel)), err.Error())
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if cerr := b.ctx.Err(); cerr != nil {
			return cerr
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		name := joinSlash(prefix, relSlash)
		if skip != nil && skip(relSlash, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			b.skip(name, "symlink (never followed)")
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			b.skip(name, "not a regular file")
			return nil
		}
		_, err = b.addFile(files, name, p, false, false)
		return err
	})
}

// sessionEntry builds the manifest entry of s. ok is false when the
// session was skipped with a warning (an unusable slug, a transcript
// symlink).
func (b *builder) sessionEntry(s transcripts.Session) (bundle.Entry, bool, error) {
	slug := s.Slug
	sid := s.ID
	if slug == "" || sid == "" || s.Path == "" {
		b.warn("skipped session %s: incomplete listing (slug %q)", sid, slug)
		return bundle.Entry{}, false, nil
	}
	transcriptName := joinSlash("projects", slug, sid+transcripts.TranscriptExt)
	if err := bundle.ValidateEntryName(transcriptName); err != nil {
		b.warn("skipped session %s: %v", sid, err)
		return bundle.Entry{}, false, nil
	}
	if kind, _, _, err := bundle.ClassifyName(transcriptName); err != nil || kind != bundle.NameTranscript {
		b.warn("skipped session %s: projects/%s is not a bundle slug", sid, slug)
		return bundle.Entry{}, false, nil
	}

	e := bundle.Entry{
		Kind:                  bundle.EntrySession,
		Slug:                  slug,
		Cwd:                   s.Cwd,
		SessionID:             sid,
		Title:                 sanitize(s.Title),
		GitBranch:             sanitize(s.GitBranch),
		Started:               s.FirstTS,
		Last:                  s.LastTS,
		LivePossiblyTruncated: s.Live,
	}
	e.ProjectKey = s.Cwd
	if s.Cwd != "" && dirExists(s.Cwd) {
		if key, err := transcripts.ProjectKey(s.Cwd); err == nil {
			e.ProjectKey = key
		}
		e.GitRemote = gitRemote(b.ctx, s.Cwd)
	}
	if e.ProjectKey != "" {
		if pf, ok := b.flags[e.ProjectKey]; ok {
			e.SourceTrust = &bundle.TrustInfo{
				Accepted:                     deref(pf.TrustAccepted),
				ExternalIncludesApproved:     deref(pf.ExternalIncludesApproved),
				ExternalIncludesWarningShown: deref(pf.ExternalIncludesWarningShown),
			}
		}
	}

	has := map[string]bool{}
	// The transcript: required; a live one keeps its handle open.
	info, err := os.Lstat(s.Path)
	if err != nil {
		return bundle.Entry{}, false, fmt.Errorf("session %s: %w", sid, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		b.warn("skipped session %s: %s is a symlink (never followed)", sid, s.Path)
		return bundle.Entry{}, false, nil
	}
	ok, err := b.addFile(&e.Files, transcriptName, s.Path, s.Live, true)
	if err != nil {
		return bundle.Entry{}, false, fmt.Errorf("session %s: %w", sid, err)
	}
	if !ok {
		b.warn("skipped session %s: transcript name rejected", sid)
		return bundle.Entry{}, false, nil
	}
	has["transcript"] = true

	art := transcripts.ArtifactsFor(b.root, s)

	if b.parts.Sidecar {
		n := len(e.Files)
		skip := func(rel string, isDir bool) bool {
			return isDir && rel == "tool-results" && !b.parts.ToolResults
		}
		if err := b.walkTree(&e.Files, joinSlash("projects", slug, sid), art.SidecarDir, skip); err != nil {
			return bundle.Entry{}, false, err
		}
		has["sidecar"] = len(e.Files) > n
	}
	if b.parts.FileHistory {
		n := len(e.Files)
		skip := func(rel string, isDir bool) bool { return isDir } // one level only
		if err := b.walkTree(&e.Files, joinSlash("file-history", sid), art.FileHistoryDir, skip); err != nil {
			return bundle.Entry{}, false, err
		}
		has["file-history"] = len(e.Files) > n
	}
	if b.parts.Plans && s.PlanSlug != "" {
		planName := joinSlash("plans", s.PlanSlug+".md")
		if _, _, _, err := bundle.ClassifyName(planName); err != nil {
			b.warn("session %s: plan slug %q is outside the bundle grammar; plan files not exported", sid, s.PlanSlug)
		} else {
			e.PlanSlug = s.PlanSlug
			n := len(e.Files)
			for _, p := range art.PlanFiles {
				if _, err := b.addFile(&e.Files, joinSlash("plans", filepath.Base(p)), p, false, false); err != nil {
					return bundle.Entry{}, false, err
				}
			}
			has["plans"] = len(e.Files) > n
		}
	}
	if b.parts.Tasks {
		n := len(e.Files)
		skip := func(rel string, isDir bool) bool {
			base := path.Base(rel)
			return !isDir && (base == ".lock" || strings.HasSuffix(base, ".lock"))
		}
		if err := b.walkTree(&e.Files, joinSlash("tasks", sid), art.TasksDir, skip); err != nil {
			return bundle.Entry{}, false, err
		}
		has["tasks"] = len(e.Files) > n
	}
	if b.parts.History {
		if lines := b.history[sid]; len(lines) > 0 {
			mtime := b.historyMtime()
			if b.addBytes(&e.Files, joinSlash("history", sid+transcripts.TranscriptExt), lines, mtime) {
				has["history"] = true
			}
		}
	}
	e.Parts = partNames(has)
	return e, true, nil
}

// historyMtime is the mtime stamped on synthesised history files: the
// source history.jsonl's, else the epoch.
func (b *builder) historyMtime() time.Time {
	if info, err := os.Stat(filepath.Join(b.root.ConfigDir, transcripts.HistoryFile)); err == nil {
		return info.ModTime()
	}
	return time.Unix(0, 0).UTC()
}

// memoryEntry builds the manifest entry of mem: MEMORY.md, top-level
// topic files and logs/** — never proposals/ or index caches (the walk
// transcripts uses, applied through the bundle grammar).
// memoryEntry lists a memory directory; keep, when non-nil, narrows the
// files to the named ones (slash-relative to the directory).
func (b *builder) memoryEntry(mem transcripts.Memory, keep map[string]bool) (bundle.Entry, bool, error) {
	if mem.Slug == "" || mem.Dir == "" {
		b.warn("skipped memory %q: incomplete listing", mem.Dir)
		return bundle.Entry{}, false, nil
	}
	prefix := joinSlash("memory", mem.Slug)
	if _, _, _, err := bundle.ClassifyName(joinSlash(prefix, transcripts.MemoryIndexFile)); err != nil {
		b.warn("skipped memory %s: %v", mem.Dir, err)
		return bundle.Entry{}, false, nil
	}
	e := bundle.Entry{Kind: bundle.EntryMemory, Slug: mem.Slug, Cwd: mem.Cwd, ProjectKey: mem.Cwd}
	if mem.Cwd != "" && dirExists(mem.Cwd) {
		if key, err := transcripts.ProjectKey(mem.Cwd); err == nil {
			e.ProjectKey = key
		}
	}
	skip := func(rel string, isDir bool) bool {
		parts := strings.Split(rel, "/")
		if isDir {
			// Only logs/ is walked; inside it proposals/ and index* are out.
			if len(parts) == 1 {
				return parts[0] != transcripts.MemoryLogsSubdir
			}
			last := parts[len(parts)-1]
			return last == transcripts.MemoryProposalsSubdir || strings.HasPrefix(strings.ToLower(last), "index")
		}
		return !strings.HasSuffix(rel, ".md")
	}
	if err := b.walkTree(&e.Files, prefix, mem.Dir, skip); err != nil {
		return bundle.Entry{}, false, err
	}
	if keep != nil {
		kept := e.Files[:0]
		for _, f := range e.Files {
			if keep[strings.TrimPrefix(f.Path, prefix+"/")] {
				kept = append(kept, f)
			}
		}
		e.Files = kept
		if len(e.Files) == 0 {
			return bundle.Entry{}, false, nil // deselected entirely: not a warning
		}
	}
	if len(e.Files) == 0 {
		b.warn("skipped memory %s: no exportable file", mem.Dir)
		return bundle.Entry{}, false, nil
	}
	return e, true, nil
}

// sourceInfo fills manifest.source for this machine and account.
func sourceInfo(root transcripts.Root, o ExportOptions) bundle.Source {
	host, _ := os.Hostname()
	host = ident(host)
	if host == "" {
		host = fallbackHostname
	}
	home, _ := os.UserHomeDir()
	user := ""
	if home != "" {
		user = filepath.Base(home)
	}
	if user == "" || user == "." || user == string(filepath.Separator) {
		user = os.Getenv("USER")
	}
	src := bundle.Source{
		Hostname:    host,
		User:        ident(user),
		Home:        home,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		ConfigDir:   root.ConfigDir,
		RootDir:     root.Dir,
		Account:     ident(o.Account.Name),
		AccountType: string(o.Account.Type),
		Isolation:   string(o.Isolation),
	}
	return src
}

// ident reduces s to the ^[A-Za-z0-9_.-]{1,64}$ shape the manifest
// requires for hostname/user/account, dropping every other byte.
func ident(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s) && sb.Len() < 64; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func deref(p *bool) bool { return p != nil && *p }

// gitRemote is the best-effort origin URL of dir: `git -C dir remote
// get-url origin`, 2 s, stdio pinned, environment without the transfer
// code. Anything but a clean single-line answer is "".
func gitRemote(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, gitRemoteTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.Stdin = nil
	cmd.Env = childEnv("GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return ""
	}
	line := strings.TrimSpace(out.String())
	if line == "" || strings.ContainsAny(line, "\n\r") {
		return ""
	}
	return sanitize(line)
}

// childEnv is os.Environ() minus BFFS_TRANSFER_CODE with extra KEY=VALUE
// entries replacing inherited ones.
func childEnv(extra ...string) []string {
	drop := []string{envTransferCode}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		drop = append(drop, k)
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		skip := false
		for _, d := range drop {
			if strings.EqualFold(k, d) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// collectHistory reads history.jsonl once and returns, per wanted session
// id, the verbatim lines whose sessionId matches (each newline-terminated),
// under the same caps AppendHistory applies. A missing file is empty.
func collectHistory(ctx context.Context, path string, want map[string]bool) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(want) == 0 {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	counts := map[string]int{}
	marker := []byte(`"sessionId"`)
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 && len(line) <= historyMaxLine && bytes.Contains(line, marker) {
			var rec struct {
				SessionID string `json:"sessionId"`
			}
			trimmed := bytes.TrimSpace(line)
			if json.Unmarshal(trimmed, &rec) == nil && want[rec.SessionID] && counts[rec.SessionID] < historyMaxLines {
				counts[rec.SessionID]++
				out[rec.SessionID] = append(out[rec.SessionID], trimmed...)
				out[rec.SessionID] = append(out[rec.SessionID], '\n')
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	return out, nil
}

// newBundleID returns a fresh lowercase UUID v4 from crypto/rand.
func newBundleID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("bundle id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// source is where an Opener finds a bundle path's bytes: a path to open,
// in-memory data, or a handle kept open since the pre-pass.
type source struct {
	path string
	data []byte
	file *os.File
	size int64
}

// opener serves the files BuildManifest listed. Open of a kept handle
// hands out the handle itself (positioned at 0 through a SectionReader)
// and Close on that reader closes the file; the opener's own Close
// releases every handle still open.
type opener struct {
	files map[string]source
}

func (o *opener) Open(p string) (io.ReadCloser, error) {
	s, ok := o.files[p]
	if !ok {
		return nil, fmt.Errorf("open %q: %w", p, fs.ErrNotExist)
	}
	switch {
	case s.file != nil:
		delete(o.files, p)
		return &handleReader{SectionReader: io.NewSectionReader(s.file, 0, s.size), f: s.file}, nil
	case s.data != nil:
		return io.NopCloser(bytes.NewReader(s.data)), nil
	default:
		f, err := os.Open(s.path)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
}

// Close releases every handle the pre-pass kept open and has not been
// handed out yet.
func (o *opener) Close() error {
	var errs []error
	for p, s := range o.files {
		if s.file != nil {
			if err := s.file.Close(); err != nil {
				errs = append(errs, err)
			}
			delete(o.files, p)
		}
	}
	return errors.Join(errs...)
}

// handleReader reads a kept handle from offset 0 and closes it once.
type handleReader struct {
	*io.SectionReader
	f *os.File
}

func (h *handleReader) Close() error { return h.f.Close() }

// ctxReader fails the next Read once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
