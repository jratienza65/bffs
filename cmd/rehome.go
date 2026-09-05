package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

var (
	rehomeBundle          string
	rehomeSessions        []string
	rehomeMaps            []string
	rehomeInto            string
	rehomeProject         string
	rehomeAccount         string
	rehomeMemory          string
	rehomeNoRewriteMemory bool
	rehomeSetLastSession  bool
	rehomeForceStamp      bool
	rehomeDryRun          bool
	rehomeYes             bool
	rehomeSuggest         bool
	rehomeClaudeDir       string
)

var rehomeCmd = &cobra.Command{
	Use:   "rehome (--map <old>=<new>... | --into <dir> --project <oldDir>) [--bundle <id>] [--session <id>...] [--dry-run] [-y]",
	Short: "Move sessions and memory of a directory that moved — or came from another machine — to where it lives now",
	Long: `A Claude Code session belongs to the directory it was started in: its transcript
lives under projects/<slug of that directory>/ and the picker only shows it
there. When the project moves (or arrives from another machine through
` + "`bffs import`" + ` as-is), the sessions stay behind under the old path. bffs rehome
does what Claude itself does when a directory moves: it renames the transcript
and its sidecar into the new directory's entry and appends one relocated
record — conversation content is never rewritten — then merges the project's
auto-memory into the new directory's memory (old paths rewritten, side files
for anything that differs) and prints the command to check the result.

--map OLD=NEW is a prefix rule, longest match first: --map /Users/jonas=/home/jonas
covers every project under the old home. --into <dir> --project <oldDir> is
the single-project spelling. --bundle <id> and --session narrow the scope to
one import record or to named sessions. Transcript mtimes are restored after
the move, so ` + "`claude --continue`" + ` and the picker order do not change; a session
open in a running claude is never moved. Nothing is deleted: the old memory
directory stays until you remove it.

The rehome works in the root claude uses from the current directory (or the
one --account names; "home" is ~/.claude). Exit status is 1 when nothing
could be moved and at least one session was refused; refusals beside a
successful move are listed and the status stays 0.

--suggest lists, per old directory of the import records, where the project
may live here (same git remote, same path relative to home, same folder
name) without changing anything.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		req := rehomeRequest{
			Bundle:          rehomeBundle,
			Sessions:        rehomeSessions,
			Maps:            rehomeMaps,
			Into:            rehomeInto,
			Project:         rehomeProject,
			Account:         rehomeAccount,
			Memory:          rehomeMemory,
			NoRewriteMemory: rehomeNoRewriteMemory,
			SetLastSession:  rehomeSetLastSession,
			ForceStamp:      rehomeForceStamp,
			DryRun:          rehomeDryRun,
			Yes:             rehomeYes,
			Suggest:         rehomeSuggest,
			ClaudeDir:       rehomeClaudeDir,
			Cwd:             cwd,
			Now:             time.Now(),
		}
		pr := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
		return runRehome(cmd, dir, pr, req, isTTY())
	},
}

func init() {
	f := rehomeCmd.Flags()
	f.StringVar(&rehomeBundle, "bundle", "", "only the sessions of this import record (bundle id or unique prefix; bffs sessions imports)")
	f.StringArrayVar(&rehomeSessions, "session", nil, "only these sessions (id or unique prefix of at least 8 characters; repeatable)")
	f.StringArrayVar(&rehomeMaps, "map", nil, "prefix rule OLD=NEW, longest match first (repeatable)")
	f.StringVar(&rehomeInto, "into", "", "the directory the project lives in now (single-project form; needs --project)")
	f.StringVar(&rehomeProject, "project", "", "with --into: the old directory the sessions were recorded under")
	f.StringVar(&rehomeAccount, "account", "", "root to rehome in (default: the account claude would use here; \"home\" = ~/.claude)")
	f.StringVar(&rehomeMemory, "memory", string(rehome.MemoryMerge), `what to do with the old memory directory: "merge" into the new one, "skip", or "overwrite" (existing set aside, never deleted)`)
	f.BoolVar(&rehomeNoRewriteMemory, "no-rewrite-memory", false, "keep old paths in the merged memory files as they are")
	f.BoolVar(&rehomeSetLastSession, "set-last-session", false, "point the account's lastSessionId for the new directory at the newest moved session (claude --continue)")
	f.BoolVar(&rehomeForceStamp, "force-stamp", false, "stamp a transcript whose last line is incomplete (a crashed session) instead of refusing it")
	f.BoolVar(&rehomeDryRun, "dry-run", false, "show the plan and write nothing")
	f.BoolVarP(&rehomeYes, "yes", "y", false, "skip the confirmation")
	f.BoolVar(&rehomeSuggest, "suggest", false, "list candidate directories per old directory of the import records and stop")
	f.StringVar(&rehomeClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = f.MarkHidden("claude-dir")
	rootCmd.AddCommand(rehomeCmd)
}

// rehomeRequest is one resolved `bffs rehome` invocation, independent of
// the flag variables so tests can drive runRehome directly.
type rehomeRequest struct {
	Bundle          string
	Sessions        []string
	Maps            []string
	Into            string
	Project         string
	Account         string
	Memory          string
	NoRewriteMemory bool
	SetLastSession  bool
	ForceStamp      bool
	DryRun          bool
	Yes             bool
	Suggest         bool
	ClaudeDir       string
	Cwd             string
	Now             time.Time
}

// parseRehomeMappings turns --map values and the --into/--project pair
// into prefix rules. At least one rule is required; --into without
// --project (or the reverse) is an error.
func parseRehomeMappings(maps []string, into, project string) ([]rehome.Mapping, error) {
	var out []rehome.Mapping
	for _, m := range maps {
		parsed, err := rehome.ParseMapping(m)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	switch {
	case into != "" && project == "":
		return nil, errors.New("--into needs --project <oldDir>: the directory the sessions were recorded under")
	case into == "" && project != "":
		return nil, errors.New("--project needs --into <dir>: where that project lives now")
	case into != "":
		parsed, err := rehome.ParseMapping(project + "=" + into)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	if len(out) == 0 {
		return nil, errors.New("nothing to map: pass --map OLD=NEW (repeatable) or --into <dir> --project <oldDir>; --suggest lists candidates")
	}
	return out, nil
}

// rehomeTarget is the root a rehome works in, the account whose
// .claude.json --set-last-session writes and whose name prefixes the
// verify line ("" for the unmanaged home), and that file.
type rehomeTarget struct {
	root       transcripts.Root
	account    string
	claudeJSON string
}

// label names the account side of the target for messages.
func (t rehomeTarget) label() string {
	if t.account == "" {
		return "the unmanaged home (~/.claude.json)"
	}
	return fmt.Sprintf("account %q", t.account)
}

// resolveRehomeTarget picks the root (A-7): the named account's (an
// api_key account works in ~/.claude; "home" is ~/.claude itself), else
// the root of the account resolver.Resolve picks for cwd, else the home
// root. A resolution failure is a warning, never an error.
func resolveRehomeTarget(dir string, env *catalogEnv, account, cwd string) (rehomeTarget, string, error) {
	home, err := env.homeRoot()
	if err != nil {
		return rehomeTarget{}, "", err
	}
	if account != "" {
		root, err := env.rootForAccount(account)
		if err != nil {
			return rehomeTarget{}, "", err
		}
		t := rehomeTarget{root: root, claudeJSON: root.ClaudeJSON}
		if acc, ok := env.accs.Get(account); ok {
			if acc.Type == store.TypeOAuth {
				t.account = account
				t.claudeJSON = filepath.Join(sessions.Dir(dir, account), claudejson.Filename)
			}
		} else if root.Owner != "" {
			t.account = root.Owner
		}
		return t, "", nil
	}
	fallback := rehomeTarget{root: home, claudeJSON: home.ClaudeJSON}
	r, err := resolver.Resolve(dir, cwd)
	if err != nil {
		return fallback, fmt.Sprintf("%v; using the home root", err), nil
	}
	if r.Source == resolver.SourceNone || r.Account.Type != store.TypeOAuth {
		return fallback, "", nil
	}
	root, err := transcripts.RootFor(env.roots, r.Account.Name)
	if err != nil {
		return fallback, fmt.Sprintf("%v; using the home root", err), nil
	}
	return rehomeTarget{root: root, account: r.Account.Name, claudeJSON: filepath.Join(sessions.Dir(dir, r.Account.Name), claudejson.Filename)}, "", nil
}

// rehomeAccountMismatch warns, per mapped directory, when claude launched
// there would run as another oauth account than the one this rehome
// addresses (a bffs.toml or path rule in the new place): lastSessionId and
// the verify line would then name the wrong account. Callers skip it when
// --account was given — the user has decided.
func rehomeAccountMismatch(cfgDir string, target rehomeTarget, moves []rehome.Move) []string {
	seen := map[string]bool{}
	var out []string
	for _, mv := range moves {
		if mv.NewCwd == "" || seen[mv.NewCwd] {
			continue
		}
		seen[mv.NewCwd] = true
		r, err := resolver.Resolve(cfgDir, mv.NewCwd)
		if err != nil || r.Source == resolver.SourceNone || r.Account.Type != store.TypeOAuth || r.Account.Name == target.account {
			continue
		}
		out = append(out, fmt.Sprintf("claude in %s runs as account %q (%s rule), not %s: pass --account %s so lastSessionId and the verify line address it",
			short(mv.NewCwd), r.Account.Name, r.Source, target.label(), r.Account.Name))
	}
	return out
}

// runRehome plans, shows, confirms and applies a rehome — or, with
// --suggest, only lists candidate directories. The exit status is 1 when
// no session could be moved and at least one was refused; refusals beside
// a move are listed and the status stays 0.
func runRehome(cmd *cobra.Command, dir string, pr *prompter, req rehomeRequest, tty bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if req.Suggest {
		return runRehomeSuggest(out, dir, req.Bundle)
	}
	maps, err := parseRehomeMappings(req.Maps, req.Into, req.Project)
	if err != nil {
		return err
	}
	memory := rehome.MemoryMode(req.Memory)
	switch memory {
	case "", rehome.MemoryMerge, rehome.MemorySkip, rehome.MemoryOverwrite:
	default:
		return fmt.Errorf("invalid --memory %q: must be %q, %q or %q", req.Memory, rehome.MemoryMerge, rehome.MemorySkip, rehome.MemoryOverwrite)
	}
	env, err := loadCatalogEnv(dir, req.ClaudeDir)
	if err != nil {
		return err
	}
	target, warning, err := resolveRehomeTarget(dir, env, req.Account, req.Cwd)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(errOut, "warning:", warning)
	}
	var oldHome, bundleID string
	if req.Bundle != "" {
		rec, err := rehome.FindRecord(dir, req.Bundle)
		if err != nil {
			return err
		}
		oldHome, bundleID = rec.Source.Home, rec.BundleID
	}
	newHome, _ := os.UserHomeDir()
	ctx := cmdContext(cmd)
	// A rehome moves transcripts; without knowing which sessions a
	// running claude owns nothing may move (plan §9.5).
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		return fmt.Errorf("cannot tell which sessions are open in a running claude: %w; nothing was moved", err)
	}
	days, _ := transcripts.CleanupPeriodDays(target.root.ConfigDir)
	opts := rehome.Options{
		Sessions:       req.Sessions,
		BundleID:       bundleID,
		RewriteMemory:  !req.NoRewriteMemory,
		SetLastSession: req.SetLastSession,
		ForceStamp:     req.ForceStamp,
		Memory:         memory,
		Mtime:          rehome.MtimePolicy{CleanupPeriodDays: days, Now: req.Now},
		ClaudeJSON:     target.claudeJSON,
		OldHome:        oldHome,
		NewHome:        newHome,
		Now:            req.Now,
		DryRun:         req.DryRun,
		CfgDir:         dir,
		StagingDir:     filepath.Join(dir, porter.StagingSubdir),
		Account:        target.account,
		Env:            os.Environ(),
	}
	plan, err := rehome.PlanRehome(ctx, target.root, live, maps, opts)
	if err != nil {
		return err
	}
	for _, w := range plan.Warnings {
		fmt.Fprintln(errOut, "warning:", transcripts.Sanitize(w))
	}
	if req.Account == "" {
		for _, w := range rehomeAccountMismatch(dir, target, plan.Moves) {
			fmt.Fprintln(errOut, "warning:", w)
		}
	}
	lastSessionFile := ""
	if req.SetLastSession {
		lastSessionFile = target.claudeJSON
	}
	renderRehomePlan(out, plan, maps, lastSessionFile)
	switch {
	case len(plan.Moves) == 0 && len(plan.Refusals) > 0:
		return exitWith(1, fmt.Errorf("nothing could be moved: %s refused (listed above)", countNoun(len(plan.Refusals), "session")))
	case len(plan.Moves) == 0 && len(plan.Memory) == 0:
		fmt.Fprintln(out, "nothing to rehome")
		return nil
	}
	if req.DryRun {
		res, err := rehome.Apply(ctx, target.root, plan, opts)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "dry run: nothing is written; after the real run, check it with:")
		renderVerifyLines(out, res.Verify)
		return nil
	}
	ok, err := confirmOrAbort(pr, fmt.Sprintf("Rehome %s? [y/N] ", rehomeCount(plan)), req.Yes, tty)
	if err != nil {
		return fmt.Errorf("refusing to rehome: %w", err)
	}
	if !ok {
		return nil
	}
	res, err := rehome.Apply(ctx, target.root, plan, opts)
	for _, w := range res.Warnings {
		fmt.Fprintln(errOut, "warning:", transcripts.Sanitize(w))
	}
	renderRehomeResult(out, res)
	if err != nil {
		return exitWith(1, err)
	}
	if len(res.Moved) == 0 && len(res.Refusals) > 0 {
		return exitWith(1, fmt.Errorf("nothing could be moved: %s refused (listed above)", countNoun(len(res.Refusals), "session")))
	}
	return nil
}

// runRehomeSuggest prints rehome.Suggest's candidates for every old
// directory of the import records (or of one bundle).
func runRehomeSuggest(out io.Writer, dir, bundle string) error {
	var recs []imports.Record
	if bundle != "" {
		rec, err := rehome.FindRecord(dir, bundle)
		if err != nil {
			return err
		}
		recs = []imports.Record{rec}
	} else {
		all, err := imports.Load(dir)
		if err != nil {
			return err
		}
		recs = all
	}
	home, _ := os.UserHomeDir()
	var suggestions []rehome.Suggestion
	seen := map[string]bool{}
	for _, rec := range recs {
		for _, s := range rehome.Suggest(rec, home, nil) {
			if seen[s.OldCwd] {
				continue
			}
			seen[s.OldCwd] = true
			suggestions = append(suggestions, s)
		}
	}
	renderRehomeSuggestions(out, suggestions, len(recs))
	return nil
}

// renderRehomeSuggestions prints one block per old directory: its
// candidates numbered ("[1] <dir>   <reason>"), or a note that none was
// found. Old directories come from bundles and are sanitised.
func renderRehomeSuggestions(w io.Writer, suggestions []rehome.Suggestion, records int) {
	if len(suggestions) == 0 {
		if records == 0 {
			fmt.Fprintln(w, "no import records (bffs sessions imports); nothing to suggest for")
			return
		}
		fmt.Fprintln(w, "no old directories to suggest for")
		return
	}
	for i, s := range suggestions {
		if i > 0 {
			fmt.Fprintln(w)
		}
		old := transcripts.Sanitize(s.OldCwd)
		fmt.Fprintln(w, old)
		if len(s.Candidates) == 0 {
			if isDir(s.OldCwd) {
				fmt.Fprintln(w, "  exists here as-is (no rehome needed)")
			} else {
				fmt.Fprintln(w, "  no candidate found under ~/build ~/src ~/code ~/projects ~/dev; clone it, then --map "+rehome.ShellQuote(old)+"=<dir>")
			}
			continue
		}
		width := 0
		dirs := make([]string, len(s.Candidates))
		for j, c := range s.Candidates {
			dirs[j] = transcripts.Sanitize(short(c.Dir))
			width = max(width, len(dirs[j]))
		}
		for j, c := range s.Candidates {
			fmt.Fprintf(w, "  [%d] %-*s  %s\n", j+1, width, dirs[j], transcripts.Sanitize(c.Reason))
		}
	}
}

// rehomeCount renders "3 sessions and 1 memory directory".
func rehomeCount(p rehome.Plan) string {
	parts := []string{}
	if n := len(p.Moves); n > 0 {
		parts = append(parts, countNoun(n, "session"))
	}
	if n := len(p.Memory); n > 0 {
		parts = append(parts, countNoun(n, "memory directory"))
	}
	return strings.Join(parts, " and ")
}

// liveRefusalPrefix opens the engine's reason for a session a running
// claude owns; the rendered line drops it in favour of "held (live)".
const liveRefusalPrefix = "open in a running claude "

// isLiveRefusal mirrors rehome.Apply's split of refusals into Held and
// Skipped.
func isLiveRefusal(r rehome.Refusal) bool {
	return strings.Contains(r.Reason, "running claude")
}

// refusalLine renders one refusal: "held (live): <sid> (pid N); …" for a
// session open in a running claude, "refused <sid>: <reason>" otherwise.
func refusalLine(r rehome.Refusal) string {
	reason := transcripts.Sanitize(r.Reason)
	if isLiveRefusal(r) {
		return fmt.Sprintf("held (live): %s %s", shortID(r.SessionID), strings.TrimPrefix(reason, liveRefusalPrefix))
	}
	return fmt.Sprintf("refused %s: %s", shortID(r.SessionID), reason)
}

// rehomeNewest picks, per mapped directory, the move with the newest
// transcript — the session --set-last-session points lastSessionId at,
// the same choice rehome.Apply makes. Directories come back sorted.
func rehomeNewest(moves []rehome.Move) ([]string, map[string]rehome.Move) {
	newest := map[string]rehome.Move{}
	for _, mv := range moves {
		if cur, ok := newest[mv.NewCwd]; !ok || mv.LastTS.After(cur.LastTS) {
			newest[mv.NewCwd] = mv
		}
	}
	cwds := make([]string, 0, len(newest))
	for cwd := range newest {
		cwds = append(cwds, cwd)
	}
	sort.Strings(cwds)
	return cwds, newest
}

// renderRehomePlan prints what a rehome would do: the rules, one line per
// session (old entry → new entry, "stamp only" when the entry stays), one
// per memory directory, every refusal with its reason, and — when
// lastSessionFile is set (--set-last-session) — the lastSessionId each
// mapped directory would get. Titles and paths read from disk are
// sanitised.
func renderRehomePlan(w io.Writer, p rehome.Plan, maps []rehome.Mapping, lastSessionFile string) {
	fmt.Fprintf(w, "rehome in %s:\n", rootLabel(p.Root))
	for _, m := range maps {
		fmt.Fprintf(w, "  rule    %s → %s\n", transcripts.Sanitize(m.Old), transcripts.Sanitize(short(m.New)))
	}
	for _, mv := range p.Moves {
		title := transcripts.Sanitize(mv.Title)
		if title == "" {
			title = "(untitled)"
		}
		title = truncateCell(title, titleWidth)
		from := transcripts.Sanitize(filepath.Base(filepath.Dir(mv.From)))
		action := fmt.Sprintf("projects/%s → projects/%s", from, transcripts.Sanitize(mv.NewSlug))
		if mv.SameSlug {
			action = fmt.Sprintf("projects/%s (already there; relocated stamp only)", transcripts.Sanitize(mv.NewSlug))
		}
		extra := ""
		if mv.Sidecar {
			extra = " + sidecar"
		}
		fmt.Fprintf(w, "  move    %s  %-*s  %s%s\n", shortID(mv.SessionID), titleWidth, title, action, extra)
	}
	for _, m := range p.Memory {
		mode := m.Mode
		if mode == "" {
			mode = rehome.MemoryMerge
		}
		fmt.Fprintf(w, "  memory  %s → %s  (%s; the old directory stays)\n", transcripts.Sanitize(short(m.From)), transcripts.Sanitize(short(m.To)), mode)
	}
	for _, r := range p.Refusals {
		fmt.Fprintf(w, "  %s\n", refusalLine(r))
	}
	if lastSessionFile != "" && len(p.Moves) > 0 {
		cwds, newest := rehomeNewest(p.Moves)
		for _, cwd := range cwds {
			fmt.Fprintf(w, "  set     lastSessionId for %s → %s in %s (claude --continue there opens it)\n",
				transcripts.Sanitize(short(cwd)), shortID(newest[cwd].SessionID), short(lastSessionFile))
		}
	}
	if len(p.Moves) > 0 {
		fmt.Fprintln(w, "  (a relocated record is appended to each transcript; conversation content, mtimes and picker order are unchanged)")
	}
}

// renderRehomeResult prints what happened, in the shape of plan §5.10:
// "moved N sessions → projects/<slug> (…)", one memory line per merged
// directory with its rewrites and the lines left to review, the sessions
// not moved, the lastSessionId pointer and one verify line per mapping.
func renderRehomeResult(w io.Writer, res rehome.Result) {
	moved := map[string]bool{}
	for _, sid := range res.Moved {
		moved[sid] = true
	}
	perSlug := map[string]int{}
	var slugs []string
	for _, mv := range res.Moves {
		if !moved[mv.SessionID] {
			continue
		}
		if _, ok := perSlug[mv.NewSlug]; !ok {
			slugs = append(slugs, mv.NewSlug)
		}
		perSlug[mv.NewSlug]++
	}
	sort.Strings(slugs)
	if len(res.Moves) > 0 && len(res.Moved) == 0 {
		fmt.Fprintln(w, "moved no sessions")
	}
	for _, slug := range slugs {
		fmt.Fprintf(w, "moved %s → projects/%s (relocated stamp appended; mtimes and picker order preserved)\n", countNoun(perSlug[slug], "session"), transcripts.Sanitize(slug))
	}
	if n := len(res.Moves) - len(res.Moved); n > 0 {
		fmt.Fprintf(w, "%s not moved (see the error)\n", countNoun(n, "session"))
	}
	for _, m := range res.Memory {
		fmt.Fprintln(w, memoryResultLine(res, m))
		for _, warn := range m.Warnings {
			fmt.Fprintf(w, "  note: %s\n", transcripts.Sanitize(warn))
		}
		if n := len(m.Remaining); n > 0 {
			verb := "mention"
			if n == 1 {
				verb = "mentions"
			}
			fmt.Fprintf(w, "%s still %s absolute paths: bffs memory scan-paths --project %s\n", countNoun(n, "memory line"), verb, rehome.ShellQuote(transcripts.Sanitize(rehomeProjectOf(res, m))))
		}
	}
	if n := len(res.Refusals); n > 0 {
		var parts []string
		if h := len(res.Held); h > 0 {
			parts = append(parts, fmt.Sprintf("%d held by a running claude", h))
		}
		if s := len(res.Skipped); s > 0 {
			parts = append(parts, fmt.Sprintf("%d refused", s))
		}
		fmt.Fprintf(w, "%s not moved: %s (listed above)\n", countNoun(n, "session"), strings.Join(parts, ", "))
	}
	if res.LastSessionSet != "" {
		fmt.Fprintf(w, "lastSessionId → %s (claude --continue opens it)\n", shortID(res.LastSessionSet))
	}
	renderVerifyLines(w, res.Verify)
}

// renderVerifyLines prints one "verify: cd <new> && claude --resume <sid>"
// line per mapped directory.
func renderVerifyLines(w io.Writer, lines []string) {
	for _, line := range lines {
		fmt.Fprintf(w, "verify: %s\n", transcripts.Sanitize(line))
	}
}

// memoryResultLine renders one merged memory directory the way §5.10
// shows it: "merged memory: 6 files (2 new, 3 identical, 1 renamed
// notes.imported-6f1e2c0a.md); MEMORY.md +1 section; rewrote /old → /new
// in 4 files". The verb follows the mode; the directory is named when
// more than one was written.
func memoryResultLine(res rehome.Result, m rehome.MemoryMove) string {
	total := len(m.Added) + len(m.Renamed) + len(m.Unchanged)
	written := len(m.Added)+len(m.Renamed) > 0
	head := "merged memory"
	switch m.Mode {
	case rehome.MemoryOverwrite:
		head = "replaced memory"
	case rehome.MemorySkip:
		head = "wrote memory"
		if !written {
			head = "kept memory"
		}
	}
	if len(res.Memory) > 1 {
		head += " for " + transcripts.Sanitize(short(rehomeProjectOf(res, m)))
	}
	var parts []string
	if n := len(m.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d new", n))
	}
	if n := len(m.Unchanged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d identical", n))
	}
	if n := len(m.Renamed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d renamed %s", n, strings.Join(sanitizeAll(m.Renamed), ", ")))
	}
	if m.Mode == rehome.MemoryOverwrite {
		parts = append(parts, "the previous directory set aside, never deleted")
	}
	if m.Mode == rehome.MemorySkip && !written {
		parts = append(parts, "--memory skip")
	}
	line := fmt.Sprintf("%s: %s", head, countNoun(total, "file"))
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	if m.IndexAppended {
		line += "; MEMORY.md +1 section"
	}
	if n := len(m.Rewritten); n > 0 {
		line += fmt.Sprintf("; rewrote %s in %s", rewriteRulesLabel(res.Mappings), countNoun(n, "file"))
	}
	return line
}

// rewriteRulesLabel renders the prefix rules a rehome rewrote in memory:
// "/Users/jonas → /home/jonas", several joined with ", ".
func rewriteRulesLabel(maps []rehome.Mapping) string {
	if len(maps) == 0 {
		return "old paths"
	}
	parts := make([]string, len(maps))
	for i, m := range maps {
		parts[i] = transcripts.Sanitize(m.Old) + " → " + transcripts.Sanitize(m.New)
	}
	return strings.Join(parts, ", ")
}

// rehomeProjectOf names the directory a merged memory directory belongs
// to: the mapped directory of a session whose memory it is, else the
// memory's own mapped path, else the projects/ entry it was written to.
func rehomeProjectOf(res rehome.Result, m rehome.MemoryMove) string {
	for _, mv := range res.Moves {
		if mv.OldCwd == m.OldCwd && mv.NewCwd != "" {
			return mv.NewCwd
		}
	}
	if newCwd, _, ok := rehome.ApplyMappings(res.Mappings, m.OldCwd, m.OldCwd); ok {
		return newCwd
	}
	return filepath.Dir(filepath.Dir(m.To))
}
