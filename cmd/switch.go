package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/store"
)

var switchCmd = &cobra.Command{
	Use:   "switch <name>",
	Short: "Set the global default account",
	Long: `Sets the account that ` + "`claude`" + ` should use when no project file applies
and no BFFS_ACCOUNT env var is set.

Both account types are per-invocation — switch does no Keychain writes,
no ~/.claude.json patching, no global side effects beyond updating
` + "`state.toml`" + `. The shim picks up the new active account on the next
` + "`claude`" + ` invocation.

  - api_key:  shim sets ANTHROPIC_API_KEY.
  - oauth:    shim sets CLAUDE_CONFIG_DIR=<bffs-config>/sessions/<name>/.

Project ` + "`bffs.toml`" + ` files still override this default within their tree.

Claude Code records its folder-trust and external-imports answers per
account, so the account you switch to may be asked again for the current
directory. When another account already answered, switch says so;
--sync-trust carries the answers over right away (see ` + "`bffs trust`" + `).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		if _, ok := accs.Accounts[name]; !ok {
			return fmt.Errorf("unknown account %q; known: %v", name, accs.Names())
		}
		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}
		state.Active = name
		if err := store.SaveState(dir, state); err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Global default account set to %q.\n", name)
		fmt.Fprintln(out, "(project bffs.toml files still override this within their tree)")
		if switchSyncTrust {
			return switchTrustSync(cmd, dir, name)
		}
		// Best-effort note, two small JSON reads, never a write: errors
		// are swallowed so the switch itself always reports success.
		if isTTY() && trustHintEnabled(state) {
			if hint := trustHintForCwd(dir, accs, name); hint != "" {
				fmt.Fprint(out, hint)
			}
		}
		return nil
	},
}

var switchSyncTrust bool

func init() {
	switchCmd.Flags().BoolVar(&switchSyncTrust, "sync-trust", false, "carry the current directory's trust answers onto the account (asks first)")
	rootCmd.AddCommand(switchCmd)
}
