package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWrite replaces the file at path with data in one step: the bytes go
// to a temp file in the same directory (so the final rename never crosses a
// device), the temp file is chmod'ed to perm (defeating the umask), fsynced,
// closed, and renamed over path. The parent directory is fsynced afterwards
// on a best-effort basis so the rename itself survives a crash; a failed
// directory fsync is not an error because some filesystems refuse it.
//
// Readers never observe a partial file: they see either the old content or
// the new one. Any failure leaves the original untouched and removes the
// temp file.
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	fail := func(stage string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s %q: %w", stage, path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail("chmod temp for", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write temp for", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("fsync temp for", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp for %q: %w", path, err)
	}
	if err := renameFn(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp over %q: %w", path, err)
	}
	syncDir(dir)
	return nil
}
