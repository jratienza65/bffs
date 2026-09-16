//go:build !darwin && !linux && !windows

package fsutil

// FreeSpace reports -1 (unknown) on platforms bffs does not probe.
func FreeSpace(string) (int64, error) { return -1, nil }
