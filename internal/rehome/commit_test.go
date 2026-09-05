package rehome

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

const (
	testSID  = "0c5e19b2-1111-4222-8333-444455556666"
	testSlug = "-home-jonas-src-bffs"
	testID8  = "6f1e2c0a"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

func chtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func near(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < 2*time.Second
}

// stageSession lays out a staged session under dir and returns the request
// that commits it. The transcript ends without a newline so stamp tests can
// see the separator logic.
func stageSession(t *testing.T, dir string) CommitRequest {
	t.Helper()
	tr := filepath.Join(dir, "projects", testSlug, testSID+".jsonl")
	write(t, tr, `{"type":"user","cwd":"/Users/jonas/build/projects/bffs","sessionId":"`+testSID+`"}`+"\n"+`{"type":"assistant"}`)
	side := filepath.Join(dir, "projects", testSlug, testSID)
	write(t, filepath.Join(side, "subagents", "agent-1.jsonl"), "{}\n")
	write(t, filepath.Join(side, "tool-results", "abc.txt"), "output\n")
	fh := filepath.Join(dir, "file-history", testSID, "0123456789abcdef@v1")
	write(t, fh, "backup-v1")
	plan := filepath.Join(dir, "plans", "frolicking-pizza.md")
	write(t, plan, "# plan\n")
	tasks := filepath.Join(dir, "tasks", testSID)
	write(t, filepath.Join(tasks, "1.json"), `{"id":1}`)
	old := fixedNow.Add(-40 * 24 * time.Hour)
	for _, p := range []string{filepath.Join(side, "subagents", "agent-1.jsonl"), filepath.Join(side, "tool-results", "abc.txt"), fh, plan, filepath.Join(tasks, "1.json")} {
		chtimes(t, p, old)
	}
	return CommitRequest{
		SessionID:   testSID,
		Slug:        testSlug,
		Transcript:  tr,
		SidecarDir:  side,
		FileHistory: []string{fh},
		Plans:       []string{plan},
		TasksDir:    tasks,
		OrigMtime:   fixedNow.Add(-10 * 24 * time.Hour),
		ID8:         testID8,
	}
}

func openDest(t *testing.T) (*os.Root, string) {
	t.Helper()
	cfg := t.TempDir()
	root, err := os.OpenRoot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root, cfg
}

func defaultPolicy() MtimePolicy {
	return MtimePolicy{CleanupPeriodDays: 30, Now: fixedNow}
}

// filesUnder lists every regular file below dir, relative and slash-separated.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCommitSessionHappyPath(t *testing.T) {
	staging := t.TempDir()
	req := stageSession(t, staging)
	req.Stamp = []byte(`{"type":"relocated","sessionId":"` + testSID + `","relocatedCwd":"/home/jonas/src/bffs"}` + "\n")
	req.History = [][]byte{[]byte(`{"display":"hi","project":"/Users/jonas/build/projects/bffs","sessionId":"` + testSID + `","timestamp":1700000000000}`)}
	req.HistoryProject = "/home/jonas/src/bffs"
	dest, cfg := openDest(t)

	var steps []string
	res, err := CommitSession(context.Background(), dest, req, defaultPolicy(), func(s string) error {
		steps = append(steps, s)
		return nil
	})
	if err != nil {
		t.Fatalf("CommitSession: %v", err)
	}
	if want := []string{StepSidecar, StepTranscript, StepDone}; strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Errorf("journal steps = %v, want %v", steps, want)
	}

	tr := filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")
	got := readFile(t, tr)
	if !strings.HasSuffix(got, "\n"+string(req.Stamp)) {
		t.Errorf("stamp not appended after a newline:\n%s", got)
	}
	if strings.Count(got, "\n") != 3 {
		t.Errorf("transcript newline count = %d, want 3:\n%q", strings.Count(got, "\n"), got)
	}
	for _, p := range []string{
		filepath.Join(cfg, "projects", testSlug, testSID, "subagents", "agent-1.jsonl"),
		filepath.Join(cfg, "projects", testSlug, testSID, "tool-results", "abc.txt"),
		filepath.Join(cfg, "file-history", testSID, "0123456789abcdef@v1"),
		filepath.Join(cfg, "plans", "frolicking-pizza.md"),
		filepath.Join(cfg, "tasks", testSID, "1.json"),
	} {
		if !exists(p) {
			t.Errorf("%s missing", p)
			continue
		}
		if mt := mtimeOf(t, p); !near(mt, fixedNow) {
			t.Errorf("%s mtime = %v, want now (%v)", p, mt, fixedNow)
		}
	}
	if mt := mtimeOf(t, tr); !near(mt, req.OrigMtime) {
		t.Errorf("transcript mtime = %v, want original %v", mt, req.OrigMtime)
	}
	if res.MtimeRaised || !near(res.Mtime, req.OrigMtime) {
		t.Errorf("result mtime = %v raised=%v", res.Mtime, res.MtimeRaised)
	}
	if res.HistoryAdded != 1 {
		t.Errorf("HistoryAdded = %d, want 1", res.HistoryAdded)
	}
	hist := readFile(t, filepath.Join(cfg, "history.jsonl"))
	if !strings.Contains(hist, `"project":"/home/jonas/src/bffs"`) {
		t.Errorf("history project not rewritten: %s", hist)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
	wantPlaced := []string{
		"projects/" + testSlug + "/" + testSID + "/",
		"file-history/" + testSID + "/0123456789abcdef@v1",
		"plans/frolicking-pizza.md",
		"tasks/" + testSID + "/",
		"projects/" + testSlug + "/" + testSID + ".jsonl",
	}
	if strings.Join(res.Placed, "\n") != strings.Join(wantPlaced, "\n") {
		t.Errorf("Placed = %v, want %v", res.Placed, wantPlaced)
	}
	for _, f := range filesUnder(t, cfg) {
		if strings.Contains(f, tmpSuffix) {
			t.Errorf("tmp entry left behind: %s", f)
		}
	}
	if info, err := os.Stat(tr); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("transcript mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestCommitSessionStampNeedsNoSeparatorAfterNewline(t *testing.T) {
	staging := t.TempDir()
	req := stageSession(t, staging)
	write(t, req.Transcript, "{\"a\":1}\n")
	req.Stamp = []byte("{\"type\":\"relocated\"}\n")
	dest, cfg := openDest(t)
	if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(cfg, "projects", testSlug, testSID+".jsonl"))
	if got != "{\"a\":1}\n{\"type\":\"relocated\"}\n" {
		t.Errorf("transcript = %q", got)
	}
}

func TestCommitSessionNoStampCopiesVerbatim(t *testing.T) {
	staging := t.TempDir()
	req := stageSession(t, staging)
	src := readFile(t, req.Transcript)
	dest, cfg := openDest(t)
	if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")); got != src {
		t.Errorf("transcript changed: %q != %q", got, src)
	}
}

func TestCommitSessionRollsBackWhenTranscriptRenameFails(t *testing.T) {
	staging := t.TempDir()
	req := stageSession(t, staging)
	dest, cfg := openDest(t)
	// Pre-existing plans/ dir with an unrelated file: must survive intact.
	write(t, filepath.Join(cfg, "plans", "other.md"), "keep\n")

	var renames []string
	orig := renameFn
	renameFn = func(r *os.Root, oldname, newname string) error {
		renames = append(renames, filepath.ToSlash(newname))
		if strings.HasSuffix(newname, transcripts.TranscriptExt) {
			return errors.New("injected")
		}
		return r.Rename(oldname, newname)
	}
	t.Cleanup(func() { renameFn = orig })

	var steps []string
	_, err := CommitSession(context.Background(), dest, req, defaultPolicy(), func(s string) error {
		steps = append(steps, s)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("err = %v, want injected failure", err)
	}
	if len(renames) != 2 || !strings.HasSuffix(renames[0], "/"+testSID) || !strings.HasSuffix(renames[1], testSID+".jsonl") {
		t.Errorf("rename order = %v, want sidecar then transcript", renames)
	}
	if strings.Join(steps, ",") != StepSidecar+","+StepTranscript {
		t.Errorf("steps = %v", steps)
	}
	if got := filesUnder(t, cfg); len(got) != 1 || got[0] != "plans/other.md" {
		t.Errorf("destination not restored; files = %v", got)
	}
	if exists(filepath.Join(cfg, "projects", testSlug)) {
		t.Errorf("slug dir created by the commit survived rollback")
	}
	if exists(filepath.Join(cfg, "file-history")) || exists(filepath.Join(cfg, "tasks")) {
		t.Errorf("directories created by the commit survived rollback")
	}
	// The staged sources are untouched, so a re-run succeeds.
	renameFn = orig
	if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err != nil {
		t.Fatalf("re-run after rollback: %v", err)
	}
}

func TestCommitSessionRollsBackWhenHistoryFails(t *testing.T) {
	staging := t.TempDir()
	req := stageSession(t, staging)
	req.History = [][]byte{[]byte(`{"sessionId":"` + testSID + `","timestamp":1}`)}
	dest, cfg := openDest(t)
	// A directory where history.jsonl must be a file makes the append fail
	// after the transcript has landed.
	if err := os.Mkdir(filepath.Join(cfg, "history.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err == nil {
		t.Fatal("expected failure")
	}
	if exists(filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")) || exists(filepath.Join(cfg, "projects", testSlug, testSID)) {
		t.Errorf("session survived rollback")
	}
}

func TestCommitSessionMtimeRules(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name   string
		days   int
		orig   time.Time
		want   time.Time
		raised bool
	}{
		{"10d-old preserved at 30", 30, fixedNow.Add(-10 * day), fixedNow.Add(-10 * day), false},
		{"45d-old raised to now-15d at 30", 30, fixedNow.Add(-45 * day), fixedNow.Add(-15 * day), true},
		{"8 days -> floor now-4d", 8, fixedNow.Add(-45 * day), fixedNow.Add(-4 * day), true},
		{"0 -> untouched", 0, fixedNow.Add(-45 * day), fixedNow.Add(-45 * day), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := stageSession(t, t.TempDir())
			req.OrigMtime = tc.orig
			dest, cfg := openDest(t)
			res, err := CommitSession(context.Background(), dest, req, MtimePolicy{CleanupPeriodDays: tc.days, Now: fixedNow}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := mtimeOf(t, filepath.Join(cfg, "projects", testSlug, testSID+".jsonl"))
			if !near(got, tc.want) {
				t.Errorf("transcript mtime = %v, want %v", got, tc.want)
			}
			if res.MtimeRaised != tc.raised {
				t.Errorf("MtimeRaised = %v, want %v", res.MtimeRaised, tc.raised)
			}
		})
	}
}

func TestCommitSessionPreserveSidecarsKeepsSourceMtimes(t *testing.T) {
	req := stageSession(t, t.TempDir())
	dest, cfg := openDest(t)
	p := MtimePolicy{CleanupPeriodDays: 30, Now: fixedNow, PreserveSidecars: true}
	if _, err := CommitSession(context.Background(), dest, req, p, nil); err != nil {
		t.Fatal(err)
	}
	old := fixedNow.Add(-40 * 24 * time.Hour)
	for _, rel := range []string{
		filepath.Join("projects", testSlug, testSID, "subagents", "agent-1.jsonl"),
		filepath.Join("file-history", testSID, "0123456789abcdef@v1"),
		filepath.Join("plans", "frolicking-pizza.md"),
		filepath.Join("tasks", testSID, "1.json"),
	} {
		if mt := mtimeOf(t, filepath.Join(cfg, rel)); !near(mt, old) {
			t.Errorf("%s mtime = %v, want source %v", rel, mt, old)
		}
	}
}

func TestCommitSessionCollisions(t *testing.T) {
	req := stageSession(t, t.TempDir())
	dest, cfg := openDest(t)
	// file-history: identical → skipped; plan: different → .imported; tasks: existing → skipped.
	write(t, filepath.Join(cfg, "file-history", testSID, "0123456789abcdef@v1"), "backup-v1")
	chtimes(t, filepath.Join(cfg, "file-history", testSID, "0123456789abcdef@v1"), fixedNow.Add(-5*24*time.Hour))
	write(t, filepath.Join(cfg, "plans", "frolicking-pizza.md"), "# my own plan\n")
	write(t, filepath.Join(cfg, "tasks", testSID, "mine.json"), "{}")

	res, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(cfg, "plans", "frolicking-pizza.md")); got != "# my own plan\n" {
		t.Errorf("existing plan overwritten: %q", got)
	}
	imported := filepath.Join(cfg, "plans", "frolicking-pizza.imported-"+testID8+".md")
	if got := readFile(t, imported); got != "# plan\n" {
		t.Errorf("imported plan = %q", got)
	}
	if mt := mtimeOf(t, filepath.Join(cfg, "file-history", testSID, "0123456789abcdef@v1")); near(mt, fixedNow) {
		t.Errorf("identical file-history entry was rewritten (mtime refreshed)")
	}
	if exists(filepath.Join(cfg, "file-history", testSID, "0123456789abcdef@v1.imported-"+testID8)) {
		t.Errorf("identical file-history entry produced an .imported copy")
	}
	if exists(filepath.Join(cfg, "tasks", testSID, "1.json")) {
		t.Errorf("existing tasks dir was written into")
	}
	if len(res.Warnings) != 2 {
		t.Fatalf("warnings = %v, want plan collision + tasks skip", res.Warnings)
	}
	if !strings.Contains(res.Warnings[0], "plans/frolicking-pizza.md") || !strings.Contains(res.Warnings[0], "imported-"+testID8) {
		t.Errorf("plan warning = %q", res.Warnings[0])
	}
	if !strings.Contains(res.Warnings[1], "tasks/"+testSID) {
		t.Errorf("tasks warning = %q", res.Warnings[1])
	}
	for _, p := range res.Placed {
		if strings.HasPrefix(p, "tasks/") || strings.HasPrefix(p, "file-history/") {
			t.Errorf("Placed lists a skipped entry: %s", p)
		}
	}

	// Different file-history content → .imported-<id8> + warning.
	req2 := stageSession(t, t.TempDir())
	req2.SessionID = "0c5e19b2-1111-4222-8333-444455557777"
	write(t, req2.FileHistory[0], "backup-v1-different")
	dest2, cfg2 := openDest(t)
	write(t, filepath.Join(cfg2, "file-history", req2.SessionID, "0123456789abcdef@v1"), "backup-v1")
	res2, err := CommitSession(context.Background(), dest2, req2, defaultPolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(cfg2, "file-history", req2.SessionID, "0123456789abcdef@v1.imported-"+testID8)); got != "backup-v1-different" {
		t.Errorf("imported file-history = %q", got)
	}
	if len(res2.Warnings) != 1 || !strings.Contains(res2.Warnings[0], "file-history/"+req2.SessionID+"/0123456789abcdef@v1") {
		t.Errorf("warnings = %v", res2.Warnings)
	}
}

func TestCommitSessionRefusesExistingTranscript(t *testing.T) {
	req := stageSession(t, t.TempDir())
	dest, cfg := openDest(t)
	write(t, filepath.Join(cfg, "projects", testSlug, testSID+".jsonl"), "existing\n")
	_, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want already exists", err)
	}
	if got := readFile(t, filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")); got != "existing\n" {
		t.Errorf("existing transcript touched: %q", got)
	}
	if exists(filepath.Join(cfg, "projects", testSlug, testSID)) {
		t.Errorf("sidecar written despite refusal")
	}
}

func TestCommitSessionRefusesPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	outside := t.TempDir()
	t.Run("sidecar path", func(t *testing.T) {
		req := stageSession(t, t.TempDir())
		dest, cfg := openDest(t)
		if err := os.MkdirAll(filepath.Join(cfg, "projects", testSlug), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(cfg, "projects", testSlug, testSID)); err != nil {
			t.Fatal(err)
		}
		if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err == nil {
			t.Fatal("expected refusal")
		}
		if got := filesUnder(t, outside); len(got) != 0 {
			t.Errorf("wrote outside the root: %v", got)
		}
		if exists(filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")) {
			t.Errorf("transcript landed despite refusal")
		}
	})
	t.Run("slug dir", func(t *testing.T) {
		req := stageSession(t, t.TempDir())
		dest, cfg := openDest(t)
		if err := os.MkdirAll(filepath.Join(cfg, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(cfg, "projects", testSlug)); err != nil {
			t.Fatal(err)
		}
		if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err == nil {
			t.Fatal("expected refusal")
		}
		if got := filesUnder(t, outside); len(got) != 0 {
			t.Errorf("wrote outside the root: %v", got)
		}
	})
}

func TestCommitSessionValidatesRequest(t *testing.T) {
	dest, _ := openDest(t)
	base := stageSession(t, t.TempDir())
	cases := []struct {
		name string
		mut  func(*CommitRequest)
	}{
		{"bad sid", func(r *CommitRequest) { r.SessionID = "not-a-uuid" }},
		{"reserved slug", func(r *CommitRequest) { r.Slug = "memory" }},
		{"set-aside slug", func(r *CommitRequest) { r.Slug = "x.bffs-replaced-1" }},
		{"slug with separator", func(r *CommitRequest) { r.Slug = "a/b" }},
		{"bad id8", func(r *CommitRequest) { r.ID8 = "XYZ" }},
		{"no transcript", func(r *CommitRequest) { r.Transcript = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mut(&req)
			if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCommitSessionCancelled(t *testing.T) {
	req := stageSession(t, t.TempDir())
	dest, cfg := openDest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CommitSession(ctx, dest, req, defaultPolicy(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := filesUnder(t, cfg); len(got) != 0 {
		t.Errorf("files left after cancelled commit: %v", got)
	}
}

func TestCommitSessionUsesRandomTmpWhenLeftoverExists(t *testing.T) {
	req := stageSession(t, t.TempDir())
	dest, cfg := openDest(t)
	leftoverDir := filepath.Join(cfg, "projects", testSlug, testSID+tmpSuffix)
	write(t, filepath.Join(leftoverDir, "junk"), "x")
	leftoverFile := filepath.Join(cfg, "projects", testSlug, testSID+".jsonl"+tmpSuffix)
	write(t, leftoverFile, "partial")
	if _, err := CommitSession(context.Background(), dest, req, defaultPolicy(), nil); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(leftoverDir, "junk")) || readFile(t, leftoverFile) != "partial" {
		t.Errorf("leftover tmp entries were touched")
	}
	if !exists(filepath.Join(cfg, "projects", testSlug, testSID+".jsonl")) || !exists(filepath.Join(cfg, "projects", testSlug, testSID, "subagents", "agent-1.jsonl")) {
		t.Errorf("session did not land next to the leftovers")
	}
}

func TestImportedNames(t *testing.T) {
	if got := importedPlanName("frolicking-pizza.md", testID8); got != "frolicking-pizza.imported-"+testID8+".md" {
		t.Errorf("plan name = %q", got)
	}
	if got := importedPlanName("x.workshop.md", testID8); got != "x.workshop.imported-"+testID8+".md" {
		t.Errorf("workshop name = %q", got)
	}
	if got := importedFileName("0123456789abcdef@v1", testID8); got != "0123456789abcdef@v1.imported-"+testID8 {
		t.Errorf("file-history name = %q", got)
	}
}

// TestSetAsideNamesAreRecognised pins the suffixes this package writes to
// what transcripts.IsSetAside hides from Claude's listing.
func TestSetAsideNamesAreRecognised(t *testing.T) {
	for _, name := range []string{
		testSID + tmpSuffix,
		testSID + ".jsonl" + tmpSuffix,
		testSID + tmpSuffix + "-" + randSuffix(),
		SetAsideName("memory", fixedNow),
		SetAsideName(testSID+".jsonl", fixedNow),
	} {
		if !transcripts.IsSetAside(name) {
			t.Errorf("IsSetAside(%q) = false", name)
		}
	}
	if got, want := SetAsideName("memory", fixedNow), "memory.bffs-replaced-"+itoa(fixedNow.UnixMilli()); got != want {
		t.Errorf("SetAsideName = %q, want %q", got, want)
	}
}
