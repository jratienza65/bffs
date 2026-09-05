package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	testSID1 = "1e005053-380a-4245-a145-52c2715afa73"
	testSID2 = "b19c4e20-1111-4222-8333-444455556666"
	testSID3 = "c0ffee00-1111-4222-8333-444455556666"

	// osc52 is a clipboard-write escape a hostile title could carry.
	osc52 = "\x1b]52;c;aGVsbG8=\x07"
)

var catalogNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// fakeHome points HOME (and USERPROFILE on Windows) at a temp dir so
// short() renders ~ paths deterministically, and returns it.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// neutralCatalogEnv clears the variables the resolver and Claude read, so
// the tests behave the same inside a bffs-launched claude.
func neutralCatalogEnv(t *testing.T) {
	t.Helper()
	t.Setenv("BFFS_ACCOUNT", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "")
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", "")
	t.Setenv("CLAUDE_COWORK_MEMORY_PATH_OVERRIDE", "")
	t.Chdir(t.TempDir())
}

func sharedRoot(home string) transcripts.Root {
	return transcripts.Root{
		Dir:        filepath.Join(home, ".claude", "projects"),
		ConfigDir:  filepath.Join(home, ".claude"),
		Shared:     true,
		Accounts:   []string{"aviate", "innomind"},
		ClaudeJSON: filepath.Join(home, ".claude.json"),
	}
}

func ownedRoot(home, owner string) transcripts.Root {
	cfg := filepath.Join(home, "bffs", "sessions", owner)
	return transcripts.Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg, Owner: owner, ClaudeJSON: filepath.Join(cfg, ".claude.json")}
}

func testSessions(home string) []transcripts.Session {
	root := sharedRoot(home)
	project := filepath.Join(home, "build", "projects", "bffs")
	slug := "-Users-jonas-build-projects-bffs"
	rec := &imports.Record{BundleID: "6f1e2c0a-0000-4000-8000-000000000001", Kind: imports.KindImport, ImportedAt: catalogNow.Add(-8 * 24 * time.Hour), Account: "innomind",
		Source: imports.Source{Hostname: "mac-a", User: "jonas", Home: "/Users/jonas"}}
	return []transcripts.Session{
		{
			ID: testSID1, Slug: slug, Path: filepath.Join(root.Dir, slug, testSID1+".jsonl"), SidecarDir: filepath.Join(root.Dir, slug, testSID1), Root: root,
			Cwd: project, HeadCwd: project, CwdExists: true,
			GitBranch: "main", Version: "2.1.259", Title: osc52 + "Plan: session export", TitleSource: transcripts.TitleSourceCustom,
			FirstTS: catalogNow.Add(-3 * time.Hour), LastTS: catalogNow.Add(-2 * time.Hour), Size: 42_600_000, Subagents: 2, Live: true,
			Account: "aviate", AttribSource: "launch-log",
		},
		{
			ID: testSID2, Slug: slug, Path: filepath.Join(root.Dir, slug, testSID2+".jsonl"), SidecarDir: filepath.Join(root.Dir, slug, testSID2), Root: root,
			Cwd: "/home/jonas/src/bffs", HeadCwd: "/home/jonas/src/bffs",
			GitBranch: "main", LastTS: catalogNow.Add(-9 * 24 * time.Hour), Size: 400_000,
			Import: &imports.SessionRef{Record: rec, Session: &imports.Session{ID: testSID2, OldCwd: "/home/jonas/src/bffs", Status: imports.StatusPending, GitRemote: "git@github.com:x/bffs.git"}},
		},
	}
}

func TestRenderSessionsTable(t *testing.T) {
	home := fakeHome(t)
	ss := testSessions(home)
	blocks := []sessionBlock{{Root: sharedRoot(home), Project: filepath.Join(home, "build", "projects", "bffs"), DirExists: true, Sessions: ss}}
	var sb strings.Builder
	if err := renderSessionsTable(&sb, blocks, 30*24*time.Hour, catalogNow); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"project " + filepath.Join("~", "build", "projects", "bffs") + "  (shared pool: aviate, innomind)",
		"ID", "TITLE", "ACCOUNT", "LAST", "SIZE", "BRANCH", "STATE",
		"1e005053", "Plan: session export", "aviate", "2h ago", "42.6 MB", "main", "live",
		"b19c4e20", "9d ago", "0.4 MB", "imported·pending",
		"2 sessions. " + sessionsFooter,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "52;c;") {
		t.Errorf("escape sequence reached the table:\n%q", out)
	}
	// The imported row has no title and no account: both render as "-".
	line := lineContaining(out, "b19c4e20")
	if !strings.Contains(line, "  -  ") {
		t.Errorf("empty cells not dashed: %q", line)
	}
}

func TestRenderSessionsTableEmpty(t *testing.T) {
	home := fakeHome(t)
	project := filepath.Join(home, "x")
	cases := []struct {
		name   string
		block  sessionBlock
		since  time.Duration
		want   string
		absent string
	}{
		{"dir missing", sessionBlock{Project: project}, 30 * 24 * time.Hour, "no sessions for " + filepath.Join("~", "x"), "modified"},
		{"dir empty in window", sessionBlock{Project: project, DirExists: true}, 30 * 24 * time.Hour, "modified in the last 30d (--since 0 lists all)", ""},
		{"no bound", sessionBlock{Project: project, DirExists: true}, 0, "no sessions for " + filepath.Join("~", "x"), "modified"},
		{"every project", sessionBlock{}, 0, "no sessions\n", "for"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := renderSessionsTable(&sb, []sessionBlock{tc.block}, tc.since, catalogNow); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sb.String(), tc.want) {
				t.Errorf("want %q in %q", tc.want, sb.String())
			}
			if tc.absent != "" && strings.Contains(sb.String(), tc.absent) {
				t.Errorf("did not want %q in %q", tc.absent, sb.String())
			}
		})
	}
}

// Listing every project groups the rows per projects/<slug> directory,
// each block headed by the sessions' directory.
func TestRenderSessionsTableGroupsProjects(t *testing.T) {
	home := fakeHome(t)
	ss := testSessions(home)
	ss[1].Slug = "-home-jonas-src-bffs"
	ss = append(ss, transcripts.Session{ID: testSID3, Slug: "-home-jonas-src-bffs", Root: sharedRoot(home), LastTS: catalogNow.Add(-time.Hour), Size: 10})
	blocks := []sessionBlock{{Root: ownedRoot(home, "work"), Sessions: ss}}
	var sb strings.Builder
	if err := renderSessionsTable(&sb, blocks, 0, catalogNow); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if n := strings.Count(out, "project "); n != 2 {
		t.Errorf("want 2 project headers, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "project "+filepath.Join("~", "build", "projects", "bffs")+"  (account: work)") {
		t.Errorf("first group header missing:\n%s", out)
	}
	if !strings.Contains(out, "project /home/jonas/src/bffs  (account: work)") {
		t.Errorf("second group header should use the newest session's cwd:\n%s", out)
	}
	if !strings.Contains(out, "3 sessions.") {
		t.Errorf("total missing:\n%s", out)
	}
}

func TestRootLabel(t *testing.T) {
	home := fakeHome(t)
	cases := []struct {
		root transcripts.Root
		want string
	}{
		{sharedRoot(home), "shared pool: aviate, innomind"},
		{ownedRoot(home, "work"), "account: work"},
		{transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Owner: "gone", Orphan: true}, "orphan: gone, read-only"},
		{transcripts.Root{Dir: filepath.Join(home, ".claude", "projects"), ConfigDir: filepath.Join(home, ".claude")}, "home: " + filepath.Join("~", ".claude")},
	}
	for _, tc := range cases {
		if got := rootLabel(tc.root); got != tc.want {
			t.Errorf("rootLabel = %q, want %q", got, tc.want)
		}
	}
}

func TestSessionInfoJSONShape(t *testing.T) {
	home := fakeHome(t)
	ss := testSessions(home)
	want := []string{"session_id", "root", "account", "account_source", "cwd", "cwd_exists", "slug", "title", "title_source",
		"git_branch", "claude_version", "first_at", "last_at", "size_bytes", "subagents", "live", "bundle_id", "old_cwd", "old_home", "old_host", "git_remote"}
	for _, s := range ss {
		raw, err := json.Marshal(sessionInfoOf(s))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if len(m) != len(want) {
			t.Errorf("want %d keys, got %d: %s", len(want), len(m), raw)
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("key %q missing from %s", k, raw)
			}
		}
	}
	info := sessionInfoOf(ss[0])
	if info.Title != "Plan: session export" || info.FirstAt != "2026-08-24T09:00:00Z" || info.LastAt != "2026-08-24T10:00:00Z" || info.Root != sharedRoot(home).Dir {
		t.Errorf("info = %+v", info)
	}
	hostile := ss[0]
	hostile.Cwd = osc52 + hostile.Cwd
	if got := sessionInfoOf(hostile).Cwd; got != ss[0].Cwd {
		t.Errorf("cwd not sanitised in JSON: %q", got)
	}
	imp := sessionInfoOf(ss[1])
	if imp.BundleID != "6f1e2c0a-0000-4000-8000-000000000001" || imp.OldCwd != "/home/jonas/src/bffs" || imp.OldHost != "mac-a" || imp.OldHome != "/Users/jonas" || imp.GitRemote != "git@github.com:x/bffs.git" || imp.FirstAt != "" {
		t.Errorf("imported info = %+v", imp)
	}

	var sb strings.Builder
	if err := writeSessionsJSON(&sb, nil); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(sb.String()) != "[]" {
		t.Errorf("empty listing must be an empty array, got %q", sb.String())
	}
	sb.Reset()
	if err := writeSessionsJSON(&sb, []sessionBlock{{Sessions: ss}}); err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(sb.String()), &arr); err != nil || len(arr) != 2 {
		t.Errorf("array of 2 expected, got %v: %s", err, sb.String())
	}
}

func TestRenderSessionShow(t *testing.T) {
	home := fakeHome(t)
	ss := testSessions(home)
	project := filepath.Join(home, "build", "projects", "bffs")
	d := sessionDetail{
		Session:   ss[0],
		Artifacts: transcripts.ArtifactsFor(ss[0].Root, ss[0]),
		LivePID:   38445, SidecarSize: 1_200_000, SidecarExists: true,
		Claimants: []string{"aviate"},
	}
	d.Artifacts.PlanFiles = []string{filepath.Join(home, ".claude", "plans", "session-export.md")}
	var sb strings.Builder
	renderSessionShow(&sb, d, catalogNow)
	out := sb.String()
	for _, want := range []string{
		"session:      " + testSID1,
		"title:        Plan: session export  (custom)",
		"root:         " + filepath.Join("~", ".claude", "projects") + "  (shared pool: aviate, innomind)",
		"cwd:          " + filepath.Join("~", "build", "projects", "bffs") + "  (exists)",
		"account:      aviate  (launch-log)",
		"state:        live (pid 38445)",
		"last-session pointer: aviate (claude-recorded)",
		"size:         42.6 MB",
		"branch:       main",
		"version:      2.1.259",
		"transcript:   " + filepath.Join("~", ".claude", "projects", "-Users-jonas-build-projects-bffs", testSID1+".jsonl"),
		"(1.2 MB, 2 subagents)",
		"file-history: " + filepath.Join("~", ".claude", "file-history", testSID1) + "  (absent)",
		"plan:         " + filepath.Join("~", ".claude", "plans", "session-export.md"),
		"resume:       cd " + shellWord(project) + " && claude --resume " + testSID1,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence reached the screen:\n%q", out)
	}
	if strings.Contains(out, "head cwd") || strings.Contains(out, "import:") {
		t.Errorf("unexpected lines for a plain session:\n%s", out)
	}

	// An imported, pending session on a full-isolation root: import block,
	// missing cwd, BFFS_ACCOUNT prefix on the resume line.
	imp := ss[1]
	imp.Root = ownedRoot(home, "work")
	imp.Path, imp.SidecarDir = "", "" // computed from the root by ArtifactsFor
	imp.Relocated, imp.HeadCwd = true, "/Users/jonas/src/bffs"
	sb.Reset()
	renderSessionShow(&sb, sessionDetail{Session: imp, Artifacts: transcripts.ArtifactsFor(imp.Root, imp)}, catalogNow)
	out = sb.String()
	for _, want := range []string{
		"title:        -",
		"cwd:          /home/jonas/src/bffs  (missing on this machine)",
		"head cwd:     /Users/jonas/src/bffs  (relocated since)",
		"account:      -  (unknown",
		"state:        imported·pending",
		"import:       bundle 6f1e2c0a-0000-4000-8000-000000000001  (import 2026-08-16) from mac-a",
		"from mac-a (jonas, /Users/jonas), account innomind, status pending",
		"old cwd:      /home/jonas/src/bffs",
		"sidecar:      " + filepath.Join("~", "bffs", "sessions", "work", "projects", "-Users-jonas-build-projects-bffs", testSID2) + "  (absent)",
		"plan:         -",
		"resume:       BFFS_ACCOUNT=work cd /home/jonas/src/bffs && claude --resume " + testSID2,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("imported show missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "last-session pointer") {
		t.Errorf("pointer line without claimants:\n%s", out)
	}

	// A hostile cwd (transcript-derived) is sanitised everywhere it is
	// rendered, the resume line included.
	evil := ss[0]
	evil.Cwd, evil.HeadCwd, evil.GitBranch, evil.PlanSlug = osc52+"/tmp/x", osc52+"/tmp/y", osc52+"main", osc52+"pizza"
	evil.Relocated = true
	sb.Reset()
	renderSessionShow(&sb, sessionDetail{Session: evil, Artifacts: transcripts.ArtifactsFor(evil.Root, evil)}, catalogNow)
	out = sb.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence reached the screen:\n%q", out)
	}
	if !strings.Contains(out, "resume:       cd /tmp/x && claude --resume "+testSID1) {
		t.Errorf("resume line not sanitised:\n%s", out)
	}
}

func TestResumeLine(t *testing.T) {
	cases := []struct {
		name string
		s    transcripts.Session
		want string
	}{
		{"shared", transcripts.Session{ID: testSID1, Cwd: "/a/b", Root: transcripts.Root{Shared: true}}, "cd /a/b && claude --resume " + testSID1},
		{"owned", transcripts.Session{ID: testSID1, Cwd: "/a/b", Root: transcripts.Root{Owner: "work"}}, "BFFS_ACCOUNT=work cd /a/b && claude --resume " + testSID1},
		{"orphan", transcripts.Session{ID: testSID1, Cwd: "/a/b", Root: transcripts.Root{Owner: "gone", Orphan: true, ConfigDir: "/cfg/sessions/gone"}}, "CLAUDE_CONFIG_DIR=/cfg/sessions/gone cd /a/b && claude --resume " + testSID1},
		{"quoted cwd", transcripts.Session{ID: testSID1, Cwd: "/a/my project's"}, `cd '/a/my project'\''s' && claude --resume ` + testSID1},
		{"no cwd", transcripts.Session{ID: testSID1}, "claude --resume " + testSID1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeLine(tc.s); got != tc.want {
				t.Errorf("resumeLine = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"0", 0, false},
		{"", 0, false},
		{"all", 0, false},
		{"-1d", 0, true},
		{"-5h", 0, true},
		{"soon", 0, true},
		{"1.5d", 0, true},
	}
	for _, tc := range cases {
		got, err := parseSince(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseSince(%q): want error, got %v", tc.in, got)
			} else if !strings.Contains(err.Error(), fmt.Sprintf("invalid --since %q", tc.in)) {
				t.Errorf("parseSince(%q): error %v", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if got := formatSince(30 * 24 * time.Hour); got != "30d" {
		t.Errorf("formatSince(30d) = %q", got)
	}
	if got := formatSince(12 * time.Hour); got != "12h0m0s" {
		t.Errorf("formatSince(12h) = %q", got)
	}
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 KB"}, {1_234, "1 KB"}, {99_499, "99 KB"}, {400_000, "0.4 MB"}, {42_600_000, "42.6 MB"}, {1_300_000_000, "1.3 GB"},
	}
	for _, tc := range cases {
		if got := formatSize(tc.n); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestTruncateCell(t *testing.T) {
	if got := truncateCell("short", 40); got != "short" {
		t.Errorf("short: %q", got)
	}
	long := strings.Repeat("ab ", 20)
	got := truncateCell(long, 40)
	if len([]rune(got)) > 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("long: %q (%d runes)", got, len([]rune(got)))
	}
	if got := truncateCell("ééééé", 3); got != "éé…" {
		t.Errorf("runes: %q", got)
	}
}

func TestCountNoun(t *testing.T) {
	if got := countNoun(1, "session"); got != "1 session" {
		t.Errorf("%q", got)
	}
	if got := countNoun(2, "session"); got != "2 sessions" {
		t.Errorf("%q", got)
	}
}

// catalogFixture is a bffs config dir plus a fake ~/.claude with one
// project directory and transcripts in it.
type catalogFixture struct {
	t         *testing.T
	cfgDir    string
	claudeDir string
	project   string // an existing directory, normalised
	slug      string
}

func newCatalogFixture(t *testing.T, accs store.Accounts) *catalogFixture {
	t.Helper()
	home := fakeHome(t)
	neutralCatalogEnv(t)
	f := &catalogFixture{t: t, cfgDir: filepath.Join(home, "bffs"), claudeDir: filepath.Join(home, ".claude")}
	if err := store.SaveAccounts(f.cfgDir, accs); err != nil {
		t.Fatal(err)
	}
	// Create before normalising: NormalizePath resolves symlinks only for
	// paths that exist (macOS: /var → /private/var), and ProjectKey will.
	if err := os.MkdirAll(filepath.Join(home, "build", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	project, err := store.NormalizePath(filepath.Join(home, "build", "proj"))
	if err != nil {
		t.Fatal(err)
	}
	f.project = project
	if f.slug, err = transcripts.Slug(project); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.claudeDir, "projects", f.slug), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// transcript writes a minimal transcript whose head records cwd and a
// first prompt, under the fixture's project slug, with the given mtime.
func (f *catalogFixture) transcript(root, sid, prompt string, mtime time.Time) string {
	f.t.Helper()
	path := filepath.Join(root, f.slug, sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"version":"2.1.259","gitBranch":"main","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
		f.project, sid, mtime.Add(-time.Minute).Format(time.RFC3339), prompt)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *catalogFixture) env() *catalogEnv {
	f.t.Helper()
	env, err := loadCatalogEnv(f.cfgDir, f.claudeDir)
	if err != nil {
		f.t.Fatal(err)
	}
	return env
}

func TestCatalogEnvRoots(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth},
		"api":  {Type: store.TypeAPIKey, Secret: "sk-ant-test-1234"},
	}})
	// An orphan session dir with a real projects/ shows up read-only.
	if err := os.MkdirAll(filepath.Join(f.cfgDir, "sessions", "gone", "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := f.env()

	home, err := env.homeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if home.Dir != filepath.Join(f.claudeDir, "projects") || !home.Shared || len(home.Accounts) != 1 || home.Accounts[0] != "work" {
		t.Errorf("home root = %+v", home)
	}
	if r, err := env.rootForAccount("api"); err != nil || r.Dir != home.Dir {
		t.Errorf("api_key account should map to the home root: %+v, %v", r, err)
	}
	if r, err := env.rootForAccount("work"); err != nil || r.Dir != home.Dir {
		t.Errorf("partial account should map to the shared root: %+v, %v", r, err)
	}
	if r, err := env.rootForAccount("gone"); err != nil || !r.Orphan {
		t.Errorf("orphan by name: %+v, %v", r, err)
	}
	if _, err := env.rootForAccount("ghost"); err == nil || err.Error() != `unknown account "ghost"; known: [api gone home work]` {
		t.Errorf("unknown account error = %v", err)
	}

	root, warning, err := env.defaultRoot(f.project)
	if err != nil || warning != "" || root.Dir != home.Dir {
		t.Errorf("defaultRoot with no active account = %+v, %q, %v", root, warning, err)
	}
	t.Setenv("BFFS_ACCOUNT", "ghost")
	root, warning, err = env.defaultRoot(f.project)
	if err != nil || root.Dir != home.Dir || !strings.Contains(warning, "ghost") || !strings.Contains(warning, "using the home root") {
		t.Errorf("defaultRoot with a bad pin = %+v, %q, %v", root, warning, err)
	}

	roots, warnings, err := env.selectRoots(true, "", f.project)
	if err != nil || len(roots) != 2 {
		t.Fatalf("selectRoots(all) = %d roots, %v", len(roots), err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "orphan session dir") || !strings.Contains(warnings[0], "read-only") {
		t.Errorf("orphan warning = %v", warnings)
	}
	if dirs := env.configDirs(); len(dirs) != 2 || dirs[0] != f.claudeDir {
		t.Errorf("configDirs = %v", dirs)
	}
}

func TestListSessionBlocks(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}})
	root := filepath.Join(f.claudeDir, "projects")
	f.transcript(root, testSID1, "first prompt of one", catalogNow.Add(-time.Hour))
	f.transcript(root, testSID2, "first prompt of two", catalogNow.Add(-40*24*time.Hour))
	// A transcript of another project must not appear under the project filter.
	other := filepath.Join(root, "-other-proj", testSID3+".jsonl")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"type":"user","cwd":"/other/proj","timestamp":"2026-08-24T11:00:00Z","message":{"role":"user","content":"x"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := f.env()
	home, _ := env.homeRoot()

	q := sessionsQuery{Roots: []transcripts.Root{home}, Project: f.project, Since: 30 * 24 * time.Hour, Limit: 50, Now: catalogNow}
	blocks, err := listSessionBlocks(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || !blocks[0].DirExists || blocks[0].ProjectDir != filepath.Join(root, f.slug) {
		t.Fatalf("blocks = %+v", blocks)
	}
	ss := blocks[0].Sessions
	if len(ss) != 1 || ss[0].ID != testSID1 {
		t.Fatalf("want only the in-window session of the project, got %+v", ss)
	}
	if ss[0].Title != "first prompt of one" || ss[0].TitleSource != transcripts.TitleSourceFirstPrompt || ss[0].Cwd != f.project || !ss[0].CwdExists || ss[0].GitBranch != "main" {
		t.Errorf("titles not read: %+v", ss[0])
	}

	// No bound, every project: three sessions, newest first.
	q.Since, q.Project = 0, ""
	blocks, err = listSessionBlocks(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].Sessions; len(got) != 3 || got[0].ID != testSID3 || got[1].ID != testSID1 || got[2].ID != testSID2 {
		t.Errorf("all projects: %v", ids(got))
	}

	// --live keeps only the sessions the live map names.
	q.Live, q.LiveMap = true, map[string]transcripts.LiveSession{testSID2: {PID: 1, SessionID: testSID2}}
	blocks, err = listSessionBlocks(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].Sessions; len(got) != 1 || got[0].ID != testSID2 || !got[0].Live {
		t.Errorf("live filter: %v", ids(got))
	}
	q.LiveMap = nil
	blocks, err = listSessionBlocks(t.Context(), q)
	if err != nil || len(blocks[0].Sessions) != 0 {
		t.Errorf("live filter with nothing live: %v, %v", blocks[0].Sessions, err)
	}

	// A project that has no directory yet lists as an empty block.
	q.Live, q.Project = false, filepath.Join(f.project, "sub")
	if err := os.Mkdir(q.Project, 0o755); err != nil {
		t.Fatal(err)
	}
	blocks, err = listSessionBlocks(t.Context(), q)
	if err != nil || len(blocks) != 1 || blocks[0].DirExists || len(blocks[0].Sessions) != 0 {
		t.Errorf("missing project dir: %+v, %v", blocks, err)
	}
}

func ids(ss []transcripts.Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func lineContaining(out, needle string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, needle) {
			return l
		}
	}
	return ""
}
