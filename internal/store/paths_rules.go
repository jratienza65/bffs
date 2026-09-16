package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PathsFile holds directory -> account rules: per-project pinning with the
// mapping in the user's config instead of a bffs.toml in the working tree.
const PathsFile = "paths.toml"

// PathRule maps a directory prefix to an account. A rule covers the directory
// and everything beneath it.
type PathRule struct {
	Path    string `toml:"path"`
	Account string `toml:"account"`
}

type Paths struct {
	Rules []PathRule `toml:"rules"`
}

func LoadPaths(dir string) (Paths, error) {
	path := filepath.Join(dir, PathsFile)
	var p Paths
	if _, err := toml.DecodeFile(path, &p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Paths{}, nil
		}
		return Paths{}, fmt.Errorf("read %s: %w", path, err)
	}
	return p, nil
}

func SavePaths(dir string, p Paths) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	p.sort()
	return writeToml(filepath.Join(dir, PathsFile), p)
}

// sort keeps the file deterministic and readable; Match does not depend on it.
func (p *Paths) sort() {
	sort.Slice(p.Rules, func(i, j int) bool { return p.Rules[i].Path < p.Rules[j].Path })
}

// NormalizePath makes a rule path comparable: absolute, symlink-resolved where
// possible, and cleaned. Resolution is best-effort — a rule may legitimately
// name a directory that does not exist yet (an unmounted volume, a repo not
// cloned on this machine), and that should still be storable.
func NormalizePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is empty")
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// Match returns the account for the most specific rule covering cwd.
//
// "Most specific" is the longest matching prefix, so a rule on `/a/b/c`
// beats one on `/a/b`. Matching is on path segments, never raw strings:
// `/a/b` must not match `/a/bc`.
func (p Paths) Match(cwd string) (PathRule, bool) {
	target, err := NormalizePath(cwd)
	if err != nil {
		return PathRule{}, false
	}

	best := PathRule{}
	bestLen := -1
	for _, r := range p.Rules {
		if r.Account == "" || r.Path == "" {
			continue
		}
		rp, err := NormalizePath(r.Path)
		if err != nil {
			continue
		}
		if !underOrEqual(target, rp) {
			continue
		}
		if len(rp) > bestLen {
			best, bestLen = PathRule{Path: rp, Account: r.Account}, len(rp)
		}
	}
	return best, bestLen >= 0
}

// Set adds or replaces the rule for a directory. Returns the previous account
// if one was replaced, so callers can report the change.
func (p *Paths) Set(dir, account string) (string, error) {
	norm, err := NormalizePath(dir)
	if err != nil {
		return "", err
	}
	for i := range p.Rules {
		existing, err := NormalizePath(p.Rules[i].Path)
		if err != nil {
			continue
		}
		if existing == norm {
			prev := p.Rules[i].Account
			p.Rules[i] = PathRule{Path: norm, Account: account}
			return prev, nil
		}
	}
	p.Rules = append(p.Rules, PathRule{Path: norm, Account: account})
	return "", nil
}

// Remove deletes the rule for exactly this directory; a directory that only
// inherits a parent rule is not removed. Reports whether one was found.
func (p *Paths) Remove(dir string) (bool, error) {
	norm, err := NormalizePath(dir)
	if err != nil {
		return false, err
	}
	for i := range p.Rules {
		existing, err := NormalizePath(p.Rules[i].Path)
		if err != nil {
			continue
		}
		if existing == norm {
			p.Rules = append(p.Rules[:i], p.Rules[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// underOrEqual reports whether target is root itself or nested inside it,
// comparing whole path segments so /a/bc is not treated as under /a/b.
func underOrEqual(target, root string) bool {
	if target == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(target, root)
}
