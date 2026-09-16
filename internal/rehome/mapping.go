package rehome

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jratienza65/bffs/internal/store"
)

// ParseMapping reads one "OLD=NEW" prefix rule (plan §9.3): the value is
// split on the FIRST '=', both halves are trimmed and must be absolute
// paths. NEW names a directory on this machine and is normalised with
// store.NormalizePath ("~" expands, symlinks resolve when it exists); its
// existence is checked when the rule is applied, not here. OLD is the
// directory recorded on the source machine — possibly another OS — so a
// POSIX path is accepted on every platform and only cleaned; a "~" prefix
// on OLD expands to this machine's home, the spelling of a local move. A
// NEW that itself contains '=' cannot be spelled on the command line; the
// interactive prompt takes any path.
func ParseMapping(s string) (Mapping, error) {
	old, dst, ok := strings.Cut(s, "=")
	if !ok {
		return Mapping{}, fmt.Errorf("invalid mapping %q: expected OLD=NEW", s)
	}
	old, dst = strings.TrimSpace(old), strings.TrimSpace(dst)
	if old == "" || dst == "" {
		return Mapping{}, fmt.Errorf("invalid mapping %q: both OLD and NEW are required", s)
	}
	if strings.Contains(dst, "=") {
		return Mapping{}, fmt.Errorf("invalid mapping %q: the new path contains '='; choose it through the interactive prompt instead", s)
	}
	oldNorm, err := normalizeForeign(old)
	if err != nil {
		return Mapping{}, fmt.Errorf("invalid mapping %q: %w", s, err)
	}
	if !filepath.IsAbs(dst) && !strings.HasPrefix(dst, "~") {
		return Mapping{}, fmt.Errorf("invalid mapping %q: new path %q is not absolute", s, dst)
	}
	newNorm, err := store.NormalizePath(dst)
	if err != nil {
		return Mapping{}, fmt.Errorf("invalid mapping %q: %w", s, err)
	}
	if !filepath.IsAbs(newNorm) {
		return Mapping{}, fmt.Errorf("invalid mapping %q: new path %q is not absolute", s, dst)
	}
	return Mapping{Old: oldNorm, New: newNorm}, nil
}

// normalizeForeign makes a recorded directory comparable: a path that is
// absolute on this OS (or spelled with a "~" prefix) goes through
// store.NormalizePath (so a macOS "/tmp" meets its "/private/tmp"
// spelling), a POSIX path from another OS is only cleaned, anything else
// is refused.
func normalizeForeign(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", errors.New("path is empty")
	case filepath.IsAbs(p), strings.HasPrefix(p, "~"):
		n, err := store.NormalizePath(p)
		if err != nil {
			return "", err
		}
		return resolveAncestors(n), nil
	case strings.HasPrefix(p, "/"):
		return path.Clean(p), nil
	case isWindowsAbs(p):
		return filepath.Clean(p), nil
	}
	return "", fmt.Errorf("path %q is not absolute", p)
}

// resolveAncestors resolves the symlinks of the longest existing ancestor
// of p (store.NormalizePath resolves only a path that exists as a whole),
// so a directory that has moved away still compares equal to a rule
// spelled through a symlinked parent — /var/x and /private/var/x on macOS.
func resolveAncestors(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveAncestors(parent), filepath.Base(p))
}

// isWindowsAbs recognises "C:\x" / "C:/x" on every OS.
func isWindowsAbs(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// ApplyMappings applies the prefix rules to a session's recorded cwd and
// project key, longest Old first (plan §9.3). A rule matches when its Old
// equals the path or is followed by a path separator in it — an exact-cwd
// rule is the degenerate case. Both sides are compared in normalised form
// (normalizeForeign); the mapped path is New joined with the remainder in
// this OS's separators. newKey is the mapped project key when a rule
// covers it and "" when none does (the caller derives the key from the
// new directory then). ok is false when no rule matches cwd — or, for an
// entry without a cwd, the key.
func ApplyMappings(maps []Mapping, cwd, projectKey string) (newCwd, newKey string, ok bool) {
	if len(maps) == 0 {
		return "", "", false
	}
	rules := sortedRules(maps)
	newCwd, cwdOK := mapPath(rules, cwd)
	newKey, keyOK := mapPath(rules, projectKey)
	switch {
	case cwd != "":
		return newCwd, newKey, cwdOK
	case keyOK:
		return newKey, newKey, true
	}
	return "", "", false
}

// rule is a mapping with its Old side normalised for comparison.
type rule struct {
	old, dst string
}

// sortedRules normalises the rules and orders them longest Old first
// (stable, so equal lengths keep the caller's order).
func sortedRules(maps []Mapping) []rule {
	rules := make([]rule, 0, len(maps))
	for _, m := range maps {
		old, err := normalizeForeign(m.Old)
		if err != nil || m.New == "" {
			continue
		}
		rules = append(rules, rule{old: old, dst: m.New})
	}
	sort.SliceStable(rules, func(i, j int) bool { return len(rules[i].old) > len(rules[j].old) })
	return rules
}

// mapPath rewrites p with the first rule that matches at a separator
// boundary. p is normalised first; "" never matches.
func mapPath(rules []rule, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	norm, err := normalizeForeign(p)
	if err != nil {
		norm = p
	}
	for _, r := range rules {
		rest, ok := cutPrefix(norm, r.old)
		if !ok {
			continue
		}
		if rest == "" {
			return r.dst, true
		}
		return filepath.Join(r.dst, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))), true
	}
	return "", false
}

// cutPrefix reports whether prefix covers p up to a separator boundary and
// returns the remainder (starting with the separator, or empty).
func cutPrefix(p, prefix string) (string, bool) {
	if prefix == "" || !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if rest == "" {
		return "", true
	}
	if isSep(rest[0]) {
		return rest, true
	}
	// A prefix that itself ends with a separator ("/" or "C:\") covers
	// whatever follows.
	if isSep(prefix[len(prefix)-1]) {
		return "/" + rest, true
	}
	return "", false
}

func isSep(c byte) bool { return c == '/' || c == '\\' }
