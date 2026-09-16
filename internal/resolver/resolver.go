package resolver

import (
	"fmt"
	"os"

	"github.com/jratienza65/bffs/internal/projectconfig"
	"github.com/jratienza65/bffs/internal/store"
)

const EnvAccount = "BFFS_ACCOUNT"

type Source string

const (
	SourceEnv     Source = "env"
	SourceProject Source = "project"
	SourceGlobal  Source = "global"
	SourceNone    Source = "none"
)

type Result struct {
	Account     store.Account
	Source      Source
	ProjectFile string
}

// Resolve picks the account to use, given a config dir and a starting cwd.
// Precedence: env BFFS_ACCOUNT > nearest project file > global state > none.
// An unknown account name from any source is an error.
func Resolve(configDir, cwd string) (Result, error) {
	accounts, err := store.LoadAccounts(configDir)
	if err != nil {
		return Result{}, err
	}

	if name := os.Getenv(EnvAccount); name != "" {
		acc, ok := accounts.Get(name)
		if !ok {
			return Result{}, unknownAccount(name, EnvAccount, accounts)
		}
		return Result{Account: acc, Source: SourceEnv}, nil
	}

	found, ok, err := projectconfig.Find(cwd)
	if err != nil {
		return Result{}, err
	}
	if ok && found.Config.Account != "" {
		acc, ok := accounts.Get(found.Config.Account)
		if !ok {
			return Result{}, unknownAccount(found.Config.Account, found.Path, accounts)
		}
		return Result{Account: acc, Source: SourceProject, ProjectFile: found.Path}, nil
	}

	state, err := store.LoadState(configDir)
	if err != nil {
		return Result{}, err
	}
	if state.Active != "" {
		acc, ok := accounts.Get(state.Active)
		if !ok {
			return Result{}, unknownAccount(state.Active, "global state", accounts)
		}
		return Result{Account: acc, Source: SourceGlobal}, nil
	}

	return Result{Source: SourceNone}, nil
}

func unknownAccount(name, origin string, accounts store.Accounts) error {
	known := accounts.Names()
	if len(known) == 0 {
		return fmt.Errorf("account %q (from %s) is unknown; no accounts have been added yet (try `bffs add %s`)", name, origin, name)
	}
	return fmt.Errorf("account %q (from %s) is unknown; known accounts: %v", name, origin, known)
}
