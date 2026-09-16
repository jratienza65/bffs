package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/accounts"
	"github.com/jratienza65/bffs/internal/store"
)

var (
	loginConsole      bool
	loginSSO          bool
	loginEmail        string
	loginForce        bool
	loginNoSwap       bool
	loginPreset       string
	loginNoTrustCarry bool
)

var loginCmd = &cobra.Command{
	Use:   "login [name]",
	Short: "Run `claude auth login` against a per-account session dir and save the result as a new account",
	Long: `Drives the real ` + "`claude auth login`" + ` flow (browser-based OAuth) inside a
per-account session directory under <bffs-config>/sessions/<name>/.

Claude Code reads its entire config tree (identity at .claude.json,
credentials at .credentials.json or a hashed Keychain entry on macOS) from
CLAUDE_CONFIG_DIR. Pointing it at a per-account dir gives full per-account
isolation: concurrent ` + "`claude`" + ` sessions on different accounts cannot collide,
and per-project pinning via bffs.toml works.

The --preset flag controls how much of ~/.claude is symlinked into the
per-account dir: "full" (nothing shared), "partial" (settings/skills/plugins
shared — default), or "minimal" (everything except .claude.json and
.credentials.json shared). The chosen preset is stored on the account and
can be changed later with ` + "`bffs reisolate <name>`" + `.

A new account's .claude.json is seeded from ~/.claude.json once (so claude's
first-run wizard does not fire again) and then receives the folder-trust and
external-imports answers the previously active account recorded, for every
project whose directory still exists — never downgrading, never overriding a
decline. --no-trust-carry skips that step; see ` + "`bffs trust`" + `.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfgDir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(cfgDir)
		if err != nil {
			return err
		}
		state, err := store.LoadState(cfgDir)
		if err != nil {
			return err
		}
		prevActive := state.Active

		out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

		var name string
		if len(args) > 0 {
			name = args[0]
		}
		if name == "" {
			name = defaultNameFromEmail(loginEmail)
		}
		if name == "" {
			return errors.New("could not determine an account name; rerun with: `bffs login <name>` (or pass --email)")
		}
		if err := validateName(name); err != nil {
			return err
		}
		if existing, exists := accs.Accounts[name]; exists && !loginForce {
			return fmt.Errorf("account %q already exists (type=%s); pass --force to overwrite, or pick a different name", name, existing.Type)
		}

		prep, err := accounts.PrepareOAuth(cfgDir, name, store.IsolationPreset(loginPreset), loginConsole, loginSSO, loginEmail)
		if err != nil {
			return err
		}
		for _, s := range prep.Skipped {
			fmt.Fprintf(errOut, "warning: %s already exists in %s as a real file; left untouched (won't be shared with ~/.claude)\n", s, prep.SessionDir)
		}

		fmt.Fprintf(errOut, "Session dir: %s (isolation=%s)\n", prep.SessionDir, prep.Preset)
		fmt.Fprintf(errOut, "Launching: %s %s\n", prep.Bin, strings.Join(prep.Args, " "))
		fmt.Fprintln(errOut, "(complete the browser flow; credentials land in this account's session dir)")
		fmt.Fprintln(errOut)

		c := exec.Command(prep.Bin, prep.Args...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		c.Env = prep.Env
		if err := c.Run(); err != nil {
			return fmt.Errorf("`claude auth login` failed: %w", err)
		}

		acc, err := accounts.CompleteOAuth(cfgDir, prep, loginEmail, !loginNoSwap)
		if err != nil {
			return err
		}
		email := acc.Email
		accs.Accounts[name] = acc

		fmt.Fprintln(out)
		fmt.Fprintf(out, "Saved as account %q (type=oauth", name)
		if email != "" {
			fmt.Fprintf(out, ", email=%s", email)
		}
		fmt.Fprintln(out, ").")
		if !loginNoSwap {
			fmt.Fprintln(out, "This is now the active account; the shim will set CLAUDE_CONFIG_DIR per-invocation.")
		}
		fmt.Fprintln(out, "Per-project pinning works: drop a `bffs.toml` with `account = \""+name+"\"` in any project.")

		// Carry the previously active account's dialog answers over so
		// claude does not ask again in every project. Best-effort: the
		// login itself has already succeeded.
		if !loginNoTrustCarry {
			n, src, err := carryTrustOnLogin(cfgDir, "", accs, prevActive, name)
			switch {
			case err != nil:
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not carry trust answers over: %v\n", err)
			case n > 0 && src == reservedAccountName:
				fmt.Fprintf(out, "added %d answers from ~/.claude.json\n", n)
			case n > 0:
				fmt.Fprintf(out, "added %d answers from %q that ~/.claude.json did not have\n", n, src)
			}
		}
		return nil
	},
}

func init() {
	loginCmd.Flags().BoolVar(&loginConsole, "console", false, "use Anthropic Console (API usage billing) instead of Claude subscription")
	loginCmd.Flags().BoolVar(&loginSSO, "sso", false, "force SSO login flow")
	loginCmd.Flags().StringVar(&loginEmail, "email", "", "pre-populate the email on the login page")
	loginCmd.Flags().BoolVar(&loginForce, "force", false, "overwrite an existing account with the same name")
	loginCmd.Flags().BoolVar(&loginNoSwap, "no-swap", false, "don't make the new account active")
	loginCmd.Flags().StringVar(&loginPreset, "preset", "", `isolation preset for this account: "partial" (default — drop-in: only auth per-account) or "full" (fresh world per account)`)
	loginCmd.Flags().BoolVar(&loginNoTrustCarry, "no-trust-carry", false, "don't copy the previously active account's trust answers onto the new account")
	rootCmd.AddCommand(loginCmd)
}

// defaultHomeClaudeDir returns ~/.claude (the user's shared Claude Code dir
// that "partial" / "minimal" isolation symlink subpaths back to).
func defaultHomeClaudeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}
