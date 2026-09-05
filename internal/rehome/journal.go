package rehome

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// JournalFile is the name of the journal inside a staging directory.
const JournalFile = "journal.json"

// Journal records the session commit in flight so an interrupted operation
// can be repaired by Recover. One journal per operation, rewritten at every
// step. Origin is where the session's transcript is being taken from: a
// path inside staging for an import (it stays there while the copy is in
// flight) or the transcript's old path for an in-place rehome (M6). Target
// is the transcript's destination path. Step is "sidecar", "transcript" or
// "done".
type Journal struct {
	BundleID  string `json:"bundle_id"`
	SessionID string `json:"session_id"`
	Origin    string `json:"origin"`
	Target    string `json:"target"`
	Step      string `json:"step"`
}

// Journal steps, in commit order.
const (
	StepSidecar    = "sidecar"
	StepTranscript = "transcript"
	StepDone       = "done"
)

// WriteJournal writes j to <stagingDir>/journal.json atomically with mode
// 0600, replacing the previous step's record.
func WriteJournal(stagingDir string, j Journal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.AtomicWrite(filepath.Join(stagingDir, JournalFile), data, 0o600); err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	return nil
}

// readJournal returns the journal in stagingDir. A missing or unreadable
// journal is (Journal{}, false): recovery must not fail on it.
func readJournal(stagingDir string) (Journal, bool) {
	if stagingDir == "" {
		return Journal{}, false
	}
	data, err := os.ReadFile(filepath.Join(stagingDir, JournalFile))
	if err != nil {
		return Journal{}, false
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return Journal{}, false
	}
	return j, true
}

// tmpEntry is one ".bffs-tmp" entry found under a projects/ root.
type tmpEntry struct {
	slug   string
	name   string // entry name inside the slug dir
	sid    string
	isDir  bool
	target string // the name it restores to inside the slug dir
}

// Recover repairs what an interrupted commit left under root
// (restore-never-delete, plan §9.6). It walks every slug directory of
// root.Dir for entries of the ".bffs-tmp" family (transcripts.IsSetAside):
//
//   - "<sid>.jsonl.bffs-tmp" (or "…-<rand>") is renamed to "<sid>.jsonl" in
//     place when no "<sid>.jsonl" for that sid exists anywhere in the root
//     (any slug); otherwise it is kept and listed.
//   - "<sid>.bffs-tmp/" (or "<sid>.bffs-tmp-<rand>/") is removed only when
//     its original "<sid>/" already exists — the copy that superseded it is
//     complete — else it is renamed into place.
//
// Sidecars are handled before transcripts, so a session becomes visible
// last, the way the commit works. Nothing else is ever deleted.
//
// stagingDir (may be "") is consulted only for its journal.json. For an
// import, whose Origin is a path inside staging: when the journal names
// the session and its Origin still exists, the transcript tmp is a copy of
// a source that is still intact — possibly a truncated one — so it is kept
// and listed rather than restored; the re-run of the import writes it
// again. For an in-place rehome (Apply), whose Origin is the transcript's
// old path inside root: while that file still exists the session has not
// moved, so a sidecar the interrupted run had already renamed — the
// original, not a copy — goes back beside it, whether it sits at
// "<new>/<sid>.bffs-tmp/" or already at "<new>/<sid>/"; it is never
// removed. Staging itself is never cleaned here.
//
// restored lists the final path of every entry renamed into place and the
// path of every redundant sidecar tmp removed; kept lists every tmp entry
// left on disk. Both are sorted.
func Recover(root transcripts.Root, stagingDir string) (restored, kept []string, err error) {
	slugs, err := os.ReadDir(root.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("recover: read %q: %w", root.Dir, err)
	}
	journal, hasJournal := readJournal(stagingDir)
	// homeSlug is set for an interrupted in-place rehome whose transcript
	// is still at its old path: the entry the session's sidecar returns to.
	homeSlug := ""
	if hasJournal && journal.Step != StepDone && isUUID(journal.SessionID) && insideDir(journal.Origin, root.Dir) {
		if _, err := os.Lstat(journal.Origin); err == nil {
			homeSlug = filepath.Base(filepath.Dir(journal.Origin))
		}
	}

	// Pass 1: index every <sid>.jsonl in the root and collect tmp entries.
	present := map[string]bool{}
	var tmps []tmpEntry
	for _, s := range slugs {
		if !s.IsDir() || transcripts.IsReserved(s.Name()) {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root.Dir, s.Name()))
		if err != nil {
			continue // unreadable slug dir: nothing to restore there
		}
		for _, e := range entries {
			name := e.Name()
			if sid, ok := strings.CutSuffix(name, transcripts.TranscriptExt); ok && isUUID(sid) && !e.IsDir() {
				present[sid] = true
				continue
			}
			if t, ok := classifyTmp(name, e.IsDir()); ok {
				t.slug = s.Name()
				tmps = append(tmps, t)
			}
		}
	}
	// A rehome interrupted after its sidecar had reached the target
	// entry, before the transcript followed: the sidecar goes back.
	var back [2]string // target-entry path → old-entry path, relative
	if homeSlug != "" && journal.Target != "" {
		if tSlug := filepath.Base(filepath.Dir(journal.Target)); tSlug != homeSlug && !transcripts.IsReserved(tSlug) {
			back = [2]string{filepath.Join(tSlug, journal.SessionID), filepath.Join(homeSlug, journal.SessionID)}
		}
	}
	if len(tmps) == 0 && back[0] == "" {
		return nil, nil, nil
	}

	dest, err := os.OpenRoot(root.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("recover: open %q: %w", root.Dir, err)
	}
	defer dest.Close()

	if back[0] != "" {
		if info, err := dest.Lstat(back[0]); err == nil && info.IsDir() {
			if _, err := dest.Lstat(back[1]); errors.Is(err, fs.ErrNotExist) {
				if err := dest.Rename(back[0], back[1]); err != nil {
					return nil, nil, fmt.Errorf("recover: restore %q: %w", filepath.Join(root.Dir, back[0]), err)
				}
				restored = append(restored, filepath.Join(root.Dir, back[1]))
			} else {
				kept = append(kept, filepath.Join(root.Dir, back[0]))
			}
		}
	}

	// Pass 2: sidecars first, then transcripts.
	sort.SliceStable(tmps, func(i, j int) bool {
		if tmps[i].isDir != tmps[j].isDir {
			return tmps[i].isDir
		}
		return tmps[i].slug+"/"+tmps[i].name < tmps[j].slug+"/"+tmps[j].name
	})
	for _, t := range tmps {
		tmpRel := filepath.Join(t.slug, t.name)
		tmpAbs := filepath.Join(root.Dir, tmpRel)
		finalRel := filepath.Join(t.slug, t.target)
		finalAbs := filepath.Join(root.Dir, finalRel)
		if t.isDir {
			if homeSlug != "" && t.sid == journal.SessionID {
				// The in-place rehome renamed the original here; it goes
				// back beside its transcript and is never removed.
				backRel := filepath.Join(homeSlug, t.target)
				if _, err := dest.Lstat(backRel); err == nil {
					kept = append(kept, tmpAbs)
					continue
				}
				if err := dest.Rename(tmpRel, backRel); err != nil {
					return restored, kept, fmt.Errorf("recover: restore %q: %w", tmpAbs, err)
				}
				restored = append(restored, filepath.Join(root.Dir, backRel))
				continue
			}
			if _, err := dest.Lstat(finalRel); err == nil {
				if err := dest.RemoveAll(tmpRel); err != nil {
					return restored, kept, fmt.Errorf("recover: remove %q: %w", tmpAbs, err)
				}
				restored = append(restored, tmpAbs)
				continue
			}
			if err := dest.Rename(tmpRel, finalRel); err != nil {
				return restored, kept, fmt.Errorf("recover: restore %q: %w", tmpAbs, err)
			}
			restored = append(restored, finalAbs)
			continue
		}
		if present[t.sid] {
			kept = append(kept, tmpAbs)
			continue
		}
		if hasJournal && journal.SessionID == t.sid && journal.Origin != "" {
			if _, err := os.Lstat(journal.Origin); err == nil {
				kept = append(kept, tmpAbs) // a copy in flight; its source is intact
				continue
			}
		}
		if _, err := dest.Lstat(finalRel); err == nil {
			kept = append(kept, tmpAbs) // never rename over something that appeared meanwhile
			continue
		}
		if err := dest.Rename(tmpRel, finalRel); err != nil {
			return restored, kept, fmt.Errorf("recover: restore %q: %w", tmpAbs, err)
		}
		present[t.sid] = true
		restored = append(restored, finalAbs)
	}
	sort.Strings(restored)
	sort.Strings(kept)
	return restored, kept, nil
}

// insideDir reports whether p lies strictly below dir (both cleaned; no
// symlink resolution — a journal spells Origin the way the root did).
func insideDir(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// classifyTmp recognises "<sid>.jsonl.bffs-tmp[-<rand>]" (a regular file)
// and "<sid>.bffs-tmp[-<rand>]" (a directory) and says what each restores
// to. Anything else — including a tmp of the wrong type — is ignored.
func classifyTmp(name string, isDir bool) (tmpEntry, bool) {
	if !transcripts.IsSetAside(name) {
		return tmpEntry{}, false
	}
	i := strings.LastIndex(name, tmpSuffix)
	if i < 0 {
		return tmpEntry{}, false
	}
	rest := name[i+len(tmpSuffix):]
	if rest != "" && !strings.HasPrefix(rest, "-") {
		return tmpEntry{}, false
	}
	base := name[:i]
	if sid, ok := strings.CutSuffix(base, transcripts.TranscriptExt); ok {
		if !isUUID(sid) || isDir {
			return tmpEntry{}, false
		}
		return tmpEntry{name: name, sid: sid, target: base}, true
	}
	if !isUUID(base) || !isDir {
		return tmpEntry{}, false
	}
	return tmpEntry{name: name, sid: base, isDir: true, target: base}, true
}
