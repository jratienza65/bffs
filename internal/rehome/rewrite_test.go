package rehome

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

func TestRewriteMemoryPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	notes := "---\nname: notes\n---\n" +
		"see /Users/dev/build/projects/bffs/x.md and `/Users/dev/build/projects/bffs`\n" +
		"not /Users/devx/y nor /Users/dev/build/projects/bffsx\n" +
		"quoted \"/Users/dev/notes\" and (/Users/dev/build/projects/bffs) end /Users/dev\n" +
		"win C:\\Users\\dev\\x\\y\n" +
		"other /opt/tool/bin\n"
	write(t, filepath.Join(dir, "MEMORY.md"), "- [notes](notes.md) - /Users/dev/build/projects/bffs\n")
	write(t, filepath.Join(dir, "notes.md"), notes)
	write(t, filepath.Join(dir, "logs", "2026", "09", "05", "abcd1234-title.md"), "log /Users/dev/build/projects/bffs/z\n")
	write(t, filepath.Join(dir, "proposals", "p.md"), "/Users/dev/build/projects/bffs never\n")
	write(t, filepath.Join(dir, "untouched.md"), "nothing here\n")
	old := mtimeOf(t, filepath.Join(dir, "untouched.md"))
	// A rewritten topic file keeps its mtime: Claude picks the newest
	// pinned files by it.
	notesMtime := old.Add(-30 * 24 * time.Hour)
	chtimes(t, filepath.Join(dir, "notes.md"), notesMtime)

	pairs := [][2]string{
		{"/Users/dev", "/home/dev"},
		{"/Users/dev/build/projects/bffs", "/home/dev/src/bffs"}, // longer, listed second
		{`C:\Users\dev`, `D:\work`},
		{"", "/x"}, {"/same", "/same"},
	}
	changed, remaining, err := RewriteMemoryPaths(dir, pairs)
	if err != nil {
		t.Fatal(err)
	}
	wantChanged := []string{"MEMORY.md", "logs/2026/09/05/abcd1234-title.md", "notes.md"}
	if strings.Join(changed, ",") != strings.Join(wantChanged, ",") {
		t.Errorf("changed = %v, want %v", changed, wantChanged)
	}
	got := readFile(t, filepath.Join(dir, "notes.md"))
	want := "---\nname: notes\n---\n" +
		"see /home/dev/src/bffs/x.md and `/home/dev/src/bffs`\n" +
		"not /Users/devx/y nor /home/dev/build/projects/bffsx\n" +
		"quoted \"/home/dev/notes\" and (/home/dev/src/bffs) end /home/dev\n" +
		"win D:\\work\\x\\y\n" +
		"other /opt/tool/bin\n"
	if got != want {
		t.Errorf("notes.md =\n%s\nwant\n%s", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "MEMORY.md")); got != "- [notes](notes.md) - /home/dev/src/bffs\n" {
		t.Errorf("MEMORY.md = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "logs", "2026", "09", "05", "abcd1234-title.md")); got != "log /home/dev/src/bffs/z\n" {
		t.Errorf("log = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "proposals", "p.md")); !strings.HasPrefix(got, "/Users/dev") {
		t.Errorf("proposals rewritten: %q", got)
	}
	if mt := mtimeOf(t, filepath.Join(dir, "untouched.md")); !mt.Equal(old) {
		t.Errorf("untouched file rewritten (mtime %v → %v)", old, mt)
	}
	if mt := mtimeOf(t, filepath.Join(dir, "notes.md")); !near(mt, notesMtime) {
		t.Errorf("rewritten file got a fresh mtime (%v, want %v)", mt, notesMtime)
	}
	// What is left mentions the source machine still: the near-miss and
	// the unrelated /opt path, plus the rewritten /home/ paths (they are
	// absolute paths too — the user reviews them).
	var paths []string
	for _, r := range remaining {
		if r.File == "notes.md" {
			paths = append(paths, r.Path)
		}
	}
	for _, want := range []string{"/Users/devx/y", "/home/dev/build/projects/bffsx", "/opt/tool/bin"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("remaining lacks %q: %v", want, paths)
		}
	}
	if _, _, err := RewriteMemoryPaths(filepath.Join(t.TempDir(), "missing"), pairs); err == nil {
		t.Error("missing dir accepted")
	}
	// No rules: nothing changes, the scan still runs.
	changed, remaining, err = RewriteMemoryPaths(dir, nil)
	if err != nil || len(changed) != 0 || len(remaining) == 0 {
		t.Errorf("no-rule call = %v, %d remaining, %v", changed, len(remaining), err)
	}
	_ = transcripts.PathKindAbs
}

func TestRewritePrefixesBoundary(t *testing.T) {
	rules := rewriteRules([][2]string{{"/a/b", "/x"}})
	cases := map[string]string{
		"/a/b":       "/x",
		"/a/b/c":     "/x/c",
		"/a/b\\c":    "/x\\c",
		"/a/b c":     "/x c",
		"/a/b\tc":    "/x\tc",
		"/a/b)":      "/x)",
		"'/a/b'":     "'/x'",
		"/a/bc":      "/a/bc",
		"/a/b.md":    "/a/b.md",
		"/a/b,":      "/a/b,",
		"@/a/b/c.md": "@/x/c.md",
		"/a/b\n/a/b": "/x\n/x",
	}
	for in, want := range cases {
		got, _ := rewritePrefixes([]byte(in), rules)
		if string(got) != want {
			t.Errorf("rewrite(%q) = %q, want %q", in, got, want)
		}
	}
}
