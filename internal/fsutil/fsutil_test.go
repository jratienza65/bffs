package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestTouch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Touch(path, fixedMtime); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(fixedMtime) {
		t.Fatalf("mtime: want %v, got %v", fixedMtime, info.ModTime())
	}
}

func TestTouchMissing(t *testing.T) {
	err := Touch(filepath.Join(t.TempDir(), "missing"), fixedMtime)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestSameDevice(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	same, err := SameDevice(dir, file)
	if err != nil {
		t.Fatalf("SameDevice: %v", err)
	}
	if !same {
		t.Fatal("a file and its parent directory must be on the same device")
	}
	if _, err := SameDevice(dir, filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist for a missing path, got %v", err)
	}
}
