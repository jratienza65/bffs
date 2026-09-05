package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CopyFile(dst, src, 0o600); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "hello" {
		t.Fatalf("content: want %q, got %q", "hello", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(dst)
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm: want 0600, got %o", perm)
		}
	}
}

func TestCopyFileRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := CopyFile(dst, src, 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want ErrExist, got %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "keep" {
		t.Fatalf("existing dst clobbered: %q", got)
	}
}

func TestCopyFileRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := CopyFile(filepath.Join(dir, "dst"), dir, 0o600); err == nil {
		t.Fatal("want error copying a directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "dst")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dst should not exist, stat err = %v", err)
	}
}

func TestCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	err := CopyFile(filepath.Join(dir, "dst"), filepath.Join(dir, "missing"), 0o600)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}
