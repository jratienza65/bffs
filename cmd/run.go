package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/runner"
	"github.com/jratienza65/bffs/internal/store"
)

var (
	runDir     string
	runTimeout time.Duration
)

var runCmd = &cobra.Command{
	Use:   "run <account> [--dir <d>] [--timeout <dur>] [-- <claude args...>]",
	Short: "Run claude as a child process on a specific account and wait",
	Long: `Spawns the real claude on a *named* account and waits for it to finish,
propagating its exit code. Contrast with ` + "`bffs exec`" + `, which resolves the
account by the usual precedence and *replaces* the bffs process.

With no trailing args this opens an interactive claude on that account:

  bffs run work

Everything after ` + "`--`" + ` is passed to claude verbatim, so headless delegated
runs work too:

  bffs run work -- -p "summarize this repo" --output-format json

The child is a fresh top-level session: session markers inherited from any
hosting claude are stripped, and the run is recorded in the launch log for
` + "`bffs usage`" + ` attribution (opt out with $BFFS_NO_USAGE_LOG).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		account, claudeArgs, err := parseRunArgs(args, cmd.ArgsLenAtDash())
		if err != nil {
			return err
		}
		cfgDir := mustConfigDir(cmd)
		dir := runDir
		if dir == "" {
			if dir, err = os.Getwd(); err != nil {
				return err
			}
		}
		if dir, err = store.NormalizePath(dir); err != nil {
			return err
		}

		// The child shares the terminal's process group and handles SIGINT
		// itself; bffs must survive the signal to propagate the exit code.
		signal.Ignore(os.Interrupt)

		exit, err := runner.Run(cmd.Context(), cfgDir, runner.Request{
			Account: account,
			Dir:     dir,
			Args:    claudeArgs,
			Timeout: runTimeout,
			// Real stdio (not cmd.OutOrStdout()) so the child's tty
			// detection works — same as the login flow.
			Stdin:  os.Stdin,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		})
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
		}
		if exit < 0 {
			exit = 1
		}
		os.Exit(exit)
		return nil
	},
}

func init() {
	runCmd.Flags().StringVar(&runDir, "dir", "", "working directory for the child (default: current directory)")
	runCmd.Flags().DurationVar(&runTimeout, "timeout", 0, "kill the run after this duration (default: none)")
	rootCmd.AddCommand(runCmd)
}

// parseRunArgs splits cobra's args into the account name and the claude argv
// tail. dashAt is cmd.ArgsLenAtDash(): the index in args where `--` sat, or
// -1 if absent. The account must come before the dash.
func parseRunArgs(args []string, dashAt int) (account string, claudeArgs []string, err error) {
	if dashAt == 0 {
		return "", nil, fmt.Errorf("account name must come before `--`: bffs run <account> -- <claude args...>")
	}
	if dashAt > 1 || (dashAt == -1 && len(args) > 1) {
		return "", nil, fmt.Errorf("unexpected argument %q; claude args go after `--`", args[1])
	}
	return args[0], args[1:], nil
}
