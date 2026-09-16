package fsutil

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFreeSpace(t *testing.T) {
	got, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if got <= 0 {
			t.Errorf("FreeSpace = %d, want > 0 on %s", got, runtime.GOOS)
		}
	default:
		if got != -1 {
			t.Errorf("FreeSpace = %d, want -1 (unknown) on %s", got, runtime.GOOS)
		}
	}
}

func TestFreeSpaceMissingDir(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no probe on this platform")
	}
	_, err := FreeSpace(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want one wrapping fs.ErrNotExist", err)
	}
}
