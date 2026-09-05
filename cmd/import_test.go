package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/transfer"
)

func TestParseFrom(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "bundle.bin")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "dir.bffs")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in      string
		kind    fromKind
		target  string
		wantErr string
	}{
		{"-", fromStdin, "", ""},
		{" - ", fromStdin, "", ""},
		{existing, fromFile, mustNormalize(t, existing), ""},
		{filepath.Join(dir, "missing.bffs"), 0, "", `bundle file "` + filepath.Join(dir, "missing.bffs") + `" not found`},
		{"missing.BFFS", 0, "", `bundle file "missing.BFFS" not found`},
		{subdir, 0, "", `bundle "` + subdir + `" is a directory`},
		{"", 0, "", "--from is required"},
		{"192.168.1.20", fromHost, "192.168.1.20", ""},
		{"192.168.1.20:7345", fromHost, "192.168.1.20:7345", ""},
		{"[fe80::1%en0]:7345", fromHost, "[fe80::1%en0]:7345", ""},
		{"mac-b", fromHost, "mac-b", ""},
		{"mac-b.local", fromHost, "mac-b.local", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			kind, target, err := parseFrom(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || kind != tc.kind || target != tc.target {
				t.Errorf("parseFrom = %v, %q, %v; want %v, %q", kind, target, err, tc.kind, tc.target)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{"2G", 2 << 30, false}, {"2g", 2 << 30, false}, {"2GB", 2 << 30, false}, {"2GiB", 2 << 30, false},
		{"500M", 500 << 20, false}, {"1.5G", 3 << 29, false}, {"10K", 10 << 10, false}, {"1T", 1 << 40, false},
		{"4096", 4096, false}, {" 2G ", 2 << 30, false},
		{"", 0, true}, {"abc", 0, true}, {"-1G", 0, true}, {"0", 0, true}, {"G", 0, true}, {"1e30G", 0, true},
	}
	for _, tc := range cases {
		got, err := parseSize(tc.in)
		if tc.bad {
			if err == nil || !strings.Contains(err.Error(), "invalid --max-size") {
				t.Errorf("parseSize(%q) = %d, %v; want error", tc.in, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	if got := formatLimit(2 << 30); got != "2.0 GB" {
		t.Errorf("formatLimit(2 GiB) = %q", got)
	}
	if got := formatLimit(500 << 20); got != "500 MB" {
		t.Errorf("formatLimit(500 MiB) = %q", got)
	}
}

func TestRetentionLabel(t *testing.T) {
	home := fakeHome(t)
	cfg := filepath.Join(home, ".claude")
	cases := []struct {
		days   int
		source string
		want   string
	}{
		{30, transcripts.CleanupSourceDefault, "30 days (default)"},
		{14, transcripts.SettingsFile, "14 days (" + filepath.Join("~", ".claude", "settings.json") + ")"},
		{7, transcripts.LocalSettingsFile, "7 days (" + filepath.Join("~", ".claude", "settings.local.json") + ")"},
		{0, transcripts.SettingsFile, "never (" + filepath.Join("~", ".claude", "settings.json") + ")"},
		{0, transcripts.CleanupSourceInvalid, "unknown (settings unreadable; transcript mtimes preserved)"},
	}
	for _, tc := range cases {
		if got := retentionLabel(cfg, tc.days, tc.source); got != tc.want {
			t.Errorf("retentionLabel(%d, %q) = %q, want %q", tc.days, tc.source, got, tc.want)
		}
	}
}

func TestRenderImportSummary(t *testing.T) {
	home := fakeHome(t)
	m := testManifest(home)
	// The manifest's paths and title are foreign strings: hostile ones must
	// not reach the terminal.
	m.Entries[0].Cwd = osc52 + m.Entries[0].Cwd
	m.Entries[2].Cwd = m.Entries[0].Cwd
	dest := importDest{root: sharedRoot(home), account: "work", label: importTargetLabel("work", sharedRoot(home), "resolved for "+short(filepath.Join(home, "x")))}
	sum := newImportSummary(m, dest, importRequest{})
	sum.Limit = 2 << 30
	sum.Retention = "30 days (default)"
	var sb strings.Builder
	renderImportSummary(&sb, sum)
	out := sb.String()
	project := filepath.Join(home, "build", "projects", "bffs")
	for _, want := range []string{
		`Bundle 6f1e2c0a from mac-a (jonas, darwin/arm64, bffs 0.3.0, claude 2.1.259, account "aviate", partial):`,
		"  project " + project + "        exists here ✗ → imported as-is; rehome later with bffs rehome or /bffs-rehome in claude",
		"    2 sessions  207.4 MB   memory 2 files   (trust: accepted on mac-a — informational)",
		"    note: memory files in this bundle will be loaded into every future claude session for projects/-Users-jonas-build-projects-bffs/memory (as-is)",
		"          (pinned files arrive unpinned; --trust-memory keeps them pinned)",
		`Target: account "work" (resolved for ` + filepath.Join("~", "x") + " → shared pool " + filepath.Join("~", ".claude") + ")   limit 2.0 GB   retention: 30 days (default)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "52;c;") {
		t.Errorf("escape sequence reached the summary:\n%q", out)
	}

	// The directory exists here: identity placement, memory goes to it.
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	m = testManifest(home)
	sum = newImportSummary(m, importDest{label: importTargetLabel("", sharedRoot(home), "")}, importRequest{TrustMemory: true})
	sb.Reset()
	renderImportSummary(&sb, sum)
	out = sb.String()
	for _, want := range []string{
		"exists here ✓ (same directory — no rehome needed)",
		"loaded into every future claude session for " + project + "\n",
		"(--trust-memory: pinned files stay pinned)",
		"Target: home (" + filepath.Join("~", ".claude") + ", unmanaged claude)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("identity summary missing %q:\n%s", want, out)
		}
	}

	// A relative cwd in the manifest is never "the same directory", even
	// when it names something that exists relative to this process.
	t.Chdir(filepath.Dir(project))
	rel := testManifest(home)
	for i := range rel.Entries {
		rel.Entries[i].Cwd = filepath.Base(project)
	}
	sum = newImportSummary(rel, importDest{label: "x"}, importRequest{})
	sb.Reset()
	renderImportSummary(&sb, sum)
	if !strings.Contains(sb.String(), "exists here ✗") || strings.Contains(sb.String(), "exists here ✓") {
		t.Errorf("relative cwd treated as identity:\n%s", sb.String())
	}

	// --as-is ignores the directory.
	sum = newImportSummary(m, importDest{label: "x"}, importRequest{AsIs: true})
	sb.Reset()
	renderImportSummary(&sb, sum)
	if !strings.Contains(sb.String(), "imported as-is under projects/-Users-jonas-build-projects-bffs/ (--as-is); rehome later") {
		t.Errorf("as-is summary:\n%s", sb.String())
	}

	// No trust info, no memory, no cwd: nothing invented.
	m = &bundle.Manifest{BundleID: testBundleID, Source: bundle.Source{Hostname: "h"}, Entries: []bundle.Entry{{Kind: bundle.EntrySession, Slug: "s", SessionID: testSID1}}}
	sum = newImportSummary(m, importDest{label: "x"}, importRequest{})
	sb.Reset()
	renderImportSummary(&sb, sum)
	out = sb.String()
	if !strings.HasPrefix(out, "Bundle 6f1e2c0a from h:\n  project projects/s (no directory recorded)") || strings.Contains(out, "trust:") || strings.Contains(out, "note:") {
		t.Errorf("minimal summary:\n%s", out)
	}
}

func TestImportTargetLabel(t *testing.T) {
	home := fakeHome(t)
	cases := []struct {
		account string
		root    transcripts.Root
		how     string
		want    string
	}{
		{"", sharedRoot(home), "", "home (" + filepath.Join("~", ".claude") + ", unmanaged claude)"},
		{"work", sharedRoot(home), "--account", `account "work" (--account → shared pool ` + filepath.Join("~", ".claude") + ")"},
		{"work", ownedRoot(home, "work"), "active account", `account "work" (active account → own root ` + filepath.Join("~", "bffs", "sessions", "work") + ")"},
		{"api", transcripts.Root{ConfigDir: filepath.Join(home, ".claude")}, "", `account "api" (` + filepath.Join("~", ".claude") + ")"},
	}
	for _, tc := range cases {
		if got := importTargetLabel(tc.account, tc.root, tc.how); got != tc.want {
			t.Errorf("importTargetLabel(%q, %q) = %q, want %q", tc.account, tc.how, got, tc.want)
		}
	}
}

func TestResolveImportDest(t *testing.T) {
	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth},
		"api":  {Type: store.TypeAPIKey, Secret: "sk-ant-test-1234"},
	}})
	env := f.env()
	home, _ := env.homeRoot()

	d, warning, err := resolveImportDest(f.cfgDir, env, "", f.project)
	if err != nil || warning != "" || d.root.Dir != home.Dir || d.account != "" || !strings.HasPrefix(d.label, "home (") {
		t.Errorf("no pin: %+v, %q, %v", d, warning, err)
	}
	d, _, err = resolveImportDest(f.cfgDir, env, "api", f.project)
	if err != nil || d.root.Dir != home.Dir || d.account != "api" || !strings.HasPrefix(d.label, `account "api" (--account → shared pool `) {
		t.Errorf("api_key account: %+v, %v", d, err)
	}
	d, _, err = resolveImportDest(f.cfgDir, env, transcripts.HomeName, f.project)
	if err != nil || d.root.Dir != home.Dir || d.account != "" {
		t.Errorf("home: %+v, %v", d, err)
	}
	if _, _, err := resolveImportDest(f.cfgDir, env, "ghost", f.project); err == nil || !strings.Contains(err.Error(), `unknown account "ghost"`) {
		t.Errorf("unknown account: %v", err)
	}
	if err := store.SaveState(f.cfgDir, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	d, warning, err = resolveImportDest(f.cfgDir, env, "", f.project)
	if err != nil || warning != "" || d.account != "work" || d.root.Dir != home.Dir || !strings.Contains(d.label, "(active account → shared pool ") {
		t.Errorf("active account: %+v, %q, %v", d, warning, err)
	}
	t.Setenv("BFFS_ACCOUNT", "work")
	d, _, _ = resolveImportDest(f.cfgDir, env, "", f.project)
	if !strings.Contains(d.label, "(resolved for "+short(f.project)+" → shared pool ") {
		t.Errorf("pinned label = %q", d.label)
	}
	t.Setenv("BFFS_ACCOUNT", "ghost")
	d, warning, err = resolveImportDest(f.cfgDir, env, "", f.project)
	if err != nil || d.root.Dir != home.Dir || d.account != "" || !strings.Contains(warning, "importing into the home root") {
		t.Errorf("bad pin: %+v, %q, %v", d, warning, err)
	}
}

func TestRenderImportReceipt(t *testing.T) {
	home := fakeHome(t)
	rep := porter.Report{
		BundleID:     testBundleID,
		Imported:     []string{testSID1},
		Pending:      []string{testSID2},
		Skipped:      []string{testSID3},
		Held:         []string{"deadbeef-0000-4000-8000-000000000000"},
		Overwritten:  []string{testSID1},
		Reasons:      map[string]string{testSID3: osc52 + "exists (--on-conflict overwrite to replace)", "deadbeef-0000-4000-8000-000000000000": "open in a running claude; close it before overwriting"},
		MemoryDirs:   []string{filepath.Join(home, ".claude", "projects", "-x", "memory")},
		MtimeRaised:  []string{testSID2},
		SweepDate:    time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		SweptCount:   1,
		HistoryLines: 41,
		Verify:       []string{"cd /home/jonas/src/bffs && claude --resume " + testSID1, osc52 + "claude --resume " + testSID2},
	}
	rep.MemoryReview = map[string]int{rep.MemoryDirs[0]: 3}
	rec := &imports.Record{BundleID: testBundleID, Sessions: []imports.Session{
		{ID: testSID1, Slug: "-home-jonas-src-bffs", NewCwd: "/home/jonas/src/bffs", Status: imports.StatusPlaced},
		{ID: testSID2, Slug: "-Users-jonas-old", Status: imports.StatusPending},
		{ID: testSID3, Slug: "-home-jonas-src-bffs", Status: imports.StatusSkipped},
	}, Memories: []imports.Memory{{Dir: rep.MemoryDirs[0], OldCwd: osc52 + "/home/jonas/src/my bffs", Status: imports.StatusPlaced}}}
	m := &bundle.Manifest{BundleID: testBundleID, Entries: []bundle.Entry{
		{Kind: bundle.EntryMemory, Slug: "-x", Files: []bundle.File{{Path: "memory/-x/MEMORY.md"}, {Path: "memory/-x/a.md"}, {Path: "memory/-x/b.md"}}},
	}}
	r := importReceipt{Report: rep, Record: rec, Manifest: m, Elapsed: 2800 * time.Millisecond, RecordPath: filepath.Join(home, "bffs", "imports", testBundleID+".json")}
	var sb strings.Builder
	renderImportReceipt(&sb, r)
	out := sb.String()
	for _, want := range []string{
		"  sessions   2 committed to projects/-Users-jonas-old/ (as-is, pending rehome), projects/-home-jonas-src-bffs/; 2 skipped; 1 existing set aside as *.bffs-replaced-<time> (never deleted); 1 mtime raised to the retention floor",
		"             1 session will be swept by Claude on 2026-09-19 unless resumed",
		"             held deadbeef: open in a running claude; close it before overwriting",
		"             skipped c0ffee00: exists (--on-conflict overwrite to replace)",
		"  memory     3 files into " + filepath.Join("~", ".claude", "projects", "-x", "memory") + " (side files: *.imported-6f1e2c0a.md; MEMORY.md untouched)",
		"  history    41 prompt lines added",
		"Done in 2.8s. Check it:",
		"\n    cd /home/jonas/src/bffs && claude --resume " + testSID1,
		"\n    claude --resume " + testSID2,
		"\n    bffs sessions list --project /home/jonas/src/bffs\n",
		"\n    bffs memory scan-paths --project '/home/jonas/src/my bffs'   # 3 lines to review\n",
		"    pending: 1 session imported as-is (directory missing here) — bffs sessions list --pending-rehome; rehome later with bffs rehome or /bffs-rehome in claude",
		"note: the first claude launch there asks about folder trust (and external CLAUDE.md imports",
		"`bffs trust sync --to <acct>` carries the answer to other accounts.",
		"Import record: " + filepath.Join("~", "bffs", "imports", testBundleID+".json") + "  (bffs sessions imports)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("receipt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "52;c;") {
		t.Errorf("escape sequence reached the receipt:\n%q", out)
	}

	// Dry run: conditional wording, the manifest's slugs (there is no
	// record yet), no record line, no history line.
	m.Entries = append(m.Entries,
		bundle.Entry{Kind: bundle.EntrySession, Slug: "-a", SessionID: testSID1},
		bundle.Entry{Kind: bundle.EntrySession, Slug: "-b", SessionID: testSID2})
	dry := importReceipt{Report: rep, Manifest: m, DryRun: true}
	sb.Reset()
	renderImportReceipt(&sb, dry)
	out = sb.String()
	for _, want := range []string{
		"  sessions   2 would be committed to projects/-a/, projects/-b/ (as-is, pending rehome); 2 skipped",
		"  memory     files would be written to " + filepath.Join("~", ".claude", "projects", "-x", "memory"),
		"Dry run — nothing written. Afterwards, check it with:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run receipt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Import record:") || strings.Contains(out, "history") || strings.Contains(out, "Done in") {
		t.Errorf("dry-run receipt has real-run lines:\n%s", out)
	}
	// Neither a record nor a manifest: the count stands alone.
	dry.Manifest = nil
	sb.Reset()
	renderImportReceipt(&sb, dry)
	if !strings.Contains(sb.String(), "  sessions   2 would be committed; 2 skipped") {
		t.Errorf("no slugs known:\n%s", sb.String())
	}

	// Nothing landed.
	empty := importReceipt{Report: porter.Report{BundleID: testBundleID, Skipped: []string{testSID1}, Reasons: map[string]string{testSID1: "exists"}}, Elapsed: time.Second, RecordPath: "/r.json"}
	sb.Reset()
	renderImportReceipt(&sb, empty)
	out = sb.String()
	if !strings.Contains(out, "  sessions   none committed; 1 skipped") || !strings.Contains(out, "  memory     nothing written") || !strings.Contains(out, "  history    no new prompt lines") || strings.Contains(out, "note: the first claude") {
		t.Errorf("empty receipt:\n%s", out)
	}
}

// The confirmation flow of runImport with piped input: n aborts before
// any write, no terminal without -y refuses, y proceeds.
func TestImportConfirmFlow(t *testing.T) {
	a := newExportFixture(t)
	file := filepath.Join(t.TempDir(), "a.bffs")
	c, pr, _, errOut := newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err != nil {
		t.Fatalf("export: %v\n%s", err, errOut.String())
	}
	b := newImportMachine(t)
	req := b.request(file)
	req.Yes = false
	noWrite := func(step string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(b.claudeDir, "projects")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: destination written: %v", step, err)
		}
		if recs, _ := imports.Load(b.cfgDir); len(recs) != 0 {
			t.Errorf("%s: record written: %v", step, recs)
		}
	}

	c, pr, out, _ := newImportCmd("n\n")
	if err := runImport(c, b.cfgDir, pr, req, true); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if !strings.Contains(out.String(), "Import into "+short(b.claudeDir)+"? [y/N] ") || !strings.Contains(out.String(), "aborted") {
		t.Errorf("decline output:\n%s", out.String())
	}
	noWrite("decline")

	c, pr, out, _ = newImportCmd("")
	err := runImport(c, b.cfgDir, pr, req, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to import: stdin is not a terminal; pass -y") {
		t.Errorf("non-tty: err = %v", err)
	}
	if !strings.Contains(out.String(), "Bundle ") {
		t.Errorf("the summary must precede the refusal:\n%s", out.String())
	}
	noWrite("non-tty")

	c, pr, out, errOut = newImportCmd("y\n")
	if err := runImport(c, b.cfgDir, pr, req, true); err != nil {
		t.Fatalf("accept: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "  sessions   1 committed to projects/") || !strings.Contains(out.String(), "Import record:") {
		t.Errorf("accept output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(b.claudeDir, "projects", a.slug, testSID1+".jsonl")); err != nil {
		t.Errorf("accepted import did not land the transcript: %v", err)
	}
}

// --from - needs -y (stdin carries the bundle); a host off the local
// network is refused before any dial; policy values are validated before
// anything is read.
func TestImportRequestValidation(t *testing.T) {
	b := newImportMachine(t)
	c, pr, _, _ := newSplitCmd("")
	req := b.request("-")
	req.Yes = false
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil || !strings.Contains(err.Error(), "pass -y") {
		t.Errorf("stdin without -y: %v", err)
	}
	// A host that is not on a local network is refused before anything is
	// dialled or resolved (exit 1); the summary path never starts.
	lanTestSeams(t)
	c, pr, _, errOut := newImportCmd("")
	err := runImport(c, b.cfgDir, pr, b.request("203.0.113.5"), true)
	if err == nil || !strings.Contains(err.Error(), "refusing to pair with 203.0.113.5: not on a local network of this machine (--allow-routed for multi-VLAN offices). Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -") || exitCode(err) != 1 {
		t.Errorf("host: %v (exit %d)", err, exitCode(err))
	}
	if strings.Contains(errOut.String(), "connected to") {
		t.Errorf("a refused host was dialled:\n%s", errOut.String())
	}
	c, pr, _, _ = newSplitCmd("")
	req = b.request("-")
	req.OnConflict = "fork"
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil || !strings.Contains(err.Error(), `invalid --on-conflict "fork"`) {
		t.Errorf("on-conflict: %v", err)
	}
	req = b.request("-")
	req.Memory = "replace"
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil || !strings.Contains(err.Error(), `invalid --memory "replace"`) {
		t.Errorf("memory mode: %v", err)
	}
	req = b.request("-")
	req.Into, req.Map = "/x", []string{"a=b"}
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil || !strings.Contains(err.Error(), "--into and --map are mutually exclusive") {
		t.Errorf("into+map: %v", err)
	}
	req = b.request("-")
	req.CarryTrust, req.AsIs = true, true
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil || !strings.Contains(err.Error(), "--carry-trust cannot be combined with --as-is") {
		t.Errorf("carry-trust+as-is: %v", err)
	}
	req = b.request("-")
	req.Map = []string{"no-equals-sign"}
	if err := runImport(c, b.cfgDir, pr, req, true); err == nil {
		t.Errorf("bad rule accepted")
	}
	req = b.request("-")
	req.Stdin = strings.NewReader("not a bundle")
	err = runImport(c, b.cfgDir, pr, req, true)
	if err == nil || !strings.Contains(err.Error(), "bundle:") || exitCode(err) != 1 {
		t.Errorf("bad envelope: %v (exit %d)", err, exitCode(err))
	}
}

// --clean-staging lists leftovers, asks, and removes them; a decline
// keeps them and stops the run. Without it, an import warns about them.
func TestImportCleanStaging(t *testing.T) {
	a := newExportFixture(t)
	file := filepath.Join(t.TempDir(), "a.bffs")
	c, pr, _, errOut := newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err != nil {
		t.Fatalf("export: %v\n%s", err, errOut.String())
	}
	b := newImportMachine(t)
	leftover := porter.StagingDir(b.cfgDir, "aaaaaaaa-0000-4000-8000-000000000001")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}

	c, pr, out, _ := newImportCmd("n\n")
	req := b.request(file)
	req.CleanStaging, req.Yes = true, false
	if err := runImport(c, b.cfgDir, pr, req, true); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if !strings.Contains(out.String(), "leftover staging of interrupted imports (1 directory):") || !strings.Contains(out.String(), short(leftover)) || !strings.Contains(out.String(), "remove them? [y/N] ") || !strings.Contains(out.String(), "aborted") {
		t.Errorf("decline output:\n%s", out.String())
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("declined clean removed the directory: %v", err)
	}
	if strings.Contains(out.String(), "Bundle ") {
		t.Errorf("a declined clean must stop before the import:\n%s", out.String())
	}

	c, pr, out, errOut = newSplitCmd("")
	req.Yes = true
	if err := runImport(c, b.cfgDir, pr, req, false); err != nil {
		t.Fatalf("clean + import: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "removed "+short(leftover)) || !strings.Contains(out.String(), "Import record:") {
		t.Errorf("clean output:\n%s", out.String())
	}
	if _, err := os.Stat(leftover); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("leftover not removed: %v", err)
	}
	if strings.Contains(errOut.String(), "leftover staging directory") {
		t.Errorf("warning about a removed leftover:\n%s", errOut.String())
	}

	// A leftover that stays is a warning on the next import.
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	c, pr, _, errOut = newSplitCmd("")
	req.CleanStaging, req.Force = false, true
	if err := runImport(c, b.cfgDir, pr, req, false); err != nil {
		t.Fatalf("second import: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(errOut.String(), "warning: leftover staging directory "+leftover+" (remove with --clean-staging)") {
		t.Errorf("no leftover warning:\n%s", errOut.String())
	}
}

// --as-is skips the identity check: the session lands pending even though
// its directory exists, and the receipt says so.
func TestImportAsIs(t *testing.T) {
	a := newExportFixture(t)
	file := filepath.Join(t.TempDir(), "a.bffs")
	c, pr, _, errOut := newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err != nil {
		t.Fatalf("export: %v\n%s", err, errOut.String())
	}
	b := newImportMachine(t)
	req := b.request(file)
	req.AsIs = true
	c, pr, out, errOut := newSplitCmd("")
	if err := runImport(c, b.cfgDir, pr, req, false); err != nil {
		t.Fatalf("import: %v\n%s", err, errOut.String())
	}
	for _, want := range []string{
		"imported as-is under projects/" + a.slug + "/ (--as-is)",
		"  sessions   1 committed to projects/" + a.slug + "/ (as-is, pending rehome); 0 skipped",
		"    pending: 1 session imported as-is",
		"    claude --resume " + testSID1 + "\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("as-is output missing %q:\n%s", want, out.String())
		}
	}
	recs, _ := imports.Load(b.cfgDir)
	if len(recs) != 1 || len(recs[0].Sessions) != 1 || recs[0].Sessions[0].Status != imports.StatusPending {
		t.Errorf("record = %+v", recs)
	}
}

// ---- --from host (M5) ----

// testLAN is a synthetic address set for the on-link checks: one IPv4
// network on en0 and its link-local IPv6 prefix.
func testLAN() []transfer.LinkAddr {
	return []transfer.LinkAddr{
		{Addr: netip.MustParseAddr("192.168.1.31"), Prefix: netip.MustParsePrefix("192.168.1.0/24"), Iface: "en0"},
		{Addr: netip.MustParseAddr("fd00::31"), Prefix: netip.MustParsePrefix("fd00::/64"), Iface: "en0"},
		{Addr: netip.MustParseAddr("fe80::31").WithZone("en0"), Prefix: netip.MustParsePrefix("fe80::/64"), Iface: "en0"},
	}
}

// lanTestSeams gives the fetch side the synthetic LAN and a resolver that
// must never be reached; the real ones come back at cleanup.
func lanTestSeams(t *testing.T) {
	t.Helper()
	oldLocal, oldLookup, oldDial := fetchLocal, fetchLookup, fetchDial
	fetchLocal = func(transfer.LANOptions) ([]transfer.LinkAddr, error) { return testLAN(), nil }
	fetchLookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("lookup of %q reached the network", host)
	}
	fetchDial = func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("dial of %s reached the network", addr)
	}
	t.Cleanup(func() { fetchLocal, fetchLookup, fetchDial = oldLocal, oldLookup, oldDial })
}

func TestSplitFromHost(t *testing.T) {
	cases := []struct {
		in, host string
		port     uint16
		wantErr  string
	}{
		{"192.168.1.20", "192.168.1.20", 7345, ""},
		{"192.168.1.20:8000", "192.168.1.20", 8000, ""},
		{"[fe80::1%en0]:7345", "fe80::1%en0", 7345, ""},
		{"[fe80::1]", "fe80::1", 7345, ""},
		{"fe80::1%en0", "fe80::1%en0", 7345, ""},
		{"mac-a.local", "mac-a.local", 7345, ""},
		{"mac-a:9", "mac-a", 9, ""},
		{"[fe80::1", "", 0, "missing ]"},
		{"[fe80::1]x", "", 0, "use [address]:port"},
		{"mac-a:0", "", 0, `invalid port "0"`},
		{"mac-a:70000", "", 0, `invalid port "70000"`},
		{"mac-a:x", "", 0, `invalid port "x"`},
		{":7345", "", 0, "no host"},
	}
	for _, tc := range cases {
		host, port, err := splitFromHost(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: err = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("%q = %q, %d, %v; want %q, %d", tc.in, host, port, err, tc.host, tc.port)
		}
	}
}

// resolveHost: literals go through the on-link check without any lookup,
// names are resolved once and every address must pass, .local and
// single-label names warn, and nothing is dialled here.
func TestResolveHost(t *testing.T) {
	refusal := "not on a local network of this machine (--allow-routed for multi-VLAN offices). Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -"
	cases := []struct {
		in       string
		lookup   map[string][]netip.Addr
		routed   bool
		want     string
		wantErr  string
		warn     bool
		noLookup bool
	}{
		{in: "192.168.1.20", want: "192.168.1.20:7345", noLookup: true},
		{in: "192.168.1.20:8000", want: "192.168.1.20:8000", noLookup: true},
		{in: "[fe80::1%en0]:7345", want: "[fe80::1%en0]:7345", noLookup: true},
		{in: "[fe80::1%en1]", wantErr: "refusing to pair with fe80::1: " + refusal, noLookup: true},
		{in: "[fe80::1]", wantErr: "link-local address fe80::1 needs an interface", noLookup: true},
		{in: "203.0.113.5", wantErr: "refusing to pair with 203.0.113.5: " + refusal, noLookup: true},
		{in: "10.8.0.5", wantErr: "refusing to pair with 10.8.0.5: " + refusal, noLookup: true},
		{in: "10.8.0.5", routed: true, want: "10.8.0.5:7345", noLookup: true},
		{in: "100.64.1.1", routed: true, wantErr: "refusing to pair with 100.64.1.1: " + refusal, noLookup: true},
		{in: "::ffff:192.168.1.20", want: "192.168.1.20:7345", noLookup: true},
		{in: "mac-a.local", lookup: map[string][]netip.Addr{"mac-a.local": {netip.MustParseAddr("192.168.1.20")}}, want: "192.168.1.20:7345", warn: true},
		{in: "mac-a", lookup: map[string][]netip.Addr{"mac-a": {netip.MustParseAddr("192.168.1.20")}}, want: "192.168.1.20:7345", warn: true},
		{in: "mac-a.example.com:8000", lookup: map[string][]netip.Addr{"mac-a.example.com": {netip.MustParseAddr("fd00::20"), netip.MustParseAddr("192.168.1.20")}}, want: "192.168.1.20:8000"},
		{in: "mac-a.example.com", lookup: map[string][]netip.Addr{"mac-a.example.com": {netip.MustParseAddr("192.168.1.20"), netip.MustParseAddr("2001:db8::1")}}, wantErr: "refusing to pair with 2001:db8::1: " + refusal},
		{in: "vpn.example.com", lookup: map[string][]netip.Addr{"vpn.example.com": {netip.MustParseAddr("10.8.0.5")}}, wantErr: "refusing to pair with 10.8.0.5: " + refusal},
		{in: "nowhere.example.com", wantErr: `could not resolve "nowhere.example.com": use the IP address shown on the other machine`},
		{in: "bad host!", wantErr: `invalid host "bad host!"`, noLookup: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			looked := 0
			lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
				looked++
				if a, ok := tc.lookup[host]; ok {
					return a, nil
				}
				return nil, errors.New("no such host")
			}
			var warn strings.Builder
			got, err := resolveHost(context.Background(), tc.in, testLAN(), transfer.LANOptions{AllowRouted: tc.routed}, lookup, &warn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(tc.wantErr, "refusing") && !errors.Is(err, transfer.ErrNotLAN) {
					t.Errorf("refusal does not wrap ErrNotLAN: %v", err)
				}
			} else if err != nil || got.String() != tc.want {
				t.Fatalf("resolveHost = %s, %v; want %s", got, err, tc.want)
			}
			if tc.noLookup && looked != 0 {
				t.Errorf("a literal was looked up")
			}
			if tc.warn != strings.Contains(warn.String(), "warning: any host on this network can answer that name — the IPv4 address shown on the other machine is the safe form") {
				t.Errorf("warning = %q, want present=%v", warn.String(), tc.warn)
			}
		})
	}
}

// The B-side printer: the connect line ends in the code prompt and names
// the peer key; code-ok is one line; nothing else is printed (failures
// come back as errors and are reported once).
func TestFetchPrinterLines(t *testing.T) {
	var sb strings.Builder
	p := &fetchPrinter{w: &sb, peer: "192.168.1.20", bar: newProgressBar(&sb, "receiving", false)}
	p.event(transfer.Event{Kind: "connect", Peer: "192.168.1.20:7345", Text: "connected to 192.168.1.20:7345 (TLS 1.3, peer key 3f9a1c2e)"})
	p.event(transfer.Event{Kind: "code-ok", Peer: "192.168.1.20:7345", Text: "mac-a accepted the code (bffs 0.3.0, user jonas, account aviate, compression 1)"})
	p.event(transfer.Event{Kind: "bad-code", Peer: "192.168.1.20:7345", Text: "the other machine rejected the code (2 attempts left there). Run bffs import again."})
	p.event(transfer.Event{Kind: "error", Peer: "192.168.1.20:7345", Text: "import failed: " + osc52})
	want := "connected to 192.168.1.20 (TLS 1.3, peer key 3f9a1c2e) — it asks for the pairing code: code accepted — the other machine proved it knows the code too\n"
	if sb.String() != want {
		t.Errorf("printer output = %q, want %q", sb.String(), want)
	}
}

// The code comes from $BFFS_TRANSFER_CODE when set, folded like typed
// input, and the variable is cleared before anything else runs. The
// output never shows it.
func TestReadPairingCodeFromEnv(t *testing.T) {
	t.Setenv(envTransferCode, " 7k3q-m9xd ")
	var sb strings.Builder
	pr := newPrompter(strings.NewReader("unread\n"), &sb)
	code, err := readPairingCode(pr, true)
	if err != nil {
		t.Fatal(err)
	}
	if code.Display() != "7K3Q-M9XD" {
		t.Errorf("code = %q", code.Display())
	}
	if v, ok := os.LookupEnv(envTransferCode); ok {
		t.Errorf("%s still set to %q after reading", envTransferCode, v)
	}
	if !strings.Contains(sb.String(), "(from $BFFS_TRANSFER_CODE)") || strings.Contains(sb.String(), "7K3Q") || strings.Contains(sb.String(), "7k3q") {
		t.Errorf("output = %q", sb.String())
	}
	if rest, _ := pr.line(""); rest != "unread" {
		t.Errorf("the environment path consumed input: %q", rest)
	}
	t.Setenv(envTransferCode, "nope")
	if _, err := readPairingCode(pr, true); !errors.Is(err, transfer.ErrInvalidCode) {
		t.Errorf("invalid env code: err = %v", err)
	}
	if _, ok := os.LookupEnv(envTransferCode); ok {
		t.Errorf("%s still set after an invalid read", envTransferCode)
	}
}

// Without the variable and without a terminal the code is one line of the
// shared input (a script piping it in), never echoed, and the rest of the
// input stays for the confirmation; exhausted input is a clear refusal.
func TestReadPairingCodePiped(t *testing.T) {
	t.Setenv(envTransferCode, "") // restores whatever the caller had
	os.Unsetenv(envTransferCode)
	var sb strings.Builder
	pr := newPrompter(strings.NewReader("7k3q m9xd\ny\n"), &sb)
	code, err := readPairingCode(pr, false)
	if err != nil {
		t.Fatal(err)
	}
	if code.Display() != "7K3Q-M9XD" {
		t.Errorf("code = %q", code.Display())
	}
	if sb.String() != "\n" || strings.Contains(sb.String(), "7") {
		t.Errorf("output = %q, want just the line end", sb.String())
	}
	if rest, _ := pr.line(""); rest != "y" {
		t.Errorf("the confirmation's answer was consumed: %q", rest)
	}

	sb.Reset()
	pr = newPrompter(strings.NewReader(""), &sb)
	_, err = readPairingCode(pr, false)
	if err == nil || err.Error() != "no pairing code given: type it on a terminal, or set $BFFS_TRANSFER_CODE" {
		t.Errorf("exhausted input: err = %v", err)
	}
	if sb.String() != "\n" {
		t.Errorf("the prompt line was not ended before the error: %q", sb.String())
	}
}
