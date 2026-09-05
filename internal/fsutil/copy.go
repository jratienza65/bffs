package fsutil

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CopyFile copies the regular file src to a new file dst with mode perm. dst
// is created with O_EXCL — an existing file is an error, never overwritten —
// and the copy is fsynced (file strictly, parent directory best-effort)
// before CopyFile returns. On any failure the partially written dst is
// removed. Timestamps are not preserved; use Touch for that.
func CopyFile(dst, src string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("copy %q: not a regular file", src)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	fail := func(stage string, err error) error {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("%s %q: %w", stage, dst, err)
	}
	if err := out.Chmod(perm); err != nil {
		return fail("chmod", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail("write", err)
	}
	if err := out.Sync(); err != nil {
		return fail("fsync", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close %q: %w", dst, err)
	}
	syncDir(filepath.Dir(dst))
	return nil
}

// copyTree recreates the directory src at dst (which must not exist).
// Regular files are copied with their permission bits and mtime, symlinks
// are recreated as symlinks (never followed), directories get their source
// permission bits plus owner rwx; any other file type is an error. Directory
// mtimes are restored best-effort after the tree is complete. The caller is
// responsible for removing dst on error.
func copyTree(dst, src string) error {
	type dirStamp struct {
		path  string
		mtime time.Time
	}
	var dirs []dirStamp

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info() // lstat semantics: never follows symlinks
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.Mkdir(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			dirs = append(dirs, dirStamp{target, info.ModTime()})
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case d.Type().IsRegular():
			if err := CopyFile(target, p, info.Mode().Perm()); err != nil {
				return err
			}
			if err := Touch(target, info.ModTime()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("copy %q: unsupported file type %s", p, d.Type())
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = Touch(dirs[i].path, dirs[i].mtime)
	}
	return nil
}
