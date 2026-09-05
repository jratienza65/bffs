package transcripts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	// SettingsFile and LocalSettingsFile are Claude's user-scope settings
	// files inside a config dir; the local one overrides the other.
	SettingsFile      = "settings.json"
	LocalSettingsFile = "settings.local.json"

	// EnvRemoteMemoryDir and EnvCoworkMemoryPathOverride relocate Claude's
	// auto-memory tree when set in claude's environment (disk.md §5).
	// MemoryDirFor honours the cowork override, which names the one memory
	// directory verbatim; the remote dir replaces the config dir as the
	// root of the default projects/<slug>/memory layout but with a slug
	// function bffs has not verified, so MemoryDirFor refuses it.
	EnvRemoteMemoryDir          = "CLAUDE_CODE_REMOTE_MEMORY_DIR"
	EnvCoworkMemoryPathOverride = "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE"

	// minAutoMemoryDirLen is the shortest autoMemoryDirectory value Claude
	// accepts; shorter ones are ignored.
	minAutoMemoryDirLen = 3

	// DefaultCleanupPeriodDays is Claude's retention window when no settings
	// file sets cleanupPeriodDays.
	DefaultCleanupPeriodDays = 30

	// CleanupSourceDefault and CleanupSourceInvalid are the source values
	// CleanupPeriodDays returns besides a settings file name.
	CleanupSourceDefault = "default"
	CleanupSourceInvalid = "invalid"
)

// CleanupPeriodDays returns the retention window Claude applies to the
// config dir — the number of days after which it sweeps stale sidecars,
// plans and file-history by mtime — and where the value came from:
// LocalSettingsFile, SettingsFile, CleanupSourceDefault (30 days) or
// CleanupSourceInvalid. The local file is consulted first; the first file
// that defines cleanupPeriodDays wins. 0 means Claude never sweeps. A file
// that exists but cannot be read or parsed, or a value that is not a number,
// yields (0, CleanupSourceInvalid): bffs cannot know what Claude will do, so
// callers must not derive an mtime clamp from it. A negative value is 0.
func CleanupPeriodDays(configDir string) (int, string) {
	for _, name := range []string{LocalSettingsFile, SettingsFile} {
		raw, err := os.ReadFile(filepath.Join(configDir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, CleanupSourceInvalid
		}
		var doc struct {
			Days json.RawMessage `json:"cleanupPeriodDays"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return 0, CleanupSourceInvalid
		}
		if len(doc.Days) == 0 || string(doc.Days) == "null" {
			continue
		}
		var f float64
		if err := json.Unmarshal(doc.Days, &f); err != nil {
			return 0, CleanupSourceInvalid
		}
		if f < 0 {
			return 0, name
		}
		return int(math.Min(f, math.MaxInt32)), name
	}
	return DefaultCleanupPeriodDays, CleanupSourceDefault
}

// MemoryDirFor returns the auto-memory directory Claude would use for dir
// under root, resolved in Claude's own order (disk.md §5):
//
//  1. EnvCoworkMemoryPathOverride in this process's environment names the
//     directory verbatim (absolute; no "~" expansion) — one directory for
//     every project.
//  2. autoMemoryDirectory in root.ConfigDir's settings.local.json, else
//     settings.json, names it for every project: a leading "~/" expands to
//     the home directory; a value containing "..", naming a filesystem
//     root, shorter than three characters or not absolute after expansion
//     is ignored the way Claude ignores it, and the search continues.
//  3. Otherwise <root.Dir>/<MemorySlug(dir)>/memory. When EnvRemoteMemoryDir
//     is set Claude uses <remote>/projects/<slug>/memory instead, keyed by
//     a slug function bffs has not verified (disk.md §5, `sC`); rather than
//     guess, the result is an error wrapping ErrMemoryDirOverridden that
//     names the variable, and callers place no memory.
//
// Only the third form depends on dir, so a caller placing memory for
// several projects must expect the same answer for all of them under an
// override (rehome's planner drops a memory move whose source and target
// coincide). A settings file that cannot be parsed is ignored here (Claude
// ignores it too); use CleanupPeriodDays to surface it.
func MemoryDirFor(root Root, dir string) (string, error) {
	if v := os.Getenv(EnvCoworkMemoryPathOverride); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s=%q is not an absolute path", EnvCoworkMemoryPathOverride, v)
		}
		return filepath.Clean(v), nil
	}
	if root.ConfigDir != "" {
		for _, name := range []string{LocalSettingsFile, SettingsFile} {
			if d, ok := autoMemoryDirectory(filepath.Join(root.ConfigDir, name)); ok {
				return d, nil
			}
		}
	}
	if os.Getenv(EnvRemoteMemoryDir) != "" {
		return "", fmt.Errorf("%w: %s is set (claude keys memory there by a slug bffs does not compute)", ErrMemoryDirOverridden, EnvRemoteMemoryDir)
	}
	slug, err := MemorySlug(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root.Dir, slug, MemorySubdir), nil
}

// autoMemoryDirectory returns the usable autoMemoryDirectory the settings
// file at path sets, expanded and cleaned, or ok=false when the file is
// missing, unparsable, does not set it, or sets a value Claude rejects.
func autoMemoryDirectory(path string) (dir string, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var doc struct {
		Dir string `json:"autoMemoryDirectory"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", false
	}
	return expandAutoMemoryDirectory(doc.Dir)
}

// expandAutoMemoryDirectory applies Claude's validation to one
// autoMemoryDirectory value: "~/" expands to the home directory, and values
// containing "..", shorter than minAutoMemoryDirLen, relative after
// expansion, or naming a filesystem root are rejected.
func expandAutoMemoryDirectory(v string) (string, bool) {
	if len(v) < minAutoMemoryDirLen || strings.Contains(v, "..") {
		return "", false
	}
	if strings.HasPrefix(v, "~/") || strings.HasPrefix(v, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		v = filepath.Join(home, v[2:])
	}
	v = filepath.Clean(v)
	if !filepath.IsAbs(v) || filepath.Dir(v) == v {
		return "", false
	}
	return v, true
}
