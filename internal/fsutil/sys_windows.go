//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// errCrossDevice is ERROR_NOT_SAME_DEVICE (0x11): MoveFileEx without
// MOVEFILE_COPY_ALLOWED fails with it when source and target are on
// different volumes. It plays the role EXDEV plays on unix.
var errCrossDevice error = syscall.Errno(0x11)

// syncDir is a no-op on Windows: FlushFileBuffers on a directory handle is
// refused, and NTFS journals directory metadata updates itself.
func syncDir(string) {}

// SameDevice reports whether a and b live on the same volume, compared by
// volume serial number (GetFileInformationByHandle). When the serial cannot
// be read for either path it falls back to comparing the volume names of
// the absolute paths (drive letter or UNC share), case-insensitively.
func SameDevice(a, b string) (bool, error) {
	sa, errA := volumeSerial(a)
	sb, errB := volumeSerial(b)
	if errA == nil && errB == nil {
		return sa == sb, nil
	}
	va, err := volumeName(a)
	if err != nil {
		return false, err
	}
	vb, err := volumeName(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

func volumeSerial(path string) (uint32, error) {
	f, err := os.Open(path) // opens directories too (FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0, &os.PathError{Op: "GetFileInformationByHandle", Path: path, Err: err}
	}
	return info.VolumeSerialNumber, nil
}

func volumeName(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.VolumeName(abs), nil
}
