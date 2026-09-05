// Package fsutil holds the small set of filesystem primitives the
// session/memory features build on: torn-free writes, cross-device-safe
// moves, mtime stamping, device identity, and a mkdir-style advisory lock
// compatible with the one Claude Code takes on .claude.json.
//
// It is a leaf package (no internal imports) so that every other package —
// including ones that must stay subprocess- and store-free — can use it.
// Nothing here is Claude-specific; the only convention baked in is the
// ".part" intermediate that MoveFile/MoveDir use for cross-device moves.
package fsutil

import (
	"os"
	"time"
)

// Touch sets both the access and modification time of path to mtime. It is
// the one place bffs adjusts timestamps, so callers that must reason about
// Claude's retention sweep (transcript mtimes) go through a single name.
func Touch(path string, mtime time.Time) error {
	return os.Chtimes(path, mtime, mtime)
}
