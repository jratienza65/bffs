package porter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// fullRoot registers name as a full-isolation oauth account under e's
// bffs home and returns its (still empty) root, config dir created.
func fullRoot(t *testing.T, e *env, names ...string) transcripts.Root {
	t.Helper()
	accs, err := store.LoadAccounts(e.cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if accs.Accounts == nil {
		accs.Accounts = map[string]store.Account{}
	}
	for _, n := range names {
		accs.Accounts[n] = store.Account{Type: store.TypeOAuth, Isolation: store.IsolationFull}
	}
	if err := store.SaveAccounts(e.cfgDir, accs); err != nil {
		t.Fatal(err)
	}
	roots, err := transcripts.Roots(e.cfgDir, e.claudeDir, accs, store.State{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := transcripts.RootFor(roots, names[len(names)-1])
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, root.ConfigDir)
	return root
}

func copyOpts(dst transcripts.Root) ImportOptions {
	return ImportOptions{Dest: dst, Now: fixedNow, TrustMemory: true}
}

// copyRecord loads the single import record under cfgDir.
func copyRecord(t *testing.T, cfgDir string) imports.Record {
	t.Helper()
	recs, err := imports.Load(cfgDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	return recs[0]
}

func TestCopyLocalSameRoot(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	before := digestTree(t, e.claudeDir)
	sel := selectAll(t, e, DefaultParts, nil)

	// The same root spelled two ways: the home root itself, and a
	// partial-isolation account whose projects/ resolves to it.
	same := e.root
	same.Shared, same.Accounts = true, []string{"aviate", "innomind"}
	for _, dst := range []transcripts.Root{e.root, same} {
		rep, err := CopyLocal(context.Background(), e.cfgDir, sel, copyOpts(dst), false)
		if !errors.Is(err, ErrSameRoot) {
			t.Fatalf("err = %v, want ErrSameRoot", err)
		}
		if !strings.Contains(err.Error(), "nothing to copy") || !strings.Contains(err.Error(), "partial isolation") {
			t.Errorf("message: %v", err)
		}
		if len(rep.Imported)+len(rep.Pending)+len(rep.Warnings) != 0 {
			t.Errorf("report not empty: %+v", rep)
		}
	}
	if !reflect.DeepEqual(digestTree(t, e.claudeDir), before) {
		t.Error("the pool changed")
	}
	if stale, _ := StaleStaging(e.cfgDir); len(stale) != 0 {
		t.Errorf("staging left: %v", stale)
	}
	if recs, _ := imports.Load(e.cfgDir); len(recs) != 0 {
		t.Errorf("record written: %v", recs)
	}
	if !SameRoot(e.root, same) || SameRoot(e.root, transcripts.Root{Dir: filepath.Join(t.TempDir(), "projects")}) || SameRoot(transcripts.Root{}, e.root) {
		t.Error("SameRoot")
	}
}

func TestCopyLocalPoolToFull(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	before := digestTree(t, src.claudeDir)
	work := fullRoot(t, src, "work")
	sel := selectAll(t, src, DefaultParts, nil)

	rep, err := CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(work), false)
	if err != nil {
		t.Fatalf("CopyLocal: %v\n%+v", err, rep)
	}
	// Every session lands: the source copies under the excluded root are
	// no collision, and identity placement stamps nothing.
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || len(rep.Skipped)+len(rep.Held)+len(rep.Pending)+len(rep.Rehomed) != 0 {
		t.Errorf("Imported %v Skipped %v Held %v Pending %v Rehomed %v", rep.Imported, rep.Skipped, rep.Held, rep.Pending, rep.Rehomed)
	}
	slugDir := filepath.Join(work.Dir, src.slug(t))
	for _, p := range []string{
		filepath.Join(slugDir, sidA+".jsonl"),
		filepath.Join(slugDir, sidB+".jsonl"),
		filepath.Join(slugDir, sidA, "tool-results", "abc.txt"),
		filepath.Join(slugDir, sidA, "subagents", "agent-1.jsonl"),
		filepath.Join(work.ConfigDir, transcripts.FileHistorySubdir, sidA, "0123456789abcdef@v1"),
		filepath.Join(work.ConfigDir, transcripts.PlansSubdir, planSlug+".md"),
		filepath.Join(slugDir, transcripts.MemorySubdir, "topic.md"),
		filepath.Join(slugDir, transcripts.MemorySubdir, transcripts.MemoryIndexFile),
		filepath.Join(slugDir, transcripts.MemorySubdir, transcripts.MemoryLogsSubdir, "2026", "08", "24", "0f3b2c1e-fix.md"),
	} {
		if !exists(p) {
			t.Errorf("%s not copied", p)
		}
	}
	if strings.Contains(readFile(t, filepath.Join(slugDir, sidA+".jsonl")), "relocated") {
		t.Error("identity copy stamped a relocated record")
	}
	if got := readFile(t, filepath.Join(slugDir, transcripts.MemorySubdir, "topic.md")); !strings.Contains(got, "pinned: true") {
		t.Errorf("pinned memory not trusted on a same-machine copy:\n%s", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(slugDir, transcripts.MemorySubdir)); len(entries) != 3 { // MEMORY.md, topic.md, logs/
		for _, en := range entries {
			t.Logf("memory entry: %s", en.Name())
		}
		t.Errorf("memory dir holds %d entries, want the 3 exported ones under their own names", len(entries))
	}
	if len(rep.MemoryDirs) != 1 || rep.HistoryLines == 0 {
		t.Errorf("MemoryDirs %v HistoryLines %d", rep.MemoryDirs, rep.HistoryLines)
	}
	if len(rep.Verify) != 1 || !strings.Contains(rep.Verify[0], "cd "+rehome.ShellQuote(src.cwd)+" && BFFS_ACCOUNT=work claude --resume "+sidB) {
		t.Errorf("Verify = %v", rep.Verify)
	}
	rec := copyRecord(t, src.cfgDir)
	if rec.Kind != imports.KindCopy || rec.BundleID != rep.BundleID || len(rec.Mapping) != 0 || rec.DestRoot != work.Dir {
		t.Errorf("record: kind %q id %q mapping %v dest %q", rec.Kind, rec.BundleID, rec.Mapping, rec.DestRoot)
	}
	if err := imports.ValidateBundleID(rec.BundleID); err != nil {
		t.Errorf("bundle id %q: %v", rec.BundleID, err)
	}
	for _, s := range rec.Sessions {
		if s.Status != imports.StatusPlaced || s.NewCwd != src.cwd {
			t.Errorf("session %s: status %q new cwd %q", short8(s.ID), s.Status, s.NewCwd)
		}
	}
	if len(rec.Memories) != 1 || rec.Memories[0].Status != imports.StatusPlaced {
		t.Errorf("memories: %+v", rec.Memories)
	}
	if !reflect.DeepEqual(digestTree(t, src.claudeDir), before) {
		t.Error("a copy changed the source")
	}
	if stale, _ := StaleStaging(src.cfgDir); len(stale) != 0 {
		t.Errorf("staging left: %v", stale)
	}
	// Without TrustMemory the same-machine copy still neutralises pinning,
	// like any import.
	other := fullRoot(t, src, "work", "other")
	opts := copyOpts(other)
	opts.TrustMemory = false
	if _, err := CopyLocal(context.Background(), src.cfgDir, sel, opts, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(other.Dir, src.slug(t), transcripts.MemorySubdir, "topic.md")); !strings.Contains(got, "pinned-imported: true") {
		t.Errorf("untrusted copy kept pinning:\n%s", got)
	}
}

func TestCopyLocalDryRun(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	work := fullRoot(t, src, "work")
	sel := selectAll(t, src, DefaultParts, nil)
	opts := copyOpts(work)
	opts.DryRun = true
	rep, err := CopyLocal(context.Background(), src.cfgDir, sel, opts, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || len(rep.MemoryDirs) != 1 || len(rep.Verify) != 1 {
		t.Errorf("plan: %+v", rep)
	}
	if entries, _ := os.ReadDir(work.ConfigDir); len(entries) != 0 {
		t.Errorf("dry run wrote into %s: %v", work.ConfigDir, entries)
	}
	if recs, _ := imports.Load(src.cfgDir); len(recs) != 0 {
		t.Errorf("dry run wrote a record: %v", recs)
	}
	if stale, _ := StaleStaging(src.cfgDir); len(stale) != 0 {
		t.Errorf("staging left: %v", stale)
	}
	if !exists(filepath.Join(src.slugDir(t), sidA+".jsonl")) {
		t.Error("dry-run move removed the source")
	}
}

func TestCopyLocalMoveRefusesLive(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	before := digestTree(t, src.claudeDir)
	work := fullRoot(t, src, "work")
	live := map[string]transcripts.LiveSession{sidB: {PID: 4242, SessionID: sidB}}
	sel := selectAll(t, src, DefaultParts, live)
	opts := copyOpts(work)
	opts.Live = live
	_, err := CopyLocal(context.Background(), src.cfgDir, sel, opts, true)
	if !errors.Is(err, ErrLiveSource) || !strings.Contains(err.Error(), "session "+sidB+" is open in a running claude (pid 4242)") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(work.ConfigDir); len(entries) != 0 {
		t.Errorf("refused move wrote into %s: %v", work.ConfigDir, entries)
	}
	if recs, _ := imports.Load(src.cfgDir); len(recs) != 0 {
		t.Errorf("refused move wrote a record: %v", recs)
	}
	if !reflect.DeepEqual(digestTree(t, src.claudeDir), before) {
		t.Error("refused move changed the source")
	}
	// The Selection's own Live flag refuses too, without a pid.
	sel2 := selectAll(t, src, DefaultParts, nil)
	for i := range sel2.Sessions {
		if sel2.Sessions[i].ID == sidA {
			sel2.Sessions[i].Live = true
		}
	}
	if _, err := CopyLocal(context.Background(), src.cfgDir, sel2, copyOpts(work), true); !errors.Is(err, ErrLiveSource) || strings.Contains(err.Error(), "pid") {
		t.Errorf("Selection.Live: %v", err)
	}
	// A plain copy of a live session is not refused here (the caller
	// decides; export allows a read-only copy).
	if _, err := CopyLocal(context.Background(), src.cfgDir, sel, opts, false); err != nil {
		t.Errorf("copy with a live session: %v", err)
	}
}

func TestCopyLocalMoveVerifiesBeforeDeleting(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	before := digestTree(t, src.claudeDir)
	sel := selectAll(t, src, DefaultParts, nil)

	// A landed file corrupted before verification: the move is refused,
	// the copies stay, the source is untouched.
	work := fullRoot(t, src, "work")
	corrupted := filepath.Join(work.Dir, src.slug(t), sidA, "tool-results", "abc.txt")
	copyLanded = func(cfgDir string, rep Report) {
		if err := os.WriteFile(corrupted, []byte("bit rot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { copyLanded = nil }()
	rep, err := CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(work), true)
	if !errors.Is(err, ErrMoveVerify) || !strings.Contains(err.Error(), "projects/"+src.slug(t)+"/"+sidA+"/tool-results/abc.txt") || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) {
		t.Errorf("Imported = %v", rep.Imported)
	}
	if !reflect.DeepEqual(digestTree(t, src.claudeDir), before) {
		t.Error("refused move changed the source")
	}
	if !exists(filepath.Join(work.Dir, src.slug(t), sidB+".jsonl")) {
		t.Error("the copies did not stay")
	}
	if rec := copyRecord(t, src.cfgDir); rec.Kind != imports.KindCopy {
		t.Errorf("record kind = %q", rec.Kind)
	}
	copyLanded = nil

	// A clean move: every listed source file goes, nothing else does.
	work2 := fullRoot(t, src, "work", "work2")
	rep, err = CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(work2), true)
	if err != nil {
		t.Fatalf("move: %v\n%+v", err, rep)
	}
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) {
		t.Errorf("Imported = %v", rep.Imported)
	}
	slugDir := src.slugDir(t)
	mem := src.memDir(t)
	for _, p := range []string{
		filepath.Join(slugDir, sidA+".jsonl"),
		filepath.Join(slugDir, sidB+".jsonl"),
		filepath.Join(slugDir, sidA), // every sidecar file was listed, so the dir is pruned
		filepath.Join(src.claudeDir, transcripts.FileHistorySubdir, sidA),
		filepath.Join(src.claudeDir, transcripts.PlansSubdir, planSlug+".md"),
		filepath.Join(mem, "topic.md"),
		filepath.Join(mem, transcripts.MemoryIndexFile),
		filepath.Join(mem, transcripts.MemoryLogsSubdir),
	} {
		if exists(p) {
			t.Errorf("%s still at the source", p)
		}
	}
	for _, p := range []string{
		filepath.Join(src.claudeDir, transcripts.HistoryFile),         // never touched
		filepath.Join(src.claudeDir, transcripts.TasksSubdir, sidA),   // tasks are not in DefaultParts
		filepath.Join(mem, "notes.txt"),                               // not exported, so not removed
		filepath.Join(mem, transcripts.MemoryProposalsSubdir, "p.md"), // idem
		filepath.Join(mem, "index_persist", "x.md"),                   // idem
		filepath.Join(src.claudeDir, transcripts.PlansSubdir),
		slugDir,
	} {
		if !exists(p) {
			t.Errorf("%s removed although the manifest never listed it", p)
		}
	}
	if got, want := readFile(t, filepath.Join(src.claudeDir, transcripts.HistoryFile)), before[transcripts.HistoryFile]; want == "" || digestTree(t, src.claudeDir)[transcripts.HistoryFile] != want {
		t.Errorf("history.jsonl changed:\n%s", got)
	}
	// The destination has it all.
	dstSlug := filepath.Join(work2.Dir, src.slug(t))
	for _, p := range []string{
		filepath.Join(dstSlug, sidA+".jsonl"),
		filepath.Join(dstSlug, sidA, "tool-results", "abc.txt"),
		filepath.Join(dstSlug, transcripts.MemorySubdir, "topic.md"),
		filepath.Join(work2.ConfigDir, transcripts.PlansSubdir, planSlug+".md"),
	} {
		if !exists(p) {
			t.Errorf("%s missing at the destination", p)
		}
	}
	if containsWarning(rep.Warnings, "kept at") {
		t.Errorf("warnings: %v", rep.Warnings)
	}
}

func TestCopyLocalMoveKeepsMergedMemory(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	work := fullRoot(t, src, "work")
	// The destination already has a memory directory for the project with
	// a different index and topic: the merge appends to MEMORY.md and lands
	// the topic as a side file, so the source memory is kept.
	dstMem := filepath.Join(work.Dir, src.slug(t), transcripts.MemorySubdir)
	writeFile(t, filepath.Join(dstMem, transcripts.MemoryIndexFile), "# Memory Index\n- [other](other.md) - theirs\n")
	writeFile(t, filepath.Join(dstMem, "topic.md"), "different\n")
	sel := selectAll(t, src, DefaultParts, nil)
	rep, err := CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(work), true)
	if err != nil {
		t.Fatalf("move: %v\n%+v", err, rep)
	}
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || exists(filepath.Join(src.slugDir(t), sidA+".jsonl")) {
		t.Errorf("sessions not moved: %v", rep.Imported)
	}
	mem := src.memDir(t)
	for _, p := range []string{filepath.Join(mem, "topic.md"), filepath.Join(mem, transcripts.MemoryIndexFile)} {
		if !exists(p) {
			t.Errorf("%s removed although it did not read back at the destination", p)
		}
	}
	id8 := short8(rep.BundleID)
	if !exists(filepath.Join(dstMem, "topic.imported-"+id8+".md")) || !strings.Contains(readFile(t, filepath.Join(dstMem, transcripts.MemoryIndexFile)), "## Imported") {
		t.Error("merge did not happen as expected")
	}
	if !containsWarning(rep.Warnings, "kept at "+mem) || !containsWarning(rep.Warnings, transcripts.MemoryIndexFile) {
		t.Errorf("warnings = %v", rep.Warnings)
	}
}

func TestCopyLocalMemoryWithoutTranscripts(t *testing.T) {
	src := newEnv(t)
	// A project whose sessions Claude has swept: memory only, and the
	// directory known to the source's .claude.json.
	mem := src.memDir(t)
	writeFile(t, filepath.Join(mem, transcripts.MemoryIndexFile), "# Memory Index\n- [topic](topic.md) - notes\n")
	writeFile(t, filepath.Join(mem, "topic.md"), "remember this\n")
	sel, err := Select(context.Background(), src.root, nil, nil, SelectOptions{Projects: []string{src.cwd}, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Sessions) != 0 || len(sel.Memories) != 1 || sel.Memories[0].Cwd != "" {
		t.Fatalf("selection: %d sessions, %+v", len(sel.Sessions), sel.Memories)
	}

	// Unknown to .claude.json: the copy cannot confirm the directory and
	// the memory lands as side files (plan §9.9).
	blind := fullRoot(t, src, "blind")
	if _, err := CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(blind), false); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(blind.Dir, src.slug(t), transcripts.MemorySubdir)); len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "topic.imported-") {
		t.Errorf("unconfirmed memory: %v", entries)
	}

	// Known: the directory is confirmed and the files keep their names.
	writeFile(t, src.root.ClaudeJSON, `{"projects":{`+jsonString(src.cwd)+`:{"hasTrustDialogAccepted":true}}}`)
	work := fullRoot(t, src, "blind", "work")
	rep, err := CopyLocal(context.Background(), src.cfgDir, sel, copyOpts(work), false)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	dst := filepath.Join(work.Dir, src.slug(t), transcripts.MemorySubdir)
	if !exists(filepath.Join(dst, "topic.md")) || !exists(filepath.Join(dst, transcripts.MemoryIndexFile)) {
		entries, _ := os.ReadDir(dst)
		t.Errorf("confirmed memory not copied under its own names: %v", entries)
	}
	var mine *imports.Record
	recs, _ := imports.Load(src.cfgDir)
	for i := range recs {
		if recs[i].BundleID == rep.BundleID {
			mine = &recs[i]
		}
	}
	if mine == nil || mine.Kind != imports.KindCopy || len(mine.Memories) != 1 || mine.Memories[0].OldCwd != src.cwd || mine.Memories[0].Status != imports.StatusPlaced {
		t.Errorf("record %s: %+v", short8(rep.BundleID), mine)
	}
}

func jsonString(s string) string {
	b := []byte{'"'}
	for _, c := range []byte(s) {
		switch c {
		case '"', '\\':
			b = append(b, '\\', c)
		default:
			b = append(b, c)
		}
	}
	return string(append(b, '"'))
}

func TestExcludeUnderRoot(t *testing.T) {
	pool := t.TempDir()
	other := t.TempDir()
	mkdir(t, filepath.Join(pool, "a"))
	mkdir(t, filepath.Join(other, "a"))
	in := transcripts.Session{ID: sidA, Path: filepath.Join(pool, "a", sidA+".jsonl")}
	out := transcripts.Session{ID: sidA, Path: filepath.Join(other, "a", sidA+".jsonl")}
	gone := transcripts.Session{ID: sidB, Path: filepath.Join(pool, "a", sidB+".jsonl")} // never written: the directory still decides
	got := excludeUnderRoot([]transcripts.Session{in, out, gone}, pool)
	if len(got) != 1 || got[0].Path != out.Path {
		t.Errorf("excludeUnderRoot = %v", got)
	}
	if got := excludeUnderRoot([]transcripts.Session{in}, ""); len(got) != 1 {
		t.Errorf("empty root filtered: %v", got)
	}
	// A sibling directory whose name merely extends the pool's is outside.
	sibling := transcripts.Session{ID: sidC, Path: filepath.Join(pool+"x", sidC+".jsonl")}
	if got := excludeUnderRoot([]transcripts.Session{sibling}, pool); len(got) != 1 {
		t.Errorf("prefix sibling excluded: %v", got)
	}
}

// The collision scan of a plain Import honours ExcludeRoot: a transcript
// of the incoming session listed under another slug of the destination
// refuses the session ("would make claude --resume ambiguous") unless it
// lies under the excluded pool. The pool here is the destination itself,
// which no caller passes — the test exercises the wiring, not a layout.
func TestImportExcludeRootOption(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	// The stray copy records another cwd: one recording src.cwd would make
	// transcripts.ProjectDirFor pick -other as the target directory itself.
	writeFile(t, filepath.Join(dst.root.Dir, "-other", sidB+".jsonl"), userRec(sidB, filepath.Join(t.TempDir(), "other"), "2026-08-14T10:00:00Z", "older copy"))
	opts := importOpts(dst)
	opts.DryRun = true
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Skipped, []string{sidB}) || !strings.Contains(rep.Reasons[sidB], "exists under -other") {
		t.Fatalf("without ExcludeRoot: Skipped %v Reasons %v", rep.Skipped, rep.Reasons)
	}
	opts.ExcludeRoot = dst.root.Dir
	rep = doImport(t, dst, data, opts)
	if len(rep.Skipped) != 0 || !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) {
		t.Errorf("with ExcludeRoot: Skipped %v Imported %v", rep.Skipped, rep.Imported)
	}
	// A pool elsewhere excludes nothing here.
	opts.ExcludeRoot = t.TempDir()
	rep = doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Skipped, []string{sidB}) {
		t.Errorf("unrelated ExcludeRoot: Skipped %v", rep.Skipped)
	}
}

func TestImportedMarkdownName(t *testing.T) {
	if got := importedMarkdownName("topic.md", "6f1e2c0a"); got != "topic.imported-6f1e2c0a.md" {
		t.Errorf("md: %q", got)
	}
	if got := importedMarkdownName("0123@v1", "6f1e2c0a"); got != "0123@v1.imported-6f1e2c0a" {
		t.Errorf("plain: %q", got)
	}
	if got := importedMarkdownName(transcripts.MemoryIndexFile, "6f1e2c0a"); got != "MEMORY.imported-6f1e2c0a.md" {
		t.Errorf("index: %q", got)
	}
}
