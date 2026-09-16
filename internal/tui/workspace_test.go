package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// fullRoot adds a full-isolation account with its own projects/ tree
// and returns that tree.
func (f *fixture) fullRoot(name string) string {
	f.t.Helper()
	f.accounts.Accounts[name] = store.Account{Type: store.TypeOAuth, Isolation: store.IsolationFull}
	dir := filepath.Join(f.cfgDir, "sessions", name, "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// The project preview compares the project across roots — sessions per
// slug, memory files by sha256 — and lists what each account's
// .claude.json records for the project key.
func TestDriftAcrossRoots(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	f.lastSession("work", f.project, sid1)
	fullProjects := f.fullRoot("full")
	// The full root holds one older transcript and a memory dir with a
	// newer, different notes.md and no MEMORY.md.
	other := filepath.Join(fullProjects, f.slug, sid2+".jsonl")
	if err := os.MkdirAll(filepath.Join(fullProjects, f.slug, transcripts.MemorySubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"type":"user","cwd":"`+f.project+`","sessionId":"`+sid2+`","message":{"role":"user","content":"x"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(fullProjects, f.slug, transcripts.MemorySubdir, "notes.md")
	if err := os.WriteFile(notes, []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(notes, later, later); err != nil {
		t.Fatal(err)
	}
	key, err := transcripts.ProjectKey(f.project)
	if err != nil {
		t.Fatal(err)
	}
	homeJSON := filepath.Join(f.home, ".claude.json")
	doc := map[string]any{"projects": map[string]any{key: map[string]any{"hasTrustDialogAccepted": true, "hasClaudeMdExternalIncludesApproved": true}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(homeJSON, b, 0o600); err != nil {
		t.Fatal(err)
	}

	h := f.start("sessions")
	out := h.view()
	wantAll(t, out, "project "+shortPath(f.project), "reference: shared pool: work",
		"shared pool: work", "2 files",
		"account: full", "differs — MEMORY.md only here, notes.md differs (newer there)",
		"PER ACCOUNT",
		"work        ← –        –        "+sid1[:8]+" (this project)",
		"home          ✓        ✓        –")
	// The memory file preview compares the same file on the other root.
	h.keys("3", "]", "down")
	wantAll(t, h.view(), "memory notes.md", "drift        account: full: differs (newer there)")
	h.keys("up")
	wantAll(t, h.view(), "memory MEMORY.md", "drift        account: full: only here")
}

// x lists the actions the selection allows and runs the chosen one
// after the menu closes.
func TestMenu(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("x")
	if _, ok := h.a.top().(*menuScreen); !ok {
		t.Fatalf("x should open the menu, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "actions for "+shortPath(f.project), "transfer wizard", "receive a bundle over the LAN", "export the whole project", "send the whole project", "copy the whole project",
		"trust matrix for", "sync the memory of", "scan the memory of")
	wantNone(t, out, "resume", "rehome")
	// A letter runs the action straight from the menu.
	h.keys("e")
	if _, ok := h.a.top().(*exportScreen); !ok || len(h.a.stack) != 1 {
		t.Fatalf("e in the menu should open export over a closed menu, got %T x%d", h.a.top(), len(h.a.stack))
	}
	h.keys("esc")
	// enter runs the cursor's item; esc closes the menu.
	h.keys("x", "enter")
	if _, ok := h.a.top().(*wizardScreen); !ok {
		t.Fatalf("enter on the first item should open the transfer wizard, got %T", h.a.top())
	}
	h.keys("esc", "x", "down", "enter")
	if _, ok := h.a.top().(*receiveScreen); !ok {
		t.Fatalf("the second item should open receive, got %T", h.a.top())
	}
	h.keys("esc", "x", "esc")
	if h.a.top() != nil {
		t.Fatalf("esc should close the menu, got %T", h.a.top())
	}
	// On the sessions panel the menu gains resume, the pointer and rehome
	// for a selection.
	h.keys("3", "space", "x")
	wantAll(t, h.view(), "resume "+sid1[:8]+" in claude", "point an account's last session at "+sid1[:8], "rehome 1 session", "export 1 selected session")
	h.keys("esc")
}

// S copies the project's memory alone into a full-isolation root,
// merging it there; the pool it already lives in is greyed.
func TestSyncMemory(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	fullProjects := f.fullRoot("full")
	h := f.start("sessions")
	h.keys("S")
	sc, ok := h.a.top().(*copyScreen)
	if !ok || sc.tgt.only != "memories" {
		t.Fatalf("S should open the memory-only copy, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "sync memory to account", "copy the memory of "+shortPath(f.project), "already shared under partial isolation", "full")
	if sc.rows[sc.cursor].name != "full" {
		t.Fatalf("cursor should start on the first usable destination, got %q", sc.rows[sc.cursor].name)
	}
	h.keys("enter")
	if sc.state != copyConfirm {
		t.Fatalf("state = %v:\n%s", sc.state, h.view())
	}
	wantAll(t, h.view(), "0 sessions, 1 memory dir", "copy 1 memory dir (merged into the destination's memory) to full? [y/N]")
	h.keys("y")
	rs, ok := h.a.top().(*resultScreen)
	if !ok || rs.err != nil {
		t.Fatalf("y should end on a clean result, got %T:\n%s", h.a.top(), h.view())
	}
	for _, name := range []string{transcripts.MemoryIndexFile, "notes.md"} {
		if _, err := os.Stat(filepath.Join(fullProjects, f.slug, transcripts.MemorySubdir, name)); err != nil {
			t.Errorf("%s not synced: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fullProjects, f.slug, sid1+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("a memory sync must not copy transcripts: %v", err)
	}
	h.keys("esc")
	// The drift table now compares the synced copy: MEMORY.md is the
	// same, notes.md differs because the merge neutralised its pinned
	// frontmatter (pinned-imported) — the drift view says so.
	out = h.view()
	wantAll(t, out, "account: full", "differs — notes.md differs")
	wantNone(t, out, "MEMORY.md only here", "MEMORY.md differs")
}

// L points an account's last-session pointer at the cursor's session,
// under Claude's lock, and leaves the rest of the file alone.
func TestPointerScreen(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.lastSession("work", f.project, sid2)
	h, _ := f.openSessions()
	h.keys("L")
	sc, ok := h.a.top().(*pointerScreen)
	if !ok {
		t.Fatalf("L should open the pointer screen, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "last-session pointer", "point an account's last session for "+shortPath(f.project)+" at "+sid1[:8], "ACCOUNT", "CURRENT POINTER", "work", sid2[:8], "home")
	if sc.rows[sc.cursor].name != "work" {
		t.Fatalf("cursor = %q", sc.rows[sc.cursor].name)
	}
	h.keys("enter")
	wantAll(t, h.view(), "point work's lastSessionId for "+shortPath(f.project)+" at "+sid1[:8]+"? [y/N]")
	h.keys("y")
	if h.a.top() != nil {
		t.Fatalf("y should write and close, got %T:\n%s", h.a.top(), h.view())
	}
	wantAll(t, h.view(), "work: lastSessionId for "+shortPath(f.project)+" now points at "+sid1[:8])
	key, err := transcripts.ProjectKey(f.project)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := claudejson.ReadProjectFlags(filepath.Join(f.cfgDir, "sessions", "work", ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if flags[key].LastSessionID != sid1 {
		t.Errorf("lastSessionId = %q, want %s", flags[key].LastSessionID, sid1)
	}
	// The refreshed preview shows the pointer on the account row.
	wantAll(t, h.view(), "work        ← –        –        "+sid1[:8]+" (this session)")
	// Pointing again is a no-op with a status line.
	h.keys("L", "enter")
	wantAll(t, h.view(), "work already points at this session")
	h.keys("esc")
}

// The session preview lists the files Claude keeps for it — only the
// ones that exist — and enter on the transcript row is not needed: the
// transcript opens from the row itself.
func TestSessionPreviewFiles(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	side := filepath.Join(f.claudeDir, "projects", f.slug, sid1, "subagents")
	if err := os.MkdirAll(side, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(side, "agent-1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := f.openSessions()
	out := h.view()
	wantAll(t, out, "FILES", "transcript   0 KB", "sidecar      0 KB, 1 subagent")
	wantNone(t, out, "(absent)", "file-history", "tasks")
}

// click and wheel drive the frame the way the keys do: a click focuses
// the panel under it and picks the row, the wheel scrolls whatever is
// under the pointer, and the preview takes the keys when clicked.
func TestMouse(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*time.Hour))
	f.transcript(f.slug, sid3, f.project, "first prompt of three", fixedNow.Add(-3*time.Hour))
	h := f.start("sessions")
	ws := h.a.ws
	if v := h.a.View(); v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("the view should ask for mouse reporting, got %v", v.MouseMode)
	}

	// The frame: the header is line 0, the box starts on line 1, and
	// each panel is a title line followed by its rows.
	hs := ws.panelHeights()
	row := func(n int) int { return 1 + (1 + hs[0]) + (1 + hs[1]) + 1 + n }
	// A click on the third session row of the (unfocused) items panel
	// focuses it and selects that row.
	h.send(tea.MouseClickMsg{X: 4, Y: row(2), Button: tea.MouseLeft})
	if ws.focus != panelItems {
		t.Fatalf("a click should focus the panel under it, got %d", ws.focus)
	}
	if r, ok := wsCursor(h, panelItems).(*sessionRow); !ok || r.s.ID != sid3 {
		t.Errorf("cursor row = %+v, want the third session", wsCursor(h, panelItems))
	}
	wantAll(t, h.view(), "first prompt of three", "session "+sid3[:8])

	// The wheel over the same panel moves its cursor without changing
	// focus; over the preview it scrolls the pane.
	h.send(tea.MouseWheelMsg{X: 4, Y: row(1), Button: tea.MouseWheelUp})
	if r, ok := wsCursor(h, panelItems).(*sessionRow); !ok || r.s.ID != sid1 {
		t.Errorf("wheel up should move the cursor to the top, got %+v", wsCursor(h, panelItems))
	}
	// Over the preview the wheel scrolls the pane (a short window, so
	// the session preview overflows it) and never moves focus.
	h.send(tea.WindowSizeMsg{Width: 400, Height: 18})
	mainX := ws.sideWidth() + 2 + paneGap + 2
	before := ws.vp.YOffset()
	h.send(tea.MouseWheelMsg{X: mainX, Y: 4, Button: tea.MouseWheelDown})
	if ws.vp.YOffset() <= before {
		t.Errorf("the wheel over the preview should scroll it: %d → %d", before, ws.vp.YOffset())
	}
	if ws.focus != panelItems || ws.mainFocus {
		t.Errorf("the wheel must not move focus: focus=%d main=%v", ws.focus, ws.mainFocus)
	}
	h.send(tea.WindowSizeMsg{Width: 400, Height: 40})
	hs = ws.panelHeights()

	// A click on the preview hands it the keys; esc gives them back.
	h.send(tea.MouseClickMsg{X: ws.sideWidth() + 2 + paneGap + 2, Y: 6, Button: tea.MouseLeft})
	if !ws.mainFocus {
		t.Fatal("a click on the preview should focus it")
	}
	h.keys("esc")
	if ws.mainFocus {
		t.Error("esc should return to the panels")
	}

	// A click on a panel's title line focuses it without touching its
	// cursor; the gutter between the boxes is inert.
	h.send(tea.MouseClickMsg{X: 4, Y: 1, Button: tea.MouseLeft})
	if ws.focus != panelAccounts {
		t.Errorf("a click on the accounts title should focus it, got %d", ws.focus)
	}
	h.send(tea.MouseClickMsg{X: ws.sideWidth() + 2, Y: 5, Button: tea.MouseLeft})
	if ws.focus != panelAccounts {
		t.Errorf("the gutter should be inert, focus moved to %d", ws.focus)
	}

	// Over an overlay the mouse belongs to the overlay: the wheel
	// scrolls the transcript, a click on the frame behind does nothing.
	h.keys("3", "enter")
	tv, ok := h.a.top().(*transcriptScreen)
	if !ok {
		t.Fatalf("enter should open the transcript, got %T", h.a.top())
	}
	h.send(tea.MouseClickMsg{X: 4, Y: row(1), Button: tea.MouseLeft})
	if h.a.top() != tv {
		t.Error("a click behind an overlay must not reach the frame")
	}
	h.keys("esc")

	// In the menu a click runs the item under the pointer.
	h.keys("x")
	h.send(tea.MouseClickMsg{X: ws.sideWidth() + 6, Y: 2 + menuTop + 1, Button: tea.MouseLeft})
	if _, ok := h.a.top().(*receiveScreen); !ok {
		t.Fatalf("a click on the second menu item should open receive, got %T", h.a.top())
	}
}
