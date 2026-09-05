package transcripts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

const (
	// SettingsFile and LocalSettingsFile are Claude's user-scope settings
	// files inside a config dir; the local one overrides the other.
	SettingsFile      = "settings.json"
	LocalSettingsFile = "settings.local.json"

	// EnvRemoteMemoryDir and EnvCoworkMemoryPathOverride relocate Claude's
	// auto-memory tree when set in claude's environment. bffs does not model
	// either; MemoryDirFor refuses when they are present.
	EnvRemoteMemoryDir          = "CLAUDE_CODE_REMOTE_MEMORY_DIR"
	EnvCoworkMemoryPathOverride = "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE"

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
// under root: <root.Dir>/<MemorySlug(dir)>/memory. It returns an error
// wrapping ErrMemoryDirOverridden when EnvRemoteMemoryDir or
// EnvCoworkMemoryPathOverride is set in this process's environment, or when
// root.ConfigDir's settings.local.json or settings.json sets a non-empty
// autoMemoryDirectory — cases where Claude keeps memory somewhere this
// function does not compute. A settings file that cannot be parsed is
// ignored here (Claude ignores it too); use CleanupPeriodDays to surface it.
func MemoryDirFor(root Root, dir string) (string, error) {
	for _, env := range []string{EnvRemoteMemoryDir, EnvCoworkMemoryPathOverride} {
		if os.Getenv(env) != "" {
			return "", fmt.Errorf("%w: %s is set", ErrMemoryDirOverridden, env)
		}
	}
	if root.ConfigDir != "" {
		for _, name := range []string{LocalSettingsFile, SettingsFile} {
			path := filepath.Join(root.ConfigDir, name)
			if autoMemoryDirectorySet(path) {
				return "", fmt.Errorf("%w: autoMemoryDirectory is set in %s", ErrMemoryDirOverridden, path)
			}
		}
	}
	slug, err := MemorySlug(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root.Dir, slug, MemorySubdir), nil
}

// autoMemoryDirectorySet reports whether the settings file at path sets a
// non-empty autoMemoryDirectory. Missing or unparsable files read as unset.
func autoMemoryDirectorySet(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc struct {
		Dir string `json:"autoMemoryDirectory"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return doc.Dir != ""
}
