package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

var (
	trustProject     string
	trustAllProjects bool
	trustJSON        bool
	trustClaudeDir   string

	trustSyncFrom               string
	trustSyncTo                 string
	trustSyncProject            string
	trustSyncAllProjects        bool
	trustSyncIncludePermissions bool
	trustSyncMirror             bool
	trustSyncDryRun             bool
	trustSyncYes                bool
)

// trustSyncAll is the --to value that fans a sync out to every row.
const trustSyncAll = "all"

// liveNote is the tail of every `live:` line: why the line is there.
const liveNote = "a running claude picks changes up within about a second"

var trustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Show which accounts have answered Claude Code's trust dialogs for a project",
	Long: `Claude Code asks two questions the first time it runs in a directory — folder
trust and "Allow external CLAUDE.md file imports?" — and records the answers
per project in the .claude.json it reads from CLAUDE_CONFIG_DIR. Every bffs
oauth account has its own .claude.json, so the answers diverge per account
and the dialogs come back after ` + "`bffs switch`" + `.

This matrix shows, per account and for ~/.claude.json ("home" — what an
unmanaged claude and api_key accounts read), what has been answered for the
current project (--project <dir>, or --all-projects for every project the
active account's file knows). Folder trust is inherited from a trusted parent
directory, bounded by the git root inside a repository; the external-imports
answer is exact-key only. Nothing is written here — carry answers over with
` + "`bffs trust sync`" + `.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		if trustProject != "" && trustAllProjects {
			return errors.New("--project and --all-projects are mutually exclusive")
		}
		env, err := loadTrustEnv(dir, trustClaudeDir)
		if err != nil {
			return err
		}
		keys, skipped, err := trustProjectKeys(env, "", trustProject, trustAllProjects)
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		live, err := trust.LiveLines(ctx, env.files)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not list running claude sessions: %v\n", err)
		}
		blocks := make([]trustBlock, 0, len(keys))
		for _, key := range keys {
			b, err := trustBlockFor(env, key, live)
			if err != nil {
				return err
			}
			blocks = append(blocks, b)
		}
		out := cmd.OutOrStdout()
		if trustJSON {
			for _, s := range skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped (directory missing): %s\n", transcripts.Sanitize(s))
			}
			return renderTrustJSON(out, blocks)
		}
		for i, b := range blocks {
			if i > 0 {
				fmt.Fprintln(out)
			}
			renderTrustMatrix(out, b, env.activeRow)
		}
		for _, s := range skipped {
			fmt.Fprintf(out, "skipped (directory missing): %s\n", transcripts.Sanitize(s))
		}
		return nil
	},
}

var trustSyncCmd = &cobra.Command{
	Use:   "sync --to <account|home|all> [--from <account|home>]",
	Short: "Carry dialog answers from one account's .claude.json to another's",
	Long: `Copies the folder-trust and external-imports answers recorded for a project
from one .claude.json to another under three rules: never downgrade (only
false/absent becomes true), never override an explicit decline (a declined
answer lands only where nothing was answered), and never touch anything but
those entries — lastSessionId, metrics and identity fields stay as they are.
--mirror copies the source values verbatim instead, downgrades included.

The source defaults to the active account when it accepted the project's
folder trust, else the first account (by name) that did, else ~/.claude.json
("home"); an account that only inherits trust from a parent directory comes
after those, since its answer is not one that can be copied. --from
overrides. --to all fans the source out to every managed account and home.
Permission grants (allowedTools and MCP server approvals) travel only with
--include-permissions; every mcpServers command line is printed first.

The write takes Claude's own lock on the file and holds it well under a
second; a claude already running on that account picks the change up within
about a second.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		if trustSyncTo == "" {
			return errors.New(`--to is required: an account name, "home", or "all"`)
		}
		if trustSyncProject != "" && trustSyncAllProjects {
			return errors.New("--project and --all-projects are mutually exclusive")
		}
		env, err := loadTrustEnv(dir, trustClaudeDir)
		if err != nil {
			return err
		}
		if trustSyncFrom != "" {
			if err := trustRowExists(env, "--from", trustSyncFrom); err != nil {
				return err
			}
		}
		targets, err := trustSyncTargets(env, trustSyncFrom, trustSyncTo)
		if err != nil {
			return err
		}
		keys, skipped, err := trustProjectKeys(env, trustSyncFrom, trustSyncProject, trustSyncAllProjects)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, s := range skipped {
			fmt.Fprintf(out, "skipped (directory missing): %s\n", transcripts.Sanitize(s))
		}
		req := syncRequest{
			From:               trustSyncFrom,
			To:                 targets,
			Keys:               keys,
			IncludePermissions: trustSyncIncludePermissions,
			Mirror:             trustSyncMirror,
			DryRun:             trustSyncDryRun,
			Yes:                trustSyncYes,
		}
		pr := newPrompter(cmd.InOrStdin(), out)
		return runTrustSync(cmd, pr, env, req, isTTY())
	},
}

func init() {
	trustCmd.Flags().StringVar(&trustProject, "project", "", "project directory (default: the current directory)")
	trustCmd.Flags().BoolVar(&trustAllProjects, "all-projects", false, "every project the active account's .claude.json knows whose directory exists")
	trustCmd.Flags().BoolVar(&trustJSON, "json", false, "machine-readable output")
	trustCmd.PersistentFlags().StringVar(&trustClaudeDir, "claude-dir", "", "override the shared claude config dir; its sibling .claude.json is the home file (testing)")
	_ = trustCmd.PersistentFlags().MarkHidden("claude-dir")

	trustSyncCmd.Flags().StringVar(&trustSyncFrom, "from", "", `source account, or "home" (default: the active account when it accepted the project, else the first that did, else home)`)
	trustSyncCmd.Flags().StringVar(&trustSyncTo, "to", "", `target account, "home", or "all"`)
	trustSyncCmd.Flags().StringVar(&trustSyncProject, "project", "", "project directory (default: the current directory)")
	trustSyncCmd.Flags().BoolVar(&trustSyncAllProjects, "all-projects", false, "every project the source's .claude.json knows whose directory exists")
	trustSyncCmd.Flags().BoolVar(&trustSyncIncludePermissions, "include-permissions", false, "also copy allowedTools and MCP server approvals (printed before applying)")
	trustSyncCmd.Flags().BoolVar(&trustSyncMirror, "mirror", false, "copy the source values verbatim, downgrades included")
	trustSyncCmd.Flags().BoolVar(&trustSyncDryRun, "dry-run", false, "show the plan and write nothing")
	trustSyncCmd.Flags().BoolVarP(&trustSyncYes, "yes", "y", false, "skip confirmation")

	trustCmd.AddCommand(trustSyncCmd)
	rootCmd.AddCommand(trustCmd)
}

// trustEnv is what every trust surface needs: the .claude.json files a sync
// can address (accounts by name plus trust.HomeName), the accounts behind
// them, and the row the active account reads.
type trustEnv struct {
	cfgDir    string
	homeJSON  string
	accs      store.Accounts
	state     store.State
	files     map[string]string
	activeRow string // the files row the active account reads; "" when none
}

// loadTrustEnv reads accounts.toml and state.toml under cfgDir and lists the
// addressable .claude.json files. claudeDir overrides ~/.claude (its sibling
// .claude.json becomes the home file); "" is the real one.
func loadTrustEnv(cfgDir, claudeDir string) (*trustEnv, error) {
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return nil, err
	}
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return nil, err
	}
	homeJSON := ""
	if claudeDir != "" {
		homeJSON = filepath.Join(filepath.Dir(claudeDir), claudejson.Filename)
	}
	files, err := trust.Files(cfgDir, homeJSON, accs)
	if err != nil {
		return nil, err
	}
	return &trustEnv{
		cfgDir:    cfgDir,
		homeJSON:  files[trust.HomeName],
		accs:      accs,
		state:     state,
		files:     files,
		activeRow: trustRowFor(files, accs, state.Active),
	}, nil
}

// trustRowFor maps an account name to the row of files it reads: its own
// entry for an oauth account (or "home" itself), trust.HomeName for an
// api_key account — they share ~/.claude.json — and "" for anything else.
func trustRowFor(files map[string]string, accs store.Accounts, name string) string {
	if _, ok := files[name]; ok {
		return name
	}
	if acc, ok := accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return trust.HomeName
	}
	return ""
}

// trustRowExists validates a --from/--to name against the addressable
// files, phrasing the api_key refusal for the flag in question.
func trustRowExists(env *trustEnv, flag, name string) error {
	if _, ok := env.files[name]; ok {
		return nil
	}
	if acc, ok := env.accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return fmt.Errorf("%s: account %q is an api_key account; it shares ~/.claude.json with unmanaged claude — use %s %s", flag, name, flag, trust.HomeName)
	}
	_, err := trust.JSONPathFor(env.files, env.accs, name)
	return fmt.Errorf("%s: %w", flag, err)
}

// trustRowOrder lists the rows of files the way the matrix prints them:
// accounts by name, home last.
func trustRowOrder(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		if n != trust.HomeName {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if _, ok := files[trust.HomeName]; ok {
		names = append(names, trust.HomeName)
	}
	return names
}

// projectKeyFor resolves dir ("" = the current directory) to the key Claude
// files it under in .claude.json's projects map: the canonical git root
// inside a repository, else the symlink-resolved directory.
func projectKeyFor(dir string) (string, error) {
	var err error
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	norm, err := store.NormalizePath(dir)
	if err != nil {
		return "", err
	}
	return transcripts.ProjectKey(norm)
}

// gitRootOf is the repository root that bounds folder-trust inheritance for
// a project key: "" outside a repository.
func gitRootOf(key string) string {
	if root, ok := transcripts.GitRoot(key); ok {
		return root
	}
	return ""
}

// trustProjectKeys lists the project keys a trust command works on: the one
// project at dir ("" = cwd), or under allProjects every key in the
// key-source file — from when given, else the active account's file, else
// the home file — whose directory still exists. Keys whose directory is
// gone come back in skipped.
func trustProjectKeys(env *trustEnv, from, dir string, allProjects bool) (keys, skipped []string, err error) {
	if !allProjects {
		key, err := projectKeyFor(dir)
		if err != nil {
			return nil, nil, err
		}
		return []string{key}, nil, nil
	}
	src := from
	if src == "" {
		src = env.activeRow
	}
	if _, ok := env.files[src]; !ok {
		src = trust.HomeName
	}
	return projectKeysIn(env.files[src])
}

// projectKeysIn returns the project keys of the .claude.json at path whose
// directory exists, sorted, and separately the keys whose directory is gone.
func projectKeysIn(path string) (keys, missing []string, err error) {
	flags, err := claudejson.ReadProjectFlags(path)
	if err != nil {
		return nil, nil, err
	}
	for k := range flags {
		if st, err := os.Stat(k); err == nil && st.IsDir() {
			keys = append(keys, k)
		} else {
			missing = append(missing, k)
		}
	}
	sort.Strings(keys)
	sort.Strings(missing)
	return keys, missing, nil
}

// trustBlock is one project's matrix: every file's answers plus the claude
// sessions running inside the project.
type trustBlock struct {
	Key      string
	Statuses []trust.Status
	Live     []liveLine
}

// liveLine is one running claude. Owner is the row whose own runtime
// sessions/ dir recorded it (full isolation), or "" when that dir is shared
// and the account cannot be told apart.
type liveLine struct {
	PID       int
	SessionID string
	Cwd       string
	Owner     string
}

// trustBlockFor reads every file's answers for key and keeps the live
// sessions whose cwd lies inside the project.
func trustBlockFor(env *trustEnv, key string, live []transcripts.LiveSession) (trustBlock, error) {
	statuses, err := trust.Report(env.files, key, gitRootOf(key))
	if err != nil {
		return trustBlock{}, err
	}
	b := trustBlock{Key: key, Statuses: statuses}
	for _, ls := range live {
		if !inProject(ls.Cwd, key) {
			continue
		}
		b.Live = append(b.Live, liveLine{PID: ls.PID, SessionID: ls.SessionID, Cwd: ls.Cwd, Owner: liveOwner(env.files, ls)})
	}
	return b, nil
}

// inProject reports whether cwd is key or lies under it, checking the path
// as recorded and after symlink resolution: Claude records the logical cwd,
// keys are resolved.
func inProject(cwd, key string) bool {
	if cwd == "" {
		return false
	}
	candidates := []string{filepath.Clean(cwd)}
	if norm, err := store.NormalizePath(cwd); err == nil {
		candidates = append(candidates, norm)
	}
	for _, c := range candidates {
		if c == key || strings.HasPrefix(c, key+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// liveOwner names the row whose own runtime sessions/ dir recorded ls.
// Under partial isolation every account's sessions/ is a symlink into
// ~/.claude, so several rows resolve to the same dir and the account cannot
// be told apart: "".
func liveOwner(files map[string]string, ls transcripts.LiveSession) string {
	target := resolvedRuntimeDir(ls.ConfigDir)
	owner := ""
	for name, path := range files {
		if resolvedRuntimeDir(trustConfigDir(name, path)) != target {
			continue
		}
		if owner != "" {
			return ""
		}
		owner = name
	}
	return owner
}

// trustConfigDir is the Claude config dir behind a row's .claude.json: the
// account dir itself, or ~/.claude next to the home file.
func trustConfigDir(name, jsonPath string) string {
	if name == trust.HomeName {
		return filepath.Join(filepath.Dir(jsonPath), ".claude")
	}
	return filepath.Dir(jsonPath)
}

// resolvedRuntimeDir is <configDir>/sessions with symlinks resolved, so
// partial-isolation accounts compare equal to home.
func resolvedRuntimeDir(configDir string) string {
	p := filepath.Join(configDir, transcripts.RuntimeSessionsSubdir)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// renderTrustMatrix prints one project block: the header, the per-row
// table, a `live:` line per running claude inside the project, and — when
// some row lacks an answer another row has — the legend and the sync hint.
func renderTrustMatrix(w io.Writer, b trustBlock, activeRow string) {
	key := transcripts.Sanitize(b.Key)
	fmt.Fprintf(w, "project:  %s        (key: %s)\n", short(key), key)
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tFOLDER-TRUST\tEXTERNAL-IMPORTS\tTOOLS\tMCP")
	for _, s := range b.Statuses {
		marker := " "
		if s.Account == activeRow {
			marker = "*"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\n", marker, trustRowLabel(s.Account),
			folderCell(s), externalCell(s.External), countCell(s.Tools, ""), countCell(s.MCPEnabled, " enabled"))
	}
	_ = tw.Flush()
	for _, l := range b.Live {
		fmt.Fprintln(w, liveText(l))
	}
	target, ok := trustSyncTarget(b.Statuses)
	if !ok {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, `"-" = never answered on that account (claude will ask); "inherited" = a parent directory is trusted (claude will not ask). Carry answers over with:`)
	fmt.Fprintf(w, "    bffs trust sync --to %s\n", target)
}

func trustRowLabel(name string) string {
	if name == trust.HomeName {
		return "(" + trust.HomeName + ")"
	}
	return name
}

func folderCell(s trust.Status) string {
	switch s.Folder {
	case trust.Accepted:
		return "accepted"
	case trust.Inherited:
		return fmt.Sprintf("inherited (from %s)", transcripts.Sanitize(s.InheritedFrom))
	}
	return "-"
}

func externalCell(a trust.Answer) string {
	switch a {
	case trust.Accepted:
		return "allowed"
	case trust.Declined:
		return "declined"
	}
	return "-"
}

func countCell(n int, suffix string) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// liveText renders one `live:` line.
func liveText(l liveLine) string {
	where := short(transcripts.Sanitize(l.Cwd))
	if where == "" {
		where = "(unknown directory)"
	}
	switch l.Owner {
	case "":
		return fmt.Sprintf("live: pid %d in %s (shared runtime dir — account not determinable); %s", l.PID, where, liveNote)
	case trust.HomeName:
		return fmt.Sprintf("live: pid %d in %s (unmanaged claude on ~/.claude.json); %s", l.PID, where, liveNote)
	}
	return fmt.Sprintf("live: pid %d in %s (account %q); a running claude on that account picks changes up within about a second", l.PID, where, l.Owner)
}

// trustSyncTarget picks the row the matrix's hint points at: the first row
// (accounts by name, home last) with a dialog unanswered that another row
// answered.
func trustSyncTarget(statuses []trust.Status) (string, bool) {
	for _, s := range statuses {
		if len(missingAnswers(statuses, s)) > 0 {
			return s.Account, true
		}
	}
	return "", false
}

// missingAnswers lists the dialogs s has not answered that some other row
// has, as "folder-trust" and/or "external-imports". Only an exact-key
// folder answer elsewhere counts — an inherited one cannot be copied.
func missingAnswers(statuses []trust.Status, s trust.Status) []string {
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
	var out []string
	if s.Folder == trust.Unset && folderElsewhere {
		out = append(out, "folder-trust")
	}
	if s.External == trust.Unset && externalElsewhere {
		out = append(out, "external-imports")
	}
	return out
}

// answeredBy names the row that answered the most of the dialogs s lacks
// (first by matrix order on ties).
func answeredBy(statuses []trust.Status, s trust.Status, missing []string) string {
	best, bestScore := "", 0
	for _, o := range statuses {
		if o.Account == s.Account {
			continue
		}
		score := 0
		for _, m := range missing {
			switch m {
			case "folder-trust":
				if o.Folder == trust.Accepted {
					score++
				}
			case "external-imports":
				if o.External != trust.Unset {
					score++
				}
			}
		}
		if score > bestScore {
			best, bestScore = o.Account, score
		}
	}
	return best
}

type trustJSONRow struct {
	Account       string `json:"account"`
	File          string `json:"file"`
	Present       bool   `json:"present"`
	Folder        string `json:"folder"`
	External      string `json:"external"`
	InheritedFrom string `json:"inherited_from,omitempty"`
	Tools         int    `json:"tools"`
	MCPEnabled    int    `json:"mcp_enabled"`
}

type trustJSONLive struct {
	PID       int    `json:"pid"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

type trustJSONBlock struct {
	ProjectKey string          `json:"project_key"`
	Accounts   []trustJSONRow  `json:"accounts"`
	Live       []trustJSONLive `json:"live"`
}

// renderTrustJSON writes the blocks as a JSON array, one element per project.
func renderTrustJSON(w io.Writer, blocks []trustBlock) error {
	out := make([]trustJSONBlock, 0, len(blocks))
	for _, b := range blocks {
		jb := trustJSONBlock{ProjectKey: transcripts.Sanitize(b.Key), Accounts: []trustJSONRow{}, Live: []trustJSONLive{}}
		for _, s := range b.Statuses {
			jb.Accounts = append(jb.Accounts, trustJSONRow{
				Account:       s.Account,
				File:          transcripts.Sanitize(s.File),
				Present:       s.Present,
				Folder:        s.Folder.String(),
				External:      s.External.String(),
				InheritedFrom: transcripts.Sanitize(s.InheritedFrom),
				Tools:         s.Tools,
				MCPEnabled:    s.MCPEnabled,
			})
		}
		for _, l := range b.Live {
			jb.Live = append(jb.Live, trustJSONLive{PID: l.PID, SessionID: transcripts.Sanitize(l.SessionID), Cwd: transcripts.Sanitize(l.Cwd)})
		}
		out = append(out, jb)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// syncRequest is one `bffs trust sync` (or `switch --sync-trust`) run.
type syncRequest struct {
	From string   // source row; "" = trust.BestSource per project
	To   []string // target rows
	Keys []string // project keys

	IncludePermissions bool
	Mirror             bool
	DryRun             bool
	Yes                bool
}

// syncPlan is everything planned for one target file.
type syncPlan struct {
	Target   string
	Path     string
	Projects []projectPlan
}

// projectPlan is the changes for one project key in a target, with the row
// they come from.
type projectPlan struct {
	Key     string
	Source  string
	Changes []trust.Change
}

// trustSyncTargets resolves --to: "all" is every row except from; anything
// else is one existing row (api_key accounts are refused with a pointer at
// home).
func trustSyncTargets(env *trustEnv, from, to string) ([]string, error) {
	if to == trustSyncAll {
		var out []string
		for _, n := range trustRowOrder(env.files) {
			if n != from {
				out = append(out, n)
			}
		}
		return out, nil
	}
	if err := trustRowExists(env, "--to", to); err != nil {
		return nil, err
	}
	if to == from {
		return nil, fmt.Errorf("--from and --to are both %q; nothing to copy", to)
	}
	return []string{to}, nil
}

// planTrustSync computes, without writing, what every target would receive.
// The source of a project is req.From, or trust.BestSource for that project
// (active row first); a project nobody trusts has no source and is skipped
// — an error when it is the only project.
func planTrustSync(env *trustEnv, req syncRequest) ([]syncPlan, error) {
	sources := make(map[string]string, len(req.Keys))
	for _, key := range req.Keys {
		src := req.From
		if src == "" {
			statuses, err := trust.Report(env.files, key, gitRootOf(key))
			if err != nil {
				return nil, err
			}
			name, ok := trust.BestSource(statuses, env.activeRow)
			if !ok {
				if len(req.Keys) == 1 {
					return nil, fmt.Errorf("no account has answered the folder-trust dialog for %s; nothing to copy (pass --from to pick a source anyway)", short(transcripts.Sanitize(key)))
				}
				continue
			}
			src = name
		}
		sources[key] = src
	}

	plans := make([]syncPlan, 0, len(req.To))
	for _, target := range req.To {
		p := syncPlan{Target: target, Path: env.files[target]}
		var order []string
		bySource := map[string][]string{}
		own := 0
		for _, key := range req.Keys {
			src, ok := sources[key]
			if !ok {
				continue
			}
			if src == target {
				own++
				continue
			}
			if _, seen := bySource[src]; !seen {
				order = append(order, src)
			}
			bySource[src] = append(bySource[src], key)
		}
		if own > 0 && len(order) == 0 {
			continue // this row is only ever the source (--to all): not a target
		}
		byKey := map[string][]trust.Change{}
		for _, src := range order {
			changes, err := trust.Plan(env.files[src], p.Path, trust.Options{
				ProjectKeys:        bySource[src],
				Mirror:             req.Mirror,
				IncludePermissions: req.IncludePermissions,
			})
			if err != nil {
				return nil, err
			}
			for _, c := range changes {
				byKey[c.ProjectKey] = append(byKey[c.ProjectKey], c)
			}
		}
		for _, key := range req.Keys {
			if changes := byKey[key]; len(changes) > 0 {
				p.Projects = append(p.Projects, projectPlan{Key: key, Source: sources[key], Changes: changes})
			}
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// runTrustSync plans, prints, confirms and applies req. Nothing is written
// before every target has been planned; a non-interactive run without -y
// stops there. One prompter serves every confirmation.
func runTrustSync(cmd *cobra.Command, pr *prompter, env *trustEnv, req syncRequest, interactive bool) error {
	out := cmd.OutOrStdout()
	plans, err := planTrustSync(env, req)
	if err != nil {
		return err
	}
	total := 0
	for _, p := range plans {
		total += len(p.Projects)
	}
	if total == 0 {
		fmt.Fprintln(out, "no changes")
		return nil
	}
	if !req.DryRun && !req.Yes && !interactive {
		return errors.New("refusing to write without -y in a non-interactive session")
	}
	for _, p := range plans {
		if len(p.Projects) == 0 {
			if len(plans) > 1 {
				fmt.Fprintf(out, "no changes for %q\n", p.Target)
			}
			continue
		}
		for _, pp := range p.Projects {
			fmt.Fprintf(out, "project %s → account %q (from %q):\n", short(transcripts.Sanitize(pp.Key)), p.Target, pp.Source)
			for _, c := range pp.Changes {
				fmt.Fprintf(out, "  %-41s %s -> %s\n", c.Key, changeCell(c.Key, c.From), changeCell(c.Key, c.To))
			}
			if req.IncludePermissions {
				printMCPCommands(out, pp.Changes)
			}
		}
		if req.DryRun {
			continue
		}
		if !req.Yes {
			if req.IncludePermissions {
				fmt.Fprintln(out, "this copies allowedTools and MCP server approvals; each mcpServers command line is printed before applying")
			}
			ans, err := pr.line(fmt.Sprintf("apply to %s? [y/N] ", short(transcripts.Sanitize(p.Path))))
			if err != nil {
				return err
			}
			if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
				fmt.Fprintln(out, "aborted")
				return nil
			}
		}
		var changes []trust.Change
		for _, pp := range p.Projects {
			changes = append(changes, pp.Changes...)
		}
		if err := applyTrust(p.Path, changes); err != nil {
			return err
		}
		who := "A running claude on that account"
		if p.Target == trust.HomeName {
			who = "A running unmanaged claude"
		}
		fmt.Fprintf(out, "updated %s in %q. %s picks the change up within about a second.\n", plural(len(p.Projects), "project"), p.Target, who)
		if !req.IncludePermissions {
			fmt.Fprintln(out, "(allowedTools and MCP approvals were not copied; add --include-permissions to include them.)")
		}
	}
	if req.DryRun {
		fmt.Fprintln(out, "dry run: nothing written")
	}
	return nil
}

// changeCell renders one side of a change line. An mcpServers value is
// shown as the set of server names — a server definition can carry an env
// block with secrets, and the command lines the user is approving are
// printed separately by printMCPCommands. Everything else is compact JSON.
func changeCell(key string, raw json.RawMessage) string {
	if raw == nil || key != "mcpServers" {
		return rawCell(raw)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil {
		return "(not an object)"
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, transcripts.Sanitize(n))
	}
	sort.Strings(names)
	return "{" + strings.Join(names, ", ") + "}"
}

// rawCell renders a change value: compact JSON, or "unset" for an absent
// field.
func rawCell(raw json.RawMessage) string {
	if raw == nil {
		return "unset"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return transcripts.Sanitize(string(raw))
	}
	return transcripts.Sanitize(buf.String())
}

// printMCPCommands prints the command line of every MCP server a change
// would copy, so the user sees what they approve before confirming.
func printMCPCommands(w io.Writer, changes []trust.Change) {
	for _, c := range changes {
		if c.Key != "mcpServers" {
			continue
		}
		var servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			URL     string   `json:"url"`
		}
		if err := json.Unmarshal(c.To, &servers); err != nil {
			continue
		}
		names := make([]string, 0, len(servers))
		for n := range servers {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := servers[n]
			line := strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
			if line == "" {
				line = s.URL
			}
			if s.Type != "" {
				line = "(" + s.Type + ") " + line
			}
			fmt.Fprintf(w, "  mcpServers[%q]: %s\n", transcripts.Sanitize(n), transcripts.Sanitize(line))
		}
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// distinctProjects counts the project keys a set of changes touches.
func distinctProjects(changes []trust.Change) int {
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.ProjectKey] = true
	}
	return len(seen)
}

// trustHintEnabled reports whether switch/show may print the trust note:
// only an explicit trust_hint = false in state.toml silences it.
func trustHintEnabled(state store.State) bool {
	return state.TrustHint == nil || *state.TrustHint
}

// trustHintForCwd is the note `bffs switch` and `bffs show` append when
// account has not answered a dialog for the current directory's project
// that another row has. Best-effort: "" on any error, nothing is written.
func trustHintForCwd(cfgDir string, accs store.Accounts, account string) string {
	files, err := trust.Files(cfgDir, "", accs)
	if err != nil {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return trustHint(files, trustRowFor(files, accs, account), cwd)
}

// trustHint renders the switch/show note for row and the project at dir,
// or "" when row has answered everything another row has (or on error).
func trustHint(files map[string]string, row, dir string) string {
	if row == "" {
		return ""
	}
	key, err := projectKeyFor(dir)
	if err != nil {
		return ""
	}
	statuses, err := trust.Report(files, key, gitRootOf(key))
	if err != nil {
		return ""
	}
	var self *trust.Status
	for i := range statuses {
		if statuses[i].Account == row {
			self = &statuses[i]
			break
		}
	}
	if self == nil {
		return ""
	}
	missing := missingAnswers(statuses, *self)
	if len(missing) == 0 {
		return ""
	}
	dialogs := strings.Join(missing, " / ") + " dialog"
	if len(missing) > 1 {
		dialogs += "s"
	}
	return fmt.Sprintf("note: %q hasn't answered the %s for %s (%q has).\n      Carry them over:  bffs trust sync --to %s\n",
		row, dialogs, short(transcripts.Sanitize(key)), answeredBy(statuses, *self, missing), row)
}

// switchTrustSync is `bffs switch --sync-trust`: carry the current
// directory's answers onto the account just made active, with an inline
// confirmation.
func switchTrustSync(cmd *cobra.Command, cfgDir, account string) error {
	env, err := loadTrustEnv(cfgDir, "")
	if err != nil {
		return err
	}
	row := trustRowFor(env.files, env.accs, account)
	if row == "" {
		return fmt.Errorf("unknown account %q; known: %v", account, env.accs.Names())
	}
	key, err := projectKeyFor("")
	if err != nil {
		return err
	}
	pr := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
	return runTrustSync(cmd, pr, env, syncRequest{To: []string{row}, Keys: []string{key}}, isTTY())
}

// carryTrustOnLogin copies every project answer the source row holds — for
// projects whose directory still exists — onto the account that just
// logged in, and reports how many projects changed. The source is the
// previously active account when it is a managed account other than
// target, else home.
func carryTrustOnLogin(cfgDir, homeJSON string, accs store.Accounts, prevActive, target string) (n int, source string, err error) {
	files, err := trust.Files(cfgDir, homeJSON, accs)
	if err != nil {
		return 0, "", err
	}
	source = trust.HomeName
	if prevActive != target && prevActive != "" {
		if _, ok := files[prevActive]; ok {
			source = prevActive
		}
	}
	dst, ok := files[target]
	if !ok {
		return 0, source, fmt.Errorf("account %q has no .claude.json to carry answers into", target)
	}
	keys, _, err := projectKeysIn(files[source])
	if err != nil {
		return 0, source, err
	}
	changes, err := trust.Plan(files[source], dst, trust.Options{ProjectKeys: keys})
	if err != nil {
		return 0, source, err
	}
	if len(changes) == 0 {
		return 0, source, nil
	}
	if err := applyTrust(dst, changes); err != nil {
		return 0, source, err
	}
	return distinctProjects(changes), source, nil
}

// applyTrust writes changes into the .claude.json at path, creating its
// directory first: Claude's lock is a directory next to the file, so a
// target whose account dir does not exist yet could not be locked.
func applyTrust(path string, changes []trust.Change) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return trust.Apply(path, changes)
}

// seedClaudeJSONOnce seeds a per-account .claude.json from ~/.claude.json
// only when it does not exist yet: a --force re-login keeps whatever answers
// were synced into the existing file.
func seedClaudeJSONOnce(target string) (seeded bool, err error) {
	if _, err := os.Lstat(target); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := claudejson.SeedFromHome(target); err != nil {
		return false, err
	}
	return true, nil
}
