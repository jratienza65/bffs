package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
	"github.com/jratienza65/bffs/internal/usage"
)

// The read-only half of the session/memory transfer tools (plan §10.3):
// list_sessions, list_memories and trust_status are the MCP twins of
// `bffs sessions list --json`, `bffs memory list --json` and `bffs trust`.
// They walk the same internal packages the CLI does (transcripts, usage,
// imports, trust) and return the same rows; they never open a transcript
// beyond the title windows, never return memory contents or the grants
// inside .claude.json, and never write anything.

const (
	// listSessionsDefaultLimit and listSessionsMaxLimit bound how many
	// sessions one list_sessions call reads title windows for.
	listSessionsDefaultLimit = 50
	listSessionsMaxLimit     = 500

	// attributionCaveat is the footer `bffs sessions list` prints under every
	// listing, phrased for a JSON consumer.
	attributionCaveat = "account is best-effort attribution (launch log + lastSessionId + import records; never a token scan); an empty account means unknown."

	// trustWriteCaveat is why trust_status has no write twin.
	trustWriteCaveat = "Read-only: nothing was written. Carrying dialog answers between accounts is a human decision made on the CLI — a person runs suggested_command (bffs trust sync) in a terminal; a running claude on the target account picks the change up within about a second."
)

// catalog is what the session and memory tools need to address Claude's
// on-disk pools: the accounts, the state and every root — the same data
// `bffs sessions` and `bffs memory` load. It is re-read on every call: the
// server is long-lived and accounts.toml changes underneath it.
type catalog struct {
	cfgDir string
	accs   store.Accounts
	state  store.State
	roots  []transcripts.Root
}

func (h *handlers) loadCatalog() (*catalog, error) {
	accs, err := store.LoadAccounts(h.cfgDir)
	if err != nil {
		return nil, err
	}
	state, err := store.LoadState(h.cfgDir)
	if err != nil {
		return nil, err
	}
	roots, err := transcripts.Roots(h.cfgDir, h.homeClaudeDir, accs, state)
	if err != nil {
		return nil, err
	}
	return &catalog{cfgDir: h.cfgDir, accs: accs, state: state, roots: roots}, nil
}

// configDirs lists the distinct Claude config dirs behind every root, in
// root order — the dirs whose runtime sessions/ transcripts.Live reads.
func (c *catalog) configDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, r := range c.roots {
		if seen[r.ConfigDir] {
			continue
		}
		seen[r.ConfigDir] = true
		dirs = append(dirs, r.ConfigDir)
	}
	return dirs
}

// homeRoot is the unmanaged ~/.claude root (the shared pool under partial
// isolation).
func (c *catalog) homeRoot() (transcripts.Root, error) {
	return transcripts.RootFor(c.roots, transcripts.HomeName)
}

// rootFor picks the root a tool call works on, the way `bffs sessions list`
// does. A named account is that account's root — an api_key account runs
// claude against ~/.claude so it maps to the home root, "home" is the home
// root itself, an orphan session dir answers to its name. With no account
// it is the root claude would write to when launched in dir: the root of
// the account the resolver picks there (BFFS_ACCOUNT, bffs.toml, directory
// rule, then the global default), else the home root. A failed resolution
// comes back as a warning, never an error: browsing must not depend on a
// valid pin.
func (c *catalog) rootFor(account, dir string) (root transcripts.Root, warning string, err error) {
	if account != "" {
		if acc, ok := c.accs.Get(account); ok && acc.Type == store.TypeAPIKey {
			root, err = c.homeRoot()
			return root, "", err
		}
		root, err = transcripts.RootFor(c.roots, account)
		return root, "", err
	}
	home, err := c.homeRoot()
	if err != nil {
		return transcripts.Root{}, "", err
	}
	r, err := resolver.Resolve(c.cfgDir, dir)
	if err != nil {
		return home, fmt.Sprintf("%v; using the home root", err), nil
	}
	if r.Source == resolver.SourceNone || r.Account.Type != store.TypeOAuth {
		return home, "", nil
	}
	root, err = transcripts.RootFor(c.roots, r.Account.Name)
	if err != nil {
		return home, fmt.Sprintf("%v; using the home root", err), nil
	}
	return root, "", nil
}

// otherRootsNote names the roots a call did not list, so a model learns
// that a full-isolation account or an orphan dir keeps its own pool. Empty
// when root is the only one.
func (c *catalog) otherRootsNote(root transcripts.Root) string {
	var others []string
	for _, r := range c.roots {
		if r.Dir == root.Dir {
			continue
		}
		others = append(others, rootLabel(r))
	}
	if len(others) == 0 {
		return ""
	}
	return "Other roots on this machine, not listed here (pass account to list one): " + strings.Join(others, "; ") + "."
}

// rootLabel names a root: the accounts sharing a pool, the owner of a
// full-isolation root, an orphan dir, or the unmanaged home dir.
func rootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan session dir %s (account %q no longer in accounts.toml; read-only)", transcripts.Sanitize(r.ConfigDir), transcripts.Sanitize(r.Owner))
	case r.Owner != "":
		return "account " + transcripts.Sanitize(r.Owner) + " (full isolation)"
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool of " + transcripts.Sanitize(strings.Join(r.Accounts, ", "))
	default:
		return "home " + transcripts.Sanitize(r.ConfigDir)
	}
}

type ListSessionsIn struct {
	Account       string `json:"account,omitempty" jsonschema:"bffs account whose root to list (home = the unmanaged ~/.claude tree); empty = the account claude would use in directory, else the home root. An api_key account shares ~/.claude and maps to home"`
	Directory     string `json:"directory,omitempty" jsonschema:"project directory; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly"`
	AllProjects   bool   `json:"all_projects,omitempty" jsonschema:"list every project in the root, not just the one at directory"`
	PendingRehome bool   `json:"pending_rehome,omitempty" jsonschema:"only imported sessions whose recorded directory does not exist on this machine yet - the ones bffs rehome is for"`
	Limit         int    `json:"limit,omitempty" jsonschema:"newest N sessions; default 50, max 500"`
}

// SessionInfo is one row: the same shape as `bffs sessions list --json`.
type SessionInfo struct {
	SessionID     string `json:"session_id"`
	Root          string `json:"root" jsonschema:"the projects/ directory the transcript lives in"`
	Account       string `json:"account" jsonschema:"bffs account that ran the session, best-effort; empty = unknown"`
	AccountSource string `json:"account_source" jsonschema:"tier that attributed the account: root, last-session, import, or launch-log; empty when unknown"`
	Cwd           string `json:"cwd" jsonschema:"effective working directory (after any relocation)"`
	CwdExists     bool   `json:"cwd_exists" jsonschema:"cwd is a directory on this machine"`
	Slug          string `json:"slug" jsonschema:"the projects/ entry name"`
	Title         string `json:"title"`
	TitleSource   string `json:"title_source" jsonschema:"custom, ai, last-prompt, summary, first-prompt, or history; empty with an empty title"`
	GitBranch     string `json:"git_branch"`
	ClaudeVersion string `json:"claude_version"`
	FirstAt       string `json:"first_at" jsonschema:"RFC3339; empty when unknown"`
	LastAt        string `json:"last_at" jsonschema:"RFC3339 (transcript mtime when the tail has no timestamp)"`
	SizeBytes     int64  `json:"size_bytes"`
	Subagents     int    `json:"subagents" jsonschema:"subagent transcripts in the sidecar dir"`
	Live          bool   `json:"live" jsonschema:"open in a running claude right now"`
	BundleID      string `json:"bundle_id" jsonschema:"bffs import record the session arrived with; empty when not imported"`
	OldCwd        string `json:"old_cwd" jsonschema:"directory on the source machine, from the import record"`
	OldHome       string `json:"old_home"`
	OldHost       string `json:"old_host"`
	GitRemote     string `json:"git_remote"`
}

type ListSessionsOut struct {
	Sessions []SessionInfo `json:"sessions"`
	Roots    []string      `json:"roots" jsonschema:"the projects/ directories that were listed"`
	Note     string        `json:"note"`
}

func (h *handlers) listSessions(ctx context.Context, req *mcp.CallToolRequest, in ListSessionsIn) (*mcp.CallToolResult, ListSessionsOut, error) {
	out := ListSessionsOut{Sessions: []SessionInfo{}, Roots: []string{}}
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	c, err := h.loadCatalog()
	if err != nil {
		return nil, out, err
	}
	root, warning, err := c.rootFor(in.Account, dir)
	if err != nil {
		return nil, out, err
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if root.Orphan {
		warnings = append(warnings, rootLabel(root))
	}
	out.Roots = append(out.Roots, transcripts.Sanitize(root.Dir))

	limit := clampLimit(in.Limit)
	opts := transcripts.ListOptions{Titles: true, Limit: limit, Now: time.Now()}
	if in.PendingRehome {
		// The filter needs every session's cwd; the limit applies after it.
		opts.Limit = 0
	}
	// Attribution, import records and liveness are enrichment: a problem
	// with any of them is a warning in the note, and the listing still
	// comes out.
	if attr, err := usage.NewAttributor(h.cfgDir, c.accs); err != nil {
		warnings = append(warnings, "attribution unavailable: "+err.Error())
	} else {
		opts.Attributor = attr
	}
	if recs, err := imports.Load(h.cfgDir); err != nil {
		warnings = append(warnings, "import records unavailable: "+err.Error())
	} else {
		opts.Imports = imports.BySession(recs)
	}
	if live, err := transcripts.Live(ctx, c.configDirs()); err != nil {
		warnings = append(warnings, "liveness unavailable: "+err.Error())
	} else {
		opts.Live = live
	}

	scope := "every project in the root"
	if !in.AllProjects {
		scope = "project " + transcripts.Sanitize(dir)
		// Locate the projects/<slug> directory the way Claude does, so a
		// relocated or hash-suffixed slug is found rather than predicted.
		pd, err := transcripts.ProjectDirFor(root, dir, os.Environ())
		if err != nil {
			if errors.Is(err, transcripts.ErrSlugTooLong) {
				return nil, out, fmt.Errorf("project %s: %w, and no existing project dir under %s records it", dir, err, root.Dir)
			}
			return nil, out, err
		}
		if info, err := os.Stat(pd); err != nil || !info.IsDir() {
			out.Note = joinNote(fmt.Sprintf("No sessions: %s has no projects/ directory under %s (claude has not been launched there with this root). %s", scope, transcripts.Sanitize(root.Dir), attributionCaveat), c.otherRootsNote(root), warnings)
			return nil, out, nil
		}
		opts.Slug = filepath.Base(pd)
	}

	ss, err := transcripts.List(ctx, root, opts)
	if err != nil {
		return nil, out, err
	}
	if in.PendingRehome {
		ss = pendingRehome(ss, limit)
	}
	for _, s := range ss {
		out.Sessions = append(out.Sessions, sessionInfoOf(s))
	}

	summary := fmt.Sprintf("%d session(s) for %s in %s, newest first (limit %d). %s Transcript content is never returned; a person resumes one in a terminal with `cd <cwd> && claude --resume <session_id>` (prefix BFFS_ACCOUNT=<account> on a full-isolation account's root).",
		len(out.Sessions), scope, rootLabel(root), limit, attributionCaveat)
	out.Note = joinNote(summary, c.otherRootsNote(root), warnings)
	return nil, out, nil
}

// clampLimit applies list_sessions's default and ceiling.
func clampLimit(n int) int {
	if n <= 0 {
		return listSessionsDefaultLimit
	}
	return min(n, listSessionsMaxLimit)
}

// pendingRehome keeps the sessions an import record knows whose directory
// does not exist on this machine — the ones `bffs rehome` is for — at most
// limit of them (0 = all).
func pendingRehome(ss []transcripts.Session, limit int) []transcripts.Session {
	var out []transcripts.Session
	for _, s := range ss {
		if s.Import == nil || s.CwdExists {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// sessionInfoOf converts a catalog session to its row. Every string that
// came out of a transcript, an import record or a directory listing passes
// through Sanitize.
func sessionInfoOf(s transcripts.Session) SessionInfo {
	info := SessionInfo{
		SessionID:     transcripts.Sanitize(s.ID),
		Root:          transcripts.Sanitize(s.Root.Dir),
		Account:       transcripts.Sanitize(s.Account),
		AccountSource: transcripts.Sanitize(s.AttribSource),
		Cwd:           transcripts.Sanitize(s.Cwd),
		CwdExists:     s.CwdExists,
		Slug:          transcripts.Sanitize(s.Slug),
		Title:         transcripts.Sanitize(s.Title),
		TitleSource:   s.TitleSource,
		GitBranch:     transcripts.Sanitize(s.GitBranch),
		ClaudeVersion: transcripts.Sanitize(s.Version),
		FirstAt:       rfc3339(s.FirstTS),
		LastAt:        rfc3339(s.LastTS),
		SizeBytes:     s.Size,
		Subagents:     s.Subagents,
		Live:          s.Live,
	}
	if s.Import != nil && s.Import.Record != nil {
		info.BundleID = transcripts.Sanitize(s.Import.Record.BundleID)
		info.OldHome = transcripts.Sanitize(s.Import.Record.Source.Home)
		info.OldHost = transcripts.Sanitize(s.Import.Record.Source.Hostname)
		if s.Import.Session != nil {
			info.OldCwd = transcripts.Sanitize(s.Import.Session.OldCwd)
			info.GitRemote = transcripts.Sanitize(s.Import.Session.GitRemote)
		}
	}
	return info
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type ListMemoriesIn struct {
	Account     string `json:"account,omitempty" jsonschema:"bffs account whose root to look in (home = the unmanaged ~/.claude tree); empty = the account claude would use in directory, else the home root. An api_key account maps to home"`
	Directory   string `json:"directory,omitempty" jsonschema:"project directory; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly"`
	AllProjects bool   `json:"all_projects,omitempty" jsonschema:"every auto-memory directory in the root, not just the project's"`
}

type MemoryFileInfo struct {
	Name          string   `json:"name" jsonschema:"path relative to the memory dir, slash-separated"`
	SizeBytes     int64    `json:"size_bytes"`
	ModifiedAt    string   `json:"modified_at" jsonschema:"RFC3339"`
	Pinned        bool     `json:"pinned" jsonschema:"frontmatter pinned: true - injected into every session claude starts for the project"`
	AbsolutePaths []string `json:"absolute_paths" jsonschema:"distinct absolute paths mentioned in the file - the lines to review after a rehome"`
	AtRefs        []string `json:"at_refs" jsonschema:"distinct @-references (@/ @~ @.) - the includes that can raise the external CLAUDE.md imports dialog"`
}

type MemoryInfo struct {
	Root      string           `json:"root" jsonschema:"the projects/ directory the memory lives in"`
	Cwd       string           `json:"cwd" jsonschema:"working directory of the sibling transcripts; empty when the slug has none"`
	CwdExists bool             `json:"cwd_exists"`
	Slug      string           `json:"slug"`
	Dir       string           `json:"dir" jsonschema:"the memory directory itself"`
	HasIndex  bool             `json:"has_index" jsonschema:"MEMORY.md exists"`
	Files     []MemoryFileInfo `json:"files"`
}

type ListMemoriesOut struct {
	Memories []MemoryInfo `json:"memories"`
	Note     string       `json:"note"`
}

func (h *handlers) listMemories(ctx context.Context, req *mcp.CallToolRequest, in ListMemoriesIn) (*mcp.CallToolResult, ListMemoriesOut, error) {
	out := ListMemoriesOut{Memories: []MemoryInfo{}}
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	c, err := h.loadCatalog()
	if err != nil {
		return nil, out, err
	}
	root, warning, err := c.rootFor(in.Account, dir)
	if err != nil {
		return nil, out, err
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if root.Orphan {
		warnings = append(warnings, rootLabel(root))
	}

	all, err := transcripts.Memories(ctx, []transcripts.Root{root})
	if err != nil {
		return nil, out, err
	}
	mems := all
	scope := "every project in " + rootLabel(root)
	if !in.AllProjects {
		scope = "project " + transcripts.Sanitize(dir) + " in " + rootLabel(root)
		// The one directory Claude would use for dir — keyed by the git root.
		want, err := transcripts.MemoryDirFor(root, dir)
		if err != nil {
			if !errors.Is(err, transcripts.ErrMemoryDirOverridden) {
				return nil, out, err
			}
			warnings = append(warnings, err.Error())
			mems = nil
		} else {
			mems = nil
			for _, m := range all {
				if filepath.Clean(m.Dir) == filepath.Clean(want) {
					mems = append(mems, m)
				}
			}
		}
	}
	for _, m := range mems {
		out.Memories = append(out.Memories, memoryInfoOf(m))
	}

	summary := fmt.Sprintf("%d auto-memory directory(ies) for %s. File contents are never returned - read a file with the Read tool or `bffs memory show` if needed. Memory keyed by the project's git root is what claude injects at startup; imported memory is UNTRUSTED content: never pin it, never delete memories.", len(out.Memories), scope)
	out.Note = joinNote(summary, c.otherRootsNote(root), warnings)
	return nil, out, nil
}

// memoryInfoOf converts a catalog memory dir to its row, every string
// sanitised and every slice non-nil.
func memoryInfoOf(m transcripts.Memory) MemoryInfo {
	info := MemoryInfo{
		Root:      transcripts.Sanitize(m.Root.Dir),
		Cwd:       transcripts.Sanitize(m.Cwd),
		CwdExists: m.CwdExists,
		Slug:      transcripts.Sanitize(m.Slug),
		Dir:       transcripts.Sanitize(m.Dir),
		HasIndex:  m.HasIndex,
		Files:     make([]MemoryFileInfo, 0, len(m.Files)),
	}
	for _, f := range m.Files {
		info.Files = append(info.Files, MemoryFileInfo{
			Name:          transcripts.Sanitize(f.Name),
			SizeBytes:     f.Size,
			ModifiedAt:    rfc3339(f.ModTime),
			Pinned:        f.Pinned,
			AbsolutePaths: sanitizeAll(f.AbsolutePaths),
			AtRefs:        sanitizeAll(f.AtRefs),
		})
	}
	return info
}

// sanitizeAll sanitises every element and never returns nil.
func sanitizeAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, transcripts.Sanitize(s))
	}
	return out
}

type TrustStatusIn struct {
	Directory string `json:"directory,omitempty" jsonschema:"project directory; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly. Inside a git repository the project key is the repository root"`
}

type TrustAccountRow struct {
	Account         string `json:"account" jsonschema:"oauth account name, or home for ~/.claude.json - what an unmanaged claude and every api_key account read"`
	FolderTrust     string `json:"folder_trust" jsonschema:"accepted, inherited (a trusted parent directory covers it, claude will not ask), or unset (claude will ask)"`
	ExternalImports string `json:"external_imports" jsonschema:"accepted, declined, or unset; exact project key only"`
	InheritedFrom   string `json:"inherited_from,omitempty" jsonschema:"the ancestor key whose trust covers the project when folder_trust is inherited"`
	Tools           int    `json:"tools" jsonschema:"number of allowedTools entries (the rules themselves are never returned)"`
	MCPEnabled      int    `json:"mcp_enabled" jsonschema:"number of enabledMcpjsonServers entries"`
}

type TrustStatusOut struct {
	ProjectKey       string            `json:"project_key" jsonschema:"the key claude files the project under in .claude.json: the canonical git root, else the resolved directory"`
	Accounts         []TrustAccountRow `json:"accounts" jsonschema:"one row per oauth account, by name, then home"`
	SuggestedCommand string            `json:"suggested_command,omitempty" jsonschema:"the exact bffs trust sync command that carries answers onto the first account missing one; empty when every account has what another has"`
	Note             string            `json:"note"`
}

// trustStatus reads the per-account dialog answers for a project and
// never writes: the write twin is deliberately a CLI-only command.
func (h *handlers) trustStatus(ctx context.Context, req *mcp.CallToolRequest, in TrustStatusIn) (*mcp.CallToolResult, TrustStatusOut, error) {
	out := TrustStatusOut{Accounts: []TrustAccountRow{}}
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	key, err := transcripts.ProjectKey(dir)
	if err != nil {
		return nil, out, err
	}
	accs, err := store.LoadAccounts(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	homeJSON := ""
	if h.homeClaudeDir != "" {
		homeJSON = filepath.Join(filepath.Dir(h.homeClaudeDir), claudejson.Filename)
	}
	files, err := trust.Files(h.cfgDir, homeJSON, accs)
	if err != nil {
		return nil, out, err
	}
	gitRoot := ""
	if r, ok := transcripts.GitRoot(key); ok {
		gitRoot = r
	}
	statuses, err := trust.Report(files, key, gitRoot)
	if err != nil {
		return nil, out, err
	}

	out.ProjectKey = transcripts.Sanitize(key)
	for _, s := range statuses {
		out.Accounts = append(out.Accounts, TrustAccountRow{
			Account:         transcripts.Sanitize(s.Account),
			FolderTrust:     s.Folder.String(),
			ExternalImports: s.External.String(),
			InheritedFrom:   transcripts.Sanitize(s.InheritedFrom),
			Tools:           s.Tools,
			MCPEnabled:      s.MCPEnabled,
		})
	}
	note := "Folder trust and the external-CLAUDE.md-imports answer are recorded per account in the .claude.json each one reads, so the dialogs come back after `bffs switch`. api_key accounts and an unmanaged claude read the home row. " + trustWriteCaveat
	if target, ok := trustSyncTarget(statuses); ok {
		out.SuggestedCommand = "bffs trust sync --to " + shellWord(target) + " --project " + shellWord(out.ProjectKey)
		note = fmt.Sprintf("%q has not answered a dialog another row has; suggested_command carries the answers over (never downgrades, never overrides a decline). ", target) + note
	} else {
		note = "Every row already has each answer some other row has; nothing to carry over. " + note
	}
	out.Note = note
	return nil, out, nil
}

// trustSyncTarget picks the row the hint points at: the first row (accounts
// by name, home last) with a dialog unanswered that another row answered.
// Only an exact-key folder answer elsewhere counts — an inherited one is
// not an entry a sync can copy.
func trustSyncTarget(statuses []trust.Status) (string, bool) {
	for _, s := range statuses {
		folderElsewhere, externalElsewhere := false, false
		for _, o := range statuses {
			if o.Account == s.Account {
				continue
			}
			if o.Folder == trust.Accepted {
				folderElsewhere = true
			}
			if o.External != trust.Unset {
				externalElsewhere = true
			}
		}
		if (s.Folder == trust.Unset && folderElsewhere) || (s.External == trust.Unset && externalElsewhere) {
			return s.Account, true
		}
	}
	return "", false
}

// shellWord quotes s for a POSIX shell when it needs it, so a suggested
// command survives a path with spaces.
func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-./~:@+,\\", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// joinNote assembles a tool note from its summary, an optional extra
// sentence and the warnings gathered along the way.
func joinNote(summary, extra string, warnings []string) string {
	parts := []string{summary}
	if extra != "" {
		parts = append(parts, extra)
	}
	if len(warnings) > 0 {
		parts = append(parts, "Warnings: "+strings.Join(warnings, "; ")+".")
	}
	return strings.Join(parts, " ")
}
