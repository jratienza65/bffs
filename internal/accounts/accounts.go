// Package accounts creates the accounts bffs manages.
//
// An api_key account is one write; an oauth account is a session
// directory, a seeded .claude.json and a `claude auth login` that a
// person completes in a browser. Both `bffs add` / `bffs login` and the
// browser go through here, so the rules live in one place: the name
// grammar, the reserved `home`, which isolation preset applies, seeding
// the per-account .claude.json exactly once, and reading back what
// claude wrote.
//
// The login itself is not run here. It needs the terminal — a browser
// flow, a device code — and who owns the terminal is the caller's
// business: the CLI runs it in place, the TUI hands it over through
// bubbletea's ExecProcess. PrepareOAuth returns the command to run and
// CompleteOAuth records what it produced.
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/store"
)

// HomeName is the pseudo-account that stands for ~/.claude.json — the
// config unmanaged claude and api_key accounts run with. Trust sync
// addresses it as `home`, so no real account may take the name.
const HomeName = "home"

// ValidateName is the single gate for a new account name: add, login,
// rename and the browser all go through it. The name becomes a
// directory under sessions/, hence the charset.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if name == HomeName {
		return fmt.Errorf("account name %q is reserved for ~/.claude.json (the unmanaged/api_key home config); choose another name", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("name %q contains invalid character %q (use letters, digits, - or _)", name, r)
		}
	}
	return nil
}

// Taken reports whether the name is already an account, and of what
// type — the caller decides whether that is an error or a re-login.
func Taken(accs store.Accounts, name string) (store.AccountType, bool) {
	a, ok := accs.Accounts[name]
	return a.Type, ok
}

// AddAPIKey saves an api_key account. The secret is the bare
// sk-ant-... key; it is stored in accounts.toml at 0600 (SECURITY.md
// has the plan for moving it into the OS keystore).
func AddAPIKey(cfgDir, name, secret, email string, force bool) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("secret is required")
	}
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return err
	}
	if t, ok := Taken(accs, name); ok && !force {
		return fmt.Errorf("account %q already exists (type=%s); pass --force to overwrite, or pick a different name", name, t)
	}
	accs.Accounts[name] = store.Account{Type: store.TypeAPIKey, Secret: strings.TrimSpace(secret), Email: email}
	return store.SaveAccounts(cfgDir, accs)
}

// OAuthPrep is everything the login needs: where the account's config
// tree is, what to run, and with which environment.
type OAuthPrep struct {
	Name       string
	SessionDir string
	Preset     store.IsolationPreset // the resolved preset, for display
	Requested  store.IsolationPreset // what was asked for ("" = follow the global default)
	Bin        string                // the real claude
	Args       []string
	Env        []string
	Skipped    []string // paths where claude had written a real file
	PrevActive string
}

// PrepareOAuth makes the account's session directory, syncs the
// symlinks its isolation preset implies and seeds its .claude.json,
// then returns the `claude auth login` to run. Nothing is written to
// accounts.toml until CompleteOAuth: an abandoned login leaves a
// session directory and no account.
func PrepareOAuth(cfgDir, name string, requested store.IsolationPreset, console, sso bool, email string) (OAuthPrep, error) {
	var p OAuthPrep
	if err := ValidateName(name); err != nil {
		return p, err
	}
	if requested != "" && !requested.Valid() {
		return p, fmt.Errorf(`invalid isolation preset %q: must be "partial" or "full"`, requested)
	}
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return p, err
	}
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return p, err
	}
	realClaude, err := shim.FindRealClaude(cfgDir)
	if err != nil {
		return p, err
	}
	homeClaude, err := HomeClaudeDir()
	if err != nil {
		return p, fmt.Errorf("locate ~/.claude: %w", err)
	}

	p = OAuthPrep{
		Name:       name,
		SessionDir: sessions.Dir(cfgDir, name),
		Preset:     store.ResolveIsolation(requested, state.Isolation),
		Requested:  requested,
		Bin:        realClaude,
		PrevActive: state.Active,
	}
	_ = accs // loaded so a caller's refusal and ours read the same file

	if p.Skipped, err = sessions.SyncSymlinks(p.SessionDir, homeClaude, p.Preset); err != nil {
		return p, fmt.Errorf("set up session dir %s: %w", p.SessionDir, err)
	}
	// Seed the per-account .claude.json from ~/.claude.json so claude's
	// first-run wizard does not fire on the next invocation. Only ever
	// once: a re-login must not wipe answers already synced into it.
	if err := seedOnce(filepath.Join(p.SessionDir, claudejson.Filename)); err != nil {
		return p, fmt.Errorf("seed per-account .claude.json: %w", err)
	}

	p.Args = []string{"auth", "login"}
	if console {
		p.Args = append(p.Args, "--console")
	} else {
		p.Args = append(p.Args, "--claudeai")
	}
	if sso {
		p.Args = append(p.Args, "--sso")
	}
	if email != "" {
		p.Args = append(p.Args, "--email", email)
	}
	p.Env = WithConfigDir(os.Environ(), p.SessionDir)
	return p, nil
}

// CompleteOAuth records the account the login produced: the display
// metadata claude wrote into the session dir's .claude.json, and the
// isolation preset that was asked for (empty follows the global
// default). makeActive writes state.toml the way `bffs switch` does.
func CompleteOAuth(cfgDir string, p OAuthPrep, email string, makeActive bool) (store.Account, error) {
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return store.Account{}, err
	}
	acc := store.Account{Type: store.TypeOAuth, Email: email, Isolation: p.Requested}
	snap, snapErr := claudejson.ReadFrom(filepath.Join(p.SessionDir, claudejson.Filename))
	if snapErr == nil {
		if acc.Email == "" {
			acc.Email = SnapshotEmail(snap)
		}
		if len(snap.OAuthAccount) > 0 {
			acc.OAuthAccountMeta = string(snap.OAuthAccount)
		}
		acc.UserID = snap.UserID
	}
	accs.Accounts[p.Name] = acc
	if err := store.SaveAccounts(cfgDir, accs); err != nil {
		return acc, err
	}
	if makeActive {
		state, err := store.LoadState(cfgDir)
		if err != nil {
			return acc, err
		}
		state.Active = p.Name
		if err := store.SaveState(cfgDir, state); err != nil {
			return acc, err
		}
	}
	return acc, nil
}

// WithConfigDir returns env with CLAUDE_CONFIG_DIR replaced (or
// appended) to point at sessionDir. Other variables are preserved.
func WithConfigDir(env []string, sessionDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == shim.EnvClaudeCfgDir {
			continue
		}
		out = append(out, kv)
	}
	return append(out, shim.EnvClaudeCfgDir+"="+sessionDir)
}

// HomeClaudeDir is ~/.claude, the shared tree partial isolation
// symlinks back to.
func HomeClaudeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// SnapshotEmail extracts oauthAccount.emailAddress from a .claude.json
// snapshot, "" when the field is not there.
func SnapshotEmail(s claudejson.Snapshot) string {
	if len(s.OAuthAccount) == 0 {
		return ""
	}
	var x struct {
		EmailAddress string `json:"emailAddress"`
	}
	_ = json.Unmarshal(s.OAuthAccount, &x)
	return x.EmailAddress
}

// seedOnce seeds a per-account .claude.json from ~/.claude.json only
// when it does not exist yet.
func seedOnce(target string) error {
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return claudejson.SeedFromHome(target)
}
