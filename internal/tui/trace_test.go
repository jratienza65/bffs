package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// BFFS_DEBUG is the only window into a session that owns the terminal:
// one line per event with the state it left behind.
func TestTraceRecordsEventsWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.log")
	t.Setenv("BFFS_DEBUG", path)
	a := goldenApp(t, 120, 32, func(a *app) { _ = a.ws.setFocus(panelItems) })
	a.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	a.Update(tea.MouseWheelMsg{X: 20, Y: 10, Button: tea.MouseWheelDown})
	// Read it after the writer has let go: on Windows an open file
	// cannot be deleted, so leaving it open fails the temp-dir cleanup.
	a.closeTrace()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{"=== bffs browser", "key:j", "wheel:", "cur=", "preview=", "size=120x32"} {
		if !strings.Contains(log, want) {
			t.Errorf("trace missing %q:\n%s", want, log)
		}
	}
	if lines := strings.Count(log, "\n"); lines < 3 {
		t.Errorf("want a line per event, got:\n%s", log)
	}
}

// Without the variable nothing is opened and nothing is written.
func TestTraceOffByDefault(t *testing.T) {
	t.Setenv("BFFS_DEBUG", "")
	a := goldenApp(t, 120, 32, nil)
	if a.tracer != nil {
		t.Fatal("BFFS_DEBUG unset should leave the browser untraced")
	}
	a.Update(tea.KeyPressMsg{Code: 'j', Text: "j"}) // must not panic
}
