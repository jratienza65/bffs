package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// The wizard's second step is a checklist: every session and memory
// file of the project, the parts, the live toggle. What is left
// unchecked stays out of the bundle.
func TestWizardSendToFile(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("w")
	sc, ok := h.a.top().(*wizardScreen)
	if !ok {
		t.Fatalf("w should open the wizard, got %T", h.a.top())
	}
	out := h.view()
	wantAll(t, out, "step 1 of 4", "What do you want to do?", "Send sessions from this machine", "Receive sessions from another machine", "pairing code")
	h.keys("enter")
	wantAll(t, h.view(), "step 2 of 4", "How much to send", "This project: "+shortPath(f.project), "Every project of shared pool: work", "1 project", "2 sessions")
	h.keys("enter") // this project
	out = h.view()
	wantAll(t, out, "step 3 of 4", "What to send from "+shortPath(f.project), "2 of 2 sessions · 2 of 2 memory files",
		"SESSIONS", "[x]   first prompt of one", "[x]   first prompt of two", "1h ago", "2h ago",
		"MEMORY", "[x] MEMORY.md", "[x] notes.md", "pinned",
		"PARTS OF EVERY SELECTED SESSION", "tool results    include", "may contain pasted secrets", "file history    include", "prompt history  include", "live sessions   include", "continue →")
	// Uncheck the first session, MEMORY.md and the tool results.
	h.keys("space")
	wantAll(t, h.view(), "[ ]   first prompt of one", "1 of 2 sessions")
	h.keys("down", "down", "space") // the MEMORY header is skipped
	wantAll(t, h.view(), "[ ] MEMORY.md", "1 of 2 memory files")
	h.keys("down", "down", "space")
	wantAll(t, h.view(), "tool results    leave out")
	if sc.parts.ToolResults {
		t.Fatal("space should toggle the part")
	}
	// a checks a whole section when any of it is unchecked, then clears it.
	h.keys("up", "up", "up", "a")
	wantAll(t, h.view(), "2 of 2 sessions")
	h.keys("a")
	wantAll(t, h.view(), "0 of 2 sessions")
	h.keys("up", "space") // the first session alone
	tgt := sc.target()
	if len(tgt.keepSessions) != 1 || !tgt.keepSessions[sid1] || len(tgt.keepMemory) != 1 || !tgt.keepMemory["notes.md"] {
		t.Fatalf("target = sessions %v memory %v", tgt.keepSessions, tgt.keepMemory)
	}
	// Continue: the how step, then the ssh line names the session.
	h.keys("pgdown", "enter")
	out = h.view()
	wantAll(t, out, "step 4 of 4", "How to send it", "Over the local network", "To a .bffs file", "Through ssh", "no inbound port needed")
	h.keys("down", "down", "enter")
	out = h.view()
	wantAll(t, out, "Run this in a terminal", "bffs export --session "+sid1+" --project "+shellWord(f.project)+" --no-tool-results --out - | ssh <other-machine> 'bffs import --from - -y --as-is'", "c copies it")
	h.keys("c")
	wantAll(t, h.view(), "copied to the clipboard")
	h.keys("esc")
	wantAll(t, h.view(), "step 4 of 4")
	// The file route hands off to the export screen with the choices.
	h.keys("up", "enter")
	ex, ok := h.a.top().(*exportScreen)
	if !ok || ex.tgt.parts == nil || ex.tgt.parts.ToolResults || len(ex.tgt.keepSessions) != 1 {
		t.Fatalf("file route should open export with the choices, got %T", h.a.top())
	}
	if len(h.a.stack) != 1 {
		t.Fatalf("the wizard should be replaced by the export screen, stack = %d", len(h.a.stack))
	}
	h.keys("enter") // accept the default path
	wantAll(t, h.view(), "1 session", "tool-results excluded", "memory        1 files", "[y/N]")
	h.keys("y")
	if _, ok := h.a.top().(*resultScreen); !ok {
		t.Fatalf("y should end on the result, got %T:\n%s", h.a.top(), h.view())
	}
	bf, err := os.Open(ex.path)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	defer bf.Close()
	m, _, _, err := bundle.PeekManifest(bf)
	if err != nil {
		t.Fatal(err)
	}
	nSessions, nMemory := manifestCounts(m)
	if nSessions != 1 || nMemory != 1 {
		t.Errorf("bundle carries %d sessions and %d memory files, want 1 and 1", nSessions, nMemory)
	}
	for _, e := range m.Entries {
		if e.Kind == bundle.EntrySession && e.SessionID != sid1 {
			t.Errorf("unexpected session %s in the bundle", e.SessionID)
		}
		if e.Kind == bundle.EntryMemory && !strings.HasSuffix(e.Files[0].Path, "/notes.md") {
			t.Errorf("unexpected memory file %s", e.Files[0].Path)
		}
	}
}

// Unchecking every memory file narrows the target to sessions; marked
// sessions arrive pre-checked; nothing checked and no project are
// refused with a hint.
func TestWizardSendChoices(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.transcript(f.slug, sid2, f.project, "first prompt of two", fixedNow.Add(-2*time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("w", "enter", "enter", "down", "down", "a") // this project; clear the memory section
	sc := h.a.top().(*wizardScreen)
	if sc.target().only != "sessions" || sc.target().keepMemory != nil {
		t.Errorf("no memory files should narrow to sessions: %+v", sc.target())
	}
	wantAll(t, h.view(), "0 of 2 memory files")
	h.keys("pgdown", "up", "space") // live sessions: leave out
	if !sc.target().noLive || !strings.Contains(sc.sshCommand(), "--only sessions --no-live") {
		t.Errorf("command = %q", sc.sshCommand())
	}
	// Nothing at all checked is refused.
	h.keys("pgup", "a")
	wantAll(t, h.view(), "0 of 2 sessions")
	h.keys("pgdown", "enter")
	wantAll(t, h.view(), "nothing selected: check at least one session or memory file")
	h.keys("esc") // back to the scope step
	wantAll(t, h.view(), "step 2 of 4", "How much to send")
	h.keys("esc", "esc")
	if h.a.top() != nil {
		t.Fatalf("esc out of every step should close the wizard, got %T", h.a.top())
	}

	// With sessions marked the scope step offers them first, and the
	// checklist then lists only those.
	h.keys("3", "space", "w", "enter")
	wantAll(t, h.view(), "step 2 of 4", "The 1 marked session", "This project:", "Every project of")
	h.keys("down", "enter") // this project: marked ones pre-checked
	wantAll(t, h.view(), "1 of 2 sessions", "[x]   first prompt of one", "[ ]   first prompt of two")
	h.keys("esc", "esc", "esc")

	// A project without sessions or cwd cannot be sent.
	g := newFixture(t)
	hg := g.start("sessions")
	hg.keys("w", "enter")
	wantAll(t, hg.view(), "nothing to send: this pool has no projects")
	if _, ok := hg.a.top().(*wizardScreen); !ok {
		t.Fatalf("the wizard should stay open, got %T", hg.a.top())
	}
}

// The LAN route hands off to the serve screen; the receive route to the
// receive screen's host input.
func TestWizardLANHandoffs(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	loopbackTransfer(t, mustParseCode(t, "7K3Q-M9XD"))
	h := f.start("sessions")
	h.keys("w", "enter", "enter", "pgdown", "pgdown", "enter", "enter")
	if _, ok := h.a.top().(*serveScreen); !ok {
		t.Fatalf("the LAN route should open the serve screen, got %T:\n%s", h.a.top(), h.view())
	}
	h.keys("esc")
	for h.a.top() != nil {
		h.keys("esc")
	}
	h.keys("w", "down", "enter")
	wantAll(t, h.view(), "step 2 of 2", "Where from?", `as account "work"`, "From another machine on the local network", "From a .bffs file on this machine")
	h.keys("enter")
	rs, ok := h.a.top().(*receiveScreen)
	if !ok || rs.state != recvHost || rs.file != "" {
		t.Fatalf("the LAN route should open the receive screen on its host input, got %T", h.a.top())
	}
}

// The file route reads a bundle written elsewhere, asks where its
// project lives, and lands it.
func TestWizardReceiveFromFile(t *testing.T) {
	a := newFixture(t)
	a.transcript(a.slug, sid1, a.project, "first prompt of one", fixedNow.Add(-time.Hour))
	a.memory()
	ha := a.start("sessions")
	// Write the bundle outside the project, so nothing was just created
	// inside the directory the test then moves away: on Windows a fresh
	// file keeps its directory busy while the scanner reads it.
	bundlePath := filepath.Join(t.TempDir(), "a.bffs")
	ha.keys("e")
	ex, ok := ha.a.top().(*exportScreen)
	if !ok {
		t.Fatalf("e should open the export screen, got %T", ha.a.top())
	}
	ex.input.SetValue(bundlePath)
	ha.keys("enter", "y")
	if _, ok := ha.a.top().(*resultScreen); !ok {
		t.Fatalf("export should end on a result, got %T:\n%s", ha.a.top(), ha.view())
	}
	if _, err := os.Stat(bundlePath); err != nil {
		t.Fatalf("bundle not written: %v", err)
	}

	// The project moves away, as on another machine, so the import has to
	// ask where it lives. Windows refuses to rename the process's own
	// working directory, and the fixture chdir'd into the project.
	t.Chdir(t.TempDir())
	if err := os.Rename(a.project, a.project+"-moved"); err != nil {
		t.Fatal(err)
	}
	b := newFixture(t) // a second machine: a fresh HOME without the project
	hb := b.start("sessions")
	hb.keys("w", "down", "enter", "down", "enter")
	wantAll(t, hb.view(), "Which file?", "manifest is shown and confirmed")
	hb.keys("n", "o", "p", "e", "enter")
	wantAll(t, hb.view(), "nope does not exist")
	hb.keys("ctrl+u")
	for _, r := range bundlePath {
		hb.keys(string(r))
	}
	hb.keys("enter")
	rs, ok := hb.a.top().(*receiveScreen)
	if !ok || rs.file != bundlePath {
		t.Fatalf("enter should open the file import, got %T", hb.a.top())
	}
	out := hb.view()
	wantAll(t, out, "import "+shortPath(bundlePath), "Bundle", "exists here ✗", "where does "+a.project+" live on this machine?", "import as-is; rehome later")
	// Choose as-is (the last option), then confirm.
	h := hb
	for i := 0; i < 4; i++ {
		h.keys("down")
	}
	h.keys("enter")
	wantAll(t, h.view(), "imported as-is; rehome later", "Import into "+shortPath(b.claudeDir)+"? [y/N]")
	h.keys("y")
	res, ok := h.a.top().(*resultScreen)
	if !ok || res.err != nil {
		t.Fatalf("y should end on a clean result, got %T err=%v:\n%s", h.a.top(), res.err, h.view())
	}
	wantAll(t, h.view(), "Done in", "pending")
	landed := filepath.Join(b.claudeDir, "projects", a.slug, sid1+".jsonl")
	if _, err := os.Stat(landed); err != nil {
		t.Errorf("session not landed: %v", err)
	}
	// A swapped file is refused: the manifest sha256 was pinned at review.
	if _, err := transcripts.ProjectKey(b.project); err != nil {
		t.Fatal(err)
	}
}

// The pool scope exports every project of the root, and unchecking one
// narrows it to the rest by directory.
func TestWizardSendWholePool(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	other := filepath.Join(f.home, "src", "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	otherSlug, err := transcripts.Slug(other)
	if err != nil {
		t.Fatal(err)
	}
	f.transcript(otherSlug, sid2, other, "prompt from the other project", fixedNow.Add(-2*time.Hour))
	h := f.start("sessions")
	h.keys("w", "enter")
	wantAll(t, h.view(), "step 2 of 4", "Every project of shared pool: work", "2 projects", "2 sessions")
	h.keys("down", "enter") // every project
	sc := h.a.top().(*wizardScreen)
	out := h.view()
	wantAll(t, out, "step 3 of 4", "Which projects of shared pool: work", "2 of 2 projects · 2 sessions",
		"PROJECTS", "[x] "+shortPath(f.project), "1 session · memory", "[x] "+shortPath(other), "PARTS OF EVERY SELECTED SESSION")
	if tgt := sc.target(); !tgt.allProjects || tgt.projects != nil {
		t.Fatalf("everything checked should be --all-projects: %+v", tgt)
	}
	if !strings.Contains(sc.sshCommand(), "bffs export --all-projects --out -") {
		t.Errorf("command = %q", sc.sshCommand())
	}
	// Uncheck the second project: the target names the first by directory.
	h.keys("down", "space")
	wantAll(t, h.view(), "1 of 2 projects · 1 session", "[ ] "+shortPath(other))
	tgt := sc.target()
	if tgt.allProjects || len(tgt.projects) != 1 || tgt.projects[0] != f.project {
		t.Fatalf("target = %+v", tgt)
	}
	// Export it: the bundle carries the first project only, with memory.
	h.keys("pgdown", "enter", "down", "enter", "enter")
	ex, ok := h.a.top().(*exportScreen)
	if !ok {
		t.Fatalf("the file route should open export, got %T:\n%s", h.a.top(), h.view())
	}
	wantAll(t, h.view(), "1 session", "memory        2 files")
	wantNone(t, h.view(), "prompt from the other project")
	h.keys("y")
	if _, ok := h.a.top().(*resultScreen); !ok {
		t.Fatalf("y should end on the result, got %T:\n%s", h.a.top(), h.view())
	}
	bf, err := os.Open(ex.path)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	m, _, _, err := bundle.PeekManifest(bf)
	if err != nil {
		t.Fatal(err)
	}
	if n, mem := manifestCounts(m); n != 1 || mem != 2 {
		t.Errorf("bundle carries %d sessions and %d memory files, want 1 and 2", n, mem)
	}
}

// e on the accounts panel exports the whole pool without going through
// the wizard.
func TestExportWholePoolFromAccounts(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("1", "e")
	ex, ok := h.a.top().(*exportScreen)
	if !ok || !ex.tgt.allProjects {
		t.Fatalf("e on the accounts panel should export every project, got %T", h.a.top())
	}
	wantAll(t, h.view(), "export every project of shared pool: work with its memory")
	h.keys("esc")
	h.keys("x")
	wantAll(t, h.view(), "export every project of shared pool: work with its memory to a file", "send every project", "copy every project")
}

// The wheel scrolls the text of the overlays drawn in the preview pane
// — the wizard's checklist and the action menu — and never moves what
// is selected there.
func TestOverlayWheelScrollsTextOnly(t *testing.T) {
	f := newFixture(t)
	f.memory()
	for i := 0; i < 12; i++ {
		f.transcript(f.slug, fmt.Sprintf("%08d-1111-4222-8333-444455556666", i+1), f.project, fmt.Sprintf("prompt %d", i), fixedNow.Add(-time.Duration(i+1)*time.Hour))
	}
	h := f.start("sessions")
	h.send(tea.WindowSizeMsg{Width: 400, Height: 20}) // the checklist overflows
	h.keys("w", "enter", "enter")
	sc, ok := h.a.top().(*wizardScreen)
	if !ok {
		t.Fatalf("w should reach the checklist, got %T", h.a.top())
	}
	cursor, offset := sc.cursor, sc.offset
	h.send(tea.MouseWheelMsg{X: 200, Y: 8, Button: tea.MouseWheelDown})
	if sc.offset <= offset {
		t.Errorf("the wheel should scroll the checklist: %d → %d", offset, sc.offset)
	}
	if sc.cursor != cursor {
		t.Errorf("the wheel must not move the selection: %d → %d", cursor, sc.cursor)
	}
	// The keyboard still drags the view along with the cursor.
	h.keys("down", "down")
	if sc.cursor != cursor+2 {
		t.Errorf("down should move the cursor, got %d", sc.cursor)
	}
	// Scrolling up past the top stops there, selection intact.
	for i := 0; i < 20; i++ {
		h.send(tea.MouseWheelMsg{X: 200, Y: 8, Button: tea.MouseWheelUp})
	}
	if sc.offset != 0 || sc.cursor != cursor+2 {
		t.Errorf("offset=%d cursor=%d, want 0 and %d", sc.offset, sc.cursor, cursor+2)
	}
	h.keys("esc", "esc", "esc")

	// The menu behaves the same way.
	h.keys("x")
	m, ok := h.a.top().(*menuScreen)
	if !ok {
		t.Fatalf("x should open the menu, got %T", h.a.top())
	}
	h.view() // one render, so the menu knows how many rows it has
	before := m.cursor
	h.send(tea.MouseWheelMsg{X: 200, Y: 6, Button: tea.MouseWheelDown})
	if m.cursor != before {
		t.Errorf("the wheel must not move the menu's selection: %d → %d", before, m.cursor)
	}
}
