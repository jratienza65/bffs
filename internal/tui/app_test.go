package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	h.send(tea.WindowSizeMsg{Width: 400, Height: 40})
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

// view is the rendered frame with SGR styling stripped: tests assert on
// content; colours are the theme's business (theme_test.go).
func (h *harness) view() string { return plain(h.a.View().Content) }

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
	if strings.HasPrefix(s, "ctrl+") && len(s) == 6 {
		return tea.KeyPressMsg{Code: rune(s[5]), Mod: tea.ModCtrl}
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

// wsCursor is the row under the cursor of a panel.
func wsCursor(h *harness, id panelID) row { return h.a.ws.panels[id].selected() }

// plain strips SGR sequences so frame lines can be matched as text.
func plain(l string) string { return ansiSGR.ReplaceAllString(l, "") }

var ansiSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// wantCounter asserts that the frame line carrying a panel's label ends
// with the counter on its right: "─ 3 SESSIONS | memory ──── 1/16 ─".
func wantCounter(t *testing.T, out, label, counter string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		l = plain(l)
		if strings.Contains(l, label) && strings.Contains(l, "─") {
			if !strings.Contains(l, " "+counter+" ─") {
				t.Errorf("panel %q should show the counter %q on the right: %q", label, counter, l)
			}
			return
		}
	}
	t.Errorf("no frame line carries %q:\n%s", label, out)
}

func TestWorkspaceSingleRoot(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*24*time.Hour))
	f.transcript("-home-nobody-src-x", sid3, "/home/nobody/src/x", "elsewhere", fixedNow.Add(-5*24*time.Hour))
	f.memory()
	h := f.start("sessions")
	ws := h.a.ws

	if h.a.top() != nil || ws.focus != panelProjects {
		t.Fatalf("the workspace should start on the projects panel with no overlay: top=%T focus=%d", h.a.top(), ws.focus)
	}
	out := h.view()
	wantAll(t, out, "bffs test", "work › "+shortPath(f.project)+" › sessions",
		"1 accounts", "work", "partial", "1 account",
		"2 projects", "2 projects in "+shortPath(filepath.Join(f.claudeDir, "projects")),
		shortPath(f.project), "  2 mem 1h ago", "! /home/nobody/src/x", "  1     5d ago",
		"3 SESSIONS | memory",
		// the preview: the project's summary and drift tables
		"2 sessions · memory 2 files · newest 1h ago", "ACROSS ROOTS", "reference: shared pool: work", "shared pool: work", "2 files",
		"one root on this machine", "PER ACCOUNT", "work        ← –", "home          –")
	wantNone(t, out, "\x1b]", "4 files", "(absent)", "accounts 1/1", "projects 1/2")
	wantCounter(t, out, "1 accounts", "1/1")
	wantCounter(t, out, "2 projects", "1/2")
	wantCounter(t, out, "3 SESSIONS | memory", "1/2")
	if r, ok := wsCursor(h, panelProjects).(*projectRow); !ok || r.cwd != f.project {
		t.Errorf("cursor not on the current project: %+v", wsCursor(h, panelProjects))
	}
	// p scans the memory of the project; d never deletes.
	h.keys("p")
	if _, ok := h.a.top().(*scanPathsScreen); !ok {
		t.Fatalf("p should open scan paths, got %T", h.a.top())
	}
	wantAll(t, h.view(), "notes.md:4: /Users/jonas/x", "@ref MEMORY.md:2: @~/notes.md", "3 references")
	h.keys("esc")
	h.keys("d")
	wantAll(t, h.view(), "never deletes")
	// esc goes one panel up; at the top it explains.
	h.keys("esc")
	if ws.focus != panelAccounts {
		t.Errorf("esc on projects should focus accounts, got %d", ws.focus)
	}
	wantAll(t, h.view(), "account work", "oauth · partial isolation", "POOL", "root         shared pool: work", "RECORDED IN ITS .CLAUDE.JSON")
	h.keys("esc")
	wantAll(t, h.view(), "q quits")
	// Panels cycle with tab and h/l; numbers jump; enter drills.
	h.keys("tab")
	if ws.focus != panelProjects {
		t.Errorf("tab should focus panel 2, got %d", ws.focus)
	}
	h.keys("enter")
	if ws.focus != panelItems {
		t.Errorf("enter on a project should focus the sessions, got %d", ws.focus)
	}
	h.keys("h", "h")
	if ws.focus != panelAccounts {
		t.Errorf("h twice should focus panel 1, got %d", ws.focus)
	}
	h.keys("3")
	if ws.focus != panelItems {
		t.Errorf("3 should focus the items panel, got %d", ws.focus)
	}
	// + gives the preview the whole width, _ the panels; again restores.
	h.keys("+")
	wantNone(t, h.view(), "1 accounts")
	h.keys("+")
	wantAll(t, h.view(), "1 accounts", "┐ ┌")
	h.keys("_")
	wantAll(t, h.view(), "preview hidden (_ pressed)")
	wantNone(t, h.view(), "┐ ┌")
	h.keys("_")
	if ws.mode != modeAuto {
		t.Errorf("mode = %d, want auto", ws.mode)
	}
}

// Below the breakpoint the panels take the width and enter shows the
// preview; below the minimum a message names it.
func TestNarrowLayout(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("sessions")
	ws := h.a.ws
	h.send(tea.WindowSizeMsg{Width: 90, Height: 30})
	out := h.view()
	wantAll(t, out, "1 accounts", "2 projects", "3 SESSIONS | memory", "first prompt of one", "preview hidden: the terminal is narrower than 96 columns — enter shows it")
	wantNone(t, out, "┐ ┌", "ACROSS ROOTS")
	// At the breakpoint the two fit side by side again, with a gutter
	// between the boxes and padding inside them.
	h.send(tea.WindowSizeMsg{Width: 96, Height: 30})
	out = h.view()
	wantAll(t, out, "┐ ┌", "ACROSS ROOTS", "│ 1 project in")
	wantNone(t, out, "preview hidden")
	h.send(tea.WindowSizeMsg{Width: 90, Height: 30})
	h.keys("3", "enter")
	if !ws.mainFocus {
		t.Fatal("enter on a session below the breakpoint should show the preview")
	}
	out = h.view()
	wantAll(t, out, "session "+sid1[:8], "EXCERPT", "first prompt   first prompt of one")
	wantNone(t, out, "1 accounts")
	h.keys("enter")
	if _, ok := h.a.top().(*transcriptScreen); !ok {
		t.Fatalf("enter on the preview should open the transcript, got %T", h.a.top())
	}
	h.keys("esc", "esc")
	if ws.mainFocus {
		t.Error("esc should return to the panels")
	}
	wantAll(t, h.view(), "1 accounts")
	h.send(tea.WindowSizeMsg{Width: 30, Height: 10})
	wantAll(t, h.view(), "too small: need 40×12")
}

// Panel 1 lists the accounts as perspectives: the active one first and
// marked, full-isolation and orphan roots as their own rows; space makes
// an account the active one the way bffs switch does.
func TestAccountsPanel(t *testing.T) {
	f := newFixture(t)
	f.accounts.Accounts["full"] = store.Account{Type: store.TypeOAuth, Isolation: store.IsolationFull}
	for _, name := range []string{"full", "gone"} {
		if err := os.MkdirAll(filepath.Join(f.cfgDir, "sessions", name, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveState(f.cfgDir, store.State{Active: "full"}); err != nil {
		t.Fatal(err)
	}
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("sessions")
	ws := h.a.ws

	out := h.view()
	wantAll(t, out, "1 accounts", "full", "● active", "work", "partial", "gone", "orphan", "warning: orphan session dir", "no projects")
	wantCounter(t, out, "1 accounts", "1/3")
	if ws.account != "full" || ws.root.Owner != "full" {
		t.Fatalf("the active account should be the perspective: account=%q owner=%q", ws.account, ws.root.Owner)
	}
	h.keys("1")
	wantAll(t, h.view(), "2 accounts · active full", "oauth · full isolation · active", "root         account: full")
	h.keys("space")
	wantAll(t, h.view(), "full is already the active account")
	h.keys("down")
	if ws.account != "work" || ws.root.Owner != "" {
		t.Fatalf("down should select work on the shared pool: account=%q owner=%q", ws.account, ws.root.Owner)
	}
	wantAll(t, h.view(), shortPath(f.project), "oauth · partial isolation")
	h.keys("space")
	st, err := store.LoadState(f.cfgDir)
	if err != nil || st.Active != "work" {
		t.Fatalf("space should switch the active account: state=%+v err=%v", st, err)
	}
	out = h.view()
	wantAll(t, out, "active account is now work", "2 accounts · active work")
	if r, ok := wsCursor(h, panelAccounts).(*accountRow); !ok || r.name != "work" || !r.active {
		t.Errorf("cursor row = %+v", wsCursor(h, panelAccounts))
	}
	h.keys("down", "down", "space")
	wantAll(t, h.view(), "orphan session dir has no account")
	h.keys("enter")
	if ws.focus != panelProjects {
		t.Errorf("enter on an account should focus the projects, got %d", ws.focus)
	}
}

func TestSessionsPanel(t *testing.T) {
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
	ws := h.a.ws
	h.keys("3")

	out := h.view()
	wantCounter(t, out, "3 SESSIONS | memory", "1/3")
	wantAll(t, out, "work › "+shortPath(f.project)+" › sessions", "3 SESSIONS | memory", "3 sessions · 1 live",
		" ● Plan: session export", "1h ago",
		"   first prompt of three", "3h ago",
		" ↓ prompt two", "9d ago",
		// the preview of the cursor's session leads with the title and a summary
		"session "+sid1[:8], "Plan: session export",
		"account unknown · live · 1h ago · 0 KB · main · "+shortPath(f.project),
		"resume  cd "+shellWord(f.project)+" && claude --resume "+sid1,
		"EXCERPT", "first prompt   first prompt of one",
		"PER ACCOUNT", "work        ← –", "FILES", "transcript   0 KB", "DETAILS", "id           "+sid1, "attribution  unknown")
	wantNone(t, out, "\x1b]", "52;c;", "(1e005053)", "(absent)", "sidecar")
	if r, ok := wsCursor(h, panelItems).(*sessionRow); !ok || r.s.ID != sid1 {
		t.Errorf("cursor should start on the newest session: %+v", wsCursor(h, panelItems))
	}

	// Multi-select: space marks and moves down, a marks every visible
	// row, a again clears; the status row counts.
	h.keys("space")
	wantAll(t, h.view(), "*● Plan: session export", "1 marked")
	if len(ws.sel) != 1 {
		t.Errorf("sel = %v", ws.sel)
	}
	h.keys("a")
	wantAll(t, h.view(), "3 marked")
	h.keys("a")
	wantNone(t, h.view(), "marked")

	// Filter: only the matching row stays; esc clears the filter.
	h.keys("/", "t", "h", "r", "e", "e", "enter")
	out = h.view()
	wantAll(t, out, "first prompt of three")
	wantCounter(t, out, "3 SESSIONS | memory", "1/1")
	wantNone(t, out, "Plan: session export", "prompt two")
	h.keys("esc")
	wantAll(t, h.view(), "Plan: session export", "prompt two")
	wantCounter(t, h.view(), "3 SESSIONS | memory", "1/3")
	if h.a.top() != nil || ws.focus != panelItems {
		t.Fatalf("esc with a filter applied must clear it, not move; top=%T focus=%d", h.a.top(), ws.focus)
	}

	// ? opens the keys overlay with the actions named; ? closes it.
	h.keys("?")
	if _, ok := h.a.top().(*helpScreen); !ok {
		t.Fatalf("? should open the keys, got %T", h.a.top())
	}
	wantAll(t, h.view(), "export", "send", "rehome", "resume", "sync memory", "last session", "no delete here", "memory tab", "switch to account")
	h.keys("?")
	if h.a.top() != nil {
		t.Fatalf("? again should close the keys, got %T", h.a.top())
	}

	// enter opens the full transcript of the cursor's session.
	h.keys("enter")
	if _, ok := h.a.top().(*transcriptScreen); !ok {
		t.Fatalf("enter should open the transcript, got %T", h.a.top())
	}
	out = h.view()
	wantAll(t, out, "transcript "+sid1[:8], "> first prompt of one", "— title: Plan: session export", "2 records shown")
	wantNone(t, out, "\x1b]", "52;c;")
	h.keys("esc")

	// The imported session's preview carries the record.
	h.keys("down", "down")
	out = h.view()
	wantAll(t, out, "prompt two", "imported·pending · 9d ago", "/home/nobody/src/x (missing here)",
		"import       bundle 6f1e2c0a-0000-4000-8000-000000000001", "from mac-a (jonas, /Users/jonas), account work, status pending",
		"old cwd      /home/nobody/src/x")
	// The per-account row names the pointer that claude recorded.
	h.keys("up")
	wantAll(t, h.view(), "first prompt of three", "attribution  last-session · pointer of work", "work        ← –        –        "+sid3[:8]+" (this session)")

	// q quits; the view goes blank.
	h.keys("q")
	if !h.quit || h.view() != "" {
		t.Errorf("q should quit: quit=%v view=%q", h.quit, h.view())
	}
}

// Titles are read after the listing and cached by (path, mtime, size):
// the sessions panel resolves the visible page, and a tab switch away
// and back serves them from the cache.
func TestSessionsLazyTitles(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*time.Hour))
	f.memory()
	h := f.start("sessions")
	ws := h.a.ws
	h.keys("3")
	wantAll(t, h.view(), "first prompt of one", "first prompt of two")
	wantNone(t, h.view(), "(1e005053)")
	if len(ws.svc.titles) != 2 {
		t.Errorf("title cache holds %d entries, want 2", len(ws.svc.titles))
	}
	if len(ws.pending) != 0 {
		t.Errorf("reads still pending: %v", ws.pending)
	}
	h.keys("]", "[")
	if ws.tab != tabSessions || ws.sessLoadedFor == "" {
		t.Fatalf("tab switch lost the sessions: tab=%d loaded=%q", ws.tab, ws.sessLoadedFor)
	}
	wantAll(t, h.view(), "first prompt of one", "first prompt of two")
	if len(ws.svc.titles) != 2 || len(ws.pending) != 0 {
		t.Errorf("titles re-read after the tab switch: cache=%d pending=%v", len(ws.svc.titles), ws.pending)
	}
}

func TestMemoryTab(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	dir := f.memory()
	h := f.start("memories")
	ws := h.a.ws

	if ws.focus != panelItems || ws.tab != tabMemory {
		t.Fatalf("bffs memory opens on the memory tab: focus=%d tab=%d", ws.focus, ws.tab)
	}
	out := h.view()
	wantCounter(t, out, "3 sessions | MEMORY", "1/2")
	wantAll(t, out, shortPath(f.project)+" › memory", "3 sessions | MEMORY", "2 files · 1 pinned",
		"MEMORY.md", "notes.md", "pin",
		// the preview of the cursor's file: who reads it, drift, references, contents
		"memory MEMORY.md", "read by      shared pool — visible to: work", "drift        no other root on this machine",
		"REFERENCES", "@ref 2     @~/notes.md", "CONTENT", "# Memory", "- [Notes](notes.md) — see @~/notes.md")

	// [ flips to sessions and ] back; the selection chain survives.
	h.keys("[")
	if ws.tab != tabSessions {
		t.Fatalf("[ should switch to sessions, got %d", ws.tab)
	}
	wantAll(t, h.view(), "first prompt of one", "3 SESSIONS | memory")
	h.keys("]")
	if ws.tab != tabMemory {
		t.Fatalf("] should switch back to memory, got %d", ws.tab)
	}

	// enter hands the keys to the main pane; esc returns them.
	h.keys("enter")
	if !ws.mainFocus {
		t.Fatal("enter on a memory file should focus the main pane")
	}
	h.keys("esc")
	if ws.mainFocus {
		t.Fatal("esc should return to the panels")
	}

	// The next file's references say whether the paths exist here.
	h.keys("down")
	wantAll(t, h.view(), "memory notes.md", "path 4     /Users/jonas/x", "missing here")

	// p scans the directory's paths.
	h.keys("p")
	if _, ok := h.a.top().(*scanPathsScreen); !ok {
		t.Fatalf("p should open scan paths, got %T", h.a.top())
	}
	wantAll(t, h.view(), "notes.md:4: /Users/jonas/x", "notes.md:4: /Users/jonas/y", "@ref MEMORY.md:2: @~/notes.md", shortPath(dir))
	h.keys("esc")
	if h.a.top() != nil {
		t.Fatalf("esc should close the overlay, got %T", h.a.top())
	}
}

func TestMemoryTabWithoutMemoryDir(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("memories")
	wantAll(t, h.view(), "no memory dir yet", "would be     "+shortPath(filepath.Join(f.claudeDir, "projects", f.slug, "memory")), "3 sessions | MEMORY", "no memory dir")
	wantCounter(t, h.view(), "3 sessions | MEMORY", "0")
	h.keys("p")
	wantAll(t, h.view(), "no memory dir for this project")
	h.keys("S")
	wantAll(t, h.view(), "no memory dir for this project")
	if h.a.top() != nil {
		t.Errorf("p or S without a memory dir must stay put, got %T", h.a.top())
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
	h.keys("3", "/")
	if !h.a.ws.capturing() {
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

// pump runs a workspace command chain the way the harness runs the
// app's, feeding every message back to the workspace, until stop says
// a message is the one the caller wants to inspect first.
func pump(ws *workspace, cmd tea.Cmd, stop func(tea.Msg) bool) tea.Msg {
	pending := []tea.Cmd{cmd}
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch m := msg.(type) {
		case nil:
		case tea.BatchMsg:
			pending = append(pending, m...)
		default:
			if stop != nil && stop(m) {
				return m
			}
			pending = append(pending, ws.Update(m))
		}
	}
	return nil
}

// The listing is the fast path: a transcript whose body is not JSON at
// all lists like any other (id, mtime, size from ReadDir + Info) with no
// error, shows its id until the windows are read, then "-" — proof that
// nothing on the way to the first frame parsed the file. The projects
// panel decodes the cwd from the newest transcript's head and falls
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

	ws := newWorkspace(svc)
	ws.setSize(200, 40)
	page := pump(ws, ws.setRoots(svc.roots), func(m tea.Msg) bool { _, ok := m.(sessionsPageMsg); return ok })
	m, ok := page.(sessionsPageMsg)
	if !ok || m.err != nil || len(m.items) != 1 {
		t.Fatalf("page = %#v", page)
	}
	if it := m.items[0]; it.ID != sid1 || it.Title != "" || it.Cwd != "" || it.Size != int64(len(body)) || !it.LastTS.Equal(mtime) {
		t.Fatalf("fast-path item = %+v", it)
	}
	ws.focus = panelItems
	cmd := ws.Update(page)
	wantAll(t, ws.View(200, 40, nil, "", false), "(1e005053)", "1h ago")
	titles := pump(ws, cmd, func(m tea.Msg) bool { _, ok := m.(titlesResolvedMsg); return ok })
	if titles == nil {
		t.Fatal("the page should request the visible titles")
	}
	pump(ws, ws.Update(titles), nil)
	if r := ws.sessRows[0]; !r.resolved || r.title() != "-" {
		t.Errorf("after the windows were read: resolved=%v title=%q", r.resolved, r.title())
	}

	// End to end through the app: projects lists the slug (no cwd could be
	// decoded), the sessions panel shows the session without an error.
	h := f.start("sessions")
	wantAll(t, h.view(), f.slug, "  1     1h ago")
	h.keys("3")
	if h.a.statusErr {
		t.Errorf("status shows an error: %q", h.a.status)
	}
	wantCounter(t, h.view(), "3 SESSIONS | memory", "1/1")
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
