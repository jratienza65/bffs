package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The browser takes the mouse, so the terminal's own selection needs
// shift held. That is a poor trade for a pane whose whole purpose is
// text worth copying — a resume command, a path, a memory file — so the
// browser does the selection itself: drag over a document to select the
// cells under the pointer, painted in reverse video; releasing copies
// through OSC 52, which reaches the local clipboard even over SSH; y
// copies again and esc lets go.
//
// The selection lives in document coordinates (line and column of the
// content, not of the screen), so it stays on its text while the
// document scrolls under it.

// cellPos is one cell of a document.
type cellPos struct{ line, col int }

func (c cellPos) before(o cellPos) bool {
	return c.line < o.line || (c.line == o.line && c.col < o.col)
}

// selection is a range of cells between the two ends of a drag.
type selection struct {
	anchor, head cellPos
	active       bool
	dragging     bool
}

func (s selection) empty() bool { return !s.active || s.anchor == s.head }

// bounds orders the two ends, since a drag can run either way.
func (s selection) bounds() (cellPos, cellPos) {
	if s.head.before(s.anchor) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// dragScrollMsg is one tick of a drag held past an edge.
type dragScrollMsg struct{ seq int }

// dragTick paces the auto-scroll: twenty lines a second.
const dragTick = 50 * time.Millisecond

func dragScroll(seq int) tea.Cmd {
	return tea.Tick(dragTick, func(time.Time) tea.Msg { return dragScrollMsg{seq: seq} })
}

// dragSelect is the state of selecting in one viewport: what is
// selected, and — while a drag is held past an edge — which edge, the
// column it is held at, and which tick chain is current.
type dragSelect struct {
	sel   selection
	edge  int
	dragX int
	seq   int
}

// press starts a selection at a position in the viewport's content.
func (d *dragSelect) press(vp *viewport.Model, x, y int) {
	c := cellAt(vp, x, y)
	d.sel = selection{anchor: c, head: c, active: true, dragging: true}
	d.edge, d.dragX = 0, x
}

// motion extends a held selection. Past the top or bottom edge it
// scrolls instead: by the overshoot at once, then a line per tick for
// as long as the button is held there, so a selection can run past what
// is on screen the way it does in a terminal.
func (d *dragSelect) motion(vp *viewport.Model, x, y int) tea.Cmd {
	if !d.sel.dragging {
		return nil
	}
	d.dragX = x
	edge, over := 0, 0
	switch {
	case y < 0:
		edge, over = -1, -y
	case y >= vp.Height():
		edge, over = 1, y-vp.Height()+1
	}
	was := d.edge
	d.edge = edge
	if edge == 0 {
		d.sel.head = cellAt(vp, x, y)
		return nil
	}
	d.scroll(vp, over)
	if was == edge {
		return nil // the tick chain for this edge is already running
	}
	d.seq++
	return dragScroll(d.seq)
}

// step is one tick of the auto-scroll: a line in the held direction,
// and another tick — until the button lifts, the pointer comes back
// inside, or the document has no more to show that way.
func (d *dragSelect) step(vp *viewport.Model, msg dragScrollMsg) tea.Cmd {
	if msg.seq != d.seq || !d.sel.dragging || d.edge == 0 {
		return nil
	}
	if (d.edge < 0 && vp.AtTop()) || (d.edge > 0 && vp.AtBottom()) {
		d.edge = 0 // nothing left that way; a flip starts a new chain
		return nil
	}
	d.scroll(vp, 1)
	return dragScroll(msg.seq)
}

// scroll moves the document n lines in the held direction and puts the
// head at the edge row, under the pointer's column.
func (d *dragSelect) scroll(vp *viewport.Model, n int) {
	y := 0
	if d.edge < 0 {
		vp.ScrollUp(n)
	} else {
		vp.ScrollDown(n)
		y = max(0, vp.Height()-1)
	}
	d.sel.head = cellAt(vp, d.dragX, y)
}

// release ends a drag and copies what it covered. A click that never
// moved selects nothing, and lets go of whatever was selected before.
func (d *dragSelect) release(vp *viewport.Model) tea.Cmd {
	if !d.sel.dragging {
		return nil
	}
	d.sel.dragging = false
	d.edge = 0
	return d.copy(vp)
}

// copy puts the selection on the clipboard and says what went there.
func (d *dragSelect) copy(vp *viewport.Model) tea.Cmd {
	text := selectedText(vp, d.sel)
	if text == "" {
		d.clear()
		return nil
	}
	// OSC 52 reaches the local clipboard even over SSH.
	return tea.Batch(tea.SetClipboard(text), status(copiedNote(text)))
}

func (d *dragSelect) clear() { d.sel = selection{} }

// cellAt maps a position in a viewport's content to the document cell
// under it. Positions past an edge clamp to it, so a drag that runs off
// the pane still selects to the end of what is visible.
func cellAt(vp *viewport.Model, x, y int) cellPos {
	line := vp.YOffset() + clampInt(y, 0, max(0, vp.Height()-1))
	return cellPos{
		line: clampInt(line, 0, max(0, vp.TotalLineCount()-1)),
		col:  clampInt(x, 0, max(0, vp.Width()-1)),
	}
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// selectedText is the selection as text: whole lines between the two
// ends, the first from its column, the last to its column, trailing
// blanks dropped from each — what a terminal hands over.
func selectedText(vp *viewport.Model, sel selection) string {
	if sel.empty() {
		return ""
	}
	from, to := sel.bounds()
	lines := strings.Split(ansi.Strip(vp.GetContent()), "\n")
	var out []string
	for l := from.line; l <= to.line && l < len(lines); l++ {
		a, b := 0, ansi.StringWidth(lines[l])
		if l == from.line {
			a = from.col
		}
		if l == to.line {
			b = min(b, to.col+1)
		}
		if a >= b {
			out = append(out, "")
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(lines[l], a, b), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// copiedNote says what went to the clipboard: how many lines, or the
// text itself when it is one short line.
func copiedNote(text string) string {
	if n := strings.Count(text, "\n") + 1; n > 1 {
		return fmt.Sprintf("copied %d lines", n)
	}
	return "copied " + strconv.Quote(ansi.Truncate(text, 40, glyph.ellipsis))
}

// paintSelection paints the selection over a viewport's rendered rows.
// Each selected run is cut out of the styled line by cell and
// re-rendered in reverse video; the rest of the line keeps its own
// styling, because ansi.Cut carries the escape sequences with it.
func paintSelection(view string, vp *viewport.Model, sel selection) string {
	if sel.empty() {
		return view
	}
	from, to := sel.bounds()
	lines := strings.Split(view, "\n")
	for i := range lines {
		l := vp.YOffset() + i
		if l < from.line || l > to.line {
			continue
		}
		lw := ansi.StringWidth(lines[i])
		a, b := 0, lw
		if l == from.line {
			a = from.col
		}
		if l == to.line {
			b = min(lw, to.col+1)
		}
		if a >= b {
			continue
		}
		lines[i] = ansi.Cut(lines[i], 0, a) +
			styleSelection.Render(ansi.Strip(ansi.Cut(lines[i], a, b))) +
			ansi.Cut(lines[i], b, lw)
	}
	return strings.Join(lines, "\n")
}

// vpSelectMouse is vpMouse plus selection: the wheel scrolls, a left
// press starts a selection, motion extends it, release copies it.
func vpSelectMouse(vp *viewport.Model, d *dragSelect, msg tea.MouseMsg, x, y int) tea.Cmd {
	switch e := msg.(type) {
	case tea.MouseClickMsg:
		if e.Button == tea.MouseLeft {
			d.press(vp, x, y)
		}
		return nil
	case tea.MouseMotionMsg:
		if e.Button == tea.MouseLeft {
			return d.motion(vp, x, y)
		}
		return nil
	case tea.MouseReleaseMsg:
		return d.release(vp)
	}
	return vpMouse(vp, msg)
}
