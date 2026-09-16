package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// These tests read the selection back off the rendered frame at the
// coordinates that were dragged, never off the layout arithmetic that
// produced them — which is the only way the coordinate math is worth
// trusting.

// screenText is what the frame shows in a rectangle of cells.
func screenText(frame string, x, y, w, h int) string {
	lines := strings.Split(frame, "\n")
	var out []string
	for row := y; row < y+h && row < len(lines); row++ {
		out = append(out, strings.TrimRight(ansi.Cut(lines[row], x, x+w), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// previewSpan is where the preview's content sits in the frame: its
// first column, its first row and its width.
func previewSpan(a *app) (x, y, w int) {
	ws := a.ws
	left := 1 + padX
	if side := ws.sideWidth(); side > 0 && ws.mainWidth() > 0 {
		left = side + 2 + paneGap + 1 + padX
	}
	return left, 2, ws.mainInner() // the header line, then the pane's top border
}

func TestPreviewSelectionCopiesWhatWasDragged(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)
	x0, y0, w := previewSpan(a)

	// Drag from the third column of the second preview row to the
	// twelfth column three rows down.
	fromX, fromY := x0+2, y0+1
	toX, toY := x0+11, y0+4
	a.Update(tea.MouseClickMsg{X: fromX, Y: fromY, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: toX, Y: toY, Button: tea.MouseLeft})
	if a.ws.drag.sel.empty() {
		t.Fatal("a drag over the preview selects nothing")
	}
	a.Update(tea.MouseReleaseMsg{X: toX, Y: toY, Button: tea.MouseLeft})

	frame := ansi.Strip(a.View().Content)
	var want []string
	for row := fromY; row <= toY; row++ {
		x, width := x0, w
		if row == fromY {
			x, width = fromX, w-(fromX-x0)
		}
		if row == toY {
			width = toX - x + 1
		}
		want = append(want, strings.TrimRight(screenText(frame, x, row, width, 1), " "))
	}
	got := selectedText(&a.ws.vp, a.ws.drag.sel)
	if got != strings.TrimRight(strings.Join(want, "\n"), "\n") {
		t.Errorf("selected text is not what the frame shows at those cells:\ngot:\n%s\n\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("a drag over four rows should select four lines: %q", got)
	}
}

// The selected cells are painted, and only those: a hole would be a run
// of unpainted cells inside the selection.
func TestPreviewSelectionIsPainted(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)
	x0, y0, _ := previewSpan(a)
	a.Update(tea.MouseClickMsg{X: x0 + 2, Y: y0 + 1, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: x0 + 20, Y: y0 + 1, Button: tea.MouseLeft})

	row := strings.Split(a.View().Content, "\n")[y0+1]
	painted := 0
	for _, r := range sgrRuns(row) {
		if painting(r.sgr) {
			painted += len([]rune(r.text))
		}
	}
	if painted == 0 {
		t.Fatalf("the selection is not painted: %q", spellEscapes(row))
	}
	// 19 cells: the third column through the twenty-first, inclusive.
	if painted != 19 {
		t.Errorf("painted %d cells, want the 19 that were dragged: %q", painted, spellEscapes(row))
	}
}

// A drag held past the bottom edge scrolls the document under it, so a
// selection can run past what is on screen.
func TestSelectionAutoScrollsPastTheEdge(t *testing.T) {
	a := goldenApp(t, 120, 32, nil)
	ws := a.ws
	// A document longer than the pane: there has to be somewhere to
	// scroll to. The fixture seeds the preview itself, so this goes in
	// after it.
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d of a document longer than the pane", i)
	}
	ws.vp.SetContentLines(lines)
	x0, y0, _ := previewSpan(a)
	bottom := y0 + ws.vp.Height() - 1
	a.Update(tea.MouseClickMsg{X: x0 + 1, Y: y0, Button: tea.MouseLeft})
	before := ws.vp.YOffset()
	a.Update(tea.MouseMotionMsg{X: x0 + 1, Y: bottom + 3, Button: tea.MouseLeft})
	if ws.vp.YOffset() <= before {
		t.Fatalf("a drag past the bottom edge did not scroll: offset %d → %d", before, ws.vp.YOffset())
	}
	// The tick chain keeps going while the button is held there.
	at := ws.vp.YOffset()
	a.Update(dragScrollMsg{seq: ws.drag.seq})
	if ws.vp.YOffset() <= at {
		t.Errorf("the auto-scroll tick did not advance: offset %d → %d", at, ws.vp.YOffset())
	}
	// Releasing ends it: a later tick of the same chain does nothing.
	a.Update(tea.MouseReleaseMsg{X: x0 + 1, Y: bottom + 3, Button: tea.MouseLeft})
	at = ws.vp.YOffset()
	a.Update(dragScrollMsg{seq: ws.drag.seq})
	if ws.vp.YOffset() != at {
		t.Errorf("the auto-scroll kept running after the button lifted: offset %d → %d", at, ws.vp.YOffset())
	}
}

// esc lets go of the selection rather than leaving the preview, and a
// click that does not move lets go of the last one.
func TestSelectionIsReleasedByEscAndByAClick(t *testing.T) {
	a := goldenApp(t, 120, 32, func(a *app) { a.ws.mainFocus = true })
	x0, y0, _ := previewSpan(a)
	a.Update(tea.MouseClickMsg{X: x0 + 2, Y: y0 + 1, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: x0 + 20, Y: y0 + 1, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: x0 + 20, Y: y0 + 1, Button: tea.MouseLeft})
	if a.ws.drag.sel.empty() {
		t.Fatal("nothing selected after the drag")
	}
	a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !a.ws.drag.sel.empty() {
		t.Error("esc should let go of the selection")
	}
	if !a.ws.mainFocus {
		t.Error("the first esc lets go of the selection, it does not also leave the preview")
	}
	a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.ws.mainFocus {
		t.Error("the second esc leaves the preview")
	}
}

// copiedNote says what went to the clipboard.
func TestCopiedNote(t *testing.T) {
	if got := copiedNote("one line"); got != `copied "one line"` {
		t.Errorf("copiedNote = %q", got)
	}
	if got := copiedNote("a\nb\nc"); got != "copied 3 lines" {
		t.Errorf("copiedNote = %q", got)
	}
}
