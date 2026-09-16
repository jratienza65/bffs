package store

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	EnvConfigDir   = "BFFS_HOME"
	AccountsFile   = "accounts.toml"
	StateFile      = "state.toml"
	RealClaudePath = "real-claude.path"
)

func ConfigDir() (string, error) {
	if v := os.Getenv(EnvConfigDir); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "bffs"), nil
}

func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
