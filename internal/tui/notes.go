package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A note on the status line and a toast are the two ways the browser
// says something happened without taking the reader anywhere: the
// status line answers the key that was just pressed, and a toast
// carries the verdict of work that finished while the reader had moved
// on. Both expire by themselves, because a stale note read as the
// answer to the next key is worse than no note.

// noteKind is what a note is telling the reader, and so how it is
// painted. One tone for everything reads "success" over refusals, which
// is the opposite of what they say.
type noteKind int

const (
	noteInfo noteKind = iota // a state change the reader asked for
	noteDone                 // work finished: copied, switched, exported
	noteWarn                 // a refusal: the act did not happen, and why
	noteBad                  // an error, from an engine or from the network
)

// noteLinger is how long a note stays on the status line, and
// toastLinger how long a toast stays up.
const noteLinger = 6 * time.Second

// expireNote takes a note down once its time is up. The timestamp is
// carried through so a newer note is never cleared by an older timer.
//
// It is a variable because the model tests run a command where it is
// returned: a real tick would stall every test that posts a note for
// six seconds, and a short one would expire the note before the test
// could read it. They replace this and send statusOutMsg themselves.
var expireNote = func(at time.Time) tea.Cmd {
	return tea.Tick(noteLinger, func(time.Time) tea.Msg { return statusOutMsg{at: at} })
}

// expireToast is the same seam for a toast.
var expireToast = func(seq int) tea.Cmd {
	return tea.Tick(toastLinger, func(time.Time) tea.Msg { return toastOutMsg{seq: seq} })
}

// mark is the glyph a note leads with, so its kind survives NO_COLOR
// and a terminal with sixteen colours.
func (k noteKind) mark() string {
	switch k {
	case noteDone:
		return glyph.ok + " "
	case noteWarn:
		return glyph.missing + " "
	case noteBad:
		return glyph.bad + " "
	}
	return ""
}

func (k noteKind) style() lipgloss.Style {
	switch k {
	case noteDone:
		return styleOK
	case noteWarn:
		return styleWarn
	case noteBad:
		return styleError
	}
	return styleStatus
}

const toastLinger = 5 * time.Second

// toast is a floating notice: the verdict of work that finished in the
// background, where the reader may have moved on from whatever asked
// for it. The status line is a row the eye has to go to; a toast comes
// to the eye, and leaves by itself.
type toast struct {
	kind  noteKind
	title string
	body  []string
	seq   int
}

// notify posts a toast and returns the tick that takes it down.
func (a *app) notify(kind noteKind, title string, body ...string) tea.Cmd {
	a.toastSeq++
	a.toast = toast{kind: kind, title: title, body: body, seq: a.toastSeq}
	return expireToast(a.toastSeq)
}

// toastBox draws the card: a bar down its left in the verdict's tone,
// the title, then the body. It is sized to its content, bounded by the
// terminal, so it never becomes a second pane.
func (a *app) toastBox(width int) []string {
	t := a.toast
	inner := clampInt(width-4, 16, 56) - 3 // the bar, and a space each side
	lines := wrapText(t.title, inner)
	titled := len(lines)
	for _, b := range t.body {
		lines = append(lines, wrapText(b, inner)...)
	}
	w := 0
	for _, l := range lines {
		w = max(w, lipgloss.Width(l))
	}
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		style := styleFaint
		if i < titled {
			style = styleStatus
		}
		out = append(out, t.kind.style().Render(glyph.quote)+style.Render(" "+pad(l, w)+" "))
	}
	return out
}

// wrapText breaks a note into lines of at most width cells, on spaces
// where it can and mid-word where it must.
func wrapText(s string, width int) []string {
	return wrapSpans([]span{{text: s}}, max(4, width), "", "")
}

// placeToast lays the card over the bottom right of the body, one row
// above the status line, leaving everything it does not cover alone.
func placeToast(body []string, card []string, width int) []string {
	if len(card) == 0 || len(body) == 0 {
		return body
	}
	cw := 0
	for _, l := range card {
		cw = max(cw, lipgloss.Width(l))
	}
	top := max(0, len(body)-len(card)-1)
	left := max(0, width-cw-2)
	for i, l := range card {
		row := top + i
		if row >= len(body) {
			break
		}
		line := body[row]
		body[row] = ansi.Cut(line, 0, left) + l + ansi.Cut(line, left+cw, max(left+cw, lipgloss.Width(line)))
	}
	return body
}
