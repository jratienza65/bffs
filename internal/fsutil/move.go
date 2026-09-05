package fsutil

import (
	"errors"
	"fmt"
	"os"
)

// renameFn is the rename used by every move in this package. It exists as a
// variable so tests can inject failures — in particular a cross-device error
// — without resorting to permission tricks, which do not work on Windows.
var renameFn = os.Rename

// isExdev reports whether err is a cross-device rename failure: EXDEV on
// unix, ERROR_NOT_SAME_DEVICE on Windows (errCrossDevice is per-OS).
func isExdev(err error) bool {
	return errors.Is(err, errCrossDevice)
}

// MoveFile moves the regular file src to dst. It is a plain rename when the
// two live on one device. Across devices it copies src to dst+".part"
// (fsynced, mtime preserved), renames the .part over dst, and only then
// removes src — so a crash leaves either the source intact or a complete
// destination, never a half-copied dst. A stale dst+".part" from an earlier
// interrupted move is discarded first.
func MoveFile(dst, src string) error {
	err := renameFn(src, dst)
	if err == nil {
		return nil
	}
	if !isExdev(err) {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("move %q: not a regular file", src)
	}
	part := dst + partSuffix
	_ = os.Remove(part)
	if err := CopyFile(part, src, info.Mode().Perm()); err != nil {
		return err
	}
	if err := Touch(part, info.ModTime()); err != nil {
		_ = os.Remove(part)
		return err
	}
	if err := renameFn(part, dst); err != nil {
		_ = os.Remove(part)
		return err
	}
	return os.Remove(src)
}

// MoveDir moves the directory src to dst. Same-device moves are one rename.
// Across devices the tree is copied into dst+".part" (files fsynced, file
// mtimes preserved, symlinks recreated), the .part is renamed to dst, and
// src is removed last. A failed copy removes the .part and leaves src as it
// was. A stale dst+".part" from an earlier interrupted move is discarded
// first.
func MoveDir(dst, src string) error {
	err := renameFn(src, dst)
	if err == nil {
		return nil
	}
	if !isExdev(err) {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("move %q: not a directory", src)
	}
	part := dst + partSuffix
	_ = os.RemoveAll(part)
	if err := copyTree(part, src); err != nil {
		_ = os.RemoveAll(part)
		return err
	}
	if err := renameFn(part, dst); err != nil {
		_ = os.RemoveAll(part)
		return err
	}
	return os.RemoveAll(src)
}

// partSuffix marks the intermediate a cross-device move writes next to its
// destination. It is bffs-owned: safe to discard when found stale.
const partSuffix = ".part"
