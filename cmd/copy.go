package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/transcripts"
)

var (
	copyFrom           string
	copyTo             string
	copyAllProjects    bool
	copyProjects       []string
	copySessions       []string
	copyOnly           string
	copyOnConflict     string
	copyMemory         string
	copyMove           bool
	copySetLastSession bool
	copyDryRun         bool
	copyYes            bool
	copyClaudeDir      string
)

var copyCmd = &cobra.Command{
	Use:   "copy --from <acct|home> --to <acct|home> (--all-projects | --project <dir>... | --session <sid>...)",
	Short: "Copy (or, with --move, move) sessions and auto-memories between bffs accounts on this machine",
	Long: `Copy sessions and auto-memory from one account's Claude config dir to another's.

Under the default partial isolation every oauth account shares one projects
pool with ~/.claude, so there is nothing to copy between two such accounts —
only their trust answers differ, and ` + "`bffs trust sync`" + ` carries those. A real
copy happens between different roots: into or out of a full-isolation
account, or from an orphan session dir (read-only source).

The copy runs through the same staged, verified pipeline as ` + "`bffs import`" + `.
--move deletes the originals only after every landed file was read back and
its digest matched, and never touches a session a running claude has open.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		req, err := newCopyRequest(cmd)
		if err != nil {
			return err
		}
		return runCopy(cmd, dir, newPrompter(os.Stdin, cmd.OutOrStdout()), req, isTTY())
	},
}

func init() {
	f := copyCmd.Flags()
	f.StringVar(&copyFrom, "from", "", `source account ("home" = ~/.claude; an orphan session dir by name)`)
	f.StringVar(&copyTo, "to", "", `destination account ("home" = ~/.claude)`)
	f.BoolVar(&copyAllProjects, "all-projects", false, "every project in the source root")
	f.StringArrayVar(&copyProjects, "project", nil, "project directory (repeatable)")
	f.StringArrayVar(&copySessions, "session", nil, "session id or unique prefix of at least 8 hex characters (repeatable)")
	f.StringVar(&copyOnly, "only", "", `restrict to one kind: "sessions" or "memories" (default: both)`)
	f.StringVar(&copyOnConflict, "on-conflict", "skip", `a session that already exists in the destination: "skip" or "overwrite" (the existing files are set aside, never deleted)`)
	f.StringVar(&copyMemory, "memory", "merge", `memory directories: "merge", "skip" or "overwrite"`)
	f.BoolVar(&copyMove, "move", false, "delete the originals after every landed file was verified (never for a session a running claude has open)")
	f.BoolVar(&copySetLastSession, "set-last-session", false, "point the destination account's lastSessionId at the newest copied session per project")
	f.BoolVar(&copyDryRun, "dry-run", false, "show the plan without writing")
	f.BoolVarP(&copyYes, "yes", "y", false, "skip the confirmation")
	f.StringVar(&copyClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = f.MarkHidden("claude-dir")
	rootCmd.AddCommand(copyCmd)
}

// copyRequest is one resolved `bffs copy` invocation.
type copyRequest struct {
	From, To       string
	AllProjects    bool
	Projects       []string
	Sessions       []string
	Only           string
	OnConflict     string
	Memory         string
	Move           bool
	SetLastSession bool
	DryRun         bool
	Yes            bool
	ClaudeDir      string
	Cwd            string
	Now            time.Time
}

func newCopyRequest(cmd *cobra.Command) (copyRequest, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return copyRequest{}, err
	}
	req := copyRequest{
		From: copyFrom, To: copyTo,
		AllProjects: copyAllProjects, Projects: copyProjects, Sessions: copySessions,
		Only: copyOnly, OnConflict: copyOnConflict, Memory: copyMemory,
		Move: copyMove, SetLastSession: copySetLastSession, DryRun: copyDryRun, Yes: copyYes,
		ClaudeDir: copyClaudeDir, Cwd: cwd, Now: time.Now(),
	}
	return req, req.validate()
}

func (r copyRequest) validate() error {
	if r.From == "" || r.To == "" {
		return errors.New("--from and --to are required")
	}
	if r.From == r.To {
		return fmt.Errorf("--from and --to name the same account %q", r.From)
	}
	if !r.AllProjects && len(r.Projects) == 0 && len(r.Sessions) == 0 {
		return errors.New("select what to copy: --all-projects, --project <dir> or --session <id>")
	}
	if r.AllProjects && (len(r.Projects) > 0 || len(r.Sessions) > 0) {
		return errors.New("--all-projects cannot be combined with --project or --session")
	}
	switch r.Only {
	case "", "sessions", "memories":
	default:
		return fmt.Errorf(`invalid --only %q: must be "sessions" or "memories"`, r.Only)
	}
	switch r.OnConflict {
	case string(porter.ConflictSkip), string(porter.ConflictOverwrite):
	default:
		return fmt.Errorf(`invalid --on-conflict %q: must be "skip" or "overwrite"`, r.OnConflict)
	}
	switch rehome.MemoryMode(r.Memory) {
	case rehome.MemoryMerge, rehome.MemorySkip, rehome.MemoryOverwrite:
	default:
		return fmt.Errorf(`invalid --memory %q: must be "merge", "skip" or "overwrite"`, r.Memory)
	}
	return nil
}

// runCopy resolves both roots, answers "nothing to copy" for a shared pool,
// otherwise selects, plans, confirms and runs porter.CopyLocal.
func runCopy(cmd *cobra.Command, dir string, pr *prompter, req copyRequest, tty bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	env, err := loadCatalogEnv(dir, req.ClaudeDir)
	if err != nil {
		return err
	}
	src, err := env.rootForAccount(req.From)
	if err != nil {
		return err
	}
	dst, err := env.rootForAccount(req.To)
	if err != nil {
		return err
	}
	if dst.Orphan {
		return fmt.Errorf("orphan session dir %q is read-only (no account in accounts.toml uses it); it cannot be a --to", req.To)
	}
	projectLabel := req.Cwd
	if len(req.Projects) > 0 {
		projectLabel = req.Projects[0]
	}
	if porter.SameRoot(src, dst) {
		renderNothingToCopy(out, req.From, req.To, projectLabel)
		return nil
	}

	ctx := cmdContext(cmd)
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		if req.Move {
			return fmt.Errorf("cannot tell which sessions are open in a running claude: %w; nothing was moved", err)
		}
		fmt.Fprintln(errOut, "warning: liveness unavailable:", err)
	}
	selOpts := porter.SelectOptions{
		AllProjects: req.AllProjects,
		Projects:    req.Projects,
		Sessions:    req.Sessions,
		Only:        req.Only,
		IncludeLive: true,
		Now:         req.Now,
	}
	sel, err := porter.Select(ctx, src, nil, live, selOpts)
	if err != nil {
		return err
	}
	sel.Parts = porter.DefaultParts

	// A move never touches a session a running claude has open; those are
	// held back and reported. A copy reads them as they are (the transcript
	// may be truncated) exactly like an export does.
	var held []transcripts.Session
	if req.Move {
		kept := sel.Sessions[:0:0]
		for _, s := range sel.Sessions {
			if s.Live {
				held = append(held, s)
				continue
			}
			kept = append(kept, s)
		}
		sel.Sessions = kept
	}
	if len(sel.Sessions) == 0 && len(sel.Memories) == 0 {
		if len(held) > 0 {
			renderHeld(out, held, live)
			return exitWith(1, errors.New("every selected session is open in a running claude; nothing was moved"))
		}
		return errors.New("nothing selected: no sessions or memory dirs matched")
	}

	fmt.Fprintf(out, "plan: %s, %s  from %s  to  %s\n",
		countNoun(len(sel.Sessions), "session"), countNoun(len(sel.Memories), "memory dir"),
		short(src.Dir), short(dst.Dir))
	renderHeld(out, held, live)
	if req.DryRun {
		fmt.Fprintln(out, "dry run: nothing written")
		return nil
	}

	verb := "copy"
	prompt := fmt.Sprintf("copy %s? [y/N] ", countNoun(len(sel.Sessions), "session"))
	if req.Move {
		verb = "move"
		prompt = fmt.Sprintf("move %s and delete the originals after verification? [y/N] ", countNoun(len(sel.Sessions), "session"))
	}
	ok, err := confirmOrAbort(pr, prompt, req.Yes, tty)
	if err != nil {
		return fmt.Errorf("refusing to %s: %w", verb, err)
	}
	if !ok {
		return nil
	}
	if req.Move && !req.Yes && len(sel.Sessions) > 1 {
		if err := confirmCount(pr, len(sel.Sessions)); err != nil {
			return err
		}
	}

	opts := porter.ImportOptions{
		Dest:           dst,
		OnConflict:     porter.ConflictPolicy(req.OnConflict),
		Memory:         rehome.MemoryMode(req.Memory),
		SetLastSession: req.SetLastSession,
		Limits:         bundle.DefaultLimits,
		Now:            req.Now,
		Live:           live,
		LaunchEnv:      os.Environ(),
	}
	if req.To != transcripts.HomeName {
		opts.Account = req.To
	}
	rep, err := porter.CopyLocal(ctx, dir, sel, opts, req.Move)
	for _, w := range rep.Warnings {
		fmt.Fprintln(errOut, "warning:", transcripts.Sanitize(w))
	}
	if err != nil {
		if len(rep.Imported)+len(rep.Pending)+len(rep.MemoryDirs) > 0 {
			fmt.Fprintf(out, "%s stopped; what landed before the failure:\n", verb)
			renderCopyReceipt(out, rep, false)
		}
		if rep.StagingDir != "" {
			fmt.Fprintf(errOut, "staging kept at %s (inspect it, then rerun bffs import --clean-staging)\n", rep.StagingDir)
		}
		return exitWith(1, err)
	}
	renderCopyReceipt(out, rep, req.Move)
	return nil
}

// renderNothingToCopy is the answer for two accounts on one pool (plan §5.10).
func renderNothingToCopy(w io.Writer, from, to, project string) {
	fmt.Fprintf(w, "nothing to copy: %q and %q share one projects pool (partial isolation) — the transcripts and the memory\n", from, to)
	fmt.Fprintf(w, "for %s are already the same files. What differs per account is trust:\n", short(project))
	fmt.Fprintf(w, "    bffs trust sync --from %s --to %s --project %s\n", shellWord(from), shellWord(to), shellWord(project))
}

// renderHeld lists the live sessions a move leaves alone.
func renderHeld(w io.Writer, held []transcripts.Session, live map[string]transcripts.LiveSession) {
	for _, s := range held {
		pid := ""
		if ls, ok := live[s.ID]; ok {
			pid = fmt.Sprintf(" (pid %d)", ls.PID)
		}
		fmt.Fprintf(w, "held (live, skipped): %s%s\n", s.ID, pid)
	}
}

// confirmCount makes a multi-session deletion deliberate: the user types
// the number of sessions about to lose their originals.
func confirmCount(pr *prompter, n int) error {
	ans, err := pr.line(fmt.Sprintf("type %d to confirm: ", n))
	if err != nil {
		return err
	}
	if got, err := strconv.Atoi(strings.TrimSpace(ans)); err != nil || got != n {
		fmt.Fprintln(pr.out, "aborted")
		return errors.New("aborted")
	}
	return nil
}

// renderCopyReceipt prints what landed and how to check it.
func renderCopyReceipt(w io.Writer, rep porter.Report, moved bool) {
	n := len(rep.Imported) + len(rep.Pending)
	switch {
	case moved:
		fmt.Fprintf(w, "verified %s (sha256) — originals removed\n", countNoun(n, "session"))
	default:
		fmt.Fprintf(w, "copied %s", countNoun(n, "session"))
		if len(rep.MemoryDirs) > 0 {
			fmt.Fprintf(w, ", %s", countNoun(len(rep.MemoryDirs), "memory dir"))
		}
		fmt.Fprintln(w)
	}
	if len(rep.Skipped) > 0 {
		fmt.Fprintf(w, "skipped %s already in the destination (--on-conflict overwrite replaces them)\n", countNoun(len(rep.Skipped), "session"))
	}
	for _, sid := range rep.Held {
		fmt.Fprintf(w, "held %s: %s\n", short8(sid), transcripts.Sanitize(rep.Reasons[sid]))
	}
	if len(rep.MtimeRaised) > 0 {
		fmt.Fprintf(w, "%s raised to the retention floor", countNoun(len(rep.MtimeRaised), "mtime"))
		if rep.SweptCount > 0 && !rep.SweepDate.IsZero() {
			fmt.Fprintf(w, "; %s will be swept by Claude on %s unless resumed", countNoun(rep.SweptCount, "session"), rep.SweepDate.Format("2006-01-02"))
		}
		fmt.Fprintln(w)
	}
	for _, v := range rep.Verify {
		fmt.Fprintf(w, "verify:  %s\n", transcripts.Sanitize(v))
	}
}

// copyContext is a small seam so tests can cancel a copy midway.
var copyContext = func(cmd *cobra.Command) context.Context { return cmdContext(cmd) }
