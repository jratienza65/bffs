//go:build windows

package fsutil

import (
	"golang.org/x/sys/windows"
)

// FreeSpace returns the number of bytes the caller may still write on the
// volume holding dir (GetDiskFreeSpaceEx's lpFreeBytesAvailableToCaller,
// which honours quotas), or -1 with a nil error when the platform cannot
// say. dir must exist.
func FreeSpace(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, &pathError{op: "statfs", path: dir, err: err}
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, &pathError{op: "statfs", path: dir, err: err}
	}
	if avail > 1<<62 {
		return -1, nil
	}
	return int64(avail), nil
}
