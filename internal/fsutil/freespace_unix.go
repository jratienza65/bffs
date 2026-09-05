//go:build darwin || linux

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// FreeSpace returns the number of bytes an unprivileged caller may still
// write on the filesystem holding dir (statfs f_bavail × f_bsize), or -1
// with a nil error when the platform cannot say. dir must exist.
func FreeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return -1, nil
		}
		return 0, &pathError{op: "statfs", path: dir, err: err}
	}
	bavail, bsize := uint64(st.Bavail), uint64(st.Bsize)
	if bsize == 0 || bavail > uint64(1<<62)/bsize {
		return -1, nil
	}
	return int64(bavail * bsize), nil
}
