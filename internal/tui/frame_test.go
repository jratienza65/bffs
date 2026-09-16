package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/textdiff"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

var update = flag.Bool("update", false, "rewrite golden frames")

// The frame tests are about rendering, so their fixture never touches the
// filesystem: every path is a literal that reads the same on Linux, macOS
// and Windows, and nothing here depends on a temp directory whose length
// would shift a truncation. The model tests in app_test.go cover the
// loaders that put real data in these rows.
const (
	goldenPool     = "/home/d/.claude"
	goldenProject  = "/home/d/build/projects/bffs"
	goldenOther    = "/home/d/build/sidequests/ledger"
	goldenSession  = "1e005053-380a-4245-a145-52c2715afa73"
	goldenSession2 = "b19c4e20-1111-4222-8333-444455556666"
	goldenSession3 = "c0ffee00-1111-4222-8333-444455556666"
)

func goldenRoot() transcripts.Root {
	return transcripts.Root{
		Dir: goldenPool + "/projects", ConfigDir: goldenPool, Shared: true,
		Accounts: []string{"alpha", "beta"}, ClaudeJSON: "/home/d/.claude.json",
	}
}

func goldenFullRoot() transcripts.Root {
	return transcripts.Root{Dir: "/home/d/bffs/sessions/work/projects", ConfigDir: "/home/d/bffs/sessions/work", Owner: "work"}
}

// goldenServices is the fixture: two accounts on one shared pool and a
// third on its own, a clock that does not move, no attributor.
func goldenServices() *services {
	now := func() time.Time { return fixedNow }
	return &services{
		ctx:           context.Background(),
		cfgDir:        "/home/d/bffs",
		homeClaudeDir: goldenPool,
		version:       "test",
		start:         "sessions",
		cwd:           goldenProject,
		accs: store.Accounts{Accounts: map[string]store.Account{
			"alpha": {Type: store.TypeOAuth, Email: "a@example.com"},
			"beta":  {Type: store.TypeOAuth},
			"work":  {Type: store.TypeOAuth, Isolation: store.IsolationFull},
		}},
		state:    store.State{Active: "alpha"},
		roots:    []transcripts.Root{goldenRoot(), goldenFullRoot()},
		imports:  map[string]imports.SessionRef{},
		now:      now,
		titles:   map[titleKey]titleMeta{},
		history:  map[string]transcripts.HistoryIndex{},
		memories: map[string][]transcripts.Memory{},
	}
}

// goldenSessions are the rows of the sessions panel: one live, one plain,
// one imported whose directory is missing here.
func goldenSessions(sel map[string]bool, now func() time.Time) []*sessionRow {
	root := goldenRoot()
	rec := &imports.Record{BundleID: "6f1e2c0a-0000-4000-8000-000000000001", Kind: imports.KindImport,
		ImportedAt: fixedNow.Add(-8 * 24 * time.Hour), Account: "work",
		Source: imports.Source{Hostname: "mac-a", User: "dev", Home: "/Users/dev"}}
	return []*sessionRow{
		{s: transcripts.Session{ID: goldenSession, Slug: "-home-d-build-projects-bffs", Path: goldenPool + "/projects/-home-d-build-projects-bffs/" + goldenSession + ".jsonl",
			Root: root, Cwd: goldenProject, CwdExists: true, GitBranch: "main", Version: "2.1.259",
			Title: "Session and memory import/export", TitleSource: "custom", Account: "alpha", AttribSource: "last-session",
			FirstTS: fixedNow.Add(-3 * time.Hour), LastTS: fixedNow.Add(-time.Hour), Size: 5_242_880, Live: true},
			resolved: true, sel: sel, now: now},
		{s: transcripts.Session{ID: goldenSession2, Slug: "-home-d-build-projects-bffs", Path: goldenPool + "/projects/-home-d-build-projects-bffs/" + goldenSession2 + ".jsonl",
			Root: root, Cwd: goldenProject, CwdExists: true, GitBranch: "main",
			Title: "Plan: session export", TitleSource: "ai", Account: "beta", AttribSource: "launch-log",
			LastTS: fixedNow.Add(-4 * time.Hour), Size: 1_100_000},
			resolved: true, sel: sel, now: now},
		{s: transcripts.Session{ID: goldenSession3, Slug: "-machine-a-src-proj", Path: goldenPool + "/projects/-machine-a-src-proj/" + goldenSession3 + ".jsonl",
			Root: root, Cwd: "/machine-a/src/proj", Title: "prompt from the other machine",
			LastTS: fixedNow.Add(-9 * 24 * time.Hour), Size: 42_000,
			Import: &imports.SessionRef{Record: rec, Session: &imports.Session{ID: goldenSession3, OldCwd: "/machine-a/src/proj", Status: imports.StatusPending}}},
			resolved: true, sel: sel, now: now},
	}
}

func goldenDrift() projectDrift {
	pool, full := goldenRoot(), goldenFullRoot()
	accepted, declined := trust.Accepted, trust.Declined
	_ = declined
	return projectDrift{
		slug: "-home-d-build-projects-bffs", cwd: goldenProject, key: goldenProject, gitRoot: goldenProject,
		ref: pool, sessions: 3, newest: fixedNow.Add(-time.Hour),
		roots: []rootDrift{
			{root: pool, here: true, sessions: 3, files: map[string]memFile{"MEMORY.md": {}, "notes.md": {}}, state: "here"},
			{root: full, sessions: 1, files: map[string]memFile{"notes.md": {}}, state: "differs",
				diffs: []fileDrift{{"MEMORY.md", "only here"}, {"notes.md", "differs (newer there)"}}},
		},
		accounts: []accountDrift{
			{status: trust.Status{Account: "alpha", Folder: accepted, External: accepted}, lastSession: goldenSession, inProject: true},
			{status: trust.Status{Account: "beta", Folder: trust.Declined}},
			{status: trust.Status{Account: "home", Folder: trust.Inherited, InheritedFrom: "/home/d"}},
		},
	}
}

// goldenApp builds the browser at a size with every panel seeded, then
// applies the tweak the state under test needs.
func goldenApp(t testing.TB, w, h int, tweak func(*app)) *app {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("BFFS_THEME", "")
	goldenTZ(t)
	prevHost := osHostname
	osHostname = func() (string, error) { return "mac-a", nil }
	t.Cleanup(func() { osHostname = prevHost })
	svc := goldenServices()
	a := newApp(svc)
	ws := a.ws
	ws.roots = svc.roots

	accounts := []row{
		&accountRow{name: "alpha", kind: "partial", root: goldenRoot(), active: true},
		&accountRow{name: "beta", kind: "partial", root: goldenRoot()},
		&accountRow{name: "work", kind: "full", root: goldenFullRoot()},
	}
	_ = ws.panels[panelAccounts].setRows(accounts)
	ws.panels[panelAccounts].setStatus("3 accounts · active alpha")
	ws.account, ws.rootKey, ws.root = "alpha", rootID(goldenRoot()), goldenRoot()

	projects := []row{
		&projectRow{slug: "-home-d-build-projects-bffs", cwd: goldenProject, cwdExists: true, sessions: 3, hasMemory: true, newest: fixedNow.Add(-time.Hour), now: svc.now},
		&projectRow{slug: "-home-d-build-sidequests-ledger", cwd: goldenOther, cwdExists: true, sessions: 4, newest: fixedNow.Add(-26 * time.Hour), now: svc.now},
		&projectRow{slug: "-machine-a-src-proj", cwd: "/machine-a/src/proj", sessions: 1, hasMemory: true, newest: fixedNow.Add(-9 * 24 * time.Hour), now: svc.now},
	}
	_ = ws.panels[panelProjects].setRows(projects)
	ws.panels[panelProjects].setStatus("3 projects in " + shortPath(goldenRoot().Dir))
	ws.project = projects[0].(*projectRow)
	ws.projectKey = ws.rootKey + "\x00" + ws.project.slug

	ws.sessRows = goldenSessions(ws.sel, svc.now)
	ws.byID = map[string]*sessionRow{}
	for _, r := range ws.sessRows {
		ws.byID[r.s.ID] = r
	}
	ws.sessLoadedFor = ws.projectKey
	_ = ws.panels[panelItems].setRows(ws.sessionRowsAsRows())
	ws.panels[panelItems].setStatus("3 sessions · 1 live")
	ws.itemsKey = fmt.Sprintf("%d\x00%s", ws.tab, ws.projectKey)

	ws.mem = &transcripts.Memory{Root: goldenRoot(), Slug: ws.project.slug, Dir: goldenPool + "/projects/-home-d-build-projects-bffs/memory",
		Cwd: goldenProject, GitRoot: goldenProject, CwdExists: true, HasIndex: true,
		Files: []transcripts.MemoryFile{
			{Name: "MEMORY.md", Size: 2048, ModTime: fixedNow.Add(-2 * time.Hour), AtRefs: []string{"@~/notes.md"}},
			{Name: "notes.md", Size: 1024, ModTime: fixedNow.Add(-30 * time.Hour), Pinned: true, AbsolutePaths: []string{"/home/d/x", "/home/d/y"}},
		}}
	ws.memDir, ws.memLoadedFor = ws.mem.Dir, ws.projectKey

	ws.previewKey = "drift:" + ws.projectKey
	ws.vp.SetContentLines(driftLines(goldenDrift(), "alpha", fixedNow))

	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	if tweak != nil {
		tweak(a)
	}
	// A tweak that moves focus or switches tab leaves the workspace where
	// Update would have left it: sized, and with the panel rows the tab
	// implies.
	if ws.tab == tabMemory {
		_ = ws.panels[panelItems].setRows(ws.memoryRows())
		ws.panels[panelItems].setStatus("2 files · 1 pinned · " + shortPath(ws.mem.Dir))
	}
	seedPreview(a)
	// An overlay a tweak put on the stack gets its size the way push
	// would have given it one; its Init is deliberately not run, so no
	// state here is waiting on an operation.
	if a.top() != nil {
		_ = a.forward(a.mainSize())
	}
	ws.layout()
	return a
}

// seedPreview fills the main pane with what the preview command for the
// current selection would have returned. A tweak that moves the focus
// goes through sync, which starts that command; the test never runs it,
// so without this the frame would freeze at "loading…" — and running it
// would read the filesystem the fixture deliberately avoids.
func seedPreview(a *app) {
	ws := a.ws
	key, _ := ws.previewFor()
	ws.previewKey, ws.previewGen, ws.previewBusy = key, ws.gen, false
	var lines []string
	switch {
	case strings.HasPrefix(key, "session:"):
		lines = detailLines(goldenDetail(ws.selectedSession()), fixedNow)
	case strings.HasPrefix(key, "memfile:"):
		lines = goldenMemoryLines(ws.selectedMemFile())
	case strings.HasPrefix(key, "account:"):
		lines = goldenAccountLines(ws.selectedAccount())
	default:
		lines = driftLines(goldenDrift(), ws.account, fixedNow)
	}
	ws.vp.SetContentLines(lines)
	ws.vp.GotoTop()
}

// goldenTranscript is a few records of a conversation in Claude's own
// shape, rendered by the real renderer so the golden shows what the
// viewer draws rather than a fixture of its own.
const goldenTranscript = `{"type":"user","timestamp":"2026-08-24T09:00:03Z","message":{"role":"user","content":"add a golden harness for the browser"}}
{"type":"assistant","timestamp":"2026-08-24T09:00:07Z","message":{"role":"assistant","content":[{"type":"text","text":"Thirteen frames and a fit sweep. The fixture never touches the filesystem."},{"type":"tool_use","name":"Write","input":{"file_path":"internal/tui/frame_test.go"}}]}}
{"type":"user","timestamp":"2026-08-24T09:00:31Z","isMeta":true,"message":{"role":"user","content":"<system-reminder>hidden</system-reminder>"}}
{"type":"user","timestamp":"2026-08-24T09:00:32Z","message":{"role":"user","content":[{"type":"tool_result","content":"wrote 407 lines"}]}}
{"type":"summary","timestamp":"2026-08-24T09:02:00Z","summary":"Golden frames and a fit sweep for the browser"}
`

// goldenDetail is what loadDetail would have gathered for a row: the
// artifacts Claude keeps beside a transcript, sized and present.
func goldenDetail(r *sessionRow) sessionDetail {
	d := sessionDetail{Session: r.s, Artifacts: transcripts.Artifacts{
		Transcript:     r.s.Path,
		SidecarDir:     strings.TrimSuffix(r.s.Path, ".jsonl"),
		FileHistoryDir: goldenPool + "/file-history/" + r.s.ID,
		TasksDir:       goldenPool + "/todos/" + r.s.ID,
	}}
	if r.s.Live {
		d.LivePID, d.SidecarExists, d.SidecarSize, d.FileHistoryExist = 4213, true, 18_432, true
		d.Claimants = []string{"alpha"}
	}
	return d
}

// goldenMemoryLines mirrors memoryFilePreview: the header, who reads the
// file, how it compares elsewhere, its references and its content.
func goldenMemoryLines(r *memoryFileRow) []string {
	lines := []string{
		styleHeader.Render(r.f.Name) + "  " + linkPath(styleLink, joinName(r.dir, r.f.Name)),
		kvLine("read by", memoryVisibility(goldenRoot())),
		kvLine("drift", shortRootLabel(goldenFullRoot())+": "+stateStyled("differs")),
	}
	if len(r.f.AbsolutePaths) > 0 || len(r.f.AtRefs) > 0 {
		lines = append(lines, "", section("references", "absolute paths and @-refs inside the file"))
		for _, ref := range append(append([]string{}, r.f.AtRefs...), r.f.AbsolutePaths...) {
			lines = append(lines, "  "+ref+"  (missing)")
		}
	}
	// The content goes through the real renderer, so the golden shows
	// what a memory file actually looks like in the pane.
	doc := "# " + r.f.Name + "\n\nWhat this project is, in a sentence that has to wrap.\n\n" +
		"## Decisions\n\n- memory is per root, never per account\n- `bffs copy` never moves\n\n" +
		"```bash\nbffs export --out ~/bffs-mac-a.bffs\n```\n"
	return append(append(lines, "", section("content", "")), renderMarkdown(doc, goldenPreviewWidth)...)
}

// goldenPreviewWidth is the main pane's content width at 120 columns,
// which is what the memory golden is rendered for.
const goldenPreviewWidth = 73

// goldenAccountLines mirrors accountPreview without reading .claude.json.
func goldenAccountLines(r *accountRow) []string {
	summary := "oauth · " + r.kind + " isolation"
	if r.active {
		summary += " · active"
	}
	return []string{
		styleHeader.Render(r.name), summary, "",
		section("pool", ""),
		kvLine("root", shortRootLabel(r.root)),
		kvLink("projects dir", r.root.Dir),
		kvLink("config dir", r.root.ConfigDir),
		kvLine("retention", "30 days (settings.json)"),
		kvLine("live sessions", "1"),
		"", section("recorded in its .claude.json", shortPath(r.root.ConfigDir+"/.claude.json")),
		kvLine("projects", "12"), kvLine("trusted", "9"), kvLine("last sessions", "4"),
		"", styleFaint.Render("space makes it the active account · enter opens its projects · i receives a bundle into its pool"),
	}
}

// goldenTZ pins the zone, because a preview formats timestamps in the
// local one: without this a golden written in Manila fails in CI.
func goldenTZ(t testing.TB) {
	t.Helper()
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
}

// painted renders a frame with its escapes, for the tests about colour.
func painted(t testing.TB, w, h int, tweak func(*app)) string {
	t.Helper()
	return goldenApp(t, w, h, tweak).View().Content
}

// frameAt strips the escapes: a golden holds layout, not colour, which
// keeps it readable and immune to a colour profile differing per machine.
func frameAt(t testing.TB, w, h int, tweak func(*app)) string {
	t.Helper()
	return ansi.Strip(painted(t, w, h, tweak))
}

// downsample puts a frame through the writer Bubble Tea uses on a terminal
// of that profile: ANSI rounds every colour to the base 16, Ascii drops
// colour but keeps the SGR attributes such as bold.
func downsample(s string, p colorprofile.Profile) string {
	var b strings.Builder
	w := &colorprofile.Writer{Forward: &b, Profile: p}
	if _, err := w.WriteString(s); err != nil {
		t := &testing.T{}
		t.Fatal(err)
	}
	return b.String()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: make golden)", err)
	}
	if got != string(want) {
		t.Fatalf("frame %s differs (make golden rewrites it; read the diff first)\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
	}
}

// states are the frames worth pinning: every panel focused, both tabs,
// the two layouts the width chooses between, and each overlay.
func goldenStates() map[string]func(*app) {
	return map[string]func(*app){
		"accounts": func(a *app) { _ = a.ws.setFocus(panelAccounts) },
		"projects": nil,
		"sessions": func(a *app) { _ = a.ws.setFocus(panelItems) },
		"marked": func(a *app) {
			_ = a.ws.setFocus(panelItems)
			a.ws.sel[goldenSession] = true
			a.ws.sel[goldenSession2] = true
		},
		"memory":    func(a *app) { a.ws.tab = tabMemory; a.ws.applyTab(); _ = a.ws.setFocus(panelItems) },
		"main-only": func(a *app) { a.ws.mode = modeMainOnly },
		"side-only": func(a *app) { a.ws.mode = modeSideOnly },
		"preview-focused": func(a *app) {
			a.ws.mainFocus = true
		},
		"menu": func(a *app) { a.stack = append(a.stack, newMenuScreen(a.ws.menuTitle(), a.ws.actions())) },
		"keys": func(a *app) { a.stack = append(a.stack, newHelpScreen(a.ws.helpGroups())) },
		"result": func(a *app) {
			a.stack = append(a.stack, newResultScreen("export", []string{"wrote ~/bffs-mac-a.bffs (5.2 MB, 3 sessions, 2 memory files, bundle 6f1e2c0a)", "", "On the other machine: bffs import --from bffs-mac-a.bffs"}, nil))
		},
		"toast": func(a *app) {
			_ = a.notify(noteDone, "export finished", "wrote ~/bffs-mac-a.bffs — 5.2 MB, 3 sessions, 2 memory files")
		},
		"status-done": func(a *app) {
			a.status, a.statusKind = "copied the transcript path: ~/.claude/projects/-home-d-build-projects-bffs/1e005053.jsonl", noteDone
		},
		"status-error": func(a *app) {
			a.status, a.statusKind = "refusing to pair with 203.0.113.5: not on a local network of this machine", noteBad
		},
		// The action overlays: the screens most likely to overflow, each
		// drawn over the workspace at the step it opens on.
		"export": func(a *app) { a.stack = append(a.stack, newExportScreen(a.svc, a.ws.target())) },
		"copy":   func(a *app) { a.stack = append(a.stack, newCopyScreen(a.svc, a.ws.target())) },
		"rehome": func(a *app) {
			a.stack = append(a.stack, newRehomeScreen(a.svc, a.ws.target(), []transcripts.Session{a.ws.sessRows[0].s}))
		},
		"diff": func(a *app) {
			sc := newDriftScreen(a.svc, goldenRoot(), "-home-d-build-projects-bffs", goldenProject, "notes.md")
			a.stack = append(a.stack, sc)
			there := "---\npinned: true\n---\n\n- the other machine's copy of this note\n- with a line that only it has\n"
			here := "---\npinned: true\n---\n\n- the copy on this root\n- with a line that only it has\n- and one more\n"
			lines := []string{section(shortRootLabel(goldenFullRoot()), stateStyled("differs")),
				"  notes.md  " + stateStyled("differs (newer there)")}
			hunks := textdiff.Diff(there, here)
			stat := textdiff.Count(hunks)
			lines = append(lines, styleFaint.Render(fmt.Sprintf("    %d added, %d removed (there → here)", stat.Added, stat.Removed)))
			_ = a.forward(diffLoadedMsg{key: sc.key, lines: append(lines, hunkLines(hunks)...)})
		},
		"trust": func(a *app) {
			cwd := a.ws.projectCwd()
			a.stack = append(a.stack, newTrustScreen(a.svc, cwd))
			var statuses []trust.Status
			for _, ad := range goldenDrift().accounts {
				statuses = append(statuses, ad.status)
			}
			_ = a.forward(trustLoadedMsg{project: cwd, key: cwd, statuses: statuses, files: map[string]string{
				"alpha": goldenPool + "/.claude.json",
				"beta":  goldenPool + "/.claude.json",
				"home":  "/home/d/.claude.json",
			}})
		},
		"receive": func(a *app) { a.stack = append(a.stack, newReceiveScreen(a.svc, a.ws.root, a.ws.account)) },
		"transcript": func(a *app) {
			sess := a.ws.sessRows[0].s
			a.stack = append(a.stack, newTranscriptScreen(a.svc, sess))
			lines, records, hidden, truncated, err := renderTranscript(strings.NewReader(goldenTranscript), 1<<20)
			_ = a.forward(transcriptLoadedMsg{path: sess.Path, lines: lines, records: records, hidden: hidden, truncated: truncated, err: err})
		},
		"wizard": func(a *app) {
			var projects []*projectRow
			for _, r := range a.ws.panels[panelProjects].rows() {
				projects = append(projects, r.(*projectRow))
			}
			a.stack = append(a.stack, newWizardScreen(a.svc, a.ws.target(), a.ws.root, a.ws.perspectiveAccount(), true, projects))
		},
		"filtered": func(a *app) {
			_ = a.ws.setFocus(panelItems)
			p := a.ws.panels[panelItems]
			p.list.SetFilterText("export")
			p.list.SetFilterState(1)
		},
	}
}

// Every state at the standard size, pinned. When one of these changes, the
// diff of the golden is the review.
func TestGoldenFrames(t *testing.T) {
	for name, tweak := range goldenStates() {
		t.Run(name, func(t *testing.T) {
			checkGolden(t, name+"-120x32", frameAt(t, 120, 32, tweak))
		})
	}
}

// The sizes a frame has to survive, including the two below the floor.
var frameSizes = [][2]int{{160, 40}, {120, 32}, {100, 30}, {96, 24}, {90, 24}, {80, 24}, {72, 20}, {60, 16}, {48, 12}, {40, 12}, {30, 10}, {20, 5}}

// The sweep: no frame may be wider than its terminal or taller than it, in
// any state, at any size — including below the floor, where the frame is
// replaced by the message that names the minimum.
func TestFramesFitTheirTerminal(t *testing.T) {
	for name, tweak := range goldenStates() {
		for _, size := range frameSizes {
			w, h := size[0], size[1]
			frame := frameAt(t, w, h, tweak)
			lines := strings.Split(frame, "\n")
			if len(lines) > h {
				t.Errorf("%s at %dx%d: %d lines", name, w, h, len(lines))
			}
			for i, l := range lines {
				if got := ansi.StringWidth(l); got > w {
					t.Errorf("%s at %dx%d: line %d is %d cells: %q", name, w, h, i, got, l)
				}
			}
		}
	}
}

// Below the floor the frame says what it needs, at any width, and nothing
// else is drawn.
func TestTooSmallNamesTheMinimum(t *testing.T) {
	for _, size := range [][2]int{{39, 20}, {80, 11}, {20, 5}} {
		frame := frameAt(t, size[0], size[1], nil)
		if !strings.Contains(frame, fmt.Sprintf("need %d×%d", minWidth, minHeight)) {
			t.Errorf("%dx%d: %q", size[0], size[1], frame)
		}
		if strings.Contains(frame, "1 accounts") {
			t.Errorf("%dx%d drew the panels anyway:\n%s", size[0], size[1], frame)
		}
	}
}
