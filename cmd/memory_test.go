package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

func testMemories(home string) []transcripts.Memory {
	project := filepath.Join(home, "build", "projects", "bffs")
	shared := sharedRoot(home)
	return []transcripts.Memory{
		{
			Root: shared, Slug: "-Users-jonas-build-projects-bffs", Dir: filepath.Join(shared.Dir, "-Users-jonas-build-projects-bffs", "memory"),
			Cwd: project, GitRoot: project, CwdExists: true, HasIndex: true,
			Files: []transcripts.MemoryFile{
				{Name: "MEMORY.md", Size: 1_200, ModTime: catalogNow.Add(-2 * time.Hour), AtRefs: []string{"@~/notes.md"}},
				{Name: "notes.md", Size: 3_400, ModTime: catalogNow.Add(-26 * time.Hour), Pinned: true, AbsolutePaths: []string{"/Users/jonas/x", "/Users/jonas/y"}},
				{Name: "logs/2026/08/24/a.md", Size: 10, ModTime: catalogNow.Add(-3 * time.Hour), AbsolutePaths: []string{"/Users/jonas/x"}},
			},
		},
		{
			Root: ownedRoot(home, "work"), Slug: "-tmp-scratch", Dir: filepath.Join(home, "bffs", "sessions", "work", "projects", "-tmp-scratch", "memory"),
			Files: []transcripts.MemoryFile{{Name: "topic.md", Size: 20, ModTime: catalogNow.Add(-5 * 24 * time.Hour)}},
		},
	}
}

func TestRenderMemoryList(t *testing.T) {
	home := fakeHome(t)
	var sb strings.Builder
	if err := renderMemoryList(&sb, testMemories(home), "", catalogNow); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"PROJECT", "ROOT", "FILES", "INDEX", "MODIFIED", "ABS-PATHS", "@REFS",
		filepath.Join("~", "build", "projects", "bffs"), "shared", "yes", "2h ago",
		"-tmp-scratch", "work", "5d ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	shared := lineContaining(out, "shared")
	fields := strings.Fields(shared)
	// PROJECT ROOT FILES INDEX MODIFIED(2 words) ABS-PATHS @REFS: distinct
	// paths across files, not the sum of per-file lists.
	if len(fields) != 8 || fields[2] != "3" || fields[3] != "yes" || fields[6] != "2" || fields[7] != "1" {
		t.Errorf("shared row = %q", shared)
	}
	if !strings.Contains(lineContaining(out, "-tmp-scratch"), "  -  ") {
		t.Errorf("missing index not dashed:\n%s", out)
	}

	sb.Reset()
	if err := renderMemoryList(&sb, nil, filepath.Join(home, "x"), catalogNow); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sb.String()); got != "no memory dir for "+filepath.Join("~", "x") {
		t.Errorf("empty listing = %q", got)
	}
	sb.Reset()
	_ = renderMemoryList(&sb, nil, "", catalogNow)
	if got := strings.TrimSpace(sb.String()); got != "no memory dirs" {
		t.Errorf("empty all-projects listing = %q", got)
	}
}

func TestRenderMemoryShow(t *testing.T) {
	home := fakeHome(t)
	m := testMemories(home)[0]
	project := filepath.Join(home, "build", "projects", "bffs")
	v := memoryView{Memory: m, Project: project, Key: project, Entries: []memoryEntry{
		{Title: osc52 + "Build notes", File: "notes.md", Description: "how the shim is built"},
		{Title: "Scratch", File: "scratch.md"},
	}}
	var sb strings.Builder
	if err := renderMemoryShow(&sb, v, catalogNow); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"project:  " + filepath.Join("~", "build", "projects", "bffs") + "   (key: " + project + ")",
		"memory:   " + filepath.Join("~", ".claude", "projects", "-Users-jonas-build-projects-bffs", "memory") + "   (shared pool — visible to: aviate, innomind)",
		"MEMORY.md  2 entries   modified 2h ago",
		"- [Build notes](notes.md) - how the shim is built",
		"- [Scratch](scratch.md)\n",
		"NAME", "SIZE", "MODIFIED", "PINNED", "ABS-PATHS", "@REFS",
		"notes.md", "1d ago", "yes", "logs/2026/08/24/a.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence reached the screen:\n%q", out)
	}
	notes := lineContaining(out, "notes.md  ")
	// NAME SIZE(2 words) MODIFIED(2 words) PINNED ABS-PATHS @REFS
	if f := strings.Fields(notes); len(f) != 8 || f[5] != "yes" || f[6] != "2" || f[7] != "0" {
		t.Errorf("notes row = %q", notes)
	}

	// No index file, a full-isolation root.
	v.Memory = testMemories(home)[1]
	v.Entries = nil
	sb.Reset()
	if err := renderMemoryShow(&sb, v, catalogNow); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "MEMORY.md  (missing)") || !strings.Contains(sb.String(), "(account: work)") {
		t.Errorf("owned memory without index:\n%s", sb.String())
	}
}

func TestMemoryVisibility(t *testing.T) {
	home := fakeHome(t)
	if got := memoryVisibility(transcripts.Root{ConfigDir: filepath.Join(home, ".claude")}); got != "home — unmanaged "+filepath.Join("~", ".claude") {
		t.Errorf("home: %q", got)
	}
	if got := memoryVisibility(transcripts.Root{Owner: "gone", Orphan: true}); !strings.Contains(got, "orphan") {
		t.Errorf("orphan: %q", got)
	}
}

func TestMemoryIndexEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	content := strings.Join([]string{
		"# Memory",
		"",
		"- [Build notes](notes.md) - how the shim is built",
		"* [Dash variant](dash.md) — em dash description",
		"  - [Indented](indent.md): colon description",
		"- [No description](bare.md)",
		"- not an entry",
		"[link](x.md) without a bullet",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := memoryIndexEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []memoryEntry{
		{"Build notes", "notes.md", "how the shim is built"},
		{"Dash variant", "dash.md", "em dash description"},
		{"Indented", "indent.md", "colon description"},
		{"No description", "bare.md", ""},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
	if entries, err := memoryIndexEntries(filepath.Join(dir, "absent.md")); err != nil || entries != nil {
		t.Errorf("missing index: %v, %v", entries, err)
	}
}

func TestRenderScanPaths(t *testing.T) {
	home := fakeHome(t)
	dirA := filepath.Join(home, ".claude", "projects", "-a", "memory")
	refs := []transcripts.PathRef{
		{File: "notes.md", Line: 3, Path: "/Users/jonas/x", Kind: transcripts.PathKindAbs},
		{File: "MEMORY.md", Line: 1, Path: "@~/foo.md", Kind: transcripts.PathKindAt},
		{File: "logs/2026/08/24/a.md", Line: 2, Path: osc52 + "/tmp/evil", Kind: transcripts.PathKindAbs},
	}
	var sb strings.Builder
	if err := renderScanPaths(&sb, []scannedDir{{Dir: dirA, Refs: refs}}); err != nil {
		t.Fatal(err)
	}
	want := "notes.md:3: /Users/jonas/x\n@ref MEMORY.md:1: @~/foo.md\nlogs/2026/08/24/a.md:2: /tmp/evil\n"
	if sb.String() != want {
		t.Errorf("single dir:\n got %q\nwant %q", sb.String(), want)
	}

	// Several directories: files are prefixed with their directory.
	dirB := filepath.Join(home, ".claude", "projects", "-b", "memory")
	sb.Reset()
	if err := renderScanPaths(&sb, []scannedDir{{Dir: dirA, Refs: refs[:1]}, {Dir: dirB, Refs: refs[1:2]}}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"~/.claude/projects/-a/memory/notes.md:3: /Users/jonas/x\n",
		"@ref ~/.claude/projects/-b/memory/MEMORY.md:1: @~/foo.md\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("multi dir missing %q:\n%s", want, out)
		}
	}
}

func TestMemoryInfoJSONShape(t *testing.T) {
	home := fakeHome(t)
	m := testMemories(home)[1]
	raw, err := json.Marshal(memoryInfoOf(m))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"root", "cwd", "cwd_exists", "git_root", "slug", "dir", "has_index", "files"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("key %q missing from %s", k, raw)
		}
	}
	files, _ := doc["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", doc["files"])
	}
	file := files[0].(map[string]any)
	for _, k := range []string{"name", "size_bytes", "modified_at", "pinned", "absolute_paths", "at_refs"} {
		if _, ok := file[k]; !ok {
			t.Errorf("file key %q missing from %s", k, raw)
		}
	}
	// Empty lists are arrays, never null.
	if _, ok := file["absolute_paths"].([]any); !ok {
		t.Errorf("absolute_paths should be an array: %s", raw)
	}
	// cwd and git_root come from transcripts: sanitised like every other
	// transcript-derived string.
	hostile := m
	hostile.Cwd, hostile.GitRoot = osc52+m.Cwd, osc52+m.GitRoot
	if info := memoryInfoOf(hostile); info.Cwd != m.Cwd || info.GitRoot != m.GitRoot {
		t.Errorf("cwd/git_root not sanitised: %+v", info)
	}
	var sb strings.Builder
	if err := writeMemoriesJSON(&sb, nil); err != nil || strings.TrimSpace(sb.String()) != "[]" {
		t.Errorf("empty listing = %q, %v", sb.String(), err)
	}
}

func TestSelectMemories(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}})
	root := filepath.Join(f.claudeDir, "projects")
	memDir := filepath.Join(root, f.slug, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- [Notes](notes.md) - see /Users/jonas/x and @~/foo.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "notes.md"), []byte("---\npinned: true\n---\nplain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A second project's memory, only listed under every-project.
	otherMem := filepath.Join(root, "-other-proj", "memory")
	if err := os.MkdirAll(otherMem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherMem, "MEMORY.md"), []byte("# nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := f.env()
	home, _ := env.homeRoot()

	mems, warnings, err := selectMemories(t.Context(), memoryQuery{Roots: []transcripts.Root{home}, Project: f.project})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("selectMemories: %v, %v", err, warnings)
	}
	if len(mems) != 1 || mems[0].Dir != memDir || !mems[0].HasIndex || len(mems[0].Files) != 2 {
		t.Fatalf("mems = %+v", mems)
	}
	for _, file := range mems[0].Files {
		switch file.Name {
		case "MEMORY.md":
			if len(file.AbsolutePaths) != 1 || len(file.AtRefs) != 1 {
				t.Errorf("MEMORY.md refs = %+v", file)
			}
		case "notes.md":
			if !file.Pinned {
				t.Errorf("notes.md should be pinned: %+v", file)
			}
		}
	}
	entries, err := memoryIndexEntries(filepath.Join(mems[0].Dir, transcripts.MemoryIndexFile))
	if err != nil || len(entries) != 1 || entries[0].Title != "Notes" {
		t.Errorf("entries = %+v, %v", entries, err)
	}

	all, _, err := selectMemories(t.Context(), memoryQuery{Roots: []transcripts.Root{home}})
	if err != nil || len(all) != 2 {
		t.Errorf("every project: %d memories, %v", len(all), err)
	}

	// A project with no memory dir yields nothing (and no error).
	none, _, err := selectMemories(t.Context(), memoryQuery{Roots: []transcripts.Root{home}, Project: filepath.Join(f.project, "sub")})
	if err != nil || len(none) != 0 {
		t.Errorf("no memory: %+v, %v", none, err)
	}

	// An overridden memory location is a warning, not a listing.
	t.Setenv("CLAUDE_CODE_REMOTE_MEMORY_DIR", filepath.Join(f.claudeDir, "elsewhere"))
	mems, warnings, err = selectMemories(t.Context(), memoryQuery{Roots: []transcripts.Root{home}, Project: f.project})
	if err != nil || len(mems) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], transcripts.ErrMemoryDirOverridden.Error()) {
		t.Errorf("overridden: mems=%d warnings=%v err=%v", len(mems), warnings, err)
	}

	// The scan the command runs over the selected directory.
	refs, err := transcripts.ScanAbsolutePaths(memDir)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	_ = renderScanPaths(&sb, []scannedDir{{Dir: memDir, Refs: refs}})
	if out := sb.String(); !strings.Contains(out, "MEMORY.md:1: /Users/jonas/x") || !strings.Contains(out, "@ref MEMORY.md:1: @~/foo.md") {
		t.Errorf("scan-paths output:\n%s", out)
	}
}
