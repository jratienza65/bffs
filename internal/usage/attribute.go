package usage

import (
	"path/filepath"
	"time"

	"github.com/jratienza65/bffs/internal/usagelog"
)

const (
	// attribSlack tolerates clock skew and write ordering between the launch
	// event and the session's first record.
	attribSlack = 2 * time.Minute
	// attribAmbiguityWindow: two different accounts launched from the same
	// cwd within this span before the session started → refuse to guess.
	attribAmbiguityWindow = 10 * time.Minute
)

// attributeSession decides which account a session belongs to. First tier
// that decides wins; "cannot decide" is the empty account (unattributed) —
// never a guess.
func attributeSession(sess *session, lastIDs map[string][]string, events []usagelog.Event) (account, src string) {
	// 1. Root ownership: the session lives in a full-isolation account's own
	// config tree; nothing else could have written it there.
	if sess.owner != "" {
		return sess.owner, SrcRoot
	}
	// 2. lastSessionId ground truth from the account's own .claude.json.
	// Multiple claimants should be impossible (UUIDs) — treat as corrupt.
	if claimants := lastIDs[sess.id]; len(claimants) == 1 {
		return claimants[0], SrcLastSession
	} else if len(claimants) > 1 {
		return "", ""
	}
	// 3. Launch-log correlation by (cwd, time).
	if sess.cwd == "" || sess.firstTS.IsZero() {
		return "", ""
	}
	cwd := filepath.Clean(sess.cwd)
	winLo := sess.firstTS.Add(-attribAmbiguityWindow)
	winHi := sess.firstTS.Add(attribSlack)
	var best *usagelog.Event
	distinct := map[string]bool{}
	for i := range events {
		e := &events[i]
		// No EvalSymlinks: the shim and claude share one Getwd.
		if filepath.Clean(e.Cwd) != cwd || e.TS.After(winHi) {
			continue
		}
		// A SourceNone launch counts as a distinct "account" here: an
		// unmanaged claude launched nearby makes attribution unsafe.
		if !e.TS.Before(winLo) {
			distinct[e.Account] = true
		}
		if best == nil || e.TS.After(best.TS) {
			best = e
		}
	}
	if len(distinct) >= 2 || best == nil || best.Account == "" {
		return "", ""
	}
	return best.Account, SrcLaunchLog
}
