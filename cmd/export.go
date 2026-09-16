package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

var (
	exportProjects      []string
	exportAllProjects   bool
	exportSessions      []string
	exportOnly          string
	exportAccount       string
	exportSince         string
	exportNoLive        bool
	exportNoToolResults bool
	exportNoFileHistory bool
	exportNoPlans       bool
	exportNoHistory     bool
	exportWithTasks     bool
	exportOut           string
	exportServe         bool
	exportForce         bool
	exportYes           bool
	exportClaudeDir     string
)

// exportOutAuto is the --out value (besides "") that asks for the default
// file name bffs-<host>-<yyyymmdd-hhmm>.bffs in the current directory.
const exportOutAuto = "auto"

// exportOutStdout is the --out value that sends the bundle to stdout.
const exportOutStdout = "-"

var exportCmd = &cobra.Command{
	Use:   "export (--out <file.bffs> | --out -) [--project <dir>]... [--all-projects] [--session <id>]...",
	Short: "Pack sessions and auto-memory into a .bffs bundle for another machine or account",
	Long: `Writes the selected Claude Code sessions (transcript, sidecar, file-history,
plan files, prompt-history lines) and the project's auto-memory directory into
one .bffs bundle that ` + "`bffs import`" + ` reads on the other side. The default
selection is the current directory's project — sessions and memory — from the
pool the account claude would use here (the shared ~/.claude/projects under
partial isolation); --project, --all-projects, --session, --only and --since
widen or narrow it, --account picks another root.

Never exported: credentials, .claude.json, Claude's runtime files, and
anything Claude sweeps within its retention window. The summary lists the
sizes of the three parts that can carry pasted secrets (tool-results,
file-history, history) so they can be left out with --no-tool-results,
--no-file-history and --no-history. Sessions open in a running claude are
exported read-only and may be truncated; --no-live skips them.

With --out - the bundle goes to stdout and everything else to stderr:
    bffs export --project . --out - | ssh other-machine 'bffs import --from - -y --as-is'`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		if exportServe {
			return exitWith(1, errors.New("serving over the network arrives in the next milestone; use --out"))
		}
		if !cmd.Flags().Changed("out") {
			return errors.New(`--out is required: a .bffs file, "-" for stdout, or "auto" for bffs-<host>-<date>.bffs in the current directory`)
		}
		since, err := parseSince(exportSince)
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		req := exportRequest{
			Projects:    exportProjects,
			AllProjects: exportAllProjects,
			Sessions:    exportSessions,
			Only:        exportOnly,
			Account:     exportAccount,
			Since:       since,
			NoLive:      exportNoLive,
			Parts: porter.Parts{
				Sidecar:     true,
				ToolResults: !exportNoToolResults,
				FileHistory: !exportNoFileHistory,
				Plans:       !exportNoPlans,
				History:     !exportNoHistory,
				Tasks:       exportWithTasks,
			},
			Out:       exportOut,
			Force:     exportForce,
			Yes:       exportYes,
			ClaudeDir: exportClaudeDir,
			Cwd:       cwd,
			Now:       time.Now(),
			Version:   Version,
		}
		// Human text goes to stderr: stdout may carry the bundle.
		pr := newPrompter(cmd.InOrStdin(), cmd.ErrOrStderr())
		return runExport(cmd, dir, pr, req, isTTY())
	},
}

func init() {
	f := exportCmd.Flags()
	f.StringArrayVar(&exportProjects, "project", nil, "project directory (repeatable; default: the current directory)")
	f.BoolVar(&exportAllProjects, "all-projects", false, "every project of the root")
	f.StringArrayVar(&exportSessions, "session", nil, "session id or unique prefix of at least 8 characters (repeatable)")
	f.StringVar(&exportOnly, "only", "", `"sessions" or "memories" (default: both)`)
	f.StringVar(&exportAccount, "account", "", "root to export from (default: the account claude would use here; \"home\" = ~/.claude)")
	f.StringVar(&exportSince, "since", "", "only sessions modified since: 30d, 2w, 12h (default: any age)")
	f.BoolVar(&exportNoLive, "no-live", false, "skip sessions open in a running claude (default: include, read-only, possibly truncated)")
	f.BoolVar(&exportNoToolResults, "no-tool-results", false, "leave out saved tool outputs (may contain pasted secrets)")
	f.BoolVar(&exportNoFileHistory, "no-file-history", false, "leave out backups of files Claude edited")
	f.BoolVar(&exportNoPlans, "no-plans", false, "leave out plan files")
	f.BoolVar(&exportNoHistory, "no-history", false, "leave out prompt-history lines")
	f.BoolVar(&exportWithTasks, "with-tasks", false, "include task lists (tasks/<sid>/)")
	f.StringVar(&exportOut, "out", "", `output file, "-" for stdout, or "auto" for bffs-<host>-<date>.bffs in the current directory`)
	f.BoolVar(&exportServe, "serve", false, "serve the bundle to another machine on the LAN (next milestone)")
	f.BoolVar(&exportForce, "force", false, "overwrite an existing output file")
	f.BoolVarP(&exportYes, "yes", "y", false, "skip the selection confirmation")
	f.StringVar(&exportClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = f.MarkHidden("claude-dir")
	rootCmd.AddCommand(exportCmd)
}

// exportRequest is one resolved `bffs export` invocation, independent of
// the flag variables so tests can drive runExport directly.
type exportRequest struct {
	Projects    []string
	AllProjects bool
	Sessions    []string
	Only        string
	Account     string
	Since       time.Duration
	NoLive      bool
	Parts       porter.Parts
	Out         string // file path, exportOutStdout, or ""/exportOutAuto for the default name
	Force       bool
	Yes         bool
	ClaudeDir   string
	Cwd         string
	Now         time.Time
	Version     string
}

// exportSource is the root an export reads and the account it runs as
// (the manifest's source.account / account_type / isolation).
type exportSource struct {
	root      transcripts.Root
	account   store.Account
	isolation store.IsolationPreset
}

// runExport selects, summarises, confirms and writes. The summary, the
// warnings and the prompt go to stderr (pr.out); the "wrote" line goes to
// stdout for a file and to stderr when the bundle itself is on stdout.
func runExport(cmd *cobra.Command, dir string, pr *prompter, req exportRequest, tty bool) error {
	errOut := cmd.ErrOrStderr()
	env, err := loadCatalogEnv(dir, req.ClaudeDir)
	if err != nil {
		return err
	}
	src, warning, err := resolveExportSource(dir, env, req.Account, req.Cwd)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(errOut, "warning:", warning)
	}
	ctx := cmdContext(cmd)
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		fmt.Fprintln(errOut, "warning: liveness unavailable:", err)
	}

	selOpts := porter.SelectOptions{
		AllProjects: req.AllProjects,
		Projects:    req.Projects,
		Sessions:    req.Sessions,
		Only:        req.Only,
		Since:       req.Since,
		IncludeLive: !req.NoLive,
		Now:         req.Now,
	}
	if !req.AllProjects && len(req.Projects) == 0 && len(req.Sessions) == 0 {
		selOpts.Projects = []string{req.Cwd}
	}
	sel, err := porter.Select(ctx, src.root, nil, live, selOpts)
	if err != nil {
		return err
	}
	sel.Parts = req.Parts

	opts := porter.ExportOptions{
		Compression: bundle.CompGzip,
		Version:     req.Version,
		Account:     src.account,
		Isolation:   src.isolation,
		Now:         req.Now,
	}
	m, opener, warnings, err := porter.BuildManifest(ctx, sel, opts)
	if err != nil {
		return err
	}
	release := func() {
		if c, ok := opener.(io.Closer); ok {
			_ = c.Close()
		}
	}

	renderExportSummary(errOut, newExportSummary(src.root, m, opener, sel.Parts), req.Now)
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}

	toStdout := req.Out == exportOutStdout
	// A bundle on stdout is usually piped somewhere; the question is asked
	// only when a terminal is there to answer it. A file is never written
	// without an explicit yes.
	if !req.Yes && (tty || !toStdout) {
		ok, err := confirmOrAbort(pr, "Proceed? [y/N] ", false, tty)
		if err != nil {
			release()
			return fmt.Errorf("refusing to export: %w", err)
		}
		if !ok {
			release()
			return nil
		}
	}

	nSessions, nMemoryFiles := manifestCounts(m)
	id8 := short8(m.BundleID)
	if toStdout {
		bw := bufio.NewWriterSize(cmd.OutOrStdout(), 256<<10)
		cw := &countingWriter{w: bw}
		if _, err := porter.Write(ctx, cw, m, opener, opts); err != nil {
			return exitWith(1, exportWriteError(err, m))
		}
		if err := bw.Flush(); err != nil {
			return exitWith(1, fmt.Errorf("export: %w", err))
		}
		fmt.Fprintf(errOut, "wrote %s to stdout (%s, %d memory files, bundle %s)\n", formatSize(cw.n), countNoun(nSessions, "session"), nMemoryFiles, id8)
		return nil
	}

	path, err := exportOutPath(req.Out, req.Cwd, m.Source.Hostname, req.Now)
	if err != nil {
		release()
		return err
	}
	if _, err := os.Lstat(path); err == nil && !req.Force {
		release()
		return fmt.Errorf("%s exists; --force to overwrite", short(path))
	}
	size, err := writeBundleFile(ctx, path, m, opener, opts)
	if err != nil {
		return exitWith(1, exportWriteError(err, m))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%s, %s, %d memory files, bundle %s)\n", short(path), formatSize(size), countNoun(nSessions, "session"), nMemoryFiles, id8)
	return nil
}

// exportWriteError wraps a bundle-writing failure. A short read — the
// source ended before the size the pre-pass recorded — can only come from
// a live transcript a running claude rewrote under us (the pre-pass keeps
// the old inode open, but a truncate-in-place still shortens it), so the
// message names the --no-live way out when the bundle carried one (§6.2).
func exportWriteError(err error, m *bundle.Manifest) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "short read") {
		for _, e := range m.Entries {
			if e.LivePossiblyTruncated {
				return fmt.Errorf("export: %w; a running claude changed a live session under the export — close it, or retry with --no-live", err)
			}
		}
	}
	return fmt.Errorf("export: %w", err)
}

// resolveExportSource picks the root to export from: the named account's
// (an api_key account reads ~/.claude; "home" is ~/.claude itself), else
// the root of the account resolver.Resolve picks for cwd, else the home
// root. A resolution failure is a warning, never an error — the pool is
// readable regardless of a broken pin.
func resolveExportSource(dir string, env *catalogEnv, account, cwd string) (exportSource, string, error) {
	if account != "" {
		root, err := env.rootForAccount(account)
		if err != nil {
			return exportSource{}, "", err
		}
		src := exportSource{root: root}
		if acc, ok := env.accs.Get(account); ok {
			src.account = acc
			if acc.Type == store.TypeOAuth {
				src.isolation = store.ResolveIsolation(acc.Isolation, env.state.Isolation)
			}
		}
		return src, "", nil
	}
	home, err := env.homeRoot()
	if err != nil {
		return exportSource{}, "", err
	}
	r, err := resolver.Resolve(dir, cwd)
	if err != nil {
		return exportSource{root: home}, fmt.Sprintf("%v; exporting from the home root", err), nil
	}
	if r.Source == resolver.SourceNone {
		return exportSource{root: home}, "", nil
	}
	src := exportSource{root: home, account: r.Account}
	if r.Account.Type != store.TypeOAuth {
		return src, "", nil
	}
	src.isolation = store.ResolveIsolation(r.Account.Isolation, env.state.Isolation)
	root, err := transcripts.RootFor(env.roots, r.Account.Name)
	if err != nil {
		return exportSource{root: home}, fmt.Sprintf("%v; exporting from the home root", err), nil
	}
	src.root = root
	return src, "", nil
}

// exportSummary is what the user confirms: the root, one block per
// project, and the total.
type exportSummary struct {
	Root     transcripts.Root
	Parts    porter.Parts
	Projects []exportProject
	Total    int64
}

// exportProject is one project's share of a bundle.
type exportProject struct {
	Label        string // the directory (short) or the slug when no cwd is recorded
	Sessions     int
	Newest       string // title of the newest session, sanitised
	NewestAt     time.Time
	Bytes        int64 // every session file
	Live         int
	ToolResults  int64
	FileHistory  int64
	HistoryLines int
	MemoryFiles  int
	MemoryBytes  int64
}

// newExportSummary groups the manifest's entries by project. History line
// counts come from the in-memory history files the opener serves.
func newExportSummary(root transcripts.Root, m *bundle.Manifest, src bundle.Opener, parts porter.Parts) exportSummary {
	sum := exportSummary{Root: root, Parts: parts, Total: m.Totals.Bytes}
	index := map[string]int{}
	group := func(key, label string) *exportProject {
		if i, ok := index[key]; ok {
			return &sum.Projects[i]
		}
		index[key] = len(sum.Projects)
		sum.Projects = append(sum.Projects, exportProject{Label: label})
		return &sum.Projects[len(sum.Projects)-1]
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		cwd := transcripts.Sanitize(e.Cwd)
		switch e.Kind {
		case bundle.EntrySession:
			key, label := cwd, short(cwd)
			if cwd == "" {
				key, label = "projects/"+e.Slug, "projects/"+transcripts.Sanitize(e.Slug)+" (no cwd recorded)"
			}
			p := group(key, label)
			p.Sessions++
			if e.LivePossiblyTruncated {
				p.Live++
			}
			// The newest session names the group; an older title stands in
			// when the newest has none.
			title := transcripts.Sanitize(e.Title)
			if p.Sessions == 1 || e.Last.After(p.NewestAt) {
				p.NewestAt = e.Last
				if title != "" {
					p.Newest = title
				}
			} else if p.Newest == "" {
				p.Newest = title
			}
			for _, f := range e.Files {
				p.Bytes += f.Size
				kind, _, _, err := bundle.ClassifyName(f.Path)
				if err != nil {
					continue
				}
				switch kind {
				case bundle.NameSidecar:
					if strings.Contains(f.Path, "/tool-results/") {
						p.ToolResults += f.Size
					}
				case bundle.NameFileHistory:
					p.FileHistory += f.Size
				case bundle.NameHistory:
					p.HistoryLines += countLines(src, f.Path)
				}
			}
		case bundle.EntryMemory:
			key, label := cwd, short(cwd)
			if cwd == "" {
				key, label = "memory/"+e.Slug, "memory/"+transcripts.Sanitize(e.Slug)+" (no cwd recorded)"
			}
			p := group(key, label)
			p.MemoryFiles += len(e.Files)
			for _, f := range e.Files {
				p.MemoryBytes += f.Size
			}
		}
	}
	return sum
}

// countLines counts the newline-terminated lines of a bundle file the
// opener serves from memory (history/<sid>.jsonl). Anything else is 0.
func countLines(src bundle.Opener, path string) int {
	rc, err := src.Open(path)
	if err != nil {
		return 0
	}
	defer rc.Close()
	n := 0
	r := bufio.NewReaderSize(rc, 64<<10)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return n
		}
		if b == '\n' {
			n++
		}
	}
}

// renderExportSummary prints the confirmation block of plan §5.10: the
// root line, one block per project with the three secret-bearing parts
// side by side, and the total. Every manifest-derived string is sanitised.
func renderExportSummary(w io.Writer, s exportSummary, now time.Time) {
	fmt.Fprintf(w, "Exporting from %s:\n", exportRootLabel(s.Root))
	for _, p := range s.Projects {
		fmt.Fprintf(w, "  project %s\n", transcripts.Sanitize(p.Label))
		if p.Sessions > 0 {
			line := "    " + countNoun(p.Sessions, "session")
			if p.Newest != "" {
				line += fmt.Sprintf("   (newest: %q, %s)", p.Newest, humanizeAgo(p.NewestAt, now))
			}
			line += "   " + formatSize(p.Bytes)
			if p.Live > 0 {
				line += fmt.Sprintf("   %d live (may be truncated)", p.Live)
			}
			fmt.Fprintln(w, line)
			fmt.Fprintf(w, "      tool-results %s (saved tool outputs — may contain pasted secrets)   file-history %s (backups of files Claude edited)   history %s (prompt history)\n",
				partSize(s.Parts.ToolResults, p.ToolResults), partSize(s.Parts.FileHistory, p.FileHistory), partLines(s.Parts.History, p.HistoryLines))
		}
		if p.MemoryFiles > 0 {
			fmt.Fprintf(w, "    memory        %d files   %s\n", p.MemoryFiles, formatSize(p.MemoryBytes))
		}
	}
	fmt.Fprintf(w, "  total %s\n", formatSize(s.Total))
}

// exportRootLabel names the root an export reads, with the accounts that
// share it: the plan's "shared pool (~/.claude; partial isolation: a, b)".
func exportRootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan session dir %q (%s; read-only)", r.Owner, short(r.ConfigDir))
	case r.Owner != "":
		return fmt.Sprintf("account %q (%s; full isolation)", r.Owner, short(r.ConfigDir))
	case r.Shared && len(r.Accounts) > 0:
		return fmt.Sprintf("the shared pool (%s; partial isolation: %s)", short(r.ConfigDir), strings.Join(r.Accounts, ", "))
	default:
		return fmt.Sprintf("%s (home, unmanaged)", short(r.ConfigDir))
	}
}

func partSize(included bool, n int64) string {
	if !included {
		return "excluded"
	}
	return formatSize(n)
}

func partLines(included bool, n int) string {
	if !included {
		return "excluded"
	}
	return countNoun(n, "line")
}

// manifestCounts is the number of session entries and of memory files.
func manifestCounts(m *bundle.Manifest) (nSessions, nMemoryFiles int) {
	for _, e := range m.Entries {
		switch e.Kind {
		case bundle.EntrySession:
			nSessions++
		case bundle.EntryMemory:
			nMemoryFiles += len(e.Files)
		}
	}
	return nSessions, nMemoryFiles
}

// exportOutPath resolves --out: "" or "auto" becomes
// bffs-<host>-<yyyymmdd-hhmm>.bffs in cwd; everything else is normalised
// like every user path (~, relative to cwd, cleaned).
func exportOutPath(out, cwd, host string, now time.Time) (string, error) {
	if out == "" || out == exportOutAuto {
		if host == "" {
			host = "host"
		}
		out = filepath.Join(cwd, fmt.Sprintf("bffs-%s-%s%s", host, now.Format("20060102-1504"), bundle.Ext))
	}
	return store.NormalizePath(out)
}

// writeBundleFile streams the bundle into a temporary file beside path
// (mode 0600) and renames it into place once it is complete and synced,
// so an interrupted export never leaves a truncated .bffs under the final
// name. It returns the bytes written.
func writeBundleFile(ctx context.Context, path string, m *bundle.Manifest, src bundle.Opener, opts porter.ExportOptions) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		if c, ok := src.(io.Closer); ok {
			_ = c.Close()
		}
		return 0, err
	}
	tmpPath := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return 0, err
	}
	bw := bufio.NewWriterSize(tmp, 256<<10)
	cw := &countingWriter{w: bw}
	if _, err := porter.Write(ctx, cw, m, src, opts); err != nil {
		return 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		done = true
		return 0, err
	}
	done = true
	return cw.n, nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// short8 is the eight-character prefix of a bundle id for messages.
func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// confirmOrAbort asks prompt through pr and reports whether the user said
// yes. yes skips the question. Without a terminal and without yes it
// refuses: a piped run must state its consent with -y rather than have a
// default answered for it. A decline prints "aborted".
func confirmOrAbort(pr *prompter, prompt string, yes, tty bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !tty {
		return false, errors.New("stdin is not a terminal; pass -y to confirm")
	}
	ans, err := pr.line(prompt)
	if err != nil {
		return false, err
	}
	if a := strings.ToLower(strings.TrimSpace(ans)); a == "y" || a == "yes" {
		return true, nil
	}
	fmt.Fprintln(pr.out, "aborted")
	return false, nil
}
