//go:build !windows

package fsutil

import (
	"fmt"
	"os"
	"syscall"
)

// errCrossDevice is the errno a rename across filesystems fails with.
var errCrossDevice error = syscall.EXDEV

// syncDir fsyncs a directory so a preceding rename/create inside it is
// durable. Best-effort: some filesystems refuse to fsync directories and the
// data itself has already been fsynced.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// SameDevice reports whether a and b live on the same filesystem (st_dev),
// i.e. whether a rename between them can succeed without a copy.
func SameDevice(a, b string) (bool, error) {
	da, err := device(a)
	if err != nil {
		return false, err
	}
	db, err := device(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

func device(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %q: no device information", path)
	}
	return uint64(st.Dev), nil // Dev is int32 on darwin, uint64 on linux
}
