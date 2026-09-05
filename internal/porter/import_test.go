package porter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// roundTrip seeds a source machine, exports its project and imports the
// bundle into a fresh second machine (same HOME, same project dir, so the
// placement is identity).
func roundTrip(t *testing.T, o func(*ImportOptions)) (src, dst *env, m *bundle.Manifest, data []byte, rep Report) {
	t.Helper()
	src = newEnv(t)
	seedPool(t, src)
	m, data = exportPool(t, src)
	dst = secondEnv(t, src)
	opts := importOpts(dst)
	if o != nil {
		o(&opts)
	}
	rep = doImport(t, dst, data, opts)
	return src, dst, m, data, rep
}

func TestImportRoundTrip(t *testing.T) {
	src, dst, m, _, rep := roundTrip(t, nil)
	slug := src.slug(t)
	id8 := m.BundleID[:8]

	if got, want := rep.Imported, []string{sidB, sidA}; !reflect.DeepEqual(got, want) {
		t.Errorf("Imported = %v, want %v", got, want)
	}
	if len(rep.Pending)+len(rep.Skipped)+len(rep.Held)+len(rep.Overwritten) != 0 {
		t.Errorf("unexpected lists: %+v", rep)
	}
	if rep.BundleID != m.BundleID || rep.Bytes != m.Totals.Bytes || rep.StagingDir != "" {
		t.Errorf("report header = %+v", rep)
	}
	if exists(StagingDir(dst.cfgDir, m.BundleID)) {
		t.Error("staging dir not removed")
	}

	// Transcripts and sidecars are byte-identical: no stamp on identity.
	srcTree := digestTree(t, filepath.Join(src.root.Dir, slug))
	dstTree := digestTree(t, filepath.Join(dst.root.Dir, slug))
	for _, name := range []string{sidA + ".jsonl", sidB + ".jsonl", sidA + "/subagents/agent-1.jsonl", sidA + "/subagents/agent-1.meta.json", sidA + "/tool-results/abc.txt", sidA + "/custom-title.json"} {
		if srcTree[name] == "" || srcTree[name] != dstTree[name] {
			t.Errorf("%s: src %s dst %s", name, srcTree[name], dstTree[name])
		}
	}
	if got := readFile(t, filepath.Join(dst.root.Dir, slug, sidA+".jsonl")); strings.Contains(got, "relocated") {
		t.Error("identity placement stamped the transcript")
	}
	for _, name := range []string{
		filepath.Join(transcripts.FileHistorySubdir, sidA, "0123456789abcdef@v1"),
		filepath.Join(transcripts.PlansSubdir, planSlug+".md"),
		filepath.Join(transcripts.TasksSubdir, sidA, "1.json"),
	} {
		if readFile(t, filepath.Join(src.claudeDir, name)) != readFile(t, filepath.Join(dst.claudeDir, name)) {
			t.Errorf("%s differs", name)
		}
	}
	if exists(filepath.Join(dst.claudeDir, transcripts.TasksSubdir, sidA, ".lock")) {
		t.Error("tasks .lock travelled")
	}

	// Memory: unconfirmed placement → side files, neutralised pin, no index.
	mem := dst.memDir(t)
	if got, want := rep.MemoryDirs, []string{mem}; !reflect.DeepEqual(got, want) {
		t.Errorf("MemoryDirs = %v, want %v", got, want)
	}
	topic := readFile(t, filepath.Join(mem, "topic.imported-"+id8+".md"))
	if !strings.Contains(topic, "pinned-imported: true") || strings.Contains(topic, "\npinned: true") {
		t.Errorf("topic = %q", topic)
	}
	if !near(mtimeOf(t, filepath.Join(mem, "topic.imported-"+id8+".md")), fixedNow.Add(-20*day)) {
		t.Error("topic mtime not preserved")
	}
	if !exists(filepath.Join(mem, transcripts.MemoryLogsSubdir, "2026", "08", "24", "0f3b2c1e-fix.imported-"+id8+".md")) {
		t.Error("log file missing")
	}
	for _, name := range []string{transcripts.MemoryIndexFile, "topic.md", "notes.txt", "proposals", "index_persist"} {
		if exists(filepath.Join(mem, name)) {
			t.Errorf("%s must not be written", name)
		}
	}
	if !containsWarning(rep.Warnings, "absolute paths") {
		t.Errorf("no path-review warning: %v", rep.Warnings)
	}

	// History: both sessions' lines, appended once.
	hist := readFile(t, filepath.Join(dst.claudeDir, transcripts.HistoryFile))
	if n := strings.Count(hist, "\n"); n != 3 || rep.HistoryLines != 3 || strings.Contains(hist, "not ours") {
		t.Errorf("history lines = %d (report %d): %q", n, rep.HistoryLines, hist)
	}

	// Mtimes and the sweep line.
	if got, want := rep.MtimeRaised, []string{sidA}; !reflect.DeepEqual(got, want) || rep.SweptCount != 1 {
		t.Errorf("MtimeRaised = %v, SweptCount %d", got, rep.SweptCount)
	}
	if !rep.SweepDate.Equal(fixedNow.Add(15 * day)) {
		t.Errorf("SweepDate = %v, want %v", rep.SweepDate, fixedNow.Add(15*day))
	}

	// Verify line: the newest imported session of the one directory.
	if len(rep.Verify) != 1 || rep.Verify[0] != rehome.VerifyCommand(src.cwd, sidB, "") {
		t.Errorf("Verify = %v", rep.Verify)
	}

	// The import record.
	recs, err := imports.Load(dst.cfgDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	rec := recs[0]
	if rec.BundleID != m.BundleID || rec.Kind != imports.KindImport || rec.DestRoot != dst.root.Dir || !rec.ImportedAt.Equal(fixedNow) || rec.Source.Hostname != m.Source.Hostname || rec.Source.BFFSVersion != "0.4.0" {
		t.Errorf("record = %+v", rec)
	}
	ref, ok := imports.BySession(recs)[sidA]
	if !ok || ref.Session.Status != imports.StatusPlaced || ref.Session.NewCwd != src.cwd || ref.Session.Slug != slug || ref.Session.OldSlug != slug || ref.Session.Title != "Title A" || !near(ref.Session.OrigMtime, fixedNow.Add(-45*day)) {
		t.Errorf("BySession[sidA] = %+v", ref.Session)
	}
	if len(rec.Memories) != 1 || rec.Memories[0].Status != imports.StatusPlaced || rec.Memories[0].Dir != mem || rec.Memories[0].OldCwd != src.cwd {
		t.Errorf("record memories = %+v", rec.Memories)
	}
	if !exists(filepath.Join(dst.cfgDir, imports.Subdir, m.BundleID+".json")) {
		t.Error("record file missing")
	}
}

func TestImportAsIsPending(t *testing.T) {
	src, dst, _, _, rep := roundTrip(t, func(o *ImportOptions) { o.AsIs = true })
	if got, want := rep.Pending, []string{sidB, sidA}; !reflect.DeepEqual(got, want) || len(rep.Imported) != 0 {
		t.Errorf("Pending = %v Imported = %v", rep.Pending, rep.Imported)
	}
	if !exists(filepath.Join(dst.root.Dir, src.slug(t), sidA+".jsonl")) {
		t.Error("session not under the original slug")
	}
	if len(rep.Verify) != 1 || rep.Verify[0] != "claude --resume "+sidB {
		t.Errorf("Verify = %v", rep.Verify)
	}
	recs, _ := imports.Load(dst.cfgDir)
	if ref := imports.BySession(recs)[sidA]; ref.Session.Status != imports.StatusPending || ref.Session.NewCwd != "" {
		t.Errorf("record = %+v", ref.Session)
	}
	if ref := recs[0].Memories[0]; ref.Status != imports.StatusPending || ref.Dir != dst.memDir(t) {
		t.Errorf("memory record = %+v", ref)
	}
}

func TestImportMissingCwdIsPending(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	m, data := exportPool(t, src)
	dst := secondEnv(t, src)
	if err := os.RemoveAll(src.cwd); err != nil {
		t.Fatal(err)
	}
	rep := doImport(t, dst, data, importOpts(dst))
	if got, want := rep.Pending, []string{sidB, sidA}; !reflect.DeepEqual(got, want) {
		t.Errorf("Pending = %v", got)
	}
	if !exists(filepath.Join(dst.root.Dir, m.Entries[0].Slug, sidB+".jsonl")) {
		t.Error("not under the original slug")
	}
	memSlug := entryFor(m, bundle.EntryMemory, "").Slug
	if got, want := rep.MemoryDirs, []string{filepath.Join(dst.root.Dir, memSlug, transcripts.MemorySubdir)}; !reflect.DeepEqual(got, want) {
		t.Errorf("MemoryDirs = %v, want %v", got, want)
	}
}

func TestImportCollisionSkip(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	existing := filepath.Join(dst.root.Dir, src.slug(t), sidA+".jsonl")
	writeFile(t, existing, userRec(sidA, src.cwd, "2026-07-10T10:00:00Z", "already here"))
	rep := doImport(t, dst, data, importOpts(dst))
	if !reflect.DeepEqual(rep.Imported, []string{sidB}) || !reflect.DeepEqual(rep.Skipped, []string{sidA}) {
		t.Errorf("Imported %v Skipped %v", rep.Imported, rep.Skipped)
	}
	if !strings.Contains(rep.Reasons[sidA], "exists") {
		t.Errorf("reason = %q", rep.Reasons[sidA])
	}
	if !strings.Contains(readFile(t, existing), "already here") {
		t.Error("existing transcript replaced")
	}
	if exists(filepath.Join(dst.root.Dir, src.slug(t), sidA)) {
		t.Error("sidecar of a skipped session written")
	}
	recs, _ := imports.Load(dst.cfgDir)
	if ref := imports.BySession(recs)[sidA]; ref.Session.Status != imports.StatusSkipped {
		t.Errorf("record = %+v", ref.Session)
	}
}

func TestImportCollisionOverwrite(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	slugDir := filepath.Join(dst.root.Dir, src.slug(t))
	writeFile(t, filepath.Join(slugDir, sidA+".jsonl"), userRec(sidA, src.cwd, "2026-07-10T10:00:00Z", "old"))
	writeFile(t, filepath.Join(slugDir, sidA, "tool-results", "old.txt"), "old output")
	opts := importOpts(dst)
	opts.OnConflict = ConflictOverwrite
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || !reflect.DeepEqual(rep.Overwritten, []string{sidA}) {
		t.Errorf("Imported %v Overwritten %v", rep.Imported, rep.Overwritten)
	}
	if got := readFile(t, filepath.Join(slugDir, sidA+".jsonl")); !strings.Contains(got, "first prompt A") {
		t.Errorf("transcript not replaced: %q", got)
	}
	stamp := fmt.Sprint(fixedNow.UnixMilli())
	asideTr := filepath.Join(slugDir, sidA+".jsonl.bffs-replaced-"+stamp)
	asideSide := filepath.Join(slugDir, sidA+".bffs-replaced-"+stamp)
	if !strings.Contains(readFile(t, asideTr), "old") || readFile(t, filepath.Join(asideSide, "tool-results", "old.txt")) != "old output" {
		t.Error("set-asides missing or wrong")
	}
	if exists(filepath.Join(slugDir, sidA, "tool-results", "old.txt")) {
		t.Error("old sidecar content survived in place")
	}
	// Set-asides are invisible to the listing.
	ss, err := transcripts.List(context.Background(), dst.root, transcripts.ListOptions{})
	if err != nil || len(ss) != 2 {
		t.Errorf("listing after overwrite: %v %d", err, len(ss))
	}
	// A second overwrite with the same clock picks the next free stamp.
	opts.Force = true
	rep = doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Overwritten, []string{sidB, sidA}) {
		t.Errorf("second Overwritten = %v", rep.Overwritten)
	}
	if !exists(filepath.Join(slugDir, sidA+".jsonl.bffs-replaced-"+fmt.Sprint(fixedNow.UnixMilli()+1))) {
		t.Error("second set-aside did not bump the stamp")
	}
	if !strings.Contains(readFile(t, asideTr), "old") {
		t.Error("first set-aside clobbered")
	}
}

func TestImportCollisionDifferentSlugRefused(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	writeFile(t, filepath.Join(dst.root.Dir, "-other", sidA+".jsonl"), userRec(sidA, "/other", "2026-07-10T10:00:00Z", "x"))
	opts := importOpts(dst)
	opts.OnConflict = ConflictOverwrite
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidB}) || !reflect.DeepEqual(rep.Skipped, []string{sidA}) {
		t.Errorf("Imported %v Skipped %v", rep.Imported, rep.Skipped)
	}
	if want := "exists under -other; would make claude --resume ambiguous"; rep.Reasons[sidA] != want {
		t.Errorf("reason = %q, want %q", rep.Reasons[sidA], want)
	}
	if exists(filepath.Join(dst.root.Dir, src.slug(t), sidA+".jsonl")) {
		t.Error("ambiguous session written anyway")
	}
}

func TestImportCollisionLiveHeld(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	existing := filepath.Join(dst.root.Dir, src.slug(t), sidB+".jsonl")
	writeFile(t, existing, userRec(sidB, src.cwd, "2026-08-14T10:00:00Z", "open"))
	opts := importOpts(dst)
	opts.OnConflict = ConflictOverwrite
	opts.Live = map[string]transcripts.LiveSession{sidB: {PID: 4242, SessionID: sidB}}
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Held, []string{sidB}) || !reflect.DeepEqual(rep.Imported, []string{sidA}) || len(rep.Skipped) != 0 {
		t.Errorf("Held %v Imported %v Skipped %v", rep.Held, rep.Imported, rep.Skipped)
	}
	if !strings.Contains(rep.Reasons[sidB], "running claude") {
		t.Errorf("reason = %q", rep.Reasons[sidB])
	}
	if !strings.Contains(readFile(t, existing), "open") {
		t.Error("live transcript touched")
	}
	// The superseded-sibling rule counts as live too.
	dst2 := secondEnv(t, src)
	existing = filepath.Join(dst2.root.Dir, src.slug(t), sidB+".jsonl")
	writeFile(t, existing, userRec(sidB, src.cwd, "2026-08-14T10:00:00Z", "open"))
	writeFile(t, existing+".superseded-1", "")
	chtimes(t, existing+".superseded-1", fixedNow.Add(-10*time.Second))
	opts = importOpts(dst2)
	opts.OnConflict = ConflictOverwrite
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.Held, []string{sidB}) {
		t.Errorf("sibling rule: Held = %v", rep.Held)
	}
}

func TestImportRollbackKeepsEarlierSessions(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	m, data := exportPool(t, src)
	dst := secondEnv(t, src)
	orig := commitSession
	commitSession = func(ctx context.Context, dest *os.Root, req rehome.CommitRequest, p rehome.MtimePolicy, journal func(string) error) (rehome.CommitResult, error) {
		if req.SessionID == sidA {
			req.Transcript = filepath.Join(t.TempDir(), "vanished.jsonl") // fails at step c, after the sidecar landed
		}
		return orig(ctx, dest, req, p, journal)
	}
	defer func() { commitSession = orig }()

	rep, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), importOpts(dst))
	if err == nil {
		t.Fatal("expected an error")
	}
	staging := StagingDir(dst.cfgDir, m.BundleID)
	if !strings.Contains(err.Error(), "staging kept at "+staging) || rep.StagingDir != staging || !exists(filepath.Join(staging, rehome.JournalFile)) {
		t.Errorf("err = %v; StagingDir = %q", err, rep.StagingDir)
	}
	if !reflect.DeepEqual(rep.Imported, []string{sidB}) {
		t.Errorf("Imported = %v, want the session committed before the failure", rep.Imported)
	}
	slugDir := filepath.Join(dst.root.Dir, src.slug(t))
	if !exists(filepath.Join(slugDir, sidB+".jsonl")) {
		t.Error("earlier session lost")
	}
	for _, p := range []string{filepath.Join(slugDir, sidA+".jsonl"), filepath.Join(slugDir, sidA), filepath.Join(dst.claudeDir, transcripts.FileHistorySubdir, sidA), filepath.Join(dst.claudeDir, transcripts.TasksSubdir, sidA)} {
		if exists(p) {
			t.Errorf("%s left behind by the rolled-back session", p)
		}
	}
	entries, _ := os.ReadDir(slugDir)
	for _, en := range entries {
		if strings.Contains(en.Name(), "bffs-tmp") {
			t.Errorf("tmp entry left: %s", en.Name())
		}
	}
	// The memory landed before the sessions and stays; the record lists
	// what landed.
	if len(rep.MemoryDirs) != 1 {
		t.Errorf("MemoryDirs = %v", rep.MemoryDirs)
	}
	recs, _ := imports.Load(dst.cfgDir)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	by := imports.BySession(recs)
	if by[sidB].Session.Status != imports.StatusPlaced || by[sidA].Session.Status != imports.StatusSkipped {
		t.Errorf("record statuses: %+v / %+v", by[sidB].Session, by[sidA].Session)
	}

	// A rerun needs --force (the record exists) and refuses the leftover
	// staging dir until it is cleaned.
	if _, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), importOpts(dst)); err == nil || !strings.Contains(err.Error(), "--force to import again") {
		t.Errorf("rerun without --force: %v", err)
	}
	opts := importOpts(dst)
	opts.Force = true
	if _, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), opts); err == nil || !strings.Contains(err.Error(), "--clean-staging") {
		t.Errorf("rerun with leftover staging: %v", err)
	}
	if stale, _ := StaleStaging(dst.cfgDir); !reflect.DeepEqual(stale, []string{staging}) {
		t.Errorf("StaleStaging = %v", stale)
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
	}
	commitSession = orig
	rep = doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidA}) || !reflect.DeepEqual(rep.Skipped, []string{sidB}) {
		t.Errorf("rerun: Imported %v Skipped %v", rep.Imported, rep.Skipped)
	}
}

func TestImportMtimeRules(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	slug := src.slug(t)
	trA := func(e *env) string { return filepath.Join(e.root.Dir, slug, sidA+".jsonl") }
	trB := func(e *env) string { return filepath.Join(e.root.Dir, slug, sidB+".jsonl") }
	tool := func(e *env) string { return filepath.Join(e.root.Dir, slug, sidA, "tool-results", "abc.txt") }

	t.Run("default 30 days", func(t *testing.T) {
		dst := secondEnv(t, src)
		rep := doImport(t, dst, data, importOpts(dst))
		if got := mtimeOf(t, trA(dst)); !near(got, fixedNow.Add(-15*day)) {
			t.Errorf("45 d-old transcript mtime = %v, want now-15d", got)
		}
		if got := mtimeOf(t, trB(dst)); !near(got, fixedNow.Add(-10*day)) {
			t.Errorf("10 d-old transcript mtime = %v, want preserved", got)
		}
		for _, p := range []string{
			tool(dst),
			filepath.Join(dst.root.Dir, slug, sidA, "subagents", "agent-1.jsonl"),
			filepath.Join(dst.claudeDir, transcripts.FileHistorySubdir, sidA, "0123456789abcdef@v1"),
			filepath.Join(dst.claudeDir, transcripts.PlansSubdir, planSlug+".md"),
			filepath.Join(dst.claudeDir, transcripts.TasksSubdir, sidA, "1.json"),
		} {
			if got := mtimeOf(t, p); !near(got, fixedNow) {
				t.Errorf("%s mtime = %v, want now", p, got)
			}
		}
		if !reflect.DeepEqual(rep.MtimeRaised, []string{sidA}) {
			t.Errorf("MtimeRaised = %v", rep.MtimeRaised)
		}
	})
	t.Run("cleanupPeriodDays 0", func(t *testing.T) {
		dst := secondEnv(t, src)
		writeFile(t, filepath.Join(dst.claudeDir, transcripts.SettingsFile), `{"cleanupPeriodDays": 0}`)
		rep := doImport(t, dst, data, importOpts(dst))
		if got := mtimeOf(t, trA(dst)); !near(got, fixedNow.Add(-45*day)) {
			t.Errorf("mtime = %v, want untouched", got)
		}
		if len(rep.MtimeRaised) != 0 || !rep.SweepDate.IsZero() || rep.SweptCount != 0 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("cleanupPeriodDays 8", func(t *testing.T) {
		dst := secondEnv(t, src)
		writeFile(t, filepath.Join(dst.claudeDir, transcripts.LocalSettingsFile), `{"cleanupPeriodDays": 8}`)
		rep := doImport(t, dst, data, importOpts(dst))
		for _, p := range []string{trA(dst), trB(dst)} {
			if got := mtimeOf(t, p); !near(got, fixedNow.Add(-4*day)) {
				t.Errorf("%s mtime = %v, want now-4d", p, got)
			}
		}
		if !reflect.DeepEqual(rep.MtimeRaised, []string{sidB, sidA}) || !rep.SweepDate.Equal(fixedNow.Add(4*day)) || rep.SweptCount != 2 {
			t.Errorf("report = %+v", rep)
		}
	})
	t.Run("preserve mtimes", func(t *testing.T) {
		dst := secondEnv(t, src)
		opts := importOpts(dst)
		opts.PreserveMtimes = true
		doImport(t, dst, data, opts)
		if got := mtimeOf(t, tool(dst)); !near(got, fixedNow.Add(-40*day)) {
			t.Errorf("tool-results mtime = %v, want the source's", got)
		}
		if got := mtimeOf(t, trA(dst)); !near(got, fixedNow.Add(-15*day)) {
			t.Errorf("transcript floor must still apply: %v", got)
		}
	})
}

// sweep imitates Claude's retention pass at instant at with a 30-day
// window: transcripts older than the cutoff go, with their sidecar; so do
// tool-results files older than the cutoff.
func sweep(t *testing.T, root transcripts.Root, at time.Time) {
	t.Helper()
	cutoff := at.Add(-30 * day)
	slugs, _ := os.ReadDir(root.Dir)
	for _, s := range slugs {
		dir := filepath.Join(root.Dir, s.Name())
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
					os.Remove(filepath.Join(dir, e.Name()))
					os.RemoveAll(filepath.Join(dir, strings.TrimSuffix(e.Name(), ".jsonl")))
				}
			}
		}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.Contains(p, string(filepath.Separator)+"tool-results"+string(filepath.Separator)) {
				if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
					os.Remove(p)
				}
			}
			return nil
		})
	}
}

func TestImportSurvivesSimulatedSweep(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	slug := src.slug(t)
	for _, delay := range []time.Duration{25 * time.Hour, 14 * day} {
		dst := secondEnv(t, src)
		doImport(t, dst, data, importOpts(dst))
		sweep(t, dst.root, fixedNow.Add(delay))
		for _, p := range []string{
			filepath.Join(dst.root.Dir, slug, sidA+".jsonl"),
			filepath.Join(dst.root.Dir, slug, sidB+".jsonl"),
			filepath.Join(dst.root.Dir, slug, sidA, "tool-results", "abc.txt"),
		} {
			if !exists(p) {
				t.Errorf("sweep at import+%v removed %s", delay, p)
			}
		}
	}
	// Without the mtime rules the same sweep would have taken the 45-day
	// transcript and the 40-day tool-results file: prove the simulation bites.
	dst := secondEnv(t, src)
	writeFile(t, filepath.Join(dst.claudeDir, transcripts.SettingsFile), `{"cleanupPeriodDays": 0}`)
	opts := importOpts(dst)
	opts.PreserveMtimes = true
	doImport(t, dst, data, opts)
	sweep(t, dst.root, fixedNow.Add(25*time.Hour))
	if exists(filepath.Join(dst.root.Dir, slug, sidA+".jsonl")) || exists(filepath.Join(dst.root.Dir, slug, sidA, "tool-results", "abc.txt")) {
		t.Error("simulated sweep does not remove old files")
	}
}

func TestImportMemoryModes(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	m, data := exportPool(t, src)
	id8 := m.BundleID[:8]

	t.Run("skip existing", func(t *testing.T) {
		dst := secondEnv(t, src)
		mem := dst.memDir(t)
		writeFile(t, filepath.Join(mem, "mine.md"), "mine\n")
		rep := doImport(t, dst, data, importOpts(dst))
		if len(rep.MemoryDirs) != 0 || exists(filepath.Join(mem, "topic.imported-"+id8+".md")) {
			t.Errorf("memory written over an existing dir: %v", rep.MemoryDirs)
		}
		if !containsWarning(rep.Warnings, "memory directory exists") {
			t.Errorf("warnings = %v", rep.Warnings)
		}
		recs, _ := imports.Load(dst.cfgDir)
		if recs[0].Memories[0].Status != imports.StatusSkipped {
			t.Errorf("memory record = %+v", recs[0].Memories[0])
		}
	})
	t.Run("overwrite sets aside", func(t *testing.T) {
		dst := secondEnv(t, src)
		mem := dst.memDir(t)
		writeFile(t, filepath.Join(mem, "mine.md"), "mine\n")
		opts := importOpts(dst)
		opts.Memory = rehome.MemoryOverwrite
		rep := doImport(t, dst, data, opts)
		if !reflect.DeepEqual(rep.MemoryDirs, []string{mem}) {
			t.Errorf("MemoryDirs = %v", rep.MemoryDirs)
		}
		aside := mem + ".bffs-replaced-" + fmt.Sprint(fixedNow.UnixMilli())
		if readFile(t, filepath.Join(aside, "mine.md")) != "mine\n" || exists(filepath.Join(mem, "mine.md")) {
			t.Error("existing memory not set aside")
		}
		if !exists(filepath.Join(mem, "topic.imported-"+id8+".md")) || exists(filepath.Join(mem, transcripts.MemoryIndexFile)) {
			t.Error("unconfirmed rules not applied after overwrite")
		}
	})
	t.Run("trust memory keeps pins", func(t *testing.T) {
		dst := secondEnv(t, src)
		opts := importOpts(dst)
		opts.TrustMemory = true
		doImport(t, dst, data, opts)
		if got := readFile(t, filepath.Join(dst.memDir(t), "topic.imported-"+id8+".md")); !strings.Contains(got, "\npinned: true\n") {
			t.Errorf("topic = %q", got)
		}
	})
	t.Run("override refuses memory only", func(t *testing.T) {
		dst := secondEnv(t, src)
		t.Setenv(transcripts.EnvRemoteMemoryDir, filepath.Join(t.TempDir(), "remote"))
		rep := doImport(t, dst, data, importOpts(dst))
		if len(rep.MemoryDirs) != 0 || !containsWarning(rep.Warnings, transcripts.EnvRemoteMemoryDir) {
			t.Errorf("report = %+v", rep)
		}
		if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) {
			t.Errorf("sessions must still land: %v", rep.Imported)
		}
	})
	t.Run("merge unconfirmed adds side files", func(t *testing.T) {
		dst := secondEnv(t, src)
		mem := dst.memDir(t)
		writeFile(t, filepath.Join(mem, "mine.md"), "mine\n")
		opts := importOpts(dst)
		opts.Memory = rehome.MemoryMerge
		rep := doImport(t, dst, data, opts)
		if !reflect.DeepEqual(rep.MemoryDirs, []string{mem}) {
			t.Errorf("MemoryDirs = %v", rep.MemoryDirs)
		}
		if !exists(filepath.Join(mem, "topic.imported-"+id8+".md")) || exists(filepath.Join(mem, transcripts.MemoryIndexFile)) || readFile(t, filepath.Join(mem, "mine.md")) != "mine\n" {
			t.Error("identity placement is unconfirmed: side files only, no index, existing files untouched")
		}
		recs, _ := imports.Load(dst.cfgDir)
		if recs[0].Memories[0].Status != imports.StatusPlaced {
			t.Errorf("memory record = %+v", recs[0].Memories[0])
		}
	})
}

func TestImportHistoryDedupe(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	writeFile(t, filepath.Join(dst.claudeDir, transcripts.HistoryFile), historyLine(sidA, src.cwd, "first prompt A", 1752141600000))
	rep := doImport(t, dst, data, importOpts(dst))
	if rep.HistoryLines != 2 {
		t.Errorf("HistoryLines = %d, want the two new lines", rep.HistoryLines)
	}
	opts := importOpts(dst)
	opts.Force = true
	opts.OnConflict = ConflictOverwrite
	rep = doImport(t, dst, data, opts)
	if rep.HistoryLines != 0 {
		t.Errorf("second import appended %d lines", rep.HistoryLines)
	}
	hist := readFile(t, filepath.Join(dst.claudeDir, transcripts.HistoryFile))
	if n := strings.Count(hist, "\n"); n != 3 {
		t.Errorf("history has %d lines: %q", n, hist)
	}
}

func TestImportBundleIDCollision(t *testing.T) {
	src, dst, m, data, _ := roundTrip(t, nil)
	_, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), importOpts(dst))
	want := "bundle " + m.BundleID[:8] + " was imported on 2026-08-24 into " + dst.root.Dir + "; --force to import again"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}
	if exists(StagingDir(dst.cfgDir, m.BundleID)) {
		t.Error("staging created for a refused import")
	}
	opts := importOpts(dst)
	opts.Force = true
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Skipped, []string{sidB, sidA}) {
		t.Errorf("forced rerun: Skipped = %v", rep.Skipped)
	}
	_ = src
}

func TestImportRefusesPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	outside := t.TempDir()
	slugDir := filepath.Join(dst.root.Dir, src.slug(t))
	mkdir(t, slugDir)
	if err := os.Symlink(outside, filepath.Join(slugDir, sidA)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.jsonl")
	writeFile(t, target, "outside\n")
	if err := os.Symlink(target, filepath.Join(slugDir, sidB+".jsonl")); err != nil {
		t.Fatal(err)
	}
	rep := doImport(t, dst, data, importOpts(dst))
	if len(rep.Imported) != 0 || !reflect.DeepEqual(rep.Skipped, []string{sidB, sidA}) {
		t.Errorf("Imported %v Skipped %v", rep.Imported, rep.Skipped)
	}
	if !strings.Contains(rep.Reasons[sidA], "already exists") || !strings.Contains(rep.Reasons[sidB], "already exists") {
		t.Errorf("reasons = %v", rep.Reasons)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Error("wrote through the planted symlink")
	}
	if readFile(t, target) != "outside\n" {
		t.Error("wrote through the transcript symlink")
	}
}

func TestImportDryRun(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	m, data := exportPool(t, src)
	dst := secondEnv(t, src)
	writeFile(t, filepath.Join(dst.root.Dir, src.slug(t), sidB+".jsonl"), userRec(sidB, src.cwd, "2026-08-14T10:00:00Z", "here"))
	opts := importOpts(dst)
	opts.DryRun = true
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidA}) || !reflect.DeepEqual(rep.Skipped, []string{sidB}) || !reflect.DeepEqual(rep.MemoryDirs, []string{dst.memDir(t)}) {
		t.Errorf("plan = %+v", rep)
	}
	if len(rep.Verify) != 1 || !strings.Contains(rep.Verify[0], sidA) || !rep.SweepDate.Equal(fixedNow.Add(15*day)) || rep.Bytes != m.Totals.Bytes {
		t.Errorf("plan report = %+v", rep)
	}
	if exists(filepath.Join(dst.cfgDir, StagingSubdir)) || exists(filepath.Join(dst.cfgDir, imports.Subdir)) || exists(filepath.Join(dst.root.Dir, src.slug(t), sidA+".jsonl")) || exists(dst.memDir(t)) {
		t.Error("dry run wrote something")
	}
	if exists(filepath.Join(dst.claudeDir, transcripts.HistoryFile)) {
		t.Error("dry run touched history")
	}
}

func TestImportOptionValidation(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	cases := []struct {
		name string
		mod  func(*ImportOptions)
		want string
	}{
		{"into missing", func(o *ImportOptions) { o.Into = filepath.Join(t.TempDir(), "gone") }, "is not a directory"},
		{"into and map", func(o *ImportOptions) {
			o.Into = t.TempDir()
			o.Map = []rehome.Mapping{{Old: "/a", New: "/b"}}
		}, "--into and --map are mutually exclusive"},
		{"carry trust as-is", func(o *ImportOptions) { o.AsIs, o.CarryTrust = true, true }, "--carry-trust needs a mapped placement"},
		{"bad conflict", func(o *ImportOptions) { o.OnConflict = "fork" }, "invalid --on-conflict"},
		{"bad memory", func(o *ImportOptions) { o.Memory = "clobber" }, "invalid --memory"},
		{"orphan", func(o *ImportOptions) { o.Dest.Orphan = true; o.Dest.Owner = "ghost" }, "orphans are never destinations"},
		{"unknown owner", func(o *ImportOptions) { o.Dest.Owner = "ghost" }, "not a Claude config dir bffs knows"},
		{"unset", func(o *ImportOptions) { o.Dest = transcripts.Root{} }, "not set"},
		{"missing config dir", func(o *ImportOptions) {
			x := filepath.Join(t.TempDir(), "x")
			o.Dest = transcripts.Root{Dir: filepath.Join(x, "projects"), ConfigDir: x}
		}, "does not exist"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := importOpts(dst)
			c.mod(&opts)
			_, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	// Full-isolation destination: the account's own root passes the identity check.
	if err := store.SaveAccounts(dst.cfgDir, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull}}}); err != nil {
		t.Fatal(err)
	}
	accs, _ := store.LoadAccounts(dst.cfgDir)
	roots, err := transcripts.Roots(dst.cfgDir, dst.claudeDir, accs, store.State{})
	if err != nil {
		t.Fatal(err)
	}
	work, err := transcripts.RootFor(roots, "work")
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, work.ConfigDir)
	opts := importOpts(dst)
	opts.Dest = work
	rep := doImport(t, dst, data, opts)
	if !exists(filepath.Join(work.Dir, src.slug(t), sidA+".jsonl")) {
		t.Error("session not placed in the account root")
	}
	if len(rep.Verify) == 0 || !strings.Contains(rep.Verify[0], " && BFFS_ACCOUNT=work claude --resume ") {
		t.Errorf("Verify = %v", rep.Verify)
	}
	// An explicit account on the shared root prefixes the verify line too.
	dst2 := secondEnv(t, src)
	opts = importOpts(dst2)
	opts.Account = "aviate"
	rep = doImport(t, dst2, data, opts)
	if len(rep.Verify) == 0 || !strings.Contains(rep.Verify[0], " && BFFS_ACCOUNT=aviate claude --resume ") {
		t.Errorf("Verify = %v", rep.Verify)
	}
	recs, _ := imports.Load(dst2.cfgDir)
	if recs[0].Account != "aviate" {
		t.Errorf("record account = %q", recs[0].Account)
	}
}

// handBundle builds a one-session bundle whose transcript bytes are given
// verbatim, so tests can ship inconsistent content.
func handBundle(t *testing.T, slug, sid string, transcript []byte) (*bundle.Manifest, []byte) {
	t.Helper()
	sum := sha256.Sum256(transcript)
	p := "projects/" + slug + "/" + sid + ".jsonl"
	m := &bundle.Manifest{
		Format: bundle.FormatVersion, BundleID: "6f1e2c0a-1111-4222-8333-444455556666", Created: fixedNow,
		Source: bundle.Source{Hostname: "mac-a"},
		Entries: []bundle.Entry{{Kind: bundle.EntrySession, Slug: slug, SessionID: sid, Files: []bundle.File{
			{Path: p, Size: int64(len(transcript)), SHA256: hex.EncodeToString(sum[:]), ModTime: fixedNow},
		}}},
		Totals: bundle.Totals{Entries: 1, Files: 1, Bytes: int64(len(transcript))},
	}
	var buf bytes.Buffer
	if _, err := bundle.Build(context.Background(), &buf, m, memOpener{p: transcript}, bundle.CompNone, nil); err != nil {
		t.Fatal(err)
	}
	return m, buf.Bytes()
}

type memOpener map[string][]byte

func (m memOpener) Open(p string) (io.ReadCloser, error) {
	b, ok := m[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func TestImportRefusesMismatchedHead(t *testing.T) {
	dst := newEnv(t)
	m, data := handBundle(t, "-x", sidA, []byte(userRec(sidC, "/x", "2026-08-24T10:00:00Z", "wrong id")))
	_, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(data), importOpts(dst))
	if err == nil || !strings.Contains(err.Error(), "records sessionId") {
		t.Errorf("err = %v", err)
	}
	if exists(StagingDir(dst.cfgDir, m.BundleID)) || exists(filepath.Join(dst.root.Dir, "-x")) {
		t.Error("refused import left something behind")
	}
	// A consistent one lands as-is (its cwd does not exist here).
	_, data = handBundle(t, "-x", sidA, []byte(userRec(sidA, "/x", "2026-08-24T10:00:00Z", "ok")))
	rep := doImport(t, dst, data, importOpts(dst))
	if !reflect.DeepEqual(rep.Pending, []string{sidA}) || !exists(filepath.Join(dst.root.Dir, "-x", sidA+".jsonl")) {
		t.Errorf("report = %+v", rep)
	}
}

func TestImportManifestSHA(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	sel := selectAll(t, src, Parts{}, nil)
	var buf bytes.Buffer
	_, raw, err := Export(context.Background(), sel, &buf, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	dst := secondEnv(t, src)
	opts := importOpts(dst)
	opts.ExpectManifestSHA256 = strings.Repeat("0", 64)
	if _, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(buf.Bytes()), opts); err == nil || !strings.Contains(err.Error(), "manifest sha256") {
		t.Errorf("err = %v", err)
	}
	if exists(filepath.Join(dst.cfgDir, StagingSubdir)) {
		t.Error("staging created before the manifest was accepted")
	}
	sum := sha256.Sum256(raw)
	opts.ExpectManifestSHA256 = hex.EncodeToString(sum[:])
	doImport(t, dst, buf.Bytes(), opts)
	// Trailing bytes are refused in file mode and tolerated in stream mode.
	dst = secondEnv(t, src)
	trailing := append(append([]byte(nil), buf.Bytes()...), "junk"...)
	if _, err := Import(context.Background(), dst.cfgDir, bytes.NewReader(trailing), importOpts(dst)); err == nil {
		t.Error("trailing bytes accepted in file mode")
	}
	if exists(filepath.Join(dst.cfgDir, StagingSubdir, "x")) {
		t.Error("unexpected staging")
	}
	opts = importOpts(dst)
	opts.StreamMode = true
	doImport(t, dst, trailing, opts)
}

func TestImportRecoversLeftovers(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	lone := filepath.Join(dst.root.Dir, "-old", sidC+".jsonl.bffs-tmp")
	writeFile(t, lone, userRec(sidC, "/old", "2026-08-01T10:00:00Z", "interrupted"))
	stale := filepath.Join(dst.cfgDir, StagingSubdir, "11111111-2222-4333-8444-555555555555")
	mkdir(t, stale)
	if got, _ := StaleStaging(dst.cfgDir); !reflect.DeepEqual(got, []string{stale}) {
		t.Errorf("StaleStaging = %v", got)
	}
	if got, err := StaleStaging(t.TempDir()); got != nil || err != nil {
		t.Errorf("StaleStaging(empty) = %v, %v", got, err)
	}
	rep := doImport(t, dst, data, importOpts(dst))
	restored := filepath.Join(dst.root.Dir, "-old", sidC+".jsonl")
	if !exists(restored) || exists(lone) {
		t.Error("lone .bffs-tmp not restored")
	}
	if !containsWarning(rep.Warnings, "recovered "+restored) || !containsWarning(rep.Warnings, "leftover staging directory "+stale) {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if !exists(stale) {
		t.Error("stale staging removed without --clean-staging")
	}
	if got, _ := StaleStaging(dst.cfgDir); !reflect.DeepEqual(got, []string{stale}) {
		t.Errorf("StaleStaging after import = %v", got)
	}
}

func TestImportCustomStagingAndProgress(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	opts := importOpts(dst)
	opts.StagingDir = filepath.Join(t.TempDir(), "custom-stage")
	var phases []string
	opts.Progress = func(p bundle.Progress) { phases = append(phases, p.Phase) }
	doImport(t, dst, data, opts)
	if exists(opts.StagingDir) || exists(filepath.Join(dst.cfgDir, StagingSubdir)) {
		t.Error("custom staging dir kept or default one created")
	}
	sort.Strings(phases)
	if len(phases) == 0 || phases[0] != "unpack" {
		t.Errorf("progress phases = %v", phases)
	}
}

func TestIdentityPlacement(t *testing.T) {
	e := newEnv(t)
	if !identityPlacement(e.cwd, e.cwd) {
		t.Error("existing dir is not identity")
	}
	if identityPlacement(filepath.Join(e.cwd, "missing"), filepath.Join(e.cwd, "missing")) || identityPlacement("", "") {
		t.Error("missing dir counted as identity")
	}
	if identityPlacement(e.cwd, filepath.Join(e.cwd, "..", filepath.Base(e.cwd)+"x")) {
		t.Error("different dir counted as identity")
	}
	if !identityPlacement(e.cwd, filepath.Join(e.cwd, "..", filepath.Base(e.cwd))) {
		t.Error("unclean spelling of the same dir rejected")
	}
	// A manifest cwd that is not absolute here never names "the same
	// directory": "." would resolve against the importing process and "~"
	// against this user's home, both of which exist.
	t.Chdir(e.cwd)
	for _, rel := range []string{".", "~", "proj", "./" + filepath.Base(e.cwd)} {
		if identityPlacement(rel, rel) {
			t.Errorf("relative cwd %q counted as identity", rel)
		}
	}
}
