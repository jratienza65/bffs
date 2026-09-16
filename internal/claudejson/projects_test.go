package claudejson

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// roundTripFixture mixes every JSON value kind at the top level and inside a
// project entry so the round-trip test proves value equality, not formatting.
const roundTripFixture = `{
  "numStartups": 42,
  "theme": "dark",
  "oauthAccount": {"emailAddress": "a@example.com", "accountUuid": "U-1"},
  "userID": "uid-1",
  "tipsHistory": {"intro": 3, "shortcuts": 0},
  "cachedGrowthBookFeatures": {"flag": true, "ratio": 0.75, "nested": [1, "two", null, {"x": 1e21}]},
  "nullish": null,
  "html": "<b>&amp;</b> 'quoted' é",
  "mcpServers": {"bffs": {"type": "stdio", "command": "/opt/bffs/bffs", "args": ["mcp", "serve"]}},
  "projects": {
    "/Users/x/proj-a": {
      "allowedTools": ["Bash(git:*)", "Read"],
      "mcpContextUris": [],
      "mcpServers": {"local": {"type": "stdio", "command": "srv"}},
      "enabledMcpjsonServers": ["one"],
      "disabledMcpjsonServers": [],
      "hasTrustDialogAccepted": false,
      "hasClaudeMdExternalIncludesApproved": false,
      "hasClaudeMdExternalIncludesWarningShown": false,
      "lastSessionId": "sid-a",
      "lastCost": 0.0123,
      "lastDuration": 123456,
      "lastModelUsage": {"claude-x": {"inputTokens": 10, "outputTokens": 20}},
      "exampleFiles": ["a.go", "b.go"],
      "lastTotalInputTokens": 1758000000000
    },
    "/Users/x/prój-b": {
      "hasTrustDialogAccepted": true,
      "lastSessionId": "sid-b"
    }
  }
}`

// decodeAny decodes raw JSON into plain Go values so two documents can be
// compared by value regardless of indentation, key order or escaping.
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

// entryOf pulls projects[key] out of a decoded document.
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

// setEntryField decodes projects[key], sets field to v and re-encodes it —
// the shape every fn passed to UpdateProjects takes.
func setEntryField(t *testing.T, projects map[string]json.RawMessage, key, field string, v any) {
	t.Helper()
	entry := map[string]json.RawMessage{}
	if raw, ok := projects[key]; ok {
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("decode entry %q: %v", key, err)
		}
	}
	if err := encodeInto(entry, field, v); err != nil {
		t.Fatal(err)
	}
	if err := encodeInto(projects, key, entry); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateProjectsRoundTrip(t *testing.T) {
	path := writeFixture(t, roundTripFixture)
	before := decodeAny(t, []byte(roundTripFixture))

	written, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		if len(projects) != 2 {
			t.Errorf("fn saw %d projects, want 2", len(projects))
		}
		setEntryField(t, projects, "/Users/x/proj-a", "hasTrustDialogAccepted", true)
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateProjects: %v", err)
	}
	if !written {
		t.Error("want written=true")
	}

	after := readAny(t, path)
	if len(after) != len(before) {
		t.Errorf("top-level key count changed: before %d, after %d", len(before), len(after))
	}
	for k, want := range before {
		if k == "projects" {
			continue
		}
		got, ok := after[k]
		if !ok {
			t.Errorf("top-level %q lost", k)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("top-level %q changed:\n want %#v\n got  %#v", k, want, got)
		}
	}
	if _, ok := after["nullish"]; !ok {
		t.Error("null-valued top-level key dropped")
	}

	// The untouched entry is equal as a whole, including its non-ASCII key.
	if got, want := entryOf(t, after, "/Users/x/prój-b"), entryOf(t, before, "/Users/x/prój-b"); !reflect.DeepEqual(got, want) {
		t.Errorf("untouched entry changed:\n want %#v\n got  %#v", want, got)
	}

	// The touched entry differs only in the field fn set.
	gotA, wantA := entryOf(t, after, "/Users/x/proj-a"), entryOf(t, before, "/Users/x/proj-a")
	if len(gotA) != len(wantA) {
		t.Errorf("entry key count changed: before %d, after %d", len(wantA), len(gotA))
	}
	for k, want := range wantA {
		if k == "hasTrustDialogAccepted" {
			continue
		}
		if got := gotA[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("entry field %q changed:\n want %#v\n got  %#v", k, want, got)
		}
	}
	if got := gotA["hasTrustDialogAccepted"]; got != true {
		t.Errorf("hasTrustDialogAccepted = %v, want true", got)
	}
}

func TestUpdateProjectsPreservesPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := writeFixture(t, `{"projects":{}}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		projects["/p"] = json.RawMessage(`{}`)
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateProjects: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("perm: want 0644, got %o", perm)
	}
}

func TestUpdateProjectsCreatesMissingFileAndProjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, Filename)
	written, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		if projects == nil {
			t.Fatal("fn got a nil map for a missing file")
		}
		if len(projects) != 0 {
			t.Errorf("fn got %d projects for a missing file, want 0", len(projects))
		}
		projects["/p"] = json.RawMessage(`{"hasTrustDialogAccepted":true}`)
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateProjects: %v", err)
	}
	if !written {
		t.Error("want written=true")
	}
	doc := readAny(t, path)
	if got := entryOf(t, doc, "/p")["hasTrustDialogAccepted"]; got != true {
		t.Errorf("created entry: %v", doc)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("created file perm: want 0600, got %o", perm)
		}
	}

	// A file without a projects field gets one; its other keys survive.
	path = writeFixture(t, `{"theme":"dark"}`)
	if _, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		projects["/q"] = json.RawMessage(`{}`)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateProjects (no projects field): %v", err)
	}
	doc = readAny(t, path)
	if doc["theme"] != "dark" {
		t.Errorf("theme lost: %v", doc)
	}
	entryOf(t, doc, "/q")

	// A null projects field counts as absent, not as corrupt.
	path = writeFixture(t, `{"projects":null}`)
	if _, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		projects["/r"] = json.RawMessage(`{}`)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateProjects (null projects): %v", err)
	}
	entryOf(t, readAny(t, path), "/r")
}

func TestUpdateProjectsNoWriteWhenUnchangedOrFailed(t *testing.T) {
	path := writeFixture(t, roundTripFixture)
	dir := filepath.Dir(path)

	written, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		// Mutating the map without reporting a change must not reach disk.
		projects["/Users/x/ignored"] = json.RawMessage(`{}`)
		return false, nil
	})
	if err != nil {
		t.Fatalf("UpdateProjects: %v", err)
	}
	if written {
		t.Error("want written=false when fn reports no change")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != roundTripFixture {
		t.Error("file rewritten although fn reported no change")
	}

	boom := errors.New("boom")
	written, err = UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		projects["/Users/x/ignored"] = json.RawMessage(`{}`)
		return true, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("want fn's error back, got %v", err)
	}
	if written {
		t.Error("want written=false when fn fails")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != roundTripFixture {
		t.Error("file rewritten although fn failed")
	}

	// fn handing back invalid JSON must fail before anything lands on disk.
	_, err = UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		projects["/Users/x/bad"] = json.RawMessage(`{not json`)
		return true, nil
	})
	if err == nil {
		t.Error("want error for invalid raw JSON from fn")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != roundTripFixture {
		t.Error("file rewritten although encoding failed")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("stray files next to %s: %v", Filename, names)
	}
}

func TestUpdateProjectsRejectsNonObjectProjects(t *testing.T) {
	for _, body := range []string{`{"projects":"corrupt"}`, `{"projects":[1,2]}`, `{"projects":7}`} {
		path := writeFixture(t, body)
		_, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
			t.Errorf("fn called for %s", body)
			return true, nil
		})
		if err == nil {
			t.Errorf("%s: want error, got nil", body)
			continue
		}
		if !strings.Contains(err.Error(), "projects is not a JSON object") {
			t.Errorf("%s: error %q lacks the shape explanation", body, err)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != body {
			t.Errorf("%s: file modified despite error", body)
		}
	}

	path := writeFixture(t, `{"projects":{`)
	if _, err := UpdateProjects(path, func(map[string]json.RawMessage) (bool, error) { return true, nil }); err == nil {
		t.Error("want parse error for a corrupt file")
	}
}

func boolPtrString(b *bool) string {
	if b == nil {
		return "nil"
	}
	if *b {
		return "true"
	}
	return "false"
}

func TestReadProjectFlags(t *testing.T) {
	path := writeFixture(t, `{
  "userID": "abc",
  "projects": {
    "/accepted": {
      "hasTrustDialogAccepted": true,
      "hasClaudeMdExternalIncludesApproved": true,
      "hasClaudeMdExternalIncludesWarningShown": true,
      "lastSessionId": "sid-1",
      "allowedTools": ["Bash(git:*)", "Read", "Edit"],
      "enabledMcpjsonServers": ["a", "b"],
      "lastCost": 1.5
    },
    "/declined": {
      "hasTrustDialogAccepted": false,
      "hasClaudeMdExternalIncludesApproved": false,
      "hasClaudeMdExternalIncludesWarningShown": true,
      "allowedTools": "Bash(npm:*)"
    },
    "/unset": {"lastCost": 2},
    "/odd": {
      "hasTrustDialogAccepted": "yes",
      "hasClaudeMdExternalIncludesApproved": 1,
      "hasClaudeMdExternalIncludesWarningShown": null,
      "lastSessionId": 5,
      "allowedTools": "  ",
      "enabledMcpjsonServers": {"a": true}
    },
    "/legacy-empty": {"allowedTools": "", "enabledMcpjsonServers": []},
    "/not-an-object": [1, 2],
    "/null": null
  }
}`)
	flags, err := ReadProjectFlags(path)
	if err != nil {
		t.Fatalf("ReadProjectFlags: %v", err)
	}
	if len(flags) != 5 {
		t.Errorf("want 5 entries (non-objects skipped), got %d: %v", len(flags), flags)
	}
	if _, ok := flags["/not-an-object"]; ok {
		t.Error("array entry should be skipped")
	}
	if _, ok := flags["/null"]; ok {
		t.Error("null entry should be skipped")
	}

	cases := []struct {
		key                      string
		trust, approved, shown   string
		sid                      string
		allowedTools, mcpEnabled int
	}{
		{"/accepted", "true", "true", "true", "sid-1", 3, 2},
		{"/declined", "false", "false", "true", "", 1, 0},
		{"/unset", "nil", "nil", "nil", "", 0, 0},
		{"/odd", "nil", "nil", "nil", "", 0, 0},
		{"/legacy-empty", "nil", "nil", "nil", "", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			f, ok := flags[c.key]
			if !ok {
				t.Fatalf("missing entry %q", c.key)
			}
			if got := boolPtrString(f.TrustAccepted); got != c.trust {
				t.Errorf("TrustAccepted = %s, want %s", got, c.trust)
			}
			if got := boolPtrString(f.ExternalIncludesApproved); got != c.approved {
				t.Errorf("ExternalIncludesApproved = %s, want %s", got, c.approved)
			}
			if got := boolPtrString(f.ExternalIncludesWarningShown); got != c.shown {
				t.Errorf("ExternalIncludesWarningShown = %s, want %s", got, c.shown)
			}
			if f.LastSessionID != c.sid {
				t.Errorf("LastSessionID = %q, want %q", f.LastSessionID, c.sid)
			}
			if f.AllowedTools != c.allowedTools {
				t.Errorf("AllowedTools = %d, want %d", f.AllowedTools, c.allowedTools)
			}
			if f.MCPEnabled != c.mcpEnabled {
				t.Errorf("MCPEnabled = %d, want %d", f.MCPEnabled, c.mcpEnabled)
			}
		})
	}
}

func TestReadProjectFlagsLenientAndMissing(t *testing.T) {
	flags, err := ReadProjectFlags(filepath.Join(t.TempDir(), Filename))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if flags == nil || len(flags) != 0 {
		t.Errorf("missing file: want empty non-nil map, got %#v", flags)
	}

	for _, body := range []string{`{}`, `{"projects":"x"}`, `{"projects":null}`, `{"projects":[]}`} {
		flags, err := ReadProjectFlags(writeFixture(t, body))
		if err != nil {
			t.Errorf("%s: %v", body, err)
		}
		if flags == nil || len(flags) != 0 {
			t.Errorf("%s: want empty non-nil map, got %#v", body, flags)
		}
	}

	if _, err := ReadProjectFlags(writeFixture(t, `{"projects":`)); err == nil {
		t.Error("want error for a corrupt file")
	}
}

func TestSetLastSessionID(t *testing.T) {
	path := writeFixture(t, roundTripFixture)
	before := readAny(t, path)

	if err := SetLastSessionID(path, "/Users/x/proj-a", "sid-new"); err != nil {
		t.Fatalf("SetLastSessionID: %v", err)
	}
	after := readAny(t, path)
	gotA, wantA := entryOf(t, after, "/Users/x/proj-a"), entryOf(t, before, "/Users/x/proj-a")
	if gotA["lastSessionId"] != "sid-new" {
		t.Errorf("lastSessionId = %v, want sid-new", gotA["lastSessionId"])
	}
	if len(gotA) != len(wantA) {
		t.Errorf("entry key count changed: before %d, after %d", len(wantA), len(gotA))
	}
	for k, want := range wantA {
		if k == "lastSessionId" {
			continue
		}
		if got := gotA[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("entry field %q changed:\n want %#v\n got  %#v", k, want, got)
		}
	}
	for k, want := range before {
		if k == "projects" {
			continue
		}
		if got := after[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("top-level %q changed:\n want %#v\n got  %#v", k, want, got)
		}
	}
	flags, err := ReadProjectFlags(path)
	if err != nil {
		t.Fatal(err)
	}
	if flags["/Users/x/proj-a"].LastSessionID != "sid-new" {
		t.Errorf("ReadProjectFlags after set: %+v", flags["/Users/x/proj-a"])
	}

	// Same id again: no write at all.
	raw1, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetLastSessionID(path, "/Users/x/proj-a", "sid-new"); err != nil {
		t.Fatalf("SetLastSessionID (same id): %v", err)
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw1) != string(raw2) {
		t.Error("file rewritten although lastSessionId was already set")
	}
}

func TestSetLastSessionIDCreatesDefaultEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), Filename)
	if err := SetLastSessionID(path, "/new/project", "sid-x"); err != nil {
		t.Fatalf("SetLastSessionID: %v", err)
	}
	entry := entryOf(t, readAny(t, path), "/new/project")
	want := map[string]any{
		"allowedTools":                            []any{},
		"mcpContextUris":                          []any{},
		"mcpServers":                              map[string]any{},
		"enabledMcpjsonServers":                   []any{},
		"disabledMcpjsonServers":                  []any{},
		"hasTrustDialogAccepted":                  false,
		"hasClaudeMdExternalIncludesApproved":     false,
		"hasClaudeMdExternalIncludesWarningShown": false,
		"lastSessionId":                           "sid-x",
	}
	if !reflect.DeepEqual(entry, want) {
		t.Errorf("created entry:\n want %#v\n got  %#v", want, entry)
	}

	flags, err := ReadProjectFlags(path)
	if err != nil {
		t.Fatal(err)
	}
	f := flags["/new/project"]
	if f.TrustAccepted == nil || *f.TrustAccepted || f.LastSessionID != "sid-x" || f.AllowedTools != 0 || f.MCPEnabled != 0 {
		t.Errorf("flags of created entry: %+v", f)
	}
}

func TestSetLastSessionIDRefusals(t *testing.T) {
	path := writeFixture(t, `{"projects":{"/bad":"string"}}`)
	if err := SetLastSessionID(path, "/bad", "sid"); err == nil || !strings.Contains(err.Error(), `projects["/bad"] is not a JSON object`) {
		t.Errorf("non-object entry: want shape error, got %v", err)
	}
	if err := SetLastSessionID(path, "", "sid"); err == nil {
		t.Error("empty project key: want error")
	}
	if err := SetLastSessionID(path, "/p", ""); err == nil {
		t.Error("empty session id: want error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"projects":{"/bad":"string"}}` {
		t.Error("file modified despite errors")
	}
}

func TestDefaultProjectEntryIsFresh(t *testing.T) {
	a := DefaultProjectEntry()
	a["allowedTools"] = json.RawMessage(`["x"]`)
	delete(a, "mcpServers")
	b := DefaultProjectEntry()
	if len(b) != 8 {
		t.Errorf("want 8 keys, got %d: %v", len(b), b)
	}
	if string(b["allowedTools"]) != `[]` {
		t.Errorf("shared state leaked between calls: %s", b["allowedTools"])
	}
	if _, ok := b["mcpServers"]; !ok {
		t.Error("shared state leaked between calls: mcpServers missing")
	}
	for k, v := range b {
		if !json.Valid(v) {
			t.Errorf("%s: invalid JSON %q", k, v)
		}
	}
}
