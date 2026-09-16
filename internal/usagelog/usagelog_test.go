package usagelog

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestAppendReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []Event{
		{TS: time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), Account: "work", Type: "oauth", Source: "global", Cwd: "/tmp/a"},
		{TS: time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC), Source: "none", Cwd: "/tmp/b"},
	}
	for _, e := range want {
		if err := Append(dir, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("want %d events, got %d", len(want), len(got))
	}
	for i := range want {
		if !got[i].TS.Equal(want[i].TS) || got[i].Account != want[i].Account ||
			got[i].Type != want[i].Type || got[i].Source != want[i].Source || got[i].Cwd != want[i].Cwd {
			t.Errorf("event %d: want %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestReadMissingFile(t *testing.T) {
	got, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for missing file, got %v", got)
	}
}

func TestReadSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	content := `{"ts":"2026-08-24T10:00:00Z","source":"global","cwd":"/a"}
not json at all
{"ts":"0001-01-01T00:00:00Z","source":"global","cwd":"/zero-ts"}
{"ts":"2026-08-24T12:00:00Z","source":"env","cwd":"/b"}`
	// Note: last line has no trailing newline — a valid unterminated line is kept.
	if err := os.WriteFile(Path(dir), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 valid events, got %d: %+v", len(got), got)
	}
	if got[0].Cwd != "/a" || got[1].Cwd != "/b" {
		t.Errorf("wrong events survived: %+v", got)
	}
}

func TestAppendFilePerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	if err := Append(dir, Event{TS: time.Now().UTC(), Source: "global", Cwd: "/x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm: want 0600, got %o", perm)
	}
}

func TestDisabled(t *testing.T) {
	t.Setenv(EnvDisable, "")
	if Disabled() {
		t.Error("empty env should not disable")
	}
	t.Setenv(EnvDisable, "1")
	if !Disabled() {
		t.Error("non-empty env should disable")
	}
}

func TestConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	const perGoroutine = 10
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				_ = Append(dir, Event{TS: time.Now().UTC(), Source: "global", Cwd: "/c"})
			}
		}()
	}
	wg.Wait()
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2*perGoroutine {
		t.Errorf("want %d events, got %d (torn writes?)", 2*perGoroutine, len(got))
	}
}
