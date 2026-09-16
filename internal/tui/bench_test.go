package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The browser redraws the whole frame after every event, so the wheel
// is the worst case: one update and one render per notch, at the rate
// the terminal delivers them. These benchmarks are the guard for that
// path — nothing that reads a file, parses TOML or renders Markdown may
// end up in Update or View, where it would run per notch. Preview
// loaders are commands for that reason, and a renderer added later
// (Glamour) has to be memoised on a key rather than run here.
//
//	go test ./internal/tui -run XXX -bench . -benchmem

func benchApp(b *testing.B, tweak func(*app)) *app {
	b.Helper()
	a := goldenApp(b, 160, 48, tweak)
	return a
}

// BenchmarkView is one full frame: three panels, the preview, the
// header and the footer.
func BenchmarkView(b *testing.B) {
	a := benchApp(b, nil)
	b.ReportAllocs()
	for b.Loop() {
		_ = a.View()
	}
}

// BenchmarkScrollStorm is a wheel notch and the frame it produces, over
// the preview (the viewport) and over a panel (its own offset).
func BenchmarkScrollStorm(b *testing.B) {
	for _, tc := range []struct {
		name string
		x, y int
	}{
		{"preview", 120, 10},
		{"panel", 20, 10},
	} {
		b.Run(tc.name, func(b *testing.B) {
			a := benchApp(b, func(a *app) { _ = a.ws.setFocus(panelItems) })
			msg := tea.MouseWheelMsg{X: tc.x, Y: tc.y, Button: tea.MouseWheelDown}
			b.ReportAllocs()
			for b.Loop() {
				a.Update(msg)
				_ = a.View()
			}
		})
	}
}

// BenchmarkCursorMove is the keyboard equivalent: a move re-derives the
// selection chain (workspace.sync), which must stay free of I/O — the
// loading it needs is returned as a command and run elsewhere.
func BenchmarkCursorMove(b *testing.B) {
	a := benchApp(b, func(a *app) { _ = a.ws.setFocus(panelItems) })
	down := tea.KeyPressMsg{Code: 'j', Text: "j"}
	up := tea.KeyPressMsg{Code: 'k', Text: "k"}
	b.ReportAllocs()
	for b.Loop() {
		a.Update(down)
		_ = a.View()
		a.Update(up)
		_ = a.View()
	}
}

// BenchmarkOverlayView draws the transcript overlay over the workspace:
// the stack's cost on top of the frame.
func BenchmarkOverlayView(b *testing.B) {
	a := benchApp(b, func(a *app) {
		a.stack = append(a.stack, newHelpScreen(a.ws.helpGroups()))
	})
	b.ReportAllocs()
	for b.Loop() {
		_ = a.View()
	}
}
