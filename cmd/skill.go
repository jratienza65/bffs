package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/skillpack"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

var (
	skillForce     bool
	skillClaudeDir string
)

// skillInstallHint is the one-line pointer `bffs init` and `bffs mcp install`
// end with.
const skillInstallHint = "Optional: bffs skill install (adds /bffs-rehome for imported sessions)"

// skillValidateTimeout bounds `claude plugin validate` after an install; the
// validator is a local file walk (under a second over a populated ~/.claude),
// so anything slower is a hung claude. A var so tests can shorten it.
var skillValidateTimeout = 20 * time.Second

// transferCodeEnv must never reach a child process: it is the LAN pairing
// secret `bffs import` reads.
const transferCodeEnv = "BFFS_TRANSFER_CODE"

var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Install the /bffs-rehome skill into Claude Code",
	Long: `Ships the bffs-rehome skill: a SKILL.md that teaches a Claude Code session
how to rehome sessions and auto-memory that bffs imported from another
machine or account root — discover what is pending, propose a directory per
old project, dry-run and apply ` + "`bffs rehome`" + `, then review memory and trust.

The skill pre-approves only read and plan commands; every write still goes
through Claude Code's permission prompt.`,
}

var skillInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install /bffs-rehome for Claude Code (user scope, plus every full-isolation account)",
	Long: `Writes the embedded skill to ~/.claude/skills/bffs-rehome/ and, for every
oauth account under full isolation, to that account's own session dir —
each full-isolation account reads its own skills/, while partial accounts
see the home copy through their skills/ symlink.

The frontmatter is stamped with this bffs version, so re-run install after
upgrading bffs. A same-named skill that bffs did not write is left alone
unless --force is given. When claude is available the installed skill is
checked with ` + "`claude plugin validate`" + ` and its findings are printed as
warnings; they never fail the install.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		accs, state, homeClaude, err := loadSkillEnv(dir)
		if err != nil {
			return err
		}
		written, err := skillpack.Install(homeClaude, dir, accs, state, Version, skillForce)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, d := range written {
			fmt.Fprintf(out, "installed %s\n", short(d))
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		warnings, _ := validateInstalledSkill(ctx, dir, homeClaude)
		for _, w := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, skillInstallMessage(len(written) > 1))
		return nil
	},
}

var skillUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the /bffs-rehome skill from Claude Code",
	Long: `Removes every bffs-rehome skill dir that bffs installed (home and
full-isolation session dirs). A same-named skill without the bffs marker was
written by you and is left in place.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		accs, state, homeClaude, err := loadSkillEnv(dir)
		if err != nil {
			return err
		}
		removed, err := skillpack.Uninstall(homeClaude, dir, accs, state)
		var notManaged *skillpack.NotManagedError
		if err != nil && !errors.As(err, &notManaged) {
			return err
		}
		out := cmd.OutOrStdout()
		for _, d := range removed {
			fmt.Fprintf(out, "removed %s\n", short(d))
		}
		if notManaged != nil {
			for _, p := range notManaged.Paths {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s was not installed by bffs; left in place\n", short(p))
			}
		}
		if len(removed) == 0 && notManaged == nil {
			fmt.Fprintf(out, "skill %q was not installed.\n", skillpack.SkillName)
		}
		return nil
	},
}

func init() {
	skillCmd.PersistentFlags().StringVar(&skillClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = skillCmd.PersistentFlags().MarkHidden("claude-dir")
	skillInstallCmd.Flags().BoolVar(&skillForce, "force", false, "overwrite a same-named skill that bffs did not install")
	skillCmd.AddCommand(skillInstallCmd, skillUninstallCmd)
	rootCmd.AddCommand(skillCmd)
}

// loadSkillEnv reads accounts.toml and state.toml under cfgDir and resolves
// the home claude dir (--claude-dir, else ~/.claude).
func loadSkillEnv(cfgDir string) (store.Accounts, store.State, string, error) {
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return store.Accounts{}, store.State{}, "", err
	}
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return store.Accounts{}, store.State{}, "", err
	}
	homeClaude := skillClaudeDir
	if homeClaude == "" {
		homeClaude, err = defaultHomeClaudeDir()
	} else {
		homeClaude, err = filepath.Abs(homeClaude)
	}
	if err != nil {
		return store.Accounts{}, store.State{}, "", err
	}
	return accs, state, homeClaude, nil
}

// skillInstallMessage is the closing line of `bffs skill install`. The
// parenthetical grows when a full-isolation account got its own copy.
func skillInstallMessage(fullCopies bool) string {
	scope := "user scope"
	if fullCopies {
		scope += "; full-isolation accounts got their own copy"
	}
	return fmt.Sprintf("Installed skill %q for Claude Code (%s). Restart claude; invoke with /%s, or just say \"rehome the sessions I imported\".",
		skillpack.SkillName, scope, skillpack.SkillName)
}

// validateInstalledSkill runs `claude plugin validate <homeClaudeDir> --json`
// over the freshly installed skill and returns the findings that concern it
// as warning lines. ran is false when the call was skipped: no claude could
// be found (shim.FindRealClaude) or it could not be started. The validator
// is pointed at the config dir rather than the skill dir because on a bare
// skill dir it looks for a plugin manifest and reports nothing else; given
// a dir that holds skills/ it validates the components in it, so findings
// about other files under that dir are filtered out here.
//
// Stdout and stderr are captured (never inherited), the environment loses
// the transfer code, and a hung validator is killed after
// skillValidateTimeout. Nothing here fails the install.
func validateInstalledSkill(ctx context.Context, cfgDir, homeClaudeDir string) (warnings []string, ran bool) {
	claude, err := shim.FindRealClaude(cfgDir)
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, skillValidateTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, claude, "plugin", "validate", homeClaudeDir, "--json")
	c.Stdout = &stdout
	c.Stderr = &stderr
	c.Env = envWithout(os.Environ(), transferCodeEnv)
	c.WaitDelay = 2 * time.Second
	runErr := c.Run()
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Killed by the timeout: say so rather than "signal: killed".
			// A `claude` that resolves to a bffs shim with a poisoned
			// real-claude.path cache execs itself forever and lands here.
			return []string{fmt.Sprintf("claude plugin validate: no result after %s (%s); the installed skill was not checked", skillValidateTimeout, short(claude))}, true
		}
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// Could not start at all (missing or unrunnable binary): skip
			// silently, like a missing claude.
			return nil, false
		}
	}
	warnings, ok := parsePluginValidate(stdout.Bytes(), skillpack.SkillDir(homeClaudeDir))
	if !ok && runErr != nil {
		msg := firstLine(stderr.String())
		if msg == "" {
			msg = firstLine(stdout.String())
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return []string{"claude plugin validate: " + transcripts.Sanitize(msg)}, true
	}
	return warnings, true
}

// pluginValidateReport mirrors the parts of `claude plugin validate --json`
// this command reads: the manifest block (null for a plain config dir) and
// one entry per component. Unknown fields are ignored so a newer claude
// cannot break the install.
type pluginValidateReport struct {
	Success  bool                  `json:"success"`
	Manifest *pluginValidateEntry  `json:"manifest"`
	Contents []pluginValidateEntry `json:"contents"`
}

type pluginValidateEntry struct {
	File     string                `json:"file"`
	Type     string                `json:"type"`
	Errors   []pluginValidateIssue `json:"errors"`
	Warnings []pluginValidateIssue `json:"warnings"`
}

type pluginValidateIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// parsePluginValidate extracts the findings about skillDir from a validate
// report. ok is false when raw is not a report.
func parsePluginValidate(raw []byte, skillDir string) (warnings []string, ok bool) {
	var rep pluginValidateReport
	if err := json.Unmarshal(bytes.TrimSpace(raw), &rep); err != nil {
		return nil, false
	}
	entries := rep.Contents
	if rep.Manifest != nil {
		entries = append(entries, *rep.Manifest)
	}
	skillsDir := filepath.Dir(skillDir)
	for _, e := range entries {
		label, relevant := "", false
		switch {
		case underDir(e.File, skillDir):
			relevant = true
			if rel, err := filepath.Rel(skillDir, e.File); err == nil && rel != "." {
				label = filepath.ToSlash(rel)
			} else {
				label = skillpack.SkillName
			}
		case sameDir(e.File, skillsDir):
			// Findings at the skills/ level mention the skill by name when
			// they are about it; everything else there is a neighbour's.
			label = skillpack.SkillsSubdir
			for _, is := range append(e.Errors, e.Warnings...) {
				if strings.Contains(is.Message, skillpack.SkillName) {
					relevant = true
				}
			}
		}
		if !relevant {
			continue
		}
		for _, is := range e.Errors {
			warnings = append(warnings, formatValidateIssue(label, is))
		}
		for _, is := range e.Warnings {
			if label == skillpack.SkillsSubdir && !strings.Contains(is.Message, skillpack.SkillName) {
				continue
			}
			warnings = append(warnings, formatValidateIssue(label, is))
		}
	}
	return warnings, true
}

// formatValidateIssue renders one finding as a warning line. The label,
// path and message come from claude's report (file names under skills/ and
// free text), so they pass through transcripts.Sanitize like every other
// rendered string that did not originate in bffs.
func formatValidateIssue(label string, is pluginValidateIssue) string {
	if is.Path != "" {
		return transcripts.Sanitize(fmt.Sprintf("claude plugin validate: %s: %s: %s", label, is.Path, is.Message))
	}
	return transcripts.Sanitize(fmt.Sprintf("claude plugin validate: %s: %s", label, is.Message))
}

// canonPath makes p comparable across the ways claude may print it: absolute,
// symlinks resolved when possible (/var vs /private/var on macOS), and case-
// folded on Windows.
func canonPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func sameDir(a, b string) bool {
	return canonPath(a) == canonPath(b)
}

// underDir reports whether p is dir itself or inside it.
func underDir(p, dir string) bool {
	rel, err := filepath.Rel(canonPath(dir), canonPath(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// envWithout returns env minus every entry whose key is one of keys.
func envWithout(env []string, keys ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, d := range keys {
			if strings.EqualFold(k, d) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
