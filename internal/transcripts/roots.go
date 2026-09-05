package transcripts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

// HomeName is the name RootFor accepts for the unmanaged ~/.claude tree
// (its .claude.json is ~/.claude.json). No account may be called this;
// RootFor refuses to work while one is.
const HomeName = "home"

// Root is one projects/ pool as Claude sees it, together with the config
// dir it belongs to. Under partial isolation every account's projects/ is
// a symlink into ~/.claude/projects, so the home root is Shared and lists
// those accounts; under full isolation an account has its own root with
// Owner set. Orphan roots are session dirs on disk that accounts.toml no
// longer knows — valid read-only sources, never destinations.
type Root struct {
	Dir        string   // the projects/ directory (may not exist yet)
	ConfigDir  string   // the Claude config dir that owns Dir
	Owner      string   // account name; "" for the home root
	Shared     bool     // partial-isolation accounts share this pool
	Accounts   []string // the accounts sharing it, sorted
	Orphan     bool     // session dir without an account behind it
	ClaudeJSON string   // the .claude.json paired with ConfigDir
}

// Roots enumerates every projects/ pool on this machine: the home root
// (<homeClaudeDir>/projects, "" meaning ~/.claude, paired with the
// .claude.json next to that directory — ~/.claude.json), one root per
// oauth account whose <account dir>/projects is a real directory, and one
// per orphan session dir under <cfgDir>/sessions with a real projects/.
// An account whose projects/ is a symlink (partial isolation) is attached
// to the root the link resolves to, which becomes Shared; an account with
// nothing on disk yet is placed by its effective isolation preset. Roots
// are deduplicated by symlink-resolved path, nothing is created, and the
// result is ordered home first, then owners by name, orphans last.
func Roots(cfgDir, homeClaudeDir string, accs store.Accounts, state store.State) ([]Root, error) {
	if homeClaudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home directory: %w", err)
		}
		homeClaudeDir = filepath.Join(home, ".claude")
	}
	homeClaudeDir = filepath.Clean(homeClaudeDir)

	var roots []Root
	index := map[string]int{} // canonical projects path → index into roots
	add := func(r Root) int {
		c := canonical(r.Dir)
		if i, ok := index[c]; ok {
			return i
		}
		index[c] = len(roots)
		roots = append(roots, r)
		return len(roots) - 1
	}
	share := func(i int, name string) {
		roots[i].Shared = true
		roots[i].Accounts = append(roots[i].Accounts, name)
	}

	homeIdx := add(Root{
		Dir:        filepath.Join(homeClaudeDir, ProjectsSubdir),
		ConfigDir:  homeClaudeDir,
		ClaudeJSON: filepath.Join(filepath.Dir(homeClaudeDir), claudejson.Filename),
	})

	for _, name := range accs.Names() {
		acc := accs.Accounts[name]
		if acc.Type != store.TypeOAuth {
			continue
		}
		accDir := sessions.Dir(cfgDir, name)
		p := filepath.Join(accDir, ProjectsSubdir)
		own := Root{Dir: p, ConfigDir: accDir, Owner: name, ClaudeJSON: filepath.Join(accDir, claudejson.Filename)}
		info, err := os.Lstat(p)
		switch {
		case err == nil && info.IsDir():
			// A real directory: this account's own pool — full isolation, or
			// a partial account where claude wrote a real dir in the link's
			// place. Disk truth wins over the preset.
			if i, ok := index[canonical(p)]; ok {
				share(i, name)
				continue
			}
			add(own)
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			// Partial isolation: a link into the shared pool.
			if i, ok := index[symlinkCanonical(p)]; ok {
				share(i, name)
				continue
			}
			if _, err := filepath.EvalSymlinks(p); err == nil {
				// A link to some existing pool nobody else lists: its own root.
				add(own)
				continue
			}
			// Dangling — the home pool has not been created yet; the only
			// thing bffs ever links to.
			share(homeIdx, name)
		default:
			// Nothing on disk yet (never launched): go by the preset.
			if store.ResolveIsolation(acc.Isolation, state.Isolation) == store.IsolationFull {
				add(own)
			} else {
				share(homeIdx, name)
			}
		}
	}

	sessionsDir := filepath.Join(cfgDir, sessions.SessionsSubdir)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", sessionsDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if _, known := accs.Accounts[name]; known || !e.IsDir() {
			continue
		}
		accDir := filepath.Join(sessionsDir, name)
		p := filepath.Join(accDir, ProjectsSubdir)
		if info, err := os.Lstat(p); err != nil || !info.IsDir() {
			continue
		}
		add(Root{Dir: p, ConfigDir: accDir, Owner: name, Orphan: true, ClaudeJSON: filepath.Join(accDir, claudejson.Filename)})
	}

	sort.SliceStable(roots, func(i, j int) bool {
		ri, rj := rootRank(roots[i]), rootRank(roots[j])
		if ri != rj {
			return ri < rj
		}
		return roots[i].Owner < roots[j].Owner
	})
	for i := range roots {
		sort.Strings(roots[i].Accounts)
	}
	return roots, nil
}

// RootFor picks the root an account works in: "" or HomeName is the home
// root; a partial-isolation account is the shared root it is attached to;
// a full-isolation account is its own root; an orphan name is its read-only
// root. It refuses to answer while any root's owner or attached account is
// literally named HomeName, because that name would be ambiguous.
func RootFor(roots []Root, account string) (Root, error) {
	for _, r := range roots {
		if r.Owner == HomeName {
			return Root{}, fmt.Errorf("%q is reserved for ~/.claude.json: rename that account (bffs rename) or remove the stray directory %s", HomeName, r.ConfigDir)
		}
		for _, a := range r.Accounts {
			if a == HomeName {
				return Root{}, fmt.Errorf("%q is reserved for ~/.claude.json: rename that account (bffs rename)", HomeName)
			}
		}
	}
	if account == "" || account == HomeName {
		for _, r := range roots {
			if r.Owner == "" && !r.Orphan {
				return r, nil
			}
		}
		return Root{}, errors.New("no home root")
	}
	for _, r := range roots {
		if !r.Orphan && r.Owner == account {
			return r, nil
		}
	}
	for _, r := range roots {
		for _, a := range r.Accounts {
			if a == account {
				return r, nil
			}
		}
	}
	for _, r := range roots {
		if r.Orphan && r.Owner == account {
			return r, nil
		}
	}
	return Root{}, fmt.Errorf("unknown account %q; known: %v", account, knownNames(roots))
}

// knownNames lists every name RootFor accepts, sorted, HomeName included.
func knownNames(roots []Root) []string {
	seen := map[string]bool{HomeName: true}
	names := []string{HomeName}
	for _, r := range roots {
		for _, n := range append([]string{r.Owner}, r.Accounts...) {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func rootRank(r Root) int {
	switch {
	case r.Orphan:
		return 2
	case r.Owner == "":
		return 0
	default:
		return 1
	}
}

// canonical resolves symlinks when the path exists and cleans it otherwise;
// it is the dedupe key for roots, exactly as in usage.scan.
func canonical(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// symlinkCanonical is canonical for a symlink whose target may not exist
// yet: a dangling link resolves to its (cleaned, absolute) target.
func symlinkCanonical(link string) string {
	if r, err := filepath.EvalSymlinks(link); err == nil {
		return r
	}
	target, err := os.Readlink(link)
	if err != nil {
		return filepath.Clean(link)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target)
}
