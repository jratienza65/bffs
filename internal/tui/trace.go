package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// BFFS_DEBUG names a file the browser appends one line to per key and
// per mouse event, holding the state the event left behind: the mode,
// the focused panel, the tab, each panel's cursor and view offset, the
// overlay on top and what the preview is showing. The browser owns the
// terminal, so a log line is the only way to see what a key did; this
// turns "the wheel does nothing in the wizard" into a sequence anyone
// can replay.
//
// Off by default and cheap when off: one nil check per event. The file
// is opened once per browser (newApp), never once per process, so a
// test can drive it.
func openTrace() *os.File {
	path := os.Getenv("BFFS_DEBUG")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil // tracing is a debugging aid; it never breaks a session
	}
	fmt.Fprintf(f, "\n=== bffs browser %s\n", time.Now().Format(time.RFC3339))
	return f
}

// trace records one event and the state it left behind.
func (a *app) trace(event string) {
	f := a.tracer
	if f == nil {
		return
	}
	ws := a.ws
	cursors := make([]string, 0, panelCount)
	for _, p := range ws.panels {
		cursors = append(cursors, fmt.Sprintf("%d+%d/%d", p.list.Index(), p.offset, len(p.list.Items())))
	}
	top := "-"
	if s := a.top(); s != nil {
		top = fmt.Sprintf("%T", s)
	}
	fmt.Fprintf(f, "%s %-14s size=%dx%d mode=%d focus=%d tab=%d main=%t cur=%s top=%s busy=%t preview=%q status=%q\n",
		time.Now().Format("15:04:05.000"), event, a.width, a.height,
		ws.mode, ws.focus, ws.tab, ws.mainFocus, strings.Join(cursors, ","),
		top, a.busy(), ws.previewKey, a.status)
}

// traceMouse names a mouse event the way the driver's script spells it.
func traceMouse(msg tea.MouseMsg) string {
	m := msg.Mouse()
	kind := "mouse"
	switch msg.(type) {
	case tea.MouseClickMsg:
		kind = "click"
	case tea.MouseReleaseMsg:
		kind = "release"
	case tea.MouseWheelMsg:
		kind = "wheel"
	case tea.MouseMotionMsg:
		kind = "motion"
	}
	return fmt.Sprintf("%s:%s@%d,%d", kind, m.Button, m.X, m.Y)
}
