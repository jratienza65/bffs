package imports

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var (
	fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	idA      = "0f4b6e2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b"
	idB      = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b"
)

func sample(id string, at time.Time, sids ...string) Record {
	r := Record{
		BundleID:   id,
		Kind:       KindImport,
		ImportedAt: at,
		Account:    "work",
		DestRoot:   "/Users/me/.claude/projects",
		Source: Source{
			Hostname: "mac-a", User: "me", Home: "/Users/me", OS: "darwin",
			Account: "personal", BFFSVersion: "0.9.0", ClaudeVersion: "2.1.259",
		},
		Memories: []Memory{{OldCwd: "/Users/me/src/x", Dir: "/Users/me/.claude/projects/-Users-me-src-x/memory", Status: StatusPlaced}},
		Mapping:  []Mapping{{Old: "/home/me", New: "/Users/me"}},
	}
	for _, sid := range sids {
		r.Sessions = append(r.Sessions, Session{
			ID: sid, OldCwd: "/home/me/src/x", OldSlug: "-home-me-src-x",
			NewCwd: "/Users/me/src/x", Slug: "-Users-me-src-x", Title: "fix the thing",
			GitRemote: "git@github.com:me/x.git", Status: StatusRehomed,
			OrigMtime: at.Add(-48 * time.Hour),
		})
	}
	return r
}

func TestPath(t *testing.T) {
	got := Path("/cfg", idA)
	want := filepath.Join("/cfg", "imports", idA+".json")
	if got != want {
		t.Fatalf("Path: want %q, got %q", want, got)
	}
}

func TestValidateBundleID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{idA, true},
		{idB, true},
		{"", false},
		{"../x", false},
		{"0F4B6E2A-1C3D-4E5F-8A9B-0C1D2E3F4A5B", false}, // uppercase
		{"local-" + idA, false},                         // no prefixes: the id must be the manifest's uuid verbatim
		{"remote-" + idA, false},
		{idA + "/x", false},
		{idA + ".json", false},
		{"0f4b6e2a1c3d4e5f8a9b0c1d2e3f4a5b", false}, // no dashes
	}
	for _, c := range cases {
		err := ValidateBundleID(c.id)
		if (err == nil) != c.ok {
			t.Errorf("ValidateBundleID(%q): ok=%v, err=%v", c.id, c.ok, err)
		}
	}
}

func TestLoadMissingDir(t *testing.T) {
	recs, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if recs != nil {
		t.Fatalf("want nil for a missing dir, got %v", recs)
	}
}

func TestSaveRoundTripAndPerms(t *testing.T) {
	cfg := t.TempDir()
	want := sample(idA, fixedNow, "11111111-1111-4111-8111-111111111111")

	if err := Save(cfg, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(Path(cfg, idA))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("perm: want 0600, got %o", perm)
		}
		dinfo, _ := os.Stat(filepath.Join(cfg, Subdir))
		if perm := dinfo.Mode().Perm(); perm != 0o700 {
			t.Fatalf("dir perm: want 0700, got %o", perm)
		}
	}
	raw, _ := os.ReadFile(Path(cfg, idA))
	for _, key := range []string{`"bundle_id"`, `"imported_at"`, `"dest_root"`, `"old_cwd"`, `"orig_mtime"`, `"git_remote"`, `"bffs_version"`, `"claude_version"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("on-disk record missing snake_case key %s", key)
		}
	}

	recs, err := Load(cfg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	got := recs[0]
	if got.BundleID != want.BundleID || got.Kind != want.Kind || got.Account != want.Account || got.DestRoot != want.DestRoot {
		t.Fatalf("header mismatch: got %+v", got)
	}
	if !got.ImportedAt.Equal(want.ImportedAt) {
		t.Fatalf("imported_at: want %v, got %v", want.ImportedAt, got.ImportedAt)
	}
	if got.Source != want.Source {
		t.Fatalf("source: want %+v, got %+v", want.Source, got.Source)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].ID != want.Sessions[0].ID || got.Sessions[0].Status != StatusRehomed ||
		!got.Sessions[0].OrigMtime.Equal(want.Sessions[0].OrigMtime) || got.Sessions[0].GitRemote != want.Sessions[0].GitRemote {
		t.Fatalf("sessions: want %+v, got %+v", want.Sessions, got.Sessions)
	}
	if len(got.Memories) != 1 || got.Memories[0] != want.Memories[0] {
		t.Fatalf("memories: want %+v, got %+v", want.Memories, got.Memories)
	}
	if len(got.Mapping) != 1 || got.Mapping[0] != want.Mapping[0] {
		t.Fatalf("mapping: want %+v, got %+v", want.Mapping, got.Mapping)
	}
}

func TestSaveReplacesExisting(t *testing.T) {
	cfg := t.TempDir()
	r := sample(idA, fixedNow, "11111111-1111-4111-8111-111111111111")
	if err := Save(cfg, r); err != nil {
		t.Fatal(err)
	}
	r.Account = "personal"
	if err := Save(cfg, r); err != nil {
		t.Fatal(err)
	}
	recs, _ := Load(cfg)
	if len(recs) != 1 || recs[0].Account != "personal" {
		t.Fatalf("want one replaced record, got %+v", recs)
	}
}

func TestSaveRejectsBadRecords(t *testing.T) {
	cfg := t.TempDir()
	bad := sample("../escape", fixedNow)
	if err := Save(cfg, bad); err == nil || !strings.Contains(err.Error(), "invalid bundle id") {
		t.Fatalf("want invalid bundle id error, got %v", err)
	}
	unknownKind := sample(idA, fixedNow)
	unknownKind.Kind = "sync"
	if err := Save(cfg, unknownKind); err == nil || !strings.Contains(err.Error(), "invalid record kind") {
		t.Fatalf("want invalid kind error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, Subdir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing should be created on a rejected Save: %v", err)
	}
}

func TestLoadSkipsCorruptFiles(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, Subdir)
	if err := Save(cfg, sample(idB, fixedNow.Add(time.Hour), "22222222-2222-4222-8222-222222222222")); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg, sample(idA, fixedNow, "11111111-1111-4111-8111-111111111111")); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "33333333-3333-4333-8333-333333333333.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	noID := filepath.Join(dir, "44444444-4444-4444-8444-444444444444.json")
	if err := os.WriteFile(noID, []byte(`{"kind":"import"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not records: a temp file left by an interrupted AtomicWrite, a stray
	// file with another extension, a subdirectory.
	for _, name := range []string{idA + ".json.tmp.123", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	recs, skipped, err := LoadAll(cfg)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d: %+v", len(recs), recs)
	}
	// Oldest first regardless of directory order.
	if recs[0].BundleID != idA || recs[1].BundleID != idB {
		t.Fatalf("order: want [%s %s], got [%s %s]", idA, idB, recs[0].BundleID, recs[1].BundleID)
	}
	if len(skipped) != 2 {
		t.Fatalf("want 2 skipped, got %+v", skipped)
	}
	paths := map[string]bool{}
	for _, s := range skipped {
		paths[s.Path] = true
		if s.Err == nil {
			t.Errorf("skipped %s has nil error", s.Path)
		}
	}
	if !paths[corrupt] || !paths[noID] {
		t.Fatalf("skipped set: got %v", paths)
	}

	// Load itself hides the skips.
	plain, err := Load(cfg)
	if err != nil || len(plain) != 2 {
		t.Fatalf("Load: %v, %d records", err, len(plain))
	}
}

func TestExists(t *testing.T) {
	cfg := t.TempDir()
	if _, ok := Exists(cfg, idA); ok {
		t.Fatal("nothing saved yet: want absent")
	}
	if _, ok := Exists(cfg, "../x"); ok {
		t.Fatal("invalid id: want absent")
	}

	r := sample(idA, fixedNow, "11111111-1111-4111-8111-111111111111")
	if err := Save(cfg, r); err != nil {
		t.Fatal(err)
	}
	got, ok := Exists(cfg, idA)
	if !ok {
		t.Fatal("saved record: want present")
	}
	if got.BundleID != idA || got.Account != "work" || len(got.Sessions) != 1 {
		t.Fatalf("Exists returned %+v", got)
	}

	if err := os.WriteFile(Path(cfg, idB), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok = Exists(cfg, idB)
	if !ok {
		t.Fatal("corrupt record file must still count as present")
	}
	if got.BundleID != idB {
		t.Fatalf("corrupt record should carry the id: %+v", got)
	}
}

func TestBySession(t *testing.T) {
	older := sample(idA, fixedNow, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	newer := sample(idB, fixedNow.Add(time.Hour), "22222222-2222-4222-8222-222222222222", "")
	newer.Sessions[0].Status = StatusPending
	recs := []Record{older, newer}

	idx := BySession(recs)
	if len(idx) != 2 {
		t.Fatalf("want 2 ids (empty id ignored), got %d: %v", len(idx), idx)
	}
	ref := idx["11111111-1111-4111-8111-111111111111"]
	if ref.Record == nil || ref.Session == nil || ref.Record.BundleID != idA || ref.Session.Status != StatusRehomed {
		t.Fatalf("unique sid: got %+v", ref)
	}
	dup := idx["22222222-2222-4222-8222-222222222222"]
	if dup.Record.BundleID != idB || dup.Session.Status != StatusPending {
		t.Fatalf("duplicate sid should resolve to the later record: got %s/%s", dup.Record.BundleID, dup.Session.Status)
	}
	// The refs alias the slice: writes through them are visible in recs.
	dup.Session.Status = StatusRehomed
	if recs[1].Sessions[0].Status != StatusRehomed {
		t.Fatal("SessionRef must point into the caller's slice")
	}
	if BySession(nil) == nil {
		t.Fatal("BySession(nil) must return an empty, non-nil map")
	}
}
