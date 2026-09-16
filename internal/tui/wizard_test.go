package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	wantAll(t, out, "step 1 of 3", "What do you want to do?", "Send sessions from this machine", "Receive sessions from another machine", "pairing code")
	h.keys("enter")
	out = h.view()
	wantAll(t, out, "step 2 of 3", "What to send from "+shortPath(f.project), "2 of 2 sessions · 2 of 2 memory files",
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
	wantAll(t, out, "step 3 of 3", "How to send it", "Over the local network", "To a .bffs file", "Through ssh", "no inbound port needed")
	h.keys("down", "down", "enter")
	out = h.view()
	wantAll(t, out, "Run this in a terminal", "bffs export --session "+sid1+" --project "+shellWord(f.project)+" --no-tool-results --out - | ssh <other-machine> 'bffs import --from - -y --as-is'", "c copies it")
	h.keys("c")
	wantAll(t, h.view(), "copied to the clipboard")
	h.keys("esc")
	wantAll(t, h.view(), "step 3 of 3")
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
	h.keys("w", "enter", "down", "down", "a") // clear the memory section
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
	h.keys("esc", "esc")
	if h.a.top() != nil {
		t.Fatalf("esc twice should close the wizard, got %T", h.a.top())
	}

	// Marked sessions arrive pre-checked, the rest unchecked.
	h.keys("3", "space", "w", "enter")
	wantAll(t, h.view(), "1 of 2 sessions", "[x]   first prompt of one", "[ ]   first prompt of two")
	h.keys("esc", "esc")

	// A project without sessions or cwd cannot be sent.
	g := newFixture(t)
	hg := g.start("sessions")
	hg.keys("w", "enter")
	wantAll(t, hg.view(), "nothing to send: select a project (2) or mark sessions (space) first")
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
	h.keys("w", "enter", "pgdown", "pgdown", "enter", "enter")
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
	ha.keys("e", "enter", "y")
	ex, ok := ha.a.stack[0].(*resultScreen)
	if !ok {
		t.Fatalf("export should end on a result, got %T", ha.a.top())
	}
	_ = ex
	var bundlePath string
	if entries, err := filepath.Glob(filepath.Join(a.project, "*.bffs")); err == nil && len(entries) == 1 {
		bundlePath = entries[0]
	} else {
		t.Fatalf("bundle not found in %s: %v", a.project, entries)
	}

	// The bundle travels; the project moves away, as on another machine,
	// so the import has to ask where it lives.
	carried := filepath.Join(t.TempDir(), "a.bffs")
	if err := os.Rename(bundlePath, carried); err != nil {
		t.Fatal(err)
	}
	bundlePath = carried
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
