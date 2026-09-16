// Package usage estimates per-account Claude usage from local artifacts:
// Claude Code's transcript files (token counts, timestamps, limit events) and
// the bffs launch log (account attribution). Everything here is heuristic —
// transcripts carry no account identity, so sessions are attributed by, in
// order: the config tree they live in (full-isolation accounts), the
// lastSessionId metadata each oauth account's own .claude.json records, and
// launch-log correlation by working directory and time. What cannot be
// attributed is reported honestly as such, never guessed.
package usage

import (
	"os"
	"path/filepath"
	"time"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

const (
	// DefaultHorizon bounds the long window and the transcript-file mtime scan.
	DefaultHorizon = 7 * 24 * time.Hour
	// ShortWindow approximates Anthropic's rolling session-limit window.
	ShortWindow = 5 * time.Hour
)

// Weighted-burn weights, roughly proportional to API price ratios. A
// heuristic for comparing accounts, not billing truth: raw token counts are
// dominated ~100x by cheap cache reads, so unweighted totals would rank
// accounts by cache traffic instead of actual consumption.
const (
	weightInput         = 1.0
	weightOutput        = 5.0
	weightCacheCreation = 1.25
	weightCacheRead     = 0.1
)

// Attribution source labels (keys of AccountUsage.Attribution).
const (
	SrcRoot        = "root"
	SrcLastSession = "last-session"
	SrcLaunchLog   = "launch-log"
)

// Options configures Collect. The zero value means: now, DefaultHorizon,
// the real ~/.claude.
type Options struct {
	Now           time.Time
	Horizon       time.Duration
	HomeClaudeDir string
}

// Tokens is a usage sum over some set of assistant records.
type Tokens struct {
	Input, Output, CacheCreation, CacheRead int64
	Messages                                int
}

func (t Tokens) Total() int64 {
	return t.Input + t.Output + t.CacheCreation + t.CacheRead
}

// Weighted collapses the four token kinds into one comparable burn score.
func (t Tokens) Weighted() float64 {
	return weightInput*float64(t.Input) +
		weightOutput*float64(t.Output) +
		weightCacheCreation*float64(t.CacheCreation) +
		weightCacheRead*float64(t.CacheRead)
}

func (t Tokens) add(o Tokens) Tokens {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheCreation += o.CacheCreation
	t.CacheRead += o.CacheRead
	t.Messages += o.Messages
	return t
}

// WindowStats is a Tokens sum plus the number of sessions contributing to it.
type WindowStats struct {
	Tokens
	Sessions int
}

// merge folds one session's window sum in; empty sums don't count a session.
func (w WindowStats) merge(t Tokens) WindowStats {
	if t.Messages == 0 {
		return w
	}
	w.Tokens = w.Tokens.add(t)
	w.Sessions++
	return w
}

// LimitEvent is a detected "you've hit your ... limit" API error.
type LimitEvent struct {
	TS      time.Time
	Kind    string    // "session" | "weekly" | "unknown"
	ResetAt time.Time // zero when the reset time could not be parsed
}

// AccountUsage is one account's heuristic usage picture.
type AccountUsage struct {
	Name, Type  string
	Tier        Tier
	LastUsed    time.Time // max(latest launch event, latest attributed record)
	Short, Long WindowStats
	ByModel     map[string]Tokens // long window
	ByDay       map[string]Tokens // "2006-01-02" UTC, long window
	Attribution map[string]int    // source label → attributed session count
	Limit       *LimitEvent       // most recent attributed limit event
}

// Report is the full Collect result.
type Report struct {
	Now               time.Time
	Horizon           time.Duration
	Accounts          []AccountUsage // accounts.toml order (sorted by name)
	Unattributed      WindowStats    // long window
	UnattributedShort WindowStats
	Suggested         string // account with the most estimated headroom; "" if none
}

// Collect scans in-horizon transcripts, attributes sessions to accounts, and
// assembles the per-account picture.
func Collect(cfgDir string, opts Options) (Report, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	horizon := opts.Horizon
	if horizon <= 0 {
		horizon = DefaultHorizon
	}
	home := opts.HomeClaudeDir
	if home == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return Report{}, err
		}
		home = filepath.Join(uh, ".claude")
	}

	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return Report{}, err
	}
	// The launch log is heuristic input; a broken one degrades attribution,
	// it doesn't fail the report.
	events, _ := usagelog.Read(cfgDir)

	scanned := scan(cfgDir, accs, home, now, horizon)
	lastIDs := lastSessionOwners(cfgDir, accs)

	rep := Report{Now: now, Horizon: horizon}
	per := map[string]*AccountUsage{}
	for _, name := range accs.Names() {
		acc := accs.Accounts[name]
		per[name] = &AccountUsage{
			Name:        name,
			Type:        string(acc.Type),
			Tier:        ParseTier(acc.OAuthAccountMeta),
			ByModel:     map[string]Tokens{},
			ByDay:       map[string]Tokens{},
			Attribution: map[string]int{},
		}
	}

	for _, sess := range scanned {
		owner, src := attributeSession(sess, lastIDs, events)
		au, known := per[owner]
		if owner == "" || !known {
			rep.Unattributed = rep.Unattributed.merge(sess.long)
			rep.UnattributedShort = rep.UnattributedShort.merge(sess.short)
			continue
		}
		au.Attribution[src]++
		au.Short = au.Short.merge(sess.short)
		au.Long = au.Long.merge(sess.long)
		for m, t := range sess.byModel {
			au.ByModel[m] = au.ByModel[m].add(t)
		}
		for d, t := range sess.byDay {
			au.ByDay[d] = au.ByDay[d].add(t)
		}
		if sess.lastTS.After(au.LastUsed) {
			au.LastUsed = sess.lastTS
		}
		for _, le := range sess.limits {
			if au.Limit == nil || le.TS.After(au.Limit.TS) {
				l := le
				au.Limit = &l
			}
		}
	}

	// A launch is usage even when the session left no (attributable) transcript.
	for _, e := range events {
		if au, ok := per[e.Account]; ok && e.TS.After(au.LastUsed) {
			au.LastUsed = e.TS
		}
	}

	for _, name := range accs.Names() {
		rep.Accounts = append(rep.Accounts, *per[name])
	}
	rep.Suggested = suggest(rep.Accounts, now)
	return rep, nil
}

// lastSessionOwners maps sessionId → claiming accounts, from every oauth
// account's own .claude.json (projects.<path>.lastSessionId).
func lastSessionOwners(cfgDir string, accs store.Accounts) map[string][]string {
	owners := map[string][]string{}
	for _, name := range accs.Names() {
		if accs.Accounts[name].Type != store.TypeOAuth {
			continue
		}
		ids, err := claudejson.LastSessionIDs(filepath.Join(sessions.Dir(cfgDir, name), claudejson.Filename))
		if err != nil {
			continue
		}
		for id := range ids {
			owners[id] = append(owners[id], name)
		}
	}
	return owners
}

// suggest picks the oauth account with the most estimated headroom: not
// currently limited, lowest short-window weighted burn; ties broken by long
// window, then least-recently used (never-used first), then name.
func suggest(accounts []AccountUsage, now time.Time) string {
	var best *AccountUsage
	for i := range accounts {
		a := &accounts[i]
		if a.Type != string(store.TypeOAuth) || limited(a, now) {
			continue
		}
		if best == nil || moreHeadroom(a, best) {
			best = a
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// limited reports whether the account is likely still inside a limit window:
// its reset time is in the future, or (reset unknown) the event is fresher
// than one session window.
func limited(a *AccountUsage, now time.Time) bool {
	if a.Limit == nil {
		return false
	}
	if !a.Limit.ResetAt.IsZero() {
		return a.Limit.ResetAt.After(now)
	}
	return now.Sub(a.Limit.TS) < ShortWindow
}

func moreHeadroom(a, b *AccountUsage) bool {
	if aw, bw := a.Short.Weighted(), b.Short.Weighted(); aw != bw {
		return aw < bw
	}
	if aw, bw := a.Long.Weighted(), b.Long.Weighted(); aw != bw {
		return aw < bw
	}
	if !a.LastUsed.Equal(b.LastUsed) {
		return a.LastUsed.Before(b.LastUsed)
	}
	return a.Name < b.Name
}
