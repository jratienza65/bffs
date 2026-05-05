package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/shim"
)

var execCmd = &cobra.Command{
	Use:                "exec [-- <args...>]",
	Short:              "Resolve the active account and exec the real `claude` with the given args",
	Long:               `Same logic the installed shim uses; useful for testing without installing or for users who prefer an alias.`,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		exit, err := shim.Run(args)
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
		}
		os.Exit(exit)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(execCmd)
}
