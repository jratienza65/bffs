package cmd

import (
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
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
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
