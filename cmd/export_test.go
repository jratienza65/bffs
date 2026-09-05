package cmd

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

const testBundleID = "6f1e2c0a-0000-4000-8000-000000000001"

// memOpener serves bundle paths from memory (the history files the export
// summary counts lines in).
type memOpener map[string][]byte

func (m memOpener) Open(p string) (io.ReadCloser, error) {
	b, ok := m[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// newSplitCmd is newTestCmd with separate stdout and stderr buffers, for
// the commands whose stdout may carry bundle bytes.
func newSplitCmd(stdin string) (*cobra.Command, *prompter, *bytes.Buffer, *bytes.Buffer) {
	c := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetIn(strings.NewReader(stdin))
	return c, newPrompter(c.InOrStdin(), errOut), out, errOut
}

// testManifest is a two-session, one-memory bundle of the fixture project
// under home, with a hostile title on the newest session.
func testManifest(home string) *bundle.Manifest {
	project := filepath.Join(home, "build", "projects", "bffs")
	slug := "-Users-jonas-build-projects-bffs"
	m := &bundle.Manifest{
		Format: bundle.FormatVersion, BundleID: testBundleID, BFFSVersion: "0.3.0", ClaudeVersion: "2.1.259", Created: catalogNow,
		Source: bundle.Source{Hostname: "mac-a", User: "jonas", Home: "/Users/jonas", OS: "darwin", Arch: "arm64", Account: "aviate", AccountType: "oauth", Isolation: "partial"},
		Entries: []bundle.Entry{
			{
				Kind: bundle.EntrySession, Slug: slug, Cwd: project, SessionID: testSID1, Title: osc52 + "Plan: session export",
				Last: catalogNow.Add(-2 * time.Hour), LivePossiblyTruncated: true, SourceTrust: &bundle.TrustInfo{Accepted: true},
				Files: []bundle.File{
					{Path: "projects/" + slug + "/" + testSID1 + ".jsonl", Size: 100_000_000},
					{Path: "projects/" + slug + "/" + testSID1 + "/tool-results/a.txt", Size: 96_000_000},
					{Path: "file-history/" + testSID1 + "/0123456789abcdef@v1", Size: 11_000_000},
					{Path: "history/" + testSID1 + ".jsonl", Size: 120},
				},
			},
			{
				Kind: bundle.EntrySession, Slug: slug, Cwd: project, SessionID: testSID2, Last: catalogNow.Add(-9 * 24 * time.Hour),
				Files: []bundle.File{{Path: "projects/" + slug + "/" + testSID2 + ".jsonl", Size: 400_000}},
			},
			{
				Kind: bundle.EntryMemory, Slug: slug, Cwd: project,
				Files: []bundle.File{{Path: "memory/" + slug + "/MEMORY.md", Size: 1_000}, {Path: "memory/" + slug + "/topic.md", Size: 13_000}},
			},
		},
	}
	for _, e := range m.Entries {
		m.Totals.Entries++
		for _, f := range e.Files {
			m.Totals.Files++
			m.Totals.Bytes += f.Size
		}
	}
	return m
}

func TestRenderExportSummary(t *testing.T) {
	home := fakeHome(t)
	m := testManifest(home)
	src := memOpener{"history/" + testSID1 + ".jsonl": []byte("{}\n{}\n{}\n")}
	sum := newExportSummary(sharedRoot(home), m, src, porter.DefaultParts)
	var sb strings.Builder
	renderExportSummary(&sb, sum, catalogNow)
	out := sb.String()
	for _, want := range []string{
		"Exporting from the shared pool (" + filepath.Join("~", ".claude") + "; partial isolation: aviate, innomind):",
		"  project " + filepath.Join("~", "build", "projects", "bffs"),
		`    2 sessions   (newest: "Plan: session export", 2h ago)   207.4 MB   1 live (may be truncated)`,
		"      tool-results 96.0 MB (saved tool outputs — may contain pasted secrets)   file-history 11.0 MB (backups of files Claude edited)   history 3 lines (prompt history)",
		"    memory        2 files   14 KB",
		"  total 207.4 MB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "52;c;") {
		t.Errorf("escape sequence reached the summary:\n%q", out)
	}

	// Parts left out are labelled, not counted as zero.
	parts := porter.Parts{Sidecar: true, ToolResults: true}
	sb.Reset()
	renderExportSummary(&sb, newExportSummary(ownedRoot(home, "work"), m, src, parts), catalogNow)
	out = sb.String()
	for _, want := range []string{
		`Exporting from account "work" (` + filepath.Join("~", "bffs", "sessions", "work") + "; full isolation):",
		"tool-results 96.0 MB", "file-history excluded", "history excluded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestExportRootLabel(t *testing.T) {
	home := fakeHome(t)
	cases := []struct {
		root transcripts.Root
		want string
	}{
		{sharedRoot(home), "the shared pool (" + filepath.Join("~", ".claude") + "; partial isolation: aviate, innomind)"},
		{ownedRoot(home, "work"), `account "work" (` + filepath.Join("~", "bffs", "sessions", "work") + "; full isolation)"},
		{transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Owner: "gone", Orphan: true}, `orphan session dir "gone" (/x; read-only)`},
		{transcripts.Root{Dir: filepath.Join(home, ".claude", "projects"), ConfigDir: filepath.Join(home, ".claude")}, filepath.Join("~", ".claude") + " (home, unmanaged)"},
	}
	for _, tc := range cases {
		if got := exportRootLabel(tc.root); got != tc.want {
			t.Errorf("exportRootLabel = %q, want %q", got, tc.want)
		}
	}
}

func TestExportOutPath(t *testing.T) {
	cwd := t.TempDir()
	got, err := exportOutPath("auto", cwd, "mac-a", catalogNow)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "bffs-mac-a-"+catalogNow.Format("20060102-1504")+".bffs" || filepath.Dir(got) != filepath.Clean(cwd) {
		t.Errorf("auto name = %q", got)
	}
	if got2, _ := exportOutPath("", cwd, "", catalogNow); filepath.Base(got2) != "bffs-host-"+catalogNow.Format("20060102-1504")+".bffs" {
		t.Errorf("empty host fallback = %q", got2)
	}
	explicit := filepath.Join(cwd, "sub", "x.bffs")
	if got, err := exportOutPath(explicit, cwd, "mac-a", catalogNow); err != nil || got != mustNormalize(t, explicit) {
		t.Errorf("explicit = %q, %v", got, err)
	}
}

func mustNormalize(t *testing.T, p string) string {
	t.Helper()
	n, err := store.NormalizePath(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConfirmOrAbort(t *testing.T) {
	cases := []struct {
		name    string
		stdin   string
		yes     bool
		tty     bool
		want    bool
		wantErr string
		wantOut string
	}{
		{"yes flag skips the prompt", "", true, false, true, "", ""},
		{"y", "y\n", false, true, true, "", "go? [y/N] "},
		{"yes", "YES\n", false, true, true, "", ""},
		{"n", "n\n", false, true, false, "", "aborted"},
		{"empty answer", "\n", false, true, false, "", "aborted"},
		{"no input", "", false, true, false, "", "aborted"},
		{"non-tty", "y\n", false, false, false, "stdin is not a terminal; pass -y to confirm", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, pr, out := newTestCmd(tc.stdin)
			got, err := confirmOrAbort(pr, "go? [y/N] ", tc.yes, tc.tty)
			if got != tc.want {
				t.Errorf("ok = %v, want %v", got, tc.want)
			}
			if (err == nil) != (tc.wantErr == "") || (err != nil && err.Error() != tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output %q lacks %q", out.String(), tc.wantOut)
			}
			if tc.tty && !tc.yes && !strings.Contains(out.String(), "go? [y/N] ") {
				t.Errorf("prompt not shown: %q", out.String())
			}
			if (!tc.tty || tc.yes) && strings.Contains(out.String(), "[y/N]") {
				t.Errorf("prompt shown without a terminal: %q", out.String())
			}
		})
	}
}

// exportFixture is machine A: a bffs config dir, a fake ~/.claude with one
// session of the fixture project and a memory directory with a pinned
// topic file.
type exportFixture struct {
	*catalogFixture
	memDir string
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	f := newCatalogFixture(t, store.Accounts{})
	root := filepath.Join(f.claudeDir, "projects")
	f.transcript(root, testSID1, "first prompt of one", catalogNow.Add(-time.Hour))
	home, err := transcripts.RootFor(f.env().roots, transcripts.HomeName)
	if err != nil {
		t.Fatal(err)
	}
	memDir, err := transcripts.MemoryDirFor(home, f.project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("# Memory Index\n- [topic](topic.md) - notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "topic.md"), []byte("---\nname: topic\npinned: true\n---\nnotes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &exportFixture{catalogFixture: f, memDir: memDir}
}

func (f *exportFixture) request(out string) exportRequest {
	return exportRequest{
		Projects:  []string{f.project},
		Parts:     porter.DefaultParts,
		Out:       out,
		Yes:       true,
		ClaudeDir: f.claudeDir,
		Cwd:       f.project,
		Now:       catalogNow,
		Version:   "0.4.0-test",
	}
}

// importMachine is machine B: an empty bffs config dir and Claude config
// dir beside the fixture's, sharing HOME and the project directory.
type importMachine struct {
	cfgDir, claudeDir string
}

func newImportMachine(t *testing.T) *importMachine {
	t.Helper()
	base := t.TempDir()
	m := &importMachine{cfgDir: filepath.Join(base, "bffs"), claudeDir: filepath.Join(base, "claude")}
	if err := store.SaveAccounts(m.cfgDir, store.Accounts{}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *importMachine) request(from string) importRequest {
	return importRequest{
		From:       from,
		OnConflict: string(porter.ConflictSkip),
		Memory:     string(rehomeSkip),
		Yes:        true,
		ClaudeDir:  m.claudeDir,
		Cwd:        m.cfgDir,
		Now:        catalogNow,
	}
}

// rehomeSkip mirrors rehome.MemorySkip without importing rehome here.
const rehomeSkip = "skip"

// TestExportImportRoundTrip drives runExport and runImport the way the
// commands do: machine A exports its project to a file, machine B imports
// it by identity (the directory exists), and the receipt, the record and
// the landed files all name the session.
func TestExportImportRoundTrip(t *testing.T) {
	a := newExportFixture(t)
	file := filepath.Join(t.TempDir(), "mac-a.bffs")

	c, pr, out, errOut := newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err != nil {
		t.Fatalf("runExport: %v\nstderr: %s", err, errOut.String())
	}
	for _, want := range []string{
		"Exporting from " + filepath.Join("~", ".claude") + " (home, unmanaged):",
		"  project " + short(a.project),
		`    1 session   (newest: "first prompt of one", 1h ago)`,
		"tool-results 0 KB", "history 0 lines",
		"    memory        2 files",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, errOut.String())
		}
	}
	if !strings.HasPrefix(out.String(), "wrote "+short(file)+" (") || !strings.Contains(out.String(), "1 session, 2 memory files, bundle ") {
		t.Errorf("wrote line = %q", out.String())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(bundle.Magic)) {
		t.Fatalf("file does not start with the bundle magic: %q", raw[:8])
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Errorf("temp file left beside the bundle: %v", entries)
	}

	// An existing file is refused without --force.
	c, pr, _, _ = newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err == nil || !strings.Contains(err.Error(), "--force to overwrite") {
		t.Errorf("existing file: err = %v", err)
	}

	b := newImportMachine(t)

	// Dry run: the plan, nothing written.
	c, pr, out, errOut = newSplitCmd("")
	dry := b.request(file)
	dry.DryRun, dry.Yes = true, false
	if err := runImport(c, b.cfgDir, pr, dry, false); err != nil {
		t.Fatalf("dry run: %v\nstderr: %s", err, errOut.String())
	}
	for _, want := range []string{
		"Bundle ", " from ", "bffs 0.4.0-test",
		"  project " + a.project + "        exists here ✓ (same directory — no rehome needed)",
		"    1 session  ", "   memory 2 files",
		"    note: memory files in this bundle will be loaded into every future claude session for " + a.project,
		"(pinned files arrive unpinned; --trust-memory keeps them pinned)",
		"Target: home (" + short(b.claudeDir) + ", unmanaged claude)   limit 2.0 GB   retention: 30 days (default)",
		"dry run: nothing is written",
		"  sessions   1 would be committed to projects/" + a.slug + "/; 0 skipped",
		"  memory     files would be written to",
		"Dry run — nothing written.",
		"claude --resume " + testSID1,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Import record:") || strings.Contains(out.String(), "[y/N]") {
		t.Errorf("dry run printed a record or a prompt:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(b.claudeDir, "projects")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dry run wrote into the destination: %v", err)
	}

	// The real import.
	c, pr, out, errOut = newSplitCmd("")
	if err := runImport(c, b.cfgDir, pr, b.request(file), false); err != nil {
		t.Fatalf("runImport: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	recs, err := imports.Load(b.cfgDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	rec := recs[0]
	id8 := short8(rec.BundleID)
	for _, want := range []string{
		"  sessions   1 committed to projects/" + a.slug + "/; 0 skipped",
		"  memory     2 files into " + short(filepath.Join(b.claudeDir, "projects", a.slug, "memory")) + " (side files: *.imported-" + id8 + ".md; MEMORY.md untouched)",
		"  history    no new prompt lines",
		"Done in ", ". Check it:",
		"    cd " + shellWord(a.project) + " && claude --resume " + testSID1,
		"note: the first claude launch there asks about folder trust",
		"Import record: " + short(imports.Path(b.cfgDir, rec.BundleID)) + "  (bffs sessions imports)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("receipt missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("-y must skip the prompt:\n%s", out.String())
	}
	if len(rec.Sessions) != 1 || rec.Sessions[0].ID != testSID1 || rec.Sessions[0].Status != imports.StatusPlaced || rec.Sessions[0].Slug != a.slug {
		t.Errorf("record sessions = %+v", rec.Sessions)
	}
	landed := filepath.Join(b.claudeDir, "projects", a.slug, testSID1+".jsonl")
	got, err := os.ReadFile(landed)
	if err != nil {
		t.Fatalf("transcript not landed: %v", err)
	}
	orig, _ := os.ReadFile(filepath.Join(a.claudeDir, "projects", a.slug, testSID1+".jsonl"))
	if !bytes.Equal(got, orig) {
		t.Errorf("landed transcript differs from the source")
	}
	memDir := filepath.Join(b.claudeDir, "projects", a.slug, "memory")
	if _, err := os.Stat(filepath.Join(memDir, "MEMORY.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unconfirmed placement must not create MEMORY.md: %v", err)
	}
	topic, err := os.ReadFile(filepath.Join(memDir, "topic.imported-"+id8+".md"))
	if err != nil {
		t.Fatalf("topic side file: %v", err)
	}
	if !strings.Contains(string(topic), "pinned-imported: true") || strings.Contains(string(topic), "\npinned: true") {
		t.Errorf("pinned frontmatter not neutralised:\n%s", topic)
	}
	if entries, _ := os.ReadDir(filepath.Join(b.cfgDir, "staging")); len(entries) != 0 {
		t.Errorf("staging not cleaned: %v", entries)
	}

	// The record shows up in `bffs sessions imports`.
	var sb strings.Builder
	if err := renderImportsTable(&sb, b.cfgDir, recs, catalogNow); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BUNDLE", "FROM", "ACCOUNT", "IMPORTED", "SESSIONS", "PENDING", "MEMORY", id8, "home", "1 import under"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("imports table missing %q:\n%s", want, sb.String())
		}
	}
	if got := lastFields(lineContaining(sb.String(), id8), 3); got != "1 0 1" {
		t.Errorf("SESSIONS PENDING MEMORY = %q", got)
	}

	// A second import of the same bundle is refused without --force.
	c, pr, _, _ = newSplitCmd("")
	err = runImport(c, b.cfgDir, pr, b.request(file), false)
	if err == nil || !strings.Contains(err.Error(), "was imported on") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("repeat import: err = %v", err)
	}
	if exitCode(err) != 1 {
		t.Errorf("exit code = %d", exitCode(err))
	}
}

// The confirmation flow of runExport with piped input: n aborts, y
// proceeds, and no terminal without -y refuses before writing.
func TestExportConfirmFlow(t *testing.T) {
	a := newExportFixture(t)
	file := filepath.Join(t.TempDir(), "x.bffs")

	c, pr, out, errOut := newSplitCmd("n\n")
	req := a.request(file)
	req.Yes = false
	if err := runExport(c, a.cfgDir, pr, req, true); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if !strings.Contains(errOut.String(), "Proceed? [y/N] ") || !strings.Contains(errOut.String(), "aborted") {
		t.Errorf("decline output:\n%s", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty on decline: %q", out.String())
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("declined export wrote %s", file)
	}

	c, pr, _, errOut = newSplitCmd("")
	err := runExport(c, a.cfgDir, pr, req, false)
	if err == nil || !strings.Contains(err.Error(), "pass -y") {
		t.Errorf("non-tty: err = %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("refused export wrote %s", file)
	}

	c, pr, out, errOut = newSplitCmd("y\n")
	if err := runExport(c, a.cfgDir, pr, req, true); err != nil {
		t.Fatalf("accept: %v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "wrote ") {
		t.Errorf("accept stdout = %q", out.String())
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("accepted export did not write %s: %v", file, err)
	}
}

// --out - puts the bundle on stdout and everything else on stderr, and
// asks nothing when no terminal is attached.
func TestExportToStdout(t *testing.T) {
	a := newExportFixture(t)
	c, pr, out, errOut := newSplitCmd("")
	req := a.request(exportOutStdout)
	req.Yes = false
	if err := runExport(c, a.cfgDir, pr, req, false); err != nil {
		t.Fatalf("runExport: %v\n%s", err, errOut.String())
	}
	if !bytes.HasPrefix(out.Bytes(), []byte(bundle.Magic)) {
		t.Errorf("stdout does not start with the bundle magic: %q", out.Bytes()[:min(8, out.Len())])
	}
	if strings.Contains(errOut.String(), "[y/N]") || !strings.Contains(errOut.String(), "Exporting from") || !strings.Contains(errOut.String(), "wrote "+formatSize(int64(out.Len()))+" to stdout (1 session, 2 memory files, bundle ") {
		t.Errorf("stderr:\n%s", errOut.String())
	}

	// With a terminal the question is asked; a decline writes nothing.
	c, pr, out, errOut = newSplitCmd("n\n")
	if err := runExport(c, a.cfgDir, pr, req, true); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "aborted") {
		t.Errorf("tty decline: stdout %d bytes, stderr %q", out.Len(), errOut.String())
	}
}

func TestResolveExportSource(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull},
		"api":  {Type: store.TypeAPIKey, Secret: "sk-ant-test-1234"},
	}})
	env := f.env()
	home, _ := env.homeRoot()

	src, warning, err := resolveExportSource(f.cfgDir, env, "", f.project)
	if err != nil || warning != "" || src.root.Dir != home.Dir || src.account.Name != "" {
		t.Errorf("no pin: %+v, %q, %v", src, warning, err)
	}
	src, _, err = resolveExportSource(f.cfgDir, env, "api", f.project)
	if err != nil || src.root.Dir != home.Dir || src.account.Name != "api" || src.account.Type != store.TypeAPIKey || src.isolation != "" {
		t.Errorf("api_key account: %+v, %v", src, err)
	}
	src, _, err = resolveExportSource(f.cfgDir, env, "work", f.project)
	if err != nil || src.root.Owner != "work" || src.account.Name != "work" || src.isolation != store.IsolationFull {
		t.Errorf("full-isolation account: %+v, %v", src, err)
	}
	if _, _, err := resolveExportSource(f.cfgDir, env, "ghost", f.project); err == nil || !strings.Contains(err.Error(), `unknown account "ghost"`) {
		t.Errorf("unknown account: %v", err)
	}
	t.Setenv("BFFS_ACCOUNT", "work")
	src, warning, err = resolveExportSource(f.cfgDir, env, "", f.project)
	if err != nil || warning != "" || src.root.Owner != "work" || src.account.Name != "work" {
		t.Errorf("pinned: %+v, %q, %v", src, warning, err)
	}
	t.Setenv("BFFS_ACCOUNT", "ghost")
	src, warning, err = resolveExportSource(f.cfgDir, env, "", f.project)
	if err != nil || src.root.Dir != home.Dir || !strings.Contains(warning, "exporting from the home root") {
		t.Errorf("bad pin: %+v, %q, %v", src, warning, err)
	}
}

func TestPendingRehomeFilter(t *testing.T) {
	home := fakeHome(t)
	ss := testSessions(home)
	got := pendingRehome(ss, 0)
	if len(got) != 1 || got[0].ID != testSID2 {
		t.Errorf("pending = %v", ids(got))
	}
	ss = append(ss, transcripts.Session{ID: testSID3, Import: ss[1].Import})
	if got := pendingRehome(ss, 1); len(got) != 1 || got[0].ID != testSID2 {
		t.Errorf("limited pending = %v", ids(got))
	}
	if got := pendingRehome(nil, 0); len(got) != 0 {
		t.Errorf("empty = %v", got)
	}
}

func TestRenderImportsTable(t *testing.T) {
	home := fakeHome(t)
	cfgDir := filepath.Join(home, "bffs")
	var sb strings.Builder
	if err := renderImportsTable(&sb, cfgDir, nil, catalogNow); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "no imports recorded (bffs import --from <file.bffs>)") {
		t.Errorf("empty table = %q", sb.String())
	}
	recs := []imports.Record{
		{BundleID: "aaaaaaaa-0000-4000-8000-000000000001", Kind: imports.KindImport, ImportedAt: catalogNow.Add(-30 * 24 * time.Hour), Account: "work",
			Source:   imports.Source{Hostname: "mac-old", User: "jonas"},
			Sessions: []imports.Session{{ID: testSID3, Status: imports.StatusPlaced}}},
		{BundleID: testBundleID, Kind: imports.KindImport, ImportedAt: catalogNow.Add(-8 * 24 * time.Hour),
			Source:   imports.Source{Hostname: osc52 + "mac-a", User: "jonas"},
			Sessions: []imports.Session{{ID: testSID1, Status: imports.StatusPlaced}, {ID: testSID2, Status: imports.StatusPending}, {ID: testSID3, Status: imports.StatusSkipped}},
			Memories: []imports.Memory{{Dir: "/m", Status: imports.StatusPending}, {Dir: "/n", Status: imports.StatusSkipped}}},
	}
	sb.Reset()
	if err := renderImportsTable(&sb, cfgDir, recs, catalogNow); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence reached the table:\n%q", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[1], "6f1e2c0a") || !strings.HasPrefix(lines[2], "aaaaaaaa") {
		t.Fatalf("newest first expected:\n%s", out)
	}
	newest := lines[1]
	for _, want := range []string{"mac-a (jonas)", "home", "2026-08-16 (8d ago)", "2 (+1 skipped)"} {
		if !strings.Contains(newest, want) {
			t.Errorf("row lacks %q: %q", want, newest)
		}
	}
	if got := lastFields(newest, 2); got != "1 1" {
		t.Errorf("PENDING MEMORY = %q", got)
	}
	if !strings.Contains(lines[2], "work") || !strings.Contains(lines[2], "30d ago") {
		t.Errorf("older row: %q", lines[2])
	}
	if !strings.Contains(lines[3], "2 imports under "+short(filepath.Join(cfgDir, "imports"))) {
		t.Errorf("footer: %q", lines[3])
	}

	sb.Reset()
	if err := writeImportsJSON(&sb, nil); err != nil || strings.TrimSpace(sb.String()) != "[]" {
		t.Errorf("empty JSON = %q, %v", sb.String(), err)
	}
	sb.Reset()
	if err := writeImportsJSON(&sb, recs); err != nil || !strings.Contains(sb.String(), `"bundle_id": "`+testBundleID+`"`) || !strings.Contains(sb.String(), `"status": "pending"`) {
		t.Errorf("JSON = %q, %v", sb.String(), err)
	}
}

// lastFields joins the last n whitespace-separated fields of line.
func lastFields(line string, n int) string {
	f := strings.Fields(line)
	if len(f) < n {
		return strings.Join(f, " ")
	}
	return strings.Join(f[len(f)-n:], " ")
}

// newImportCmd is newSplitCmd with the prompter on stdout, the way
// `bffs import` wires it (its stdout never carries bundle bytes).
func newImportCmd(stdin string) (*cobra.Command, *prompter, *bytes.Buffer, *bytes.Buffer) {
	c, _, out, errOut := newSplitCmd(stdin)
	return c, newPrompter(c.InOrStdin(), out), out, errOut
}

// A short read while writing — a live transcript changed under the
// export — names --no-live; any other failure, and a short read with no
// live session in the bundle, is wrapped plainly.
func TestExportWriteError(t *testing.T) {
	short := errors.New(`short read on "projects/x/y.jsonl": manifest says 10 bytes, source had 4`)
	live := &bundle.Manifest{Entries: []bundle.Entry{{Kind: bundle.EntrySession, LivePossiblyTruncated: true}}}
	still := &bundle.Manifest{Entries: []bundle.Entry{{Kind: bundle.EntrySession}}}
	if got := exportWriteError(short, live).Error(); !strings.Contains(got, "retry with --no-live") || !errors.Is(exportWriteError(short, live), short) {
		t.Errorf("live short read: %q", got)
	}
	if got := exportWriteError(short, still).Error(); strings.Contains(got, "--no-live") || !strings.HasPrefix(got, "export: short read") {
		t.Errorf("short read without a live session: %q", got)
	}
	other := errors.New("disk full")
	if got := exportWriteError(other, live).Error(); got != "export: disk full" {
		t.Errorf("other error: %q", got)
	}
	if exportWriteError(nil, live) != nil {
		t.Error("nil error wrapped")
	}
}

// ---- --serve (M5) ----

// syncBuffer is a bytes.Buffer safe for the serve goroutine's printers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func loopbackLinks() []transfer.LinkAddr {
	return []transfer.LinkAddr{{Addr: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8"), Iface: "lo"}}
}

// loopbackTransfer points both sides at loopback: the serve binds
// 127.0.0.1 on the requested port and reports the bound address, both
// address sets are loopback, and the code is the one given. Everything is
// restored at cleanup.
func loopbackTransfer(t *testing.T, code transfer.Code) <-chan netip.AddrPort {
	t.Helper()
	addrCh := make(chan netip.AddrPort, 1)
	oldListen, oldLocal, oldGen, oldFetchLocal := serveListen, serveLocal, serveGenerate, fetchLocal
	serveListen = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err == nil {
			select {
			case addrCh <- netip.MustParseAddrPort(ln.Addr().String()):
			default:
			}
		}
		return ln, err
	}
	lo := func(transfer.LANOptions) ([]transfer.LinkAddr, error) { return loopbackLinks(), nil }
	serveLocal, fetchLocal = lo, lo
	serveGenerate = func() (transfer.Code, error) { return code, nil }
	t.Cleanup(func() {
		serveListen, serveLocal, serveGenerate, fetchLocal = oldListen, oldLocal, oldGen, oldFetchLocal
	})
	return addrCh
}

func mustParseCode(t *testing.T, s string) transfer.Code {
	t.Helper()
	c, err := transfer.ParseCode(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The A-side banner: the port is omitted when it is the default, the key
// fingerprint sits beside the code, --show-ipv6 lists link-local
// addresses without a zone and says how to add one.
func TestRenderServeBanner(t *testing.T) {
	local := []transfer.LinkAddr{
		{Addr: netip.MustParseAddr("192.168.1.20"), Prefix: netip.MustParsePrefix("192.168.1.0/24"), Iface: "en0"},
		{Addr: netip.MustParseAddr("fe80::1c2d:3e4f").WithZone("en0"), Prefix: netip.MustParsePrefix("fe80::/64"), Iface: "en0"},
	}
	var sb strings.Builder
	renderServeBanner(&sb, serveBanner{Code: "7K3Q-M9XD", Key: "3f9a1c2e", Addr: local[0].Addr, Port: 7345, Local: local, TTL: 10 * time.Minute, Attempts: 3})
	for _, want := range []string{
		"\nOn the other machine, run:    bffs import --from 192.168.1.20\n",
		"\nPairing code:   7K3Q-M9XD          (this machine's key: 3f9a1c2e — the other side shows it as \"peer key\")\n",
		"\nWaiting for the other machine…  code valid for 10:00, 3 attempts, one transfer.   (Ctrl-C cancels; --show-ipv6 lists link-local addresses)\n",
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("banner missing %q:\n%s", want, sb.String())
		}
	}
	if strings.Contains(sb.String(), "fe80") || strings.Contains(sb.String(), ":7345") {
		t.Errorf("default port or IPv6 shown without --show-ipv6:\n%s", sb.String())
	}

	sb.Reset()
	renderServeBanner(&sb, serveBanner{Code: "7K3Q-M9XD", Key: "3f9a1c2e", Addr: local[0].Addr, Port: 51234, Local: local, TTL: 90 * time.Second, Attempts: 3, ShowIPv6: true})
	for _, want := range []string{
		"bffs import --from 192.168.1.20:51234\n",
		"bffs import --from [fe80::1c2d:3e4f]:51234   (append %<your interface> to the address, e.g. %en0)\n",
		"code valid for 1:30, 3 attempts",
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("banner missing %q:\n%s", want, sb.String())
		}
	}
	if strings.Contains(sb.String(), "%en0]") {
		t.Errorf("link-local address printed with a zone:\n%s", sb.String())
	}

	// Helpers the banner and the B side share.
	if got := fromTarget(netip.MustParseAddr("fe80::1").WithZone("en0"), 7345); got != "[fe80::1%en0]" {
		t.Errorf("fromTarget with zone = %q", got)
	}
	if got := keyFromText("listening on 192.168.1.20:7345 (en0), key 3f9a1c2e", "key "); got != "3f9a1c2e" {
		t.Errorf("keyFromText = %q", got)
	}
	if got := keyFromText("connected to x (TLS 1.3, peer key 3f9a1c2e)", "peer key "); got != "3f9a1c2e" {
		t.Errorf("keyFromText peer = %q", got)
	}
	if got := keyFromText("no key here", "key "); got != "unknown" {
		t.Errorf("keyFromText without = %q", got)
	}
}

// The A-side printer: the banner once on the first listen event, one
// timestamped line per event with the plan's wording, peer text
// sanitised, the code nowhere but the banner.
func TestServePrinterLines(t *testing.T) {
	code := mustParseCode(t, "7K3Q-M9XD")
	var sb strings.Builder
	p := &servePrinter{w: &sb, code: code, local: loopbackLinks(), ttl: 10 * time.Minute, manifestSize: 12_345, bar: newProgressBar(&sb, "sending", false)}
	at := catalogNow
	ev := func(kind, peer, text string) transfer.Event {
		return transfer.Event{Time: at, Kind: kind, Peer: peer, Text: text}
	}
	p.event(ev("listen", "192.168.1.20:7345", "listening on 192.168.1.20:7345 (en0), key 3f9a1c2e"))
	p.event(ev("listen", "[fe80::1%en0]:7345", "listening on [fe80::1%en0]:7345 (en0), key 3f9a1c2e"))
	p.event(ev("connect", "192.168.1.31:50001", "192.168.1.31 connected (TLS 1.3)"))
	p.event(ev("bad-code", "192.168.1.31:50001", "192.168.1.31 tried a wrong code (2 attempts left)"))
	p.event(ev("code-ok", "192.168.1.31:50002", "192.168.1.31 gave the right code"))
	p.event(ev("manifest-sent", "192.168.1.31:50002", "manifest sent to 192.168.1.31 (12345 bytes)"))
	p.event(ev("reject", "192.168.1.31:50002", "mac-b"+osc52+" declined"))
	p.event(ev("accept", "192.168.1.31:50003", "mac-b accepted"))
	p.event(ev("sending", "192.168.1.31:50003", "sending to mac-b"))
	p.event(ev("progress", "192.168.1.31:50003", "sent 16 MiB to mac-b"))
	p.event(ev("done", "192.168.1.31:50003", "mac-b received 137 entries (148213311 bytes)"))
	p.event(ev("expired", "", "code expired after 10:00; no pairing happened"))
	out := sb.String()
	for _, want := range []string{
		"On the other machine, run:    bffs import --from 192.168.1.20\n",
		"  12:00:00  192.168.1.31 connected — waiting for its code\n",
		"  12:00:00  192.168.1.31 tried a wrong code (2 attempts left)\n",
		"  12:00:00  192.168.1.31 code accepted; manifest sent (12 KB) — waiting for the other side to review\n",
		"  12:00:00  mac-b declined\n",
		"  12:00:00  manifest accepted by mac-b\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printer missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "Pairing code:") != 1 || strings.Count(out, "7K3Q-M9XD") != 1 {
		t.Errorf("banner or code not printed exactly once:\n%s", out)
	}
	// An expiry ends the serve with ErrExpired carrying the same text, so
	// the event line is not printed on top of the error.
	for _, absent := range []string{"fe80", "gave the right code", "sending to", "sent 16 MiB", "received 137", "code expired", "\x1b"} {
		if strings.Contains(out, absent) {
			t.Errorf("printer must not print %q:\n%s", absent, out)
		}
	}
	if p.acceptedAt.IsZero() {
		t.Error("accept did not start the transfer clock")
	}
	sb.Reset()
	if _, err := p.uiWriter().Write([]byte("no connection yet — allow the firewall\n")); err != nil {
		t.Fatal(err)
	}
	if sb.String() != "  no connection yet — allow the firewall\n" {
		t.Errorf("ui hint = %q", sb.String())
	}
}

// Without a terminal the progress bar prints one final line only; the
// verified variant names the files instead of a rate.
func TestProgressBarNonTTY(t *testing.T) {
	var sb strings.Builder
	clock := catalogNow
	b := newProgressBar(&sb, "sending", false)
	b.now = func() time.Time { return clock }
	b.update(bundle.Progress{Phase: "hash", Bytes: 1, TotalBytes: 2})
	if sb.Len() != 0 || !b.started.IsZero() {
		t.Errorf("the hashing pre-pass moved the bar: %q", sb.String())
	}
	b.update(bundle.Progress{Phase: "build", Files: 1, TotalFiles: 3, Bytes: 40_000_000, TotalBytes: 148_200_000})
	clock = clock.Add(2 * time.Second)
	b.update(bundle.Progress{Phase: "build", Files: 2, TotalFiles: 3, Bytes: 100_000_000, TotalBytes: 148_200_000})
	if sb.Len() != 0 {
		t.Errorf("partial bar printed without a terminal: %q", sb.String())
	}
	clock = clock.Add(time.Second)
	b.update(bundle.Progress{Phase: "build", Files: 3, TotalFiles: 3, Bytes: 148_200_000, TotalBytes: 148_200_000})
	if want := "  sending ████████████████████ 100%   148.2 MB   49.4 MB/s\n"; sb.String() != want {
		t.Errorf("final bar = %q, want %q", sb.String(), want)
	}
	b.end()
	b.interrupt()
	if strings.Count(sb.String(), "\n") != 1 {
		t.Errorf("end/interrupt after the final line printed again: %q", sb.String())
	}

	sb.Reset()
	v := newProgressBar(&sb, "receiving", false)
	v.now = func() time.Time { return clock }
	v.verified = true
	v.update(bundle.Progress{Phase: "unpack", Files: 137, TotalFiles: 137, Bytes: 148_213_311, TotalBytes: 148_213_311})
	if want := "  receiving ████████████████████ 100%   148.2 MB   137 files verified (sha256)\n"; sb.String() != want {
		t.Errorf("verified bar = %q, want %q", sb.String(), want)
	}
	if got := barBlocks(42); got != "████████░░░░░░░░░░░░" {
		t.Errorf("barBlocks(42) = %q", got)
	}
}

// On a terminal the bar is redrawn in place with "\r" at most every
// 200 ms, a shorter redraw is padded over the previous one, and an event
// line interrupts a partial bar with a newline.
func TestProgressBarRedraw(t *testing.T) {
	var sb strings.Builder
	clock := catalogNow
	b := newProgressBar(&sb, "sending", true)
	b.now = func() time.Time { return clock }
	b.update(bundle.Progress{Phase: "build", Bytes: 10_000_000, TotalBytes: 100_000_000})
	if got := sb.String(); got != "  sending ██░░░░░░░░░░░░░░░░░░  10%   10.0 MB" {
		t.Errorf("first draw = %q", got)
	}
	clock = clock.Add(100 * time.Millisecond)
	b.update(bundle.Progress{Phase: "build", Bytes: 20_000_000, TotalBytes: 100_000_000})
	if strings.Contains(sb.String(), "\r") {
		t.Errorf("redrawn within 200 ms: %q", sb.String())
	}
	clock = clock.Add(900 * time.Millisecond)
	b.update(bundle.Progress{Phase: "build", Bytes: 20_000_000, TotalBytes: 100_000_000})
	second := "\r  sending ████░░░░░░░░░░░░░░░░  20%   20.0 MB   20.0 MB/s"
	if !strings.HasSuffix(sb.String(), second) {
		t.Errorf("second draw: %q", sb.String())
	}
	// The rate fell (20.0 MB/s → 0.3 MB/s): the shorter redraw is padded
	// to the width of the widest line drawn so far.
	clock = clock.Add(100 * time.Second)
	b.update(bundle.Progress{Phase: "build", Bytes: 30_000_000, TotalBytes: 100_000_000})
	third := "\r  sending ██████░░░░░░░░░░░░░░  30%   30.0 MB   0.3 MB/s "
	if !strings.HasSuffix(sb.String(), third) {
		t.Errorf("third draw: %q", sb.String())
	}
	if got, want := len([]rune(third)), len([]rune(second)); got != want {
		t.Errorf("third draw width %d, second %d", got, want)
	}
	b.interrupt()
	if !strings.HasSuffix(sb.String(), third+"\n") || b.onLine {
		t.Errorf("interrupt did not end the line: %q", sb.String())
	}
	b.update(bundle.Progress{Phase: "build", Bytes: 100_000_000, TotalBytes: 100_000_000})
	if !strings.HasSuffix(sb.String(), "\n  sending ████████████████████ 100%   100.0 MB   1.0 MB/s\n") {
		t.Errorf("final line after an interrupt: %q", sb.String())
	}
}

// --serve refuses a TTL above the cap and a machine without any
// local-network address before building the selection, let alone showing
// a code.
func TestServeValidation(t *testing.T) {
	a := newExportFixture(t)
	code := mustParseCode(t, "7K3Q-M9XD")
	loopbackTransfer(t, code)
	req := a.request("")
	req.Out, req.Serve, req.AllowLoopback = "", true, true
	req.Host, req.User = "mac-a", "jonas"

	req.TTL = time.Hour
	c, pr, _, errOut := newSplitCmd("")
	err := runExport(c, a.cfgDir, pr, req, false)
	if err == nil || !strings.Contains(err.Error(), "invalid --ttl 1h0m0s") {
		t.Errorf("ttl: err = %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("output before the flag check:\n%s", errOut.String())
	}

	req.TTL = time.Minute
	req.Port = 70000
	c, pr, _, errOut = newSplitCmd("")
	err = runExport(c, a.cfgDir, pr, req, false)
	if err == nil || !strings.Contains(err.Error(), "invalid --port 70000") || errOut.Len() != 0 {
		t.Errorf("port: err = %v, stderr %q", err, errOut.String())
	}

	req.Port = 0
	old := serveLocal
	serveLocal = func(transfer.LANOptions) ([]transfer.LinkAddr, error) { return nil, nil }
	t.Cleanup(func() { serveLocal = old })
	c, pr, _, errOut = newSplitCmd("")
	err = runExport(c, a.cfgDir, pr, req, false)
	if err == nil || err.Error() != "no local-network address found (only loopback/VPN interfaces are up); connect to Wi-Fi/Ethernet or use --out file.bffs" || exitCode(err) != 1 {
		t.Errorf("no address: err = %v (exit %d)", err, exitCode(err))
	}
	if errOut.Len() != 0 {
		t.Errorf("selection work before the network check:\n%s", errOut.String())
	}
}

// End to end over loopback: machine A serves its project, machine B
// first tries a wrong code (exit 2, A keeps serving), then declines
// without a terminal (exit 1, nothing written, A keeps serving), then
// pairs with the code from $BFFS_TRANSFER_CODE and lands the session. The
// code appears once on A's screen and never on B's.
func TestExportServeImportFromHost(t *testing.T) {
	a := newExportFixture(t)
	code := mustParseCode(t, "7K3Q-M9XD")
	addrCh := loopbackTransfer(t, code)

	aOut, aErr := &syncBuffer{}, &syncBuffer{}
	ac := &cobra.Command{}
	ac.SetOut(aOut)
	ac.SetErr(aErr)
	ac.SetIn(strings.NewReader(""))
	areq := a.request("")
	areq.Out, areq.Serve, areq.Port, areq.TTL, areq.AllowLoopback = "", true, 0, time.Minute, true
	areq.Host, areq.User = "mac-a", "jonas"
	served := make(chan error, 1)
	go func() { served <- runExport(ac, a.cfgDir, newPrompter(ac.InOrStdin(), aErr), areq, false) }()
	var addr netip.AddrPort
	select {
	case addr = <-addrCh:
	case err := <-served:
		t.Fatalf("serve ended before binding: %v\n%s", err, aErr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve never bound")
	}

	b := newImportMachine(t)
	breq := b.request(addr.String())
	breq.AllowLoopback = true
	breq.Host, breq.User = "mac-b", "jonas"
	var bOutputs []string
	noWrite := func(step string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(b.claudeDir, "projects")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: destination written: %v", step, err)
		}
		if recs, _ := imports.Load(b.cfgDir); len(recs) != 0 {
			t.Errorf("%s: record written: %v", step, recs)
		}
	}

	// 1. A wrong code: exit 2, the connection line named the peer key.
	t.Setenv(envTransferCode, "ZZZZ-ZZZZ")
	c, pr, out, errOut := newImportCmd("")
	err := runImport(c, b.cfgDir, pr, breq, false)
	bOutputs = append(bOutputs, out.String(), errOut.String())
	if err == nil || exitCode(err) != 2 || !errors.Is(err, transfer.ErrBadCode) || !strings.Contains(err.Error(), "rejected the code (2 attempts left there). Run bffs import again.") {
		t.Errorf("wrong code: err = %v (exit %d)", err, exitCode(err))
	}
	if !strings.Contains(errOut.String(), "connected to "+addr.String()+" (TLS 1.3, peer key ") || !strings.Contains(errOut.String(), "(from $BFFS_TRANSFER_CODE)") {
		t.Errorf("wrong code stderr:\n%s", errOut.String())
	}
	if _, ok := os.LookupEnv(envTransferCode); ok {
		t.Error("the code variable survived the read")
	}
	noWrite("wrong code")

	// 2. The right code but no terminal and no -y: the summary is shown,
	// the import is declined before any write, A keeps serving.
	t.Setenv(envTransferCode, code.Display())
	decline := breq
	decline.Yes = false
	c, pr, out, errOut = newImportCmd("")
	err = runImport(c, b.cfgDir, pr, decline, false)
	bOutputs = append(bOutputs, out.String(), errOut.String())
	if err == nil || exitCode(err) != 1 || !strings.Contains(err.Error(), "refusing to import: stdin is not a terminal; pass -y") {
		t.Errorf("non-tty decline: err = %v (exit %d)", err, exitCode(err))
	}
	for _, want := range []string{
		"code accepted — the other machine proved it knows the code too",
		"Bundle ", " from ", "bffs 0.4.0-test",
		"  project " + a.project + "        exists here ✓ (same directory — no rehome needed)",
		"Target: home (" + short(b.claudeDir) + ", unmanaged claude)   limit 2.0 GB   retention: 30 days (default)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("decline stderr missing %q:\n%s", want, errOut.String())
		}
	}
	noWrite("decline")

	// 3. The transfer.
	t.Setenv(envTransferCode, code.Display())
	c, pr, out, errOut = newImportCmd("")
	if err := runImport(c, b.cfgDir, pr, breq, false); err != nil {
		t.Fatalf("import: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	bOutputs = append(bOutputs, out.String(), errOut.String())
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty on the host path: %q", out.String())
	}
	recs, err := imports.Load(b.cfgDir)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, %v", recs, err)
	}
	for _, want := range []string{
		"  receiving ████████████████████ 100%   ",
		" files verified (sha256)\n",
		"  sessions   1 committed to projects/" + a.slug + "/; 0 skipped",
		"Done in ", ". Check it:",
		"    cd " + shellWord(a.project) + " && claude --resume " + testSID1,
		"Import record: " + short(imports.Path(b.cfgDir, recs[0].BundleID)) + "  (bffs sessions imports)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("receipt missing %q:\n%s", want, errOut.String())
		}
	}
	got, err := os.ReadFile(filepath.Join(b.claudeDir, "projects", a.slug, testSID1+".jsonl"))
	if err != nil {
		t.Fatalf("transcript not landed: %v", err)
	}
	orig, _ := os.ReadFile(filepath.Join(a.claudeDir, "projects", a.slug, testSID1+".jsonl"))
	if !bytes.Equal(got, orig) {
		t.Errorf("landed transcript differs from the source")
	}
	if entries, _ := os.ReadDir(filepath.Join(b.cfgDir, "staging")); len(entries) != 0 {
		t.Errorf("staging not cleaned: %v", entries)
	}

	// A: delivered once, exit 0, the banner with the code and key, the
	// event lines for every attempt.
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v\n%s", err, aErr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not end after the delivery")
	}
	aText := aErr.String()
	if aOut.String() != "" {
		t.Errorf("serve wrote to stdout: %q", aOut.String())
	}
	for _, want := range []string{
		"Exporting from ",
		"On the other machine, run:    bffs import --from " + addr.String() + "\n",
		"Pairing code:   7K3Q-M9XD          (this machine's key: ",
		"Waiting for the other machine…  code valid for 1:00, 3 attempts, one transfer.",
		"127.0.0.1 connected — waiting for its code",
		"127.0.0.1 tried a wrong code (2 attempts left)",
		"127.0.0.1 code accepted; manifest sent (",
		"mac-b declined",
		"manifest accepted by mac-b",
		"  sending ████████████████████ 100%   ",
		"  delivered: ", " verified by mac-b in ",
		"Done.\n",
	} {
		if !strings.Contains(aText, want) {
			t.Errorf("serve stderr missing %q:\n%s", want, aText)
		}
	}
	// The code is on A's screen exactly once, in the banner, and never
	// on B's (either form).
	if strings.Count(aText, "7K3Q-M9XD") != 1 || strings.Contains(aText, "7K3QM9XD") {
		t.Errorf("code printed elsewhere than the banner:\n%s", aText)
	}
	for i, s := range bOutputs {
		if strings.Contains(s, "7K3Q-M9XD") || strings.Contains(s, "7K3QM9XD") || strings.Contains(strings.ToLower(s), "7k3q") {
			t.Errorf("B output %d shows the code:\n%s", i, s)
		}
	}
	// The key fingerprint matches on both sides.
	keyA := keyFromText(lineContaining(aText, "this machine's key: "), "this machine's key: ")
	keyB := keyFromText(lineContaining(bOutputs[1], "peer key "), "peer key ")
	if len(keyA) != 8 || keyA != keyB {
		t.Errorf("key fingerprints differ: A %q, B %q", keyA, keyB)
	}
}
