package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"time"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Options selects what Plan copies.
type Options struct {
	// ProjectKeys are the projects[<key>] entries to consider; a key the
	// source file does not know is skipped. Empty plans nothing.
	ProjectKeys []string
	// Keys are the entry fields to copy; empty means DefaultKeys. Only
	// DefaultKeys and PermissionKeys are accepted — lastSessionId, the
	// last* metrics, exampleFiles* and the onboarding counters are never
	// synced.
	Keys []string
	// Mirror copies the source values verbatim, downgrades included.
	Mirror bool
	// IncludePermissions adds PermissionKeys to Keys.
	IncludePermissions bool
}

// Change is one field write Plan proposes and Apply performs: set
// projects[ProjectKey][Key] to To. From is the target's current value
// (nil when the field, or the whole entry, is absent); Creates is true when
// the target has no entry for ProjectKey yet, so Apply starts one from
// claudejson.DefaultProjectEntry.
type Change struct {
	ProjectKey string
	Key        string
	From, To   json.RawMessage
	Creates    bool
}

// Lock parameters for Apply: Claude's proper-lockfile treats a lock older
// than lockStale as abandoned; lockWait bounds how long Apply polls before
// giving up. Variables so tests can shorten the wait.
var (
	lockStale = 10 * time.Second
	lockWait  = 3 * time.Second
)

// entry is one decoded projects[<key>] object with its fields kept raw.
type entry map[string]json.RawMessage

var rawTrue = json.RawMessage(`true`)

// Plan compares srcJSON against dstJSON for every key in opts.ProjectKeys
// and returns the writes that would carry the source's answers over under
// T2:
//
//   - The three dialog booleans only go false/absent → true. Folder trust
//     counts as answered on the target when it is effectively trusted —
//     an inherited answer needs no change. A declined external-imports
//     source is copied as declined only onto a target that never answered;
//     a target that accepted stays accepted and a target that declined is
//     never upgraded.
//   - Permission keys (opt-in) merge: array fields gain the source's
//     entries the target lacks (target order first), mcpServers gains the
//     source's servers by name with the target's own entries winning.
//   - Mirror copies every selected source field verbatim instead.
//
// A missing target entry is created from claudejson.DefaultProjectEntry
// (Creates=true) and patched; a target that already has the source's value
// yields no change. Plan reads only; Apply writes.
func Plan(srcJSON, dstJSON string, opts Options) ([]Change, error) {
	keys, err := planKeys(opts)
	if err != nil {
		return nil, err
	}
	if len(opts.ProjectKeys) == 0 {
		return nil, nil
	}
	src, err := readProjects(srcJSON)
	if err != nil {
		return nil, err
	}
	dst, err := readProjects(dstJSON)
	if err != nil {
		return nil, err
	}
	dstFlags := trustFlags(dst)

	var changes []Change
	for _, pk := range opts.ProjectKeys {
		se, ok := src[pk]
		if !ok {
			continue
		}
		de, exists := dst[pk]
		if !exists {
			de = claudejson.DefaultProjectEntry()
		}
		gitRoot, _ := transcripts.GitRoot(pk)
		dstFolder, _ := EffectiveFolderTrust(dstFlags, pk, gitRoot)
		srcExt := externalAnswer(boolOf(se[keyApproved]), boolOf(se[keyShown]))
		dstExt := externalAnswer(boolOf(de[keyApproved]), boolOf(de[keyShown]))

		for _, key := range keys {
			srcRaw, ok := se[key]
			if !ok {
				continue
			}
			dstRaw, has := de[key]
			var to json.RawMessage
			switch {
			case opts.Mirror:
				if !has || !jsonEqual(srcRaw, dstRaw) {
					to = srcRaw
				}
			case key == keyTrust:
				if isTrue(srcRaw) && !trusts(dstFolder) {
					to = rawTrue
				}
			case key == keyApproved:
				if isTrue(srcRaw) && !isTrue(dstRaw) && dstExt != Declined {
					to = rawTrue
				}
			case key == keyShown:
				if isTrue(srcRaw) && !isTrue(dstRaw) {
					if srcExt == Declined {
						if dstExt == Unset {
							to = rawTrue
						}
					} else if dstExt != Declined {
						to = rawTrue
					}
				}
			case key == "mcpServers":
				to = mergeServers(srcRaw, dstRaw, has)
			default:
				to = mergeArray(srcRaw, dstRaw, has)
			}
			if to == nil {
				continue
			}
			c := Change{ProjectKey: pk, Key: key, To: clone(to), Creates: !exists}
			if exists && has {
				c.From = clone(dstRaw)
			}
			changes = append(changes, c)
		}
	}
	return changes, nil
}

// planKeys resolves Options.Keys (+ PermissionKeys) into a deduplicated
// list and rejects anything outside the syncable set.
func planKeys(opts Options) ([]string, error) {
	want := opts.Keys
	if len(want) == 0 {
		want = DefaultKeys
	}
	if opts.IncludePermissions {
		want = append(append([]string(nil), want...), PermissionKeys...)
	}
	allowed := append(append([]string(nil), DefaultKeys...), PermissionKeys...)
	seen := map[string]bool{}
	keys := make([]string, 0, len(want))
	for _, k := range want {
		if seen[k] {
			continue
		}
		ok := false
		for _, a := range allowed {
			if a == k {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("key %q is never synced between accounts; syncable keys: %v", k, allowed)
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys, nil
}

// Apply writes changes (from Plan) into dstJSON under Claude's own lock
// (<dstJSON>.lock, stale after 10 s, waited for up to 3 s), through
// claudejson.UpdateProjects so every other field survives verbatim. A
// missing entry starts from claudejson.DefaultProjectEntry; a field that
// already holds the wanted value is left alone. The lock is held for the
// read-modify-write only — well under a second — and released before Apply
// returns. When a running claude holds the lock past the wait, Apply fails
// with an error wrapping fsutil.ErrLocked and writes nothing.
func Apply(dstJSON string, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	var order []string
	byKey := map[string][]Change{}
	for _, c := range changes {
		if _, ok := byKey[c.ProjectKey]; !ok {
			order = append(order, c.ProjectKey)
		}
		byKey[c.ProjectKey] = append(byKey[c.ProjectKey], c)
	}

	release, err := fsutil.Lock(dstJSON+".lock", lockStale, lockWait)
	if err != nil {
		if errors.Is(err, fsutil.ErrLocked) {
			return fmt.Errorf("%s is being written by a running claude; retry in a moment (%w)", dstJSON, err)
		}
		return err
	}
	defer release()

	_, err = claudejson.UpdateProjects(dstJSON, func(projects map[string]json.RawMessage) (bool, error) {
		changed := false
		for _, pk := range order {
			e := entry(claudejson.DefaultProjectEntry())
			if raw, ok := projects[pk]; ok {
				var existing entry
				if err := json.Unmarshal(raw, &existing); err != nil {
					return false, fmt.Errorf("%s: projects[%q] is not a JSON object: %w", dstJSON, pk, err)
				}
				if existing != nil {
					e = existing
				}
			}
			touched := false
			for _, c := range byKey[pk] {
				if cur, ok := e[c.Key]; ok && jsonEqual(cur, c.To) {
					continue
				}
				e[c.Key] = clone(c.To)
				touched = true
			}
			if !touched {
				continue
			}
			enc, err := json.Marshal(e)
			if err != nil {
				return false, fmt.Errorf("encode projects[%q]: %w", pk, err)
			}
			projects[pk] = enc
			changed = true
		}
		return changed, nil
	})
	return err
}

// readProjects decodes the projects map of the .claude.json at path with
// every entry's fields kept raw. A missing file, or a projects field that
// is not an object, is empty; entries that are not objects are skipped.
func readProjects(path string) (map[string]entry, error) {
	out := map[string]entry{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(doc["projects"], &projects); err != nil {
		return out, nil
	}
	for key, v := range projects {
		var e entry
		if err := json.Unmarshal(v, &e); err != nil || e == nil {
			continue
		}
		out[key] = e
	}
	return out, nil
}

// trustFlags is the ProjectFlags view of decoded entries — enough for
// EffectiveFolderTrust's ancestor walk.
func trustFlags(entries map[string]entry) map[string]claudejson.ProjectFlags {
	flags := make(map[string]claudejson.ProjectFlags, len(entries))
	for key, e := range entries {
		flags[key] = claudejson.ProjectFlags{
			TrustAccepted:                boolOf(e[keyTrust]),
			ExternalIncludesApproved:     boolOf(e[keyApproved]),
			ExternalIncludesWarningShown: boolOf(e[keyShown]),
		}
	}
	return flags
}

// mergeArray returns the target array extended with the source's items it
// lacks (items compared by compact JSON), a raw copy when the target has
// no such field and the source's is non-empty, or nil when nothing would
// change or the shapes differ (the target then wins).
func mergeArray(src, dst json.RawMessage, has bool) json.RawMessage {
	var si, di []json.RawMessage
	if !has {
		if json.Unmarshal(src, &si) == nil && len(si) == 0 {
			return nil // nothing to carry over
		}
		return src
	}
	if json.Unmarshal(src, &si) != nil || json.Unmarshal(dst, &di) != nil {
		return nil
	}
	seen := make(map[string]bool, len(di))
	for _, d := range di {
		seen[compact(d)] = true
	}
	merged := di
	added := false
	for _, s := range si {
		k := compact(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		merged = append(merged, s)
		added = true
	}
	if !added {
		return nil
	}
	if merged == nil {
		merged = []json.RawMessage{}
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return enc
}

// mergeServers returns the target's mcpServers map extended with the
// source's servers it lacks by name (the target's own definitions win), a
// raw copy when the target has no such field and the source's is non-empty,
// or nil when nothing would change or the shapes differ.
func mergeServers(src, dst json.RawMessage, has bool) json.RawMessage {
	var sm, dm map[string]json.RawMessage
	if !has {
		if json.Unmarshal(src, &sm) == nil && len(sm) == 0 {
			return nil // nothing to carry over
		}
		return src
	}
	if json.Unmarshal(src, &sm) != nil || json.Unmarshal(dst, &dm) != nil {
		return nil
	}
	if dm == nil {
		dm = map[string]json.RawMessage{}
	}
	added := false
	for name, v := range sm {
		if _, ok := dm[name]; ok {
			continue
		}
		dm[name] = v
		added = true
	}
	if !added {
		return nil
	}
	enc, err := json.Marshal(dm)
	if err != nil {
		return nil
	}
	return enc
}

// boolOf returns the decoded boolean, or nil when raw is absent, null or
// not a JSON boolean.
func boolOf(raw json.RawMessage) *bool {
	var b *bool
	if raw == nil || json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return b
}

func isTrue(raw json.RawMessage) bool {
	b := boolOf(raw)
	return b != nil && *b
}

// jsonEqual compares two raw values by decoded value, so formatting and
// key order do not count as a difference.
func jsonEqual(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(va, vb)
}

// compact is the identity key of one array item.
func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func clone(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
