package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

var (
	importFrom           string
	importAccount        string
	importAsIs           bool
	importOnConflict     string
	importMemory         string
	importTrustMemory    bool
	importPreserveMtimes bool
	importMaxSize        string
	importForce          bool
	importCleanStaging   bool
	importDryRun         bool
	importYes            bool
	importInto           string
	importMap            []string
	importCarryTrust     bool
	importSetLastSession bool
	importForceStamp     bool
	importAllowRouted    bool
	importAllowLoopback  bool
	importClaudeDir      string
)

// importDefaultMaxSize is the --max-size default: bundle.DefaultLimits'
// 2 GiB, spelled the way the flag reads it.
const importDefaultMaxSize = "2G"

var importCmd = &cobra.Command{
	Use:   "import --from (<host>[:port] | <file.bffs> | -) [--account <name>] [--as-is] [--dry-run] [-y]",
	Short: "Land a .bffs bundle's sessions and auto-memory in a Claude config dir",
	Long: `Reads a bundle written by ` + "`bffs export`" + ` and commits its sessions and memory
into the projects/ pool of the target account (the account claude would use in
the current directory, else the active one; --account overrides; "home" is
~/.claude). The manifest is shown and confirmed before a single payload byte
is read; every file is verified against the manifest's sha256 in a staging
directory first, and each session lands transactionally — its transcript is
the last file to appear, so a session is either whole or absent.

A session whose original directory exists here is placed by identity (same
directory, no rehome needed); every other session lands as-is under its
original slug and is flagged pending — ` + "`claude --resume <id>`" + ` still finds it
from anywhere. An existing session with the same id is skipped unless
--on-conflict overwrite sets it aside (never deleted). Memory is written only
when the project has none yet (--memory overwrite sets the existing directory
aside); imported memory is prompt content, so its pinned files arrive unpinned
unless --trust-memory. Old transcripts are raised to half of Claude's
retention window so they are not swept before they can be resumed.

With --from <host>[:port] the bundle comes straight from a machine running
` + "`bffs export --serve`" + ` on the same local network: the address is checked to be
on-link before anything is dialled (--allow-routed for multi-VLAN offices),
the pairing code shown over there is asked for once the connection is up
(typed with echo off, or taken from $BFFS_TRANSFER_CODE, which is cleared
right after reading — there is no --code flag), and the manifest is reviewed
and confirmed before a single payload byte is requested. Everything is
printed on stderr. A name (mac-a.local, mac-a) is resolved once; any host on
the network can answer such a name, so the IPv4 address shown on the other
machine is the safe form. A wrong code exits 2.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		if later := importLaterFlags(); len(later) > 0 {
			return fmt.Errorf("%s not available yet: import lands sessions by identity or --as-is for now", strings.Join(later, ", "))
		}
		if importFrom == "" {
			return errors.New(`--from is required: a host[:port] shown by bffs export --serve, a .bffs file, or "-" for stdin`)
		}
		maxSize, err := parseSize(importMaxSize)
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		req := importRequest{
			From:           importFrom,
			Account:        importAccount,
			AsIs:           importAsIs,
			OnConflict:     importOnConflict,
			Memory:         importMemory,
			TrustMemory:    importTrustMemory,
			PreserveMtimes: importPreserveMtimes,
			MaxSize:        maxSize,
			Force:          importForce,
			CleanStaging:   importCleanStaging,
			DryRun:         importDryRun,
			Yes:            importYes,
			AllowRouted:    importAllowRouted,
			AllowLoopback:  importAllowLoopback,
			ClaudeDir:      importClaudeDir,
			Cwd:            cwd,
			Now:            time.Now(),
		}
		req.Host, req.User = localIdentity()
		pr := newPrompter(cmd.InOrStdin(), cmd.OutOrStdout())
		return runImport(cmd, dir, pr, req, isTTY())
	},
}

func init() {
	f := importCmd.Flags()
	f.StringVar(&importFrom, "from", "", `a host[:port] shown by bffs export --serve, a .bffs file, or "-" for stdin`)
	f.StringVar(&importAccount, "account", "", "target account (default: the account claude would use here, else the active one; \"home\" = ~/.claude)")
	f.BoolVar(&importAsIs, "as-is", false, "place every session under its original slug, flagged pending, without looking for its directory here")
	f.StringVar(&importOnConflict, "on-conflict", string(porter.ConflictSkip), `an existing session with the same id: "skip" or "overwrite" (set aside, never deleted)`)
	f.StringVar(&importMemory, "memory", string(rehome.MemorySkip), `an existing memory directory: "skip" or "overwrite" (set aside, never deleted)`)
	f.BoolVar(&importTrustMemory, "trust-memory", false, "keep pinned: frontmatter on imported memory files (default: rewritten to pinned-imported:)")
	f.BoolVar(&importPreserveMtimes, "preserve-mtimes", false, "keep source mtimes on sidecar, file-history, plan and task files (Claude sweeps them when older than its retention window)")
	f.StringVar(&importMaxSize, "max-size", importDefaultMaxSize, "largest bundle payload accepted: 500M, 2G")
	f.BoolVar(&importForce, "force", false, "import a bundle whose id already has an import record")
	f.BoolVar(&importCleanStaging, "clean-staging", false, "remove leftover staging directories of interrupted imports after listing them")
	f.BoolVar(&importDryRun, "dry-run", false, "show the plan and write nothing")
	f.BoolVarP(&importYes, "yes", "y", false, "skip the confirmation")
	f.StringVar(&importInto, "into", "", "directory of the bundle's single project here (next milestones)")
	f.StringArrayVar(&importMap, "map", nil, "prefix rule old=new, repeatable (next milestones)")
	f.BoolVar(&importCarryTrust, "carry-trust", false, "also copy the source's trust answers for mapped directories (next milestones)")
	f.BoolVar(&importSetLastSession, "set-last-session", false, "point the account's lastSessionId at the newest imported session (next milestones)")
	f.BoolVar(&importForceStamp, "force-stamp", false, "stamp a transcript whose last line is incomplete (next milestones)")
	f.BoolVar(&importAllowRouted, "allow-routed", false, "with --from host, pair with a private address that is not on-link (multi-VLAN offices); prints a warning")
	f.BoolVar(&importAllowLoopback, "allow-loopback", false, "with --from host, allow a loopback address (tests and same-machine trials)")
	_ = f.MarkHidden("allow-loopback")
	f.StringVar(&importClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
	_ = f.MarkHidden("claude-dir")
	rootCmd.AddCommand(importCmd)
}

// importLaterFlags names the flags that are parsed now but wired in a
// later milestone.
func importLaterFlags() []string {
	var later []string
	if importInto != "" {
		later = append(later, "--into")
	}
	if len(importMap) > 0 {
		later = append(later, "--map")
	}
	if importCarryTrust {
		later = append(later, "--carry-trust")
	}
	if importSetLastSession {
		later = append(later, "--set-last-session")
	}
	if importForceStamp {
		later = append(later, "--force-stamp")
	}
	return later
}

// importRequest is one resolved `bffs import` invocation, independent of
// the flag variables so tests can drive runImport directly.
type importRequest struct {
	From           string
	Account        string
	AsIs           bool
	OnConflict     string
	Memory         string
	TrustMemory    bool
	PreserveMtimes bool
	MaxSize        int64
	Force          bool
	CleanStaging   bool
	DryRun         bool
	Yes            bool
	ClaudeDir      string
	Cwd            string
	Now            time.Time
	Stdin          io.Reader // the bundle for --from -; nil = cmd.InOrStdin()

	// --from host (plan §8): what counts as the local network, and how
	// this machine names itself in accept.
	AllowRouted   bool
	AllowLoopback bool
	Host, User    string
}

// fromKind is the shape of a --from value.
type fromKind int

const (
	fromFile fromKind = iota
	fromStdin
	fromHost
)

// parseFrom classifies --from (plan §5.2): "-" is stdin; an existing
// file, or any name ending in .bffs, is a file (a .bffs name that does not
// exist is an error — never a host lookup); anything else is a host[:port]
// that resolveHost turns into a checked literal. A file target comes back
// normalised.
func parseFrom(s string) (fromKind, string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return 0, "", errors.New(`--from is required: a .bffs file, or "-" for stdin`)
	case s == "-":
		return fromStdin, "", nil
	}
	if info, err := os.Stat(s); err == nil {
		if info.IsDir() {
			return 0, "", fmt.Errorf("bundle %q is a directory", s)
		}
		p, err := store.NormalizePath(s)
		if err != nil {
			return 0, "", err
		}
		return fromFile, p, nil
	}
	if strings.HasSuffix(strings.ToLower(s), bundle.Ext) {
		return 0, "", fmt.Errorf("bundle file %q not found", s)
	}
	return fromHost, s, nil
}

// parseSize reads a --max-size value: a number with an optional K, M, G
// or T suffix (binary units; a trailing B or iB is accepted).
func parseSize(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("invalid --max-size %q: use a size like 500M or 2G", s)
	}
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "IB")
	t = strings.TrimSuffix(t, "B")
	if t == "" {
		return bad()
	}
	mult := int64(1)
	switch t[len(t)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult != 1 {
		t = t[:len(t)-1]
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n <= 0 || n*float64(mult) > float64(1<<62) {
		return bad()
	}
	return int64(n * float64(mult)), nil
}

// importDest is where an import lands: the root, the account recorded for
// it ("" = the unmanaged home), and the Target line's description.
type importDest struct {
	root    transcripts.Root
	account string
	label   string
}

// resolveImportDest picks the destination (A-7): the named account's root
// (an api_key account writes to ~/.claude; "home" is ~/.claude itself),
// else the root of the account resolver.Resolve picks for cwd — which
// includes the global active account — else the home root. A resolution
// failure is a warning, not an error, and lands in the home root.
func resolveImportDest(dir string, env *catalogEnv, account, cwd string) (importDest, string, error) {
	if account != "" {
		root, err := env.rootForAccount(account)
		if err != nil {
			return importDest{}, "", err
		}
		d := importDest{root: root}
		if account != transcripts.HomeName {
			d.account = account
		}
		d.label = importTargetLabel(d.account, root, "--account")
		return d, "", nil
	}
	home, err := env.homeRoot()
	if err != nil {
		return importDest{}, "", err
	}
	fallback := importDest{root: home, label: importTargetLabel("", home, "")}
	r, err := resolver.Resolve(dir, cwd)
	if err != nil {
		return fallback, fmt.Sprintf("%v; importing into the home root", err), nil
	}
	if r.Source == resolver.SourceNone {
		return fallback, "", nil
	}
	d := importDest{root: home, account: r.Account.Name}
	if r.Account.Type == store.TypeOAuth {
		root, err := transcripts.RootFor(env.roots, r.Account.Name)
		if err != nil {
			return fallback, fmt.Sprintf("%v; importing into the home root", err), nil
		}
		d.root = root
	}
	how := "resolved for " + short(cwd)
	if r.Source == resolver.SourceGlobal {
		how = "active account"
	}
	d.label = importTargetLabel(d.account, d.root, how)
	return d, "", nil
}

// importTargetLabel renders the Target line's subject: `account "work"
// (resolved for ~/x → shared pool ~/.claude)` or `home (~/.claude, unmanaged)`.
func importTargetLabel(account string, root transcripts.Root, how string) string {
	where := short(root.ConfigDir)
	switch {
	case root.Owner != "":
		where = "own root " + where
	case root.Shared:
		where = "shared pool " + where
	}
	if account == "" {
		return fmt.Sprintf("home (%s, unmanaged claude)", short(root.ConfigDir))
	}
	if how != "" {
		return fmt.Sprintf("account %q (%s → %s)", account, how, where)
	}
	return fmt.Sprintf("account %q (%s)", account, where)
}

// retentionLabel renders Claude's retention window for the Target line
// from transcripts.CleanupPeriodDays: "30 days (~/.claude/settings.json)",
// "30 days (default)", "never (…)", or the unreadable case.
func retentionLabel(configDir string, days int, source string) string {
	switch source {
	case transcripts.CleanupSourceInvalid:
		return "unknown (settings unreadable; transcript mtimes preserved)"
	case transcripts.CleanupSourceDefault:
		return fmt.Sprintf("%d days (default)", days)
	}
	file := short(filepath.Join(configDir, source))
	if days == 0 {
		return fmt.Sprintf("never (%s)", file)
	}
	return fmt.Sprintf("%d days (%s)", days, file)
}

// formatLimit renders --max-size in binary units: 2 GiB → "2.0 GB".
func formatLimit(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%.0f KB", float64(n)/float64(1<<10))
	}
}

// runImport shows the manifest, confirms, and runs porter.Import. Nothing
// is written before the confirmation; the receipt, the verify lines and
// the record path end the run. A failure exits 1 and names the staging
// directory when one was kept.
func runImport(cmd *cobra.Command, dir string, pr *prompter, req importRequest, tty bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	kind, target, err := parseFrom(req.From)
	if err != nil {
		return err
	}
	switch porter.ConflictPolicy(req.OnConflict) {
	case "", porter.ConflictSkip, porter.ConflictOverwrite:
	default:
		return fmt.Errorf("invalid --on-conflict %q: must be %q or %q", req.OnConflict, porter.ConflictSkip, porter.ConflictOverwrite)
	}
	switch rehome.MemoryMode(req.Memory) {
	case "", rehome.MemorySkip, rehome.MemoryOverwrite:
	case rehome.MemoryMerge:
		return fmt.Errorf("--memory merge is not available yet: use %q or %q", rehome.MemorySkip, rehome.MemoryOverwrite)
	default:
		return fmt.Errorf("invalid --memory %q: must be %q or %q", req.Memory, rehome.MemorySkip, rehome.MemoryOverwrite)
	}
	if kind == fromStdin && !req.Yes && !req.DryRun {
		return errors.New("--from - reads the bundle from stdin, which leaves no terminal for the confirmation; pass -y")
	}

	env, err := loadCatalogEnv(dir, req.ClaudeDir)
	if err != nil {
		return err
	}
	dest, warning, err := resolveImportDest(dir, env, req.Account, req.Cwd)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(errOut, "warning:", warning)
	}
	limits := bundle.DefaultLimits
	if req.MaxSize > 0 {
		limits.MaxTotalBytes = req.MaxSize
	}

	if kind == fromHost {
		// Same reader, prompts on stderr: stdout stays clean on the host
		// path, the way it does for a bundle on stdout.
		pr = &prompter{r: pr.r, out: errOut}
		out = errOut
	}
	if req.CleanStaging {
		ok, err := cleanStaging(dir, pr, out, req.Yes, tty)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	if kind == fromHost {
		return runImportFromHost(cmd, dir, env, dest, limits, pr, req, tty, target)
	}

	var r io.Reader
	switch kind {
	case fromStdin:
		r = req.Stdin
		if r == nil {
			r = cmd.InOrStdin()
		}
	default:
		f, err := os.Open(target)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	r = bufio.NewReaderSize(r, 256<<10)

	// The manifest first: everything the user sees before answering
	// comes from it, and nothing past it is read until they have.
	m, raw, rest, err := bundle.PeekManifest(r)
	if err != nil {
		return exitWith(1, fmt.Errorf("bundle: %w", err))
	}
	if err := m.Validate(limits); err != nil {
		return exitWith(1, fmt.Errorf("bundle: %w", err))
	}
	days, source := transcripts.CleanupPeriodDays(dest.root.ConfigDir)
	sum := newImportSummary(m, dest, req)
	sum.Limit = limits.MaxTotalBytes
	sum.Retention = retentionLabel(dest.root.ConfigDir, days, source)
	renderImportSummary(out, sum)

	if req.DryRun {
		fmt.Fprintln(out, "dry run: nothing is written")
	} else {
		ok, err := confirmOrAbort(pr, fmt.Sprintf("Import into %s? [y/N] ", short(dest.root.ConfigDir)), req.Yes, tty)
		if err != nil {
			return fmt.Errorf("refusing to import: %w", err)
		}
		if !ok {
			return nil
		}
	}

	ctx := cmdContext(cmd)
	live, err := transcripts.Live(ctx, env.configDirs())
	if err != nil {
		fmt.Fprintln(errOut, "warning: liveness unavailable:", err)
	}
	digest := sha256.Sum256(raw)
	opts := porter.ImportOptions{
		Dest:                 dest.root,
		Account:              dest.account,
		OnConflict:           porter.ConflictPolicy(req.OnConflict),
		Memory:               rehome.MemoryMode(req.Memory),
		TrustMemory:          req.TrustMemory,
		AsIs:                 req.AsIs,
		PreserveMtimes:       req.PreserveMtimes,
		Force:                req.Force,
		DryRun:               req.DryRun,
		Limits:               limits,
		ExpectManifestSHA256: hex.EncodeToString(digest[:]),
		Now:                  req.Now,
		Live:                 live,
		LaunchEnv:            os.Environ(),
	}
	start := time.Now()
	rep, err := porter.Import(ctx, dir, rest, opts)
	elapsed := time.Since(start)
	for _, w := range rep.Warnings {
		fmt.Fprintln(errOut, "warning:", transcripts.Sanitize(w))
	}
	receipt := newImportReceipt(dir, m, rep, req.DryRun, elapsed)
	if err != nil {
		if len(rep.Imported)+len(rep.Pending)+len(rep.MemoryDirs) > 0 {
			fmt.Fprintln(out, "import stopped; what landed before the failure:")
			renderImportReceipt(out, receipt)
		}
		if rep.StagingDir != "" {
			fmt.Fprintf(errOut, "staging kept at %s (inspect it, then rerun with --clean-staging)\n", rep.StagingDir)
		}
		return exitWith(1, err)
	}
	renderImportReceipt(out, receipt)
	return nil
}

// cleanStaging lists the leftover staging directories under the bffs
// config dir and removes them once the user agrees. ok is false when the
// user declined (the run stops there).
func cleanStaging(dir string, pr *prompter, out io.Writer, yes, tty bool) (bool, error) {
	stale, err := porter.StaleStaging(dir)
	if err != nil {
		return false, err
	}
	if len(stale) == 0 {
		fmt.Fprintln(out, "no leftover staging directories")
		return true, nil
	}
	fmt.Fprintf(out, "leftover staging of interrupted imports (%s):\n", countNoun(len(stale), "directory"))
	for _, p := range stale {
		fmt.Fprintf(out, "  %s\n", short(p))
	}
	ok, err := confirmOrAbort(pr, "remove them? [y/N] ", yes, tty)
	if err != nil || !ok {
		return false, err
	}
	for _, p := range stale {
		if err := os.RemoveAll(p); err != nil {
			return false, fmt.Errorf("remove %s: %w", p, err)
		}
		fmt.Fprintf(out, "removed %s\n", short(p))
	}
	return true, nil
}

// importSummary is the manifest as shown before the confirmation (plan
// §5.10, B side): the bundle header, one block per project, the target.
type importSummary struct {
	Header    string // "6f1e2c0a from mac-a (jonas, darwin/arm64, …)"
	Host      string
	Projects  []importProject
	AsIs      bool
	Trust     bool // --trust-memory
	Target    string
	Limit     int64
	Retention string
}

// importProject is one project of a bundle and how it will be placed.
type importProject struct {
	Label       string // the original directory (sanitised), or the slug
	Slug        string
	Exists      bool // the directory exists here → identity placement
	Sessions    int
	Bytes       int64
	MemoryFiles int
	Trust       string // "accepted" / "not accepted" / ""
}

// newImportSummary groups the manifest's entries by project and decides
// the placement line the way porter will: identity when the directory
// exists here and --as-is is not set, as-is otherwise.
func newImportSummary(m *bundle.Manifest, dest importDest, req importRequest) importSummary {
	s := importSummary{Host: transcripts.Sanitize(m.Source.Hostname), AsIs: req.AsIs, Trust: req.TrustMemory, Target: dest.label}
	parts := []string{}
	if u := transcripts.Sanitize(m.Source.User); u != "" {
		parts = append(parts, u)
	}
	if m.Source.OS != "" || m.Source.Arch != "" {
		parts = append(parts, transcripts.Sanitize(m.Source.OS+"/"+m.Source.Arch))
	}
	if v := transcripts.Sanitize(m.BFFSVersion); v != "" {
		parts = append(parts, "bffs "+v)
	}
	if v := transcripts.Sanitize(m.ClaudeVersion); v != "" {
		parts = append(parts, "claude "+v)
	}
	if a := transcripts.Sanitize(m.Source.Account); a != "" {
		parts = append(parts, fmt.Sprintf("account %q", a))
	}
	if iso := transcripts.Sanitize(m.Source.Isolation); iso != "" {
		parts = append(parts, iso)
	}
	s.Header = fmt.Sprintf("%s from %s", short8(m.BundleID), s.Host)
	if len(parts) > 0 {
		s.Header += " (" + strings.Join(parts, ", ") + ")"
	}

	index := map[string]int{}
	group := func(key, label, slug string) *importProject {
		if i, ok := index[key]; ok {
			return &s.Projects[i]
		}
		index[key] = len(s.Projects)
		s.Projects = append(s.Projects, importProject{Label: label, Slug: slug})
		return &s.Projects[len(s.Projects)-1]
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		cwd := transcripts.Sanitize(e.Cwd)
		slug := transcripts.Sanitize(e.Slug)
		key, label := cwd, cwd
		if cwd == "" {
			key, label = "projects/"+slug, "projects/"+slug+" (no directory recorded)"
		}
		p := group(key, label, slug)
		// The same rule porter applies: only an absolute path that is a
		// directory here is "the same directory" (a relative or "~" cwd
		// would resolve against this process, not the source machine).
		if e.Cwd != "" && filepath.IsAbs(e.Cwd) && isDir(e.Cwd) {
			p.Exists = true
		}
		switch e.Kind {
		case bundle.EntrySession:
			p.Sessions++
			for _, f := range e.Files {
				p.Bytes += f.Size
			}
			if e.SourceTrust != nil && p.Trust == "" {
				p.Trust = "not accepted"
				if e.SourceTrust.Accepted {
					p.Trust = "accepted"
				}
			}
		case bundle.EntryMemory:
			p.MemoryFiles += len(e.Files)
		}
	}
	return s
}

// renderImportSummary prints the block the user confirms. Every string
// from the manifest was sanitised when the summary was built.
func renderImportSummary(w io.Writer, s importSummary) {
	fmt.Fprintf(w, "Bundle %s:\n", s.Header)
	for _, p := range s.Projects {
		placement := "exists here ✗ → imported as-is; rehome later with bffs rehome or /bffs-rehome in claude"
		switch {
		case s.AsIs:
			placement = fmt.Sprintf("imported as-is under projects/%s/ (--as-is); rehome later with bffs rehome or /bffs-rehome in claude", p.Slug)
		case p.Exists:
			placement = "exists here ✓ (same directory — no rehome needed)"
		}
		fmt.Fprintf(w, "  project %s        %s\n", p.Label, placement)
		line := fmt.Sprintf("    %s  %s", countNoun(p.Sessions, "session"), formatSize(p.Bytes))
		if p.MemoryFiles > 0 {
			line += fmt.Sprintf("   memory %d files", p.MemoryFiles)
		}
		if p.Trust != "" {
			line += fmt.Sprintf("   (trust: %s on %s — informational)", p.Trust, s.Host)
		}
		fmt.Fprintln(w, line)
		if p.MemoryFiles > 0 {
			target := p.Label
			if !p.Exists || s.AsIs {
				target = "projects/" + p.Slug + "/memory (as-is)"
			}
			fmt.Fprintf(w, "    note: memory files in this bundle will be loaded into every future claude session for %s\n", target)
			if s.Trust {
				fmt.Fprintln(w, "          (--trust-memory: pinned files stay pinned)")
			} else {
				fmt.Fprintln(w, "          (pinned files arrive unpinned; --trust-memory keeps them pinned)")
			}
		}
	}
	fmt.Fprintf(w, "Target: %s   limit %s   retention: %s\n", s.Target, formatLimit(s.Limit), s.Retention)
}

// importReceipt is everything the post-import block needs.
type importReceipt struct {
	Report     porter.Report
	Record     *imports.Record // the record written; nil on a dry run or when none was saved
	Manifest   *bundle.Manifest
	DryRun     bool
	Elapsed    time.Duration
	RecordPath string
}

// newImportReceipt reads the record porter saved (when it did) so the
// receipt can name the projects/ directories sessions landed in.
func newImportReceipt(cfgDir string, m *bundle.Manifest, rep porter.Report, dryRun bool, elapsed time.Duration) importReceipt {
	r := importReceipt{Report: rep, Manifest: m, DryRun: dryRun, Elapsed: elapsed, RecordPath: imports.Path(cfgDir, m.BundleID)}
	if !dryRun {
		if rec, ok := imports.Exists(cfgDir, m.BundleID); ok && len(rec.Sessions) > 0 {
			r.Record = &rec
		}
	}
	return r
}

// renderImportReceipt prints what happened (plan §5.10): sessions,
// memory, history, the verify lines, the trust note, the record path.
func renderImportReceipt(w io.Writer, r importReceipt) {
	rep := r.Report
	id8 := short8(rep.BundleID)
	landed := len(rep.Imported) + len(rep.Pending)

	// Sessions.
	verb := "committed"
	if r.DryRun {
		verb = "would be committed"
	}
	var line string
	switch dirs := landingDirs(r); {
	case landed == 0:
		line = "none " + verb
	case len(dirs) == 0:
		line = fmt.Sprintf("%d %s", landed, verb)
	default:
		line = fmt.Sprintf("%d %s to %s", landed, verb, strings.Join(dirs, ", "))
	}
	line += fmt.Sprintf("; %d skipped", len(rep.Skipped)+len(rep.Held))
	if n := len(rep.Overwritten); n > 0 {
		line += fmt.Sprintf("; %d existing set aside as *.bffs-replaced-<time> (never deleted)", n)
	}
	if n := len(rep.MtimeRaised); n > 0 {
		line += fmt.Sprintf("; %s raised to the retention floor", countNoun(n, "mtime"))
	}
	fmt.Fprintf(w, "  sessions   %s\n", line)
	if rep.SweptCount > 0 && !rep.SweepDate.IsZero() {
		fmt.Fprintf(w, "             %s will be swept by Claude on %s unless resumed\n", countNoun(rep.SweptCount, "session"), rep.SweepDate.Local().Format("2006-01-02"))
	}
	for _, sid := range rep.Held {
		fmt.Fprintf(w, "             held %s: %s\n", short8(sid), transcripts.Sanitize(rep.Reasons[sid]))
	}
	for _, sid := range rep.Skipped {
		fmt.Fprintf(w, "             skipped %s: %s\n", short8(sid), transcripts.Sanitize(rep.Reasons[sid]))
	}

	// Memory.
	if len(rep.MemoryDirs) == 0 {
		fmt.Fprintln(w, "  memory     nothing written")
	} else {
		verb := "into"
		if r.DryRun {
			verb = "would be written to"
		}
		files := memoryFileCounts(r)
		for _, dir := range rep.MemoryDirs {
			n, ok := files[filepath.Clean(dir)]
			what := "files"
			if ok {
				what = fmt.Sprintf("%d files", n)
			}
			fmt.Fprintf(w, "  memory     %s %s %s (side files: *.imported-%s.md; MEMORY.md untouched)\n", what, verb, short(dir), id8)
		}
	}

	// History.
	switch {
	case r.DryRun:
	case rep.HistoryLines == 0:
		fmt.Fprintln(w, "  history    no new prompt lines")
	default:
		fmt.Fprintf(w, "  history    %s added\n", countNoun(rep.HistoryLines, "prompt line"))
	}

	if r.DryRun {
		fmt.Fprintln(w, "Dry run — nothing written. Afterwards, check it with:")
	} else {
		fmt.Fprintf(w, "Done in %.1fs. Check it:\n", r.Elapsed.Seconds())
	}
	fmt.Fprintln(w)
	for _, line := range rep.Verify {
		fmt.Fprintf(w, "    %s\n", transcripts.Sanitize(line))
	}
	for _, dir := range identityDirs(r) {
		fmt.Fprintf(w, "    bffs sessions list --project %s\n", rehome.ShellQuote(transcripts.Sanitize(dir)))
	}
	for _, rv := range memoryReviews(r) {
		fmt.Fprintf(w, "    bffs memory scan-paths --project %s   # %s to review\n", rehome.ShellQuote(transcripts.Sanitize(rv.cwd)), countNoun(rv.lines, "line"))
	}
	if n := len(rep.Pending); n > 0 {
		fmt.Fprintf(w, "    pending: %s imported as-is (directory missing here) — bffs sessions list --pending-rehome; rehome later with bffs rehome or /bffs-rehome in claude\n", countNoun(n, "session"))
	}
	if landed > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "note: the first claude launch there asks about folder trust (and external CLAUDE.md imports, if the project's CLAUDE.md")
		fmt.Fprintln(w, "      imports files outside the directory) — once per bffs account. `bffs trust sync --to <acct>` carries the answer to other accounts.")
	}
	if !r.DryRun {
		fmt.Fprintf(w, "Import record: %s  (bffs sessions imports)\n", short(r.RecordPath))
	}
}

// identityDirs lists, sorted, the distinct directories the landed
// identity placements belong to — the ones `bffs sessions list --project`
// shows them under. The record knows them; without one (a dry run) the
// manifest's cwds that exist here stand in, by the same rule porter uses.
func identityDirs(r importReceipt) []string {
	imported := map[string]bool{}
	for _, sid := range r.Report.Imported {
		imported[sid] = true
	}
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		if dir != "" && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	switch {
	case r.Record != nil:
		for _, s := range r.Record.Sessions {
			if imported[s.ID] && s.Status == imports.StatusPlaced {
				add(s.NewCwd)
			}
		}
	case r.Manifest != nil:
		for _, e := range r.Manifest.Entries {
			if e.Kind == bundle.EntrySession && imported[e.SessionID] && filepath.IsAbs(e.Cwd) && isDir(e.Cwd) {
				add(e.Cwd)
			}
		}
	}
	sort.Strings(out)
	return out
}

// memoryReview is one `bffs memory scan-paths` line: the project directory
// and how many of its imported memory lines still mention source paths.
type memoryReview struct {
	cwd   string
	lines int
}

// memoryReviews pairs every memory directory the report flags for review
// with the directory it was placed for. Only identity placements have one
// here (the record's OldCwd is the local directory then); an as-is memory
// directory keeps the warning the report already carries.
func memoryReviews(r importReceipt) []memoryReview {
	if r.Record == nil || len(r.Report.MemoryReview) == 0 {
		return nil
	}
	var out []memoryReview
	for _, m := range r.Record.Memories {
		n, ok := r.Report.MemoryReview[m.Dir]
		if !ok || m.Status != imports.StatusPlaced || m.OldCwd == "" {
			continue
		}
		out = append(out, memoryReview{cwd: m.OldCwd, lines: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cwd < out[j].cwd })
	return out
}

// landingDirs names the projects/ directories the landed sessions went
// to, from the record when it exists (identity placements land under the
// directory Claude uses here) and from the manifest otherwise, marking
// the as-is ones.
func landingDirs(r importReceipt) []string {
	pending := map[string]bool{}
	for _, sid := range r.Report.Pending {
		pending[sid] = true
	}
	landed := map[string]bool{}
	for _, sid := range r.Report.Imported {
		landed[sid] = true
	}
	for sid := range pending {
		landed[sid] = true
	}
	slugs := map[string]bool{}
	var order []string
	add := func(slug string, isPending bool) {
		key := "projects/" + transcripts.Sanitize(slug) + "/"
		if isPending {
			key += " (as-is, pending rehome)"
		}
		if !slugs[key] {
			slugs[key] = true
			order = append(order, key)
		}
	}
	if r.Record != nil {
		for _, s := range r.Record.Sessions {
			if landed[s.ID] {
				add(s.Slug, s.Status == imports.StatusPending)
			}
		}
	} else if r.Manifest != nil {
		for _, e := range r.Manifest.Entries {
			if e.Kind == bundle.EntrySession && landed[e.SessionID] {
				add(e.Slug, pending[e.SessionID])
			}
		}
	}
	sort.Strings(order)
	return order
}

// memoryFileCounts maps each memory directory the record says was written
// to the number of files its bundle entry carried; entries and record
// rows share the manifest order.
func memoryFileCounts(r importReceipt) map[string]int {
	out := map[string]int{}
	if r.Record == nil || r.Manifest == nil {
		return out
	}
	i := 0
	for _, e := range r.Manifest.Entries {
		if e.Kind != bundle.EntryMemory {
			continue
		}
		if i < len(r.Record.Memories) {
			mem := r.Record.Memories[i]
			if mem.Status != imports.StatusSkipped {
				out[filepath.Clean(mem.Dir)] = len(e.Files)
			}
		}
		i++
	}
	return out
}

// ---- --from host: receive from bffs export --serve on the LAN (plan §8) ----

// envTransferCode carries the pairing code for scripted runs; it is read
// once and cleared immediately (plan §5.2: there is no --code flag, argv
// leaks via ps). The transfer package owns no environment variable, so the
// name lives here.
const envTransferCode = "BFFS_TRANSFER_CODE"

// Injection seams for the loopback end-to-end test and the resolver
// table: how a fetch dials (nil is a net.Dialer inside transfer), which
// local addresses it sees, and how it resolves a name.
var (
	fetchDial   func(ctx context.Context, network, addr string) (net.Conn, error)
	fetchLocal  = transfer.LANAddrs
	fetchLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
)

// resolveHost turns a --from host[:port] into the literal to dial (plan
// §8.6): an address with an optional port ("192.168.1.20", "[fe80::1%en0]",
// "10.0.0.5:7345"), or a name resolved once through lookup. Every address
// must lie on a local network of this machine (transfer.IsLAN against
// local) before anything is dialled, or the refusal names the fix. A
// .local or single-label name warns that any host on the network can
// answer it. Nothing is dialled here.
func resolveHost(ctx context.Context, s string, local []transfer.LinkAddr, lan transfer.LANOptions, lookup func(context.Context, string) ([]netip.Addr, error), warn io.Writer) (netip.AddrPort, error) {
	host, port, err := splitFromHost(s)
	if err != nil {
		return netip.AddrPort{}, err
	}
	refuse := func(ip netip.Addr) error {
		return fmt.Errorf("refusing to pair with %s: %w. Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -", ip.WithZone(""), transfer.ErrNotLAN)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is6() && ip.IsLinkLocalUnicast() && ip.Zone() == "" {
			return netip.AddrPort{}, fmt.Errorf("link-local address %s needs an interface: [%s%%<iface>], e.g. [%s%%en0]", ip, ip, ip)
		}
		if !transfer.IsLAN(ip, local, lan) {
			return netip.AddrPort{}, refuse(ip)
		}
		return netip.AddrPortFrom(ip, port), nil
	}
	if !validHostname(host) {
		return netip.AddrPort{}, fmt.Errorf("invalid host %q in --from: use the address shown on the other machine", host)
	}
	if strings.HasSuffix(strings.ToLower(host), ".local") || !strings.Contains(host, ".") {
		fmt.Fprintln(warn, "warning: any host on this network can answer that name — the IPv4 address shown on the other machine is the safe form")
	}
	addrs, err := lookup(ctx, host)
	if err != nil || len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("could not resolve %q: use the IP address shown on the other machine", host)
	}
	var pick netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !transfer.IsLAN(a, local, lan) {
			return netip.AddrPort{}, refuse(a)
		}
		if !pick.IsValid() || (a.Is4() && !pick.Is4()) {
			pick = a
		}
	}
	return netip.AddrPortFrom(pick, port), nil
}

// splitFromHost separates a --from target into host and port: "[v6]",
// "[v6]:p", "v6" (two or more colons), "v4", "v4:p", "name", "name:p".
// The port defaults to the serve default.
func splitFromHost(s string) (string, uint16, error) {
	s = strings.TrimSpace(s)
	host, portS := s, ""
	switch {
	case strings.HasPrefix(s, "["):
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", 0, fmt.Errorf("invalid --from %q: missing ]", s)
		}
		host = s[1:end]
		rest := s[end+1:]
		switch {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			portS = rest[1:]
		default:
			return "", 0, fmt.Errorf("invalid --from %q: use [address]:port", s)
		}
	case strings.Count(s, ":") >= 2:
		// A bare IPv6 literal; a port needs brackets.
	case strings.Contains(s, ":"):
		host, portS, _ = strings.Cut(s, ":")
	}
	if host == "" {
		return "", 0, fmt.Errorf("invalid --from %q: no host", s)
	}
	port := uint16(servePortDefault)
	if portS != "" {
		n, err := strconv.Atoi(portS)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid port %q in --from %q: use 1-65535", portS, s)
		}
		port = uint16(n)
	}
	return host, port, nil
}

// validHostname accepts DNS-shaped names: letters, digits, "-" and "."
// labels, no empty label, at most 253 bytes.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.HasSuffix(h, "-") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// runImportFromHost is the B side of plan §8.5: the target is resolved
// and on-link-checked, the connection comes up, the code is asked for,
// the manifest is reviewed and confirmed exactly like a file's, and the
// body streams into porter.Import. Everything human goes to stderr. Exit
// codes (§5.9): 2 on a wrong code, 130 on Ctrl-C, 1 otherwise.
func runImportFromHost(cmd *cobra.Command, dir string, env *catalogEnv, dest importDest, limits bundle.Limits, pr *prompter, req importRequest, tty bool, target string) error {
	errOut := cmd.ErrOrStderr()
	lan := transfer.LANOptions{AllowLoopback: req.AllowLoopback, AllowRouted: req.AllowRouted}
	local, err := fetchLocal(lan)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmdContext(cmd), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first Ctrl-C cuts the connection and unwinds; restoring the
	// default behaviour then lets a second one end the process even while
	// the code prompt or a commit is blocking (commits are journalled).
	context.AfterFunc(ctx, stop)
	addr, err := resolveHost(ctx, target, local, lan, fetchLookup, errOut)
	if err != nil {
		return exitWith(1, err)
	}
	if req.AllowRouted {
		fmt.Fprintln(errOut, "warning: --allow-routed: the on-link check is off; only the pairing code protects this transfer")
	}

	fp := &fetchPrinter{w: errOut, peer: fromTarget(addr.Addr(), addr.Port()), bar: newProgressBar(errOut, "receiving", isTerminalWriter(errOut))}
	fp.bar.verified = true
	var (
		manifest *bundle.Manifest
		digest   string
		rep      porter.Report
		sinkRan  bool
		sinkErr  error
		elapsed  time.Duration
	)
	confirm := func(raw []byte) (bool, error) {
		var m bundle.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return false, fmt.Errorf("manifest from %s: %w", fp.peer, err)
		}
		if err := m.Validate(limits); err != nil {
			return false, fmt.Errorf("bundle: %w", err)
		}
		sum := sha256.Sum256(raw)
		digest = hex.EncodeToString(sum[:])
		manifest = &m
		days, source := transcripts.CleanupPeriodDays(dest.root.ConfigDir)
		s := newImportSummary(&m, dest, req)
		s.Limit = limits.MaxTotalBytes
		s.Retention = retentionLabel(dest.root.ConfigDir, days, source)
		renderImportSummary(errOut, s)
		if req.DryRun {
			fmt.Fprintln(errOut, "dry run: nothing is written")
			return true, nil
		}
		ok, err := confirmOrAbort(pr, fmt.Sprintf("Import into %s? [y/N] ", short(dest.root.ConfigDir)), req.Yes, tty)
		if err != nil {
			return false, fmt.Errorf("refusing to import: %w", err)
		}
		return ok, nil
	}
	sink := func(ctx context.Context, raw []byte, body io.Reader) (transfer.Done, error) {
		sinkRan = true
		live, err := transcripts.Live(ctx, env.configDirs())
		if err != nil {
			fp.println("warning: liveness unavailable: " + err.Error())
		}
		opts := porter.ImportOptions{
			Dest:                 dest.root,
			Account:              dest.account,
			OnConflict:           porter.ConflictPolicy(req.OnConflict),
			Memory:               rehome.MemoryMode(req.Memory),
			TrustMemory:          req.TrustMemory,
			AsIs:                 req.AsIs,
			PreserveMtimes:       req.PreserveMtimes,
			Force:                req.Force,
			Limits:               limits,
			ExpectManifestSHA256: digest,
			Now:                  req.Now,
			Progress:             fp.progress,
			Live:                 live,
			LaunchEnv:            os.Environ(),
			StreamMode:           true,
		}
		start := time.Now()
		rep, sinkErr = porter.Import(ctx, dir, bufio.NewReaderSize(body, 256<<10), opts)
		elapsed = time.Since(start)
		d := transfer.Done{Entries: manifest.Totals.Files, Bytes: rep.Bytes}
		if sinkErr != nil {
			d.Reason = sinkErr.Error()
		}
		return d, sinkErr
	}
	_, err = transfer.Fetch(ctx, transfer.FetchOptions{
		Addr:    addr,
		Code:    func() (transfer.Code, error) { return readPairingCode(pr, tty) },
		Confirm: confirm,
		Sink:    sink,
		Events:  fp.event,
		LAN:     lan,
		Dial:    fetchDial,
		DryRun:  req.DryRun,
		Version: Version,
		Host:    req.Host,
		User:    req.User,
		Local:   local,
	})
	fp.interrupt()
	for _, w := range rep.Warnings {
		fmt.Fprintln(errOut, "warning:", transcripts.Sanitize(w))
	}
	switch {
	case err == nil && req.DryRun:
		fmt.Fprintf(errOut, "Dry run — nothing requested from %s; it keeps serving.\n", fp.peer)
		return nil
	case err == nil:
		fp.end()
		renderImportReceipt(errOut, newImportReceipt(dir, manifest, rep, false, elapsed))
		return nil
	case errors.Is(err, transfer.ErrBadCode):
		return exitWith(2, err)
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		reportPartialImport(errOut, dir, manifest, rep, elapsed)
		return exitWith(130, errors.New("interrupted; the connection was closed"))
	case sinkErr != nil:
		reportPartialImport(errOut, dir, manifest, rep, elapsed)
		return exitWith(1, err)
	case sinkRan:
		// Everything landed and verified; only the completion report to
		// the other machine failed (it went away first). The import is
		// whole, so the receipt stands and the run succeeds — the other
		// side is the one that shows a failure.
		fp.end()
		renderImportReceipt(errOut, newImportReceipt(dir, manifest, rep, false, elapsed))
		fmt.Fprintf(errOut, "warning: %v — the import itself is complete; the other machine may show it as unfinished\n", err)
		return nil
	default:
		return exitWith(1, err)
	}
}

// reportPartialImport prints what landed before a failure and where the
// staging directory was kept, the way the file path does.
func reportPartialImport(w io.Writer, dir string, m *bundle.Manifest, rep porter.Report, elapsed time.Duration) {
	if m != nil && len(rep.Imported)+len(rep.Pending)+len(rep.MemoryDirs) > 0 {
		fmt.Fprintln(w, "import stopped; what landed before the failure:")
		renderImportReceipt(w, newImportReceipt(dir, m, rep, false, elapsed))
	}
	if rep.StagingDir != "" {
		fmt.Fprintf(w, "staging kept at %s (inspect it, then rerun with --clean-staging)\n", rep.StagingDir)
	}
}

// readPairingCode takes the code from $BFFS_TRANSFER_CODE — cleared the
// moment it is read, so no child inherits it — else from the terminal with
// echo off, else (piped input) from one line of the shared prompter, so a
// later confirmation still sees the rest of the input. transfer.Fetch
// calls it once the connection is up, so it follows the "connected to …"
// line, which it ends. The code is never echoed.
func readPairingCode(pr *prompter, tty bool) (transfer.Code, error) {
	if v, ok := os.LookupEnv(envTransferCode); ok {
		_ = os.Unsetenv(envTransferCode)
		fmt.Fprintln(pr.out, "(from $"+envTransferCode+")")
		return transfer.ParseCode(v)
	}
	if tty {
		s, err := promptSecret(pr.out, "")
		if err != nil {
			return transfer.Code{}, err
		}
		return transfer.ParseCode(s)
	}
	s, err := pr.line("")
	if err != nil {
		return transfer.Code{}, err
	}
	if strings.TrimSpace(s) == "" {
		// pr.line already ended the prompt line on exhausted input.
		return transfer.Code{}, errors.New("no pairing code given: type it on a terminal, or set $" + envTransferCode)
	}
	fmt.Fprintln(pr.out)
	return transfer.ParseCode(s)
}

// fetchPrinter renders the B-side lines of plan §5.10 from transfer
// events: the connect line that ends in the code prompt, the "code
// accepted" line, and the receiving bar. Failures are not printed here —
// Fetch returns them and the command reports them once.
type fetchPrinter struct {
	mu   sync.Mutex
	w    io.Writer
	peer string
	bar  *progressBar
}

func (p *fetchPrinter) event(ev transfer.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.Kind {
	case "connect":
		fmt.Fprintf(p.w, "connected to %s (TLS 1.3, peer key %s) — it asks for the pairing code: ", p.peer, keyFromText(ev.Text, "peer key "))
	case "code-ok":
		fmt.Fprintln(p.w, "code accepted — the other machine proved it knows the code too")
	}
}

func (p *fetchPrinter) progress(pr bundle.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.update(pr)
}

func (p *fetchPrinter) println(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.interrupt()
	fmt.Fprintln(p.w, s)
}

func (p *fetchPrinter) interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.interrupt()
}

func (p *fetchPrinter) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bar.end()
}
