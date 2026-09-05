package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// soloAccount is a full-isolation oauth account with its own, empty
// projects/ root — the destination the import tests land bundles in.
const soloAccount = "solo"

// transferFixture extends the catalog fixture with the solo account's
// root, an existing directory a pending session can be rehomed into, a
// directory for exported bundles outside every forbidden location, and
// tempDirs pointed at a scratch directory (the fixture itself lives under
// the real temp dir, where a bundle may never be written).
type transferFixture struct {
	*catalogFixture
	soloClaude string // <cfg>/sessions/solo
	exports    string // <home>/exports
	fakeTmp    string // what tempDirs returns
	moved      string // normalised, exists: <home>/build/moved
}

func newTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	f := &transferFixture{catalogFixture: newCatalogFixture(t)}
	accs, err := store.LoadAccounts(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	accs.Accounts[soloAccount] = store.Account{Type: store.TypeOAuth, Email: "solo@example.com", Isolation: store.IsolationFull}
	if err := store.SaveAccounts(f.cfg, accs); err != nil {
		t.Fatal(err)
	}
	f.soloClaude = sessions.Dir(f.cfg, soloAccount)
	if err := os.MkdirAll(filepath.Join(f.soloClaude, transcripts.ProjectsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeClaudeJSON(filepath.Join(f.soloClaude, claudejson.Filename), map[string]any{})

	f.exports = filepath.Join(f.home, "exports")
	f.fakeTmp = filepath.Join(f.home, "tmp")
	for _, d := range []string{f.exports, f.fakeTmp, filepath.Join(f.home, "build", "moved")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if f.moved, err = store.NormalizePath(filepath.Join(f.home, "build", "moved")); err != nil {
		t.Fatal(err)
	}
	prev := tempDirs
	tempDirs = func() []string { return []string{f.fakeTmp} }
	t.Cleanup(func() { tempDirs = prev })
	return f
}

// export writes the fixture project's bundle to <exports>/<name> and
// returns the result.
func (f *transferFixture) export(t *testing.T, in ExportBundleIn) ExportBundleOut {
	t.Helper()
	if in.Output == "" {
		in.Output = filepath.Join(f.exports, "a"+bundle.Ext)
	}
	_, out, err := f.handlers().exportBundle(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("exportBundle(%+v): %v", in, err)
	}
	return out
}

// soloTranscript is where a session lands in the solo root for cwd.
func (f *transferFixture) soloTranscript(t *testing.T, cwd, sid string) string {
	t.Helper()
	slug, err := transcripts.Slug(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f.soloClaude, transcripts.ProjectsSubdir, slug, sid+transcripts.TranscriptExt)
}

// tail returns the last line of a transcript.
func tail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimRight(string(b), "\n")
	return s[strings.LastIndex(s, "\n")+1:] + "\n"
}

func TestExportBundle(t *testing.T) {
	f := newTransferFixture(t)
	h := f.handlers()
	ctx := context.Background()
	path := filepath.Join(f.exports, "a"+bundle.Ext)

	out := f.export(t, ExportBundleIn{Directory: f.project, Output: path})
	if out.BundlePath != normalized(t, path) || out.Sessions != 1 || out.MemoryDirs != 1 || out.SizeBytes <= 0 {
		t.Fatalf("export result: %+v", out)
	}
	info, err := os.Stat(out.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != out.SizeBytes {
		t.Errorf("size_bytes %d, file is %d", out.SizeBytes, info.Size())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode %o, want 0600", info.Mode().Perm())
	}
	bf, err := os.Open(out.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bf.Close()
	m, _, _, err := bundle.PeekManifest(bf)
	if err != nil {
		t.Fatalf("PeekManifest: %v", err)
	}
	if err := m.Validate(bundle.DefaultLimits); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if m.BundleID != out.BundleID || m.Totals.Entries != 2 || m.BFFSVersion != "" {
		t.Errorf("manifest: id=%s entries=%d version=%q; out=%+v", m.BundleID, m.Totals.Entries, m.BFFSVersion, out)
	}
	for _, want := range []string{"sensitive", "No credentials", "bffs import --from"} {
		if !strings.Contains(out.Note, want) {
			t.Errorf("note lacks %q: %s", want, out.Note)
		}
	}
	mustNotLeak(t, out, memoryBody, transcriptBody, catTitle)

	// No stray file may survive a refusal, and the placeholder + temp
	// file of a successful write must be gone.
	entries, err := os.ReadDir(f.exports)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a"+bundle.Ext {
		t.Errorf("exports dir after the write: %v", entries)
	}

	t.Run("version stamp", func(t *testing.T) {
		vh := &handlers{cfgDir: f.cfg, version: "9.9.9-test"}
		_, out, err := vh.exportBundle(ctx, nil, ExportBundleIn{Directory: f.project, Output: filepath.Join(f.exports, "v"+bundle.Ext)})
		if err != nil {
			t.Fatal(err)
		}
		bf, err := os.Open(out.BundlePath)
		if err != nil {
			t.Fatal(err)
		}
		defer bf.Close()
		m, _, _, err := bundle.PeekManifest(bf)
		if err != nil {
			t.Fatal(err)
		}
		if m.BFFSVersion != "9.9.9-test" || m.Source.Account != "work" {
			t.Errorf("manifest stamps: version=%q account=%q", m.BFFSVersion, m.Source.Account)
		}
	})

	t.Run("sessions only", func(t *testing.T) {
		out := f.export(t, ExportBundleIn{Sessions: []string{catSID1[:8]}, Output: filepath.Join(f.exports, "s"+bundle.Ext)})
		if out.Sessions != 1 || out.MemoryDirs != 0 {
			t.Errorf("sessions-only export: %+v", out)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name string
			in   ExportBundleIn
			want string
		}{
			{"under ~/.claude", ExportBundleIn{Directory: f.project, Output: filepath.Join(f.claude, "x"+bundle.Ext)}, "Claude config dir"},
			{"under a session dir", ExportBundleIn{Directory: f.project, Output: filepath.Join(f.soloClaude, "x"+bundle.Ext)}, "Claude config dir"},
			{"under the bffs config dir", ExportBundleIn{Directory: f.project, Output: filepath.Join(f.cfg, "x"+bundle.Ext)}, "bffs config dir"},
			{"under the temp dir", ExportBundleIn{Directory: f.project, Output: filepath.Join(f.fakeTmp, "x"+bundle.Ext)}, "temp directory"},
			{"wrong suffix", ExportBundleIn{Directory: f.project, Output: filepath.Join(f.exports, "x.tar")}, "must end in .bffs"},
			{"existing file", ExportBundleIn{Directory: f.project, Output: path}, "exists"},
			{"empty output", ExportBundleIn{Directory: f.project}, "output is required"},
			{"bad only", ExportBundleIn{Directory: f.project, Only: "everything", Output: filepath.Join(f.exports, "o"+bundle.Ext)}, "invalid only"},
			{"unknown account", ExportBundleIn{Account: "ghost", Directory: f.project, Output: filepath.Join(f.exports, "g"+bundle.Ext)}, "unknown account"},
		}
		for _, c := range cases {
			_, _, err := h.exportBundle(ctx, nil, c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
			}
			if c.in.Output != "" && c.in.Output != path {
				if _, err := os.Lstat(c.in.Output); err == nil {
					t.Errorf("%s: refused output %s was created", c.name, c.in.Output)
				}
			}
		}
		if b, err := os.ReadFile(path); err != nil || int64(len(b)) != out.SizeBytes {
			t.Errorf("existing bundle was touched: %v (%d bytes)", err, len(b))
		}
	})
}

func TestImportBundleIdentity(t *testing.T) {
	f := newTransferFixture(t)
	h := f.handlers()
	ctx := context.Background()
	exp := f.export(t, ExportBundleIn{Directory: f.project})
	landed := f.soloTranscript(t, f.project, catSID1)
	original := tail(t, filepath.Join(f.claude, transcripts.ProjectsSubdir, f.slug, catSID1+transcripts.TranscriptExt))

	_, dry, err := h.importBundle(ctx, nil, ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.BundleID != exp.BundleID || len(dry.Imported) != 1 || dry.Imported[0] != catSID1 || !strings.Contains(dry.Note, "Dry run") {
		t.Errorf("dry run result: %+v", dry)
	}
	if _, err := os.Lstat(landed); err == nil {
		t.Fatal("dry run wrote the transcript")
	}

	_, out, err := h.importBundle(ctx, nil, ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount})
	if err != nil {
		t.Fatalf("importBundle: %v", err)
	}
	if out.BundleID != exp.BundleID || len(out.Imported) != 1 || out.Imported[0] != catSID1 {
		t.Fatalf("import result: %+v", out)
	}
	if len(out.Pending)+len(out.Skipped)+len(out.Held) != 0 || out.Pending == nil || out.Skipped == nil || out.Held == nil {
		t.Errorf("session lists: %+v", out)
	}
	if len(out.Verify) != 1 || !strings.Contains(out.Verify[0], "claude --resume "+catSID1) || !strings.Contains(out.Verify[0], "BFFS_ACCOUNT="+soloAccount) {
		t.Errorf("verify: %v", out.Verify)
	}
	for _, want := range []string{"never merged", "Trust is not carried", "Imported 1 session"} {
		if !strings.Contains(out.Note, want) {
			t.Errorf("note lacks %q: %s", want, out.Note)
		}
	}
	mustNotLeak(t, out, memoryBody, transcriptBody, catTitle)
	// Identity placement: the transcript lands under the directory's own
	// entry with no relocated record.
	if got := tail(t, landed); got != original {
		t.Errorf("identity placement must not stamp: last line %q, want %q", got, original)
	}

	t.Run("already imported", func(t *testing.T) {
		_, _, err := h.importBundle(ctx, nil, ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount})
		if err == nil || !strings.Contains(err.Error(), "was imported on") {
			t.Errorf("repeat import: %v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name string
			in   ImportBundleIn
			want string
		}{
			{"memory merge", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, Memory: "merge"}, "CLI"},
			{"memory overwrite", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, Memory: "overwrite"}, "CLI"},
			{"memory nonsense", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, Memory: "keep"}, "invalid memory"},
			{"on_conflict overwrite", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, OnConflict: "overwrite"}, "CLI"},
			{"on_conflict nonsense", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, OnConflict: "fork"}, "invalid on_conflict"},
			{"as_is with rehome", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, AsIs: true, Rehome: map[string]string{oldCwd: f.moved}}, "mutually exclusive"},
			{"rehome target missing", ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, Rehome: map[string]string{oldCwd: filepath.Join(f.home, "nope")}}, "not a directory"},
			{"empty path", ImportBundleIn{Account: soloAccount}, "bundle_path is required"},
			{"wrong suffix", ImportBundleIn{BundlePath: filepath.Join(f.exports, "x.tar"), Account: soloAccount}, "must end in .bffs"},
			{"missing file", ImportBundleIn{BundlePath: filepath.Join(f.exports, "missing"+bundle.Ext), Account: soloAccount}, "missing"},
			{"unknown account", ImportBundleIn{BundlePath: exp.BundlePath, Account: "ghost"}, "unknown account"},
		}
		for _, c := range cases {
			_, _, err := h.importBundle(ctx, nil, c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
			}
		}
	})
}

func TestImportBundleRehomeMapping(t *testing.T) {
	f := newTransferFixture(t)
	f.importPending()
	h := f.handlers()
	ctx := context.Background()
	exp := f.export(t, ExportBundleIn{Sessions: []string{catSID2}})
	if exp.Sessions != 1 {
		t.Fatalf("export of the pending session: %+v", exp)
	}

	// Without a rule the session's directory does not exist here, so it
	// would land as-is, pending rehome (a dry run: nothing is written and
	// no record is made, so the mapped import below still runs).
	_, asIs, err := h.importBundle(ctx, nil, ImportBundleIn{BundlePath: exp.BundlePath, Account: soloAccount, AsIs: true, DryRun: true})
	if err != nil {
		t.Fatalf("as-is dry run: %v", err)
	}
	if len(asIs.Pending) != 1 || asIs.Pending[0] != catSID2 || len(asIs.Imported) != 0 {
		t.Errorf("as-is dry run: %+v", asIs)
	}

	_, out, err := h.importBundle(ctx, nil, ImportBundleIn{
		BundlePath: exp.BundlePath, Account: soloAccount,
		Rehome: map[string]string{oldCwd: f.moved},
	})
	if err != nil {
		t.Fatalf("importBundle: %v", err)
	}
	if len(out.Imported) != 1 || out.Imported[0] != catSID2 || len(out.Pending) != 0 {
		t.Fatalf("mapped import: %+v", out)
	}
	landed := f.soloTranscript(t, f.moved, catSID2)
	if got, want := tail(t, landed), string(rehome.RelocatedRecord(catSID2, f.moved)); got != want {
		t.Errorf("mapped placement must stamp: last line %q, want %q", got, want)
	}
	if len(out.Verify) != 1 || !strings.Contains(out.Verify[0], f.moved) {
		t.Errorf("verify must name the new directory: %v", out.Verify)
	}
	if !strings.Contains(out.Note, "1 mapped") {
		t.Errorf("note: %s", out.Note)
	}
	mustNotLeak(t, out, transcriptBody)
}

func TestRehomeDryRunAndApply(t *testing.T) {
	f := newTransferFixture(t)
	f.importPending()
	h := f.handlers()
	ctx := context.Background()
	oldSlug, err := transcripts.Slug(oldCwd)
	if err != nil {
		t.Fatal(err)
	}
	newSlug, err := transcripts.Slug(f.moved)
	if err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(f.claude, transcripts.ProjectsSubdir, oldSlug, catSID2+transcripts.TranscriptExt)
	to := filepath.Join(f.claude, transcripts.ProjectsSubdir, newSlug, catSID2+transcripts.TranscriptExt)
	before, err := os.Stat(from)
	if err != nil {
		t.Fatal(err)
	}
	in := RehomeIn{Mappings: []RehomeMapping{{Old: oldCwd, New: f.moved}}, BundleID: catBundle[:8]}

	dry := in
	dry.DryRun = true
	_, out, err := h.rehomeSessions(ctx, nil, dry)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(out.Moves) != 1 {
		t.Fatalf("dry run moves: %+v", out)
	}
	mv := out.Moves[0]
	if mv.SessionID != catSID2 || mv.From != from || mv.To != to || mv.NewCwd != f.moved || mv.Stamped || mv.SidecarMoved {
		t.Errorf("dry run move: %+v (want from=%s to=%s)", mv, from, to)
	}
	if len(out.Verify) != 1 || !strings.Contains(out.Verify[0], "claude --resume "+catSID2) || !strings.Contains(out.Verify[0], f.moved) {
		t.Errorf("dry run verify: %v", out.Verify)
	}
	if !strings.Contains(out.Note, "Dry run") || !strings.Contains(out.Note, "never merged") || !strings.Contains(out.Note, "Trust is not carried") {
		t.Errorf("dry run note: %s", out.Note)
	}
	if out.Held == nil || out.Skipped == nil || out.MemoryFilesRewritten == nil || out.MemoryLinesStillAbsolute == nil {
		t.Errorf("slices must be initialised: %+v", out)
	}
	if _, err := os.Lstat(to); err == nil {
		t.Fatal("dry run moved the transcript")
	}
	if _, err := os.Lstat(from); err != nil {
		t.Fatalf("dry run removed the transcript: %v", err)
	}
	mustNotLeak(t, out, transcriptBody)

	_, out, err = h.rehomeSessions(ctx, nil, in)
	if err != nil {
		t.Fatalf("rehome: %v", err)
	}
	if len(out.Moves) != 1 || !out.Moves[0].Stamped || out.Moves[0].SidecarMoved {
		t.Fatalf("rehome moves: %+v", out.Moves)
	}
	if _, err := os.Lstat(from); err == nil {
		t.Error("old transcript still there after the move")
	}
	if got, want := tail(t, to), string(rehome.RelocatedRecord(catSID2, f.moved)); got != want {
		t.Errorf("moved transcript last line %q, want %q", got, want)
	}
	after, err := os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}
	if d := after.ModTime().Sub(before.ModTime()); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("mtime changed by %v (picker order must be preserved)", d)
	}
	if !strings.Contains(out.Note, "Moved 1 of 1") {
		t.Errorf("note: %s", out.Note)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.cfg, "staging")); len(entries) != 0 {
		t.Errorf("journal left behind: %v", entries)
	}

	t.Run("nothing left to rehome", func(t *testing.T) {
		_, out, err := h.rehomeSessions(ctx, nil, in)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Moves) != 0 || !strings.Contains(out.Note, "Nothing to rehome") {
			t.Errorf("second run: %+v", out)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		cases := []struct {
			name string
			in   RehomeIn
			want string
		}{
			{"no mappings", RehomeIn{}, "no mapping"},
			{"empty old", RehomeIn{Mappings: []RehomeMapping{{New: f.moved}}}, "old is required"},
			{"empty new", RehomeIn{Mappings: []RehomeMapping{{Old: oldCwd}}}, "new is required"},
			{"missing new", RehomeIn{Mappings: []RehomeMapping{{Old: oldCwd, New: filepath.Join(f.home, "nope")}}}, "not a directory"},
			{"unknown bundle", RehomeIn{Mappings: []RehomeMapping{{Old: oldCwd, New: f.moved}}, BundleID: "ffffffff"}, "ffffffff"},
			{"unknown account", RehomeIn{Mappings: []RehomeMapping{{Old: oldCwd, New: f.moved}}, Account: "ghost"}, "unknown account"},
		}
		for _, c := range cases {
			_, _, err := h.rehomeSessions(ctx, nil, c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
			}
		}
	})
}

func TestCheckBundleSize(t *testing.T) {
	ok := &bundle.Manifest{Totals: bundle.Totals{Bytes: 1 << 30}}
	if err := checkBundleSize(ok); err != nil {
		t.Errorf("exactly 1 GiB must pass: %v", err)
	}
	big := &bundle.Manifest{Totals: bundle.Totals{Bytes: 1<<30 + 1}}
	err := checkBundleSize(big)
	if err == nil || !strings.Contains(err.Error(), "use the CLI for bundles over 1 GiB") {
		t.Errorf("over 1 GiB: %v", err)
	}
}

func TestDefaultTempDirs(t *testing.T) {
	dirs := defaultTempDirs()
	want := map[string]bool{"/tmp": false, "/private/tmp": false, os.TempDir(): false}
	for _, d := range dirs {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for d, seen := range want {
		if !seen {
			t.Errorf("default temp dirs lack %q: %v", d, dirs)
		}
	}
}

func TestUnderDir(t *testing.T) {
	sep := string(filepath.Separator)
	base := filepath.Join(sep, "home", "j")
	for _, c := range []struct {
		p, dir string
		want   bool
	}{
		{filepath.Join(base, "x.bffs"), base, true},
		{base, base, true},
		{filepath.Join(base, "a", "b"), base, true},
		{filepath.Join(sep, "home", "jo", "x"), base, false},
		{filepath.Join(sep, "home"), base, false},
		{filepath.Join(base, "x"), "", false},
	} {
		if got := underDir(c.p, c.dir); got != c.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", c.p, c.dir, got, c.want)
		}
	}
}

func TestParseMappingsOrder(t *testing.T) {
	f := newTransferFixture(t)
	maps, err := parseMappings(mappingsOf(map[string]string{"/b/old": f.moved, "/a/old": f.project}))
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 2 || maps[0].Old != "/a/old" || maps[1].Old != "/b/old" || maps[0].New != f.project || maps[1].New != f.moved {
		t.Errorf("mappings in key order: %+v", maps)
	}
}

// TestTransferToolsOverTheWire drives the three write tools through a real
// client session, so the generated schemas validate the inputs (a pointer
// bool, a string map) and the outputs (every slice non-nil).
func TestTransferToolsOverTheWire(t *testing.T) {
	f := newTransferFixture(t)
	f.importPending()
	ctx := context.Background()

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := New(f.cfg, "test").Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	seen := 0
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "export_bundle", "import_bundle", "rehome":
			seen++
			a := tool.Annotations
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
				t.Errorf("%s: want the write annotations (not read-only, not destructive, closed world), got %+v", tool.Name, a)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("want the three transfer tools listed, saw %d", seen)
	}

	call := func(name string, args map[string]any, want ...string) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s errored: %+v", name, res.Content)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s: missing %q in %s", name, w, raw)
			}
		}
		for _, n := range []string{testSecret, memoryBody, transcriptBody, "\\u001b"} {
			if strings.Contains(string(raw), n) {
				t.Errorf("%s: %q leaked through the wire", name, n)
			}
		}
		m, _ := res.StructuredContent.(map[string]any)
		return m
	}

	output := filepath.Join(f.exports, "wire"+bundle.Ext)
	exp := call("export_bundle", map[string]any{"directory": f.project, "output": output, "no_tool_results": true},
		`"bundle_id":"`, `"sessions":1`, `"memory_dirs":1`)
	bundlePath, _ := exp["bundle_path"].(string)
	if bundlePath == "" {
		t.Fatalf("export_bundle returned no bundle_path: %v", exp)
	}
	call("import_bundle", map[string]any{"bundle_path": bundlePath, "account": soloAccount, "dry_run": true, "rehome": map[string]any{oldCwd: f.moved}},
		`"imported":["`+catSID1+`"]`, `"pending":[]`, `"held":[]`, `"skipped":[]`, `"verify":["`)
	call("rehome", map[string]any{"mappings": []map[string]any{{"old": oldCwd, "new": f.moved}}, "bundle_id": catBundle, "dry_run": true, "rewrite_memory": false},
		`"moves":[{`, `"session_id":"`+catSID2+`"`, `"stamped":false`, `"memory_files_rewritten":[]`, `"memory_lines_still_absolute":[]`, `"verify":["`)

	// A refusal is a tool error with content, not a protocol error.
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "import_bundle", Arguments: map[string]any{"bundle_path": bundlePath, "memory": "merge"}})
	if err != nil {
		t.Fatalf("CallTool(import_bundle merge): %v", err)
	}
	if !res.IsError {
		t.Error("memory merge must produce IsError=true")
	}
}
