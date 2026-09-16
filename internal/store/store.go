package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

type AccountType string

const (
	TypeOAuth  AccountType = "oauth"
	TypeAPIKey AccountType = "api_key"
)

func (t AccountType) Valid() bool {
	return t == TypeOAuth || t == TypeAPIKey
}

// IsolationPreset controls how much of an oauth account's CLAUDE_CONFIG_DIR
// is isolated from the user's shared ~/.claude/.
//
//   - "partial" (default): every entry in ~/.claude/ except .claude.json and
//                          .credentials.json is symlinked into the per-account
//                          dir, so settings, skills, plugins, history,
//                          projects, todos, etc. all carry across accounts.
//                          Drop-in feel — only auth/identity is per-account.
//   - "full":              nothing symlinked. Each account is a fresh Claude
//                          Code world (no shared settings, skills, history,
//                          plugins, etc.). Max isolation.
type IsolationPreset string

const (
	IsolationFull    IsolationPreset = "full"
	IsolationPartial IsolationPreset = "partial"
	IsolationDefault IsolationPreset = IsolationPartial
)

func (p IsolationPreset) Valid() bool {
	return p == IsolationFull || p == IsolationPartial
}

// ResolveIsolation returns the effective preset, with override > global > default.
func ResolveIsolation(perAccount, global IsolationPreset) IsolationPreset {
	if perAccount.Valid() {
		return perAccount
	}
	if global.Valid() {
		return global
	}
	return IsolationDefault
}

type Account struct {
	Name   string      `toml:"-"`
	Type   AccountType `toml:"type"`
	Secret string      `toml:"secret,omitempty"`
	Email  string      `toml:"email,omitempty"`

	// Isolation overrides the global isolation preset for this oauth account.
	// Empty means "use the global default in state.toml" (or IsolationDefault
	// if that is also empty). Ignored for api_key accounts.
	Isolation IsolationPreset `toml:"isolation,omitempty"`

	// OAuthAccountMeta is the JSON object Claude Code caches at
	// `<config-dir>/.claude.json` under `oauthAccount`: emailAddress,
	// accountUuid, organizationUuid, etc. For oauth accounts this is read
	// post-login from the account's per-account session dir and stored as
	// metadata for `bffs list` / `bffs show`. Not used at runtime.
	OAuthAccountMeta string `toml:"oauth_account_meta,omitempty"`

	// UserID is the per-user hash Claude Code writes at the top-level
	// `userID` field of `<config-dir>/.claude.json`. Same caveats as
	// OAuthAccountMeta — display-only.
	UserID string `toml:"user_id,omitempty"`
}

type Accounts struct {
	Accounts map[string]Account `toml:"accounts"`
}

type State struct {
	Active    string          `toml:"active,omitempty"`
	Isolation IsolationPreset `toml:"isolation,omitempty"`
}

func (a Accounts) Names() []string {
	names := make([]string, 0, len(a.Accounts))
	for n := range a.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (a Accounts) Get(name string) (Account, bool) {
	acc, ok := a.Accounts[name]
	if !ok {
		return Account{}, false
	}
	acc.Name = name
	return acc, true
}

func LoadAccounts(dir string) (Accounts, error) {
	path := filepath.Join(dir, AccountsFile)
	var a Accounts
	if _, err := toml.DecodeFile(path, &a); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Accounts{Accounts: map[string]Account{}}, nil
		}
		return Accounts{}, fmt.Errorf("read %s: %w", path, err)
	}
	if a.Accounts == nil {
		a.Accounts = map[string]Account{}
	}
	return a, nil
}

func SaveAccounts(dir string, a Accounts) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, AccountsFile)
	return writeToml(path, a)
}

func LoadState(dir string) (State, error) {
	path := filepath.Join(dir, StateFile)
	var s State
	if _, err := toml.DecodeFile(path, &s); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read %s: %w", path, err)
	}
	return s, nil
}

func SaveState(dir string, s State) error {
	if err := EnsureDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, StateFile)
	return writeToml(path, s)
}

func writeToml(path string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(v); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("encode toml: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
