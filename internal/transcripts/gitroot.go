package transcripts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// gitTimeout bounds the one git subprocess this package runs. A hung git
// (network filesystem, credential helper) must never stall a listing.
const gitTimeout = 2 * time.Second

// envTransferCode is the pairing-code variable of the LAN transfer (M5).
// Child processes spawned by any package reachable from the MCP server must
// never inherit it, so childEnv always drops it.
const envTransferCode = "BFFS_TRANSFER_CODE"

// GitRoot walks upward from dir looking for a .git entry — a directory for a
// normal checkout, a regular file for a worktree or submodule pointer — and
// returns the first directory that has one. It is pure Go (no subprocess),
// follows symlinks like git does when stat'ing, and stops at the filesystem
// root. The result is absolute and cleaned but not symlink-resolved.
func GitRoot(dir string) (string, bool) {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	cur = filepath.Clean(cur)
	for {
		if info, err := os.Stat(filepath.Join(cur, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return cur, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
}

// ProjectKey returns the key Claude uses for dir in .claude.json's projects
// map and for the auto-memory slug: the canonical git top level when dir is
// inside a repository (git rev-parse --show-toplevel, which resolves
// symlinks and honours worktrees), else the symlink-resolved directory.
// When git is missing, refuses (dubious ownership, timeout) or prints
// something that is not an absolute path, the walked GitRoot stands in,
// symlink-resolved. The result is NFC-normalised and cleaned.
//
// A directory that does not exist yields its cleaned absolute path (like
// store.NormalizePath) rather than an error, so callers can compute where a
// not-yet-created project would live.
func ProjectKey(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", dir, err)
	}
	var key string
	if root, ok := GitRoot(abs); ok {
		key = gitTopLevel(abs)
		if key == "" {
			key = realpath(root)
		}
	} else {
		key = realpath(abs)
	}
	return filepath.Clean(norm.NFC.String(key)), nil
}

// MemorySlug is Slug(ProjectKey(dir)): the projects/ entry that holds dir's
// auto-memory directory.
func MemorySlug(dir string) (string, error) {
	key, err := ProjectKey(dir)
	if err != nil {
		return "", err
	}
	return Slug(key)
}

// realpath resolves symlinks in p. When p does not exist yet, its longest
// existing ancestor is resolved and the rest appended, so the key of a
// directory about to be created matches the key it will have afterwards
// (macOS: /var/... → /private/var/...).
func realpath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(realpath(parent), filepath.Base(p))
}

// gitTopLevel runs git rev-parse --show-toplevel in dir and returns its
// output, or "" when git is unavailable or unhappy.
func gitTopLevel(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd, out := gitCommand(ctx, dir, "rev-parse", "--show-toplevel")
	if err := cmd.Run(); err != nil {
		return ""
	}
	top := strings.TrimSpace(out.String())
	if top == "" || !filepath.IsAbs(filepath.FromSlash(top)) {
		return ""
	}
	return filepath.FromSlash(top)
}

// gitCommand builds the git invocation with stdout captured, stderr
// discarded (never inherited: the MCP server's stdout is JSON-RPC) and an
// environment that cannot prompt or leak the transfer code.
func gitCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.Env = childEnv("GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return cmd, &out
}

// childEnv is os.Environ() minus envTransferCode, with extra KEY=VALUE
// entries replacing any inherited value of the same key. Every subprocess in
// this package builds its environment through it.
func childEnv(extra ...string) []string {
	drop := []string{envTransferCode}
	for _, kv := range extra {
		drop = append(drop, envKey(kv))
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k := envKey(kv)
		skip := false
		for _, d := range drop {
			if strings.EqualFold(k, d) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func envKey(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	return k
}
