// Package sessions manages per-account Claude Code config dirs.
//
// Background: Claude Code reads its entire config tree from CLAUDE_CONFIG_DIR
// (defaulting to ~/.claude). Setting CLAUDE_CONFIG_DIR per-invocation gives
// per-account isolation of identity (.claude.json), credentials (Keychain
// service name is hashed from the dir on macOS, .credentials.json elsewhere),
// and everything else under ~/.claude (settings, skills, plugins, history,
// projects, todos, etc.).
//
// Total isolation may be more than the user wants. Two presets pick what's
// per-account vs symlinked back to ~/.claude:
//
//   - partial (default): only .claude.json and .credentials.json are
//                        per-account. Everything else in ~/.claude is
//                        symlinked back, so history, projects, todos,
//                        settings, skills, plugins all carry across
//                        accounts. Drop-in for users who just want auth
//                        isolated.
//   - full:              nothing symlinked. Each account is a fresh Claude
//                        Code world.
//
// SyncSymlinks is idempotent and lenient — it adds missing symlinks and
// removes ones we no longer want, but never destroys real files claude wrote.
// Conflicting paths (real file/dir where we'd want a symlink) are skipped
// and returned to the caller so they can warn or surface the divergence.
package sessions

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jratienza65/bffs/internal/store"
)

// SessionsSubdir is the name of the subdir under the bffs config dir that
// holds per-account session dirs.
const SessionsSubdir = "sessions"

// alwaysIsolated lists subpaths that must NEVER be symlinked, regardless of
// preset — these are exactly what makes an account "different."
var alwaysIsolated = map[string]bool{
	".claude.json":      true,
	".credentials.json": true,
}

// Dir returns the per-account session dir under cfgDir.
//
// Result has no trailing separator and is not created — callers should call
// EnsureDir before exec'ing claude.
func Dir(cfgDir, accountName string) string {
	return filepath.Join(cfgDir, SessionsSubdir, accountName)
}

// EnsureDir creates the per-account session dir (and any missing parents)
// with 0700 perms. Idempotent.
func EnsureDir(sessionDir string) error {
	return os.MkdirAll(sessionDir, 0o700)
}

// SyncSymlinks reconciles sessionDir's symlinks to match preset. homeClaudeDir
// is the user's shared ~/.claude — entries in it are the symlink targets for
// the partial preset.
//
// Behavior:
//
//   - For partial: lstat each entry of homeClaudeDir. If it's not in
//     alwaysIsolated, ensure a symlink at <sessionDir>/<entry> points at
//     <homeClaudeDir>/<entry>. Real files/dirs claude wrote at the same
//     path are skipped (and reported in the returned []string).
//   - For full: lstat every entry of homeClaudeDir; if any of our symlinks
//     are present in sessionDir, remove them so claude starts fresh. Real
//     files are left alone.
//   - Always: any of OUR previously-created symlinks that no longer match
//     the preset are removed.
//
// Returns the list of subpaths that were skipped due to conflicts (real
// file/dir at the path we'd want to symlink). The caller can log them. The
// error return is for unrecoverable filesystem failures, not conflicts.
func SyncSymlinks(sessionDir, homeClaudeDir string, preset store.IsolationPreset) ([]string, error) {
	if err := EnsureDir(sessionDir); err != nil {
		return nil, err
	}

	homeEntries, err := os.ReadDir(homeClaudeDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", homeClaudeDir, err)
	}

	var skipped []string
	wantSymlink := make(map[string]bool) // entry name → true if we want a symlink

	if preset == store.IsolationPartial {
		for _, e := range homeEntries {
			if alwaysIsolated[e.Name()] {
				continue
			}
			wantSymlink[e.Name()] = true
		}
	}

	// Add or repair every wanted symlink.
	for name := range wantSymlink {
		linkPath := filepath.Join(sessionDir, name)
		target := filepath.Join(homeClaudeDir, name)
		switch result := ensureSymlink(linkPath, target); result {
		case ensureOK:
			// no-op
		case ensureSkippedConflict:
			skipped = append(skipped, name)
		default:
			return skipped, fmt.Errorf("ensure symlink %s -> %s: %w", linkPath, target, result)
		}
	}

	// Remove any symlinks under sessionDir that we no longer want — covers
	// downgrades (partial → full) and entries removed from ~/.claude.
	sessionEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return skipped, fmt.Errorf("read %s: %w", sessionDir, err)
	}
	for _, e := range sessionEntries {
		if wantSymlink[e.Name()] {
			continue
		}
		// Only remove our own symlinks; never touch real files.
		linkPath := filepath.Join(sessionDir, e.Name())
		info, err := os.Lstat(linkPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		// It's a symlink — but only remove it if it points into homeClaudeDir
		// (i.e., one we managed). User-created symlinks are left alone.
		target, err := os.Readlink(linkPath)
		if err != nil {
			continue
		}
		if !pointsInto(target, homeClaudeDir) {
			continue
		}
		_ = os.Remove(linkPath)
	}

	return skipped, nil
}

// ensureResult is a small enum so SyncSymlinks can distinguish "ok",
// "skipped because of a real-file conflict" (lenient outcome), and a real
// error (filesystem failure) without needing two return values.
type ensureResult error

var (
	ensureOK              ensureResult = nil
	ensureSkippedConflict ensureResult = fmt.Errorf("skipped: real file present")
)

// ensureSymlink atomically swings the symlink at path to point at target.
// If a non-symlink file/dir is in the way, returns ensureSkippedConflict
// (the caller logs it; user data is preserved).
func ensureSymlink(path, target string) ensureResult {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.Symlink(target, path); err != nil {
				return fmt.Errorf("symlink: %w", err)
			}
			return ensureOK
		}
		return fmt.Errorf("lstat: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return ensureSkippedConflict
	}
	cur, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("readlink: %w", err)
	}
	if cur == target {
		return ensureOK
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale symlink: %w", err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("symlink: %w", err)
	}
	return ensureOK
}

// pointsInto reports whether linkTarget resolves to a path inside parentDir.
// We use this to decide whether a session-dir symlink was bffs-managed
// (target is somewhere under ~/.claude) before removing it.
func pointsInto(linkTarget, parentDir string) bool {
	abs, err := filepath.Abs(linkTarget)
	if err != nil {
		return false
	}
	parent, err := filepath.Abs(parentDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(parent, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return false
	}
	return true
}
