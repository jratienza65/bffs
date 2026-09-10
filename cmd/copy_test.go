package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

func TestCopyRequestValidate(t *testing.T) {
	base := copyRequest{From: "home", To: "work", AllProjects: true, OnConflict: "skip", Memory: "merge"}
	cases := []struct {
		name string
		mut  func(*copyRequest)
		want string
	}{
		{"ok", func(*copyRequest) {}, ""},
		{"missing to", func(r *copyRequest) { r.To = "" }, "--from and --to are required"},
		{"same", func(r *copyRequest) { r.To = "home" }, `--from and --to name the same account "home"`},
		{"no selector", func(r *copyRequest) { r.AllProjects = false }, "select what to copy: --all-projects, --project <dir> or --session <id>"},
		{"all + project", func(r *copyRequest) { r.Projects = []string{"/x"} }, "--all-projects cannot be combined with --project or --session"},
		{"bad only", func(r *copyRequest) { r.Only = "all" }, `invalid --only "all": must be "sessions" or "memories"`},
		{"bad conflict", func(r *copyRequest) { r.OnConflict = "fork" }, `invalid --on-conflict "fork": must be "skip" or "overwrite"`},
		{"bad memory", func(r *copyRequest) { r.Memory = "replace" }, `invalid --memory "replace": must be "merge", "skip" or "overwrite"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mut(&r)
			err := r.validate()
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Errorf("err = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderNothingToCopy(t *testing.T) {
	home := fakeHome(t)
	var sb strings.Builder
	renderNothingToCopy(&sb, "aviate", "innomind", filepath.Join(home, "build", "projects", "bffs"))
	out := sb.String()
	project := filepath.Join(home, "build", "projects", "bffs")
	for _, want := range []string{
		`nothing to copy: "aviate" and "innomind" share one projects pool (partial isolation) — the transcripts and the memory`,
		"for " + short(project) + " are already the same files. What differs per account is trust:",
		"    bffs trust sync --from aviate --to innomind --project " + shellWord(project),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestConfirmCount(t *testing.T) {
	c, pr, _, errOut := newSplitCmd("3\n")
	_ = c
	if err := confirmCount(pr, 3); err != nil {
		t.Fatalf("exact count: %v", err)
	}
	c, pr, _, errOut = newSplitCmd("2\n")
	_ = c
	if err := confirmCount(pr, 3); err == nil || err.Error() != "aborted" || !strings.Contains(errOut.String(), "aborted") {
		t.Fatalf("wrong count: err=%v out=%q", err, errOut.String())
	}
}

// copyFixture is one pool with a session of the fixture project plus a
// full-isolation account "work" whose own projects/ root is empty.
type copyFixture struct {
	*catalogFixture
	workRoot string
}

func newCopyFixture(t *testing.T, accs map[string]store.Account) *copyFixture {
	t.Helper()
	f := newCatalogFixture(t, store.Accounts{Accounts: accs})
	cf := &copyFixture{catalogFixture: f}
	if _, ok := accs["work"]; ok {
		cf.workRoot = filepath.Join(sessions.Dir(f.cfgDir, "work"), "projects")
		if err := os.MkdirAll(cf.workRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sessions.Dir(f.cfgDir, "work"), ".claude.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cf
}

func (f *copyFixture) request(from, to string, move bool) copyRequest {
	return copyRequest{
		From: from, To: to,
		Projects:   []string{f.project},
		OnConflict: "skip", Memory: "merge",
		Move: move, Yes: true,
		ClaudeDir: f.claudeDir, Cwd: f.project, Now: catalogNow,
	}
}

// fakeLiveSession registers sid as open in this test process, the way
// Claude records a running session under <claudeDir>/sessions/<pid>.json.
func fakeLiveSession(t *testing.T, claudeDir, sid, cwd string) {
	t.Helper()
	start, err := transcripts.ProcStart(os.Getpid())
	if err != nil {
		t.Skipf("cannot read the process start time here: %v", err)
	}
	dir := filepath.Join(claudeDir, transcripts.RuntimeSessionsSubdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{"pid": os.Getpid(), "sessionId": sid, "cwd": cwd, "procStart": start, "kind": "interactive"}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "self.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunCopyNothingToCopy(t *testing.T) {
	f := newCopyFixture(t, map[string]store.Account{
		"aviate":   {Type: store.TypeOAuth},
		"innomind": {Type: store.TypeOAuth},
	})
	f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "hello", catalogNow.Add(-time.Hour))
	c, pr, out, _ := newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, f.request("aviate", "innomind", false), false); err != nil {
		t.Fatalf("shared pool must be an answer, not an error: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to copy") || !strings.Contains(out.String(), "bffs trust sync --from aviate --to innomind") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestRunCopyPoolToFull(t *testing.T) {
	f := newCopyFixture(t, map[string]store.Account{"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull}})
	src := f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "hello", catalogNow.Add(-time.Hour))
	c, pr, out, errOut := newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, f.request("home", "work", false), false); err != nil {
		t.Fatalf("copy: %v\nstderr: %s", err, errOut.String())
	}
	dst := filepath.Join(f.workRoot, f.slug, testSID1+".jsonl")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("copied transcript: %v\n%s", err, out.String())
	}
	want, _ := os.ReadFile(src)
	if string(got) != string(want) {
		t.Errorf("copied bytes differ")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("a copy must keep the source: %v", err)
	}
	for _, s := range []string{"plan: 1 session, 0 memory dirs", "copied 1 session", "verify:  cd " + rehome.ShellQuote(f.project) + " && BFFS_ACCOUNT=work claude --resume " + testSID1} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}

	// A second copy of the same session is a skip, not an error.
	c, pr, out, _ = newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, f.request("home", "work", false), false); err != nil {
		t.Fatalf("second copy: %v", err)
	}
	if !strings.Contains(out.String(), "skipped 1 session already in the destination") {
		t.Errorf("second copy output:\n%s", out.String())
	}
}

func TestRunCopyMove(t *testing.T) {
	f := newCopyFixture(t, map[string]store.Account{"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull}})
	src := f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "hello", catalogNow.Add(-time.Hour))
	req := f.request("home", "work", true)

	// Dry run: plan only.
	req.DryRun = true
	c, pr, out, _ := newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, req, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dry run: nothing written") {
		t.Errorf("dry run output:\n%s", out.String())
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("dry run touched the source: %v", err)
	}

	// The confirmation asks for a yes; "n" aborts without a write.
	req.DryRun, req.Yes = false, false
	c, pr, out, promptOut := newSplitCmd("n\n")
	if err := runCopy(c, f.cfgDir, pr, req, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(promptOut.String(), "move 1 session and delete the originals after verification? [y/N] ") || !strings.Contains(promptOut.String(), "aborted") {
		t.Errorf("prompt missing:\n%s\n%s", out.String(), promptOut.String())
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("aborted move touched the source: %v", err)
	}

	// Without a terminal and without -y the move is refused.
	c, pr, _, _ = newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, req, false); err == nil || !strings.Contains(err.Error(), "refusing to move") {
		t.Errorf("non-tty: err = %v", err)
	}

	req.Yes = true
	c, pr, out, errOut := newSplitCmd("")
	if err := runCopy(c, f.cfgDir, pr, req, false); err != nil {
		t.Fatalf("move: %v\nstderr: %s", err, errOut.String())
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source must be gone after a verified move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.workRoot, f.slug, testSID1+".jsonl")); err != nil {
		t.Errorf("moved transcript: %v", err)
	}
	if !strings.Contains(out.String(), "verified 1 session (sha256) — originals removed") {
		t.Errorf("receipt:\n%s", out.String())
	}
}

func TestRunCopyMoveHoldsLiveSessions(t *testing.T) {
	f := newCopyFixture(t, map[string]store.Account{"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull}})
	src := f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "hello", catalogNow.Add(-time.Hour))
	fakeLiveSession(t, f.claudeDir, testSID1, f.project)
	c, pr, out, _ := newSplitCmd("")
	err := runCopy(c, f.cfgDir, pr, f.request("home", "work", true), false)
	if err == nil || !strings.Contains(err.Error(), "open in a running claude") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "held (live, skipped): "+testSID1) {
		t.Errorf("held line missing:\n%s", out.String())
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("a held session must stay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.workRoot, f.slug, testSID1+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("nothing may land for a held session: %v", err)
	}
}

func TestRunSessionsRm(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{})
	t.Setenv(store.EnvConfigDir, f.cfgDir)
	prev := sessionsClaudeDir
	sessionsClaudeDir = f.claudeDir
	t.Cleanup(func() { sessionsClaudeDir = prev })

	root := filepath.Join(f.claudeDir, "projects")
	p1 := f.transcript(root, testSID1, "one", catalogNow.Add(-2*time.Hour))
	p2 := f.transcript(root, testSID2, "two", catalogNow.Add(-time.Hour))
	sidecar := filepath.Join(root, f.slug, testSID1, "tool-results")
	if err := os.MkdirAll(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "x.txt"), []byte("out"), 0o600); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(root, f.slug, "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(memory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("# index\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// "n" aborts and removes nothing.
	c, pr, out, _ := newSplitCmd("n\n")
	if err := runSessionsRm(c, []string{testSID1, testSID2}, pr, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "aborted") || !strings.Contains(out.String(), "remove 2 sessions") {
		t.Errorf("abort output:\n%s", out.String())
	}
	for _, p := range []string{p1, p2, filepath.Join(sidecar, "x.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("aborted rm touched %s: %v", p, err)
		}
	}

	// A wrong typed count aborts too.
	c, pr, _, _ = newSplitCmd("y\n1\n")
	if err := runSessionsRm(c, []string{testSID1, testSID2}, pr, false, true); err == nil || err.Error() != "aborted" {
		t.Errorf("wrong count: err = %v", err)
	}
	if _, err := os.Stat(p1); err != nil {
		t.Errorf("wrong count touched the transcript: %v", err)
	}

	// Yes plus the typed count removes both sessions and their sidecars,
	// lists every path first, and leaves the memory directory alone.
	c, pr, out, _ = newSplitCmd("y\n2\n")
	if err := runSessionsRm(c, []string{testSID1, testSID2[:8]}, pr, false, true); err != nil {
		t.Fatalf("rm: %v\n%s", err, out.String())
	}
	for _, want := range []string{short(p1), short(p2), short(filepath.Join(root, f.slug, testSID1)), "removed 2 sessions"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, p := range []string{p1, p2, filepath.Join(root, f.slug, testSID1)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", p, err)
		}
	}
	if _, err := os.Stat(memory); err != nil {
		t.Errorf("memory must never be removed: %v", err)
	}
}

func TestRunSessionsRmRefusesLive(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{})
	t.Setenv(store.EnvConfigDir, f.cfgDir)
	prev := sessionsClaudeDir
	sessionsClaudeDir = f.claudeDir
	t.Cleanup(func() { sessionsClaudeDir = prev })
	p := f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "one", catalogNow.Add(-time.Hour))
	fakeLiveSession(t, f.claudeDir, testSID1, f.project)
	c, pr, _, _ := newSplitCmd("")
	err := runSessionsRm(c, []string{testSID1}, pr, true, false)
	if err == nil || !strings.Contains(err.Error(), "open in a running claude") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("live transcript must stay: %v", err)
	}
}
