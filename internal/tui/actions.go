package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// actionTarget is what an action screen works on: the root and project
// of the screen that pushed it, the sessions selected there (space) and,
// when nothing is selected, every session of the project. Every action
// calls the same internal/* functions the CLI does — porter, transfer,
// rehome, trust, runner — never the bffs binary.
type actionTarget struct {
	root    transcripts.Root
	slug    string
	project string                // decoded cwd of the project, "" when unknown
	rows    []transcripts.Session // every session of the project, in row order
	ids     []string              // selected session ids, in row order
}

// allIDs lists every session of the project.
func (t actionTarget) allIDs() []string {
	ids := make([]string, 0, len(t.rows))
	for _, s := range t.rows {
		ids = append(ids, s.ID)
	}
	return ids
}

// label names the project for headers and titles.
func (t actionTarget) label() string {
	if t.project != "" {
		return transcripts.Sanitize(shortPath(t.project))
	}
	return transcripts.Sanitize(t.slug)
}

// what says in a few words what the selection covers.
func (t actionTarget) what() string {
	if len(t.ids) > 0 {
		return countNoun(len(t.ids), "selected session")
	}
	return "the whole project " + t.label() + " and its memory"
}

// selectOptions is the porter.Select request for the target: the
// selected sessions, else the project directory (sessions + memory),
// else — a project without a recorded cwd — every listed session.
func (t actionTarget) selectOptions(now time.Time) porter.SelectOptions {
	o := porter.SelectOptions{IncludeLive: true, Now: now}
	switch {
	case len(t.ids) > 0:
		o.Sessions = t.ids
	case t.project != "":
		o.Projects = []string{t.project}
	default:
		o.Sessions = t.allIDs()
	}
	return o
}

// selection resolves the target against its root: liveness first (a
// warning, never a refusal), then porter.Select with DefaultParts.
func (t actionTarget) selection(ctx context.Context, svc *services) (porter.Selection, map[string]transcripts.LiveSession, []string, error) {
	var warnings []string
	live, err := transcripts.Live(ctx, svc.configDirs())
	if err != nil {
		warnings = append(warnings, "liveness unavailable: "+err.Error())
		live = nil
	}
	sel, err := porter.Select(ctx, t.root, svc.history[t.root.ConfigDir], live, t.selectOptions(svc.now()))
	if err != nil {
		return porter.Selection{}, live, warnings, err
	}
	sel.Parts = porter.DefaultParts
	return sel, live, warnings, nil
}

// destAccount names the account an import, copy or rehome into root is
// recorded under and resumed as: the root's owner (full isolation), else
// what the resolver picks for the process cwd, "" meaning the unmanaged
// home — the rule porter.ResumeAccount encodes.
func destAccount(svc *services, root transcripts.Root) string {
	return porter.ResumeAccount(svc.cfgDir, root, svc.cwd)
}

// rootForAccount is the root a named account works in: an api_key
// account runs claude against ~/.claude, so it maps to the home root;
// "home" is the home root itself.
func rootForAccount(svc *services, name string) (transcripts.Root, error) {
	if acc, ok := svc.accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return transcripts.RootFor(svc.roots, transcripts.HomeName)
	}
	return transcripts.RootFor(svc.roots, name)
}

// homeJSON is the ~/.claude.json the trust matrix lists under "home":
// beside the overridden ~/.claude in tests, "" (claudejson.Path) otherwise.
func (svc *services) homeJSON() string {
	if svc.homeClaudeDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(svc.homeClaudeDir), ".claude.json")
}

// hostIdent is the hostname the way a manifest constrains it
// (^[A-Za-z0-9_.-]{1,64}$), "host" when nothing survives.
func hostIdent() string {
	h, err := os.Hostname()
	if err != nil {
		return "host"
	}
	var sb strings.Builder
	for _, r := range h {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			sb.WriteRune(r)
		}
		if sb.Len() >= 64 {
			break
		}
	}
	if sb.Len() == 0 {
		return "host"
	}
	return sb.String()
}

// defaultBundleName is bffs-<host>-<yyyymmdd-hhmm>.bffs, the CLI's
// --out default.
func defaultBundleName(now time.Time) string {
	return fmt.Sprintf("bffs-%s-%s%s", hostIdent(), now.Format("20060102-1504"), bundle.Ext)
}

// rootLine names the root an export reads, with the accounts that share
// it — the CLI's "Exporting from …" line.
func rootLine(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan session dir %q (%s; read-only)", transcripts.Sanitize(r.Owner), shortPath(r.ConfigDir))
	case r.Owner != "":
		return fmt.Sprintf("account %q (%s; full isolation)", transcripts.Sanitize(r.Owner), shortPath(r.ConfigDir))
	case r.Shared && len(r.Accounts) > 0:
		return fmt.Sprintf("the shared pool (%s; partial isolation: %s)", shortPath(r.ConfigDir), accountList(r.Accounts))
	default:
		return fmt.Sprintf("%s (home, unmanaged)", shortPath(r.ConfigDir))
	}
}

// manifestProject is one project's share of a bundle, as the summary
// groups it.
type manifestProject struct {
	label        string
	sessions     int
	newest       string
	newestAt     time.Time
	bytes        int64
	live         int
	toolResults  int64
	fileHistory  int64
	historyLines int
	memoryFiles  int
	memoryBytes  int64
}

// manifestProjects groups a manifest's entries by project (cwd, else
// slug) in first-seen order. src may be nil; history lines are then not
// counted.
func manifestProjects(m *bundle.Manifest, src bundle.Opener) []manifestProject {
	var out []manifestProject
	index := map[string]int{}
	group := func(key, label string) *manifestProject {
		if i, ok := index[key]; ok {
			return &out[i]
		}
		index[key] = len(out)
		out = append(out, manifestProject{label: label})
		return &out[len(out)-1]
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		cwd := transcripts.Sanitize(e.Cwd)
		switch e.Kind {
		case bundle.EntrySession:
			key, label := cwd, shortPath(cwd)
			if cwd == "" {
				key, label = "projects/"+e.Slug, "projects/"+transcripts.Sanitize(e.Slug)+" (no cwd recorded)"
			}
			p := group(key, label)
			p.sessions++
			if e.LivePossiblyTruncated {
				p.live++
			}
			title := transcripts.Sanitize(e.Title)
			if p.sessions == 1 || e.Last.After(p.newestAt) {
				p.newestAt = e.Last
				if title != "" {
					p.newest = title
				}
			} else if p.newest == "" {
				p.newest = title
			}
			for _, f := range e.Files {
				p.bytes += f.Size
				kind, _, _, err := bundle.ClassifyName(f.Path)
				if err != nil {
					continue
				}
				switch kind {
				case bundle.NameSidecar:
					if strings.Contains(f.Path, "/tool-results/") {
						p.toolResults += f.Size
					}
				case bundle.NameFileHistory:
					p.fileHistory += f.Size
				case bundle.NameHistory:
					if src != nil {
						p.historyLines += countLines(src, f.Path)
					}
				}
			}
		case bundle.EntryMemory:
			key, label := cwd, shortPath(cwd)
			if cwd == "" {
				key, label = "memory/"+e.Slug, "memory/"+transcripts.Sanitize(e.Slug)+" (no cwd recorded)"
			}
			p := group(key, label)
			p.memoryFiles += len(e.Files)
			for _, f := range e.Files {
				p.memoryBytes += f.Size
			}
		}
	}
	return out
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

// exportSummaryLines is the CLI's confirmation block: the root line, one
// block per project with the three secret-bearing parts, and the total.
func exportSummaryLines(root transcripts.Root, m *bundle.Manifest, src bundle.Opener, parts porter.Parts, now time.Time) []string {
	lines := []string{"Exporting from " + rootLine(root) + ":"}
	for _, p := range manifestProjects(m, src) {
		lines = append(lines, "  project "+p.label)
		if p.sessions > 0 {
			line := "    " + countNoun(p.sessions, "session")
			if p.newest != "" {
				line += fmt.Sprintf("   (newest: %q, %s)", p.newest, humanizeAgo(p.newestAt, now))
			}
			line += "   " + formatSize(p.bytes)
			if p.live > 0 {
				line += fmt.Sprintf("   %d live (may be truncated)", p.live)
			}
			lines = append(lines, line,
				fmt.Sprintf("      tool-results %s (saved tool outputs — may contain pasted secrets)   file-history %s (backups of files Claude edited)   history %s (prompt history)",
					partSize(parts.ToolResults, p.toolResults), partSize(parts.FileHistory, p.fileHistory), partLines(parts.History, p.historyLines)))
		}
		if p.memoryFiles > 0 {
			lines = append(lines, fmt.Sprintf("    memory        %d files   %s", p.memoryFiles, formatSize(p.memoryBytes)))
		}
	}
	return append(lines, "  total "+formatSize(m.Totals.Bytes))
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

// short8 is the eight-character prefix of a bundle id.
func short8(id string) string { return shortID(id) }

// importReceiptLines is the CLI's post-import block: sessions, memory,
// what followed a confirmed placement, history, the verify lines, the
// pending note and the record path. account "" means the unmanaged home.
func importReceiptLines(cfgDir string, rep porter.Report, account string) []string {
	id8 := short8(rep.BundleID)
	landed := len(rep.Imported) + len(rep.Pending)
	var lines []string
	line := fmt.Sprintf("%d committed", landed)
	if landed == 0 {
		line = "none committed"
	}
	line += fmt.Sprintf("; %d skipped", len(rep.Skipped)+len(rep.Held))
	if n := len(rep.Overwritten); n > 0 {
		line += fmt.Sprintf("; %d existing set aside as *.bffs-replaced-<time> (never deleted)", n)
	}
	if n := len(rep.MtimeRaised); n > 0 {
		line += fmt.Sprintf("; %s raised to the retention floor", countNoun(n, "mtime"))
	}
	lines = append(lines, "  sessions   "+line)
	if rep.SweptCount > 0 && !rep.SweepDate.IsZero() {
		lines = append(lines, fmt.Sprintf("             %s will be swept by Claude on %s unless resumed", countNoun(rep.SweptCount, "session"), rep.SweepDate.Local().Format("2006-01-02")))
	}
	for _, sid := range rep.Held {
		lines = append(lines, fmt.Sprintf("             held %s: %s", short8(sid), rep.Reasons[sid]))
	}
	for _, sid := range rep.Skipped {
		lines = append(lines, fmt.Sprintf("             skipped %s: %s", short8(sid), rep.Reasons[sid]))
	}
	if len(rep.MemoryDirs) == 0 {
		lines = append(lines, "  memory     nothing written")
	}
	for _, dir := range rep.MemoryDirs {
		lines = append(lines, fmt.Sprintf("  memory     written into %s (conflicts kept as *.imported-%s.md)", shortPath(dir), id8))
		if n := rep.MemoryReview[dir]; n > 0 {
			lines = append(lines, fmt.Sprintf("             %s still mention absolute paths from the source machine (p scans them)", countNoun(n, "line")))
		}
	}
	acct := account
	if acct == "" {
		acct = transcripts.HomeName
	}
	for _, key := range rep.TrustCarried {
		lines = append(lines, fmt.Sprintf("  trust      carried over for %s → %q", shortPath(key), acct))
	}
	if len(rep.LastSession) > 0 {
		dirs := make([]string, 0, len(rep.LastSession))
		for dir := range rep.LastSession {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			lines = append(lines, fmt.Sprintf("  last-session pointer set for %s in %q (%s)", shortPath(dir), acct, short8(rep.LastSession[dir])))
		}
	}
	if rep.HistoryLines == 0 {
		lines = append(lines, "  history    no new prompt lines")
	} else {
		lines = append(lines, fmt.Sprintf("  history    %s added", countNoun(rep.HistoryLines, "prompt line")))
	}
	if len(rep.Verify) > 0 {
		lines = append(lines, "", "Check it:")
		for _, v := range rep.Verify {
			lines = append(lines, "    "+v)
		}
	}
	if n := len(rep.Pending); n > 0 {
		lines = append(lines, fmt.Sprintf("    pending: %s imported as-is (directory missing here) — rehome them with r", countNoun(n, "session")))
	}
	if landed > 0 {
		lines = append(lines, "",
			"note: the first claude launch there asks about folder trust (and external CLAUDE.md imports) once per bffs account;",
			"      t carries the answer to the other accounts.")
	}
	for _, w := range rep.Warnings {
		lines = append(lines, "warning: "+w)
	}
	if rep.StagingDir != "" {
		lines = append(lines, fmt.Sprintf("staging kept at %s (inspect it, then run bffs import --clean-staging)", rep.StagingDir))
	}
	if rep.BundleID != "" && landed > 0 {
		lines = append(lines, "Import record: "+shortPath(imports.Path(cfgDir, rep.BundleID))+"  (bffs sessions imports)")
	}
	return lines
}

// copyReceiptLines is the CLI's `bffs copy` receipt.
func copyReceiptLines(rep porter.Report) []string {
	n := len(rep.Imported) + len(rep.Pending)
	line := "copied " + countNoun(n, "session")
	if len(rep.MemoryDirs) > 0 {
		line += ", " + countNoun(len(rep.MemoryDirs), "memory dir")
	}
	lines := []string{line}
	if len(rep.Skipped) > 0 {
		lines = append(lines, fmt.Sprintf("skipped %s already in the destination (bffs copy --on-conflict overwrite replaces them)", countNoun(len(rep.Skipped), "session")))
	}
	for _, sid := range rep.Held {
		lines = append(lines, fmt.Sprintf("held %s: %s", short8(sid), rep.Reasons[sid]))
	}
	if len(rep.MtimeRaised) > 0 {
		l := countNoun(len(rep.MtimeRaised), "mtime") + " raised to the retention floor"
		if rep.SweptCount > 0 && !rep.SweepDate.IsZero() {
			l += fmt.Sprintf("; %s will be swept by Claude on %s unless resumed", countNoun(rep.SweptCount, "session"), rep.SweepDate.Format("2006-01-02"))
		}
		lines = append(lines, l)
	}
	for _, v := range rep.Verify {
		lines = append(lines, "verify:  "+v)
	}
	for _, w := range rep.Warnings {
		lines = append(lines, "warning: "+w)
	}
	if rep.StagingDir != "" {
		lines = append(lines, fmt.Sprintf("staging kept at %s (inspect it, then run bffs import --clean-staging)", rep.StagingDir))
	}
	return lines
}

// rehomePlanLines renders what a rehome would do — the CLI's plan block.
func rehomePlanLines(p rehome.Plan, maps []rehome.Mapping) []string {
	lines := []string{"rehome in " + rootLabel(p.Root) + ":"}
	for _, m := range maps {
		lines = append(lines, fmt.Sprintf("  rule    %s → %s", m.Old, shortPath(m.New)))
	}
	for _, mv := range p.Moves {
		title := mv.Title
		if title == "" {
			title = "(untitled)"
		}
		title = truncate(transcripts.Sanitize(title), 40)
		from := filepath.Base(filepath.Dir(mv.From))
		action := fmt.Sprintf("projects/%s → projects/%s", from, mv.NewSlug)
		if mv.SameSlug {
			action = fmt.Sprintf("projects/%s (already there; relocated stamp only)", mv.NewSlug)
		}
		extra := ""
		if mv.Sidecar {
			extra = " + sidecar"
		}
		lines = append(lines, fmt.Sprintf("  move    %s  %-40s  %s%s", shortID(mv.SessionID), title, action, extra))
	}
	for _, m := range p.Memory {
		mode := m.Mode
		if mode == "" {
			mode = rehome.MemoryMerge
		}
		lines = append(lines, fmt.Sprintf("  memory  %s → %s  (%s; the old directory stays)", shortPath(m.From), shortPath(m.To), mode))
	}
	for _, r := range p.Refusals {
		lines = append(lines, "  "+refusalLine(r))
	}
	for _, w := range p.Warnings {
		lines = append(lines, "  warning: "+w)
	}
	if len(p.Moves) > 0 {
		lines = append(lines, "  (a relocated record is appended to each transcript; conversation content, mtimes and picker order are unchanged)")
	}
	return lines
}

// refusalLine renders one refusal the way the CLI does.
func refusalLine(r rehome.Refusal) string {
	if strings.Contains(r.Reason, "running claude") {
		return fmt.Sprintf("held (live): %s %s", shortID(r.SessionID), strings.TrimPrefix(r.Reason, "open in a running claude "))
	}
	return fmt.Sprintf("refused %s: %s", shortID(r.SessionID), r.Reason)
}

// rehomeCount is "N sessions and M memory directories".
func rehomeCount(p rehome.Plan) string {
	var parts []string
	if n := len(p.Moves); n > 0 {
		parts = append(parts, countNoun(n, "session"))
	}
	if n := len(p.Memory); n > 0 {
		parts = append(parts, countNoun(n, "memory directory"))
	}
	return strings.Join(parts, " and ")
}

// rehomeResultLines renders what a rehome did — the CLI's result block
// with the verify lines.
func rehomeResultLines(res rehome.Result) []string {
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
	var lines []string
	if len(res.Moves) > 0 && len(res.Moved) == 0 {
		lines = append(lines, "moved no sessions")
	}
	for _, slug := range slugs {
		lines = append(lines, fmt.Sprintf("moved %s → projects/%s (relocated stamp appended; mtimes and picker order preserved)", countNoun(perSlug[slug], "session"), slug))
	}
	if n := len(res.Moves) - len(res.Moved); n > 0 {
		lines = append(lines, countNoun(n, "session")+" not moved (see the error)")
	}
	for _, m := range res.Memory {
		total := len(m.Added) + len(m.Renamed) + len(m.Unchanged)
		l := fmt.Sprintf("merged memory: %d files (%d new, %d identical, %d renamed) into %s", total, len(m.Added), len(m.Unchanged), len(m.Renamed), shortPath(m.To))
		if m.IndexAppended {
			l += "; MEMORY.md +1 section"
		}
		if len(m.Rewritten) > 0 {
			l += fmt.Sprintf("; rewrote old paths in %s", countNoun(len(m.Rewritten), "file"))
		}
		lines = append(lines, l)
		for _, w := range m.Warnings {
			lines = append(lines, "  note: "+w)
		}
		if n := len(m.Remaining); n > 0 {
			lines = append(lines, fmt.Sprintf("%s still mention absolute paths (p scans them)", countNoun(n, "memory line")))
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
		lines = append(lines, fmt.Sprintf("%s not moved: %s", countNoun(n, "session"), strings.Join(parts, ", ")))
		for _, r := range res.Refusals {
			lines = append(lines, "  "+refusalLine(r))
		}
	}
	for _, v := range res.Verify {
		lines = append(lines, "verify: "+v)
	}
	for _, w := range res.Warnings {
		lines = append(lines, "warning: "+w)
	}
	return lines
}
