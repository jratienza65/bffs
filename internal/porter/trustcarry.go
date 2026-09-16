package porter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// Lock parameters for the lastSessionId write: Claude's proper-lockfile
// treats a lock older than 10 s as abandoned.
const (
	claudeJSONLockStale = 10 * time.Second
	claudeJSONLockWait  = 3 * time.Second
)

// accountJSON is the .claude.json --carry-trust and --set-last-session
// write: the destination root's own file for a full-isolation root, the
// chosen oauth account's file under partial isolation (every account
// reads its own copy), else the home file.
func accountJSON(cfgDir string, o ImportOptions, dest transcripts.Root) (string, error) {
	if dest.Owner != "" || o.Account == "" || o.Account == transcripts.HomeName {
		return dest.ClaudeJSON, nil
	}
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return "", err
	}
	if acc, ok := accs.Get(o.Account); ok && acc.Type == store.TypeOAuth {
		return filepath.Join(sessions.Dir(cfgDir, o.Account), claudejson.Filename), nil
	}
	return dest.ClaudeJSON, nil
}

// trustTarget is one mapped directory whose sessions landed, with the
// trust answers the source recorded for it.
type trustTarget struct {
	cwd   string
	trust bundle.TrustInfo
}

// trustTargets collects the confirmed mapped directories whose sessions
// landed and carry a source_trust block (an accepting block wins when
// entries of one directory disagree), sorted by directory.
func trustTargets(plans []sessionPlan, landed map[string]bool) []trustTarget {
	byCwd := map[string]trustTarget{}
	for _, sp := range plans {
		p := sp.place
		if sp.status != planImport || !landed[sp.entry.SessionID] || !p.CarryTrust || p.Mode != PlaceMapped || !p.Confirmed || sp.entry.SourceTrust == nil {
			continue
		}
		cur, ok := byCwd[p.NewCwd]
		if !ok || (!cur.trust.Accepted && sp.entry.SourceTrust.Accepted) {
			byCwd[p.NewCwd] = trustTarget{cwd: p.NewCwd, trust: *sp.entry.SourceTrust}
		}
	}
	out := make([]trustTarget, 0, len(byCwd))
	for _, t := range byCwd {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cwd < out[j].cwd })
	return out
}

// carryTrust writes the source's three trust answers for every mapped,
// confirmed directory that landed into jsonPath at
// transcripts.ProjectKey(newCwd) under plan §9.10: the answers are
// spelled into a temporary .claude.json in staging and carried with
// trust.Plan/trust.Apply, so the T2 rules (never downgrade, never override
// a decline) and T4 (Claude's lock, held briefly) hold exactly as for
// `bffs trust sync`. Returns the project keys handled; a failure is a
// warning — the sessions have landed already.
func carryTrust(jsonPath, stagingDir string, plans []sessionPlan, landed map[string]bool, warn func(string, ...any)) []string {
	targets := trustTargets(plans, landed)
	if len(targets) == 0 {
		return nil
	}
	var carried []string
	for i, t := range targets {
		key, err := transcripts.ProjectKey(t.cwd)
		if err != nil {
			warn("trust for %s not carried: %v", t.cwd, err)
			continue
		}
		src := filepath.Join(stagingDir, fmt.Sprintf("source-trust-%d.json", i))
		if err := writeTrustSource(src, key, t.trust); err != nil {
			warn("trust for %s not carried: %v", t.cwd, err)
			continue
		}
		changes, err := trust.Plan(src, jsonPath, trust.Options{ProjectKeys: []string{key}})
		_ = os.Remove(src)
		if err != nil {
			warn("trust for %s not carried: %v", t.cwd, err)
			continue
		}
		if err := trust.Apply(jsonPath, changes); err != nil {
			warn("trust for %s not carried: %v", t.cwd, err)
			continue
		}
		carried = append(carried, key)
	}
	return carried
}

// writeTrustSource writes a minimal .claude.json holding one project
// entry with the three dialog answers, the shape trust.Plan reads.
func writeTrustSource(path, key string, t bundle.TrustInfo) error {
	doc := map[string]any{
		"projects": map[string]any{
			key: map[string]any{
				"hasTrustDialogAccepted":                  t.Accepted,
				"hasClaudeMdExternalIncludesApproved":     t.ExternalIncludesApproved,
				"hasClaudeMdExternalIncludesWarningShown": t.ExternalIncludesWarningShown,
			},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(path, data, 0o600)
}

// setLastSessions points projects[<key>].lastSessionId of jsonPath at the
// newest landed session (by the entry's last activity) of every
// identity or mapped directory, under Claude's lock (plan §9.10, §7.2).
// Returns directory → session id for what was written; a failure is a
// warning.
func setLastSessions(jsonPath string, plans []sessionPlan, landed map[string]bool, warn func(string, ...any)) map[string]string {
	newest := map[string]sessionPlan{}
	for _, sp := range plans {
		if sp.status != planImport || !landed[sp.entry.SessionID] || sp.place.Mode == PlaceAsIs || sp.place.NewCwd == "" {
			continue
		}
		if cur, ok := newest[sp.place.NewCwd]; !ok || sp.entry.Last.After(cur.entry.Last) {
			newest[sp.place.NewCwd] = sp
		}
	}
	if len(newest) == 0 {
		return nil
	}
	release, err := fsutil.Lock(jsonPath+".lock", claudeJSONLockStale, claudeJSONLockWait)
	if err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			warn("lastSessionId not set: %s is being written by a running claude; retry with bffs rehome --set-last-session (%v)", jsonPath, err)
		} else {
			warn("lastSessionId not set: %v", err)
		}
		return nil
	}
	defer release()
	cwds := make([]string, 0, len(newest))
	for cwd := range newest {
		cwds = append(cwds, cwd)
	}
	sort.Strings(cwds)
	out := map[string]string{}
	for _, cwd := range cwds {
		sid := newest[cwd].entry.SessionID
		key, err := transcripts.ProjectKey(cwd)
		if err != nil {
			warn("lastSessionId for %s not set: %v", cwd, err)
			continue
		}
		if err := claudejson.SetLastSessionID(jsonPath, key, sid); err != nil {
			warn("lastSessionId for %s not set: %v", cwd, err)
			continue
		}
		out[cwd] = sid
	}
	return out
}
