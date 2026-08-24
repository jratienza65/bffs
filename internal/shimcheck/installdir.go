package shimcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// EnvShimDir lets a user pin the shim install directory persistently so
// they don't have to pass --dir to `bffs init` every time. --dir still wins.
const EnvShimDir = "BFFS_SHIM_DIR"

// DefaultInstallDir returns where the shim lives absent an explicit --dir:
// $BFFS_SHIM_DIR, else a per-OS default — ~/.bffs/bin (macOS/Linux) or
// %LOCALAPPDATA%\bffs\bin (Windows). The default is intentionally a dedicated
// bffs dir, not a shared location like ~/.local/bin, so the shim won't collide
// with Claude Code's own install script (which also targets ~/.local/bin).
func DefaultInstallDir() (string, error) {
	if v := os.Getenv(EnvShimDir); v != "" {
		return v, nil
	}
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set; pass --dir or set $%s to choose an install directory", EnvShimDir)
		}
		return filepath.Join(base, "bffs", "bin"), nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".bffs", "bin"), nil
	}
}
