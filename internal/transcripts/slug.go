// Package transcripts is bffs's read model of Claude Code's on-disk session
// and auto-memory tree: how a working directory becomes a projects/<slug>
// directory, which config dirs own which projects/ pool (Roots), which
// entries inside a pool are not sessions (IsReserved), which sessions are
// open in a running claude (Live), and the few settings that change how
// Claude sweeps or places files (CleanupPeriodDays, MemoryDirFor).
//
// Every rule here is pinned to Claude Code 2.1.259 and documented in
// docs/plans/research/disk.md. The package never writes into Claude's tree;
// it only reads it and computes paths for the packages that do.
//
// Dependency rule: transcripts may import store, sessions, claudejson,
// fsutil and imports — never usage, cmd or mcpserver.
package transcripts

import (
	"errors"
	"unicode/utf16"
)

const (
	// ProjectsSubdir is the directory under a Claude config dir holding one
	// <slug>/ directory per working directory claude was launched in.
	ProjectsSubdir = "projects"

	// MemorySubdir is the auto-memory directory inside projects/<slug>/.
	// Its slug is keyed by the project key (git root), not the session cwd.
	MemorySubdir = "memory"

	// RuntimeSessionsSubdir is Claude's own liveness directory under a config
	// dir: one <pid>.json per running claude (plus <pid>.<hash>.key files).
	// It is unrelated to bffs's sessions/<account>/ directory.
	RuntimeSessionsSubdir = "sessions"

	// MaxSlugLen is the longest slug Claude uses verbatim. Beyond it Claude
	// appends a hash of the path that bffs does not reproduce.
	MaxSlugLen = 200
)

var (
	// ErrSlugTooLong is returned by Slug for a path whose slug would exceed
	// MaxSlugLen: Claude names that project dir with a truncated slug plus a
	// hash bffs cannot compute, so the caller must scan instead of predict.
	ErrSlugTooLong = errors.New("slug longer than 200 characters; claude appends a hash bffs does not reproduce")

	// ErrAmbiguousSession is returned when a session id prefix matches more
	// than one session.
	ErrAmbiguousSession = errors.New("ambiguous session id")

	// ErrMemoryDirOverridden is wrapped by MemoryDirFor when
	// EnvRemoteMemoryDir redirects Claude's auto-memory tree to a layout
	// whose slug bffs has not verified. Since M10 a settings
	// autoMemoryDirectory and EnvCoworkMemoryPathOverride are resolved
	// instead of refused.
	ErrMemoryDirOverridden = errors.New("auto-memory directory is overridden")
)

// Slug converts an absolute, already-normalised path (store.NormalizePath
// output, or ProjectKey for memory) into Claude's projects/ directory name:
// every UTF-16 code unit outside [A-Za-z0-9] becomes "-", so a non-ASCII
// BMP character yields one dash and an astral character (a surrogate pair)
// yields two. A result longer than MaxSlugLen is ErrSlugTooLong.
func Slug(realPath string) (string, error) {
	if realPath == "" {
		return "", errors.New("path is empty")
	}
	units := utf16.Encode([]rune(realPath))
	buf := make([]byte, len(units))
	for i, u := range units {
		switch {
		case u >= 'a' && u <= 'z', u >= 'A' && u <= 'Z', u >= '0' && u <= '9':
			buf[i] = byte(u)
		default:
			buf[i] = '-'
		}
	}
	if len(buf) > MaxSlugLen {
		return "", ErrSlugTooLong
	}
	return string(buf), nil
}
