package transcripts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MemoryFile is one file of an auto-memory directory. Name is relative to
// the memory dir, slash-separated. Pinned files are injected into every
// session Claude starts for the project; AbsolutePaths and AtRefs are the
// distinct PathRef.Path values ScanAbsolutePaths reports for the file.
type MemoryFile struct {
	Name          string
	Size          int64
	ModTime       time.Time
	Pinned        bool
	AbsolutePaths []string
	AtRefs        []string
}

// Memory is one auto-memory directory: <root.Dir>/<slug>/memory. Cwd is
// the effective cwd of the sibling transcripts when the slug directory
// holds any ("" otherwise — memory is keyed by the git root, so a
// project whose sessions all ran in subdirectories has none); GitRoot is
// the repository root above Cwd when there is one.
type Memory struct {
	Root      Root
	Slug      string
	Dir       string
	Cwd       string
	GitRoot   string
	CwdExists bool
	HasIndex  bool // MEMORY.md exists
	Files     []MemoryFile
}

// Memories catalogs every auto-memory directory under roots: each
// <root.Dir>/<slug>/memory/ whose slug is not reserved, in root order
// then slug order, with the same file set ScanAbsolutePaths walks. Roots
// whose projects dir does not exist are skipped. ctx is checked between
// slug directories.
func Memories(ctx context.Context, roots []Root) ([]Memory, error) {
	var out []Memory
	for _, root := range roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", root.Dir, err)
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !e.IsDir() || IsReserved(e.Name()) {
				continue
			}
			slugDir := filepath.Join(root.Dir, e.Name())
			memDir := filepath.Join(slugDir, MemorySubdir)
			if info, err := os.Stat(memDir); err != nil || !info.IsDir() {
				continue
			}
			m, err := describeMemory(root, e.Name(), slugDir, memDir)
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func describeMemory(root Root, slug, slugDir, memDir string) (Memory, error) {
	m := Memory{Root: root, Slug: slug, Dir: memDir}
	files, err := memoryFiles(memDir)
	if err != nil {
		return Memory{}, fmt.Errorf("read %s: %w", memDir, err)
	}
	for _, f := range files {
		if f.name == MemoryIndexFile {
			m.HasIndex = true
		}
		mf := MemoryFile{Name: f.name, Size: f.info.Size(), ModTime: f.info.ModTime(), Pinned: f.pinned}
		seen := map[string]bool{}
		for _, ref := range f.refs {
			key := ref.Kind + "\x00" + ref.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			switch ref.Kind {
			case PathKindAbs:
				mf.AbsolutePaths = append(mf.AbsolutePaths, ref.Path)
			case PathKindAt:
				mf.AtRefs = append(mf.AtRefs, ref.Path)
			}
		}
		m.Files = append(m.Files, mf)
	}
	if cwd, err := DecodeCwd(slugDir); err == nil {
		m.Cwd = cwd
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			m.CwdExists = true
		}
		if gr, ok := GitRoot(cwd); ok {
			m.GitRoot = gr
		}
	}
	return m, nil
}
