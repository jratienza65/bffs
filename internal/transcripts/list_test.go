package transcripts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
)

const (
	slugProj  = "-Users-a-proj"
	slugOther = "-Users-a-other"
)

func touchAt(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// listFixture builds one root with:
//   - slugProj: sidA (newest, has a sidecar with two subagent transcripts),
//     sidB (older, a transcript that is NOT valid JSON), a non-UUID .jsonl,
//     a stray directory, a .dir-sync.json
//   - slugOther: sidC (oldest, relocated to cwdDir which exists)
//   - reserved entries memory/, tiny_memory/ and a set-aside slug — all skipped
func listFixture(t *testing.T) (Root, string) {
	t.Helper()
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg, Owner: "", Shared: true, Accounts: []string{"alpha"}}
	cwdDir := t.TempDir()

	proj := filepath.Join(root.Dir, slugProj)
	writeTranscript(t, filepath.Join(proj, sidA+".jsonl"),
		userRec(sidA, "/Users/a/proj", "2026-08-24T10:00:00Z", "first prompt A"),
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "Title A"}),
	)
	touchAt(t, filepath.Join(proj, sidA+".jsonl"), fixedNow.Add(-1*time.Hour))
	writeFile(t, filepath.Join(proj, sidA, SubagentsSubdir, "agent-1.jsonl"), "{}\n")
	writeFile(t, filepath.Join(proj, sidA, SubagentsSubdir, "agent-2.jsonl"), "{}\n")
	writeFile(t, filepath.Join(proj, sidA, SubagentsSubdir, "agent-2.meta.json"), "{}")
	writeFile(t, filepath.Join(proj, sidA, "tool-results", "x.txt"), "out")
	writeFile(t, filepath.Join(proj, sidB+".jsonl"), "this is not json\n")
	touchAt(t, filepath.Join(proj, sidB+".jsonl"), fixedNow.Add(-24*time.Hour))
	writeFile(t, filepath.Join(proj, "notes.jsonl"), "{}\n")
	writeFile(t, filepath.Join(proj, sidB+".dir-sync.json"), "{}")
	mkdir(t, filepath.Join(proj, "stray-dir"))

	other := filepath.Join(root.Dir, slugOther)
	writeTranscript(t, filepath.Join(other, sidC+".jsonl"),
		userRec(sidC, "/Users/a/other", "2026-08-01T09:00:00Z", "first prompt C"),
		rec(map[string]any{"type": "relocated", "sessionId": sidC, "relocatedCwd": cwdDir}),
	)
	touchAt(t, filepath.Join(other, sidC+".jsonl"), fixedNow.Add(-72*time.Hour))

	for _, reserved := range []string{"memory", "tiny_memory", slugProj + ".bffs-replaced-1756987654321"} {
		writeFile(t, filepath.Join(root.Dir, reserved, sidB+".jsonl"), "{}\n")
	}
	writeFile(t, filepath.Join(root.Dir, "bridge-pointer.json"), "{}")
	return root, cwdDir
}

func ids(ss []Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func TestListFastPath(t *testing.T) {
	root, _ := listFixture(t)
	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := ids(ss), []string{sidA, sidB, sidC}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want newest first %v", got, want)
	}
	a := ss[0]
	if a.Slug != slugProj || a.Path != filepath.Join(root.Dir, slugProj, sidA+".jsonl") || a.SidecarDir != filepath.Join(root.Dir, slugProj, sidA) {
		t.Errorf("paths = %+v", a)
	}
	if a.Root.Dir != root.Dir || a.Root.ConfigDir != root.ConfigDir {
		t.Errorf("Root not carried: %+v", a.Root)
	}
	if !a.LastTS.Equal(fixedNow.Add(-1 * time.Hour)) {
		t.Errorf("LastTS = %v, want the mtime", a.LastTS)
	}
	if a.Size == 0 || a.Subagents != 2 {
		t.Errorf("Size %d Subagents %d", a.Size, a.Subagents)
	}
	if ss[1].Subagents != 0 || ss[2].Subagents != 0 {
		t.Errorf("sessions without a subagents dir count %d/%d", ss[1].Subagents, ss[2].Subagents)
	}
	for _, s := range ss {
		if s.Title != "" || s.Cwd != "" || s.HeadCwd != "" || !s.FirstTS.IsZero() || s.Version != "" || s.Live || s.Account != "" || s.Import != nil {
			t.Errorf("fast path must not read transcripts or attribute: %+v", s)
		}
	}
}

func TestListFilters(t *testing.T) {
	root, _ := listFixture(t)
	ctx := context.Background()
	cases := []struct {
		name string
		opts ListOptions
		want []string
	}{
		{"slug", ListOptions{Slug: slugOther}, []string{sidC}},
		{"slug unknown", ListOptions{Slug: "-nope"}, nil},
		{"ids", ListOptions{IDs: []string{strings.ToUpper(sidB), sidC}}, []string{sidB, sidC}},
		{"since", ListOptions{Since: fixedNow.Add(-2 * time.Hour)}, []string{sidA}},
		{"since inclusive", ListOptions{Since: fixedNow.Add(-24 * time.Hour)}, []string{sidA, sidB}},
		{"limit", ListOptions{Limit: 2}, []string{sidA, sidB}},
		{"limit larger than list", ListOptions{Limit: 10}, []string{sidA, sidB, sidC}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.opts.Now = fixedNow
			ss, err := List(ctx, root, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(ss); !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
				t.Errorf("ids = %v, want %v", got, c.want)
			}
		})
	}
}

func TestListLiveness(t *testing.T) {
	root, _ := listFixture(t)
	proj := filepath.Join(root.Dir, slugProj)
	// A fresh compaction temp file marks sidA live; a stale superseded
	// copy does not mark sidB.
	writeFile(t, filepath.Join(proj, sidA+".jsonl.compact.tmp.4f2a"), "")
	touchAt(t, filepath.Join(proj, sidA+".jsonl.compact.tmp.4f2a"), fixedNow.Add(-30*time.Second))
	writeFile(t, filepath.Join(proj, sidB+".jsonl.superseded-1756987654321"), "")
	touchAt(t, filepath.Join(proj, sidB+".jsonl.superseded-1756987654321"), fixedNow.Add(-5*time.Minute))

	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow, Live: map[string]LiveSession{sidC: {PID: 42, SessionID: sidC}}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range ss {
		got[s.ID] = s.Live
	}
	if want := map[string]bool{sidA: true, sidB: false, sidC: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("live = %v, want %v", got, want)
	}
	if got := ids(ss); !reflect.DeepEqual(got, []string{sidA, sidB, sidC}) {
		t.Errorf("sibling files leaked into the listing: %v", got)
	}

	// The superseded copy becomes fresh when Now moves back.
	ss, err = List(context.Background(), root, ListOptions{Now: fixedNow.Add(-4*time.Minute - 30*time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.ID == sidB && !s.Live {
			t.Error("a superseded sibling younger than 60 s must mark the session live")
		}
	}
}

func TestListTitles(t *testing.T) {
	root, cwdDir := listFixture(t)
	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow, Titles: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Session{}
	for _, s := range ss {
		byID[s.ID] = s
	}
	a := byID[sidA]
	if a.Title != "Title A" || a.TitleSource != TitleSourceAI || a.Cwd != "/Users/a/proj" || a.HeadCwd != "/Users/a/proj" || a.Relocated {
		t.Errorf("sidA = %+v", a)
	}
	if a.Version != "2.1.259" || a.GitBranch != "main" || a.PlanSlug != "frolicking-drifting-pizza" || !a.FirstTS.Equal(time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("sidA head fields = %+v", a)
	}
	if a.CwdExists {
		t.Error("/Users/a/proj must not exist")
	}
	c := byID[sidC]
	if !c.Relocated || c.Cwd != cwdDir || c.HeadCwd != "/Users/a/other" || !c.CwdExists {
		t.Errorf("sidC = %+v", c)
	}
	if c.Title != "first prompt C" || c.TitleSource != TitleSourceFirstPrompt {
		t.Errorf("sidC title = %q (%s)", c.Title, c.TitleSource)
	}
	b := byID[sidB]
	if b.Title != "" || b.Cwd != "" {
		t.Errorf("an unparsable transcript must list with empty enrichment: %+v", b)
	}
	// LastTS stays the mtime even with Titles.
	if !a.LastTS.Equal(fixedNow.Add(-1 * time.Hour)) {
		t.Errorf("LastTS = %v", a.LastTS)
	}
}

func TestListHistoryFallback(t *testing.T) {
	root, _ := listFixture(t)
	// sidB's transcript is unparsable; history supplies the title.
	writeFile(t, filepath.Join(root.ConfigDir, HistoryFile), `{"display":"from disk","sessionId":"`+sidB+`"}`+"\n")
	// Only when the head names the session, which garbage does not — so
	// give sidB a torn-but-identifying head instead.
	writeTranscript(t, filepath.Join(root.Dir, slugProj, sidB+".jsonl"),
		rec(map[string]any{"type": "attachment", "sessionId": sidB, "cwd": "/Users/a/proj"}))

	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow, Titles: true, IDs: []string{sidB}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Title != "from disk" || ss[0].TitleSource != TitleSourceHistory {
		t.Fatalf("lazy history fallback: %+v", ss)
	}

	ss, err = List(context.Background(), root, ListOptions{Now: fixedNow, Titles: true, IDs: []string{sidB}, History: HistoryIndex{sidB: "given"}})
	if err != nil {
		t.Fatal(err)
	}
	if ss[0].Title != "given" {
		t.Errorf("an explicit History index must win over the file: %q", ss[0].Title)
	}
}

type fakeAttributor struct{ calls []string }

func (f *fakeAttributor) Attribute(sid, cwd string, firstTS time.Time, owner string) (string, string) {
	f.calls = append(f.calls, strings.Join([]string{sid, cwd, firstTS.UTC().Format(time.RFC3339), owner}, "|"))
	return "acct-" + sid[:8], "fake"
}

func TestListAttributionAndImports(t *testing.T) {
	root, _ := listFixture(t)
	root.Owner = "bravo"
	fa := &fakeAttributor{}
	recs := []imports.Record{{BundleID: "b1", Sessions: []imports.Session{{ID: sidC, Status: imports.StatusPending}}}}
	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow, Titles: true, Attributor: fa, Imports: imports.BySession(recs), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Account != "acct-"+sidA[:8] || ss[0].AttribSource != "fake" {
		t.Fatalf("attribution: %+v", ss)
	}
	if want := []string{sidA + "|/Users/a/proj|2026-08-24T10:00:00Z|bravo"}; !reflect.DeepEqual(fa.calls, want) {
		t.Errorf("attributor calls = %v, want %v (after limiting, with cwd and first ts)", fa.calls, want)
	}
	if ss[0].Import != nil {
		t.Error("sidA has no import record")
	}

	ss, err = List(context.Background(), root, ListOptions{Now: fixedNow, Imports: imports.BySession(recs), IDs: []string{sidC}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Import == nil || ss[0].Import.Record.BundleID != "b1" || ss[0].Import.Session.Status != imports.StatusPending {
		t.Errorf("import ref: %+v", ss)
	}
}

func TestListMissingRootAndCancel(t *testing.T) {
	ss, err := List(context.Background(), Root{Dir: filepath.Join(t.TempDir(), "never")}, ListOptions{})
	if err != nil || len(ss) != 0 {
		t.Errorf("missing projects dir: (%v, %v), want empty", ss, err)
	}
	root, _ := listFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := List(ctx, root, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestArtifactsFor(t *testing.T) {
	root, _ := listFixture(t)
	plans := filepath.Join(root.ConfigDir, PlansSubdir)
	for _, name := range []string{"pizza.md", "pizza-agent-a1.md", "pizza-agent-b2.md", "pizza.workshop.md", "pizza-other.md", "pizzas.md"} {
		writeFile(t, filepath.Join(plans, name), "# plan")
	}
	mkdir(t, filepath.Join(plans, "pizza-agent-dir.md"))
	s := Session{ID: sidA, Slug: slugProj, Path: filepath.Join(root.Dir, slugProj, sidA+".jsonl"), SidecarDir: filepath.Join(root.Dir, slugProj, sidA), PlanSlug: "pizza"}
	a := ArtifactsFor(root, s)
	if a.Transcript != s.Path || a.SidecarDir != s.SidecarDir {
		t.Errorf("artifacts = %+v", a)
	}
	if a.FileHistoryDir != filepath.Join(root.ConfigDir, FileHistorySubdir, sidA) || a.TasksDir != filepath.Join(root.ConfigDir, TasksSubdir, sidA) {
		t.Errorf("dirs = %+v", a)
	}
	want := []string{
		filepath.Join(plans, "pizza-agent-a1.md"), filepath.Join(plans, "pizza-agent-b2.md"),
		filepath.Join(plans, "pizza.md"), filepath.Join(plans, "pizza.workshop.md"),
	}
	if !reflect.DeepEqual(a.PlanFiles, want) {
		t.Errorf("PlanFiles = %v\nwant %v", a.PlanFiles, want)
	}

	if a := ArtifactsFor(root, Session{ID: sidB, Slug: slugProj}); a.PlanFiles != nil || a.Transcript != filepath.Join(root.Dir, slugProj, sidB+".jsonl") {
		t.Errorf("no plan slug / computed paths: %+v", a)
	}
	// A transcript-supplied slug never reaches outside plans/ or globs.
	writeFile(t, filepath.Join(root.ConfigDir, "escape.md"), "# outside")
	for _, bad := range []string{"../escape", "..", ".", "pizza*", "pizz?", "[p]izza", `..\escape`} {
		if a := ArtifactsFor(root, Session{ID: sidA, Slug: slugProj, PlanSlug: bad}); a.PlanFiles != nil {
			t.Errorf("PlanSlug %q listed %v", bad, a.PlanFiles)
		}
	}
}

// sparseRoot creates n transcripts of size bytes each without writing
// their content (Truncate), so the fast path's cost is measured on file
// metadata alone — exactly what it is allowed to touch.
func sparseRoot(tb testing.TB, n int, size int64) Root {
	tb.Helper()
	cfg := tb.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	dir := filepath.Join(root.Dir, slugProj)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < n; i++ {
		sid := sidB[:24] + strings.ToLower(strings.Repeat("0", 12-len(itoa(i)))+itoa(i))
		f, err := os.Create(filepath.Join(dir, sid+".jsonl"))
		if err != nil {
			tb.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			f.Close()
			tb.Skipf("cannot create a sparse %d-byte file here: %v", size, err)
		}
		f.Close()
	}
	return root
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

func TestListFastPathScales(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Truncate allocates the full 2 GB on NTFS")
	}
	root := sparseRoot(t, 200, 10<<20)
	start := time.Now()
	ss, err := List(context.Background(), root, ListOptions{Now: fixedNow})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 200 || ss[0].Size != 10<<20 {
		t.Fatalf("listed %d sessions, first size %d", len(ss), ss[0].Size)
	}
	if elapsed > time.Second {
		t.Errorf("listing 200 × 10 MB transcripts took %v; the fast path must not read them", elapsed)
	}
}

func BenchmarkListFastPath(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("Truncate allocates the full 2 GB on NTFS")
	}
	root := sparseRoot(b, 200, 10<<20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := List(context.Background(), root, ListOptions{Now: fixedNow}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLivePoolProbe lists the real ~/.claude/projects pool when
// BFFS_LIVE_PROBE=1 (never in CI): it reports how long the fast path and
// the titled path take and a few titles, so a developer can compare them
// with claude's own picker.
func TestLivePoolProbe(t *testing.T) {
	if os.Getenv("BFFS_LIVE_PROBE") != "1" {
		t.Skip("set BFFS_LIVE_PROBE=1 to list the real pool")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, ".claude")
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	start := time.Now()
	ss, err := List(context.Background(), root, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, s := range ss {
		total += s.Size
	}
	t.Logf("fast path: %d sessions, %d MB, %v", len(ss), total>>20, time.Since(start))
	start = time.Now()
	ss, err = List(context.Background(), root, ListOptions{Titles: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("titled: %d sessions in %v", len(ss), time.Since(start))
	for i, s := range ss {
		if i == 8 {
			break
		}
		t.Logf("  %s  %-10s %-40.40q cwd=%s relocated=%v live=%v sub=%d", s.ID[:8], s.TitleSource, s.Title, s.Cwd, s.Relocated, s.Live, s.Subagents)
	}
	start = time.Now()
	mems, err := Memories(context.Background(), []Root{root})
	if err != nil {
		t.Fatal(err)
	}
	refs := 0
	for _, m := range mems {
		for _, f := range m.Files {
			refs += len(f.AbsolutePaths) + len(f.AtRefs)
		}
	}
	t.Logf("memories: %d dirs, %d path refs, %v", len(mems), refs, time.Since(start))
}
