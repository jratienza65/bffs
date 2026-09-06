package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/usage"
)

var (
	sessionsAccount   string
	sessionsAllRoots  bool
	sessionsProject   string
	sessionsSince     string
	sessionsLive      bool
	sessionsJSON      bool
	sessionsLimit     int
	sessionsClaudeDir string

	sessionsPendingRehome bool
	sessionsImportsJSON   bool
	sessionsRmYes         bool
)

const (
	// sessionsDefaultSince bounds the listing to what Claude itself still
	// keeps under the default retention window.
	sessionsDefaultSince = "30d"

	// titleWidth is the widest TITLE cell of the sessions table.
	titleWidth = 40

	// shortIDLen is how much of a session id the table shows — the shortest
	// prefix `bffs sessions show` accepts.
	shortIDLen = 8

	// sessionsFooter explains the ACCOUNT column under every listing.
	sessionsFooter = "ACCOUNT is best-effort attribution (launch log + lastSessionId + import records; never a token scan); `-` = unknown."
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Browse Claude Code's sessions: which pool they live in, which account ran them",
	Long: `Claude Code keeps one transcript per conversation under projects/<slug>/ of
the config dir it runs with. Under partial isolation every bffs account shares
~/.claude/projects (one pool); a full-isolation account has its own. Transcripts
carry no account identity, so ACCOUNT is bffs's best-effort attribution from
the launch log, the lastSessionId each account's .claude.json records, and
import records — never a guess.

` + "`bffs sessions`" + ` lists the current project's sessions in the pool of the
account claude would use here; see ` + "`bffs sessions list --help`" + ` for the
filters. The interactive browser is bare ` + "`bffs`" + `.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSessionsList(cmd)
	},
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions of a project, newest first",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSessionsList(cmd)
	},
}

var sessionsShowCmd = &cobra.Command{
	Use:   "show <session-id|prefix>",
	Short: "Show one session: paths, cwd, account, liveness, import record, resume line",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSessionsShow(cmd, args[0])
	},
}

var sessionsImportsCmd = &cobra.Command{
	Use:   "imports",
	Short: "List import records: which bundles landed where, and what is still pending",
	Long: `Every ` + "`bffs import`" + ` leaves a record under <config>/imports/<bundle-id>.json:
where the bundle came from, the account it was placed under, and the status
of every session and memory directory in it. This table lists them, newest
first; ` + "`bffs sessions list --pending-rehome`" + ` lists the sessions whose directory
does not exist on this machine yet.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSessionsImports(cmd)
	},
}

var sessionsRmCmd = &cobra.Command{
	Use:   "rm <session-id|prefix>...",
	Short: "Delete sessions: transcript, sidecar, file-history, tasks and plan files (never memory, never history.jsonl)",
	Long: `Delete one or more sessions from the root they live in. Every path that
will be removed is listed first; a session a running claude has open is
refused. Memory directories and history.jsonl are never touched.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSessionsRm(cmd, args, newPrompter(os.Stdin, cmd.OutOrStdout()), sessionsRmYes, isTTY())
	},
}

func init() {
	addSessionsListFlags(sessionsCmd)
	addSessionsListFlags(sessionsListCmd)
	sessionsShowCmd.Flags().StringVar(&sessionsClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = sessionsShowCmd.Flags().MarkHidden("claude-dir")
	sessionsImportsCmd.Flags().BoolVar(&sessionsImportsJSON, "json", false, "emit the import records as a JSON array instead of the table")
	sessionsRmCmd.Flags().BoolVarP(&sessionsRmYes, "yes", "y", false, "skip the confirmation")
	sessionsRmCmd.Flags().StringVar(&sessionsClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = sessionsRmCmd.Flags().MarkHidden("claude-dir")
	sessionsCmd.AddCommand(sessionsListCmd, sessionsShowCmd, sessionsImportsCmd, sessionsRmCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// addSessionsListFlags binds the list flags to c: the group command runs
// the listing too, so both carry the same set.
func addSessionsListFlags(c *cobra.Command) {
	f := c.Flags()
	f.StringVar(&sessionsAccount, "account", "", "the root this account works in (default: the account claude would use in the project; \"home\" = ~/.claude)")
	f.BoolVar(&sessionsAllRoots, "all-roots", false, "every root: the shared pool, each full-isolation account and orphan session dirs (read-only)")
	f.StringVar(&sessionsProject, "project", "", "project directory (default: the current directory; --project \"\" = every project)")
	f.StringVar(&sessionsSince, "since", sessionsDefaultSince, "only sessions modified since: 30d, 2w, 12h (0 = no limit)")
	f.BoolVar(&sessionsLive, "live", false, "only sessions open in a running claude")
	f.BoolVar(&sessionsPendingRehome, "pending-rehome", false, "only imported sessions whose directory does not exist on this machine yet")
	f.BoolVar(&sessionsJSON, "json", false, "emit a JSON array instead of the table")
	f.IntVar(&sessionsLimit, "limit", 50, "newest N sessions per root (0 = all)")
	f.StringVar(&sessionsClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = f.MarkHidden("claude-dir")
}

// catalogEnv is what the session and memory commands need to address
// Claude's on-disk pools: the accounts, the state and every root.
type catalogEnv struct {
	cfgDir string
	accs   store.Accounts
	state  store.State
	roots  []transcripts.Root
}

// loadCatalogEnv reads accounts.toml and state.toml under cfgDir and
// enumerates the roots. claudeDir overrides ~/.claude; "" is the real one.
func loadCatalogEnv(cfgDir, claudeDir string) (*catalogEnv, error) {
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return nil, err
	}
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return nil, err
	}
	roots, err := transcripts.Roots(cfgDir, claudeDir, accs, state)
	if err != nil {
		return nil, err
	}
	return &catalogEnv{cfgDir: cfgDir, accs: accs, state: state, roots: roots}, nil
}

// configDirs lists the distinct Claude config dirs behind the roots, in
// root order — the dirs whose runtime sessions/ transcripts.Live reads.
func (e *catalogEnv) configDirs() []string {
	seen := map[string]bool{}
	var dirs []string
	for _, r := range e.roots {
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
func (e *catalogEnv) homeRoot() (transcripts.Root, error) {
	return transcripts.RootFor(e.roots, transcripts.HomeName)
}

// rootForAccount is the root a named account works in. An api_key account
// runs claude against ~/.claude, so it maps to the home root; "home" is
// the home root itself; an orphan session dir answers to its name.
func (e *catalogEnv) rootForAccount(name string) (transcripts.Root, error) {
	if acc, ok := e.accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return e.homeRoot()
	}
	if !e.knowsName(name) {
		return transcripts.Root{}, fmt.Errorf("unknown account %q; known: %v", name, e.knownNames())
	}
	return transcripts.RootFor(e.roots, name)
}

// knowsName reports whether name addresses a root: "home", an account, or
// an orphan session dir.
func (e *catalogEnv) knowsName(name string) bool {
	if name == transcripts.HomeName {
		return true
	}
	if _, ok := e.accs.Get(name); ok {
		return true
	}
	for _, r := range e.roots {
		if r.Orphan && r.Owner == name {
			return true
		}
	}
	return false
}

// knownNames lists what rootForAccount accepts, sorted.
func (e *catalogEnv) knownNames() []string {
	names := append([]string{transcripts.HomeName}, e.accs.Names()...)
	for _, r := range e.roots {
		if r.Orphan {
			names = append(names, r.Owner)
		}
	}
	sort.Strings(names)
	return names
}

// defaultRoot picks the root claude would write to when launched in dir:
// the root of the account resolver.Resolve picks there, or the home root
// when no account is configured, the account is an api_key one (it shares
// ~/.claude), or the resolution fails — the failure comes back as a
// warning, never as an error: browsing must not depend on a valid pin.
func (e *catalogEnv) defaultRoot(dir string) (root transcripts.Root, warning string, err error) {
	home, err := e.homeRoot()
	if err != nil {
		return transcripts.Root{}, "", err
	}
	r, err := resolver.Resolve(e.cfgDir, dir)
	if err != nil {
		return home, fmt.Sprintf("%v; using the home root", err), nil
	}
	if r.Source == resolver.SourceNone || r.Account.Type != store.TypeOAuth {
		return home, "", nil
	}
	root, err = transcripts.RootFor(e.roots, r.Account.Name)
	if err != nil {
		return home, fmt.Sprintf("%v; using the home root", err), nil
	}
	return root, "", nil
}

// selectRoots picks the roots a catalog command works on: every root under
// all, the named account's root, else the default for dir. Orphan roots
// are announced in warnings (they are listed read-only).
func (e *catalogEnv) selectRoots(all bool, account, dir string) (roots []transcripts.Root, warnings []string, err error) {
	switch {
	case all:
		for _, r := range e.roots {
			if r.Orphan {
				warnings = append(warnings, fmt.Sprintf("orphan session dir %s: no account in accounts.toml uses it (read-only)", short(r.ConfigDir)))
			}
		}
		return e.roots, warnings, nil
	case account != "":
		root, err := e.rootForAccount(account)
		if err != nil {
			return nil, nil, err
		}
		return []transcripts.Root{root}, nil, nil
	default:
		root, warning, err := e.defaultRoot(dir)
		if err != nil {
			return nil, nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
		return []transcripts.Root{root}, warnings, nil
	}
}

// sessionsQuery is one resolved `sessions list` request.
type sessionsQuery struct {
	Roots      []transcripts.Root
	Project    string   // normalised project directory; "" = every project
	Env        []string // the environment claude launches with (ProjectDirFor)
	Since      time.Duration
	Live       bool
	Pending    bool // only imported sessions whose cwd is missing here (--pending-rehome)
	Limit      int
	Now        time.Time
	Attributor transcripts.Attributor
	Imports    map[string]imports.SessionRef
	LiveMap    map[string]transcripts.LiveSession
}

// sessionBlock is the listing of one root: its sessions (newest first) and
// whether the project directory asked for exists in it.
type sessionBlock struct {
	Root       transcripts.Root
	Project    string // the requested project directory; "" = every project
	ProjectDir string // the projects/<slug> directory that was listed
	DirExists  bool
	Sessions   []transcripts.Session
}

func runSessionsList(cmd *cobra.Command) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, sessionsClaudeDir)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	since, err := parseSince(sessionsSince)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	project := ""
	if !(cmd.Flags().Changed("project") && sessionsProject == "") {
		project, err = targetDir([]string{sessionsProject}, 0)
		if err != nil {
			return err
		}
	}
	resolveDir := project
	if resolveDir == "" {
		resolveDir = cwd
	}
	roots, warnings, err := env.selectRoots(sessionsAllRoots, sessionsAccount, resolveDir)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}

	ctx := cmdContext(cmd)
	q := sessionsQuery{
		Roots:   roots,
		Project: project,
		Env:     os.Environ(),
		Since:   since,
		Live:    sessionsLive,
		Pending: sessionsPendingRehome,
		Limit:   sessionsLimit,
		Now:     time.Now(),
	}
	// Attribution, import records and liveness are enrichment: a problem
	// with any of them is a warning, and the listing still comes out.
	if attr, err := usage.NewAttributor(dir, env.accs); err != nil {
		fmt.Fprintln(errOut, "warning: attribution unavailable:", err)
	} else {
		q.Attributor = attr
	}
	if recs, err := imports.Load(dir); err != nil {
		fmt.Fprintln(errOut, "warning: import records unavailable:", err)
	} else {
		q.Imports = imports.BySession(recs)
	}
	if live, err := transcripts.Live(ctx, env.configDirs()); err != nil {
		fmt.Fprintln(errOut, "warning: liveness unavailable:", err)
	} else {
		q.LiveMap = live
	}

	blocks, err := listSessionBlocks(ctx, q)
	if err != nil {
		return err
	}
	if sessionsJSON {
		return writeSessionsJSON(out, blocks)
	}
	return renderSessionsTable(out, blocks, since, q.Now)
}

// listSessionBlocks lists q.Roots one by one. With a project, each root's
// projects/<slug> directory is located the way Claude does (ProjectDirFor)
// and only that slug is listed; a root that has no directory for it yields
// an empty block. --live runs the fast path first and reads titles only
// for the sessions that are live. --pending-rehome keeps only imported
// sessions whose directory is missing here, applying the limit after the
// filter.
func listSessionBlocks(ctx context.Context, q sessionsQuery) ([]sessionBlock, error) {
	var blocks []sessionBlock
	for _, root := range q.Roots {
		b := sessionBlock{Root: root, Project: q.Project}
		opts := transcripts.ListOptions{
			Titles:     true,
			Attributor: q.Attributor,
			Live:       q.LiveMap,
			Imports:    q.Imports,
			Limit:      q.Limit,
			Now:        q.Now,
		}
		if q.Since > 0 {
			opts.Since = q.Now.Add(-q.Since)
		}
		if q.Pending {
			opts.Limit = 0
		}
		if q.Project != "" {
			pd, err := transcripts.ProjectDirFor(root, q.Project, q.Env)
			if err != nil {
				if errors.Is(err, transcripts.ErrSlugTooLong) {
					return nil, fmt.Errorf("project %s: %w, and no existing project dir under %s records it", short(q.Project), err, short(root.Dir))
				}
				return nil, err
			}
			b.ProjectDir = pd
			if info, err := os.Stat(pd); err != nil || !info.IsDir() {
				blocks = append(blocks, b)
				continue
			}
			b.DirExists = true
			opts.Slug = filepath.Base(pd)
		}
		if q.Live {
			fast := opts
			fast.Titles, fast.Attributor, fast.Limit = false, nil, 0
			all, err := transcripts.List(ctx, root, fast)
			if err != nil {
				return nil, err
			}
			for _, s := range all {
				if s.Live {
					opts.IDs = append(opts.IDs, s.ID)
				}
			}
			if len(opts.IDs) == 0 {
				blocks = append(blocks, b)
				continue
			}
		}
		ss, err := transcripts.List(ctx, root, opts)
		if err != nil {
			return nil, err
		}
		if q.Pending {
			ss = pendingRehome(ss, q.Limit)
		}
		b.Sessions = ss
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// pendingRehome keeps the sessions an import record knows whose directory
// does not exist on this machine — the ones `bffs rehome` is for — at
// most limit of them (0 = all).
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

// sessionGroup is one project's rows inside a block: sessions of one
// projects/<slug> directory, headed by the directory they belong to.
type sessionGroup struct {
	Project  string // the directory to print: the request, else the newest session's cwd, else the slug
	Sessions []transcripts.Session
}

// groupSessions splits a block into per-project groups, ordered by each
// group's newest session (the block is newest first already). A block
// listed for one project is a single group.
func groupSessions(b sessionBlock) []sessionGroup {
	if b.Project != "" {
		return []sessionGroup{{Project: short(b.Project), Sessions: b.Sessions}}
	}
	index := map[string]int{}
	var groups []sessionGroup
	for _, s := range b.Sessions {
		i, ok := index[s.Slug]
		if !ok {
			label := s.Slug
			if s.Cwd != "" {
				label = short(s.Cwd)
			}
			index[s.Slug] = len(groups)
			groups = append(groups, sessionGroup{Project: label})
			i = len(groups) - 1
		}
		groups[i].Sessions = append(groups[i].Sessions, s)
	}
	return groups
}

// renderSessionsTable prints one table per project, each headed by the
// project directory and the root it was found in, then the count and the
// attribution legend. Every transcript-derived string is sanitised.
func renderSessionsTable(w io.Writer, blocks []sessionBlock, since time.Duration, now time.Time) error {
	total := 0
	first := true
	for _, b := range blocks {
		for _, g := range groupSessions(b) {
			if len(g.Sessions) == 0 {
				continue
			}
			if !first {
				fmt.Fprintln(w)
			}
			first = false
			fmt.Fprintf(w, "project %s  (%s)\n", transcripts.Sanitize(g.Project), rootLabel(b.Root))
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTITLE\tACCOUNT\tLAST\tSIZE\tBRANCH\tSTATE")
			for _, s := range g.Sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					shortID(s.ID),
					dashIfEmpty(truncateCell(transcripts.Sanitize(s.Title), titleWidth)),
					dashIfEmpty(transcripts.Sanitize(s.Account)),
					humanizeAgo(s.LastTS, now),
					formatSize(s.Size),
					dashIfEmpty(transcripts.Sanitize(s.GitBranch)),
					sessionState(s))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			total += len(g.Sessions)
		}
	}
	if total == 0 {
		fmt.Fprintln(w, noSessionsLine(blocks, since))
		return nil
	}
	fmt.Fprintf(w, "%s. %s\n", countNoun(total, "session"), sessionsFooter)
	return nil
}

// noSessionsLine phrases an empty listing: which project, and whether the
// --since window may be hiding older sessions.
func noSessionsLine(blocks []sessionBlock, since time.Duration) string {
	project, exists := "", false
	for _, b := range blocks {
		if b.Project != "" {
			project = b.Project
		}
		exists = exists || b.DirExists
	}
	line := "no sessions"
	if project != "" {
		line += " for " + short(project)
	}
	if exists && since > 0 {
		line += fmt.Sprintf(" modified in the last %s (--since 0 lists all)", formatSince(since))
	}
	return line
}

// rootLabel names a root the way the table header does: the accounts
// sharing a pool, the owner of a full-isolation root, an orphan dir, or
// the unmanaged home dir.
func rootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan: %s, read-only", r.Owner)
	case r.Owner != "":
		return "account: " + r.Owner
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool: " + strings.Join(r.Accounts, ", ")
	default:
		return "home: " + short(r.ConfigDir)
	}
}

// sessionState is the STATE cell: live, imported·pending (an import whose
// directory does not exist here), imported, or nothing.
func sessionState(s transcripts.Session) string {
	switch {
	case s.Live:
		return "live"
	case s.Import != nil && !s.CwdExists:
		return "imported·pending"
	case s.Import != nil:
		return "imported"
	default:
		return ""
	}
}

func shortID(sid string) string {
	if len(sid) > shortIDLen {
		return sid[:shortIDLen]
	}
	return sid
}

// truncateCell cuts s to width runes, ending in an ellipsis when it had to.
func truncateCell(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:width-1]), " ") + "…"
}

// formatSize renders bytes for a table column: 12 KB, 0.4 MB, 42.6 MB,
// 1.3 GB (decimal units, as Finder shows them).
func formatSize(n int64) string {
	switch {
	case n < 100_000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
}

// countNoun renders "1 session" / "2 sessions" / "3 entries".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	if strings.HasSuffix(noun, "y") {
		noun = noun[:len(noun)-1] + "ie"
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// parseSince reads a --since value: a Go duration (12h, 90m) or a count of
// days or weeks (30d, 2w); "0", "" and "all" mean no bound.
func parseSince(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "0", "all":
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		return unitDuration(s, n, 24*time.Hour)
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		return unitDuration(s, n, 7*24*time.Hour)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid --since %q: use a duration like 30d, 2w or 12h (0 = no limit)", s)
	}
	return d, nil
}

func unitDuration(whole, count string, unit time.Duration) (time.Duration, error) {
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --since %q: use a duration like 30d, 2w or 12h (0 = no limit)", whole)
	}
	return time.Duration(n) * unit, nil
}

// formatSince renders a --since bound the way it was typed: whole days as
// Nd, otherwise the duration.
func formatSince(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	return d.String()
}

// cmdContext is the command's context, or a background one when the
// command was not executed through the cobra tree (tests).
func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// sessionInfo is the --json row: the SessionInfo shape of the list_sessions
// MCP tool (plan §10.3). Field names are snake_case and stable.
type sessionInfo struct {
	SessionID     string `json:"session_id"`
	Root          string `json:"root"`
	Account       string `json:"account"`
	AccountSource string `json:"account_source"`
	Cwd           string `json:"cwd"`
	CwdExists     bool   `json:"cwd_exists"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	TitleSource   string `json:"title_source"`
	GitBranch     string `json:"git_branch"`
	ClaudeVersion string `json:"claude_version"`
	FirstAt       string `json:"first_at"`
	LastAt        string `json:"last_at"`
	SizeBytes     int64  `json:"size_bytes"`
	Subagents     int    `json:"subagents"`
	Live          bool   `json:"live"`
	BundleID      string `json:"bundle_id"`
	OldCwd        string `json:"old_cwd"`
	OldHome       string `json:"old_home"`
	OldHost       string `json:"old_host"`
	GitRemote     string `json:"git_remote"`
}

// sessionInfoOf converts a catalog session for --json. Root is the
// projects/ directory the transcript was found in.
func sessionInfoOf(s transcripts.Session) sessionInfo {
	info := sessionInfo{
		SessionID:     s.ID,
		Root:          s.Root.Dir,
		Account:       s.Account,
		AccountSource: s.AttribSource,
		Cwd:           transcripts.Sanitize(s.Cwd),
		CwdExists:     s.CwdExists,
		Slug:          s.Slug,
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
		info.BundleID = s.Import.Record.BundleID
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

// writeSessionsJSON emits every block's sessions as one array (never null).
func writeSessionsJSON(w io.Writer, blocks []sessionBlock) error {
	infos := make([]sessionInfo, 0)
	for _, b := range blocks {
		for _, s := range b.Sessions {
			infos = append(infos, sessionInfoOf(s))
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(infos)
}

// sessionDetail is everything `sessions show` prints for one transcript.
type sessionDetail struct {
	Session          transcripts.Session
	Artifacts        transcripts.Artifacts
	LivePID          int
	SidecarSize      int64
	SidecarExists    bool
	FileHistoryExist bool
	TasksExist       bool
	Claimants        []string // accounts (or "home") whose .claude.json lastSessionId is this session
}

func runSessionsShow(cmd *cobra.Command, idOrPrefix string) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, sessionsClaudeDir)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	found, err := transcripts.Find(env.roots, idOrPrefix)
	if err != nil {
		return err
	}
	ctx := cmdContext(cmd)
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		fmt.Fprintln(errOut, "warning: liveness unavailable:", err)
	}
	attr, err := usage.NewAttributor(dir, env.accs)
	if err != nil {
		fmt.Fprintln(errOut, "warning: attribution unavailable:", err)
	}
	var bySession map[string]imports.SessionRef
	if recs, err := imports.Load(dir); err != nil {
		fmt.Fprintln(errOut, "warning: import records unavailable:", err)
	} else {
		bySession = imports.BySession(recs)
	}
	homeClaimed := false
	if home, err := env.homeRoot(); err == nil {
		if ids, err := claudejson.LastSessionIDs(home.ClaudeJSON); err == nil {
			_, homeClaimed = ids[found[0].ID]
		}
	}

	now := time.Now()
	for i, s := range found {
		if i > 0 {
			fmt.Fprintln(out)
		}
		d := sessionDetail{Session: s}
		if ls, ok := live[s.ID]; ok {
			d.Session.Live = true
			d.LivePID = ls.PID
		}
		if attr != nil {
			d.Session.Account, d.Session.AttribSource = attr.Attribute(s.ID, s.Cwd, s.FirstTS, s.Root.Owner)
			d.Claimants = attr.Claimants(s.ID)
		}
		if homeClaimed {
			d.Claimants = append(d.Claimants, transcripts.HomeName)
		}
		if ref, ok := bySession[s.ID]; ok {
			ref := ref
			d.Session.Import = &ref
		}
		d.Artifacts = transcripts.ArtifactsFor(s.Root, s)
		d.SidecarSize, d.SidecarExists = dirSize(d.Artifacts.SidecarDir)
		d.FileHistoryExist = isDir(d.Artifacts.FileHistoryDir)
		d.TasksExist = isDir(d.Artifacts.TasksDir)
		renderSessionShow(out, d, now)
	}
	return nil
}

// renderSessionShow prints the key/value screen of one session, ending
// with the command that resumes it.
func renderSessionShow(w io.Writer, d sessionDetail, now time.Time) {
	s := d.Session
	// Every value passes through Sanitize: cwd, branch, title, plan slug and
	// the import record are transcript- or peer-derived.
	kv := func(label, value string) { fmt.Fprintf(w, "%-14s%s\n", label+":", transcripts.Sanitize(value)) }
	kv("session", s.ID)
	title := "-"
	if t := transcripts.Sanitize(s.Title); t != "" {
		title = fmt.Sprintf("%s  (%s)", t, s.TitleSource)
	}
	kv("title", title)
	kv("root", fmt.Sprintf("%s  (%s)", short(s.Root.Dir), rootLabel(s.Root)))
	kv("project dir", short(filepath.Dir(s.Path)))
	switch {
	case s.Cwd == "":
		kv("cwd", "-  (no cwd recorded)")
	case s.CwdExists:
		kv("cwd", short(s.Cwd)+"  (exists)")
	default:
		kv("cwd", short(s.Cwd)+"  (missing on this machine)")
	}
	if s.Relocated {
		kv("head cwd", short(s.HeadCwd)+"  (relocated since)")
	}
	if s.Account != "" {
		kv("account", fmt.Sprintf("%s  (%s)", transcripts.Sanitize(s.Account), s.AttribSource))
	} else {
		kv("account", "-  (unknown: no launch-log, lastSessionId or import evidence)")
	}
	state := sessionState(s)
	switch {
	case s.Live && d.LivePID > 0:
		state = fmt.Sprintf("live (pid %d)", d.LivePID)
	case state == "":
		state = "idle"
	}
	kv("state", state)
	if len(d.Claimants) > 0 {
		fmt.Fprintf(w, "last-session pointer: %s (claude-recorded)\n", strings.Join(d.Claimants, ", "))
	}
	if !s.FirstTS.IsZero() {
		kv("first", fmt.Sprintf("%s  (%s)", s.FirstTS.Local().Format("2006-01-02 15:04"), humanizeAgo(s.FirstTS, now)))
	}
	kv("last", fmt.Sprintf("%s  (%s)", s.LastTS.Local().Format("2006-01-02 15:04"), humanizeAgo(s.LastTS, now)))
	kv("size", formatSize(s.Size))
	kv("branch", dashIfEmpty(transcripts.Sanitize(s.GitBranch)))
	kv("version", dashIfEmpty(transcripts.Sanitize(s.Version)))
	kv("transcript", short(d.Artifacts.Transcript))
	sidecar := short(d.Artifacts.SidecarDir) + "  (absent)"
	if d.SidecarExists {
		sidecar = fmt.Sprintf("%s  (%s, %s)", short(d.Artifacts.SidecarDir), formatSize(d.SidecarSize), countNoun(s.Subagents, "subagent"))
	}
	kv("sidecar", sidecar)
	kv("file-history", short(d.Artifacts.FileHistoryDir)+presence(d.FileHistoryExist))
	kv("tasks", short(d.Artifacts.TasksDir)+presence(d.TasksExist))
	if len(d.Artifacts.PlanFiles) == 0 {
		kv("plan", "-")
	}
	for _, p := range d.Artifacts.PlanFiles {
		kv("plan", short(p))
	}
	if s.Import != nil && s.Import.Record != nil {
		r := s.Import.Record
		src := transcripts.Sanitize(r.Source.Hostname)
		if r.Source.User != "" || r.Source.Home != "" {
			src += fmt.Sprintf(" (%s, %s)", transcripts.Sanitize(r.Source.User), transcripts.Sanitize(r.Source.Home))
		}
		acct := r.Account
		if acct == "" {
			acct = transcripts.HomeName
		}
		kind := r.Kind
		if !r.ImportedAt.IsZero() {
			kind += " " + r.ImportedAt.Local().Format("2006-01-02")
		}
		line := fmt.Sprintf("bundle %s  (%s) from %s, account %s", transcripts.Sanitize(r.BundleID), kind, src, transcripts.Sanitize(acct))
		if s.Import.Session != nil {
			line += ", status " + transcripts.Sanitize(s.Import.Session.Status)
		}
		kv("import", line)
		if s.Import.Session != nil && s.Import.Session.OldCwd != "" {
			kv("old cwd", transcripts.Sanitize(s.Import.Session.OldCwd))
		}
	}
	kv("resume", resumeLine(s))
}

// resumeLine is the copy-pasteable command that reopens s: on a
// full-isolation root prefixed with BFFS_ACCOUNT so the shim picks the
// owning account; on an orphan root with the config dir itself, since no
// account can launch it.
func resumeLine(s transcripts.Session) string {
	var sb strings.Builder
	// Sanitise before quoting so a control character in a transcript's
	// cwd neither reaches the terminal nor forces quotes.
	if cwd := transcripts.Sanitize(s.Cwd); cwd != "" {
		sb.WriteString("cd " + shellWord(cwd) + " && ")
	}
	// The assignment sits on the claude word: a leading `A=1 cd X && claude`
	// would bind the variable to cd only.
	switch {
	case s.Root.Orphan:
		sb.WriteString("CLAUDE_CONFIG_DIR=" + shellWord(s.Root.ConfigDir) + " ")
	case s.Root.Owner != "":
		sb.WriteString("BFFS_ACCOUNT=" + shellWord(s.Root.Owner) + " ")
	}
	sb.WriteString("claude --resume " + s.ID)
	return sb.String()
}

// runSessionsRm deletes the named sessions after listing every path.
func runSessionsRm(cmd *cobra.Command, args []string, pr *prompter, yes, tty bool) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, sessionsClaudeDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	ctx := cmdContext(cmd)
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		return fmt.Errorf("cannot tell which sessions are open in a running claude: %w; nothing was removed", err)
	}
	var targets []rmTarget
	seen := map[string]bool{}
	for _, arg := range args {
		found, err := transcripts.Find(env.roots, arg)
		if err != nil {
			return err
		}
		if len(found) != 1 {
			return fmt.Errorf("session %q matches %d transcripts; use the full id", arg, len(found))
		}
		s := found[0]
		if seen[s.Path] {
			continue
		}
		seen[s.Path] = true
		if s.Root.Orphan {
			return fmt.Errorf("session %s lives in the orphan session dir %s, which is read-only", short8(s.ID), short(s.Root.ConfigDir))
		}
		if ls, ok := live[s.ID]; ok {
			return fmt.Errorf("session %s is open in a running claude (pid %d); close it first", s.ID, ls.PID)
		}
		t, err := newRmTarget(s)
		if err != nil {
			return err
		}
		targets = append(targets, t)
	}
	var total int64
	for _, t := range targets {
		fmt.Fprintf(out, "%s  %s\n", t.session.ID, transcripts.Sanitize(t.title))
		for _, p := range t.paths {
			fmt.Fprintf(out, "  %-8s %s\n", formatSize(p.size), short(p.path))
			total += p.size
		}
	}
	fmt.Fprintf(out, "remove %s (%s)? [y/N] ", countNoun(len(targets), "session"), formatSize(total))
	if !yes {
		if !tty {
			return errors.New("stdin is not a terminal; pass -y to confirm")
		}
		ans, err := pr.line("")
		if err != nil {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "aborted")
			return nil
		}
		if len(targets) > 1 {
			if err := confirmCount(pr, len(targets)); err != nil {
				return err
			}
		}
	} else {
		fmt.Fprintln(out, "y")
	}
	removed := 0
	for _, t := range targets {
		if err := t.remove(); err != nil {
			return exitWith(1, fmt.Errorf("removed %d of %d; %w", removed, len(targets), err))
		}
		removed++
	}
	fmt.Fprintf(out, "removed %s (%s)\n", countNoun(removed, "session"), formatSize(total))
	return nil
}

// rmTarget is one session and the paths its removal covers, all relative
// to the config dir so the deletion runs through an os.Root over it.
type rmTarget struct {
	session   transcripts.Session
	title     string
	configDir string
	paths     []rmPath
}

type rmPath struct {
	path string // absolute, for display
	rel  string // relative to configDir
	size int64
	dir  bool
}

func newRmTarget(s transcripts.Session) (rmTarget, error) {
	cfg, err := filepath.EvalSymlinks(s.Root.ConfigDir)
	if err != nil {
		return rmTarget{}, err
	}
	t := rmTarget{session: s, title: s.Title, configDir: cfg}
	art := transcripts.ArtifactsFor(s.Root, s)
	candidates := append([]string{art.Transcript, art.SidecarDir, art.FileHistoryDir, art.TasksDir}, art.PlanFiles...)
	for _, p := range candidates {
		if p == "" {
			continue
		}
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return rmTarget{}, err
		}
		real = filepath.Join(real, filepath.Base(p))
		rel, err := filepath.Rel(cfg, real)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return rmTarget{}, fmt.Errorf("%s is outside the config dir %s; refusing", short(p), short(cfg))
		}
		size := info.Size()
		if info.IsDir() {
			size = rmDirSize(p)
		}
		t.paths = append(t.paths, rmPath{path: p, rel: rel, size: size, dir: info.IsDir()})
	}
	if len(t.paths) == 0 {
		return rmTarget{}, fmt.Errorf("session %s has no files under %s", short8(s.ID), short(cfg))
	}
	return t, nil
}

// remove deletes the target's paths through an os.Root over the config
// dir: the transcript last, so a partial failure never leaves a transcript
// without the files Claude expects beside it.
func (t rmTarget) remove() error {
	root, err := os.OpenRoot(t.configDir)
	if err != nil {
		return err
	}
	defer root.Close()
	var transcript *rmPath
	for i := range t.paths {
		p := &t.paths[i]
		if !p.dir && strings.HasSuffix(p.rel, transcripts.TranscriptExt) && filepath.Base(p.rel) == t.session.ID+transcripts.TranscriptExt {
			transcript = p
			continue
		}
		if err := root.RemoveAll(p.rel); err != nil {
			return fmt.Errorf("remove %s: %w", short(p.path), err)
		}
	}
	if transcript != nil {
		if err := root.Remove(transcript.rel); err != nil {
			return fmt.Errorf("remove %s: %w", short(transcript.path), err)
		}
	}
	return nil
}

// rmDirSize sums the regular files under dir (best effort, for display).
func rmDirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// shellWord quotes s for a POSIX shell when it needs it.
func shellWord(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-./~:@+,", c)) {
			return shellSingleQuote(s)
		}
	}
	if s == "" {
		return "''"
	}
	return s
}

func presence(exists bool) string {
	if exists {
		return ""
	}
	return "  (absent)"
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// dirSize sums the regular files under dir; ok is false when it does not
// exist.
func dirSize(dir string) (total int64, ok bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total, true
}

func runSessionsImports(cmd *cobra.Command) error {
	dir := mustConfigDir(cmd)
	recs, skipped, err := imports.LoadAll(dir)
	if err != nil {
		return err
	}
	for _, sk := range skipped {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: import record %s skipped: %v\n", short(sk.Path), sk.Err)
	}
	if sessionsImportsJSON {
		return writeImportsJSON(cmd.OutOrStdout(), recs)
	}
	return renderImportsTable(cmd.OutOrStdout(), dir, recs, time.Now())
}

// importsTally counts a record's rows by status: sessions that landed
// (placed, rehomed or pending), the pending subset, sessions skipped, and
// memory directories written.
type importsTally struct {
	Landed, Pending, Skipped, Memory int
}

func tallyImport(r imports.Record) importsTally {
	var t importsTally
	for _, s := range r.Sessions {
		switch s.Status {
		case imports.StatusSkipped:
			t.Skipped++
		case imports.StatusPending:
			t.Landed++
			t.Pending++
		default:
			t.Landed++
		}
	}
	for _, m := range r.Memories {
		if m.Status != imports.StatusSkipped {
			t.Memory++
		}
	}
	return t
}

// renderImportsTable prints one row per import record, newest first.
// Every source-derived string passes through Sanitize.
func renderImportsTable(w io.Writer, cfgDir string, recs []imports.Record, now time.Time) error {
	if len(recs) == 0 {
		fmt.Fprintln(w, "no imports recorded (bffs import --from <file.bffs>)")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUNDLE\tFROM\tACCOUNT\tIMPORTED\tSESSIONS\tPENDING\tMEMORY")
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		t := tallyImport(r)
		from := transcripts.Sanitize(r.Source.Hostname)
		if u := transcripts.Sanitize(r.Source.User); u != "" {
			from += " (" + u + ")"
		}
		account := r.Account
		if account == "" {
			account = transcripts.HomeName
		}
		sessionsCell := strconv.Itoa(t.Landed)
		if t.Skipped > 0 {
			sessionsCell += fmt.Sprintf(" (+%d skipped)", t.Skipped)
		}
		imported := "-"
		if !r.ImportedAt.IsZero() {
			imported = fmt.Sprintf("%s (%s)", r.ImportedAt.Local().Format("2006-01-02"), humanizeAgo(r.ImportedAt, now))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
			shortID(transcripts.Sanitize(r.BundleID)), dashIfEmpty(from), transcripts.Sanitize(account), imported, sessionsCell, t.Pending, t.Memory)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s under %s. PENDING sessions wait for their directory: bffs sessions list --pending-rehome.\n", countNoun(len(recs), "import"), short(filepath.Join(cfgDir, imports.Subdir)))
	return nil
}

// writeImportsJSON emits the records as one array (never null), with the
// snake_case fields of the files themselves.
func writeImportsJSON(w io.Writer, recs []imports.Record) error {
	if recs == nil {
		recs = []imports.Record{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(recs)
}
