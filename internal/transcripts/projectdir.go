package transcripts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jratienza65/bffs/internal/store"
)

const (
	// EnvProjectDirName replaces the slug as the projects/ entry name when
	// claude runs with it set — and only under a CLAUDE_CONFIG_DIR
	// (disk.md §2.1).
	EnvProjectDirName = "CLAUDE_CODE_PROJECT_DIR_NAME"

	// EnvClaudeConfigDir is Claude's config-dir override; the shim sets it
	// for oauth accounts.
	EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"

	maxProjectDirName = 64
)

// DecodeCwd returns the effective cwd recorded in slugDir's newest
// transcript (by mtime; an older one is consulted only when the newer
// records no cwd at all, e.g. an empty file). It never derives a path from
// the directory name: slugs are lossy. An error means the directory holds
// no transcript with a cwd.
func DecodeCwd(slugDir string) (string, error) {
	entries, err := os.ReadDir(slugDir)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path  string
		mtime int64
	}
	var cands []candidate
	for _, e := range entries {
		sid, ok := strings.CutSuffix(e.Name(), TranscriptExt)
		if !ok || !isUUID(sid) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, candidate{filepath.Join(slugDir, e.Name()), info.ModTime().UnixNano()})
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("no transcript in %s", slugDir)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mtime != cands[j].mtime {
			return cands[i].mtime > cands[j].mtime
		}
		return cands[i].path < cands[j].path
	})
	for _, c := range cands {
		h, err := ReadHead(c.path)
		if err != nil {
			continue
		}
		t, err := ReadTail(c.path)
		if err != nil {
			continue
		}
		if cwd := EffectiveCwd(h, t); cwd != "" {
			return cwd, nil
		}
	}
	return "", fmt.Errorf("no cwd recorded in the transcripts of %s", slugDir)
}

// ProjectDirFor returns the projects/ directory Claude uses for cwd under
// root, given the environment claude is launched with. A valid
// CLAUDE_CODE_PROJECT_DIR_NAME (^[A-Za-z0-9_-]{1,64}$) in env names the
// directory outright — Claude honours it only when CLAUDE_CONFIG_DIR is
// also set, and so does this. Otherwise the root's existing directories
// are consulted: the one whose newest transcript records cwd as its
// effective cwd (both sides store.NormalizePath'd) wins — this is how a
// session Claude relocated, or a >200-character slug it hashed, is found
// without predicting the name. Failing that, the directory is
// <root.Dir>/<Slug(cwd)>, which may not exist yet; ErrSlugTooLong is
// returned when even that cannot be computed.
func ProjectDirFor(root Root, cwd string, env []string) (string, error) {
	if name, ok := projectDirNameFromEnv(env); ok {
		return filepath.Join(root.Dir, name), nil
	}
	norm, err := store.NormalizePath(cwd)
	if err != nil {
		return "", err
	}
	slug, slugErr := Slug(norm)
	if slugErr == nil {
		if p := filepath.Join(root.Dir, slug); recordsCwd(p, norm) {
			return p, nil
		}
	}
	entries, err := os.ReadDir(root.Dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", root.Dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || IsReserved(name) || name == slug {
			continue
		}
		if p := filepath.Join(root.Dir, name); recordsCwd(p, norm) {
			return p, nil
		}
	}
	if slugErr != nil {
		return "", slugErr
	}
	return filepath.Join(root.Dir, slug), nil
}

// recordsCwd reports whether slugDir's newest transcript belongs to the
// (already normalised) directory norm.
func recordsCwd(slugDir, norm string) bool {
	decoded, err := DecodeCwd(slugDir)
	if err != nil {
		return false
	}
	got, err := store.NormalizePath(decoded)
	return err == nil && got == norm
}

// projectDirNameFromEnv returns the CLAUDE_CODE_PROJECT_DIR_NAME override
// when env carries a valid one alongside a CLAUDE_CONFIG_DIR.
func projectDirNameFromEnv(env []string) (string, bool) {
	var name, cfg string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case EnvProjectDirName:
			name = v
		case EnvClaudeConfigDir:
			cfg = v
		}
	}
	if cfg == "" || !validProjectDirName(name) {
		return "", false
	}
	return name, true
}

func validProjectDirName(name string) bool {
	if name == "" || len(name) > maxProjectDirName {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
