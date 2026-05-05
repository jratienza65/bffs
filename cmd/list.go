package cmd

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/store"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configured accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}
		if len(accs.Accounts) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No accounts configured. Try `bffs add <name>`.")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ACTIVE\tNAME\tTYPE\tEMAIL\tDETAIL")
		for _, name := range accs.Names() {
			acc := accs.Accounts[name]
			active := " "
			if name == state.Active {
				active = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", active, name, acc.Type, dashIfEmpty(acc.Email), accountDetail(acc, state.Isolation))
		}
		return w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func maskSecret(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}

// accountDetail renders the per-type "detail" column. For api_key it's the
// masked secret; for oauth it's the resolved isolation preset.
func accountDetail(acc store.Account, globalIsolation store.IsolationPreset) string {
	switch acc.Type {
	case store.TypeOAuth:
		return "isolation=" + string(store.ResolveIsolation(acc.Isolation, globalIsolation))
	default:
		return maskSecret(acc.Secret)
	}
}
