// Package trust reads and carries Claude Code's per-project dialog answers
// between the .claude.json files bffs manages on one machine.
//
// Claude records two dialogs per project key (the canonical git root, else
// the realpath cwd) inside projects[<key>] of the .claude.json it reads from
// CLAUDE_CONFIG_DIR: folder trust (hasTrustDialogAccepted) and the
// "Allow external CLAUDE.md file imports?" answer (the pair
// hasClaudeMdExternalIncludesApproved / hasClaudeMdExternalIncludesWarningShown;
// a decline is WarningShown=true with Approved=false). Because every bffs
// oauth account runs with its own .claude.json, those answers diverge per
// account and the dialogs come back after `bffs switch`. This package is the
// engine that reports the per-account state (Report) and copies answers from
// one file to another (Plan/Apply) under the rules of the plan's §3:
//
//   - T2 — never downgrade, never override an explicit decline. A sync only
//     turns false/absent into true; a declined source lands only on a target
//     that has never answered; Mirror copies the source verbatim.
//   - T4 — Claude's proper-lockfile lock (<path>.lock) is taken around the
//     write and held well under a second; a running claude polls the file
//     and picks the change up, so there is no liveness refusal.
//   - T5 — targets are the oauth accounts of accounts.toml plus "home"
//     (~/.claude.json). api_key accounts share the home file and are never
//     a target in their own name; orphan session dirs are never written.
//
// Folder trust is inherited: Claude walks up parent keys, bounded by the git
// root inside a repository and by the filesystem root outside one, so a
// trusted parent covers its children (EffectiveFolderTrust). The
// external-imports answer is exact-key only.
//
// Dependency rule: trust may import claudejson, transcripts, store,
// sessions and fsutil — never usage, cmd or mcpserver.
package trust

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// HomeName is the name under which Files lists ~/.claude.json — the file
// unmanaged claude and api_key accounts read. No account may carry it.
const HomeName = transcripts.HomeName

// DefaultKeys are the projects[<key>] fields a sync copies by default: the
// three dialog answers.
var DefaultKeys = []string{
	"hasTrustDialogAccepted",
	"hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown",
}

// PermissionKeys are the permission grants a sync copies only on explicit
// opt-in (Options.IncludePermissions): tool allow rules and project-scope
// MCP server approvals.
var PermissionKeys = []string{
	"allowedTools",
	"mcpServers",
	"enabledMcpjsonServers",
	"disabledMcpjsonServers",
	"mcpContextUris",
}

// The individual dialog keys, by role.
const (
	keyTrust    = "hasTrustDialogAccepted"
	keyApproved = "hasClaudeMdExternalIncludesApproved"
	keyShown    = "hasClaudeMdExternalIncludesWarningShown"
)

// Answer is the effective state of one dialog for one project key in one
// .claude.json.
type Answer int

const (
	// Unset: never answered on that file; claude will ask.
	Unset Answer = iota
	// Accepted: answered yes at the exact key.
	Accepted
	// Declined: answered no at the exact key (external imports only —
	// folder trust has no persistent "no", claude just asks again).
	Declined
	// Inherited: not answered at the exact key, but an ancestor key is
	// trusted so claude will not ask (folder trust only).
	Inherited
)

// String renders the answer the way `bffs trust` prints it.
func (a Answer) String() string {
	switch a {
	case Unset:
		return "unset"
	case Accepted:
		return "accepted"
	case Declined:
		return "declined"
	case Inherited:
		return "inherited"
	}
	return fmt.Sprintf("Answer(%d)", int(a))
}

// trusts reports whether an answer means claude will not show the folder
// trust dialog.
func trusts(a Answer) bool {
	return a == Accepted || a == Inherited
}

// Status is one row of the trust matrix: what one .claude.json says about
// one project key.
type Status struct {
	Account    string // account name, or HomeName
	File       string // the .claude.json read
	ProjectKey string
	Present    bool   // projects[ProjectKey] exists in the file
	Folder     Answer // effective folder trust (exact key, else ancestor walk)
	External   Answer // external-imports answer (exact key only)
	// InheritedFrom is the ancestor key whose trust covers ProjectKey when
	// Folder is Inherited.
	InheritedFrom string
	Tools         int // allowedTools entries
	MCPEnabled    int // enabledMcpjsonServers entries
}

// Files maps every name a sync can address to its .claude.json: HomeName to
// homeJSON (claudejson.Path() when empty) and every oauth account of accs
// to <cfgDir>/sessions/<name>/.claude.json. api_key accounts are left out
// — they share the home file — and so are session dirs on disk that
// accounts.toml does not list (orphans are never targets). Files refuses
// to work while an account is literally named HomeName, because that name
// would be ambiguous.
func Files(cfgDir, homeJSON string, accs store.Accounts) (map[string]string, error) {
	if _, taken := accs.Accounts[HomeName]; taken {
		return nil, fmt.Errorf("%q is reserved for ~/.claude.json: rename that account (bffs rename)", HomeName)
	}
	if homeJSON == "" {
		p, err := claudejson.Path()
		if err != nil {
			return nil, err
		}
		homeJSON = p
	}
	files := map[string]string{HomeName: homeJSON}
	for _, name := range accs.Names() {
		if accs.Accounts[name].Type != store.TypeOAuth {
			continue
		}
		files[name] = filepath.Join(sessions.Dir(cfgDir, name), claudejson.Filename)
	}
	return files, nil
}

// JSONPathFor resolves a --from/--to name to the .claude.json it denotes.
// A name Files listed returns its path. An api_key account — known to accs
// but absent from files because it shares ~/.claude.json — is refused with
// a pointer at HomeName; anything else is unknown.
func JSONPathFor(files map[string]string, accs store.Accounts, name string) (string, error) {
	if p, ok := files[name]; ok {
		return p, nil
	}
	if acc, ok := accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return "", fmt.Errorf("account %q is an api_key account; it shares ~/.claude.json with unmanaged claude — use --to %s", name, HomeName)
	}
	return "", fmt.Errorf("unknown account %q; known: %v", name, knownNames(files))
}

// knownNames lists every name in files, sorted.
func knownNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// reportOrder lists the names in files the way the matrix prints them:
// accounts sorted by name, HomeName last.
func reportOrder(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		if n != HomeName {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if _, ok := files[HomeName]; ok {
		names = append(names, HomeName)
	}
	return names
}

// EffectiveFolderTrust is the folder-trust answer claude computes for key
// from a file's projects flags: Accepted when projects[key] itself has
// hasTrustDialogAccepted=true; otherwise the parent keys are walked upward
// — inside a repository up to and including gitRoot, outside one (gitRoot
// empty) up to the filesystem root — and the first trusted ancestor yields
// (Inherited, thatKey). Anything else, including an explicit false at the
// exact key, is Unset: claude keeps no "declined" folder trust, a "no" just
// makes it ask again next time.
func EffectiveFolderTrust(flags map[string]claudejson.ProjectFlags, key string, gitRoot string) (Answer, string) {
	if trustedAt(flags, key) {
		return Accepted, ""
	}
	cur := filepath.Clean(key)
	if gitRoot != "" {
		gitRoot = filepath.Clean(gitRoot)
	}
	for cur != gitRoot {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
		if trustedAt(flags, cur) {
			return Inherited, cur
		}
	}
	return Unset, ""
}

func trustedAt(flags map[string]claudejson.ProjectFlags, key string) bool {
	f, ok := flags[key]
	return ok && f.TrustAccepted != nil && *f.TrustAccepted
}

// externalAnswer classifies the external-imports pair: Approved=true is
// Accepted, WarningShown=true without approval is Declined, else Unset.
func externalAnswer(approved, shown *bool) Answer {
	switch {
	case approved != nil && *approved:
		return Accepted
	case shown != nil && *shown:
		return Declined
	}
	return Unset
}

// Report reads every file in files and returns one Status per name for
// projectKey, ordered accounts-by-name with HomeName last. gitRoot bounds
// the folder-trust ancestor walk (the repository root when projectKey is
// inside one, "" otherwise). A missing file reads as "never answered";
// an unparseable one is an error naming the account.
func Report(files map[string]string, projectKey, gitRoot string) ([]Status, error) {
	names := reportOrder(files)
	out := make([]Status, 0, len(names))
	for _, name := range names {
		path := files[name]
		flags, err := claudejson.ReadProjectFlags(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		st := Status{Account: name, File: path, ProjectKey: projectKey}
		st.Folder, st.InheritedFrom = EffectiveFolderTrust(flags, projectKey, gitRoot)
		if f, ok := flags[projectKey]; ok {
			st.Present = true
			st.External = externalAnswer(f.ExternalIncludesApproved, f.ExternalIncludesWarningShown)
			st.Tools, st.MCPEnabled = f.AllowedTools, f.MCPEnabled
		}
		out = append(out, st)
	}
	return out, nil
}

// BestSource picks the file a sync should copy from. Within each tier the
// order is the active account, then the accounts by name, then HomeName:
// first a file that accepted folder trust at the exact key — the answer
// Plan can actually copy — then one that only inherits it from a trusted
// ancestor (its external-imports answer is still worth carrying; its
// folder trust is not an entry Plan can copy). ok is false when no file
// effectively trusts the project.
func BestSource(statuses []Status, active string) (name string, ok bool) {
	sorted := append([]Status(nil), statuses...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Account < sorted[j].Account })
	for _, want := range []Answer{Accepted, Inherited} {
		for _, s := range sorted {
			if s.Account == active && s.Folder == want {
				return s.Account, true
			}
		}
		var home *Status
		for i := range sorted {
			s := &sorted[i]
			if s.Account == HomeName {
				home = s
				continue
			}
			if s.Folder == want {
				return s.Account, true
			}
		}
		if home != nil && home.Folder == want {
			return HomeName, true
		}
	}
	return "", false
}

// LiveLines returns the sessions open in a running claude across the
// config dirs behind files — the directory of each account's .claude.json,
// and ~/.claude for HomeName (the .claude directory next to ~/.claude.json)
// — sorted by pid. It feeds the informational `live:` line of `bffs
// trust`; nothing here refuses a write.
func LiveLines(ctx context.Context, files map[string]string) ([]transcripts.LiveSession, error) {
	seen := map[string]bool{}
	var dirs []string
	for _, name := range reportOrder(files) {
		dir := configDirOf(name, files[name])
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	live, err := transcripts.Live(ctx, dirs)
	if err != nil {
		return nil, err
	}
	out := make([]transcripts.LiveSession, 0, len(live))
	for _, ls := range live {
		out = append(out, ls)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out, nil
}

// configDirOf is the Claude config dir whose .claude.json is jsonPath: the
// account dir itself, or ~/.claude for the home file.
func configDirOf(name, jsonPath string) string {
	if name == HomeName {
		return filepath.Join(filepath.Dir(jsonPath), ".claude")
	}
	return filepath.Dir(jsonPath)
}
