package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAcceptsSample(t *testing.T) {
	m, _ := sampleManifest()
	if err := m.Validate(DefaultLimits); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := m.Validate(unlimited); err != nil {
		t.Fatalf("Validate(unlimited): %v", err)
	}
	empty := &Manifest{Format: 1, BundleID: bundleID, Source: Source{Hostname: "h"}}
	if err := empty.Validate(DefaultLimits); err != nil {
		t.Fatalf("empty manifest: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	sc := "projects/" + slugA + "/" + sidA
	cases := []struct {
		name string
		mut  func(m *Manifest)
		l    Limits
		want string
	}{
		{"hostname traversal", func(m *Manifest) { m.Source.Hostname = "../x" }, DefaultLimits, "source.hostname"},
		{"hostname empty", func(m *Manifest) { m.Source.Hostname = "" }, DefaultLimits, "source.hostname"},
		{"hostname long", func(m *Manifest) { m.Source.Hostname = strings.Repeat("h", 65) }, DefaultLimits, "source.hostname"},
		{"user space", func(m *Manifest) { m.Source.User = "bad user" }, DefaultLimits, "source.user"},
		{"account slash", func(m *Manifest) { m.Source.Account = "a/b" }, DefaultLimits, "source.account"},
		{"bundle id upper", func(m *Manifest) { m.BundleID = strings.ToUpper(bundleID) }, DefaultLimits, "bundle_id"},
		{"bundle id short", func(m *Manifest) { m.BundleID = "abc" }, DefaultLimits, "bundle_id"},
		{"format 2", func(m *Manifest) { m.Format = 2 }, DefaultLimits, "upgrade bffs"},
		{"format 0", func(m *Manifest) { m.Format = 0 }, DefaultLimits, "format 0"},
		{"totals entries", func(m *Manifest) { m.Totals.Entries++ }, DefaultLimits, "totals.entries"},
		{"totals files", func(m *Manifest) { m.Totals.Files-- }, DefaultLimits, "totals.files"},
		{"totals bytes", func(m *Manifest) { m.Totals.Bytes++ }, DefaultLimits, "totals.bytes"},
		{"max entries", nil, Limits{MaxTotalBytes: 1 << 30, MaxEntryBytes: 1 << 30, MaxManifestBytes: 1 << 20, MaxEntries: 1, MaxFiles: 100}, "max 1"},
		{"max files", nil, Limits{MaxTotalBytes: 1 << 30, MaxEntryBytes: 1 << 30, MaxManifestBytes: 1 << 20, MaxEntries: 10, MaxFiles: 3}, "more than 3 files"},
		{"max total", nil, Limits{MaxTotalBytes: 10, MaxEntryBytes: 1 << 30, MaxManifestBytes: 1 << 20, MaxEntries: 10, MaxFiles: 100}, "max 10"},
		{"max entry", nil, Limits{MaxTotalBytes: 1 << 30, MaxEntryBytes: 5, MaxManifestBytes: 1 << 20, MaxEntries: 10, MaxFiles: 100}, "max 5"},
		{"zero limits", nil, Limits{}, "max 0"},
		{"duplicate exact", func(m *Manifest) { m.Entries[0].Files = append(m.Entries[0].Files, m.Entries[0].Files[2]); finalize(m) }, DefaultLimits, "listed twice"},
		{"duplicate case", func(m *Manifest) {
			f := m.Entries[0].Files[1]
			f.Path = sc + "/subagents/AGENT-A1B2C3D.jsonl"
			m.Entries[0].Files = append(m.Entries[0].Files, f)
			finalize(m)
		}, DefaultLimits, "case-insensitive"},
		{"file is a dir", func(m *Manifest) {
			m.Entries[1].Files = append(m.Entries[1].Files, fileFor("memory/"+slugA+"/topic.md/x.md", []byte("x"), testNow))
			finalize(m)
		}, DefaultLimits, "which is a file"},
		{"file is a dir case", func(m *Manifest) {
			m.Entries[1].Files = append(m.Entries[1].Files, fileFor("memory/"+slugA+"/Topic.md/x.md", []byte("x"), testNow))
			finalize(m)
		}, DefaultLimits, "which is a file"},
		{"dir is a file", func(m *Manifest) {
			m.Entries[1].Files = append([]File{fileFor("memory/"+slugA+"/Logs.md/x.md", []byte("x"), testNow)}, fileFor("memory/"+slugA+"/logs.md", []byte("y"), testNow), m.Entries[1].Files[0])
			finalize(m)
		}, DefaultLimits, "is also a directory"},
		{"size overflow", func(m *Manifest) {
			m.Entries[0].Files[0].Size = 1<<62 + 1<<61
			m.Entries[0].Files[1].Size = 1 << 62
			m.Entries[0].Files[2].Size = 1 << 62
			finalize(m)
		}, unlimited, "overflow"},
		{"sha upper", func(m *Manifest) { m.Entries[0].Files[0].SHA256 = strings.ToUpper(m.Entries[0].Files[0].SHA256) }, DefaultLimits, "sha256"},
		{"sha short", func(m *Manifest) { m.Entries[0].Files[0].SHA256 = "abc" }, DefaultLimits, "sha256"},
		{"negative size", func(m *Manifest) { m.Entries[0].Files[0].Size = -1; finalize(m) }, DefaultLimits, "negative"},
		{"no transcript", func(m *Manifest) { m.Entries[0].Files = m.Entries[0].Files[1:]; finalize(m) }, DefaultLimits, "no transcript"},
		{"other session sidecar", func(m *Manifest) {
			m.Entries[0].Files[2].Path = "projects/" + slugA + "/" + sidB + "/custom-title.json"
		}, DefaultLimits, "belongs to session"},
		{"other slug transcript", func(m *Manifest) { m.Entries[0].Files[0].Path = "projects/other/" + sidA + ".jsonl" }, DefaultLimits, "belongs to slug"},
		{"other session history", func(m *Manifest) { m.Entries[0].Files[4].Path = "history/" + sidB + ".jsonl" }, DefaultLimits, "belongs to session"},
		{"memory in session", func(m *Manifest) { m.Entries[0].Files[5].Path = "memory/" + slugA + "/extra.md" }, DefaultLimits, "cannot be part of a session"},
		{"transcript in memory", func(m *Manifest) { m.Entries[1].Files[1].Path = "projects/" + slugA + "/" + sidB + ".jsonl" }, DefaultLimits, "cannot be part of a memory"},
		{"memory other slug", func(m *Manifest) { m.Entries[1].Files[1].Path = "memory/other/topic.md" }, DefaultLimits, "belongs to slug"},
		{"plan without plan_slug", func(m *Manifest) { m.Entries[0].PlanSlug = "" }, DefaultLimits, "no plan_slug"},
		{"plan wrong plan_slug", func(m *Manifest) { m.Entries[0].PlanSlug = "other-plan" }, DefaultLimits, "plan_slug"},
		{"plan_slug bad", func(m *Manifest) { m.Entries[0].PlanSlug = "Bad Slug" }, DefaultLimits, "plan_slug"},
		{"reserved slug", func(m *Manifest) { m.Entries[1].Slug = "memory" }, DefaultLimits, "reserved"},
		{"bad slug", func(m *Manifest) { m.Entries[1].Slug = "a.b" }, DefaultLimits, "slug"},
		{"bad kind", func(m *Manifest) { m.Entries[1].Kind = "foo" }, DefaultLimits, "kind"},
		{"bad session id", func(m *Manifest) { m.Entries[0].SessionID = "nope" }, DefaultLimits, "session_id"},
		{"no files", func(m *Manifest) { m.Entries[1].Files = nil; finalize(m) }, DefaultLimits, "no files"},
		{"manifest as file", func(m *Manifest) { m.Entries[0].Files[5].Path = "manifest.json" }, DefaultLimits, "entry 0"},
		{"bad name", func(m *Manifest) { m.Entries[0].Files[5].Path = "plans/../x.md" }, DefaultLimits, "not clean"},
		{"reserved other slug", func(m *Manifest) { m.Entries[0].Files[5].Path = "claudejson/other.json" }, DefaultLimits, "belongs to slug"},
		{"reserved other sid", func(m *Manifest) { m.Entries[0].Files[5].Path = "session-env/" + sidB + "/x" }, DefaultLimits, "belongs to session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := sampleManifest()
			if c.mut != nil {
				c.mut(m)
			}
			err := m.Validate(c.l)
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %q, want it to contain %q", err, c.want)
			}
		})
	}
	var nilM *Manifest
	if err := nilM.Validate(DefaultLimits); err == nil {
		t.Fatal("nil manifest accepted")
	}
}

func TestValidateAcceptsPlanShapesAndReserved(t *testing.T) {
	m, _ := sampleManifest()
	e := &m.Entries[0]
	extra := []string{
		"plans/quirky-lemur-agent-a1b2c3d.md",
		"plans/quirky-lemur.workshop.md",
		"claudejson/" + slugA + ".json",
		"session-env/" + sidA + "/PATH",
		"user-memory/CLAUDE.md",
		"tasks/" + sidA + "/todo.json",
	}
	for _, p := range extra {
		e.Files = append(e.Files, fileFor(p, []byte("x"), testNow))
	}
	m.Entries[1].Files = append(m.Entries[1].Files, fileFor("user-memory/notes.md", []byte("y"), testNow))
	finalize(m)
	if err := m.Validate(DefaultLimits); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestManifestJSONShape(t *testing.T) {
	m, _ := sampleManifest()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, key := range []string{
		`"format":1`, `"bundle_id"`, `"bffs_version"`, `"claude_version"`, `"created"`,
		`"source":{"hostname"`, `"user"`, `"home"`, `"os"`, `"arch"`, `"config_dir"`, `"root_dir"`, `"account"`, `"account_type"`, `"isolation"`,
		`"totals":{"entries":2,"files":8,"bytes":`,
		`"kind":"session"`, `"slug"`, `"cwd"`, `"project_key"`, `"git_remote"`, `"session_id"`, `"title"`, `"git_branch"`, `"plan_slug"`, `"started"`, `"last"`, `"parts"`,
		`"source_trust":{"accepted":true,"external_includes_approved":false,"external_includes_warning_shown":false}`,
		`"files":[{"path"`, `"size"`, `"sha256"`, `"mtime"`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("manifest json lacks %s", key)
		}
	}
	if strings.Contains(s, "live_possibly_truncated") {
		t.Error("live_possibly_truncated should be omitted when false")
	}
	var round Manifest
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	// The memory entry has no session fields: its object must not carry them.
	memRaw, _ := json.Marshal(round.Entries[1])
	for _, key := range []string{"session_id", "started", "last", "parts", "source_trust", "title"} {
		if strings.Contains(string(memRaw), key) {
			t.Errorf("memory entry json carries %s: %s", key, memRaw)
		}
	}
	if round.Entries[0].SourceTrust == nil || !round.Entries[0].SourceTrust.Accepted {
		t.Error("source_trust lost in round trip")
	}
	if !round.Entries[0].Started.Equal(m.Entries[0].Started) {
		t.Error("started lost in round trip")
	}
	if err := round.Validate(DefaultLimits); err != nil {
		t.Fatalf("round-tripped manifest: %v", err)
	}
}

func TestManifestIgnoresUnknownFields(t *testing.T) {
	m, _ := sampleManifest()
	raw, _ := json.Marshal(m)
	withExtra := strings.Replace(string(raw), `"format":1,`, `"format":1,"future":{"x":[1,2]},`, 1)
	withExtra = strings.Replace(withExtra, `"kind":"session",`, `"kind":"session","novel":true,`, 1)
	var round Manifest
	if err := json.Unmarshal([]byte(withExtra), &round); err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if err := round.Validate(DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"format":"1"}`), &round); err == nil {
		t.Fatal("format as a string should not decode")
	}
}
