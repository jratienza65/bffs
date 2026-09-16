package mcpserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// The write half of the session/memory transfer tools (plan §10.3, §10.4):
// export_bundle, import_bundle and rehome are the MCP twins of
// `bffs export --out`, `bffs import --from <file>` and `bffs rehome`,
// narrowed to what a model may do without a person at the keyboard. They
// share porter and rehome with the CLI, so placement, collisions,
// liveness, mtimes and records follow the same rules; what they never do
// is merge memory (imported memory is prompt content), carry trust
// answers, overwrite or delete anything, or leave this machine — a bundle
// is a local file at both ends, and the LAN transfer stays on the CLI.
// Every rendered string passes transcripts.Sanitize; no output carries
// transcript content, memory contents or a secret.

const (
	// mcpBundleMaxBytes caps the payload a bundle may carry through the
	// MCP tools: a synchronous tool call has minutes, not hours, and a
	// larger bundle belongs to the CLI where progress is visible.
	mcpBundleMaxBytes = 1 << 30

	// memoryNeverMergedNote is repeated by import_bundle and rehome: the
	// one policy those tools cannot be talked out of.
	memoryNeverMergedNote = "Memory is never merged over MCP: imported auto-memory is prompt content that would be injected into every future claude session for the project, so merging it is a human decision made on the CLI (bffs import --memory, bffs rehome)."

	// trustNotCarriedNote explains the dialog a person will see after a
	// transfer, and why no tool here answers it for them.
	trustNotCarriedNote = "Trust is not carried: claude asks the folder-trust and external-imports dialogs once per account for a directory it has not seen; trust_status shows which account has answered and names the `bffs trust sync` a person can run."

	// exportSensitiveNote is the export tool's standing caveat.
	exportSensitiveNote = "The bundle contains conversation content (transcripts, tool results, plan files, memory files); treat it as sensitive. No credentials are included."
)

// tempDirs lists the directories a bundle must never be written under:
// the OS temp dir and the conventional /tmp spellings. A variable so tests,
// whose whole fixture lives under the temp dir, can point it elsewhere.
var tempDirs = defaultTempDirs

func defaultTempDirs() []string {
	return []string{"/tmp", "/private/tmp", os.TempDir()}
}

// target is the root a write tool works in, the account it is recorded
// for (named; "" for the unmanaged home) and whose .claude.json a
// --set-last-session write goes to, and — for an export — the account
// and isolation the manifest's source block names.
type target struct {
	root       transcripts.Root
	named      string
	account    store.Account
	isolation  store.IsolationPreset
	claudeJSON string
}

// targetFor picks the target the way `bffs import`/`bffs rehome` do
// (plan §9.2, A-7): a named account is that account's root (an api_key
// account works in ~/.claude, "home" is ~/.claude itself); with no
// account it is the root of the account the resolver picks for dir
// (BFFS_ACCOUNT, bffs.toml, directory rule, then the global default),
// else the home root. A failed resolution is a warning and lands in the
// home root, never an error.
func (c *catalog) targetFor(account, dir string) (target, string, error) {
	if account != "" {
		root, _, err := c.rootFor(account, dir)
		if err != nil {
			return target{}, "", err
		}
		t := target{root: root, claudeJSON: root.ClaudeJSON}
		if acc, ok := c.accs.Get(account); ok {
			t.named = account
			t.account = acc
			if acc.Type == store.TypeOAuth {
				t.isolation = store.ResolveIsolation(acc.Isolation, c.state.Isolation)
				t.claudeJSON = filepath.Join(sessions.Dir(c.cfgDir, account), claudejson.Filename)
			}
		} else if root.Owner != "" {
			t.named = root.Owner
		}
		return t, "", nil
	}
	home, err := c.homeRoot()
	if err != nil {
		return target{}, "", err
	}
	fallback := target{root: home, claudeJSON: home.ClaudeJSON}
	r, err := resolver.Resolve(c.cfgDir, dir)
	if err != nil {
		return fallback, fmt.Sprintf("%v; using the home root", err), nil
	}
	if r.Source == resolver.SourceNone {
		return fallback, "", nil
	}
	t := target{root: home, named: r.Account.Name, account: r.Account, claudeJSON: home.ClaudeJSON}
	if r.Account.Type != store.TypeOAuth {
		return t, "", nil
	}
	root, err := transcripts.RootFor(c.roots, r.Account.Name)
	if err != nil {
		return fallback, fmt.Sprintf("%v; using the home root", err), nil
	}
	t.root = root
	t.isolation = store.ResolveIsolation(r.Account.Isolation, c.state.Isolation)
	t.claudeJSON = filepath.Join(sessions.Dir(c.cfgDir, r.Account.Name), claudejson.Filename)
	return t, "", nil
}

// checkBundleSize refuses a bundle whose payload exceeds the MCP ceiling.
func checkBundleSize(m *bundle.Manifest) error {
	if m.Totals.Bytes > mcpBundleMaxBytes {
		return fmt.Errorf("bundle payload is %d bytes (%.1f GiB), over the %d GiB ceiling of the MCP tools; use the CLI for bundles over 1 GiB (bffs export / bffs import)",
			m.Totals.Bytes, float64(m.Totals.Bytes)/float64(1<<30), mcpBundleMaxBytes>>30)
	}
	return nil
}

// parseMappings turns (old, new) pairs into prefix rules with
// rehome.ParseMapping's semantics — both sides normalised, new absolute —
// and requires every new directory to exist here, so a rule that would
// only produce refusals fails up front with a message naming it.
func parseMappings(pairs []RehomeMapping) ([]rehome.Mapping, error) {
	if len(pairs) == 0 {
		return nil, errors.New("no mapping given: pass at least one {old, new} prefix rule (old = the directory recorded in the sessions, new = where the project lives on this machine)")
	}
	out := make([]rehome.Mapping, 0, len(pairs))
	for _, p := range pairs {
		old, dst := strings.TrimSpace(p.Old), strings.TrimSpace(p.New)
		switch {
		case old == "":
			return nil, errors.New("mapping: old is required (the directory recorded in the sessions)")
		case dst == "":
			return nil, fmt.Errorf("mapping for %q: new is required (the directory the project lives in here)", old)
		case strings.Contains(old, "="):
			return nil, fmt.Errorf("mapping: old path %q contains '=', which the rule syntax cannot spell", old)
		}
		norm, err := normalizeDir(dst)
		if err != nil {
			return nil, fmt.Errorf("mapping for %q: new path %q: %w", old, dst, err)
		}
		m, err := rehome.ParseMapping(old + "=" + norm)
		if err != nil {
			return nil, err
		}
		if info, err := os.Stat(m.New); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("mapping for %q: new path %q is not a directory on this machine", old, dst)
		}
		out = append(out, m)
	}
	return out, nil
}

// mappingsOf turns import_bundle's old→new map into rules in key order,
// so a run is reproducible whatever the map's iteration order.
func mappingsOf(m map[string]string) []RehomeMapping {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]RehomeMapping, 0, len(keys))
	for _, k := range keys {
		out = append(out, RehomeMapping{Old: k, New: m[k]})
	}
	return out
}

// resolveExisting resolves the symlinks of p's longest existing ancestor
// (store.NormalizePath resolves only a path that exists as a whole), so a
// file that does not exist yet under /var/… on macOS compares equal to a
// directory known as /private/var/….
func resolveExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	parent := filepath.Dir(p)
	if parent == p {
		return filepath.Clean(p)
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(p))
}

// underDir reports whether p is dir or inside it (both cleaned;
// case-insensitive on Windows).
func underDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		p, dir = strings.ToLower(p), strings.ToLower(dir)
	}
	if p == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(p, dir)
}

// exportOutputPath validates and normalises export_bundle's output: an
// absolute path ending in .bffs that does not exist yet and lies outside
// every Claude config dir (~/.claude and the per-account session dirs),
// the bffs config dir and the temp directories — the places a bundle
// would either be swept, mistaken for Claude's own data, or picked up by
// the wrong tool.
func (h *handlers) exportOutputPath(c *catalog, raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("output is required: an absolute path ending in .bffs, outside ~/.claude, the bffs config dir and the temp directory")
	}
	out, err := store.NormalizePath(raw)
	if err != nil {
		return "", fmt.Errorf("output %q: %w", raw, err)
	}
	out = resolveExisting(out)
	if !filepath.IsAbs(out) {
		return "", fmt.Errorf("output %q is not an absolute path", raw)
	}
	if !strings.HasSuffix(strings.ToLower(out), bundle.Ext) {
		return "", fmt.Errorf("output %q must end in %s", raw, bundle.Ext)
	}
	type forbidden struct{ dir, why string }
	var dirs []forbidden
	for _, r := range c.roots {
		dirs = append(dirs, forbidden{r.ConfigDir, "a Claude config dir"})
	}
	dirs = append(dirs, forbidden{h.cfgDir, "the bffs config dir"})
	for _, d := range tempDirs() {
		dirs = append(dirs, forbidden{d, "a temp directory"})
	}
	for _, f := range dirs {
		if f.dir == "" {
			continue
		}
		if underDir(out, resolveExisting(f.dir)) {
			return "", fmt.Errorf("output %q is under %s (%s); write the bundle somewhere else, e.g. a directory of your own", raw, transcripts.Sanitize(f.dir), f.why)
		}
	}
	if _, err := os.Lstat(out); err == nil {
		return "", fmt.Errorf("output %q exists; export_bundle never overwrites a file", raw)
	}
	return out, nil
}

// writeBundleFile claims path exclusively (O_EXCL, 0600 — an existing
// file is refused, never replaced), streams the bundle into a temporary
// file beside it and renames that over the claimed name once it is
// complete and synced, so an interrupted export never leaves a truncated
// .bffs under the final name. src is closed either way.
func writeBundleFile(ctx context.Context, path string, m *bundle.Manifest, src bundle.Opener, opts porter.ExportOptions) (n int64, err error) {
	release := func() {
		if c, ok := src.(io.Closer); ok {
			_ = c.Close()
		}
	}
	claim, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		release()
		if errors.Is(err, os.ErrExist) {
			return 0, fmt.Errorf("output %q exists; export_bundle never overwrites a file", path)
		}
		return 0, err
	}
	if err := claim.Close(); err != nil {
		release()
		_ = os.Remove(path)
		return 0, err
	}
	done := false
	defer func() {
		if !done {
			_ = os.Remove(path)
		}
	}()

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		release()
		return 0, err
	}
	tmpPath := tmp.Name()
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		release()
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

type ExportBundleIn struct {
	Account       string   `json:"account,omitempty" jsonschema:"bffs account whose root to export from (home = the unmanaged ~/.claude tree); empty = the account claude would use in directory, else the home root. An api_key account maps to home"`
	Directory     string   `json:"directory,omitempty" jsonschema:"project directory whose sessions and auto-memory to export; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly. Ignored when only sessions are named"`
	Sessions      []string `json:"sessions,omitempty" jsonschema:"session ids (or unique prefixes of at least 8 hex digits) to export instead of, or in addition to, the project's"`
	Only          string   `json:"only,omitempty" jsonschema:"restrict to sessions or memories; empty exports both"`
	NoToolResults bool     `json:"no_tool_results,omitempty" jsonschema:"leave out tool-results/ (saved tool outputs, which may contain pasted secrets)"`
	NoFileHistory bool     `json:"no_file_history,omitempty" jsonschema:"leave out file-history/ (the file backups behind claude's rewind)"`
	Output        string   `json:"output" jsonschema:"path of the .bffs file to create: absolute, ending in .bffs, not existing yet, outside ~/.claude, the bffs config dir and the temp directory"`
}

type ExportBundleOut struct {
	BundlePath string `json:"bundle_path"`
	BundleID   string `json:"bundle_id"`
	Sessions   int    `json:"sessions" jsonschema:"sessions in the bundle"`
	MemoryDirs int    `json:"memory_dirs" jsonschema:"auto-memory directories in the bundle"`
	SizeBytes  int64  `json:"size_bytes" jsonschema:"size of the file written"`
	Note       string `json:"note"`
}

// exportBundle writes a .bffs bundle to a local file: the same selection
// and format as `bffs export --out`, minus the LAN serve (a person at
// both ends) and stdout.
func (h *handlers) exportBundle(ctx context.Context, req *mcp.CallToolRequest, in ExportBundleIn) (*mcp.CallToolResult, ExportBundleOut, error) {
	var out ExportBundleOut
	ctx, cancel := context.WithTimeout(ctx, runMaxTimeoutSecs*time.Second)
	defer cancel()

	switch in.Only {
	case "", porter.OnlySessions, porter.OnlyMemories:
	default:
		return nil, out, fmt.Errorf("invalid only %q: must be %q, %q or empty", in.Only, porter.OnlySessions, porter.OnlyMemories)
	}
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	c, err := h.loadCatalog()
	if err != nil {
		return nil, out, err
	}
	t, warning, err := c.targetFor(in.Account, dir)
	if err != nil {
		return nil, out, err
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	if t.root.Orphan {
		warnings = append(warnings, rootLabel(t.root))
	}
	path, err := h.exportOutputPath(c, in.Output)
	if err != nil {
		return nil, out, err
	}

	live, err := transcripts.Live(ctx, c.configDirs())
	if err != nil {
		warnings = append(warnings, "liveness unavailable: "+err.Error())
	}
	now := time.Now()
	selOpts := porter.SelectOptions{Sessions: in.Sessions, Only: in.Only, IncludeLive: true, Now: now}
	if len(in.Sessions) == 0 || in.Directory != "" {
		selOpts.Projects = []string{dir}
	}
	sel, err := porter.Select(ctx, t.root, nil, live, selOpts)
	if err != nil {
		return nil, out, err
	}
	parts := porter.DefaultParts
	parts.ToolResults = !in.NoToolResults
	parts.FileHistory = !in.NoFileHistory
	sel.Parts = parts

	opts := porter.ExportOptions{
		Compression: bundle.CompGzip,
		Version:     h.version,
		Account:     t.account,
		Isolation:   t.isolation,
		Now:         now,
	}
	m, opener, buildWarnings, err := porter.BuildManifest(ctx, sel, opts)
	if err != nil {
		return nil, out, err
	}
	warnings = append(warnings, buildWarnings...)
	if err := checkBundleSize(m); err != nil {
		if closer, ok := opener.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, out, err
	}
	size, err := writeBundleFile(ctx, path, m, opener, opts)
	if err != nil {
		return nil, out, fmt.Errorf("export: %w", err)
	}

	nSessions, nMemories := 0, 0
	for _, e := range m.Entries {
		switch e.Kind {
		case bundle.EntrySession:
			nSessions++
		case bundle.EntryMemory:
			nMemories++
		}
	}
	out.BundlePath = transcripts.Sanitize(path)
	out.BundleID = transcripts.Sanitize(m.BundleID)
	out.Sessions = nSessions
	out.MemoryDirs = nMemories
	out.SizeBytes = size
	summary := fmt.Sprintf("Wrote %s (%d bytes, %d session(s), %d memory director(ies), bundle %s) from %s. %s Land it on another machine or account with `bffs import --from <file>` or the import_bundle tool.",
		out.BundlePath, size, nSessions, nMemories, transcripts.Sanitize(short8(m.BundleID)), rootLabel(t.root), exportSensitiveNote)
	out.Note = joinNote(summary, "", sanitizeAll(warnings))
	return nil, out, nil
}

type ImportBundleIn struct {
	BundlePath     string            `json:"bundle_path" jsonschema:"path of a .bffs file on this machine (written by bffs export or export_bundle); the LAN transfer is CLI-only"`
	Account        string            `json:"account,omitempty" jsonschema:"bffs account whose root receives the sessions (home = the unmanaged ~/.claude tree); empty = the account claude would use in the server's working directory, else home. An api_key account maps to home"`
	OnConflict     string            `json:"on_conflict,omitempty" jsonschema:"an existing session with the same id: skip (the default and the only value here; overwrite is CLI-only)"`
	Memory         string            `json:"memory,omitempty" jsonschema:"what to do with the bundle's auto-memory: skip (the default and the only value here; overwrite and merge are CLI-only)"`
	Rehome         map[string]string `json:"rehome,omitempty" jsonschema:"prefix rules old directory -> new directory (longest prefix first) placing sessions whose recorded directory does not exist here; every new directory must exist on this machine"`
	AsIs           bool              `json:"as_is,omitempty" jsonschema:"land every session under its original directory entry without placing it (pending rehome); cannot be combined with rehome"`
	SetLastSession bool              `json:"set_last_session,omitempty" jsonschema:"point the account's lastSessionId for each landed directory at its newest imported session (claude --continue there opens it)"`
	DryRun         bool              `json:"dry_run,omitempty" jsonschema:"report the plan and write nothing"`
}

type ImportBundleOut struct {
	BundleID string   `json:"bundle_id"`
	Imported []string `json:"imported" jsonschema:"session ids that landed under a directory of this machine (by identity, or mapped with a relocated record)"`
	Skipped  []string `json:"skipped" jsonschema:"session ids not written (already present, or refused); the note says why"`
	Pending  []string `json:"pending" jsonschema:"session ids landed as-is under their original entry, waiting for rehome"`
	Held     []string `json:"held" jsonschema:"session ids refused because a running claude owns them"`
	Verify   []string `json:"verify" jsonschema:"one 'cd <dir> && claude --resume <id>' line per landed directory, for a person to check the result"`
	Note     string   `json:"note"`
}

// importBundle lands a local .bffs file in a root: `bffs import --from
// <file>` with the memory policy fixed at skip, the conflict policy at
// skip, trust never carried, and the bundle size capped.
func (h *handlers) importBundle(ctx context.Context, req *mcp.CallToolRequest, in ImportBundleIn) (*mcp.CallToolResult, ImportBundleOut, error) {
	out := ImportBundleOut{Imported: []string{}, Skipped: []string{}, Pending: []string{}, Held: []string{}, Verify: []string{}}
	ctx, cancel := context.WithTimeout(ctx, runMaxTimeoutSecs*time.Second)
	defer cancel()

	switch porter.ConflictPolicy(in.OnConflict) {
	case "", porter.ConflictSkip:
	case porter.ConflictOverwrite:
		return nil, out, fmt.Errorf("on_conflict %q is not available over MCP (setting an existing session aside is a human decision); use the CLI: bffs import --from <file> --on-conflict overwrite", in.OnConflict)
	default:
		return nil, out, fmt.Errorf("invalid on_conflict %q: must be %q or empty", in.OnConflict, porter.ConflictSkip)
	}
	switch rehome.MemoryMode(in.Memory) {
	case "", rehome.MemorySkip:
	case rehome.MemoryOverwrite, rehome.MemoryMerge:
		return nil, out, fmt.Errorf("memory %q is not available over MCP: imported auto-memory is prompt content, so bringing it in is a human decision; use the CLI: bffs import --from <file> --memory %s", in.Memory, in.Memory)
	default:
		return nil, out, fmt.Errorf("invalid memory %q: must be %q or empty (merge and overwrite are CLI-only)", in.Memory, rehome.MemorySkip)
	}
	if in.AsIs && len(in.Rehome) > 0 {
		return nil, out, errors.New("as_is and rehome are mutually exclusive: as_is lands every session under its original entry, rehome places it")
	}
	var maps []rehome.Mapping
	if len(in.Rehome) > 0 {
		var err error
		if maps, err = parseMappings(mappingsOf(in.Rehome)); err != nil {
			return nil, out, err
		}
	}
	if strings.TrimSpace(in.BundlePath) == "" {
		return nil, out, errors.New("bundle_path is required: a .bffs file on this machine")
	}
	path, err := store.NormalizePath(in.BundlePath)
	if err != nil {
		return nil, out, fmt.Errorf("bundle_path %q: %w", in.BundlePath, err)
	}
	if !strings.HasSuffix(strings.ToLower(path), bundle.Ext) {
		return nil, out, fmt.Errorf("bundle_path %q must end in %s", in.BundlePath, bundle.Ext)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, out, fmt.Errorf("bundle_path %q: %w", in.BundlePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, out, fmt.Errorf("bundle_path %q is not a regular file", in.BundlePath)
	}

	dir, err := normalizeDir("")
	if err != nil {
		return nil, out, err
	}
	c, err := h.loadCatalog()
	if err != nil {
		return nil, out, err
	}
	t, warning, err := c.targetFor(in.Account, dir)
	if err != nil {
		return nil, out, err
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, out, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256<<10)
	m, raw, rest, err := bundle.PeekManifest(r)
	if err != nil {
		return nil, out, fmt.Errorf("bundle: %w", err)
	}
	if err := checkBundleSize(m); err != nil {
		return nil, out, err
	}
	if err := m.Validate(bundle.DefaultLimits); err != nil {
		return nil, out, fmt.Errorf("bundle: %w", err)
	}
	out.BundleID = transcripts.Sanitize(m.BundleID)

	live, err := transcripts.Live(ctx, c.configDirs())
	if err != nil {
		warnings = append(warnings, "liveness unavailable: "+err.Error())
	}
	digest := sha256.Sum256(raw)
	opts := porter.ImportOptions{
		Dest:                 t.root,
		Account:              t.named,
		OnConflict:           porter.ConflictSkip,
		Memory:               rehome.MemorySkip,
		Map:                  maps,
		AsIs:                 in.AsIs,
		SetLastSession:       in.SetLastSession,
		DryRun:               in.DryRun,
		Limits:               bundle.DefaultLimits,
		ExpectManifestSHA256: hex.EncodeToString(digest[:]),
		Now:                  time.Now(),
		Live:                 live,
		LaunchEnv:            os.Environ(),
	}
	rep, err := porter.Import(ctx, h.cfgDir, rest, opts)
	out.Imported = sanitizeAll(rep.Imported)
	out.Skipped = sanitizeAll(rep.Skipped)
	out.Pending = sanitizeAll(rep.Pending)
	out.Held = sanitizeAll(rep.Held)
	out.Verify = sanitizeAll(rep.Verify)
	warnings = append(warnings, rep.Warnings...)
	for _, sid := range append(append([]string{}, rep.Held...), rep.Skipped...) {
		if reason := rep.Reasons[sid]; reason != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", short8(sid), reason))
		}
	}
	if err != nil {
		landed := len(rep.Imported) + len(rep.Pending) + len(rep.MemoryDirs)
		msg := fmt.Sprintf("import: %v", err)
		if landed > 0 {
			msg += fmt.Sprintf("; landed before the failure: %d session(s) imported, %d pending, %d memory director(ies)", len(rep.Imported), len(rep.Pending), len(rep.MemoryDirs))
		}
		if rep.StagingDir != "" {
			msg += fmt.Sprintf("; staging kept at %s (a person inspects it, then reruns with `bffs import --clean-staging`)", rep.StagingDir)
		}
		return nil, out, errors.New(transcripts.Sanitize(msg))
	}

	summary := fmt.Sprintf("Imported %d session(s) into %s (%d mapped with a relocated record, %d pending as-is under their original entry - the rehome tool places those; %d skipped, %d held by a running claude).",
		len(rep.Imported), rootLabel(t.root), len(rep.Rehomed), len(rep.Pending), len(rep.Skipped), len(rep.Held))
	if in.DryRun {
		summary = "Dry run: nothing was written; this is the plan. " + strings.Replace(summary, "Imported", "Would import", 1)
	}
	if n := len(rep.MemoryDirs); n > 0 {
		summary += fmt.Sprintf(" %d memory director(ies) were written where the target had none (skip mode; topic files land as *.imported-<id>.md and MEMORY.md is left alone).", n)
	} else {
		summary += " Memory directories were skipped."
	}
	if rep.SweptCount > 0 {
		summary += fmt.Sprintf(" %d transcript(s) had their mtime raised into claude's retention window; claude sweeps them on %s unless resumed.", rep.SweptCount, rep.SweepDate.Format("2006-01-02"))
	}
	summary += " " + memoryNeverMergedNote + " " + trustNotCarriedNote + " A person checks the result with the verify lines."
	out.Note = joinNote(summary, "", sanitizeAll(warnings))
	return nil, out, nil
}

type RehomeMapping struct {
	Old string `json:"old" jsonschema:"directory prefix recorded in the sessions (on the source machine, possibly another OS); an exact directory is the degenerate case"`
	New string `json:"new" jsonschema:"directory prefix on this machine it maps to; the derived directory must exist"`
}

type RehomeIn struct {
	Mappings       []RehomeMapping `json:"mappings" jsonschema:"prefix rules, applied longest old prefix first, to the sessions' recorded working directories"`
	BundleID       string          `json:"bundle_id,omitempty" jsonschema:"only the sessions of this import record (bundle id or unique prefix; see list_sessions bundle_id)"`
	Account        string          `json:"account,omitempty" jsonschema:"bffs account whose root to rehome in (home = the unmanaged ~/.claude tree); empty = the account claude would use in the server's working directory, else home. An api_key account maps to home"`
	Sessions       []string        `json:"sessions,omitempty" jsonschema:"only these session ids (or unique prefixes of at least 8 hex digits)"`
	RewriteMemory  *bool           `json:"rewrite_memory,omitempty" jsonschema:"rewrite the old directory prefixes in the memory files that were copied along; default true"`
	SetLastSession bool            `json:"set_last_session,omitempty" jsonschema:"point the account's lastSessionId for each new directory at its newest moved session (claude --continue there opens it)"`
	DryRun         bool            `json:"dry_run,omitempty" jsonschema:"report the plan and move nothing"`
}

type RehomeMove struct {
	SessionID    string `json:"session_id"`
	From         string `json:"from" jsonschema:"transcript path before the move"`
	To           string `json:"to" jsonschema:"transcript path after the move (the same path when only the relocated record is appended)"`
	NewCwd       string `json:"new_cwd" jsonschema:"the directory the relocated record names"`
	SidecarMoved bool   `json:"sidecar_moved" jsonschema:"the <id>/ sidecar directory moved along"`
	Stamped      bool   `json:"stamped" jsonschema:"the relocated record was appended (false in a dry run)"`
}

type RehomeOut struct {
	Moves                    []RehomeMove `json:"moves" jsonschema:"the sessions the rules cover, in plan order"`
	MemoryFilesRewritten     []string     `json:"memory_files_rewritten" jsonschema:"memory files in which old directory prefixes were rewritten"`
	MemoryLinesStillAbsolute []string     `json:"memory_lines_still_absolute" jsonschema:"file:line of memory lines that still mention absolute paths, for a person to review"`
	Held                     []string     `json:"held" jsonschema:"session ids refused because a running claude owns them"`
	Skipped                  []string     `json:"skipped" jsonschema:"session ids refused for another reason; the note says why"`
	Verify                   []string     `json:"verify" jsonschema:"one 'cd <dir> && claude --resume <id>' line per new directory"`
	Note                     string       `json:"note"`
}

// rehomeSessions moves sessions to the directory their project lives in
// now: `bffs rehome --map` with memory never merged (skip mode: copied
// only where the new directory has no memory yet), no forced stamps and
// no trust carry.
func (h *handlers) rehomeSessions(ctx context.Context, req *mcp.CallToolRequest, in RehomeIn) (*mcp.CallToolResult, RehomeOut, error) {
	out := RehomeOut{Moves: []RehomeMove{}, MemoryFilesRewritten: []string{}, MemoryLinesStillAbsolute: []string{}, Held: []string{}, Skipped: []string{}, Verify: []string{}}
	ctx, cancel := context.WithTimeout(ctx, runMaxTimeoutSecs*time.Second)
	defer cancel()

	maps, err := parseMappings(in.Mappings)
	if err != nil {
		return nil, out, err
	}
	dir, err := normalizeDir("")
	if err != nil {
		return nil, out, err
	}
	c, err := h.loadCatalog()
	if err != nil {
		return nil, out, err
	}
	t, warning, err := c.targetFor(in.Account, dir)
	if err != nil {
		return nil, out, err
	}
	if t.root.Orphan {
		return nil, out, fmt.Errorf("%s: an orphan session dir is read-only; re-add the account with `bffs login` or pass another account", rootLabel(t.root))
	}
	var warnings []string
	if warning != "" {
		warnings = append(warnings, warning)
	}
	var oldHome, bundleID string
	if in.BundleID != "" {
		rec, err := rehome.FindRecord(h.cfgDir, in.BundleID)
		if err != nil {
			return nil, out, err
		}
		oldHome, bundleID = rec.Source.Home, rec.BundleID
	}
	// A rehome moves transcripts; without knowing which sessions a
	// running claude owns nothing may move (plan §9.5).
	live, err := transcripts.Live(ctx, c.configDirs())
	if err != nil {
		return nil, out, fmt.Errorf("cannot tell which sessions are open in a running claude: %w; nothing was moved", err)
	}
	rewrite := in.RewriteMemory == nil || *in.RewriteMemory
	newHome, _ := os.UserHomeDir()
	now := time.Now()
	days, _ := transcripts.CleanupPeriodDays(t.root.ConfigDir)
	opts := rehome.Options{
		Sessions:       in.Sessions,
		BundleID:       bundleID,
		RewriteMemory:  rewrite,
		SetLastSession: in.SetLastSession,
		Memory:         rehome.MemorySkip,
		Mtime:          rehome.MtimePolicy{CleanupPeriodDays: days, Now: now},
		ClaudeJSON:     t.claudeJSON,
		OldHome:        oldHome,
		NewHome:        newHome,
		Now:            now,
		DryRun:         in.DryRun,
		CfgDir:         h.cfgDir,
		StagingDir:     filepath.Join(h.cfgDir, porter.StagingSubdir),
		Account:        t.named,
		Env:            os.Environ(),
	}
	plan, err := rehome.PlanRehome(ctx, t.root, live, maps, opts)
	if err != nil {
		return nil, out, err
	}
	warnings = append(warnings, plan.Warnings...)
	if in.Account == "" {
		warnings = append(warnings, h.rehomeAccountMismatch(t, plan.Moves)...)
	}
	res, applyErr := rehome.Apply(ctx, t.root, plan, opts)
	warnings = append(warnings, res.Warnings...)

	moved := map[string]bool{}
	for _, sid := range res.Moved {
		moved[sid] = true
	}
	for _, mv := range plan.Moves {
		out.Moves = append(out.Moves, RehomeMove{
			SessionID:    transcripts.Sanitize(mv.SessionID),
			From:         transcripts.Sanitize(mv.From),
			To:           transcripts.Sanitize(mv.To),
			NewCwd:       transcripts.Sanitize(mv.NewCwd),
			SidecarMoved: mv.Sidecar && moved[mv.SessionID],
			Stamped:      moved[mv.SessionID],
		})
	}
	for _, m := range res.Memory {
		for _, rel := range m.Rewritten {
			out.MemoryFilesRewritten = append(out.MemoryFilesRewritten, transcripts.Sanitize(filepath.Join(m.To, filepath.FromSlash(rel))))
		}
		for _, ref := range m.Remaining {
			out.MemoryLinesStillAbsolute = append(out.MemoryLinesStillAbsolute, transcripts.Sanitize(fmt.Sprintf("%s:%d", filepath.Join(m.To, filepath.FromSlash(ref.File)), ref.Line)))
		}
	}
	out.Held = sanitizeAll(res.Held)
	out.Skipped = sanitizeAll(res.Skipped)
	out.Verify = sanitizeAll(res.Verify)
	for _, r := range plan.Refusals {
		warnings = append(warnings, fmt.Sprintf("%s: %s", short8(r.SessionID), r.Reason))
	}
	if applyErr != nil {
		msg := fmt.Sprintf("rehome: %v; %d of %d session(s) moved", applyErr, len(res.Moved), len(plan.Moves))
		if res.JournalDir != "" {
			msg += fmt.Sprintf("; journal kept at %s (the next rehome or import repairs an interrupted move)", res.JournalDir)
		}
		return nil, out, errors.New(transcripts.Sanitize(msg))
	}

	var summary string
	switch {
	case len(plan.Moves) == 0 && len(plan.Refusals) > 0:
		summary = fmt.Sprintf("Nothing could be moved in %s: %d session(s) the rules cover were refused (held/skipped list them; the warnings say why).", rootLabel(t.root), len(plan.Refusals))
	case len(plan.Moves) == 0 && len(plan.Memory) == 0:
		summary = fmt.Sprintf("Nothing to rehome in %s: no session (in scope) records a directory the rules cover.", rootLabel(t.root))
	case in.DryRun:
		summary = fmt.Sprintf("Dry run: nothing was written; this is the plan. %d session(s) in %s would move, each gaining one relocated record; conversation content, mtimes and picker order stay unchanged.", len(plan.Moves), rootLabel(t.root))
	default:
		summary = fmt.Sprintf("Moved %d of %d session(s) in %s: a relocated record was appended to each transcript; conversation content, mtimes and picker order are unchanged. Nothing was deleted; the old memory directory, if any, stays in place.", len(res.Moved), len(plan.Moves), rootLabel(t.root))
	}
	if res.LastSessionSet != "" {
		summary += fmt.Sprintf(" lastSessionId now points at %s (claude --continue there opens it).", transcripts.Sanitize(short8(res.LastSessionSet)))
	}
	summary += " " + memoryNeverMergedNote + " " + trustNotCarriedNote
	out.Note = joinNote(summary, "", sanitizeAll(warnings))
	return nil, out, nil
}

// rehomeAccountMismatch warns, per new directory, when claude launched
// there would run as another oauth account than the one this rehome
// addresses (a bffs.toml or directory rule in the new place): the verify
// line and lastSessionId would then name the wrong account.
func (h *handlers) rehomeAccountMismatch(t target, moves []rehome.Move) []string {
	seen := map[string]bool{}
	var out []string
	for _, mv := range moves {
		if mv.NewCwd == "" || seen[mv.NewCwd] {
			continue
		}
		seen[mv.NewCwd] = true
		r, err := resolver.Resolve(h.cfgDir, mv.NewCwd)
		if err != nil || r.Source == resolver.SourceNone || r.Account.Type != store.TypeOAuth || r.Account.Name == t.named {
			continue
		}
		out = append(out, fmt.Sprintf("claude in %s runs as account %q (%s rule), not the account this rehome addresses; pass account %q so lastSessionId and the verify line address it", mv.NewCwd, r.Account.Name, r.Source, r.Account.Name))
	}
	return out
}

// short8 is the eight-character prefix of an id for messages.
func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
