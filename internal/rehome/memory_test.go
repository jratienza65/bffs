package rehome

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	notesSrc = "---\nname: notes\npinned: true\n---\n# Notes\nsee /Users/jonas/build/projects/bffs/x.md\n"
	logSrc   = "---\npinned: \"true\"\n---\nlog line\n"
	indexSrc = "- [notes](notes.md) - notes\n"
)

// stageMemory writes a source memory dir: MEMORY.md, notes.md, a log file,
// a proposals/ scratch file (never copied) and a non-markdown file.
func stageMemory(t *testing.T) (dir string, old time.Time) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "memory")
	write(t, filepath.Join(dir, "MEMORY.md"), indexSrc)
	write(t, filepath.Join(dir, "notes.md"), notesSrc)
	write(t, filepath.Join(dir, "logs", "2026", "09", "05", "abcd1234-title.md"), logSrc)
	write(t, filepath.Join(dir, "proposals", "scratch.md"), "scratch\n")
	write(t, filepath.Join(dir, "cache.json"), "{}")
	old = fixedNow.Add(-60 * 24 * time.Hour)
	for _, p := range []string{"MEMORY.md", "notes.md", filepath.Join("logs", "2026", "09", "05", "abcd1234-title.md")} {
		chtimes(t, filepath.Join(dir, p), old)
	}
	return dir, old
}

func memDst(t *testing.T) string {
	return filepath.Join(t.TempDir(), "projects", "-home-jonas-src-bffs", "memory")
}

func TestMergeMemorySkipAbsentConfirmed(t *testing.T) {
	src, old := stageMemory(t)
	dst := memDst(t)
	mv, err := MergeMemory(dst, src, MemorySkip, testID8, true, false, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if mv.From != src || mv.To != dst {
		t.Errorf("From/To = %q/%q", mv.From, mv.To)
	}
	wantAdded := []string{"logs/2026/09/05/abcd1234-title.md", "notes.md", "MEMORY.md"}
	if strings.Join(mv.Added, ",") != strings.Join(wantAdded, ",") {
		t.Errorf("Added = %v, want %v", mv.Added, wantAdded)
	}
	if len(mv.Renamed) != 0 || len(mv.Unchanged) != 0 || mv.IndexAppended {
		t.Errorf("Renamed=%v Unchanged=%v IndexAppended=%v", mv.Renamed, mv.Unchanged, mv.IndexAppended)
	}
	notes := readFile(t, filepath.Join(dst, "notes.md"))
	if !strings.Contains(notes, "\npinned-imported: true\n") || strings.Contains(notes, "\npinned: true\n") {
		t.Errorf("pinned not neutralised:\n%s", notes)
	}
	if !strings.HasSuffix(notes, "# Notes\nsee /Users/jonas/build/projects/bffs/x.md\n") {
		t.Errorf("body changed:\n%s", notes)
	}
	log := readFile(t, filepath.Join(dst, "logs", "2026", "09", "05", "abcd1234-title.md"))
	if !strings.HasPrefix(log, "---\npinned-imported: \"true\"\n---\n") {
		t.Errorf("log pinned not neutralised:\n%s", log)
	}
	if got := readFile(t, filepath.Join(dst, "MEMORY.md")); got != indexSrc {
		t.Errorf("MEMORY.md = %q", got)
	}
	if mt := mtimeOf(t, filepath.Join(dst, "notes.md")); !near(mt, old) {
		t.Errorf("notes.md mtime = %v, want source %v", mt, old)
	}
	if mt := mtimeOf(t, filepath.Join(dst, "logs", "2026", "09", "05", "abcd1234-title.md")); !near(mt, old) {
		t.Errorf("log mtime = %v, want source %v", mt, old)
	}
	if mt := mtimeOf(t, filepath.Join(dst, "MEMORY.md")); !near(mt, fixedNow) {
		t.Errorf("MEMORY.md mtime = %v, want now", mt)
	}
	if exists(filepath.Join(dst, "proposals")) || exists(filepath.Join(dst, "cache.json")) {
		t.Errorf("non-memory entries copied")
	}
	if exists(dst + tmpSuffix) {
		t.Errorf("tmp dir left behind")
	}
	var found bool
	for _, r := range mv.Remaining {
		if r.File == "notes.md" && r.Path == "/Users/jonas/build/projects/bffs/x.md" && r.Kind == transcripts.PathKindAbs {
			found = true
		}
	}
	if !found {
		t.Errorf("Remaining = %+v, want the /Users/ path in notes.md", mv.Remaining)
	}
}

func TestMergeMemorySkipPresentIsNoop(t *testing.T) {
	src, _ := stageMemory(t)
	dst := memDst(t)
	write(t, filepath.Join(dst, "MEMORY.md"), "mine\n")
	write(t, filepath.Join(dst, "notes.md"), "mine too\n")
	mv, err := MergeMemory(dst, src, MemorySkip, testID8, true, false, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"logs/2026/09/05/abcd1234-title.md", "notes.md", "MEMORY.md"}
	if strings.Join(mv.Unchanged, ",") != strings.Join(want, ",") {
		t.Errorf("Unchanged = %v, want %v", mv.Unchanged, want)
	}
	if len(mv.Added) != 0 || len(mv.Renamed) != 0 || mv.Remaining != nil {
		t.Errorf("wrote something: %+v", mv)
	}
	if readFile(t, filepath.Join(dst, "notes.md")) != "mine too\n" || readFile(t, filepath.Join(dst, "MEMORY.md")) != "mine\n" {
		t.Errorf("existing memory touched")
	}
	if got := filesUnder(t, dst); len(got) != 2 {
		t.Errorf("files = %v", got)
	}
}

func TestMergeMemoryOverwriteSetsAside(t *testing.T) {
	src, _ := stageMemory(t)
	dst := memDst(t)
	write(t, filepath.Join(dst, "MEMORY.md"), "mine\n")
	write(t, filepath.Join(dst, "secret.md"), "keep me\n")
	mv, err := MergeMemory(dst, src, MemoryOverwrite, testID8, true, false, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	aside := SetAsideName(dst, fixedNow)
	if readFile(t, filepath.Join(aside, "secret.md")) != "keep me\n" || readFile(t, filepath.Join(aside, "MEMORY.md")) != "mine\n" {
		t.Errorf("existing memory not set aside intact")
	}
	if !transcripts.IsSetAside(filepath.Base(aside)) {
		t.Errorf("set-aside name %q is not recognised", filepath.Base(aside))
	}
	if readFile(t, filepath.Join(dst, "MEMORY.md")) != indexSrc {
		t.Errorf("MEMORY.md not replaced")
	}
	if exists(filepath.Join(dst, "secret.md")) {
		t.Errorf("old topic file leaked into the new dir")
	}
	if len(mv.Added) != 3 {
		t.Errorf("Added = %v", mv.Added)
	}
}

func TestMergeMemoryUnconfirmed(t *testing.T) {
	src, old := stageMemory(t)
	for _, mode := range []MemoryMode{MemorySkip, MemoryOverwrite} {
		t.Run(string(mode), func(t *testing.T) {
			dst := memDst(t)
			mv, err := MergeMemory(dst, src, mode, testID8, false, false, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			wantRenamed := []string{"logs/2026/09/05/abcd1234-title.imported-" + testID8 + ".md", "notes.imported-" + testID8 + ".md"}
			if strings.Join(mv.Renamed, ",") != strings.Join(wantRenamed, ",") {
				t.Errorf("Renamed = %v, want %v", mv.Renamed, wantRenamed)
			}
			if len(mv.Added) != 0 {
				t.Errorf("Added = %v", mv.Added)
			}
			if strings.Join(mv.Unchanged, ",") != "MEMORY.md" {
				t.Errorf("Unchanged = %v", mv.Unchanged)
			}
			if exists(filepath.Join(dst, "MEMORY.md")) {
				t.Errorf("MEMORY.md created for an unconfirmed placement")
			}
			p := filepath.Join(dst, "notes.imported-"+testID8+".md")
			if got := readFile(t, p); !strings.Contains(got, "pinned-imported: true") {
				t.Errorf("renamed topic = %q", got)
			}
			if mt := mtimeOf(t, p); !near(mt, old) {
				t.Errorf("renamed topic mtime = %v, want %v", mt, old)
			}
			if !exists(filepath.Join(dst, "logs", "2026", "09", "05", "abcd1234-title.imported-"+testID8+".md")) {
				t.Errorf("log file not renamed")
			}
		})
	}
}

func TestMergeMemoryUnconfirmedOverwriteKeepsExistingIndexAside(t *testing.T) {
	src, _ := stageMemory(t)
	dst := memDst(t)
	write(t, filepath.Join(dst, "MEMORY.md"), "mine\n")
	if _, err := MergeMemory(dst, src, MemoryOverwrite, testID8, false, false, fixedNow); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dst, "MEMORY.md")) {
		t.Errorf("MEMORY.md written on an unconfirmed overwrite")
	}
	if readFile(t, filepath.Join(SetAsideName(dst, fixedNow), "MEMORY.md")) != "mine\n" {
		t.Errorf("old index not set aside")
	}
}

func TestMergeMemoryTrusted(t *testing.T) {
	src, _ := stageMemory(t)
	dst := memDst(t)
	if _, err := MergeMemory(dst, src, MemorySkip, testID8, true, true, fixedNow); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dst, "notes.md")); got != notesSrc {
		t.Errorf("trusted copy changed: %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "logs", "2026", "09", "05", "abcd1234-title.md")); got != logSrc {
		t.Errorf("trusted log changed: %q", got)
	}
}

func TestMergeMemoryMergeAndBadMode(t *testing.T) {
	src, _ := stageMemory(t)
	dst := memDst(t)
	if _, err := MergeMemory(dst, src, MemoryMerge, testID8, true, false, fixedNow); !errors.Is(err, ErrMemoryMergeLater) {
		t.Errorf("merge err = %v", err)
	}
	if _, err := MergeMemory(dst, src, MemoryMode("fork"), testID8, true, false, fixedNow); err == nil || !strings.Contains(err.Error(), `"fork"`) {
		t.Errorf("bad mode err = %v", err)
	}
	if _, err := MergeMemory(dst, src, MemorySkip, "nope", true, false, fixedNow); err == nil {
		t.Errorf("bad id8 accepted")
	}
	if _, err := MergeMemory(dst, filepath.Join(t.TempDir(), "missing"), MemorySkip, testID8, true, false, fixedNow); err == nil {
		t.Errorf("missing source accepted")
	}
	if exists(dst) {
		t.Errorf("destination created by a refused call")
	}
}

func TestMergeMemoryEmptySourceWritesNothing(t *testing.T) {
	src := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := memDst(t)
	mv, err := MergeMemory(dst, src, MemorySkip, testID8, true, false, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if exists(dst) || len(mv.Added) != 0 {
		t.Errorf("empty source produced %v / dir=%v", mv.Added, exists(dst))
	}
	// Only MEMORY.md, unconfirmed: nothing to write either.
	write(t, filepath.Join(src, "MEMORY.md"), "idx\n")
	if _, err := MergeMemory(dst, src, MemorySkip, testID8, false, false, fixedNow); err != nil {
		t.Fatal(err)
	}
	if exists(dst) {
		t.Errorf("directory created with nothing to hold")
	}
}

func TestNeutralisePinned(t *testing.T) {
	cases := map[string]string{
		"pinned: true\n":          "pinned-imported: true\n",
		"  pinned: 'true'\r\n":    "  pinned-imported: 'true'\r\n",
		"pinned:true":             "pinned-imported:true",
		"pinned-imported: true\n": "pinned-imported: true\n",
		"unpinned: true\n":        "unpinned: true\n",
		"name: pinned: yes\n":     "name: pinned: yes\n",
		"pinned\n":                "pinned\n",
	}
	for in, want := range cases {
		if got := neutralisePinned(in); got != want {
			t.Errorf("neutralisePinned(%q) = %q, want %q", in, got, want)
		}
	}
	// Only the leading frontmatter block is rewritten.
	src := filepath.Join(t.TempDir(), "a.md")
	out := t.TempDir()
	root, err := os.OpenRoot(out)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	write(t, src, "# no frontmatter\npinned: true\n---\npinned: true\n")
	if err := writeTopic(root, "b.md", src, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(out, "b.md")); got != "# no frontmatter\npinned: true\n---\npinned: true\n" {
		t.Errorf("body rewritten: %q", got)
	}
	// No trailing newline survives as-is.
	write(t, src, "---\npinned: true\n---\nend")
	if err := writeTopic(root, "c.md", src, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(out, "c.md")); got != "---\npinned-imported: true\n---\nend" {
		t.Errorf("got %q", got)
	}
}

// A symlink planted at the projects/ entry that should hold the memory
// directory leads out of the pool: every mode refuses it and nothing lands
// through it. A symlink at the memory directory itself is "an existing
// entry": skip leaves it alone, overwrite sets the link aside (never
// followed, never deleted) and writes a real directory in its place.
func TestMergeMemoryRefusesPlantedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	src, _ := stageMemory(t)
	newPool := func(t *testing.T) (pool, outside string) {
		t.Helper()
		pool = filepath.Join(t.TempDir(), "projects")
		outside = filepath.Join(t.TempDir(), "outside")
		for _, d := range []string{pool, outside} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		return pool, outside
	}
	untouched := func(t *testing.T, outside, link, target string) {
		t.Helper()
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("wrote through the link into %s: %v", outside, entries)
		}
		if got, err := os.Readlink(link); err != nil || got != target {
			t.Fatalf("the planted link %s was replaced (%q, %v)", link, got, err)
		}
	}

	t.Run("slug dir", func(t *testing.T) {
		pool, outside := newPool(t)
		slug := filepath.Join(pool, "-home-jonas-src-bffs")
		if err := os.Symlink(outside, slug); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(slug, "memory")
		for _, mode := range []MemoryMode{MemorySkip, MemoryOverwrite} {
			if _, err := MergeMemory(dst, src, mode, testID8, true, false, fixedNow); err == nil {
				t.Fatalf("%s: planted symlink at %s was followed", mode, slug)
			}
			untouched(t, outside, slug, outside)
		}
	})
	t.Run("memory dir", func(t *testing.T) {
		pool, outside := newPool(t)
		slug := filepath.Join(pool, "-home-jonas-src-bffs")
		if err := os.MkdirAll(slug, 0o700); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(slug, "memory")
		if err := os.Symlink(outside, dst); err != nil {
			t.Fatal(err)
		}
		mv, err := MergeMemory(dst, src, MemorySkip, testID8, true, false, fixedNow)
		if err != nil || len(mv.Added) != 0 {
			t.Fatalf("skip over a link: %v, %+v", err, mv)
		}
		untouched(t, outside, dst, outside)

		mv, err = MergeMemory(dst, src, MemoryOverwrite, testID8, true, false, fixedNow)
		if err != nil || len(mv.Added) == 0 {
			t.Fatalf("overwrite over a link: %v, %+v", err, mv)
		}
		aside := SetAsideName(dst, fixedNow)
		untouched(t, outside, aside, outside)
		if info, err := os.Lstat(dst); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("memory dir after overwrite: %v, %v", info, err)
		}
		if _, err := os.Stat(filepath.Join(dst, "notes.md")); err != nil {
			t.Fatal(err)
		}
	})
}
