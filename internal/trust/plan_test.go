package trust

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/fsutil"
)

// keyDir creates a non-git project directory under a fresh temp dir and
// returns its path — Plan looks the git root up on disk.
func keyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work", "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func keysOf(changes []Change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Key)
	}
	return out
}

func mustPlan(t *testing.T, src, dst string, opts Options) []Change {
	t.Helper()
	changes, err := Plan(src, dst, opts)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return changes
}

func decodeAny(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func readAny(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return decodeAny(t, raw)
}

func entryOf(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	projects, ok := doc["projects"].(map[string]any)
	if !ok {
		t.Fatalf("projects is %T, want object", doc["projects"])
	}
	entry, ok := projects[key].(map[string]any)
	if !ok {
		t.Fatalf("projects[%q] is %T, want object", key, projects[key])
	}
	return entry
}

func TestPlanCreatesEntryAndUpgrades(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(true, true, true, `"lastSessionId": "sid-src", "lastCost": 1.5`)})
	dst := filepath.Join(t.TempDir(), claudejson.Filename) // missing file

	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}})
	if got := keysOf(changes); !reflect.DeepEqual(got, DefaultKeys) {
		t.Fatalf("keys = %v, want %v", got, DefaultKeys)
	}
	for _, c := range changes {
		if c.ProjectKey != key || !c.Creates || c.From != nil || string(c.To) != "true" {
			t.Errorf("change %+v: want Creates, From nil, To true", c)
		}
	}

	if err := Apply(dst, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entry := entryOf(t, readAny(t, dst), key)
	def := claudejson.DefaultProjectEntry()
	if len(entry) != len(def) {
		t.Errorf("created entry has keys %v, want the default shape %v", entry, def)
	}
	for k := range def {
		if _, ok := entry[k]; !ok {
			t.Errorf("created entry lacks default key %q", k)
		}
	}
	for _, k := range DefaultKeys {
		if entry[k] != true {
			t.Errorf("%s = %v, want true", k, entry[k])
		}
	}
	if _, leaked := entry["lastSessionId"]; leaked {
		t.Error("lastSessionId must never be copied")
	}
	if _, leaked := entry["lastCost"]; leaked {
		t.Error("lastCost must never be copied")
	}

	// A second plan against the updated target is empty.
	if again := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}}); len(again) != 0 {
		t.Errorf("re-plan after apply = %+v, want none", again)
	}
}

func TestPlanNeverDowngrades(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(false, false, false, "")})
	dst := jsonFile(t, map[string]string{key: entryJSON(true, true, true, "")})
	if changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("a false source must not touch a true target: %+v", changes)
	}

	// The source not knowing the project is not a downgrade either.
	empty := jsonFile(t, map[string]string{})
	if changes := mustPlan(t, empty, dst, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("unknown source project must plan nothing: %+v", changes)
	}
	// Absent flags in the source are false.
	bare := jsonFile(t, map[string]string{key: `{"allowedTools": []}`})
	if changes := mustPlan(t, bare, dst, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("absent source flags must plan nothing: %+v", changes)
	}
}

func TestPlanNeverUpgradesADecline(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(true, true, true, "")})
	declined := jsonFile(t, map[string]string{key: entryJSON(false, false, true, "")})

	changes := mustPlan(t, src, declined, Options{ProjectKeys: []string{key}})
	if got := keysOf(changes); !reflect.DeepEqual(got, []string{keyTrust}) {
		t.Fatalf("only folder trust may change on a declined target, got %v", got)
	}
	if c := changes[0]; c.Creates || string(c.From) != "false" || string(c.To) != "true" {
		t.Errorf("folder trust change = %+v, want false -> true on an existing entry", c)
	}

	// Mirror overrides the decline — and says so through the change list.
	mirrored := mustPlan(t, src, declined, Options{ProjectKeys: []string{key}, Mirror: true})
	if got := keysOf(mirrored); !reflect.DeepEqual(got, []string{keyTrust, keyApproved}) {
		t.Errorf("mirror changes = %v, want trust + approved (shown already true)", got)
	}
}

func TestPlanDeclinedSource(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(false, false, true, "")})

	// Onto a target that never answered: copied as declined (WarningShown
	// only — Approved stays false so the dialog is silenced, not enabled).
	unset := filepath.Join(t.TempDir(), claudejson.Filename)
	changes := mustPlan(t, src, unset, Options{ProjectKeys: []string{key}})
	if got := keysOf(changes); !reflect.DeepEqual(got, []string{keyShown}) {
		t.Fatalf("declined source onto unset target = %v, want only %s", got, keyShown)
	}
	if err := Apply(unset, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entry := entryOf(t, readAny(t, unset), key)
	if entry[keyApproved] != false || entry[keyShown] != true {
		t.Errorf("target after apply = %v, want declined", entry)
	}

	// Onto a target that accepted: untouched.
	accepted := jsonFile(t, map[string]string{key: entryJSON(false, true, false, "")})
	if changes := mustPlan(t, src, accepted, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("declined source must not touch an accepting target: %+v", changes)
	}
	// Onto a target that declined: nothing to do.
	declined := jsonFile(t, map[string]string{key: entryJSON(false, false, true, "")})
	if changes := mustPlan(t, src, declined, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("declined onto declined must plan nothing: %+v", changes)
	}
}

func TestPlanAcceptedSourceCompletesThePair(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(false, true, true, "")})
	// A target with Approved but no WarningShown gets the harmless half.
	half := jsonFile(t, map[string]string{key: entryJSON(false, true, false, "")})
	if got := keysOf(mustPlan(t, src, half, Options{ProjectKeys: []string{key}})); !reflect.DeepEqual(got, []string{keyShown}) {
		t.Errorf("changes = %v, want only %s", got, keyShown)
	}
}

func TestPlanInheritedTargetNeedsNoChange(t *testing.T) {
	key := keyDir(t)
	parent := filepath.Dir(key)
	src := jsonFile(t, map[string]string{key: entryJSON(true, false, false, "")})
	dst := jsonFile(t, map[string]string{parent: entryJSON(true, false, false, "")})

	if changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("a parent-trusted target must not gain an entry: %+v", changes)
	}
	mirrored := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}, Mirror: true})
	if len(mirrored) != 1 || mirrored[0].Key != keyTrust || !mirrored[0].Creates || string(mirrored[0].To) != "true" {
		t.Errorf("mirror writes the exact key regardless: %+v", mirrored)
	}
}

func TestPlanGitBoundsInheritance(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	key := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(key, 0o700); err != nil {
		t.Fatal(err)
	}
	src := jsonFile(t, map[string]string{key: entryJSON(true, false, false, "")})

	above := jsonFile(t, map[string]string{base: entryJSON(true, false, false, "")})
	if got := keysOf(mustPlan(t, src, above, Options{ProjectKeys: []string{key}})); !reflect.DeepEqual(got, []string{keyTrust}) {
		t.Errorf("trust above the git root does not cover the repo: changes = %v, want [%s]", got, keyTrust)
	}
	inside := jsonFile(t, map[string]string{repo: entryJSON(true, false, false, "")})
	if changes := mustPlan(t, src, inside, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Errorf("a trusted repo root covers its subdirectory: %+v", changes)
	}
}

func TestPlanMirror(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(false, true, true, "")})
	dst := jsonFile(t, map[string]string{key: entryJSON(true, true, true, "")})

	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}, Mirror: true})
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly the downgrade", changes)
	}
	c := changes[0]
	if c.Key != keyTrust || string(c.From) != "true" || string(c.To) != "false" || c.Creates {
		t.Errorf("mirror change = %+v, want %s true -> false", c, keyTrust)
	}
	if err := Apply(dst, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if entry := entryOf(t, readAny(t, dst), key); entry[keyTrust] != false {
		t.Errorf("after mirror apply %s = %v, want false", keyTrust, entry[keyTrust])
	}
}

func TestPlanMultipleProjectsAndMissingSource(t *testing.T) {
	a, b := keyDir(t), keyDir(t)
	src := jsonFile(t, map[string]string{a: entryJSON(true, false, false, ""), b: entryJSON(false, true, true, "")})
	dst := filepath.Join(t.TempDir(), claudejson.Filename)
	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{b, a, filepath.Join(t.TempDir(), "unknown")}})
	want := []string{b + "/" + keyApproved, b + "/" + keyShown, a + "/" + keyTrust}
	var got []string
	for _, c := range changes {
		got = append(got, c.ProjectKey+"/"+c.Key)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %v, want %v (source order of ProjectKeys, unknown skipped)", got, want)
	}
	if err := Apply(dst, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	doc := readAny(t, dst)
	if len(doc["projects"].(map[string]any)) != 2 {
		t.Errorf("projects after apply = %v, want two entries", doc["projects"])
	}
}

func TestPlanRejectsUnsyncableKeys(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(true, true, true, `"lastSessionId": "sid"`)})
	for _, bad := range []string{"lastSessionId", "lastCost", "exampleFiles", "projectOnboardingSeenCount", "history", "oauthAccount"} {
		_, err := Plan(src, src, Options{ProjectKeys: []string{key}, Keys: []string{keyTrust, bad}})
		if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "never synced") {
			t.Errorf("Keys containing %q: got %v, want a refusal", bad, err)
		}
	}
	// Explicit keys restrict the plan; permission keys are accepted by name.
	dst := filepath.Join(t.TempDir(), claudejson.Filename)
	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}, Keys: []string{keyShown, keyShown}})
	if got := keysOf(changes); !reflect.DeepEqual(got, []string{keyShown}) {
		t.Errorf("explicit Keys = %v, want [%s] (deduplicated)", got, keyShown)
	}
	if changes := mustPlan(t, src, dst, Options{}); len(changes) != 0 {
		t.Errorf("no ProjectKeys plans nothing, got %+v", changes)
	}
}

const (
	srcPerms = `"allowedTools": ["A", "B"], "mcpServers": {"x": {"command": "src-x"}, "y": {"command": "src-y"}}, "enabledMcpjsonServers": ["e1"], "disabledMcpjsonServers": ["d1"], "mcpContextUris": []`
	dstPerms = `"allowedTools": ["B", "C"], "mcpServers": {"x": {"command": "dst-x"}}, "enabledMcpjsonServers": ["e1"]`
)

func TestPlanIncludePermissions(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(true, true, true, srcPerms)})
	dst := jsonFile(t, map[string]string{key: entryJSON(true, true, true, dstPerms+`, "lastSessionId": "sid-dst"`)})

	if changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}}); len(changes) != 0 {
		t.Fatalf("permissions must not move without opt-in: %+v", changes)
	}

	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}, IncludePermissions: true})
	got := map[string]string{}
	for _, c := range changes {
		got[c.Key] = string(c.To)
		if c.Creates {
			t.Errorf("%s: Creates on an existing entry", c.Key)
		}
	}
	want := map[string]string{
		"allowedTools":           `["B","C","A"]`,
		"mcpServers":             `{"x":{"command":"dst-x"},"y":{"command":"src-y"}}`,
		"disabledMcpjsonServers": `["d1"]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("permission changes = %v, want %v", got, want)
	}
	for _, c := range changes {
		if c.Key == "disabledMcpjsonServers" && c.From != nil {
			t.Errorf("From for an absent field = %s, want nil", c.From)
		}
		if c.Key == "allowedTools" && string(c.From) != `["B", "C"]` {
			t.Errorf("From for allowedTools = %s", c.From)
		}
	}

	if err := Apply(dst, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entry := entryOf(t, readAny(t, dst), key)
	if entry["lastSessionId"] != "sid-dst" {
		t.Errorf("lastSessionId changed: %v", entry["lastSessionId"])
	}
	servers := entry["mcpServers"].(map[string]any)
	if servers["x"].(map[string]any)["command"] != "dst-x" {
		t.Errorf("target's own server definition lost: %v", servers)
	}
	if _, ok := servers["y"]; !ok {
		t.Errorf("source-only server not added: %v", servers)
	}

	// Mirror copies the permission fields verbatim, target definitions
	// included.
	mirrored := mustPlan(t, src, jsonFile(t, map[string]string{key: entryJSON(true, true, true, dstPerms)}), Options{ProjectKeys: []string{key}, IncludePermissions: true, Mirror: true})
	got = map[string]string{}
	for _, c := range mirrored {
		got[c.Key] = string(c.To)
	}
	want = map[string]string{
		"allowedTools":           `["A", "B"]`,
		"mcpServers":             `{"x": {"command": "src-x"}, "y": {"command": "src-y"}}`,
		"disabledMcpjsonServers": `["d1"]`,
		"mcpContextUris":         `[]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mirror permission changes = %v, want %v", got, want)
	}
}

func TestPlanPermissionShapes(t *testing.T) {
	key := keyDir(t)
	// Legacy string allowedTools on the target: shapes differ, target wins.
	src := jsonFile(t, map[string]string{key: `{"allowedTools": ["A"], "mcpServers": {}, "mcpContextUris": []}`})
	legacy := jsonFile(t, map[string]string{key: `{"allowedTools": "Bash(git:*)"}`})
	changes := mustPlan(t, src, legacy, Options{ProjectKeys: []string{key}, IncludePermissions: true})
	if len(changes) != 0 {
		t.Errorf("shape mismatch / empty source fields must plan nothing: %+v", changes)
	}
	// Onto a missing entry: only non-empty fields are carried.
	missing := filepath.Join(t.TempDir(), claudejson.Filename)
	if got := keysOf(mustPlan(t, src, missing, Options{ProjectKeys: []string{key}, IncludePermissions: true})); !reflect.DeepEqual(got, []string{"allowedTools"}) {
		t.Errorf("changes onto a missing entry = %v, want [allowedTools]", got)
	}
}

func TestApplyLock(t *testing.T) {
	key := keyDir(t)
	src := jsonFile(t, map[string]string{key: entryJSON(true, true, true, "")})
	dst := jsonFile(t, map[string]string{key: entryJSON(false, false, false, `"lastSessionId": "keep"`)})
	changes := mustPlan(t, src, dst, Options{ProjectKeys: []string{key}})
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want 3", changes)
	}
	lockDir := dst + ".lock"

	// A held lock (fresh mtime, well inside the 10 s stale window) makes
	// Apply fail with ErrLocked without touching the file or the lock.
	before, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := lockWait
	lockWait = 50 * time.Millisecond
	t.Cleanup(func() { lockWait = old })
	start := time.Now()
	err = Apply(dst, changes)
	if !errors.Is(err, fsutil.ErrLocked) || !strings.Contains(err.Error(), "being written by a running claude") {
		t.Fatalf("held lock → %v, want ErrLocked with the retry hint", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Apply blocked %v on a held lock, want < 1 s", elapsed)
	}
	if after, _ := os.ReadFile(dst); string(after) != string(before) {
		t.Error("file changed although the lock was held")
	}
	if _, err := os.Stat(lockDir); err != nil {
		t.Errorf("the holder's lock dir must survive: %v", err)
	}
	lockWait = old

	// A stale lock (older than 10 s) is taken over; the write lands and the
	// lock is released — the whole thing well under a second.
	if err := os.Chtimes(lockDir, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := Apply(dst, changes); err != nil {
		t.Fatalf("Apply over a stale lock: %v", err)
	}
	if held := time.Since(start); held >= time.Second {
		t.Errorf("Apply took %v, want < 1 s", held)
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock dir must be gone after Apply: %v", err)
	}
	doc := readAny(t, dst)
	entry := entryOf(t, doc, key)
	for _, k := range DefaultKeys {
		if entry[k] != true {
			t.Errorf("%s = %v after apply, want true", k, entry[k])
		}
	}
	if entry["lastSessionId"] != "keep" || doc["numStartups"] != float64(7) || doc["userID"] != "u-1" {
		t.Errorf("unrelated fields changed: %v", doc)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dst); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("perms = %v, want 0600 preserved", info.Mode().Perm())
		}
	}

	// Re-applying the same changes is a no-op that still releases the lock.
	stamp, _ := os.Stat(dst)
	if err := Apply(dst, changes); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if info, _ := os.Stat(dst); !info.ModTime().Equal(stamp.ModTime()) || info.Size() != stamp.Size() {
		t.Error("an idempotent apply must not rewrite the file")
	}
	if _, err := os.Stat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock dir must be gone after a no-op Apply: %v", err)
	}

	// No changes: nothing happens, not even a file.
	none := filepath.Join(t.TempDir(), claudejson.Filename)
	if err := Apply(none, nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}
	if _, err := os.Stat(none); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Apply with no changes created %s", none)
	}
}

func TestApplyRefusesNonObjectEntry(t *testing.T) {
	key := keyDir(t)
	dst := jsonFile(t, map[string]string{key: `"not an object"`})
	err := Apply(dst, []Change{{ProjectKey: key, Key: keyTrust, To: rawTrue}})
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("want a shape error, got %v", err)
	}
	if _, err := os.Stat(dst + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock dir must be released on error: %v", err)
	}
}
