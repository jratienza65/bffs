package sessions

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

func TestDir(t *testing.T) {
	got := Dir("/cfg", "work")
	want := filepath.Join("/cfg", "sessions", "work")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestSyncSymlinksPartialMirrorsHomeExceptAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	// Stand up a realistic ~/.claude with a mix of files and dirs, including
	// the auth files that must NOT be symlinked.
	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	mustWriteFile(t, filepath.Join(homeDir, "history.jsonl"), "")
	mustWriteFile(t, filepath.Join(homeDir, ".claude.json"), "{}")
	mustWriteFile(t, filepath.Join(homeDir, ".credentials.json"), "{}")
	mustMkdir(t, filepath.Join(homeDir, "skills"))
	mustMkdir(t, filepath.Join(homeDir, "plugins"))
	mustMkdir(t, filepath.Join(homeDir, "projects"))
	mustMkdir(t, filepath.Join(homeDir, "todos"))

	skipped, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial)
	if err != nil {
		t.Fatalf("SyncSymlinks: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpected skipped paths: %v", skipped)
	}

	for _, sub := range []string{"settings.json", "history.jsonl", "skills", "plugins", "projects", "todos"} {
		assertSymlink(t, filepath.Join(sessionDir, sub), filepath.Join(homeDir, sub))
	}
	for _, sub := range []string{".claude.json", ".credentials.json"} {
		assertNotPresent(t, filepath.Join(sessionDir, sub))
	}
}

func TestSyncSymlinksPicksUpNewHomeEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Claude added a new dir to ~/.claude after first sync.
	mustMkdir(t, filepath.Join(homeDir, "skills"))
	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertSymlink(t, filepath.Join(sessionDir, "skills"), filepath.Join(homeDir, "skills"))
}

func TestSyncSymlinksRemovesLinksWhenHomeEntryGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Remove the source.
	if err := os.Remove(filepath.Join(homeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	assertNotPresent(t, filepath.Join(sessionDir, "settings.json"))
}

func TestSyncSymlinksFull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	mustMkdir(t, filepath.Join(homeDir, "skills"))

	skipped, err := SyncSymlinks(sessionDir, homeDir, store.IsolationFull)
	if err != nil {
		t.Fatalf("SyncSymlinks: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("full preset should never skip anything: %v", skipped)
	}
	for _, sub := range []string{"settings.json", "skills", ".claude.json", ".credentials.json"} {
		assertNotPresent(t, filepath.Join(sessionDir, sub))
	}
}

func TestSyncSymlinksDowngradePartialToFull(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	mustMkdir(t, filepath.Join(homeDir, "skills"))

	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatalf("partial: %v", err)
	}
	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationFull); err != nil {
		t.Fatalf("full: %v", err)
	}
	for _, sub := range []string{"settings.json", "skills"} {
		assertNotPresent(t, filepath.Join(sessionDir, sub))
	}
}

func TestSyncSymlinksSkipsConflictsLeniently(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	if err := EnsureDir(sessionDir); err != nil {
		t.Fatal(err)
	}
	// Plant a real settings.json in the session dir — claude already wrote it.
	planted := filepath.Join(sessionDir, "settings.json")
	mustWriteFile(t, planted, `{"theme":"dark"}`)
	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	mustMkdir(t, filepath.Join(homeDir, "skills"))

	skipped, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial)
	if err != nil {
		t.Fatalf("SyncSymlinks: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "settings.json" {
		t.Errorf("skipped = %v, want [settings.json]", skipped)
	}

	// Real file preserved.
	b, err := os.ReadFile(planted)
	if err != nil || string(b) != `{"theme":"dark"}` {
		t.Errorf("planted settings.json was disturbed: %q (err=%v)", string(b), err)
	}
	// Other targets still got linked.
	assertSymlink(t, filepath.Join(sessionDir, "skills"), filepath.Join(homeDir, "skills"))
}

func TestSyncSymlinksLeavesUserSymlinksAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	if err := EnsureDir(sessionDir); err != nil {
		t.Fatal(err)
	}
	// User created their own symlink pointing OUTSIDE ~/.claude (e.g. to a
	// shared dir); we should never remove that.
	external := t.TempDir()
	userLink := filepath.Join(sessionDir, "extras")
	if err := os.Symlink(external, userLink); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")

	if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
		t.Fatal(err)
	}
	// User's symlink survives.
	target, err := os.Readlink(userLink)
	if err != nil {
		t.Fatalf("user link gone: %v", err)
	}
	if target != external {
		t.Errorf("user link target changed: %q want %q", target, external)
	}
}

func TestSyncSymlinksIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfgDir, homeDir := t.TempDir(), t.TempDir()
	sessionDir := Dir(cfgDir, "work")

	mustWriteFile(t, filepath.Join(homeDir, "settings.json"), "{}")
	mustMkdir(t, filepath.Join(homeDir, "skills"))
	mustMkdir(t, filepath.Join(homeDir, "plugins"))

	for i := 0; i < 3; i++ {
		if _, err := SyncSymlinks(sessionDir, homeDir, store.IsolationPartial); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	want := []string{"plugins", "settings.json", "skills"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("entry[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertSymlink(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is not a symlink (mode=%v)", path, info.Mode())
		return
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if target != wantTarget {
		t.Errorf("symlink %s -> %s, want -> %s", path, target, wantTarget)
	}
}

func assertNotPresent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		t.Errorf("expected %s to not exist, but it does", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("expected NotExist for %s, got %v", path, err)
	}
}
