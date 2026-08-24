package cmd

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jratienza65/bffs/internal/shimcheck"
)

var (
	initInstallDir string
	initForce      bool
	initAuto       bool
)

// EnvShimDir lets a user pin the shim install directory persistently so
// they don't have to pass --dir every time. --dir still wins.
const EnvShimDir = shimcheck.EnvShimDir

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Install the `claude` shim so any `claude` invocation honors bffs",
	Long: `Creates a small ` + "`claude`" + ` shim in an install directory and prints the
shell snippet needed to put that directory at the front of your PATH.

The shim simply re-execs bffs in shim mode, which resolves the
account (env > project > global) and execs the real ` + "`claude`" + ` binary
found later on PATH.

Install directory resolution (highest priority wins):
  1. --dir flag
  2. $BFFS_SHIM_DIR env var
  3. Per-OS default — ~/.bffs/bin (macOS/Linux) or %LOCALAPPDATA%\bffs\bin
     (Windows). The default is intentionally a dedicated bffs dir, not a
     shared location like ~/.local/bin, so the shim won't collide with
     Claude Code's own install script (which also targets ~/.local/bin).

Before installing, init checks whether the shim would actually be the
` + "`claude`" + ` that runs. Which directories are on PATH depends on how a shell
was started — a shim can win in your terminal and lose in every other
context (IDE integrations, launchd/systemd, cron, ` + "`ssh host 'cmd'`" + `), because
those read different startup files. init probes each invocation mode and
refuses to install a shim that would silently never run.

On a terminal it prompts for the install directory, offering any directory
already early enough on PATH in every mode. Pass --auto to take the per-OS
default without prompting; it still verifies. With no terminal (CI, dotfile
bootstrap) it behaves as --auto rather than hanging on a prompt.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
		self, _ = filepath.EvalSymlinks(self)

		out := cmd.OutOrStdout()
		// --dir is an explicit choice, so it skips the prompt but not the
		// verification. --auto and a non-TTY both mean "don't ask".
		interactive := isTTY() && !initAuto && initInstallDir == ""

		dir, err := pickInstallDir()
		if err != nil {
			return err
		}
		// One prompter for the whole run: a per-prompt reader would swallow
		// the remaining input on the first read.
		pr := newPrompter(cmd.InOrStdin(), out)

		// Probe before deciding: the same report drives the interactive menu
		// and the non-interactive refusal, so the two can never disagree.
		rep := probeInstallDir(dir, self)
		if interactive {
			dir, err = chooseInstallDir(pr, cmd, dir, rep, self)
			if err != nil {
				return err
			}
			rep = probeInstallDir(dir, self)
		}
		if err := confirmOrRefuse(pr, cmd, rep, interactive); err != nil {
			return err
		}

		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		shimPath := filepath.Join(dir, claudeShimName())
		if err := refuseIfOccupied(shimPath, self); err != nil {
			return err
		}
		if err := installShim(self, shimPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nInstalled shim at %s\n\n", shimPath)

		// Verify by measurement, not prediction. Whether a given rc file
		// edit works depends on what *else* touches PATH afterwards, which
		// cannot be reasoned about statically — so re-probe and report what
		// the shells actually resolve now.
		reportOutcome(out, dir, probeInstallDir(dir, self))

		fmt.Fprintln(out, "\nOptional: let Claude Code inspect and switch bffs accounts itself:  bffs mcp install")
		return nil
	},
}

func init() {
	initCmd.Flags().StringVar(&initInstallDir, "dir", "", "directory to install the shim into (overrides $"+EnvShimDir+" and per-OS default)")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite an existing shim, and install even if the shim would not win on PATH")
	initCmd.Flags().BoolVar(&initAuto, "auto", false, "don't prompt; use the per-OS default install dir (still verifies the shim would actually run)")
	rootCmd.AddCommand(initCmd)
}

func pickInstallDir() (string, error) {
	if initInstallDir != "" {
		return initInstallDir, nil
	}
	return shimcheck.DefaultInstallDir()
}

func claudeShimName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// probeInstallDir asks, for each shell invocation mode, whether a bffs shim in
// dir is the `claude` that would actually run.
func probeInstallDir(dir, self string) shimcheck.Report {
	return shimcheck.Check(context.Background(), shimcheck.Options{
		InstallDir: dir,
		ShimName:   claudeShimName(),
		SelfPath:   self,
	})
}

// refuseIfOccupied guards the install path itself. Overwriting whatever is
// already there is a separate question from whether the shim would win, and
// deserves its own explicit --force.
func refuseIfOccupied(shimPath, self string) error {
	if _, err := os.Lstat(shimPath); err != nil {
		return nil
	}
	if initForce || isSelfShim(shimPath, self) {
		return nil
	}
	return fmt.Errorf("%s already exists; pass --force to overwrite", shimPath)
}

// chooseInstallDir runs the interactive directory picker. Options are drawn
// from the probe: any directory already early enough on PATH in every mode is
// offered first, because choosing one makes the shim work with no PATH edit
// at all.
func chooseInstallDir(pr *prompter, cmd *cobra.Command, defaultDir string, rep shimcheck.Report, self string) (string, error) {
	out := cmd.OutOrStdout()
	rep.Render(out)
	fmt.Fprintln(out)

	type option struct {
		dir  string
		note string
	}
	opts := []option{{dir: defaultDir, note: "dedicated bffs dir — needs a PATH entry"}}
	for _, d := range rep.SafeDirs(self) {
		if pathsEqual(d, defaultDir) {
			continue
		}
		opts = append(opts, option{dir: d, note: "already early enough on PATH in every mode — no PATH edit needed"})
		if len(opts) == 4 {
			break
		}
	}

	fmt.Fprintln(out, "Where should the shim go?")
	for i, o := range opts {
		marker := " "
		if i == 0 {
			marker = "*"
		}
		fmt.Fprintf(out, " %s %d) %s\n      %s\n", marker, i+1, o.dir, o.note)
	}
	fmt.Fprintf(out, "   %d) enter a custom path\n\n", len(opts)+1)

	answer, err := pr.line(fmt.Sprintf("Choice [1-%d, default 1]: ", len(opts)+1))
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return opts[0].dir, nil
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n < 1 || n > len(opts)+1 {
		return "", fmt.Errorf("invalid choice %q", answer)
	}
	if n <= len(opts) {
		return opts[n-1].dir, nil
	}
	custom, err := pr.line("Path: ")
	if err != nil {
		return "", err
	}
	custom = strings.TrimSpace(custom)
	if custom == "" {
		return "", fmt.Errorf("no path entered")
	}
	return expandHome(custom)
}

// expandHome resolves a leading ~ and makes the path absolute, so a shim dir
// typed as "~/bin" is stored as a real path rather than a literal tilde that
// no shell would ever expand.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return filepath.Abs(p)
}

// confirmOrRefuse decides whether to proceed when the probe says the shim
// would not actually run. Non-interactive callers get a hard failure rather
// than a silently useless install — that silence is the whole bug class this
// check exists to close.
func confirmOrRefuse(pr *prompter, cmd *cobra.Command, rep shimcheck.Report, interactive bool) error {
	if !rep.Blocked() || initForce {
		if rep.Blocked() && initForce {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: --force set; installing a shim that will not run in every mode")
		}
		return nil
	}
	out := cmd.OutOrStdout()
	if interactive {
		rep.Render(out)
		fmt.Fprintln(out)
		answer, err := pr.line("The shim will not run in every mode. Install anyway? [y/N]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		}
		return fmt.Errorf("aborted")
	}

	var b strings.Builder
	rep.Render(&b)
	return fmt.Errorf(`the shim would not be the %q that runs:

%s
Fix PATH so %s comes first, then re-run, or pass --force to install anyway`,
		rep.ShimName, b.String(), rep.InstallDir)
}

// reportOutcome states the measured post-install result.
func reportOutcome(out io.Writer, dir string, rep shimcheck.Report) {
	if rep.AllWin() {
		fmt.Fprintf(out, "Verified: %q resolves to the shim in every probed mode.\n", rep.ShimName)
		return
	}
	rep.Render(out)
	fmt.Fprintln(out)
	printPathHint(out, dir)
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

// shellSingleQuote wraps s in '...' and escapes any embedded single quote by
// closing the quote, emitting an escaped quote, and reopening — the standard
// shell trick:
//
//	'\''
//
// The result is a single shell word that undergoes no expansion of any kind.
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
