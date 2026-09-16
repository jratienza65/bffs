package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var fixedMtime = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// exdevFor returns a rename error classified as cross-device on this OS.
func exdevFor(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errCrossDevice}
}

// renameRecorder records every rename and fails the first one whose source
// is not a .part intermediate with a cross-device error, so the fallback
// copy path runs while the final .part → dst rename still succeeds.
type renameRecorder struct {
	calls [][2]string
	exdev bool // inject EXDEV on the direct rename
}

func (r *renameRecorder) rename(oldpath, newpath string) error {
	r.calls = append(r.calls, [2]string{oldpath, newpath})
	if r.exdev && !strings.HasSuffix(oldpath, partSuffix) {
		return exdevFor(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func TestIsExdev(t *testing.T) {
	if !isExdev(exdevFor("a", "b")) {
		t.Fatal("wrapped errCrossDevice should classify as EXDEV")
	}
	if isExdev(errors.New("boom")) {
		t.Fatal("unrelated error must not classify as EXDEV")
	}
	if isExdev(&os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrNotExist}) {
		t.Fatal("ENOENT must not classify as EXDEV")
	}
}

func TestMoveFileSameDeviceRenames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.jsonl")
	dst := filepath.Join(dir, "sub", "b.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &renameRecorder{}
	injectRename(t, rec.rename)

	if err := MoveFile(dst, src); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0] != [2]string{src, dst} {
		t.Fatalf("want exactly one direct rename, got %v", rec.calls)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("src should be gone, stat err = %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "line\n" {
		t.Fatalf("content: got %q", got)
	}
}

func TestMoveFileCrossDeviceCopies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.jsonl")
	dst := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Touch(src, fixedMtime); err != nil {
		t.Fatal(err)
	}
	// A stale intermediate from an earlier interrupted move must not block us.
	if err := os.WriteFile(dst+partSuffix, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &renameRecorder{exdev: true}
	injectRename(t, rec.rename)

	if err := MoveFile(dst, src); err != nil {
		t.Fatalf("MoveFile: %v", err)
	}
	want := [][2]string{{src, dst}, {dst + partSuffix, dst}}
	if len(rec.calls) != 2 || rec.calls[0] != want[0] || rec.calls[1] != want[1] {
		t.Fatalf("renames: want %v, got %v", want, rec.calls)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("content: got %q", got)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("src should be removed after copy, stat err = %v", err)
	}
	info, _ := os.Stat(dst)
	if !info.ModTime().Equal(fixedMtime) {
		t.Fatalf("mtime not preserved: want %v, got %v", fixedMtime, info.ModTime())
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm not preserved: got %o", perm)
		}
	}
	assertNoTempFiles(t, dir)
}

func TestMoveFileOtherErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	injectRename(t, func(string, string) error { return boom })

	if err := MoveFile(dst, src); !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("src must survive a failed rename: %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dst must not exist: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestMoveFileCrossDeviceSecondRenameFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	dst := filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	injectRename(t, func(oldpath, newpath string) error {
		if strings.HasSuffix(oldpath, partSuffix) {
			return boom
		}
		return exdevFor(oldpath, newpath)
	})

	if err := MoveFile(dst, src); !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("src must survive: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestMoveFileCrossDeviceRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "d")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	injectRename(t, exdevFor)
	err := MoveFile(filepath.Join(dir, "e"), src)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("want 'not a regular file', got %v", err)
	}
}

// makeTree builds src/{top.txt, nested/deep.txt, (link -> top.txt)} with a
// fixed mtime on the files.
func makeTree(t *testing.T, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"top.txt", filepath.Join("nested", "deep.txt")} {
		p := filepath.Join(src, f)
		if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Touch(p, fixedMtime); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("top.txt", filepath.Join(src, "link")); err != nil {
			t.Fatal(err)
		}
	}
}

func assertTree(t *testing.T, dst string) {
	t.Helper()
	for _, f := range []string{"top.txt", filepath.Join("nested", "deep.txt")} {
		p := filepath.Join(dst, f)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if string(got) != f {
			t.Fatalf("%s content: got %q", f, got)
		}
		info, _ := os.Stat(p)
		if !info.ModTime().Equal(fixedMtime) {
			t.Fatalf("%s mtime: want %v, got %v", f, fixedMtime, info.ModTime())
		}
	}
	if runtime.GOOS != "windows" {
		target, err := os.Readlink(filepath.Join(dst, "link"))
		if err != nil {
			t.Fatalf("symlink not recreated: %v", err)
		}
		if target != "top.txt" {
			t.Fatalf("symlink target: want top.txt, got %q", target)
		}
	}
}

func TestMoveDirSameDeviceRenames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	makeTree(t, src)
	rec := &renameRecorder{}
	injectRename(t, rec.rename)

	if err := MoveDir(dst, src); err != nil {
		t.Fatalf("MoveDir: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("want one rename, got %v", rec.calls)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("src should be gone: %v", err)
	}
	assertTree(t, dst)
}

func TestMoveDirCrossDeviceCopies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	makeTree(t, src)
	// Stale intermediate must be discarded, not merged into.
	if err := os.MkdirAll(filepath.Join(dst+partSuffix, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &renameRecorder{exdev: true}
	injectRename(t, rec.rename)

	if err := MoveDir(dst, src); err != nil {
		t.Fatalf("MoveDir: %v", err)
	}
	want := [][2]string{{src, dst}, {dst + partSuffix, dst}}
	if len(rec.calls) != 2 || rec.calls[0] != want[0] || rec.calls[1] != want[1] {
		t.Fatalf("renames: want %v, got %v", want, rec.calls)
	}
	assertTree(t, dst)
	if _, err := os.Stat(filepath.Join(dst, "junk")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale .part content leaked into dst: %v", err)
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("src should be removed after copy: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestMoveDirCrossDeviceCopyFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	makeTree(t, src)
	// Failing the final .part → dst rename exercises the rollback branch
	// without permission tricks (which would not work on Windows).
	boom := errors.New("boom")
	injectRename(t, func(oldpath, newpath string) error {
		if strings.HasSuffix(oldpath, partSuffix) {
			return boom
		}
		return exdevFor(oldpath, newpath)
	})

	if err := MoveDir(dst, src); !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	assertTree(t, src) // source untouched
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dst must not exist: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestMoveDirCrossDeviceRefusesFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	injectRename(t, exdevFor)
	err := MoveDir(filepath.Join(dir, "g"), src)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want 'not a directory', got %v", err)
	}
}
