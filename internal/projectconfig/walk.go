package projectconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

const Filename = "bffs.toml"

type Config struct {
	Account string `toml:"account"`
}

type Found struct {
	Path   string
	Config Config
}

// Find walks up from start looking for bffs.toml.
// Returns (Found{}, false, nil) if no file is found before reaching the
// boundary. Returns an error only if a file is found but cannot be read or
// parsed.
//
// The walk is bounded to prevent an attacker-controlled directory ABOVE the
// user's working area (e.g., a planted /tmp/bffs.toml) from pinning
// `claude` to an account of the attacker's choosing. We stop at the first of:
//
//   - the directory containing the start path's `.git` (the project root)
//   - $HOME (so even outside a git repo the walk doesn't escape user space)
//   - the filesystem root (last resort — only when both above are absent)
func Find(start string) (Found, bool, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return Found{}, false, fmt.Errorf("resolve start dir: %w", err)
	}
	home := userHome()
	dir := abs
	for {
		candidate := filepath.Join(dir, Filename)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			var cfg Config
			if _, derr := toml.DecodeFile(candidate, &cfg); derr != nil {
				return Found{}, false, fmt.Errorf("parse %s: %w", candidate, derr)
			}
			return Found{Path: candidate, Config: cfg}, true, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Found{}, false, fmt.Errorf("stat %s: %w", candidate, err)
		}
		// Boundaries: stop AFTER inspecting `dir` itself.
		if isGitToplevel(dir) {
			return Found{}, false, nil
		}
		if home != "" && dir == home {
			return Found{}, false, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Found{}, false, nil
		}
		dir = parent
	}
}

// userHome returns $HOME (or the platform equivalent) as an absolute path,
// or "" if it cannot be determined. Used as an upper bound for the walk.
func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	abs, err := filepath.Abs(h)
	if err != nil {
		return h
	}
	return abs
}

// isGitToplevel reports whether dir contains a `.git` entry — i.e., dir is the
// root of a git working tree (or a bare repo). Either a directory or a file
// (worktree pointer) counts.
func isGitToplevel(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
