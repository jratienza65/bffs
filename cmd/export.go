package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
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
	exportPort          int
	exportTTL           time.Duration
	exportIface         string
	exportAllowRouted   bool
	exportShowIPv6      bool
	exportAllowLoopback bool
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
	Use:   "export (--out <file.bffs> | --out - | --serve) [--project <dir>]... [--all-projects] [--session <id>]...",
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
    bffs export --project . --out - | ssh other-machine 'bffs import --from - -y --as-is'

With --serve the bundle is offered to one other machine on the local network:
this side shows an address and a pairing code, the other side runs
` + "`bffs import --from <address>`" + ` and types the code, and the bundle streams
over TLS once both sides have proven they know it. Only on-link peers are
accepted (no VPN or Tailscale addresses; --allow-routed relaxes the on-link
test), the code is valid for --ttl (10m, at most 30m) and three wrong codes
end the serve. Everything is printed on stderr.

Firewall notes: macOS asks once per binary whether bffs may accept
connections (an ad-hoc-signed build asks again after every rebuild:
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add /opt/bffs/bffs
--unblockapp /opt/bffs/bffs); on Windows a non-administrator is blocked
silently (netsh advfirewall firewall add rule name=bffs dir=in action=allow
program=<path>\bffs.exe protocol=tcp localport=7345 profile=private); on
Linux with ufw: ufw allow from 192.168.0.0/16 to any port 7345 proto tcp.
When no inbound port can be opened, use the ssh pipe above instead.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		if exportServe && cmd.Flags().Changed("out") {
			return errors.New("--serve and --out are mutually exclusive: serve the bundle to another machine, or write it to a file")
		}
		if !exportServe && !cmd.Flags().Changed("out") {
			return errors.New(`--out is required: a .bffs file, "-" for stdout, or "auto" for bffs-<host>-<date>.bffs in the current directory (or --serve for another machine on the LAN)`)
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
			Out:           exportOut,
			Serve:         exportServe,
			Port:          exportPort,
			TTL:           exportTTL,
			Iface:         exportIface,
			AllowRouted:   exportAllowRouted,
			ShowIPv6:      exportShowIPv6,
			AllowLoopback: exportAllowLoopback,
			Force:         exportForce,
			Yes:           exportYes,
			ClaudeDir:     exportClaudeDir,
			Cwd:           cwd,
			Now:           time.Now(),
			Version:       Version,
		}
		req.Host, req.User = localIdentity()
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
	f.BoolVar(&exportServe, "serve", false, "offer the bundle to one other machine on the local network (shows an address and a pairing code)")
	f.IntVar(&exportPort, "port", servePortDefault, "port to listen on with --serve (0 = kernel-chosen)")
	f.DurationVar(&exportTTL, "ttl", serveTTLDefault, "how long the pairing code stays valid with --serve (at most 30m)")
	f.StringVar(&exportIface, "iface", "", "listen on this interface only with --serve (e.g. en0)")
	f.BoolVar(&exportAllowRouted, "allow-routed", false, "with --serve, accept peers that are not on-link (multi-VLAN offices); prints a warning")
	f.BoolVar(&exportShowIPv6, "show-ipv6", false, "with --serve, also list IPv6 link-local addresses")
	f.BoolVar(&exportAllowLoopback, "allow-loopback", false, "with --serve, also listen on loopback (tests and same-machine trials)")
	_ = f.MarkHidden("allow-loopback")
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

	// --serve (plan §8): the listeners, the pairing window, what counts
	// as the local network, and how this machine names itself in auth-ok.
	Serve         bool
	Port          int
	TTL           time.Duration
	Iface         string
	AllowRouted   bool
	ShowIPv6      bool
	AllowLoopback bool
	Host, User    string
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
	// --serve: the pairing flags and the local network are checked before
	// any selection work, so a machine that cannot serve fails at once
	// rather than after the summary was reviewed.
	var setup serveSetup
	if req.Serve {
		var err error
		if setup, err = prepareServe(req); err != nil {
			return err
		}
		if req.AllowRouted {
			fmt.Fprintln(errOut, "warning: --allow-routed: peers beyond this machine's own networks are accepted; only the pairing code protects the transfer")
		}
	}
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

	toStdout := req.Out == exportOutStdout && !req.Serve
	// A bundle on stdout is usually piped somewhere; the question is asked
	// only when a terminal is there to answer it. A file is never written
	// (and no code is shown) without an explicit yes.
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

	if req.Serve {
		return serveExport(ctx, cmd, req, setup, src, m, opener, opts)
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

// ---- --serve: one bundle to one other machine on the LAN (plan §8) ----

// Serve defaults (plan §8.8): the port, the pairing window and its cap,
// the wrong-code budget of one serve.
const (
	servePortDefault = 7345
	serveTTLDefault  = 10 * time.Minute
	serveTTLMax      = 30 * time.Minute
	serveAttempts    = 3
)

// Injection seams for the loopback end-to-end test: how a serve binds,
// which local addresses it sees and which code it shows. Production uses
// the real ones (a nil Listen is net.Listen inside transfer).
var (
	serveListen   transfer.Listener
	serveLocal    = transfer.LANAddrs
	serveGenerate = transfer.GenerateCode
)

// serveSetup is what --serve needs before any selection work: the
// validated pairing window, what counts as the local network, and the
// addresses to bind.
type serveSetup struct {
	ttl   time.Duration
	lan   transfer.LANOptions
	local []transfer.LinkAddr
}

// prepareServe validates --ttl and --port and enumerates the local-network
// addresses; a machine with none (only loopback/VPN up) fails here, before
// the selection is built and long before a code is shown.
func prepareServe(req exportRequest) (serveSetup, error) {
	s := serveSetup{ttl: req.TTL}
	if s.ttl == 0 {
		s.ttl = serveTTLDefault
	}
	if s.ttl < 0 || s.ttl > serveTTLMax {
		return s, fmt.Errorf("invalid --ttl %s: the pairing code may stay valid for up to %s", req.TTL, serveTTLMax)
	}
	if req.Port < 0 || req.Port > 65535 {
		return s, fmt.Errorf("invalid --port %d: use 1-65535, or 0 for a kernel-chosen port", req.Port)
	}
	s.lan = transfer.LANOptions{AllowLoopback: req.AllowLoopback, AllowRouted: req.AllowRouted, Iface: req.Iface}
	local, err := serveLocal(s.lan)
	if err != nil {
		return s, err
	}
	if len(local) == 0 {
		return s, exitWith(1, errors.New("no local-network address found (only loopback/VPN interfaces are up); connect to Wi-Fi/Ethernet or use --out file.bffs"))
	}
	s.local = local
	return s, nil
}

// serveExport is the A side of plan §8.5: the manifest was built and
// confirmed by runExport; this generates the code, binds every on-link
// address, prints the banner and the event lines on stderr, streams the
// bundle to the first peer that proves it knows the code, and maps the
// outcome to the exit codes of §5.9. The opener is released when the serve
// ends, whichever way.
func serveExport(ctx context.Context, cmd *cobra.Command, req exportRequest, setup serveSetup, src exportSource, m *bundle.Manifest, opener bundle.Opener, opts porter.ExportOptions) error {
	errOut := cmd.ErrOrStderr()
	defer func() {
		if c, ok := opener.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	// The manifest bytes transfer sends ahead of the body must be entry 0
	// of that body byte for byte: bundle.Build marshals the manifest with a
	// compact json.Marshal, so the same call here yields the same bytes
	// (Body checks that below rather than trusting it).
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	code, err := serveGenerate()
	if err != nil {
		return fmt.Errorf("pairing code: %w", err)
	}
	account := src.root.Owner
	if account == "" {
		account = src.account.Name
	}

	sctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first Ctrl-C cancels the serve; restoring the default behaviour
	// then lets a second one end the process however the serve is stuck.
	context.AfterFunc(sctx, stop)
	pr := &servePrinter{
		w:            errOut,
		code:         code,
		local:        setup.local,
		ttl:          setup.ttl,
		showIPv6:     req.ShowIPv6,
		manifestSize: int64(len(manifestBytes)),
		bar:          newProgressBar(errOut, "sending", isTerminalWriter(errOut)),
	}
	res, err := transfer.Serve(sctx, transfer.ServeOptions{
		Code:        code,
		Port:        req.Port,
		TTL:         setup.ttl,
		MaxAttempts: serveAttempts,
		Manifest:    manifestBytes,
		Compression: byte(opts.Compression),
		Body: func(ctx context.Context, w io.Writer) error {
			// bundle.Build rather than porter.Write: Write closes the
			// opener after one build, and a serve may stream again after a
			// peer vanished mid-transfer. The opener is released above.
			bw := bufio.NewWriterSize(w, 256<<10)
			raw, err := bundle.Build(ctx, bw, m, opener, opts.Compression, pr.progress)
			if err != nil {
				return exportWriteError(err, m)
			}
			if !bytes.Equal(raw, manifestBytes) {
				return errors.New("manifest changed between the summary and the stream")
			}
			return bw.Flush()
		},
		UI:      pr.uiWriter(),
		Events:  pr.event,
		LAN:     setup.lan,
		Listen:  serveListen,
		Version: req.Version,
		Host:    req.Host,
		User:    req.User,
		Account: account,
		Local:   setup.local,
	})
	switch {
	case err == nil:
		pr.delivered(res)
		return nil
	case sctx.Err() != nil || errors.Is(err, context.Canceled):
		pr.interrupt()
		if res.Bytes > 0 {
			return exitWith(130, fmt.Errorf("cancelled after %s; the other machine discards the partial bundle", formatSize(res.Bytes)))
		}
		return exitWith(130, errors.New("cancelled; nothing was sent"))
	case errors.Is(err, transfer.ErrTooManyAttempts):
		pr.interrupt()
		return exitWith(1, fmt.Errorf("%d failed pairing attempts; run bffs export --serve again for a new code", serveAttempts))
	default:
		// ErrExpired carries "code expired after MM:SS; no pairing
		// happened"; a peer-reported failure "<host> reported: <reason>";
		// the rest is this side's own failure.
		pr.interrupt()
		return exitWith(1, err)
	}
}

// serveBanner is what the A side prints once its listeners are bound.
type serveBanner struct {
	Code     string // the pairing code's display form — its only appearance
	Key      string // this machine's key fingerprint (8 hex)
	Addr     netip.Addr
	Port     uint16
	Local    []transfer.LinkAddr
	TTL      time.Duration
	Attempts int
	ShowIPv6 bool
}

// renderServeBanner prints the A-side banner of plan §5.10: the import
// command with this machine's first bound address (the port omitted when
// it is the default), with --show-ipv6 the link-local addresses without a
// zone and the note about appending one, the code beside the key
// fingerprint, and the waiting line.
func renderServeBanner(w io.Writer, b serveBanner) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "On the other machine, run:    bffs import --from %s\n", fromTarget(b.Addr.WithZone(""), b.Port))
	if b.ShowIPv6 {
		for _, la := range b.Local {
			if !la.Addr.Is6() || !la.Addr.IsLinkLocalUnicast() {
				continue
			}
			fmt.Fprintf(w, "         or, over IPv6:       bffs import --from %s   (append %%<your interface> to the address, e.g. %%%s)\n", fromTarget(la.Addr.WithZone(""), b.Port), zoneExample(la.Iface))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Pairing code:   %s          (this machine's key: %s — the other side shows it as \"peer key\")\n", b.Code, b.Key)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Waiting for the other machine…  code valid for %s, %d attempts, one transfer.   (Ctrl-C cancels; --show-ipv6 lists link-local addresses)\n", fmtMMSS(b.TTL), b.Attempts)
}

// fromTarget renders an address the way --from takes it: IPv6 in
// brackets, the port only when it is not the default.
func fromTarget(addr netip.Addr, port uint16) string {
	s := addr.String()
	if addr.Is6() {
		s = "[" + s + "]"
	}
	if port != servePortDefault {
		s += fmt.Sprintf(":%d", port)
	}
	return s
}

// zoneExample is the interface name the --show-ipv6 note suggests: the
// listener's own name is the best guess for a machine of the same kind.
func zoneExample(iface string) string {
	if iface == "" || iface == "lo" || iface == "lo0" {
		return "en0"
	}
	return iface
}

// fmtMMSS renders a duration as M:SS ("10:00").
func fmtMMSS(d time.Duration) string {
	secs := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// servePrinter turns transfer events into the A-side lines of plan §5.10:
// the banner on the first listen event (the bound port is only known
// then), one timestamped line per event, the progress bar while sending.
// Every peer-derived string is sanitised; the code is printed exactly
// once, in the banner. The mutex serialises event lines, progress and the
// hints transfer writes to the UI writer.
type servePrinter struct {
	mu           sync.Mutex
	w            io.Writer
	code         transfer.Code
	local        []transfer.LinkAddr
	ttl          time.Duration
	showIPv6     bool
	manifestSize int64
	banner       bool
	bar          *progressBar
	acceptedAt   time.Time
}

func (p *servePrinter) event(ev transfer.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.Kind {
	case "listen":
		if p.banner {
			return
		}
		p.banner = true
		b := serveBanner{Code: p.code.Display(), Key: keyFromText(ev.Text, "key "), Local: p.local, TTL: p.ttl, Attempts: serveAttempts, ShowIPv6: p.showIPv6}
		if ap, err := netip.ParseAddrPort(ev.Peer); err == nil {
			b.Addr, b.Port = ap.Addr(), ap.Port()
		}
		renderServeBanner(p.w, b)
		return
	case "code-ok", "sending", "progress", "done", "expired":
		// code-ok is folded into the manifest line, the bar shows the
		// sending, the delivered line follows the serve, and an expiry
		// ends it with ErrExpired carrying the same text — printed once,
		// as the error.
		return
	case "accept":
		p.acceptedAt = time.Now()
	}
	p.bar.interrupt()
	fmt.Fprintf(p.w, "  %s  %s\n", ev.Time.Format("15:04:05"), serveEventText(ev, p.manifestSize))
}

// serveEventText is the line for one A-side event: the plan's wording
// where this side has the facts (the peer's address, the manifest size,
// what was accepted), transfer's own text otherwise. Peer text is
// sanitised.
func serveEventText(ev transfer.Event, manifestSize int64) string {
	switch ev.Kind {
	case "connect":
		return peerIP(ev.Peer) + " connected — waiting for its code"
	case "manifest-sent":
		return fmt.Sprintf("%s code accepted; manifest sent (%s) — waiting for the other side to review", peerIP(ev.Peer), formatSize(manifestSize))
	case "accept":
		// transfer says "<host> accepted"; the plan's line names what.
		if name, ok := strings.CutSuffix(ev.Text, " accepted"); ok {
			return "manifest accepted by " + transcripts.Sanitize(name)
		}
	}
	return transcripts.Sanitize(ev.Text)
}

// peerIP is the address part of an event's ip:port peer, sanitised.
func peerIP(peer string) string {
	if ap, err := netip.ParseAddrPort(peer); err == nil {
		return ap.Addr().String()
	}
	return transcripts.Sanitize(peer)
}

// keyFromText picks the key fingerprint out of a transfer event text
// ("… key 3f9a1c2e" / "… peer key 3f9a1c2e)"): the hex run after marker.
// Events are the only channel that carries it before the serve returns.
func keyFromText(text, marker string) string {
	i := strings.LastIndex(text, marker)
	if i < 0 {
		return "unknown"
	}
	s := text[i+len(marker):]
	n := 0
	for n < len(s) && n < 8 && isHexByte(s[n]) {
		n++
	}
	if n == 0 {
		return "unknown"
	}
	return s[:n]
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func (p *servePrinter) progress(pr bundle.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.update(pr)
}

// uiWriter is the writer transfer prints its hints to (the firewall note
// after 30 s without a connection), serialised with the event lines and
// indented like them.
func (p *servePrinter) uiWriter() io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.bar.interrupt()
		if _, err := p.w.Write([]byte("  ")); err != nil {
			return 0, err
		}
		return p.w.Write(b)
	})
}

// delivered ends a successful serve: the final bar, the delivered line
// and "Done.".
func (p *servePrinter) delivered(res transfer.ServeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.end()
	host := transcripts.Sanitize(res.PeerHost)
	if host == "" {
		host = res.Peer.Addr().String()
	}
	took := time.Duration(0)
	if !p.acceptedAt.IsZero() {
		took = time.Since(p.acceptedAt)
	}
	fmt.Fprintf(p.w, "  %s  delivered: %s verified by %s in %.1fs\n", time.Now().Format("15:04:05"), countNoun(res.Done.Entries, "file"), host, took.Seconds())
	fmt.Fprintln(p.w, "Done.")
}

// interrupt ends a partial progress line before an error is printed.
func (p *servePrinter) interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.interrupt()
}

// writerFunc adapts a function to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// progressBar renders one "  sending ████░░ 42%   62.1 MB   40.0 MB/s"
// line from bundle progress reports. On a terminal it is redrawn in place
// at most every 200 ms; elsewhere only the final line is printed, so a log
// never fills with partial bars. Callers serialise access.
type progressBar struct {
	w        io.Writer
	label    string
	live     bool
	verified bool // final line says "N files verified (sha256)" instead of the rate
	now      func() time.Time
	started  time.Time
	last     time.Time
	bytes    int64
	total    int64
	files    int
	onLine   bool // a partial bar is on the terminal line
	width    int  // widest line drawn on the current terminal line, in runes
	done     bool
}

func newProgressBar(w io.Writer, label string, live bool) *progressBar {
	return &progressBar{w: w, label: label, live: live, now: time.Now}
}

// update takes one bundle progress report (Phase "build" or "unpack";
// the hashing pre-pass is ignored).
func (b *progressBar) update(p bundle.Progress) {
	if p.Phase == "hash" || b.done {
		return
	}
	if b.started.IsZero() {
		b.started = b.now()
	}
	b.bytes, b.total, b.files = p.Bytes, p.TotalBytes, p.Files
	if b.total > 0 && b.bytes >= b.total {
		b.render(true)
		return
	}
	if !b.live {
		return
	}
	if t := b.now(); t.Sub(b.last) >= 200*time.Millisecond {
		b.last = t
		b.render(false)
	}
}

// end prints the final line when progress was reported and the transfer
// ended before the bar reached 100 %.
func (b *progressBar) end() {
	if !b.started.IsZero() && !b.done {
		b.render(true)
	}
}

// interrupt ends a partial line so another line can be printed.
func (b *progressBar) interrupt() {
	if b.onLine {
		fmt.Fprintln(b.w)
		b.onLine, b.width = false, 0
	}
}

func (b *progressBar) render(final bool) {
	pct := 0
	if b.total > 0 {
		pct = int(min(b.bytes*100/b.total, 100))
	}
	if final {
		pct = 100
	}
	line := fmt.Sprintf("  %s %s %3d%%   %s", b.label, barBlocks(pct), pct, formatSize(b.bytes))
	if final && b.verified {
		line += fmt.Sprintf("   %s verified (sha256)", countNoun(b.files, "file"))
	} else if rate := b.rate(); rate != "" {
		line += "   " + rate
	}
	// A redraw overwrites in place; a shorter line (the rate fell) is
	// padded so no tail of the previous one stays visible. No escape
	// sequences: a plain "\r" works on every console.
	w := utf8.RuneCountInString(line)
	if b.onLine {
		fmt.Fprint(b.w, "\r")
		if w < b.width {
			line += strings.Repeat(" ", b.width-w)
		}
	}
	b.width = max(b.width, w)
	fmt.Fprint(b.w, line)
	if final {
		fmt.Fprintln(b.w)
		b.onLine, b.width, b.done = false, 0, true
	} else {
		b.onLine = true
	}
}

func (b *progressBar) rate() string {
	el := b.now().Sub(b.started).Seconds()
	if el <= 0 || b.bytes == 0 {
		return ""
	}
	return formatSize(int64(float64(b.bytes)/el)) + "/s"
}

// barBlocks is a twenty-cell bar for a percentage.
func barBlocks(pct int) string {
	filled := min(max(pct/5, 0), 20)
	return strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
}

// isTerminalWriter reports whether w is the process's stderr and a
// terminal — the only case a progress line is redrawn in place.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && f == os.Stderr && term.IsTerminal(int(f.Fd()))
}

// localIdentity names this machine and user for the peer (auth-ok /
// accept): the hostname and the home directory's owner, reduced to a
// constrained identifier the way the manifest's source block is.
func localIdentity() (host, user string) {
	host, _ = os.Hostname()
	host = identStr(host)
	if host == "" {
		host = "host"
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		user = filepath.Base(home)
	}
	if user == "" || user == "." || user == string(filepath.Separator) {
		user = os.Getenv("USER")
	}
	return host, identStr(user)
}

// identStr keeps the bytes an identifier may carry (letters, digits, "_",
// "." and "-"), at most 64 of them.
func identStr(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s) && sb.Len() < 64; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-' {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}
