package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

var showCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the account that would be used by `claude` from the current directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		r, err := resolver.Resolve(dir, cwd)
		if err != nil {
			return err
		}
		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "config-dir: %s\n", dir)
		fmt.Fprintf(out, "cwd:        %s\n", cwd)
		fmt.Fprintf(out, "source:     %s\n", r.Source)
		if r.Source == resolver.SourceNone {
			fmt.Fprintln(out, "account:    (none — `claude` will use its own credentials)")
			return nil
		}
		fmt.Fprintf(out, "account:    %s\n", r.Account.Name)
		fmt.Fprintf(out, "type:       %s\n", r.Account.Type)
		if r.Account.Email != "" {
			fmt.Fprintf(out, "email:      %s\n", r.Account.Email)
		}
		if r.Account.Type == store.TypeAPIKey {
			fmt.Fprintf(out, "secret:     %s\n", maskSecret(r.Account.Secret))
		}
		if r.Account.Type == store.TypeOAuth {
			fmt.Fprintf(out, "session:    %s\n", sessions.Dir(dir, r.Account.Name))
			fmt.Fprintf(out, "isolation:  %s\n", store.ResolveIsolation(r.Account.Isolation, state.Isolation))
		}
		if r.ProjectFile != "" {
			fmt.Fprintf(out, "project:    %s\n", r.ProjectFile)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(showCmd)
}
