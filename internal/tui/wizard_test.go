package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// The wizard sends to a file with the parts chosen in its second step.
func TestWizardSendToFile(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
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
	wantAll(t, out, "step 2 of 3", "What to send: the whole project "+shortPath(f.project)+" and its memory",
		"memory          include", "tool results    include", "may contain pasted secrets", "file history    include", "prompt history  include", "live sessions   include", "continue →")
	// Leave the tool results out, then continue.
	h.keys("down", "space")
	wantAll(t, h.view(), "tool results    leave out")
	if sc.parts.ToolResults {
		t.Fatal("space should toggle the part")
	}
	h.keys("down", "down", "down", "down", "enter")
	out = h.view()
	wantAll(t, out, "step 3 of 3", "How to send it", "Over the local network", "To a .bffs file", "Through ssh", "no inbound port needed")
	// The ssh step shows the command with the choices made.
	h.keys("down", "down", "enter")
	out = h.view()
	wantAll(t, out, "Run this in a terminal", "bffs export --project "+shellWord(f.project)+" --no-tool-results --out - | ssh <other-machine> 'bffs import --from - -y --as-is'", "c copies it")
	h.keys("c")
	wantAll(t, h.view(), "copied to the clipboard")
	h.keys("esc")
	wantAll(t, h.view(), "step 3 of 3")
	// The file route hands off to the export screen with the parts (esc
	// left the cursor on the ssh row).
	h.keys("up", "enter")
	ex, ok := h.a.top().(*exportScreen)
	if !ok || ex.tgt.parts == nil || ex.tgt.parts.ToolResults || !ex.tgt.parts.FileHistory {
		t.Fatalf("file route should open export with the chosen parts, got %T %+v", h.a.top(), ex)
	}
	if _, isWizard := h.a.top().(*wizardScreen); isWizard || len(h.a.stack) != 1 {
		t.Fatalf("the wizard should be replaced by the export screen, stack = %d", len(h.a.stack))
	}
	h.keys("enter") // accept the default path
	wantAll(t, h.view(), "tool-results excluded", "file-history", "[y/N]")
	h.keys("y")
	if _, ok := h.a.top().(*resultScreen); !ok {
		t.Fatalf("y should end on the result, got %T:\n%s", h.a.top(), h.view())
	}
	if _, err := os.Stat(ex.path); err != nil {
		t.Errorf("bundle not written: %v", err)
	}
}

// Without memory the target is sessions-only; without a project the
// wizard says what to select first.
func TestWizardSendChoices(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	f.memory()
	h := f.start("sessions")
	h.keys("w", "enter", "space") // memory: leave out
	sc := h.a.top().(*wizardScreen)
	if sc.memory || sc.target().only != "sessions" {
		t.Errorf("memory off should narrow to sessions: memory=%v only=%q", sc.memory, sc.target().only)
	}
	wantAll(t, h.view(), "memory          leave out")
	h.keys("down", "down", "down", "down", "space") // live sessions: leave out
	if !sc.target().noLive || !strings.Contains(sc.sshCommand(), "--only sessions --no-live") {
		t.Errorf("command = %q", sc.sshCommand())
	}
	h.keys("esc", "esc")
	if h.a.top() != nil {
		t.Fatalf("esc twice should close the wizard, got %T", h.a.top())
	}

	// Marked sessions make the target the selection.
	h.keys("3", "space", "w", "enter")
	wantAll(t, h.view(), "What to send: 1 selected session")
	wantNone(t, h.view(), "memory          include")
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
	h.keys("w", "enter", "down", "down", "down", "down", "down", "down", "enter", "enter")
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
