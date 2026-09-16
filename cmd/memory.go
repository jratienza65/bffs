package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/transcripts"
)

var (
	memoryAccount     string
	memoryProject     string
	memoryAllProjects bool
	memoryJSON        bool
	memoryClaudeDir   string
)

// memoryIndexCap bounds how much of MEMORY.md is parsed for its index
// lines — far above Claude's own 25 000-character load limit.
const memoryIndexCap = 1 << 20

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Browse Claude Code's auto-memory directories and what they reference",
	Long: `Claude Code keeps a project's auto-memory under projects/<slug>/memory/ of the
config dir it runs with — a MEMORY.md index plus topic files — keyed by the
project's git root. Under partial isolation every bffs account reads the same
directory; a full-isolation account has its own.

` + "`bffs memory`" + ` lists the memory of the current project in every root (the
interactive browser is bare ` + "`bffs`" + `). ` + "`show`" + `
prints the index and the file table, and which accounts see the directory;
` + "`scan-paths`" + ` lists every absolute path and @-reference inside it — the lines
to review after a rehome, and the references that can raise Claude's
"external CLAUDE.md imports" dialog.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMemoryList(cmd)
	},
}

var memoryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List auto-memory directories",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMemoryList(cmd)
	},
}

var memoryShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show a project's memory: index entries, files, and who sees it",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMemoryShow(cmd)
	},
}

var memoryScanPathsCmd = &cobra.Command{
	Use:   "scan-paths",
	Short: "List absolute paths and @-references inside memory files",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMemoryScanPaths(cmd)
	},
}

func init() {
	for _, c := range []*cobra.Command{memoryCmd, memoryListCmd, memoryShowCmd, memoryScanPathsCmd} {
		f := c.Flags()
		f.StringVar(&memoryAccount, "account", "", "only the root this account works in (\"home\" = ~/.claude)")
		f.StringVar(&memoryProject, "project", "", "project directory (default: the current directory)")
		f.StringVar(&memoryClaudeDir, "claude-dir", "", "override the shared claude config dir (testing)")
		_ = f.MarkHidden("claude-dir")
		if c != memoryShowCmd {
			f.BoolVar(&memoryAllProjects, "all-projects", false, "every memory directory, not just the project's")
		}
		if c == memoryCmd || c == memoryListCmd {
			f.BoolVar(&memoryJSON, "json", false, "emit a JSON array instead of the table")
		}
	}
	memoryCmd.AddCommand(memoryListCmd, memoryShowCmd, memoryScanPathsCmd)
	rootCmd.AddCommand(memoryCmd)
}

// memoryQuery is one resolved memory request: the roots to look in and
// the project whose memory to pick ("" = every memory directory).
type memoryQuery struct {
	Roots   []transcripts.Root
	Project string
}

// selectMemories catalogs the memory directories q asks for: every one
// under the roots, or in each root the one Claude would use for Project.
// A root whose memory location is overridden (ErrMemoryDirOverridden) is
// reported in warnings and skipped.
func selectMemories(ctx context.Context, q memoryQuery) (mems []transcripts.Memory, warnings []string, err error) {
	all, err := transcripts.Memories(ctx, q.Roots)
	if err != nil {
		return nil, nil, err
	}
	if q.Project == "" {
		return all, nil, nil
	}
	byDir := map[string]transcripts.Memory{}
	for _, m := range all {
		byDir[filepath.Clean(m.Dir)] = m
	}
	for _, root := range q.Roots {
		dir, err := transcripts.MemoryDirFor(root, q.Project)
		if err != nil {
			if errors.Is(err, transcripts.ErrMemoryDirOverridden) {
				warnings = append(warnings, fmt.Sprintf("%s: %v", rootLabel(root), err))
				continue
			}
			return nil, nil, err
		}
		if m, ok := byDir[filepath.Clean(dir)]; ok {
			mems = append(mems, m)
		}
	}
	return mems, warnings, nil
}

// memoryRequest resolves the shared flags into the query and prints the
// root warnings. project is "" under --all-projects.
func memoryRequest(cmd *cobra.Command, env *catalogEnv, allProjects bool) (memoryQuery, error) {
	project := ""
	if !allProjects {
		var err error
		if project, err = targetDir([]string{memoryProject}, 0); err != nil {
			return memoryQuery{}, err
		}
	}
	// Every root by default: the ROOT column tells a shared directory from
	// a full-isolation account's own; --account narrows to one.
	all := memoryAccount == ""
	resolveDir := project
	if resolveDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			resolveDir = cwd
		}
	}
	roots, warnings, err := env.selectRoots(all, memoryAccount, resolveDir)
	if err != nil {
		return memoryQuery{}, err
	}
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
	return memoryQuery{Roots: roots, Project: project}, nil
}

func runMemoryList(cmd *cobra.Command) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, memoryClaudeDir)
	if err != nil {
		return err
	}
	q, err := memoryRequest(cmd, env, memoryAllProjects)
	if err != nil {
		return err
	}
	mems, warnings, err := selectMemories(cmdContext(cmd), q)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
	if memoryJSON {
		return writeMemoriesJSON(cmd.OutOrStdout(), mems)
	}
	return renderMemoryList(cmd.OutOrStdout(), mems, q.Project, time.Now())
}

// renderMemoryList prints one row per memory directory.
func renderMemoryList(w io.Writer, mems []transcripts.Memory, project string, now time.Time) error {
	if len(mems) == 0 {
		fmt.Fprintln(w, noMemoryLine(project))
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tROOT\tFILES\tINDEX\tMODIFIED\tABS-PATHS\t@REFS")
	for _, m := range mems {
		abs, at := memoryRefCounts(m)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d\t%d\n",
			transcripts.Sanitize(memoryProjectLabel(m)), memoryRootLabel(m.Root), len(m.Files), yesOrDash(m.HasIndex),
			humanizeAgo(memoryModTime(m), now), abs, at)
	}
	return tw.Flush()
}

func noMemoryLine(project string) string {
	if project == "" {
		return "no memory dirs"
	}
	return "no memory dir for " + short(project)
}

// memoryProjectLabel is the PROJECT cell: the git root the memory is keyed
// by, else the sessions' cwd, else the slug when no transcript says.
func memoryProjectLabel(m transcripts.Memory) string {
	switch {
	case m.GitRoot != "":
		return short(m.GitRoot)
	case m.Cwd != "":
		return short(m.Cwd)
	default:
		return m.Slug
	}
}

// memoryRootLabel is the ROOT cell: "shared" for the pool partial-isolation
// accounts read, the owner of a full-isolation root, "home" otherwise.
func memoryRootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return r.Owner + " (orphan)"
	case r.Owner != "":
		return r.Owner
	case r.Shared:
		return "shared"
	default:
		return transcripts.HomeName
	}
}

// memoryRefCounts totals the distinct absolute paths and @-references
// across the directory's files.
func memoryRefCounts(m transcripts.Memory) (abs, at int) {
	absSeen, atSeen := map[string]bool{}, map[string]bool{}
	for _, f := range m.Files {
		for _, p := range f.AbsolutePaths {
			absSeen[p] = true
		}
		for _, p := range f.AtRefs {
			atSeen[p] = true
		}
	}
	return len(absSeen), len(atSeen)
}

// memoryModTime is the newest file's mtime; zero for an empty directory.
func memoryModTime(m transcripts.Memory) time.Time {
	var t time.Time
	for _, f := range m.Files {
		if f.ModTime.After(t) {
			t = f.ModTime
		}
	}
	return t
}

func yesOrDash(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// memoryFileInfo and memoryInfo are the --json rows: the MemoryInfo shape
// of the list_memories MCP tool (plan §10.3), plus git_root.
type memoryFileInfo struct {
	Name          string   `json:"name"`
	SizeBytes     int64    `json:"size_bytes"`
	ModifiedAt    string   `json:"modified_at"`
	Pinned        bool     `json:"pinned"`
	AbsolutePaths []string `json:"absolute_paths"`
	AtRefs        []string `json:"at_refs"`
}

type memoryInfo struct {
	Root      string           `json:"root"`
	Cwd       string           `json:"cwd"`
	CwdExists bool             `json:"cwd_exists"`
	GitRoot   string           `json:"git_root"`
	Slug      string           `json:"slug"`
	Dir       string           `json:"dir"`
	HasIndex  bool             `json:"has_index"`
	Files     []memoryFileInfo `json:"files"`
}

func memoryInfoOf(m transcripts.Memory) memoryInfo {
	info := memoryInfo{
		Root:      m.Root.Dir,
		Cwd:       transcripts.Sanitize(m.Cwd),
		CwdExists: m.CwdExists,
		GitRoot:   transcripts.Sanitize(m.GitRoot),
		Slug:      m.Slug,
		Dir:       m.Dir,
		HasIndex:  m.HasIndex,
		Files:     make([]memoryFileInfo, 0, len(m.Files)),
	}
	for _, f := range m.Files {
		info.Files = append(info.Files, memoryFileInfo{
			Name:          f.Name,
			SizeBytes:     f.Size,
			ModifiedAt:    rfc3339(f.ModTime),
			Pinned:        f.Pinned,
			AbsolutePaths: nonNil(sanitizeAll(f.AbsolutePaths)),
			AtRefs:        nonNil(sanitizeAll(f.AtRefs)),
		})
	}
	return info
}

func sanitizeAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, transcripts.Sanitize(s))
	}
	return out
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func writeMemoriesJSON(w io.Writer, mems []transcripts.Memory) error {
	infos := make([]memoryInfo, 0, len(mems))
	for _, m := range mems {
		infos = append(infos, memoryInfoOf(m))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(infos)
}

// memoryEntry is one `- [title](file.md) - description` line of MEMORY.md.
type memoryEntry struct {
	Title, File, Description string
}

// memoryIndexLine matches Claude's index line shape, tolerating the dash
// variants and a missing description.
var memoryIndexLine = regexp.MustCompile(`^\s*[-*]\s+\[([^\]]*)\]\(([^)]*)\)\s*(?:[-–—:]\s*(.*))?$`)

// memoryIndexEntries parses the index lines of the MEMORY.md at path.
// A missing file is no entries and no error.
func memoryIndexEntries(path string) ([]memoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var entries []memoryEntry
	sc := bufio.NewScanner(io.LimitReader(f, memoryIndexCap))
	sc.Buffer(make([]byte, 64*1024), memoryIndexCap)
	for sc.Scan() {
		m := memoryIndexLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		entries = append(entries, memoryEntry{Title: m[1], File: m[2], Description: strings.TrimSpace(m[3])})
	}
	return entries, sc.Err()
}

// memoryView is everything `memory show` prints for one directory.
type memoryView struct {
	Memory  transcripts.Memory
	Project string // the directory asked about
	Key     string // its project key (git root)
	Entries []memoryEntry
}

func runMemoryShow(cmd *cobra.Command) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, memoryClaudeDir)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	project, err := targetDir([]string{memoryProject}, 0)
	if err != nil {
		return err
	}
	roots, warnings, err := env.selectRoots(false, memoryAccount, project)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}
	memDir, err := transcripts.MemoryDirFor(roots[0], project)
	if err != nil {
		return err
	}
	key, err := transcripts.ProjectKey(project)
	if err != nil {
		return err
	}
	mems, _, err := selectMemories(cmdContext(cmd), memoryQuery{Roots: roots, Project: project})
	if err != nil {
		return err
	}
	if len(mems) == 0 {
		fmt.Fprintf(out, "%s  (would be %s)\n", noMemoryLine(project), short(memDir))
		return nil
	}
	entries, err := memoryIndexEntries(filepath.Join(mems[0].Dir, transcripts.MemoryIndexFile))
	if err != nil {
		fmt.Fprintln(errOut, "warning: reading MEMORY.md:", err)
	}
	return renderMemoryShow(out, memoryView{Memory: mems[0], Project: project, Key: key, Entries: entries}, time.Now())
}

// renderMemoryShow prints the project, the directory and who sees it, the
// MEMORY.md index lines and the file table.
func renderMemoryShow(w io.Writer, v memoryView, now time.Time) error {
	m := v.Memory
	fmt.Fprintf(w, "project:  %s   (key: %s)\n", short(v.Project), v.Key)
	fmt.Fprintf(w, "memory:   %s   (%s)\n", short(m.Dir), memoryVisibility(m.Root))
	var index *transcripts.MemoryFile
	for i := range m.Files {
		if m.Files[i].Name == transcripts.MemoryIndexFile {
			index = &m.Files[i]
		}
	}
	if index == nil {
		fmt.Fprintf(w, "%s  (missing)\n", transcripts.MemoryIndexFile)
	} else {
		fmt.Fprintf(w, "%s  %s   modified %s\n", transcripts.MemoryIndexFile, countNoun(len(v.Entries), "entry"), humanizeAgo(index.ModTime, now))
		for _, e := range v.Entries {
			line := fmt.Sprintf("- [%s](%s)", transcripts.Sanitize(e.Title), transcripts.Sanitize(e.File))
			if e.Description != "" {
				line += " - " + transcripts.Sanitize(e.Description)
			}
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED\tPINNED\tABS-PATHS\t@REFS")
	for _, f := range m.Files {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n",
			transcripts.Sanitize(f.Name), formatSize(f.Size), humanizeAgo(f.ModTime, now), yesOrDash(f.Pinned), len(f.AbsolutePaths), len(f.AtRefs))
	}
	return tw.Flush()
}

// memoryVisibility says which accounts read a memory directory: every
// account attached to a shared pool, the owner of a full-isolation root,
// or the unmanaged home dir.
func memoryVisibility(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan session dir %s — no account reads it", r.Owner)
	case r.Owner != "":
		return "account: " + r.Owner
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool — visible to: " + strings.Join(r.Accounts, ", ")
	default:
		return "home — unmanaged " + short(r.ConfigDir)
	}
}

// scannedDir is one memory directory's path references.
type scannedDir struct {
	Dir  string
	Refs []transcripts.PathRef
}

func runMemoryScanPaths(cmd *cobra.Command) error {
	dir := mustConfigDir(cmd)
	env, err := loadCatalogEnv(dir, memoryClaudeDir)
	if err != nil {
		return err
	}
	q, err := memoryRequest(cmd, env, memoryAllProjects)
	if err != nil {
		return err
	}
	mems, warnings, err := selectMemories(cmdContext(cmd), q)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
	}
	if len(mems) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), noMemoryLine(q.Project))
		return nil
	}
	var scanned []scannedDir
	for _, m := range mems {
		refs, err := transcripts.ScanAbsolutePaths(m.Dir)
		if err != nil {
			return err
		}
		scanned = append(scanned, scannedDir{Dir: m.Dir, Refs: refs})
	}
	return renderScanPaths(cmd.OutOrStdout(), scanned)
}

// renderScanPaths prints one `file:line: path` per reference, @-references
// prefixed "@ref ". With more than one directory the file is prefixed by
// its directory so the lines stay unambiguous.
func renderScanPaths(w io.Writer, dirs []scannedDir) error {
	for _, d := range dirs {
		for _, r := range d.Refs {
			file := r.File
			if len(dirs) > 1 {
				file = filepath.ToSlash(filepath.Join(short(d.Dir), r.File))
			}
			prefix := ""
			if r.Kind == transcripts.PathKindAt {
				prefix = "@ref "
			}
			fmt.Fprintf(w, "%s%s:%d: %s\n", prefix, transcripts.Sanitize(file), r.Line, transcripts.Sanitize(r.Path))
		}
	}
	return nil
}
