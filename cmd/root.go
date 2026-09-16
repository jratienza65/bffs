package cmd

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/jratienza65/bffs/internal/store"
)

const cfgDirFlag = "config-dir"

// Version is the version bffs reports: `bffs --version`, the MCP
// registration, the skill frontmatter and the browser's header. A
// release stamps it at link time,
//
//	go build -ldflags "-X github.com/jratienza65/bffs/cmd.Version=<v>"
//
// which goreleaser and the Makefile both do. Left unstamped — `go
// install`, a plain `go build`, `go run` — init fills it in from the
// build info, so a binary from `go install …@v0.3.0` says 0.3.0 rather
// than whatever literal the source happens to carry. It is never a
// hardcoded number: one that outlives its release is worse than none.
var Version = ""

// devVersion is what a build with nothing to go on calls itself.
const devVersion = "dev"

// versionString picks what to report: the link-time stamp, else the
// module version a `go install <module>@<version>` build records, else
// the commit a VCS-stamped build came from.
func versionString(stamp string, bi *debug.BuildInfo, ok bool) string {
	if stamp != "" {
		return stamp
	}
	if !ok {
		return devVersion
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return devVersion
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += ".dirty"
	}
	return devVersion + "+" + rev
}

var rootCmd = &cobra.Command{
	Use:   "bffs",
	Short: "Manage multiple Claude Code accounts and switch between them per-shell or per-project",
	Long: `bffs stores multiple Claude credentials under named accounts and
selects one for the next ` + "`claude`" + ` invocation, either globally or per-project
via a bffs.toml file in the project root.

Output: results go to stdout, everything a person reads while a command
runs — progress, plans awaiting a yes, prompts, warnings — goes to
stderr, so ` + "`bffs sessions list --json | jq`" + ` and ` + "`bffs export --out - | ssh …`" + `
carry data and nothing else.

Exit codes:
  0    the command did what it was asked
  1    it failed
  2    invalid usage: an unknown flag, a bad or missing argument
  75   safe to retry, nothing was written (a wrong pairing code)
  130  interrupted (Ctrl-C)`,
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
// code: 2 for a usage error, 1 for any other failure, or the code carried
// by an exitError (exitWith) — see cmd/exit.go for the table. Cobra has already printed
// "Error: <msg>" to stderr by the time an error reaches here (SilenceErrors
// is off; it also prints the `Run 'bffs --help'` hint for an unknown
// subcommand), so nothing is printed a second time.
func Execute() {
	// Tagging happens here rather than in an init: every subcommand has
	// been added to the tree by now, and a command's own init order is
	// not something the tree should depend on.
	tagUsageErrors(rootCmd)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

func init() {
	bi, ok := debug.ReadBuildInfo()
	Version = versionString(Version, bi, ok)
	rootCmd.Version = Version
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
