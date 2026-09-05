package bundle

import (
	"fmt"
	"math"
	"path"
	"strings"
	"time"
)

// Manifest is manifest.json, tar entry 0 of every bundle. It is both the
// description the receiving side shows before accepting anything and the
// allow-list Unpack enforces on every later entry.
type Manifest struct {
	Format        int       `json:"format"`
	BundleID      string    `json:"bundle_id"`
	BFFSVersion   string    `json:"bffs_version,omitempty"`
	ClaudeVersion string    `json:"claude_version,omitempty"`
	Created       time.Time `json:"created"`
	Source        Source    `json:"source"`
	Totals        Totals    `json:"totals"`
	Entries       []Entry   `json:"entries"`
}

// Source describes where a bundle was written. Hostname, User and Account
// are constrained identifiers; the path fields are informational and are
// never joined into a local path.
type Source struct {
	Hostname    string `json:"hostname"`
	User        string `json:"user,omitempty"`
	Home        string `json:"home,omitempty"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	ConfigDir   string `json:"config_dir,omitempty"`
	RootDir     string `json:"root_dir,omitempty"`
	Account     string `json:"account,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	Isolation   string `json:"isolation,omitempty"`
}

// Totals summarises the manifest so the receiver can size the import
// before reading any payload byte. Validate requires them to equal what the
// entries list.
type Totals struct {
	Entries int   `json:"entries"`
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
}

// TrustInfo carries the source project's trust answers. It is information
// only; the importer applies it solely on explicit opt-in.
type TrustInfo struct {
	Accepted                     bool `json:"accepted"`
	ExternalIncludesApproved     bool `json:"external_includes_approved"`
	ExternalIncludesWarningShown bool `json:"external_includes_warning_shown"`
}

// File is one allow-listed tar entry: its bundle-relative name, exact size,
// lowercase hex sha256 and the mtime the writer preserved.
type File struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	ModTime time.Time `json:"mtime"`
}

// Entry is one session (kind "session": transcript plus its sidecars,
// file-history, plans, history lines and optional tasks) or one auto-memory
// directory (kind "memory").
type Entry struct {
	Kind                  string     `json:"kind"`
	Slug                  string     `json:"slug"`
	Cwd                   string     `json:"cwd,omitempty"`
	ProjectKey            string     `json:"project_key,omitempty"`
	GitRemote             string     `json:"git_remote,omitempty"`
	SessionID             string     `json:"session_id,omitempty"`
	Title                 string     `json:"title,omitempty"`
	GitBranch             string     `json:"git_branch,omitempty"`
	PlanSlug              string     `json:"plan_slug,omitempty"`
	Started               time.Time  `json:"started,omitzero"`
	Last                  time.Time  `json:"last,omitzero"`
	Parts                 []string   `json:"parts,omitempty"`
	SourceTrust           *TrustInfo `json:"source_trust,omitempty"`
	LivePossiblyTruncated bool       `json:"live_possibly_truncated,omitempty"`
	Files                 []File     `json:"files"`
}

const maxIdentLen = 64

// Validate checks the manifest against the format rules and l: format 1;
// lowercase-UUID bundle_id; source.hostname present and hostname, user and
// account matching ^[A-Za-z0-9_.-]{1,64}$ (user/account may be empty);
// entry and file counts within l and equal to totals; totals.bytes equal to
// the sum of file sizes and within l; every file name inside the grammar,
// unique after strings.ToLower and never (case-insensitively) a directory of
// another listed file, with a lowercase 64-hex sha256 and a size in
// [0, MaxEntryBytes]; every entry a session (lowercase-UUID session_id, a
// transcript at projects/<slug>/<sid>.jsonl, every file keyed to that sid or
// slug, plan files matching plan_slug) or a memory (every file under
// memory/<slug>/). Reserved-kind files are accepted when their sid/slug (if
// any) match the entry.
func (m *Manifest) Validate(l Limits) error {
	if m == nil {
		return fmt.Errorf("nil manifest")
	}
	if m.Format != FormatVersion {
		if m.Format > FormatVersion {
			return fmt.Errorf("manifest format %d is newer than this bffs understands (format %d); upgrade bffs", m.Format, FormatVersion)
		}
		return fmt.Errorf("manifest format %d is not supported (want %d)", m.Format, FormatVersion)
	}
	if !isUUID(m.BundleID) {
		return fmt.Errorf("bundle_id %q is not a lowercase uuid", m.BundleID)
	}
	if m.Source.Hostname == "" {
		return fmt.Errorf("source.hostname is required")
	}
	for _, f := range []struct{ key, val string }{
		{"source.hostname", m.Source.Hostname},
		{"source.user", m.Source.User},
		{"source.account", m.Source.Account},
	} {
		if f.val != "" && !isIdent(f.val) {
			return fmt.Errorf("%s %q must match ^[A-Za-z0-9_.-]{1,%d}$", f.key, f.val, maxIdentLen)
		}
	}
	if len(m.Entries) > l.MaxEntries {
		return fmt.Errorf("manifest lists %d entries; max %d", len(m.Entries), l.MaxEntries)
	}
	if m.Totals.Entries != len(m.Entries) {
		return fmt.Errorf("totals.entries is %d but %d entries are listed", m.Totals.Entries, len(m.Entries))
	}

	set := &fileSet{files: make(map[string]string), dirs: make(map[string]string)}
	for i := range m.Entries {
		if err := validateEntry(&m.Entries[i], l, set); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
	}
	if m.Totals.Files != set.count {
		return fmt.Errorf("totals.files is %d but %d files are listed", m.Totals.Files, set.count)
	}
	if m.Totals.Bytes != set.bytes {
		return fmt.Errorf("totals.bytes is %d but the listed files sum to %d", m.Totals.Bytes, set.bytes)
	}
	if set.bytes > l.MaxTotalBytes {
		return fmt.Errorf("bundle is %d bytes; max %d", set.bytes, l.MaxTotalBytes)
	}
	return nil
}

// fileSet accumulates the allow-list while Validate walks the entries and
// refuses the shapes a case-folding filesystem cannot hold: two paths equal
// after strings.ToLower, or a path that is a directory of another.
type fileSet struct {
	files map[string]string // lower path -> path as listed
	dirs  map[string]string // lower directory -> a listed path under it
	count int
	bytes int64
}

func (s *fileSet) add(p string, size int64, l Limits) error {
	lower := strings.ToLower(p)
	if prev, dup := s.files[lower]; dup {
		if prev == p {
			return fmt.Errorf("file %q is listed twice", p)
		}
		return fmt.Errorf("file %q collides with %q on a case-insensitive filesystem", p, prev)
	}
	if under, isDir := s.dirs[lower]; isDir {
		return fmt.Errorf("file %q is also a directory of %q", p, under)
	}
	for d := path.Dir(lower); d != "."; d = path.Dir(d) {
		if prev, isFile := s.files[d]; isFile {
			return fmt.Errorf("file %q is under %q, which is a file", p, prev)
		}
		s.dirs[d] = p
	}
	s.files[lower] = p
	s.count++
	if s.count > l.MaxFiles {
		return fmt.Errorf("manifest lists more than %d files", l.MaxFiles)
	}
	if size > math.MaxInt64-s.bytes {
		return fmt.Errorf("listed sizes overflow at file %q", p)
	}
	s.bytes += size
	return nil
}

func validateEntry(e *Entry, l Limits, set *fileSet) error {
	switch e.Kind {
	case EntrySession, EntryMemory:
	default:
		return fmt.Errorf("kind %q is not %q or %q", e.Kind, EntrySession, EntryMemory)
	}
	if err := checkSlug(e.Slug); err != nil {
		return err
	}
	if e.Kind == EntrySession && !isUUID(e.SessionID) {
		return fmt.Errorf("session_id %q is not a lowercase uuid", e.SessionID)
	}
	if e.PlanSlug != "" && !isPlanSlug(e.PlanSlug) {
		return fmt.Errorf("plan_slug %q must match ^[a-z0-9-]{1,%d}$", e.PlanSlug, maxPlanSlugLen)
	}
	if len(e.Files) == 0 {
		return fmt.Errorf("%s %q lists no files", e.Kind, e.Slug)
	}

	hasTranscript := false
	for j := range e.Files {
		f := &e.Files[j]
		kind, sid, slug, err := ClassifyName(f.Path)
		if err != nil {
			return fmt.Errorf("file %d: %w", j, err)
		}
		if kind == NameManifest {
			return fmt.Errorf("file %d: %q may only be entry 0, never a listed file", j, f.Path)
		}
		if f.Size < 0 {
			return fmt.Errorf("file %q has a negative size %d", f.Path, f.Size)
		}
		if f.Size > l.MaxEntryBytes {
			return fmt.Errorf("file %q is %d bytes; max %d", f.Path, f.Size, l.MaxEntryBytes)
		}
		if !isSHA256Hex(f.SHA256) {
			return fmt.Errorf("file %q has sha256 %q; want 64 lowercase hex digits", f.Path, f.SHA256)
		}
		if err := set.add(f.Path, f.Size, l); err != nil {
			return err
		}
		if err := entryOwns(e, f.Path, kind, sid, slug); err != nil {
			return err
		}
		if kind == NameTranscript {
			hasTranscript = true
		}
	}
	if e.Kind == EntrySession && !hasTranscript {
		return fmt.Errorf("session %s lists no transcript projects/%s/%s.jsonl", e.SessionID, e.Slug, e.SessionID)
	}
	return nil
}

// entryOwns checks that a classified file belongs to the entry that lists
// it, so a session cannot smuggle another session's files (or a memory
// directory) into its commit.
func entryOwns(e *Entry, p, kind, sid, slug string) error {
	mismatch := func(what, got, want string) error {
		return fmt.Errorf("file %q belongs to %s %q, not this entry's %q", p, what, got, want)
	}
	if kind == NameReserved {
		if sid != "" && sid != e.SessionID {
			return mismatch("session", sid, e.SessionID)
		}
		if slug != "" && slug != e.Slug {
			return mismatch("slug", slug, e.Slug)
		}
		return nil
	}
	switch e.Kind {
	case EntrySession:
		switch kind {
		case NameTranscript, NameSidecar:
			if slug != e.Slug {
				return mismatch("slug", slug, e.Slug)
			}
			if sid != e.SessionID {
				return mismatch("session", sid, e.SessionID)
			}
		case NameFileHistory, NameHistory, NameTasks:
			if sid != e.SessionID {
				return mismatch("session", sid, e.SessionID)
			}
		case NamePlans:
			base := p[len("plans/"):]
			if e.PlanSlug == "" {
				return fmt.Errorf("file %q listed but the session has no plan_slug", p)
			}
			if !planFileBelongs(base, e.PlanSlug) {
				return fmt.Errorf("file %q does not belong to plan_slug %q", p, e.PlanSlug)
			}
		default:
			return fmt.Errorf("file %q (%s) cannot be part of a session entry", p, kind)
		}
	case EntryMemory:
		if kind != NameMemory {
			return fmt.Errorf("file %q (%s) cannot be part of a memory entry", p, kind)
		}
		if slug != e.Slug {
			return mismatch("slug", slug, e.Slug)
		}
	}
	return nil
}

// isIdent matches ^[A-Za-z0-9_.-]{1,64}$.
func isIdent(s string) bool {
	if s == "" || len(s) > maxIdentLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(slugByte(b) || b == '.') {
			return false
		}
	}
	return true
}
