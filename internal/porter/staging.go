package porter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// StagingSubdir is the directory under the bffs config dir where import
// bytes land before they are committed: <cfgDir>/staging/<bundle-id>/.
const StagingSubdir = "staging"

// StagingDir is where a bundle's bytes are staged: <cfgDir>/staging/<id>.
func StagingDir(cfgDir, bundleID string) string {
	return filepath.Join(cfgDir, StagingSubdir, bundleID)
}

// StaleStaging lists the leftover staging directories under
// <cfgDir>/staging — each one an import that did not finish — sorted by
// name. A missing staging directory is (nil, nil). Nothing is removed:
// `bffs import --clean-staging` does that after listing them.
func StaleStaging(cfgDir string) ([]string, error) {
	dir := filepath.Join(cfgDir, StagingSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// createStaging makes dir fresh (0700), creating its parent when needed.
// An existing entry — a leftover of an interrupted import — is an error
// naming --clean-staging; the caller never touches it.
func createStaging(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("create staging: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("staging directory %q already exists (an earlier import of this bundle was interrupted); inspect it, then rerun with --clean-staging", dir)
		}
		return fmt.Errorf("create staging %q: %w", dir, err)
	}
	return nil
}
