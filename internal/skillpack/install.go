package skillpack

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

// NotManagedError is what Uninstall returns when a skill dir named SkillName
// exists at one or more targets without Marker: those dirs were authored by
// the user and are left in place. Every managed dir has still been removed
// by the time it is returned.
type NotManagedError struct {
	Paths []string
}

func (e *NotManagedError) Error() string {
	return fmt.Sprintf("a skill named %q at %s was not installed by bffs and was left in place",
		SkillName, strings.Join(e.Paths, ", "))
}

// Install writes the skill into every target of Targets: the dir is created
// (0755; a missing per-account session dir is created the way `bffs login`
// would, 0700), then SKILL.md — frontmatter stamped with version — and
// references/rehome-checklist.md land as 0644 files through
// fsutil.AtomicWrite. It returns the skill dirs written, in target order.
//
// A SKILL.md already present at a target that lacks Marker belongs to the
// user; Install refuses before writing anything unless force is set. Re-
// installing over bffs's own copy rewrites identical content, so the call
// is idempotent.
func Install(homeClaudeDir, cfgDir string, accs store.Accounts, state store.State, version string, force bool) (written []string, err error) {
	targets, err := Targets(homeClaudeDir, cfgDir, accs, state)
	if err != nil {
		return nil, err
	}
	skill, err := renderSkill(version)
	if err != nil {
		return nil, err
	}
	checklist, err := readAsset(ChecklistFile)
	if err != nil {
		return nil, err
	}

	// Refuse up front so a user-authored skill on one target never leaves
	// the others half-installed.
	if !force {
		for _, t := range targets {
			managed, exists, err := skillState(t)
			if err != nil {
				return nil, err
			}
			if exists && !managed {
				return nil, fmt.Errorf("a skill named %q already exists at %s and was not installed by bffs; --force overwrites it", SkillName, t)
			}
		}
	}

	absCfg, err := filepath.Abs(cfgDir)
	if err != nil {
		return nil, err
	}
	sessionsRoot := filepath.Join(absCfg, sessions.SessionsSubdir)
	for _, t := range targets {
		if err := ensureSkillDir(t, sessionsRoot); err != nil {
			return written, err
		}
		if err := fsutil.AtomicWrite(filepath.Join(t, SkillFile), skill, 0o644); err != nil {
			return written, err
		}
		if err := fsutil.AtomicWrite(filepath.Join(t, filepath.FromSlash(ChecklistFile)), checklist, 0o644); err != nil {
			return written, err
		}
		written = append(written, t)
	}
	return written, nil
}

// Uninstall removes the skill dir at every target whose SKILL.md carries
// Marker and returns the dirs removed. A same-named dir without the marker
// is a user's skill: it is left alone and reported through a
// *NotManagedError once every managed dir has been handled. Targets with no
// SKILL.md are skipped.
func Uninstall(homeClaudeDir, cfgDir string, accs store.Accounts, state store.State) (removed []string, err error) {
	targets, err := Targets(homeClaudeDir, cfgDir, accs, state)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, t := range targets {
		managed, exists, err := skillState(t)
		if err != nil {
			return removed, err
		}
		if !exists {
			continue
		}
		if !managed {
			kept = append(kept, t)
			continue
		}
		if err := os.RemoveAll(t); err != nil {
			return removed, fmt.Errorf("remove %s: %w", t, err)
		}
		removed = append(removed, t)
	}
	if len(kept) > 0 {
		return removed, &NotManagedError{Paths: kept}
	}
	return removed, nil
}

// skillState reports whether <dir>/SKILL.md exists and whether it carries
// Marker.
func skillState(dir string) (managed, exists bool, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, SkillFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read %s: %w", filepath.Join(dir, SkillFile), err)
	}
	return bytes.Contains(raw, []byte(Marker)), true, nil
}

// ensureSkillDir creates the skill dir and its references/ subdir (0755). A
// target under <cfgDir>/sessions/ whose session dir does not exist yet (a
// full-isolation account that was never launched) gets that dir created
// with the 0700 perms `bffs login` would use, so the eventual login finds
// nothing surprising.
func ensureSkillDir(skillDir, sessionsRoot string) error {
	if rel, err := filepath.Rel(sessionsRoot, skillDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		// rel is <account>/skills/<SkillName>; the first element is the session dir.
		account := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if err := sessions.EnsureDir(filepath.Join(sessionsRoot, account)); err != nil {
			return fmt.Errorf("create session dir for %q: %w", account, err)
		}
	}
	refs := filepath.Join(skillDir, filepath.Dir(filepath.FromSlash(ChecklistFile)))
	if err := os.MkdirAll(refs, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", refs, err)
	}
	return nil
}
