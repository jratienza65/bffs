package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// ErrLocked is returned (wrapped) by Lock when the lock is still held by
// someone else after wait has elapsed.
var ErrLocked = errors.New("locked")

// Lock poll cadence: exponential backoff between these bounds.
const (
	lockPollMin = 5 * time.Millisecond
	lockPollMax = 100 * time.Millisecond
)

// Lock acquires a mkdir-style advisory lock at lockDir — the same scheme
// Claude Code's proper-lockfile uses for .claude.json (mkdir <path>.lock;
// EEXIST means held; a holder older than the stale window is presumed dead
// and its directory removed). Lock creates lockDir; on EEXIST it removes the
// directory when its mtime is older than stale and retries at once, otherwise
// it polls with 5–100 ms backoff until wait has elapsed and then fails with
// an error wrapping ErrLocked. A stale of 0 disables the stale check.
//
// The returned release removes the lock directory; calling it more than once
// is harmless. Holders are expected to be brief (well under stale) and must
// never hold the lock across a user prompt.
func Lock(lockDir string, stale, wait time.Duration) (release func(), err error) {
	deadline := time.Now().Add(wait)
	delay := lockPollMin
	staleRetries := 0
	for {
		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			return func() { _ = os.Remove(lockDir) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("lock %q: %w", lockDir, err)
		}

		// Stale holder: remove and retry immediately. Bounded so a clock
		// that keeps producing "old" directories cannot spin us forever.
		if stale > 0 && staleRetries < 3 {
			if info, serr := os.Stat(lockDir); serr == nil && time.Since(info.ModTime()) > stale {
				if rerr := os.Remove(lockDir); rerr == nil || errors.Is(rerr, fs.ErrNotExist) {
					staleRetries++
					continue
				}
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("lock %q: %w", lockDir, ErrLocked)
		}
		time.Sleep(min(delay, remaining))
		delay = min(delay*2, lockPollMax)
	}
}
