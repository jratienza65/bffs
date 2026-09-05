package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteCreatesWithPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := AtomicWrite(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("content: want %q, got %q", `{"a":1}`, got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm: want 0600, got %o", perm)
		}
	}
	assertNoTempFiles(t, dir)
}

func TestAtomicWriteReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content: want %q, got %q", "new", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm after replace: want 0600, got %o", perm)
		}
	}
	assertNoTempFiles(t, dir)
}

func TestAtomicWriteRenameFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	injectRename(t, func(oldpath, newpath string) error { return boom })

	err := AtomicWrite(path, []byte("new"), 0o600)
	if !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatalf("original clobbered: %q", got)
	}
	assertNoTempFiles(t, dir)
}

func TestAtomicWriteMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "state.json")
	if err := AtomicWrite(path, []byte("x"), 0o600); err == nil {
		t.Fatal("want error for missing parent dir")
	}
}

// injectRename swaps renameFn for the duration of the test.
func injectRename(t *testing.T, fn func(oldpath, newpath string) error) {
	t.Helper()
	orig := renameFn
	renameFn = fn
	t.Cleanup(func() { renameFn = orig })
}

// assertNoTempFiles fails when AtomicWrite/CopyFile left an intermediate
// (*.tmp.*, *.part) behind in dir.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if m, _ := filepath.Match("*.tmp.*", e.Name()); m {
			t.Fatalf("leftover temp file %q", e.Name())
		}
		if filepath.Ext(e.Name()) == partSuffix {
			t.Fatalf("leftover part file %q", e.Name())
		}
	}
}
