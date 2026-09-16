package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// A note says what happened in the tone of what happened, leads with a
// glyph so the tone is not the only signal, and takes itself down.
func TestNoteKindsAndExpiry(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)

	a.Update(statusMsg{text: "copied 3 lines", kind: noteDone})
	if a.statusKind != noteDone {
		t.Fatalf("kind = %v", a.statusKind)
	}
	if got := ansi.Strip(a.View().Content); !strings.Contains(got, glyph.ok+" copied 3 lines") {
		t.Errorf("a finished note does not lead with its mark:\n%s", lastLines(got, 3))
	}
	a.Update(statusMsg{text: "nothing here to copy", kind: noteWarn})
	if got := ansi.Strip(a.View().Content); !strings.Contains(got, glyph.missing+" nothing here to copy") {
		t.Errorf("a refusal does not lead with its mark:\n%s", lastLines(got, 3))
	}

	// A timer started for an earlier note never clears a later one.
	at := a.statusAt
	a.Update(statusMsg{text: "a newer note"})
	a.Update(statusOutMsg{at: at})
	if a.status != "a newer note" {
		t.Errorf("a stale timer cleared the newer note: %q", a.status)
	}
	a.Update(statusOutMsg{at: a.statusAt})
	if a.status != "" {
		t.Errorf("the note outlived its own timer: %q", a.status)
	}
}

// A toast floats over the frame and leaves by itself; the sequence is
// what keeps an older timer from taking down a newer card.
func TestToastAppearsAndExpires(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)
	a.Update(toastMsg{kind: noteDone, title: "claude exited", body: []string{"resumed 1e005053"}})
	frame := ansi.Strip(a.View().Content)
	if !strings.Contains(frame, "claude exited") || !strings.Contains(frame, "resumed 1e005053") {
		t.Fatalf("no toast in the frame:\n%s", frame)
	}
	// It sits at the bottom right, above the status line, and covers
	// nothing at the top of the pane.
	lines := strings.Split(frame, "\n")
	for i, l := range lines {
		if strings.Contains(l, "claude exited") && i < len(lines)/2 {
			t.Errorf("the toast is at line %d of %d, not near the bottom", i, len(lines))
		}
	}
	seq := a.toast.seq
	a.Update(toastMsg{kind: noteInfo, title: "a newer notice"})
	a.Update(toastOutMsg{seq: seq})
	if a.toast.title != "a newer notice" {
		t.Errorf("a stale timer took down the newer toast: %q", a.toast.title)
	}
	a.Update(toastOutMsg{seq: a.toast.seq})
	if a.toast.title != "" {
		t.Errorf("the toast outlived its own timer: %q", a.toast.title)
	}
}

// The frame is still a frame with a toast on it: nothing wider than the
// terminal, nothing taller.
func TestToastFitsEverySize(t *testing.T) {
	for _, size := range frameSizes {
		w, h := size[0], size[1]
		frame := frameAt(t, w, h, func(a *app) {
			_ = a.notify(noteWarn, "a notice with a title long enough to need wrapping in a narrow pane",
				"and a body that is longer still, which is what a transfer's verdict looks like")
		})
		for i, l := range strings.Split(frame, "\n") {
			if got := ansi.StringWidth(l); got > w {
				t.Errorf("at %dx%d line %d is %d cells: %q", w, h, i, got, l)
			}
		}
	}
}

// A key clears the note it was the answer to, so the next key never
// reads someone else's answer.
func TestAKeyClearsTheNote(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)
	a.Update(statusMsg{text: "copied", kind: noteDone})
	a.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if a.status != "" {
		t.Errorf("the note survived the next key: %q", a.status)
	}
}

// lastLines is the tail of a frame, for a failure message.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
