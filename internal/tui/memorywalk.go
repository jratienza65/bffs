package tui

import (
	"io/fs"
	"os"
)

// walkMemoryDir visits every entry under a memory directory through an
// os.Root, so a symlink inside one cannot walk the reader out of it —
// memory directories arrive from other machines in bundles, and a plain
// filepath.WalkDir would follow whatever it found (gosec G122). rel is
// slash-separated and relative to dir, "." for the directory itself;
// visit reads a file with root.Open(rel) and may return fs.SkipDir.
func walkMemoryDir(dir string, visit func(root *os.Root, rel string, d fs.DirEntry) error) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return visit(root, rel, d)
	})
}
