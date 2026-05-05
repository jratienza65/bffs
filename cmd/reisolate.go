package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

var (
	reisolatePreset string
	reisolateReset  bool
	reisolateYes    bool
)

var reisolateCmd = &cobra.Command{
	Use:   "reisolate <name>",
	Short: "Recompute the per-account session-dir symlinks for an oauth account",
	Long: `Reconciles <bffs-config>/sessions/<name>/ to match the chosen isolation
preset. Use this after changing the global default or when you want to
move an account between presets without re-doing the login flow.

By default, real files in the session dir (e.g. claude's own writes from a
previous isolation preset) are preserved — only symlinks bffs manages are
added or removed. Pass --reset to also delete those real files so the
desired symlinks can replace them.

If --preset is given, it is also persisted on the account so it sticks
across future invocations. If omitted, the existing per-account preset
is used (falling back to the global default in state.toml, then "partial").`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		dir := mustConfigDir(cmd)
		accs, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		acc, ok := accs.Accounts[name]
		if !ok {
			return fmt.Errorf("unknown account %q; known: %v", name, accs.Names())
		}
		if acc.Type != store.TypeOAuth {
			return fmt.Errorf("reisolate only applies to oauth accounts; %q is %s", name, acc.Type)
		}
		state, err := store.LoadState(dir)
		if err != nil {
			return err
		}

		if reisolatePreset != "" {
			p := store.IsolationPreset(reisolatePreset)
			if !p.Valid() {
				return fmt.Errorf(`invalid --preset %q: must be "partial" or "full"`, reisolatePreset)
			}
			acc.Isolation = p
			accs.Accounts[name] = acc
			if err := store.SaveAccounts(dir, accs); err != nil {
				return err
			}
		}

		effective := store.ResolveIsolation(acc.Isolation, state.Isolation)
		homeClaude, err := defaultHomeClaudeDir()
		if err != nil {
			return fmt.Errorf("locate ~/.claude: %w", err)
		}
		sessionDir := sessions.Dir(dir, name)
		out := cmd.OutOrStdout()

		skipped, err := sessions.SyncSymlinks(sessionDir, homeClaude, effective)
		if err != nil {
			return fmt.Errorf("sync %s: %w", sessionDir, err)
		}

		if reisolateReset && len(skipped) > 0 {
			if err := resetConflicts(cmd, sessionDir, skipped); err != nil {
				return err
			}
			// Re-run sync now that the conflicts are gone.
			skipped, err = sessions.SyncSymlinks(sessionDir, homeClaude, effective)
			if err != nil {
				return fmt.Errorf("re-sync after reset: %w", err)
			}
		}

		for _, s := range skipped {
			fmt.Fprintf(out, "warning: %s exists in %s as a real file; left untouched (pass --reset to replace it with the symlink)\n", s, sessionDir)
		}
		fmt.Fprintf(out, "Reisolated %q (preset=%s, dir=%s).\n", name, effective, sessionDir)
		return nil
	},
}

// resetConflicts deletes each conflicting per-account real file/dir so the
// next sync can put a symlink there. Prompts once with the full list unless
// --yes is set.
func resetConflicts(cmd *cobra.Command, sessionDir string, conflicts []string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "About to delete from the per-account session dir (cannot be undone):")
	for _, s := range conflicts {
		fmt.Fprintf(out, "  %s\n", filepath.Join(sessionDir, s))
	}
	if !reisolateYes {
		ans, err := promptLine(os.Stdin, out, "Proceed? [y/N] ")
		if err != nil {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return fmt.Errorf("aborted")
		}
	}
	for _, s := range conflicts {
		path := filepath.Join(sessionDir, s)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func init() {
	reisolateCmd.Flags().StringVar(&reisolatePreset, "preset", "", `isolation preset: "partial" or "full" (default: keep current)`)
	reisolateCmd.Flags().BoolVar(&reisolateReset, "reset", false, "delete per-account real files that conflict with desired symlinks (destructive)")
	reisolateCmd.Flags().BoolVarP(&reisolateYes, "yes", "y", false, "skip the --reset confirmation prompt")
	rootCmd.AddCommand(reisolateCmd)
}
