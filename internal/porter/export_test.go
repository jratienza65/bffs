package porter

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/transcripts"
)

func TestBuildManifestShape(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	writeFile(t, e.root.ClaudeJSON, `{"projects":{"`+jsonEscape(e.cwd)+`":{"hasTrustDialogAccepted":true,"hasClaudeMdExternalIncludesWarningShown":true}}}`)
	parts := DefaultParts
	parts.Tasks = true
	sel := selectAll(t, e, parts, nil)

	m, src, warnings, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	defer src.(interface{ Close() error }).Close()
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if m.Format != 1 || len(m.BundleID) != 36 || m.BundleID[14] != '4' || m.BFFSVersion != "0.4.0" || m.ClaudeVersion != "2.1.259" || !m.Created.Equal(fixedNow) {
		t.Errorf("header = %+v", *m)
	}
	s := m.Source
	if s.Hostname == "" || s.Account != "aviate" || s.AccountType != "oauth" || s.Isolation != "partial" || s.ConfigDir != e.claudeDir || s.RootDir != e.root.Dir || s.OS != runtime.GOOS || s.Arch != runtime.GOARCH {
		t.Errorf("source = %+v", s)
	}
	if err := m.Validate(bundle.DefaultLimits); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.Totals.Entries != 3 || len(m.Entries) != 3 {
		t.Fatalf("entries = %d (%+v)", len(m.Entries), m.Entries)
	}
	slug := e.slug(t)
	a := entryFor(m, bundle.EntrySession, sidA)
	if a == nil {
		t.Fatal("no entry for sidA")
	}
	if a.Slug != slug || a.Cwd != e.cwd || a.ProjectKey != e.cwd || a.Title != "Title A" || a.GitBranch != "main" || a.PlanSlug != planSlug || a.Started.IsZero() || !a.Last.Equal(fixedNow.Add(-45*day)) || a.LivePossiblyTruncated {
		t.Errorf("sidA entry = %+v", *a)
	}
	if a.SourceTrust == nil || !a.SourceTrust.Accepted || a.SourceTrust.ExternalIncludesApproved || !a.SourceTrust.ExternalIncludesWarningShown {
		t.Errorf("source_trust = %+v", a.SourceTrust)
	}
	// Both sessions share the plan slug; the plan file travels once, with
	// the newer session (sidB, listed first), and sidA keeps plan_slug.
	if got, want := a.Parts, []string{"transcript", "sidecar", "file-history", "history", "tasks"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parts = %v, want %v", got, want)
	}
	want := []string{
		"projects/" + slug + "/" + sidA + ".jsonl",
		"projects/" + slug + "/" + sidA + "/custom-title.json",
		"projects/" + slug + "/" + sidA + "/subagents/agent-1.jsonl",
		"projects/" + slug + "/" + sidA + "/subagents/agent-1.meta.json",
		"projects/" + slug + "/" + sidA + "/tool-results/abc.txt",
		"file-history/" + sidA + "/0123456789abcdef@v1",
		"tasks/" + sidA + "/1.json",
		"history/" + sidA + ".jsonl",
	}
	if got := filePaths(a); !reflect.DeepEqual(got, want) {
		t.Errorf("sidA files =\n%v\nwant\n%v", got, want)
	}
	for _, f := range a.Files {
		if f.Size < 0 || len(f.SHA256) != 64 || f.ModTime.IsZero() {
			t.Errorf("file %+v incomplete", f)
		}
		if f.Path == "projects/"+slug+"/"+sidA+".jsonl" && !near(f.ModTime, fixedNow.Add(-45*day)) {
			t.Errorf("transcript mtime = %v", f.ModTime)
		}
	}
	b := entryFor(m, bundle.EntrySession, sidB)
	if b == nil || b.SourceTrust == nil || hasPath(b, "plans/"+planSlug+".md") == false {
		t.Errorf("sidB entry = %+v", b)
	}
	if got, want := b.Parts, []string{"transcript", "plans", "history"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sidB parts = %v, want %v", got, want)
	}
	mem := entryFor(m, bundle.EntryMemory, "")
	if mem == nil {
		t.Fatal("no memory entry")
	}
	memSlug := filepath.Base(filepath.Dir(e.memDir(t)))
	wantMem := []string{"memory/" + memSlug + "/MEMORY.md", "memory/" + memSlug + "/logs/2026/08/24/0f3b2c1e-fix.md", "memory/" + memSlug + "/topic.md"}
	if got := filePaths(mem); !reflect.DeepEqual(got, wantMem) {
		t.Errorf("memory files = %v, want %v", got, wantMem)
	}
	if mem.Cwd != e.cwd || mem.ProjectKey != e.cwd || mem.Slug != memSlug {
		t.Errorf("memory entry = %+v", *mem)
	}

	// The synthesised history file holds exactly sidA's lines, in order.
	rc, err := src.Open("history/" + sidA + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(rc)
	rc.Close()
	if lines := strings.Split(strings.TrimSpace(buf.String()), "\n"); len(lines) != 2 || !strings.Contains(lines[0], "first prompt A") || !strings.Contains(lines[1], "second prompt A") || strings.Contains(buf.String(), "not ours") {
		t.Errorf("history = %q", buf.String())
	}
}

func jsonEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

func TestBuildManifestParts(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	sel := selectAll(t, e, Parts{Sidecar: true}, nil)
	m, src, _, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	src.(interface{ Close() error }).Close()
	a := entryFor(m, bundle.EntrySession, sidA)
	for _, p := range filePaths(a) {
		if strings.Contains(p, "tool-results") || strings.HasPrefix(p, "file-history/") || strings.HasPrefix(p, "plans/") || strings.HasPrefix(p, "history/") || strings.HasPrefix(p, "tasks/") {
			t.Errorf("part not requested but listed: %s", p)
		}
	}
	if !hasPath(a, "projects/"+e.slug(t)+"/"+sidA+"/subagents/agent-1.jsonl") {
		t.Errorf("sidecar missing: %v", filePaths(a))
	}
	if got, want := a.Parts, []string{"transcript", "sidecar"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parts = %v", got)
	}
	sel = selectAll(t, e, Parts{}, nil)
	m, src, _, err = BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	src.(interface{ Close() error }).Close()
	if a := entryFor(m, bundle.EntrySession, sidA); len(a.Files) != 1 || !reflect.DeepEqual(a.Parts, []string{"transcript"}) {
		t.Errorf("transcript-only entry = %+v", *a)
	}
}

func TestExportSkipsBadNameWithWarning(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	bad := filepath.Join(e.slugDir(t), sidA, "tool-results", "bad name.txt")
	writeFile(t, bad, "x")
	writeFile(t, filepath.Join(e.memDir(t), "index.md"), "looks like an index cache\n")
	sel := selectAll(t, e, DefaultParts, nil)
	m, src, warnings, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	defer src.(interface{ Close() error }).Close()
	if !containsWarning(warnings, "skipped projects/"+e.slug(t)+"/"+sidA+"/tool-results/bad name.txt") {
		t.Errorf("no warning for the bad name: %v", warnings)
	}
	if !containsWarning(warnings, "skipped memory/") || !containsWarning(warnings, "index.md") {
		t.Errorf("no warning for the index cache: %v", warnings)
	}
	if a := entryFor(m, bundle.EntrySession, sidA); hasPath(a, "projects/"+e.slug(t)+"/"+sidA+"/tool-results/bad name.txt") {
		t.Error("bad name listed")
	}
	if err := m.Validate(bundle.DefaultLimits); err != nil {
		t.Errorf("manifest invalid after skips: %v", err)
	}
	var buf bytes.Buffer
	if _, err := Write(context.Background(), &buf, m, src, exportOpts()); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestExportSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	e := newEnv(t)
	seedPool(t, e)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, outside, "secret")
	link := filepath.Join(e.slugDir(t), sidA, "subagents", "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(e.claudeDir, transcripts.FileHistorySubdir, sidB)
	if err := os.Symlink(filepath.Dir(outside), dirLink); err != nil {
		t.Fatal(err)
	}
	sel := selectAll(t, e, DefaultParts, nil)
	m, src, warnings, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	src.(interface{ Close() error }).Close()
	if !containsWarning(warnings, "link.jsonl: symlink") || !containsWarning(warnings, "file-history/"+sidB+": symlink") {
		t.Errorf("warnings = %v", warnings)
	}
	for _, en := range m.Entries {
		for _, f := range en.Files {
			if strings.Contains(f.Path, "link.jsonl") || strings.HasPrefix(f.Path, "file-history/"+sidB) {
				t.Errorf("symlink exported: %s", f.Path)
			}
		}
	}
}

func TestExportLiveTranscriptStreamsOldInode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("renaming over an open file")
	}
	e := newEnv(t)
	seedPool(t, e)
	tr := filepath.Join(e.slugDir(t), sidB+".jsonl")
	old := readFile(t, tr)
	live := map[string]transcripts.LiveSession{sidB: {PID: 1, SessionID: sidB}}
	sel := selectAll(t, e, Parts{}, live)
	m, src, _, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	b := entryFor(m, bundle.EntrySession, sidB)
	if b == nil || !b.LivePossiblyTruncated {
		t.Fatalf("live flag not set: %+v", b)
	}
	// Claude compacts: a new file is written and renamed over the old one.
	compact := tr + ".compact.tmp.1"
	writeFile(t, compact, userRec(sidB, e.cwd, "2026-08-14T10:00:00Z", "compacted")+"\n")
	if err := os.Rename(compact, tr); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Write(context.Background(), &buf, m, src, exportOpts()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	staging := filepath.Join(t.TempDir(), "stage")
	mkdir(t, staging)
	if _, err := bundle.Unpack(context.Background(), bytes.NewReader(buf.Bytes()), staging, "", bundle.DefaultLimits, true, nil); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if got := readFile(t, filepath.Join(staging, "projects", e.slug(t), sidB+".jsonl")); got != old {
		t.Errorf("bundle carries %q, want the pre-compaction bytes %q", got, old)
	}
	// The opener's remaining handles are released by Write.
	if o := src.(*opener); len(o.files) != 0 {
		for p, s := range o.files {
			if s.file != nil {
				t.Errorf("handle for %s still open", p)
			}
		}
	}
}

func TestExportGitRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newEnv(t)
	seedPool(t, e)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", e.cwd}, args...)...)
		cmd.Env = childEnv("GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.RemoveAll(filepath.Join(e.cwd, ".git")); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:jratienza65/bffs.git")
	t.Setenv("BFFS_TRANSFER_CODE", "7K3Q-M9XD")
	sel := selectAll(t, e, Parts{}, nil)
	m, src, _, err := BuildManifest(context.Background(), sel, exportOpts())
	if err != nil {
		t.Fatal(err)
	}
	src.(interface{ Close() error }).Close()
	if a := entryFor(m, bundle.EntrySession, sidA); a.GitRemote != "git@github.com:jratienza65/bffs.git" {
		t.Errorf("git_remote = %q", a.GitRemote)
	}
	for _, kv := range childEnv() {
		if strings.HasPrefix(kv, "BFFS_TRANSFER_CODE=") {
			t.Error("transfer code leaked into the child environment")
		}
	}
}

func TestExportNothingSelected(t *testing.T) {
	e := newEnv(t)
	sel := Selection{Root: e.root, Parts: DefaultParts}
	if _, _, _, err := BuildManifest(context.Background(), sel, exportOpts()); err == nil || !strings.Contains(err.Error(), "nothing to export") {
		t.Errorf("err = %v", err)
	}
}

func TestIdentAndBundleID(t *testing.T) {
	if got := ident("mac-a.local"); got != "mac-a.local" {
		t.Errorf("ident = %q", got)
	}
	if got := ident("Jonas' Mac\x1b[2J"); got != "JonasMac2J" {
		t.Errorf("ident = %q", got)
	}
	if got := ident(strings.Repeat("x", 100)); len(got) != 64 {
		t.Errorf("ident cap = %d", len(got))
	}
	id, err := newBundleID()
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.ValidateEntryName(id); err != nil || len(id) != 36 || id[14] != '4' || !strings.ContainsAny(id[19:20], "89ab") {
		t.Errorf("bundle id %q", id)
	}
}
