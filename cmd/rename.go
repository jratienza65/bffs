package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/store"
)

var renameForce bool

var renameCmd = &cobra.Command{
	Use:     "rename <old> <new>",
	Aliases: []string{"mv"},
	Short:   "Rename an account",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		oldName, newName := args[0], args[1]
		if oldName == newName {
			return fmt.Errorf("new name is the same as the old name")
		}
		if err := validateName(newName); err != nil {
			return err
		}
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		acc, ok := accs.Accounts[oldName]
		if !ok {
			return fmt.Errorf("unknown account %q; known: %v", oldName, accs.Names())
		}
		if _, exists := accs.Accounts[newName]; exists && !renameForce {
			return fmt.Errorf("account %q already exists (use --force to overwrite)", newName)
		}
		accs.Accounts[newName] = acc
		delete(accs.Accounts, oldName)
		if err := store.SaveAccounts(dir, accs); err != nil {
			return err
		}

		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}
		updatedActive := false
		if state.Active == oldName {
			state.Active = newName
			if err := store.SaveState(dir, state); err != nil {
				return err
			}
			updatedActive = true
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Renamed %q -> %q.\n", oldName, newName)
		if updatedActive {
			fmt.Fprintln(out, "(global default updated to follow the new name.)")
		}
		fmt.Fprintln(out, "Note: any bffs.toml referencing the old name will need a manual edit.")
		return nil
	},
}

func init() {
	renameCmd.Flags().BoolVar(&renameForce, "force", false, "overwrite an existing account at the new name")
	rootCmd.AddCommand(renameCmd)
}
