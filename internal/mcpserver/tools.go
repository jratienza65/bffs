package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/projectconfig"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/shimcheck"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usage"
)

// nextLaunchCaveat is appended to every result and description that stages a
// change: the shim injects env per-invocation, so nothing bffs does can
// re-point a claude process that is already running — including the one
// hosting this server.
const nextLaunchCaveat = "Account changes take effect the NEXT time claude is launched through the bffs shim; they never change the account of an already-running claude session, including this one."

// handlers carries the dependencies the tools need. Handlers must never
// write to stdout — on stdio transport it carries JSON-RPC exclusively.
type handlers struct {
	cfgDir string
	// homeClaudeDir overrides the shared ~/.claude for account_usage; empty
	// means the real one. A test seam — New() leaves it empty.
	homeClaudeDir string
}

type ListAccountsIn struct{}

type AccountInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type" jsonschema:"account type: oauth or api_key"`
	Email     string `json:"email,omitempty"`
	Isolation string `json:"isolation,omitempty" jsonschema:"effective isolation preset (oauth accounts only): partial or full"`
	IsDefault bool   `json:"is_default" jsonschema:"whether this is the global default account"`
}

type ListAccountsOut struct {
	Accounts       []AccountInfo `json:"accounts"`
	DefaultAccount string        `json:"default_account,omitempty"`
	Note           string        `json:"note,omitempty"`
}

func (h *handlers) listAccounts(ctx context.Context, req *mcp.CallToolRequest, in ListAccountsIn) (*mcp.CallToolResult, ListAccountsOut, error) {
	var out ListAccountsOut
	accs, err := store.LoadAccounts(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	state, err := store.LoadState(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	out.Accounts = []AccountInfo{}
	for _, name := range accs.Names() {
		acc := accs.Accounts[name]
		info := AccountInfo{
			Name:      name,
			Type:      string(acc.Type),
			Email:     acc.Email,
			IsDefault: name == state.Active,
		}
		if acc.Type == store.TypeOAuth {
			info.Isolation = string(store.ResolveIsolation(acc.Isolation, state.Isolation))
		}
		out.Accounts = append(out.Accounts, info)
	}
	out.DefaultAccount = state.Active
	if len(out.Accounts) == 0 {
		out.Note = "No accounts configured. Add one in a terminal with `bffs add <name>` (api key) or `bffs login <name>` (oauth)."
	}
	return nil, out, nil
}

type ResolveIn struct {
	Directory string `json:"directory,omitempty" jsonschema:"directory to resolve for; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly"`
}

type ResolveOut struct {
	Directory             string `json:"directory"`
	Account               string `json:"account,omitempty"`
	Type                  string `json:"type,omitempty" jsonschema:"account type: oauth or api_key"`
	Email                 string `json:"email,omitempty"`
	Source                string `json:"source" jsonschema:"tier that selected the account: env, project, path, global, or none"`
	ProjectFile           string `json:"project_file,omitempty" jsonschema:"absolute path of the bffs.toml that matched, when source is project"`
	PathRule              string `json:"path_rule,omitempty" jsonschema:"matched directory-rule prefix, when source is path"`
	Isolation             string `json:"isolation,omitempty" jsonschema:"effective isolation preset (oauth accounts only)"`
	EnvOverride           string `json:"env_override,omitempty" jsonschema:"BFFS_ACCOUNT value inherited by this server; when set it outranks every other source"`
	CurrentSessionAccount string `json:"current_session_account,omitempty" jsonschema:"best-effort: the account the claude session hosting this server is running as"`
	Note                  string `json:"note"`
}

func (h *handlers) resolveAccount(ctx context.Context, req *mcp.CallToolRequest, in ResolveIn) (*mcp.CallToolResult, ResolveOut, error) {
	var out ResolveOut
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	out.Directory = dir
	r, err := resolver.Resolve(h.cfgDir, dir)
	if err != nil {
		return nil, out, err
	}
	out.Source = string(r.Source)
	out.EnvOverride = os.Getenv(resolver.EnvAccount)
	if accs, err := store.LoadAccounts(h.cfgDir); err == nil {
		out.CurrentSessionAccount = detectSessionAccount(h.cfgDir, accs)
	}

	note := nextLaunchCaveat
	if out.EnvOverride != "" {
		note = "BFFS_ACCOUNT is set in the environment this server inherited (normally the shell claude was launched from) and outranks every other source; unset it there if switches or pins seem to have no effect. " + note
	}
	if r.Source == resolver.SourceNone {
		out.Note = "No account selected for this directory; claude runs with its own untouched credentials. " + note
		return nil, out, nil
	}

	out.Account = r.Account.Name
	out.Type = string(r.Account.Type)
	out.Email = r.Account.Email
	out.ProjectFile = r.ProjectFile
	out.PathRule = r.PathRule
	if r.Account.Type == store.TypeOAuth {
		state, err := store.LoadState(h.cfgDir)
		if err != nil {
			return nil, out, err
		}
		out.Isolation = string(store.ResolveIsolation(r.Account.Isolation, state.Isolation))
	}
	out.Note = note
	return nil, out, nil
}

type SwitchIn struct {
	Name string `json:"name" jsonschema:"account name to make the global default"`
}

type SwitchOut struct {
	Active   string `json:"active"`
	Previous string `json:"previous,omitempty"`
	Note     string `json:"note"`
}

func (h *handlers) switchAccount(ctx context.Context, req *mcp.CallToolRequest, in SwitchIn) (*mcp.CallToolResult, SwitchOut, error) {
	var out SwitchOut
	accs, err := store.LoadAccounts(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	if _, ok := accs.Accounts[in.Name]; !ok {
		return nil, out, fmt.Errorf("unknown account %q; known: %v", in.Name, accs.Names())
	}
	state, err := store.LoadState(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	out.Previous = state.Active
	state.Active = in.Name
	if err := store.SaveState(h.cfgDir, state); err != nil {
		return nil, out, err
	}
	out.Active = in.Name
	out.Note = "Global default set. Project bffs.toml files, directory rules, and BFFS_ACCOUNT still override it. " + nextLaunchCaveat
	return nil, out, nil
}

type PinIn struct {
	Account   string `json:"account" jsonschema:"account name the rule selects"`
	Directory string `json:"directory,omitempty" jsonschema:"directory to pin (the rule covers it and everything beneath); defaults to the server's working directory - pass the project root explicitly"`
}

type PinOut struct {
	Directory string `json:"directory"`
	Account   string `json:"account"`
	Previous  string `json:"previous,omitempty" jsonschema:"account the replaced rule pointed to, if one existed"`
	Warning   string `json:"warning,omitempty" jsonschema:"set when a bffs.toml in or above the directory outranks this rule"`
	Note      string `json:"note"`
}

func (h *handlers) pinAccount(ctx context.Context, req *mcp.CallToolRequest, in PinIn) (*mcp.CallToolResult, PinOut, error) {
	var out PinOut
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	// Fail before writing: a rule naming an account that does not exist would
	// break every claude invocation under that directory.
	accs, err := store.LoadAccounts(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	if _, ok := accs.Get(in.Account); !ok {
		return nil, out, fmt.Errorf("unknown account %q; known accounts: %v", in.Account, accs.Names())
	}
	paths, err := store.LoadPaths(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	prev, err := paths.Set(dir, in.Account)
	if err != nil {
		return nil, out, err
	}
	if err := store.SavePaths(h.cfgDir, paths); err != nil {
		return nil, out, err
	}
	out.Directory = dir
	out.Account = in.Account
	if prev != in.Account {
		out.Previous = prev
	}
	if found, ok, err := projectconfig.Find(dir); err == nil && ok && found.Config.Account != "" {
		out.Warning = fmt.Sprintf("bffs.toml at %s sets account %q and outranks this rule within its tree", found.Path, found.Config.Account)
	}
	out.Note = "Rule saved: claude launched in this directory or beneath it uses this account, unless a bffs.toml or BFFS_ACCOUNT overrides it. " + nextLaunchCaveat
	return nil, out, nil
}

type UnpinIn struct {
	Directory string `json:"directory,omitempty" jsonschema:"directory whose rule to remove; defaults to the server's working directory"`
}

type UnpinOut struct {
	Removed   bool   `json:"removed"`
	Directory string `json:"directory"`
	Account   string `json:"account,omitempty" jsonschema:"account the removed rule pointed to"`
	Note      string `json:"note"`
}

func (h *handlers) unpinAccount(ctx context.Context, req *mcp.CallToolRequest, in UnpinIn) (*mcp.CallToolResult, UnpinOut, error) {
	var out UnpinOut
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	out.Directory = dir
	paths, err := store.LoadPaths(h.cfgDir)
	if err != nil {
		return nil, out, err
	}
	// Record what the rule pointed to before removing, so the change is easy
	// to describe and to reverse.
	if rule, ok := paths.Match(dir); ok && rule.Path == dir {
		out.Account = rule.Account
	}
	removed, err := paths.Remove(dir)
	if err != nil {
		return nil, out, err
	}
	if !removed {
		// "Nothing to remove" is a legitimate answer for a model, not a
		// failure. Distinguish "no rule here" from "a parent rule covers it" —
		// otherwise removing the wrong thing looks like a no-op.
		out.Account = ""
		if rule, ok := paths.Match(dir); ok {
			out.Note = fmt.Sprintf("No rule for exactly %s; it inherits account %q from the rule on %s. Remove that rule instead (directory %q).", dir, rule.Account, rule.Path, rule.Path)
		} else {
			out.Note = fmt.Sprintf("No directory rule for %s.", dir)
		}
		return nil, out, nil
	}
	if err := store.SavePaths(h.cfgDir, paths); err != nil {
		return nil, out, err
	}
	out.Removed = true
	out.Note = "Rule removed. " + nextLaunchCaveat
	return nil, out, nil
}

type CheckShimIn struct{}

type ShimModeResult struct {
	Mode    string `json:"mode" jsonschema:"shell invocation mode probed: interactive, login, non-interactive, or 'current shell'"`
	Status  string `json:"status" jsonschema:"wins, shadowed, not-on-PATH, or unknown"`
	Blocker string `json:"blocker,omitempty" jsonschema:"the competing claude binary that wins instead, when shadowed"`
	Note    string `json:"note,omitempty"`
}

type CheckShimOut struct {
	ShimInstalled bool             `json:"shim_installed"`
	InstallDir    string           `json:"install_dir"`
	Shell         string           `json:"shell,omitempty"`
	Modes         []ShimModeResult `json:"modes"`
	Ambient       ShimModeResult   `json:"ambient" jsonschema:"verdict against the PATH this server inherited; only meaningful for 'right now, in this terminal'"`
	AllWin        bool             `json:"all_win"`
	Blocked       bool             `json:"blocked"`
	Note          string           `json:"note,omitempty"`
}

func (h *handlers) checkShim(ctx context.Context, req *mcp.CallToolRequest, in CheckShimIn) (*mcp.CallToolResult, CheckShimOut, error) {
	var out CheckShimOut
	installDir, err := shimcheck.DefaultInstallDir()
	if err != nil {
		return nil, out, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, out, fmt.Errorf("locate self: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	rep := shimcheck.Check(ctx, shimcheck.Options{InstallDir: installDir, SelfPath: self})

	out.InstallDir = installDir
	out.Shell = rep.Shell
	shimPath := filepath.Join(installDir, shimcheck.DefaultShimName())
	if _, err := os.Lstat(shimPath); err == nil {
		out.ShimInstalled = true
	}
	out.Modes = []ShimModeResult{}
	for _, m := range rep.Modes {
		out.Modes = append(out.Modes, toShimModeResult(m))
	}
	out.Ambient = toShimModeResult(rep.Ambient)
	out.AllWin = rep.AllWin()
	out.Blocked = rep.Blocked()
	switch {
	case !out.ShimInstalled:
		out.Note = fmt.Sprintf("No shim installed at %s; run `bffs init` in a terminal.", shimPath)
	case out.Blocked:
		out.Note = "Some invocation modes bypass the shim, so account switching silently does nothing there; run `bffs init` in a terminal for guided repair."
	}
	return nil, out, nil
}

type AccountUsageIn struct {
	HorizonHours int `json:"horizon_hours,omitempty" jsonschema:"lookback hours for the long window and transcript scan; default 168 (7 days), max 2160"`
}

type UsageWindow struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
	Messages      int   `json:"messages"`
	Sessions      int   `json:"sessions"`
}

type AccountUsageRow struct {
	Name           string      `json:"name"`
	Type           string      `json:"type" jsonschema:"oauth or api_key; only oauth accounts have subscription limits"`
	Tier           string      `json:"tier,omitempty" jsonschema:"cached plan/rate-limit tier, when known"`
	LastUsed       string      `json:"last_used,omitempty" jsonschema:"RFC3339; empty when never observed"`
	FiveHour       UsageWindow `json:"five_hour" jsonschema:"attributed burn in the last 5h (Anthropic's session-limit window)"`
	Horizon        UsageWindow `json:"horizon_window" jsonschema:"attributed burn over the requested horizon"`
	WeightedBurn5h float64     `json:"weighted_burn_5h" jsonschema:"5h burn collapsed via the heuristic weights; lower means more headroom"`
	LimitedUntil   string      `json:"limited_until,omitempty" jsonschema:"RFC3339 reset time of an active limit event, when detected and parseable"`
	LimitKind      string      `json:"limit_kind,omitempty" jsonschema:"session, weekly, or unknown - set when a recent limit event was detected"`
}

type AccountUsageOut struct {
	Accounts         []AccountUsageRow `json:"accounts"`
	SuggestedAccount string            `json:"suggested_account,omitempty" jsonschema:"account with the most estimated headroom; use with switch_account or pin_account"`
	Unattributed     UsageWindow       `json:"unattributed" jsonschema:"horizon-window burn that could not be attributed to any account"`
	Note             string            `json:"note"`
}

func (h *handlers) accountUsage(ctx context.Context, req *mcp.CallToolRequest, in AccountUsageIn) (*mcp.CallToolResult, AccountUsageOut, error) {
	var out AccountUsageOut
	horizon := usage.DefaultHorizon
	if in.HorizonHours > 0 {
		horizon = time.Duration(min(in.HorizonHours, 2160)) * time.Hour
	}
	rep, err := usage.Collect(h.cfgDir, usage.Options{Horizon: horizon, HomeClaudeDir: h.homeClaudeDir})
	if err != nil {
		return nil, out, err
	}

	out.Accounts = []AccountUsageRow{}
	for _, a := range rep.Accounts {
		row := AccountUsageRow{
			Name:           a.Name,
			Type:           a.Type,
			FiveHour:       toUsageWindow(a.Short),
			Horizon:        toUsageWindow(a.Long),
			WeightedBurn5h: a.Short.Weighted(),
		}
		if d := a.Tier.Display(); d != "-" {
			row.Tier = d
		}
		if !a.LastUsed.IsZero() {
			row.LastUsed = a.LastUsed.UTC().Format(time.RFC3339)
		}
		if a.Limit != nil {
			row.LimitKind = a.Limit.Kind
			if !a.Limit.ResetAt.IsZero() && a.Limit.ResetAt.After(rep.Now) {
				row.LimitedUntil = a.Limit.ResetAt.UTC().Format(time.RFC3339)
			}
		}
		out.Accounts = append(out.Accounts, row)
	}
	out.SuggestedAccount = rep.Suggested
	out.Unattributed = toUsageWindow(rep.Unattributed)
	out.Note = "Heuristic estimates from local transcripts and the bffs launch log. Attribution is best-effort: only shim-launched or session-metadata-attributable usage is counted, and the unattributed remainder is reported. suggested_account = lowest weighted 5h burn among oauth accounts with no active limit event (weights: input 1, output 5, cache-write 1.25, cache-read 0.1). Anthropic publishes no official subscription-limit API. " + nextLaunchCaveat
	return nil, out, nil
}

func toUsageWindow(w usage.WindowStats) UsageWindow {
	return UsageWindow{
		Input:         w.Input,
		Output:        w.Output,
		CacheCreation: w.CacheCreation,
		CacheRead:     w.CacheRead,
		Messages:      w.Messages,
		Sessions:      w.Sessions,
	}
}

func toShimModeResult(m shimcheck.ModeResult) ShimModeResult {
	return ShimModeResult{
		Mode:    string(m.Mode),
		Status:  string(m.Status),
		Blocker: m.Blocker,
		Note:    m.Note,
	}
}

// normalizeDir applies the repo's canonical path normalization (~ expansion,
// absolutization, best-effort symlink resolution), defaulting to the server's
// own working directory when the tool call names none.
func normalizeDir(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	return store.NormalizePath(dir)
}

// detectSessionAccount maps the environment this server inherited back to an
// account: a CLAUDE_CONFIG_DIR equal to an oauth account's session dir, or an
// ANTHROPIC_API_KEY equal to a stored api_key secret (compared, never
// emitted). Empty when undetectable — e.g. claude was launched without the
// shim.
func detectSessionAccount(cfgDir string, accs store.Accounts) string {
	if cfg := os.Getenv(shim.EnvClaudeCfgDir); cfg != "" {
		for _, name := range accs.Names() {
			if accs.Accounts[name].Type != store.TypeOAuth {
				continue
			}
			if pathsEqual(cfg, sessions.Dir(cfgDir, name)) {
				return name
			}
		}
	}
	if key := os.Getenv(shim.EnvAPIKey); key != "" {
		for _, name := range accs.Names() {
			acc := accs.Accounts[name]
			if acc.Type == store.TypeAPIKey && acc.Secret != "" && acc.Secret == key {
				return name
			}
		}
	}
	return ""
}

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
