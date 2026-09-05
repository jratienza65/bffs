package transcripts

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

func memoryFixture(t *testing.T) (Root, string) {
	t.Helper()
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	cwd := t.TempDir()
	mkdir(t, filepath.Join(cwd, ".git"))
	norm, err := store.NormalizePath(cwd)
	if err != nil {
		t.Fatal(err)
	}
	slug, err := Slug(norm)
	if err != nil {
		t.Fatal(err)
	}
	slugDir := filepath.Join(root.Dir, slug)
	writeTranscript(t, filepath.Join(slugDir, sidA+".jsonl"), userRec(sidA, cwd, "2026-08-24T10:00:00Z", "x"))
	mem := filepath.Join(slugDir, MemorySubdir)
	writeFile(t, filepath.Join(mem, MemoryIndexFile), "# Memory Index\n\n- [topic.md](./topic.md) — see /Users/jonas/build/x\n")
	writeFile(t, filepath.Join(mem, "topic.md"), "---\nname: topic\npinned: true\n---\n\nRead `@~/.claude/extra.md` and (/home/jonas/src). Twice: /home/jonas/src\n")
	writeFile(t, filepath.Join(mem, "plain.md"), "---\nname: plain\npinned: false\n---\nbody pinned: true\n")
	writeFile(t, filepath.Join(mem, "notes.txt"), "/Users/ignored/because/not/markdown")
	writeFile(t, filepath.Join(mem, MemoryLogsSubdir, "2026", "08", "24", "0f3b2c1e-fix.md"), "log line C:\\Users\\jonas\\x.\n")
	writeFile(t, filepath.Join(mem, MemoryLogsSubdir, "index-cache", "x.md"), "/Users/never\n")
	writeFile(t, filepath.Join(mem, MemoryProposalsSubdir, "p.md"), "/Users/never\n")
	writeFile(t, filepath.Join(mem, "index_persist", "x.md"), "/Users/never\n")
	writeFile(t, filepath.Join(mem, "other", "x.md"), "/Users/never\n")

	// A slug without memory, a reserved entry with one, and a set-aside memory dir.
	writeTranscript(t, filepath.Join(root.Dir, "-Users-a-nomem", sidB+".jsonl"), userRec(sidB, "/Users/a/nomem", "2026-08-24T10:00:00Z", "x"))
	writeFile(t, filepath.Join(root.Dir, "bagel", MemorySubdir, "x.md"), "x")
	writeFile(t, filepath.Join(slugDir, MemorySubdir+".bffs-replaced-1756987654321", "x.md"), "x")
	// A memory dir whose slug has no transcripts.
	writeFile(t, filepath.Join(root.Dir, "-Users-a-orphan", MemorySubdir, "o.md"), "o")
	return root, cwd
}

func TestMemories(t *testing.T) {
	root, cwd := memoryFixture(t)
	norm, _ := store.NormalizePath(cwd)
	mems, err := Memories(context.Background(), []Root{{Dir: filepath.Join(t.TempDir(), "none")}, root})
	if err != nil {
		t.Fatalf("Memories: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("got %d memories: %+v", len(mems), mems)
	}
	orphan := mems[0]
	if orphan.Slug != "-Users-a-orphan" || orphan.Cwd != "" || orphan.HasIndex || len(orphan.Files) != 1 {
		t.Errorf("orphan memory = %+v", orphan)
	}
	m := mems[1]
	if m.Dir != filepath.Join(root.Dir, m.Slug, MemorySubdir) || !m.HasIndex || m.Root.Dir != root.Dir {
		t.Errorf("memory = %+v", m)
	}
	if got, _ := store.NormalizePath(m.Cwd); got != norm || !m.CwdExists {
		t.Errorf("Cwd = %q exists %v, want %q", m.Cwd, m.CwdExists, norm)
	}
	if gr, _ := store.NormalizePath(m.GitRoot); gr != norm {
		t.Errorf("GitRoot = %q, want %q", m.GitRoot, norm)
	}
	var names []string
	byName := map[string]MemoryFile{}
	for _, f := range m.Files {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	want := []string{MemoryIndexFile, "logs/2026/08/24/0f3b2c1e-fix.md", "plain.md", "topic.md"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files = %v, want %v", names, want)
	}
	if !byName["topic.md"].Pinned || byName["plain.md"].Pinned || byName[MemoryIndexFile].Pinned {
		t.Errorf("pinned flags: topic %v plain %v index %v", byName["topic.md"].Pinned, byName["plain.md"].Pinned, byName[MemoryIndexFile].Pinned)
	}
	topic := byName["topic.md"]
	if !reflect.DeepEqual(topic.AbsolutePaths, []string{"/home/jonas/src"}) || !reflect.DeepEqual(topic.AtRefs, []string{"@~/.claude/extra.md"}) {
		t.Errorf("topic refs = abs %v at %v", topic.AbsolutePaths, topic.AtRefs)
	}
	if !reflect.DeepEqual(byName[MemoryIndexFile].AbsolutePaths, []string{"/Users/jonas/build/x"}) {
		t.Errorf("index refs = %v", byName[MemoryIndexFile].AbsolutePaths)
	}
	if got := byName["logs/2026/08/24/0f3b2c1e-fix.md"].AbsolutePaths; !reflect.DeepEqual(got, []string{`C:\Users\jonas\x`}) {
		t.Errorf("log refs = %v", got)
	}
	if byName["topic.md"].Size == 0 || byName["topic.md"].ModTime.IsZero() {
		t.Error("Size/ModTime not filled")
	}
}

// TestMemoriesPinnedScope pins the rule Claude's pinned scan follows: only
// top-level topic files can be pinned — MEMORY.md and logs/ never are,
// whatever their frontmatter says.
func TestMemoriesPinnedScope(t *testing.T) {
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	mem := filepath.Join(root.Dir, "-Users-a-p", MemorySubdir)
	front := "---\npinned: true\n---\nbody\n"
	writeFile(t, filepath.Join(mem, MemoryIndexFile), front)
	writeFile(t, filepath.Join(mem, "topic.md"), front)
	writeFile(t, filepath.Join(mem, MemoryLogsSubdir, "2026", "08", "24", "0f3b2c1e-x.md"), front)
	mems, err := Memories(context.Background(), []Root{root})
	if err != nil || len(mems) != 1 {
		t.Fatalf("Memories = %+v, %v", mems, err)
	}
	for _, f := range mems[0].Files {
		if want := f.Name == "topic.md"; f.Pinned != want {
			t.Errorf("%s pinned = %v, want %v", f.Name, f.Pinned, want)
		}
	}
	if len(mems[0].Files) != 3 {
		t.Errorf("files = %+v", mems[0].Files)
	}
}

func TestMemoriesCancel(t *testing.T) {
	root, _ := memoryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Memories(ctx, []Root{root}); err == nil {
		t.Error("cancelled ctx must fail")
	}
}

func TestScanAbsolutePaths(t *testing.T) {
	root, _ := memoryFixture(t)
	mems, err := Memories(context.Background(), []Root{root})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := ScanAbsolutePaths(mems[1].Dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []PathRef{
		{File: MemoryIndexFile, Line: 3, Path: "/Users/jonas/build/x", Kind: PathKindAbs},
		{File: "logs/2026/08/24/0f3b2c1e-fix.md", Line: 1, Path: `C:\Users\jonas\x`, Kind: PathKindAbs},
		{File: "topic.md", Line: 6, Path: "@~/.claude/extra.md", Kind: PathKindAt},
		{File: "topic.md", Line: 6, Path: "/home/jonas/src", Kind: PathKindAbs},
		{File: "topic.md", Line: 6, Path: "/home/jonas/src", Kind: PathKindAbs},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs =\n%+v\nwant\n%+v", refs, want)
	}
	if _, err := ScanAbsolutePaths(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing dir must be an error")
	}
}

func TestClassifyToken(t *testing.T) {
	cases := []struct {
		tok, path, kind string
		ok              bool
	}{
		{"/Users/jonas/x", "/Users/jonas/x", PathKindAbs, true},
		{"/home/j/x,", "/home/j/x", PathKindAbs, true},
		{"(/root/x).", "/root/x", PathKindAbs, true},
		{"`/tmp/x`;", "/tmp/x", PathKindAbs, true},
		{"\"/private/tmp/x\":", "/private/tmp/x", PathKindAbs, true},
		{"'/opt/x'", "/opt/x", PathKindAbs, true},
		{"[C:\\Users\\x]", "C:\\Users\\x", PathKindAbs, true},
		{"<D:\\x>", "D:\\x", PathKindAbs, true},
		{"@/abs/ref", "@/abs/ref", PathKindAt, true},
		{"@~/rel/ref.md", "@~/rel/ref.md", PathKindAt, true},
		{"@./local.md,", "@./local.md", PathKindAt, true},
		{"`@~/x`", "@~/x", PathKindAt, true},
		{"@claude", "", "", false},
		{"@", "", "", false},
		{"/usr/local/bin", "", "", false},
		{"/etc/hosts", "", "", false},
		{"c:\\lower", "", "", false},
		{"C:/forward", "", "", false},
		{"https://example.com/Users/x", "", "", false},
		{"Users/relative", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		path, kind, ok := classifyToken(c.tok)
		if path != c.path || kind != c.kind || ok != c.ok {
			t.Errorf("classifyToken(%q) = (%q, %q, %v), want (%q, %q, %v)", c.tok, path, kind, ok, c.path, c.kind, c.ok)
		}
	}
}

func TestScanMemoryFileFrontmatter(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content string
		pinned        bool
	}{
		{"pinned", "---\npinned: true\n---\n", true},
		{"quoted", "---\nname: x\npinned: \"true\"\n---\n", true},
		{"nested", "---\nmetadata:\n  pinned: true\n---\n", true},
		{"false", "---\npinned: false\n---\n", false},
		{"after frontmatter", "---\nname: x\n---\npinned: true\n", false},
		{"no frontmatter", "pinned: true\n", false},
		{"unterminated frontmatter", "---\npinned: true\n", true},
		{"not first line", "\n---\npinned: true\n---\n", false},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".md")
		if err := os.WriteFile(p, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if pinned, _ := scanMemoryFile(p, c.name+".md"); pinned != c.pinned {
			t.Errorf("%s: pinned = %v, want %v", c.name, pinned, c.pinned)
		}
	}
}
