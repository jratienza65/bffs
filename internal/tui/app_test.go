package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	sid1 = "1e005053-380a-4245-a145-52c2715afa73"
	sid2 = "b19c4e20-1111-4222-8333-444455556666"
	sid3 = "c0ffee00-1111-4222-8333-444455556666"

	// osc52 is a clipboard-write escape a hostile title could carry.
	osc52 = "\x1b]52;c;aGVsbG8=\x07"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// fixture is a bffs config dir plus a fake ~/.claude with one project
// directory — the synthetic pool of cmd/sessions_test.go, no symlinks.
type fixture struct {
	t         *testing.T
	home      string
	cfgDir    string
	claudeDir string
	project   string // an existing directory, normalised
	slug      string
	accounts  store.Accounts
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home, err := store.NormalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"BFFS_ACCOUNT", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PROJECT_DIR_NAME", "CLAUDE_CODE_REMOTE_MEMORY_DIR", "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE", "TERM"} {
		t.Setenv(k, "")
	}
	f := &fixture{t: t, home: home, cfgDir: filepath.Join(home, "bffs"), claudeDir: filepath.Join(home, ".claude")}
	f.accounts = store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}}
	f.project = filepath.Join(home, "build", "proj")
	if err := os.MkdirAll(f.project, 0o755); err != nil {
		t.Fatal(err)
	}
	if f.slug, err = transcripts.Slug(f.project); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.claudeDir, "projects", f.slug), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.project)
	return f
}

func (f *fixture) saveAccounts() {
	f.t.Helper()
	if err := store.SaveAccounts(f.cfgDir, f.accounts); err != nil {
		f.t.Fatal(err)
	}
}

// transcript writes a minimal transcript under slug whose head records
// cwd and a first prompt, followed by extra records, with the given mtime.
func (f *fixture) transcript(slug, sid, cwd, prompt string, mtime time.Time, extra ...map[string]any) string {
	f.t.Helper()
	path := filepath.Join(f.claudeDir, "projects", slug, sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"version":"2.1.259","gitBranch":"main","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
		cwd, sid, mtime.Add(-time.Minute).Format(time.RFC3339), prompt))
	for _, rec := range extra {
		b, err := json.Marshal(rec)
		if err != nil {
			f.t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// live plants the superseded sibling Claude leaves next to a transcript
// it has open; its fresh mtime marks the session live.
func (f *fixture) live(sid string) {
	f.t.Helper()
	p := filepath.Join(f.claudeDir, "projects", f.slug, sid+".jsonl.superseded-1")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// memory writes the project's auto-memory directory: an index with an
// @-reference and a pinned topic file with two absolute paths.
func (f *fixture) memory() string {
	f.t.Helper()
	dir := filepath.Join(f.claudeDir, "projects", f.slug, transcripts.MemorySubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	files := map[string]string{
		transcripts.MemoryIndexFile: "# Memory\n- [Notes](notes.md) — see @~/notes.md\n",
		"notes.md":                  "---\npinned: true\n---\nPaths: /Users/jonas/x and /Users/jonas/y\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

// lastSession records sid as the project's lastSessionId in an oauth
// account's own .claude.json — attribution tier 2.
func (f *fixture) lastSession(account, cwd, sid string) {
	f.t.Helper()
	dir := filepath.Join(f.cfgDir, "sessions", account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	doc := map[string]any{"projects": map[string]any{cwd: map[string]any{"lastSessionId": sid}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), b, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// importRecord marks sid as imported from another machine, pending its
// directory.
func (f *fixture) importRecord(sid, oldCwd string) {
	f.t.Helper()
	rec := imports.Record{
		BundleID: "6f1e2c0a-0000-4000-8000-000000000001", Kind: imports.KindImport,
		ImportedAt: fixedNow.Add(-8 * 24 * time.Hour), Account: "work",
		Source:   imports.Source{Hostname: "mac-a", User: "jonas", Home: "/Users/jonas"},
		Sessions: []imports.Session{{ID: sid, OldCwd: oldCwd, Status: imports.StatusPending}},
	}
	if err := imports.Save(f.cfgDir, rec); err != nil {
		f.t.Fatal(err)
	}
}

// harness drives the root model the way the runtime would, minus the
// terminal: every command a message produces is run synchronously and
// its result fed back, until nothing is left. tea.NewProgram is never
// started.
type harness struct {
	t    *testing.T
	a    *app
	quit bool
}

func (f *fixture) start(start string) *harness {
	f.t.Helper()
	f.saveAccounts()
	svc, err := loadServices(context.Background(), Options{CfgDir: f.cfgDir, HomeClaudeDir: f.claudeDir, Version: "test", Start: start})
	if err != nil {
		f.t.Fatal(err)
	}
	svc.now = func() time.Time { return fixedNow }
	h := &harness{t: f.t, a: newApp(svc)}
	h.run(h.a.Init())
	h.send(tea.WindowSizeMsg{Width: 300, Height: 40})
	return h
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.send(cmd())
}

func (h *harness) send(msgs ...tea.Msg) {
	for _, msg := range msgs {
		switch m := msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range m {
				h.run(c)
			}
		case tea.QuitMsg:
			h.quit = true
		default:
			_, cmd := h.a.Update(msg)
			h.run(cmd)
		}
	}
}

func (h *harness) keys(ks ...string) {
	for _, k := range ks {
		h.send(keyPress(k))
	}
}

func (h *harness) view() string { return h.a.View().Content }

// keyPress builds the message the terminal would send for a key name.
func keyPress(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("view missing %q:\n%s", w, out)
		}
	}
}

func wantNone(t *testing.T, out string, absent ...string) {
	t.Helper()
	for _, w := range absent {
		if strings.Contains(out, w) {
			t.Errorf("view must not contain %q:\n%s", w, out)
		}
	}
}

// cursorLine is the line the list cursor is on.
func cursorLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "> ") {
			return l
		}
	}
	return ""
}

func TestSupportedFalseWithoutTTY(t *testing.T) {
	// go test hands the binary a pipe for stdout, never a terminal.
	if Supported() {
		t.Fatal("Supported() must be false without a terminal on stdout")
	}
	t.Setenv("TERM", "dumb")
	if Supported() {
		t.Fatal("Supported() must be false with TERM=dumb")
	}
}

func TestSingleRootSkipsRootsScreen(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*24*time.Hour))
	f.transcript("-home-nobody-src-x", sid3, "/home/nobody/src/x", "elsewhere", fixedNow.Add(-5*24*time.Hour))
	f.memory()
	h := f.start("sessions")

	if _, ok := h.a.top().(*projectsScreen); !ok || len(h.a.stack) != 1 {
		t.Fatalf("one root should open the projects screen directly, stack = %T x%d", h.a.top(), len(h.a.stack))
	}
	out := h.view()
	wantAll(t, out, "bffs test", "shared pool: work", "PROJECT", "SESSIONS", "MEMORY", "NEWEST",
		shortPath(f.project), "2 sessions", "1h ago", "! /home/nobody/src/x", "1 session", "5d ago")
	wantNone(t, out, "roots", "\x1b]")
	// The cursor starts on the process's own project.
	if line := cursorLine(out); !strings.Contains(line, shortPath(f.project)) {
		t.Errorf("cursor not on the current project: %q", line)
	}
	// p is bound only where there is a memory dir to scan; here it is a
	// reserved slot like the rest.
	h.keys("p")
	wantAll(t, h.view(), reservedHint)
	if _, ok := h.a.top().(*projectsScreen); !ok {
		t.Errorf("p on projects must stay put, got %T", h.a.top())
	}
	// esc at the bottom of the stack does not pop.
	h.keys("esc")
	if len(h.a.stack) != 1 {
		t.Errorf("esc popped the last screen")
	}
	wantAll(t, h.view(), "q quits")
}

func TestRootsScreenWithSeveralRoots(t *testing.T) {
	f := newFixture(t)
	f.accounts.Accounts["full"] = store.Account{Type: store.TypeOAuth, Isolation: store.IsolationFull}
	for _, name := range []string{"full", "gone"} {
		if err := os.MkdirAll(filepath.Join(f.cfgDir, "sessions", name, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("sessions")

	if _, ok := h.a.top().(*rootsScreen); !ok {
		t.Fatalf("several roots should open the roots screen, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "ROOT",
		"shared pool ("+shortPath(f.claudeDir)+") — accounts: work",
		"full [full isolation]",
		"orphan: gone (read-only)",
		"warning: orphan session dir")
	// Open the second root, then come back.
	h.keys("down", "enter")
	if s, ok := h.a.top().(*projectsScreen); !ok || s.root.Owner != "full" {
		t.Fatalf("enter should open the full root's projects, got %T", h.a.top())
	}
	wantAll(t, h.view(), "roots › account: full", "No projects")
	h.keys("esc")
	if _, ok := h.a.top().(*rootsScreen); !ok {
		t.Errorf("esc should pop to the roots screen, got %T", h.a.top())
	}
}

func TestSessionsScreen(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour),
		map[string]any{"type": "custom-title", "sessionId": sid1, "customTitle": osc52 + "Plan: session export"})
	f.live(sid1)
	f.transcript(f.slug, sid2, "/home/nobody/src/x", "prompt two", fixedNow.Add(-9*24*time.Hour))
	f.importRecord(sid2, "/home/nobody/src/x")
	f.transcript(f.slug, sid3, f.project, "first prompt of three", fixedNow.Add(-3*time.Hour))
	f.lastSession("work", f.project, sid3)
	f.memory()
	h := f.start("sessions")
	h.keys("enter")

	s, ok := h.a.top().(*sessionsScreen)
	if !ok {
		t.Fatalf("enter on a project should open its sessions, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, shortPath(f.project)+" › sessions", "project "+shortPath(f.project)+"  (shared pool: work)",
		"TITLE", "ACCOUNT", "LAST", "SIZE", "BRANCH", "STATE",
		"Plan: session export", "1h ago", "live",
		"first prompt of three", "work", "3h ago",
		"prompt two", "9d ago", "imported·pending",
		"main", "3 sessions")
	wantNone(t, out, "\x1b]", "52;c;", "(1e005053)")
	if line := cursorLine(out); !strings.Contains(line, "Plan: session export") {
		t.Errorf("cursor should start on the newest session: %q", line)
	}

	// Multi-select: space toggles and moves down, a selects every visible
	// row, a again clears.
	h.keys("space")
	wantAll(t, h.view(), "[x] Plan: session export", "1 selected")
	h.keys("a")
	wantAll(t, h.view(), "3 selected")
	h.keys("a")
	wantNone(t, h.view(), "selected", "[x]")

	// Reserved action keys answer with the hint; p opens scan paths.
	h.keys("e")
	wantAll(t, h.view(), reservedHint)
	h.keys("R")
	wantAll(t, h.view(), reservedHint)
	h.keys("p")
	if _, ok := h.a.top().(*scanPathsScreen); !ok {
		t.Fatalf("p should open scan paths, got %T", h.a.top())
	}
	wantAll(t, h.view(), "scan paths", "notes.md:4: /Users/jonas/x", "@ref MEMORY.md:2: @~/notes.md", "3 references")
	h.keys("esc")
	if h.a.top() != s {
		t.Fatalf("esc should pop back to the sessions screen, got %T", h.a.top())
	}

	// Filter: only the matching row stays; esc clears the filter first,
	// then goes back.
	h.keys("/", "t", "h", "r", "e", "e", "enter")
	out = h.view()
	wantAll(t, out, "first prompt of three", "“three”", "2 filtered")
	wantNone(t, out, "Plan: session export", "prompt two")
	h.keys("esc")
	wantAll(t, h.view(), "Plan: session export", "prompt two", "3 sessions")
	if h.a.top() != s {
		t.Fatalf("esc with a filter applied must clear it, not pop; top = %T", h.a.top())
	}

	// Help toggles the full view, with the reserved slots named.
	h.keys("?")
	wantAll(t, h.view(), "actions", "select all", "sessions⇄memories")
	h.keys("?")

	// enter opens the read-only detail of the cursor's session.
	h.keys("enter")
	if _, ok := h.a.top().(*showScreen); !ok {
		t.Fatalf("enter should open the show screen, got %T", h.a.top())
	}
	out = h.view()
	wantAll(t, out, "session "+sid1[:8],
		"session:      "+sid1,
		"title:        Plan: session export  (custom)",
		"root:         "+shortPath(filepath.Join(f.claudeDir, "projects"))+"  (shared pool: work)",
		"cwd:          "+shortPath(f.project)+"  (exists)",
		"state:        live",
		"branch:       main",
		"version:      2.1.259",
		"transcript:   "+shortPath(filepath.Join(f.claudeDir, "projects", f.slug, sid1+".jsonl")),
		"resume:       cd "+shellWord(f.project)+" && claude --resume "+sid1)
	wantNone(t, out, "\x1b]", "52;c;")
	h.keys("esc")

	// The imported session's detail carries the record.
	h.keys("down", "down", "enter")
	out = h.view()
	wantAll(t, out, "session:      "+sid2, "cwd:          /home/nobody/src/x  (missing on this machine)",
		"state:        imported·pending", "import:       bundle 6f1e2c0a-0000-4000-8000-000000000001", "from mac-a (jonas, /Users/jonas), account work, status pending",
		"old cwd:      /home/nobody/src/x")
	h.keys("esc", "esc")
	if _, ok := h.a.top().(*projectsScreen); !ok {
		t.Fatalf("esc should pop to the projects screen, got %T", h.a.top())
	}

	// q quits; the view goes blank.
	h.keys("q")
	if !h.quit || h.view() != "" {
		t.Errorf("q should quit: quit=%v view=%q", h.quit, h.view())
	}
}

// Titles are read after the listing: the page shows ids until the
// windows arrive, then the titles, cached by (path, mtime, size).
func TestSessionsLazyTitles(t *testing.T) {
	f := newFixture(t)
	f.saveAccounts()
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*time.Hour))
	svc, err := loadServices(context.Background(), Options{CfgDir: f.cfgDir, HomeClaudeDir: f.claudeDir})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return fixedNow }
	root, err := transcripts.RootFor(svc.roots, transcripts.HomeName)
	if err != nil {
		t.Fatal(err)
	}
	s := newSessionsScreen(svc, root, f.slug, f.project)
	_, _ = s.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	if !s.loading() {
		t.Fatal("screen should be loading before its page arrives")
	}
	page := s.Init()()
	if m, ok := page.(sessionsPageMsg); !ok || len(m.items) != 2 || m.items[0].Title != "" {
		t.Fatalf("page = %#v", page)
	}
	_, cmd := s.Update(page)
	out := s.View(120, 20)
	wantAll(t, out, "(1e005053)", "(b19c4e20)", "1h ago", "2h ago")
	wantNone(t, out, "first prompt")
	if cmd == nil {
		t.Fatal("the page should request the visible titles")
	}
	msg := cmd()
	if m, ok := msg.(titlesResolvedMsg); !ok || len(m.metas) != 2 {
		t.Fatalf("titles = %#v", msg)
	}
	_, cmd = s.Update(msg)
	if cmd != nil {
		if m, ok := cmd().(titlesResolvedMsg); ok && len(m.metas) > 0 {
			t.Errorf("titles requested twice: %v", m.metas)
		}
	}
	out = s.View(120, 20)
	wantAll(t, out, "first prompt of one", "first prompt of two")
	wantNone(t, out, "(1e005053)")
	if len(svc.titles) != 2 {
		t.Errorf("title cache holds %d entries, want 2", len(svc.titles))
	}
	// A fresh screen over the same pool serves the titles from the cache
	// without a read command.
	again := newSessionsScreen(svc, root, f.slug, f.project)
	_, _ = again.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	_, cmd = again.Update(again.Init()())
	if cmd != nil {
		if m, ok := cmd().(titlesResolvedMsg); ok && len(m.metas) > 0 {
			t.Errorf("cached titles were read again: %v", m.metas)
		}
	}
	wantAll(t, again.View(120, 20), "first prompt of one", "first prompt of two")
}

func TestMemoriesTab(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	dir := f.memory()
	h := f.start("memories")
	h.keys("enter")

	if _, ok := h.a.top().(*memoriesScreen); !ok {
		t.Fatalf("bffs memory opens a project on the memories tab, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, shortPath(f.project)+" › memories", "memory "+shortPath(dir)+"  (shared pool — visible to: work)",
		"NAME", "SIZE", "MODIFIED", "PINNED", "PATHS", "@REFS",
		"MEMORY.md", "@refs 1", "notes.md", "pinned", "paths 2", "2 files")

	// tab flips to sessions and back.
	h.keys("tab")
	if _, ok := h.a.top().(*sessionsScreen); !ok || len(h.a.stack) != 2 {
		t.Fatalf("tab should replace the tab with sessions, got %T x%d", h.a.top(), len(h.a.stack))
	}
	wantAll(t, h.view(), "first prompt of one")
	h.keys("tab")
	if _, ok := h.a.top().(*memoriesScreen); !ok {
		t.Fatalf("tab should flip back to memories, got %T", h.a.top())
	}

	// enter opens the file read-only.
	h.keys("enter")
	if _, ok := h.a.top().(*fileScreen); !ok {
		t.Fatalf("enter should open the file, got %T", h.a.top())
	}
	wantAll(t, h.view(), "MEMORY.md", "2 lines", "# Memory", "- [Notes](notes.md) — see @~/notes.md")
	h.keys("esc")

	// p scans the directory's paths.
	h.keys("p")
	if _, ok := h.a.top().(*scanPathsScreen); !ok {
		t.Fatalf("p should open scan paths, got %T", h.a.top())
	}
	wantAll(t, h.view(), "notes.md:4: /Users/jonas/x", "notes.md:4: /Users/jonas/y", "@ref MEMORY.md:2: @~/notes.md")
	h.keys("esc", "esc")
	if _, ok := h.a.top().(*projectsScreen); !ok {
		t.Fatalf("esc twice should be back on projects, got %T", h.a.top())
	}
}

func TestMemoriesTabWithoutMemoryDir(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("memories")
	h.keys("enter")
	wantAll(t, h.view(), "no memory dir for "+shortPath(f.project), "(would be "+shortPath(filepath.Join(f.claudeDir, "projects", f.slug, "memory"))+")")
	h.keys("p")
	wantAll(t, h.view(), "no memory dir for "+shortPath(f.project))
	if _, ok := h.a.top().(*memoriesScreen); !ok {
		t.Errorf("p without a memory dir must stay put, got %T", h.a.top())
	}
}

// A file with an escape sequence is shown with the sequence stripped.
func TestFileViewerSanitizes(t *testing.T) {
	f := newFixture(t)
	f.saveAccounts()
	dir := f.memory()
	if err := os.WriteFile(filepath.Join(dir, "evil.md"), []byte("hello "+osc52+"world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := loadServices(context.Background(), Options{CfgDir: f.cfgDir, HomeClaudeDir: f.claudeDir})
	if err != nil {
		t.Fatal(err)
	}
	s := newFileScreen(svc, filepath.Join(dir, "evil.md"), "evil.md")
	_, _ = s.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	_, _ = s.Update(s.Init()())
	out := s.View(80, 10)
	wantAll(t, out, "hello world", "1 line")
	wantNone(t, out, "\x1b]", "52;c;")
}

func TestCtrlCQuitsEvenWhileFiltering(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("sessions")
	h.keys("enter", "/")
	if c, ok := h.a.top().(inputCapturer); !ok || !c.capturingInput() {
		t.Fatal("/ should start typing a filter")
	}
	// q is typed into the filter, not a quit.
	h.keys("q")
	if h.quit {
		t.Fatal("q while filtering must not quit")
	}
	h.keys("ctrl+c")
	if !h.quit {
		t.Fatal("ctrl+c must quit even while filtering")
	}
}

// The listing is the fast path: a transcript whose body is not JSON at
// all lists like any other (id, mtime, size from ReadDir + Info) with no
// error, shows its id until the windows are read, then "-" — proof that
// nothing on the way to the first frame parsed the file. The projects
// screen decodes the cwd from the newest transcript's head and falls
// back to the slug when that fails, still without an error.
func TestListingNeverOpensTranscripts(t *testing.T) {
	f := newFixture(t)
	f.saveAccounts()
	body := "this is not json\n\x00\x01garbage{{{\n"
	path := filepath.Join(f.claudeDir, "projects", f.slug, sid1+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := fixedNow.Add(-time.Hour)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	svc, err := loadServices(context.Background(), Options{CfgDir: f.cfgDir, HomeClaudeDir: f.claudeDir})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return fixedNow }
	root, err := transcripts.RootFor(svc.roots, transcripts.HomeName)
	if err != nil {
		t.Fatal(err)
	}

	s := newSessionsScreen(svc, root, f.slug, f.project)
	_, _ = s.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	page := s.Init()()
	m, ok := page.(sessionsPageMsg)
	if !ok || m.err != nil || len(m.items) != 1 {
		t.Fatalf("page = %#v", page)
	}
	if it := m.items[0]; it.ID != sid1 || it.Title != "" || it.Cwd != "" || it.Size != int64(len(body)) || !it.LastTS.Equal(mtime) {
		t.Fatalf("fast-path item = %+v", it)
	}
	_, cmd := s.Update(page)
	wantAll(t, s.View(120, 20), "(1e005053)", "1h ago")
	if cmd == nil {
		t.Fatal("the page should request the visible titles")
	}
	_, _ = s.Update(cmd())
	if r := s.rows[0]; !r.resolved || r.title() != "-" {
		t.Errorf("after the windows were read: resolved=%v title=%q", r.resolved, r.title())
	}
	if s.err != nil {
		t.Errorf("unexpected screen error: %v", s.err)
	}

	// End to end through the app: projects lists the slug (no cwd could be
	// decoded), enter opens the sessions screen without an error.
	h := f.start("sessions")
	wantAll(t, h.view(), f.slug, "1 session", "1h ago")
	h.keys("enter")
	if _, ok := h.a.top().(*sessionsScreen); !ok {
		t.Fatalf("enter should open the sessions screen, got %T", h.a.top())
	}
	if h.a.statusErr {
		t.Errorf("status shows an error: %q", h.a.status)
	}
	wantAll(t, h.view(), "1 session")
}

// Labels built from names read off the disk are sanitised too: an orphan
// session dir's name and an account name never carry an escape into the
// roots screen, the headers or the memory visibility line.
func TestRootLabelsSanitize(t *testing.T) {
	r := transcripts.Root{Dir: "/x/sessions/ev\x1b[2Jil/projects", ConfigDir: "/x/sessions/ev\x1b[2Jil", Owner: "ev\x1b[2Jil", Orphan: true}
	for _, got := range []string{rootLabel(r), shortRootLabel(r), memoryVisibility(r), shortPath(r.Dir)} {
		if strings.Contains(got, "\x1b") || !strings.Contains(got, "evil") {
			t.Errorf("label not sanitised: %q", got)
		}
	}
	r = transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Shared: true, Accounts: []string{"a", osc52 + "b"}}
	for _, got := range []string{rootLabel(r), shortRootLabel(r), memoryVisibility(r)} {
		if strings.Contains(got, "\x1b") || !strings.Contains(got, "a, b") {
			t.Errorf("label not sanitised: %q", got)
		}
	}
}
