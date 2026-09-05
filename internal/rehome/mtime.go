package rehome

import "time"

// MtimePolicy says how a commit stamps what it writes (plan §9.7). Claude
// sweeps transcripts, sidecar files, file-history, plans and tasks whose
// mtime is older than CleanupPeriodDays (transcripts.CleanupPeriodDays;
// 0 means it never sweeps). Now is the clock every stamp is taken from —
// a zero value means time.Now(). PreserveSidecars keeps the source mtimes
// on sidecar, file-history, plan and task files instead of refreshing them
// to Now (the --preserve-mtimes opt-out; such files are then swept when
// older than the window).
type MtimePolicy struct {
	CleanupPeriodDays int
	Now               time.Time
	PreserveSidecars  bool
}

// now returns the policy clock.
func (p MtimePolicy) now() time.Time {
	if p.Now.IsZero() {
		return time.Now()
	}
	return p.Now
}

// TranscriptFloor is the oldest mtime an imported transcript may carry:
// Now − CleanupPeriodDays/2 (15 days at Claude's default of 30), so a
// session that was old on the source machine still has half the retention
// window to be resumed here while relative order among imports is kept.
// ok is false when CleanupPeriodDays is 0 — Claude never sweeps, so
// original mtimes are preserved without a clamp.
func TranscriptFloor(p MtimePolicy) (floor time.Time, ok bool) {
	if p.CleanupPeriodDays <= 0 {
		return time.Time{}, false
	}
	half := time.Duration(p.CleanupPeriodDays) * 24 * time.Hour / 2
	return p.now().Add(-half), true
}

// transcriptMtime returns the mtime a transcript with original mtime orig
// lands with — max(orig, floor) when a floor applies, else orig — and
// whether the floor raised it. A zero orig (unknown) counts as Now.
func transcriptMtime(orig time.Time, p MtimePolicy) (mtime time.Time, raised bool) {
	if orig.IsZero() {
		orig = p.now()
	}
	floor, ok := TranscriptFloor(p)
	if ok && floor.After(orig) {
		return floor, true
	}
	return orig, false
}

// artifactMtime returns the mtime a sidecar, file-history, plan or task
// file lands with: the source's own mtime under PreserveSidecars, else Now.
func artifactMtime(src time.Time, p MtimePolicy) time.Time {
	if p.PreserveSidecars {
		return src
	}
	return p.now()
}
