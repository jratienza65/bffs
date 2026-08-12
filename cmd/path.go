package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/projectconfig"
	"github.com/jratienza65/bffs/internal/store"
)

var pathCmd = &cobra.Command{
	Use:   "path",
	Short: "Map directories to accounts without a bffs.toml in the project",
	Long: `Directory rules pin an account for a directory and everything under it,
with the mapping stored in your bffs config instead of a bffs.toml inside the
project.

Precedence: BFFS_ACCOUNT > bffs.toml > directory rule > global default.
The most specific (longest) matching rule wins.`,
}

var pathSetCmd = &cobra.Command{
	Use:   "set <account> [dir]",
	Short: "Pin an account for a directory and everything under it",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		account := args[0]

		target, err := targetDir(args, 1)
		if err != nil {
			return err
		}

		// Fail before writing: a rule naming an account that does not exist
		// would break every claude invocation under that directory.
		accounts, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		if _, ok := accounts.Get(account); !ok {
			return fmt.Errorf("unknown account %q; known accounts: %v", account, accounts.Names())
		}

		paths, err := store.LoadPaths(dir)
		if err != nil {
			return err
		}
		prev, err := paths.Set(target, account)
		if err != nil {
			return err
		}
		if err := store.SavePaths(dir, paths); err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		if prev != "" && prev != account {
			fmt.Fprintf(out, "%s -> %s (was %s)\n", short(target), account, prev)
		} else {
			fmt.Fprintf(out, "%s -> %s\n", short(target), account)
		}
		return nil
	},
}

var pathListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List directory rules",
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		paths, err := store.LoadPaths(dir)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(paths.Rules) == 0 {
			fmt.Fprintln(out, "No directory rules. Add one with `bffs path set <account> [dir]`.")
			return nil
		}

		cwd, _ := os.Getwd()
		active, hasActive := paths.Match(cwd)

		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "MATCH\tACCOUNT\tDIRECTORY")
		for _, r := range paths.Rules {
			marker := " "
			if hasActive && active.Path == r.Path {
				marker = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", marker, r.Account, short(r.Path))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if hasActive {
			fmt.Fprintf(out, "\n* applies to the current directory\n")
		}
		return nil
	},
}

var pathRemoveCmd = &cobra.Command{
	Use:     "remove [dir]",
	Aliases: []string{"rm"},
	Short:   "Remove the rule for a directory",
	Args:    cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		target, err := targetDir(args, 0)
		if err != nil {
			return err
		}

		paths, err := store.LoadPaths(dir)
		if err != nil {
			return err
		}
		removed, err := paths.Remove(target)
		if err != nil {
			return err
		}
		if !removed {
			// Distinguish "no rule here" from "a parent rule covers it" —
			// otherwise removing the wrong thing looks like a no-op.
			if rule, ok := paths.Match(target); ok {
				return fmt.Errorf("no rule for %s; it inherits %s from %s\nremove that instead: bffs path remove %s",
					short(target), rule.Account, short(rule.Path), short(rule.Path))
			}
			return fmt.Errorf("no rule for %s", short(target))
		}
		if err := store.SavePaths(dir, paths); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed rule for %s\n", short(target))
		return nil
	},
}

var pathImportCmd = &cobra.Command{
	Use:   "import <dir>...",
	Short: "Turn existing bffs.toml files into directory rules",
	Long: `Read each directory's bffs.toml and record the same mapping as a rule.

The now-redundant bffs.toml is printed, not deleted — it may be committed and
shared with other people.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		accounts, err := store.LoadAccounts(dir)
		if err != nil {
			return err
		}
		paths, err := store.LoadPaths(dir)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		var imported []string
		for _, a := range args {
			target, err := store.NormalizePath(a)
			if err != nil {
				return err
			}
			file := filepath.Join(target, "bffs.toml")
			cfg, err := readProjectAccount(file)
			if err != nil {
				fmt.Fprintf(out, "skip  %s (%v)\n", short(target), err)
				continue
			}
			if _, ok := accounts.Get(cfg); !ok {
				fmt.Fprintf(out, "skip  %s (unknown account %q)\n", short(target), cfg)
				continue
			}
			if _, err := paths.Set(target, cfg); err != nil {
				return err
			}
			imported = append(imported, file)
			fmt.Fprintf(out, "rule  %s -> %s\n", short(target), cfg)
		}

		if len(imported) == 0 {
			return nil
		}
		if err := store.SavePaths(dir, paths); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nRules saved. These files are now redundant — delete them yourself if they are not shared:\n")
		for _, f := range imported {
			fmt.Fprintf(out, "  rm %s\n", short(f))
		}
		return nil
	},
}

func init() {
	pathCmd.AddCommand(pathSetCmd, pathListCmd, pathRemoveCmd, pathImportCmd)
	rootCmd.AddCommand(pathCmd)
}

// targetDir returns args[i] if present, else the current directory.
func targetDir(args []string, i int) (string, error) {
	if len(args) > i && args[i] != "" {
		return store.NormalizePath(args[i])
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return store.NormalizePath(cwd)
}

// short renders a path relative to $HOME for readability.
func short(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

// readProjectAccount pulls the account name out of a specific bffs.toml.
// Deliberately reads the exact file rather than walking up like the resolver
// does — importing should record what THIS directory declares, not what it
// happens to inherit from a parent.
func readProjectAccount(file string) (string, error) {
	var cfg projectconfig.Config
	if _, err := toml.DecodeFile(file, &cfg); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no %s here", projectconfig.Filename)
		}
		return "", err
	}
	if cfg.Account == "" {
		return "", fmt.Errorf("%s sets no account", projectconfig.Filename)
	}
	return cfg.Account, nil
}
