package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/store"
)

var (
	loginConsole bool
	loginSSO     bool
	loginEmail   string
	loginForce   bool
	loginNoSwap  bool
	loginPreset  string
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
can be changed later with ` + "`bffs reisolate <name>`" + `.`,
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

		out := cmd.OutOrStdout()

		realClaude, err := shim.FindRealClaude(cfgDir)
		if err != nil {
			return err
		}

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

		preset := store.IsolationPreset(loginPreset)
		if loginPreset != "" && !preset.Valid() {
			return fmt.Errorf(`invalid --preset %q: must be "partial" or "full"`, loginPreset)
		}
		effectivePreset := store.ResolveIsolation(preset, state.Isolation)

		homeClaude, err := defaultHomeClaudeDir()
		if err != nil {
			return fmt.Errorf("locate ~/.claude: %w", err)
		}

		sessionDir := sessions.Dir(cfgDir, name)
		skipped, err := sessions.SyncSymlinks(sessionDir, homeClaude, effectivePreset)
		if err != nil {
			return fmt.Errorf("set up session dir %s: %w", sessionDir, err)
		}
		for _, s := range skipped {
			fmt.Fprintf(out, "warning: %s already exists in %s as a real file; left untouched (won't be shared with ~/.claude)\n", s, sessionDir)
		}

		loginArgs := []string{"auth", "login"}
		if loginConsole {
			loginArgs = append(loginArgs, "--console")
		} else {
			loginArgs = append(loginArgs, "--claudeai")
		}
		if loginSSO {
			loginArgs = append(loginArgs, "--sso")
		}
		if loginEmail != "" {
			loginArgs = append(loginArgs, "--email", loginEmail)
		}

		fmt.Fprintf(out, "Session dir: %s (isolation=%s)\n", sessionDir, effectivePreset)
		fmt.Fprintf(out, "Launching: %s %s\n", realClaude, strings.Join(loginArgs, " "))
		fmt.Fprintln(out, "(complete the browser flow; credentials land in this account's session dir)")
		fmt.Fprintln(out)

		c := exec.Command(realClaude, loginArgs...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		c.Env = withClaudeConfigDir(os.Environ(), sessionDir)
		if err := c.Run(); err != nil {
			return fmt.Errorf("`claude auth login` failed: %w", err)
		}

		// Read the per-account .claude.json claude wrote, for display metadata.
		snap, snapErr := claudejson.ReadFrom(filepath.Join(sessionDir, ".claude.json"))
		email := loginEmail
		if email == "" && snapErr == nil {
			email = snapshotEmail(snap)
		}

		acc := store.Account{
			Type:      store.TypeOAuth,
			Email:     email,
			Isolation: preset, // empty if not specified — falls back to global default
		}
		if snapErr == nil {
			if len(snap.OAuthAccount) > 0 {
				acc.OAuthAccountMeta = string(snap.OAuthAccount)
			}
			acc.UserID = snap.UserID
		}
		accs.Accounts[name] = acc
		if err := store.SaveAccounts(cfgDir, accs); err != nil {
			return err
		}
		if !loginNoSwap {
			state.Active = name
			if err := store.SaveState(cfgDir, state); err != nil {
				return err
			}
		}

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
	rootCmd.AddCommand(loginCmd)
}

// withClaudeConfigDir returns env with CLAUDE_CONFIG_DIR replaced (or appended)
// to point at sessionDir. Other vars are preserved.
func withClaudeConfigDir(env []string, sessionDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == shim.EnvClaudeCfgDir {
			continue
		}
		out = append(out, kv)
	}
	return append(out, shim.EnvClaudeCfgDir+"="+sessionDir)
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

// snapshotEmail extracts oauthAccount.emailAddress from a claudejson Snapshot.
// Returns "" if the field isn't there.
func snapshotEmail(s claudejson.Snapshot) string {
	if len(s.OAuthAccount) == 0 {
		return ""
	}
	var x struct {
		EmailAddress string `json:"emailAddress"`
	}
	_ = json.Unmarshal(s.OAuthAccount, &x)
	return x.EmailAddress
}
