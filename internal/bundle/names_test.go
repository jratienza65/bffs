package bundle

import (
	"maps"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/transcripts"
)

func TestReservedSlugsEqual(t *testing.T) {
	if !maps.Equal(reservedSlugs, transcripts.ReservedProjectEntries) {
		t.Fatalf("bundle.reservedSlugs = %v, transcripts.ReservedProjectEntries = %v; keep the private copy equal", reservedSlugs, transcripts.ReservedProjectEntries)
	}
	for k := range reservedSlugs {
		if k != strings.ToLower(k) {
			t.Errorf("reservedSlugs key %q is not lower-case", k)
		}
	}
}

func TestValidateEntryName(t *testing.T) {
	long := strings.Repeat("a", 300)
	comp255 := strings.Repeat("b", 255)
	total1024 := strings.Repeat("c/", 511) + "dd" // 1024 bytes
	total1025 := strings.Repeat("c/", 511) + "ddd"
	reject := []string{
		"",
		"../x",
		"/abs",
		"/",
		"a/../../b",
		"a/../b",
		"a/..",
		"..",
		".",
		"./x",
		"x/./y",
		"x/.",
		"x//y",
		"x/",
		"C:\\x",
		"C:/x",
		"a\\b",
		"NUL",
		"nul",
		"NUL.txt",
		"a/NUL/b",
		"CON",
		"prn.log",
		"AUX",
		"COM1",
		"lpt9.log",
		"a/" + long,
		total1025,
		"a\x00b",
		"a b",
		"a\tb",
		"a\nb",
		"a;b",
		"a:b",
		"a*b",
		"a?b",
		"a~b",
		"a#b",
		"a%b",
		"a+b",
		"a=b",
		"ünïcode",
		"a/Ⅼ",         // U+216C ROMAN NUMERAL FIFTY, looks like L
		"ａ",           // U+FF41 FULLWIDTH LATIN SMALL LETTER A
		"pro\u0435ct", // Cyrillic е
		"a\u200bb",    // zero-width space
		"...",         // every component ending in a dot: windows strips trailing dots
		"a.",
		"x/y.",
		"x./y",
		"memory/s/topic.md.",
	}
	for _, name := range reject {
		if err := ValidateEntryName(name); err == nil {
			t.Errorf("ValidateEntryName(%q) = nil, want error", name)
		}
	}
	accept := []string{
		"manifest.json",
		"a",
		"a/b",
		"a-b_c.d@e",
		"projects/x/y",
		"x/.hidden",
		".ccr-tip.json",
		"a/" + comp255,
		total1024,
		"COM0",
		"CONS",
		"NULL",
		"LPT",
		"aux2",
		"com1x/y", // 5 chars before the dot: not a device name
		"a...b",
		".a",
	}
	for _, name := range accept {
		if err := ValidateEntryName(name); err != nil {
			t.Errorf("ValidateEntryName(%q) = %v, want nil", name, err)
		}
	}
}

func TestClassifyName(t *testing.T) {
	sidUpper := strings.ToUpper(sidA)
	sc := "projects/" + slugA + "/" + sidA
	cases := []struct {
		name string
		kind string
		sid  string
		slug string
		err  bool
	}{
		{"manifest.json", NameManifest, "", "", false},
		{"manifest.json/x", "", "", "", true},

		{"projects/" + slugA + "/" + sidA + ".jsonl", NameTranscript, sidA, slugA, false},
		{"projects/my_proj/" + sidA + ".jsonl", NameTranscript, sidA, "my_proj", false},
		{"projects/memory/" + sidA + ".jsonl", "", "", "", true},
		{"projects/Memory/" + sidA + ".jsonl", "", "", "", true},
		{"projects/tiny_memory/" + sidA + ".jsonl", "", "", "", true},
		{"projects/bagel/" + sidA + ".jsonl", "", "", "", true},
		{"projects/cloud-snapshots/" + sidA + ".jsonl", "", "", "", true},
		{"projects/" + strings.Repeat("s", 240) + "/" + sidA + ".jsonl", NameTranscript, sidA, strings.Repeat("s", 240), false},
		{"projects/" + strings.Repeat("s", 241) + "/" + sidA + ".jsonl", "", "", "", true},
		{"projects/a.b/" + sidA + ".jsonl", "", "", "", true},
		{"projects/" + slugA + "/" + sidUpper + ".jsonl", "", "", "", true},
		{"projects/" + slugA + "/" + sidA + ".json", "", "", "", true},
		{"projects/" + slugA + "/" + sidA[:35] + ".jsonl", "", "", "", true},
		{"projects/" + slugA + "/" + sidA, "", "", "", true},
		{"projects/" + slugA, "", "", "", true},
		{"projects", "", "", "", true},
		{"projects/" + slugA + "/memory/MEMORY.md", "", "", "", true},

		{sc + "/subagents/agent-abc.jsonl", NameSidecar, sidA, slugA, false},
		{sc + "/subagents/deep/er/file", NameSidecar, sidA, slugA, false},
		{sc + "/workflows/w.json", NameSidecar, sidA, slugA, false},
		{sc + "/tool-results/x.txt", NameSidecar, sidA, slugA, false},
		{sc + "/remote-agents/r.json", NameSidecar, sidA, slugA, false},
		{sc + "/mcp-tasks/t.json", NameSidecar, sidA, slugA, false},
		{sc + "/other/x", "", "", "", true},
		{sc + "/subagents", "", "", "", true},
		{sc + "/rec.cast", NameSidecar, sidA, slugA, false},
		{sc + "/.cast", "", "", "", true},
		{sc + "/custom-title.json", NameSidecar, sidA, slugA, false},
		{sc + "/.ccr-tip.json", NameSidecar, sidA, slugA, false},
		{sc + "/.precompact.json", NameSidecar, sidA, slugA, false},
		{sc + "/sent-prefix.json", NameSidecar, sidA, slugA, false},
		{sc + "/random.json", NameSidecar + "!", "", "", true},
		{sc + "/subagents/../x", "", "", "", true},

		{"file-history/" + sidA + "/deadbeefdeadbeef@v2", NameFileHistory, sidA, "", false},
		{"file-history/" + sidA + "/deadbeefdeadbeef@v10", NameFileHistory, sidA, "", false},
		{"file-history/" + sidA + "/deadbeefdeadbeef@v", "", "", "", true},
		{"file-history/" + sidA + "/DEADBEEFDEADBEEF@v2", "", "", "", true},
		{"file-history/" + sidA + "/deadbeefdeadbee@v2", "", "", "", true},
		{"file-history/" + sidA + "/x/deadbeefdeadbeef@v2", "", "", "", true},
		{"file-history/" + sidA, "", "", "", true},
		{"file-history/nope/deadbeefdeadbeef@v2", "", "", "", true},

		{"plans/quirky-lemur.md", NamePlans, "", "", false},
		{"plans/quirky-lemur-agent-a1b2c3d.md", NamePlans, "", "", false},
		{"plans/quirky-lemur-agent-A1B2_c3d.md", NamePlans, "", "", false},
		{"plans/quirky-lemur.workshop.md", NamePlans, "", "", false},
		{"plans/Quirky.md", "", "", "", true},
		{"plans/x.txt", "", "", "", true},
		{"plans/a/b.md", "", "", "", true},
		{"plans/" + strings.Repeat("p", 80) + ".md", NamePlans, "", "", false},
		{"plans/" + strings.Repeat("p", 81) + ".md", "", "", "", true},
		{"plans/.md", "", "", "", true},

		{"history/" + sidA + ".jsonl", NameHistory, sidA, "", false},
		{"history/x.jsonl", "", "", "", true},
		{"history/" + sidA, "", "", "", true},

		{"tasks/" + sidA + "/todo.json", NameTasks, sidA, "", false},
		{"tasks/" + sidA + "/a/b/c", NameTasks, sidA, "", false},
		{"tasks/" + sidA + "/.lock", "", "", "", true},
		{"tasks/" + sidA + "/sub/.lock", "", "", "", true},
		{"tasks/" + sidA + "/.lock/x", "", "", "", true},
		{"tasks/" + sidA + "/a.lock", "", "", "", true},
		{"tasks/" + sidA, "", "", "", true},
		{"tasks/x/y", "", "", "", true},

		{"memory/" + slugA + "/MEMORY.md", NameMemory, "", slugA, false},
		{"memory/" + slugA + "/logs/2026-09.md", NameMemory, "", slugA, false},
		{"memory/" + slugA + "/proposals.md", NameMemory, "", slugA, false},
		{"memory/" + slugA + "/proposals/x.md", "", "", "", true},
		{"memory/" + slugA + "/Proposals/x.md", "", "", "", true},
		{"memory/" + slugA + "/a/proposals/x.md", "", "", "", true},
		{"memory/" + slugA + "/index.json", "", "", "", true},
		{"memory/" + slugA + "/index-cache.md", "", "", "", true},
		{"memory/" + slugA + "/Index.md", "", "", "", true},
		{"memory/" + slugA + "/notes.txt", "", "", "", true},
		{"memory/memory/x.md", "", "", "", true},
		{"memory/" + slugA + "/.md", "", "", "", true},
		{"memory/" + slugA, "", "", "", true},

		{"claudejson/" + slugA + ".json", NameReserved, "", slugA, false},
		{"claudejson/memory.json", "", "", "", true},
		{"claudejson/x.txt", "", "", "", true},
		{"session-env/" + sidA + "/x", NameReserved, sidA, "", false},
		{"session-env/" + sidA + "/a/b", NameReserved, sidA, "", false},
		{"session-env/" + sidA, "", "", "", true},
		{"session-env/x/y", "", "", "", true},
		{"user-memory/CLAUDE.md", NameReserved, "", "", false},
		{"user-memory/a/b", NameReserved, "", "", false},
		{"user-memory", "", "", "", true},

		{"other/x", "", "", "", true},
		{"../x", "", "", "", true},
		{"", "", "", "", true},
		{"launches.jsonl", "", "", "", true},
		{".credentials.json", "", "", "", true},
	}
	for _, c := range cases {
		kind, sid, slug, err := ClassifyName(c.name)
		if c.err {
			if err == nil {
				t.Errorf("ClassifyName(%q) = (%q, %q, %q, nil), want error", c.name, kind, sid, slug)
			}
			continue
		}
		if err != nil {
			t.Errorf("ClassifyName(%q): %v", c.name, err)
			continue
		}
		if kind != c.kind || sid != c.sid || slug != c.slug {
			t.Errorf("ClassifyName(%q) = (%q, %q, %q), want (%q, %q, %q)", c.name, kind, sid, slug, c.kind, c.sid, c.slug)
		}
	}
}

func TestClassifyImpliesValidate(t *testing.T) {
	for _, name := range []string{"a\\b", "../x", "/abs", "projects/x/" + sidA + ".jsonl/", "memory/x/NUL.md"} {
		if _, _, _, err := ClassifyName(name); err == nil {
			t.Errorf("ClassifyName(%q) accepted a name ValidateEntryName rejects", name)
		}
	}
}
