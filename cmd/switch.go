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

Project ` + "`bffs.toml`" + ` files still override this default within their tree.`,
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
		fmt.Fprintf(cmd.OutOrStdout(), "Global default account set to %q.\n", name)
		fmt.Fprintln(cmd.OutOrStdout(), "(project bffs.toml files still override this within their tree)")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(switchCmd)
}
