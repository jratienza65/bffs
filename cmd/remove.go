package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

var removeYes bool

var removeCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm"},
	Short:   "Remove an account",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		if _, ok := accs.Accounts[name]; !ok {
			return fmt.Errorf("unknown account %q", name)
		}
		if !removeYes {
			ans, err := promptLine(os.Stdin, cmd.OutOrStdout(), fmt.Sprintf("Remove account %q? [y/N] ", name))
			if err != nil {
				return err
			}
			if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
				fmt.Fprintln(cmd.OutOrStdout(), "aborted")
				return nil
			}
		}
		acc := accs.Accounts[name]
		delete(accs.Accounts, name)
		if err := store.SaveAccounts(dir, accs); err != nil {
			return err
		}
		// For oauth accounts the session dir is removed too — it holds
		// .claude.json and possibly history/projects/todos. The Keychain
		// entry is owned by Claude Code; the user can clear it manually with
		// `security delete-generic-password` if they want.
		if acc.Type == store.TypeOAuth {
			sd := sessions.Dir(dir, name)
			if err := os.RemoveAll(sd); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove session dir %s: %v\n", sd, err)
			}
		}
		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}
		if state.Active == name {
			state.Active = ""
			if err := store.SaveState(dir, state); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed account %q (was active; global default cleared)\n", name)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed account %q\n", name)
		return nil
	},
}

func init() {
	removeCmd.Flags().BoolVarP(&removeYes, "yes", "y", false, "skip confirmation")
	rootCmd.AddCommand(removeCmd)
}
