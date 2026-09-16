package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/jratienza65/bffs/internal/store"
)

const cfgDirFlag = "config-dir"

// Version is the bffs release version. Override at build time with
//
//	go build -ldflags "-X github.com/jratienza65/bffs/cmd.Version=<v>"
var Version = "0.1.0"

var rootCmd = &cobra.Command{
	Use:   "bffs",
	Short: "Manage multiple Claude Code accounts and switch between them per-shell or per-project",
	Long: `bffs stores multiple Claude credentials under named accounts and
selects one for the next ` + "`claude`" + ` invocation, either globally or per-project
via a bffs.toml file in the project root.`,
	Version: Version,
	// Runtime errors print a clean message; cobra's usage block only appears
	// for actual flag/argument parse errors.
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	// Bare `bffs` on a terminal is the interactive browser: accounts,
	// projects, sessions and memory in one place. Elsewhere (a pipe, a
	// bffs_notui build) it prints the help.
	RunE: func(cmd *cobra.Command, args []string) error {
		if tuiSupported() {
			if err := runTUI(cmdContext(cmd), mustConfigDir(cmd), "", "sessions"); !errors.Is(err, errTUIDisabled) {
				return err
			}
		}
		return cmd.Help()
	},
}

// Execute runs the cobra tree and ends the process with the error's exit
// code: 1 for any error, or the code carried by an exitError (exitWith) —
// see cmd/exit.go for the code conventions. Cobra has already printed
// "Error: <msg>" to stderr by the time an error reaches here (SilenceErrors
// is off; it also prints the `Run 'bffs --help'` hint for an unknown
// subcommand), so nothing is printed a second time.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

func init() {
	rootCmd.PersistentFlags().String(cfgDirFlag, "", "config dir (default: $BFFS_HOME or OS user-config)")
	_ = viper.BindPFlag(cfgDirFlag, rootCmd.PersistentFlags().Lookup(cfgDirFlag))
	viper.SetEnvPrefix("BFFS")
	viper.AutomaticEnv()
}

// configDir resolves the config dir, honoring (in order): --config-dir flag,
// $BFFS_HOME, the OS default.
func configDir() (string, error) {
	if v := viper.GetString(cfgDirFlag); v != "" {
		return v, nil
	}
	return store.ConfigDir()
}

func mustConfigDir(cmd *cobra.Command) string {
	dir, err := configDir()
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
		os.Exit(1)
	}
	return dir
}
