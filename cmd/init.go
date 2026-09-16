package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var (
	initInstallDir string
	initForce      bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Install the `claude` shim so any `claude` invocation honors bffs",
	Long: `Creates a small ` + "`claude`" + ` shim in an install directory and prints the
shell snippet needed to put that directory at the front of your PATH.

The shim simply re-execs bffs in shim mode, which resolves the
account (env > project > global) and execs the real ` + "`claude`" + ` binary
found later on PATH.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
		self, _ = filepath.EvalSymlinks(self)

		dir, err := pickInstallDir()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}

		shimPath := filepath.Join(dir, claudeShimName())
		if err := refuseIfBlocked(shimPath, dir, self); err != nil {
			return err
		}
		if err := installShim(self, shimPath); err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Installed shim at %s\n\n", shimPath)
		printPathHint(out, dir)
		return nil
	},
}

func init() {
	initCmd.Flags().StringVar(&initInstallDir, "dir", "", "directory to install the shim into (default per-OS)")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite an existing shim at the install path")
	rootCmd.AddCommand(initCmd)
}

func pickInstallDir() (string, error) {
	if initInstallDir != "" {
		return initInstallDir, nil
	}
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA is not set; pass --dir to choose an install directory")
		}
		return filepath.Join(base, "bffs", "bin"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "bin"), nil
	}
}

func claudeShimName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

// refuseIfBlocked refuses to install if a different `claude` binary is already
// reachable earlier on PATH than our install dir, since the shim wouldn't win.
func refuseIfBlocked(shimPath, installDir, self string) error {
	if existing, err := os.Lstat(shimPath); err == nil {
		if !initForce {
			// If it already points at us (idempotent reinstall), allow.
			if isSelfShim(shimPath, self) {
				return nil
			}
			return fmt.Errorf("%s already exists; pass --force to overwrite", shimPath)
		}
		_ = existing
	}

	pathDirs := filepath.SplitList(os.Getenv("PATH"))
	target := claudeShimName()
	for _, d := range pathDirs {
		if d == "" {
			continue
		}
		if pathsEqual(d, installDir) {
			return nil
		}
		candidate := filepath.Join(d, target)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if isSelfShim(candidate, self) {
			continue
		}
		if !initForce {
			return fmt.Errorf(`an existing %q is earlier on PATH at:
    %s
The new shim would be installed at:
    %s
…and PATH would still resolve %q to the existing one, so the shim would do nothing.

Pick one:
  • Add %s to the FRONT of your PATH (recommended), then re-run `+"`bffs init`"+`.
  • Or run `+"`bffs init --force`"+` to install anyway and fix PATH later`,
				target, candidate, shimPath, target, installDir)
		}
		return nil
	}
	return nil
}

func isSelfShim(path, self string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return pathsEqual(resolved, self)
}

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func installShim(self, shimPath string) error {
	if _, err := os.Lstat(shimPath); err == nil {
		if err := os.Remove(shimPath); err != nil {
			return fmt.Errorf("remove existing %s: %w", shimPath, err)
		}
	}

	if runtime.GOOS == "windows" {
		return copyFile(self, shimPath, 0o755)
	}

	if err := os.Symlink(self, shimPath); err == nil {
		return nil
	}
	if err := os.Link(self, shimPath); err == nil {
		return nil
	}
	return os.WriteFile(shimPath, []byte(wrapperScript(self)), 0o755)
}

// wrapperScript returns a /bin/sh script that execs the given binary in
// shim-equivalent mode. `self` is shell-single-quoted so the shell treats
// it as a literal even when the path contains $, backtick, double-quote,
// or other metacharacters.
func wrapperScript(self string) string {
	return fmt.Sprintf("#!/bin/sh\nexec %s exec -- \"$@\"\n", shellSingleQuote(self))
}

// shellSingleQuote wraps s in '...' and escapes any embedded single quote
// using the standard '\'' trick. The result is a single shell word that
// undergoes no expansion of any kind.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func printPathHint(out io.Writer, dir string) {
	shell := strings.ToLower(filepath.Base(os.Getenv("SHELL")))
	fmt.Fprintln(out, "To make this shim take effect, ensure the install directory is at the FRONT of your PATH:")
	fmt.Fprintln(out)
	switch {
	case runtime.GOOS == "windows":
		fmt.Fprintln(out, `  PowerShell:`)
		fmt.Fprintf(out, "    $env:Path = '%s;' + $env:Path\n", dir)
		fmt.Fprintln(out, "  cmd.exe (persistent):")
		fmt.Fprintf(out, "    setx PATH \"%s;%%PATH%%\"\n", dir)
	case shell == "fish":
		fmt.Fprintln(out, "  ~/.config/fish/config.fish:")
		fmt.Fprintf(out, "    set -gx PATH %s $PATH\n", dir)
	case shell == "zsh":
		fmt.Fprintln(out, "  ~/.zshrc:")
		fmt.Fprintf(out, "    export PATH=\"%s:$PATH\"\n", dir)
	default:
		fmt.Fprintln(out, "  ~/.bashrc (or your shell's equivalent):")
		fmt.Fprintf(out, "    export PATH=\"%s:$PATH\"\n", dir)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Then open a new shell (or `source` your rc file) and run `which claude` to confirm.")
}
