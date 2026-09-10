package porter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// newDir makes a directory the tests map a project to and returns its
// normalised path.
func newDir(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	mkdir(t, d)
	n, err := store.NormalizePath(d)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func slugOf(t *testing.T, dir string) string {
	t.Helper()
	s, err := transcripts.Slug(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// manualSession describes one session of a hand-built bundle.
type manualSession struct {
	sid, cwd string
	trust    *bundle.TrustInfo
}

// manualBundle builds a bundle of sessions whose cwds are given verbatim
// (typically directories that do not exist here), each with a two-line
// transcript, plus a memory directory per distinct cwd.
func manualBundle(t *testing.T, sourceHome string, sessions []manualSession) (*bundle.Manifest, []byte) {
	t.Helper()
	files := memOpener{}
	m := &bundle.Manifest{
		Format: bundle.FormatVersion, BundleID: "6f1e2c0a-1111-4222-8333-444455556666", Created: fixedNow,
		Source: bundle.Source{Hostname: "mac-a", User: "jonas", Home: sourceHome},
	}
	add := func(e bundle.Entry, p string, data []byte) bundle.Entry {
		sum := sha256.Sum256(data)
		files[p] = data
		e.Files = append(e.Files, bundle.File{Path: p, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), ModTime: fixedNow})
		m.Totals.Files++
		m.Totals.Bytes += int64(len(data))
		return e
	}
	memDone := map[string]bool{}
	for i, s := range sessions {
		slug := slugOf(t, s.cwd)
		tr := []byte(userRec(s.sid, s.cwd, "2026-08-14T10:00:00Z", "prompt "+s.sid[:8]) + rec(map[string]any{"type": "assistant", "sessionId": s.sid}))
		e := bundle.Entry{Kind: bundle.EntrySession, Slug: slug, Cwd: s.cwd, ProjectKey: s.cwd, SessionID: s.sid, GitRemote: "git@github.com:x/y.git", Last: fixedNow.Add(-time.Duration(i) * day), SourceTrust: s.trust}
		e = add(e, "projects/"+slug+"/"+s.sid+".jsonl", tr)
		m.Entries = append(m.Entries, e)
		if !memDone[s.cwd] {
			memDone[s.cwd] = true
			me := bundle.Entry{Kind: bundle.EntryMemory, Slug: slug, Cwd: s.cwd, ProjectKey: s.cwd}
			me = add(me, "memory/"+slug+"/MEMORY.md", []byte("- [notes](notes.md) - notes\n"))
			me = add(me, "memory/"+slug+"/notes.md", []byte("---\nname: notes\n---\nsee "+s.cwd+"/x.md and "+sourceHome+"/tools\n"))
			m.Entries = append(m.Entries, me)
		}
	}
	m.Totals.Entries = len(m.Entries)
	var buf bytes.Buffer
	if _, err := bundle.Build(context.Background(), &buf, m, files, bundle.CompNone, nil); err != nil {
		t.Fatal(err)
	}
	return m, buf.Bytes()
}

func TestImportMapped(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	writeFile(t, filepath.Join(src.memDir(t), "paths.md"), "at "+src.cwd+"/file.go\n")
	m, data := exportPool(t, src)
	dst := secondEnv(t, src)
	moved := newDir(t, "moved")
	opts := importOpts(dst)
	opts.Map = []rehome.Mapping{{Old: src.cwd, New: moved}}
	opts.SetLastSession = true
	rep := doImport(t, dst, data, opts)

	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || !reflect.DeepEqual(rep.Rehomed, []string{sidB, sidA}) || len(rep.Pending) != 0 {
		t.Errorf("Imported=%v Rehomed=%v Pending=%v", rep.Imported, rep.Rehomed, rep.Pending)
	}
	slug := slugOf(t, moved)
	for _, sid := range []string{sidA, sidB} {
		tr := filepath.Join(dst.root.Dir, slug, sid+".jsonl")
		if got := readFile(t, tr); !strings.HasSuffix(got, string(rehome.RelocatedRecord(sid, moved))) {
			t.Errorf("%s not stamped:\n%s", sid, got)
		}
	}
	if !exists(filepath.Join(dst.root.Dir, slug, sidA, "tool-results", "abc.txt")) {
		t.Error("sidecar missing")
	}
	if exists(filepath.Join(dst.root.Dir, src.slug(t))) {
		t.Error("original slug created")
	}
	// History lines carry the new directory.
	proj, _ := json.Marshal(moved)
	if hist := readFile(t, filepath.Join(dst.claudeDir, transcripts.HistoryFile)); !strings.Contains(hist, `"project":`+string(proj)) || strings.Contains(hist, `"project":"`+src.cwd+`"`) {
		t.Errorf("history:\n%s", hist)
	}
	// Memory: confirmed merge into a fresh directory, old paths rewritten.
	mem, err := transcripts.MemoryDirFor(dst.root, moved)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.MemoryDirs, []string{mem}) {
		t.Errorf("MemoryDirs = %v", rep.MemoryDirs)
	}
	if !exists(filepath.Join(mem, transcripts.MemoryIndexFile)) || !exists(filepath.Join(mem, "topic.md")) {
		t.Error("confirmed memory not written under its own names")
	}
	if got := readFile(t, filepath.Join(mem, "paths.md")); got != "at "+moved+"/file.go\n" {
		t.Errorf("paths.md = %q", got)
	}
	if got := readFile(t, filepath.Join(mem, "topic.md")); !strings.Contains(got, "pinned-imported: true") {
		t.Errorf("topic = %q", got)
	}
	// lastSessionId points at the newest mapped session.
	key, err := transcripts.ProjectKey(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rep.LastSession, map[string]string{moved: sidB}) {
		t.Errorf("LastSession = %v", rep.LastSession)
	}
	flags, err := claudejson.ReadProjectFlags(dst.root.ClaudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if flags[key].LastSessionID != sidB {
		t.Errorf("lastSessionId = %q", flags[key].LastSessionID)
	}
	if exists(dst.root.ClaudeJSON + ".lock") {
		t.Error("lock left behind")
	}
	// Record: rehomed, with the mapping.
	recs, _ := imports.Load(dst.cfgDir)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	for _, s := range recs[0].Sessions {
		if s.Status != imports.StatusRehomed || s.NewCwd != moved || s.Slug != slug || s.OldCwd != src.cwd {
			t.Errorf("session record = %+v", s)
		}
	}
	if recs[0].Memories[0].Status != imports.StatusRehomed || recs[0].Memories[0].Dir != mem {
		t.Errorf("memory record = %+v", recs[0].Memories[0])
	}
	if !reflect.DeepEqual(recs[0].Mapping, opts.Map) {
		t.Errorf("record mapping = %+v", recs[0].Mapping)
	}
	if len(rep.Verify) != 1 || rep.Verify[0] != rehome.VerifyCommand(moved, sidB, "") {
		t.Errorf("Verify = %v", rep.Verify)
	}
	// The pool lists the sessions under the new directory.
	ss, err := transcripts.List(context.Background(), dst.root, transcripts.ListOptions{Titles: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Cwd != moved || !s.Relocated {
			t.Errorf("listed cwd = %q relocated %v", s.Cwd, s.Relocated)
		}
	}
	_ = m
}

func TestImportMappedMissingTargetFallsBack(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	gone := filepath.Join(t.TempDir(), "gone")
	opts := importOpts(dst)
	opts.Map = []rehome.Mapping{{Old: src.cwd, New: gone}}
	opts.NoRewriteMemory = true
	rep := doImport(t, dst, data, opts)
	if len(rep.Imported) != 0 || !reflect.DeepEqual(rep.Pending, []string{sidB, sidA}) {
		t.Errorf("Imported=%v Pending=%v", rep.Imported, rep.Pending)
	}
	if !containsWarning(rep.Warnings, fmt.Sprintf("mapped directory %q does not exist; importing as-is", gone)) {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if got := readFile(t, filepath.Join(dst.root.Dir, src.slug(t), sidA+".jsonl")); strings.Contains(got, "relocated") {
		t.Error("as-is fallback was stamped")
	}
	// A rule that covers nothing leaves identity placement in force.
	dst2 := secondEnv(t, src)
	opts = importOpts(dst2)
	opts.Map = []rehome.Mapping{{Old: "/nowhere/else", New: newDir(t, "x")}}
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || len(rep.Rehomed) != 0 {
		t.Errorf("Imported=%v Rehomed=%v", rep.Imported, rep.Rehomed)
	}
}

func TestImportInto(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	moved := newDir(t, "moved")
	opts := importOpts(dst)
	opts.Into = moved
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Rehomed, []string{sidB, sidA}) {
		t.Errorf("Rehomed = %v", rep.Rehomed)
	}
	if !exists(filepath.Join(dst.root.Dir, slugOf(t, moved), sidA+".jsonl")) {
		t.Error("session not under the --into directory")
	}
	recs, _ := imports.Load(dst.cfgDir)
	if want := []imports.Mapping{{Old: src.cwd, New: moved}}; !reflect.DeepEqual(recs[0].Mapping, want) {
		t.Errorf("record mapping = %+v, want %+v", recs[0].Mapping, want)
	}
	// --into the entry's own directory is identity: no stamp, but the
	// memory placement counts as confirmed.
	dst2 := secondEnv(t, src)
	mem := dst2.memDir(t)
	writeFile(t, filepath.Join(mem, "mine.md"), "mine\n")
	opts = importOpts(dst2)
	opts.Into = src.cwd
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.Imported, []string{sidB, sidA}) || len(rep.Rehomed) != 0 {
		t.Errorf("Imported=%v Rehomed=%v", rep.Imported, rep.Rehomed)
	}
	if got := readFile(t, filepath.Join(dst2.root.Dir, src.slug(t), sidA+".jsonl")); strings.Contains(got, "relocated") {
		t.Error("identity placement stamped")
	}
	if !exists(filepath.Join(mem, "topic.md")) || !exists(filepath.Join(mem, transcripts.MemoryIndexFile)) || readFile(t, filepath.Join(mem, "mine.md")) != "mine\n" {
		t.Error("confirmed identity placement should merge memory under its own names")
	}
	// A multi-project bundle refuses --into.
	_, multi := manualBundle(t, "/Users/other", []manualSession{{sid: sidA, cwd: "/Users/other/a"}, {sid: sidB, cwd: "/Users/other/b"}})
	dst3 := secondEnv(t, src)
	opts = importOpts(dst3)
	opts.Into = moved
	if _, err := Import(context.Background(), dst3.cfgDir, bytes.NewReader(multi), opts); err == nil || !strings.Contains(err.Error(), "bundle holds 2 projects; --into needs a single-project bundle") {
		t.Errorf("err = %v", err)
	}
}

func TestImportPlacer(t *testing.T) {
	accepted := &bundle.TrustInfo{Accepted: true, ExternalIncludesApproved: true, ExternalIncludesWarningShown: true}
	m, data := manualBundle(t, "/Users/other", []manualSession{
		{sid: sidA, cwd: "/Users/other/build/a", trust: accepted},
		{sid: sidB, cwd: "/Users/other/build/b"},
		{sid: sidC, cwd: "/Users/other/build/a", trust: accepted},
	})
	dst := newEnv(t)
	movedA := newDir(t, "a")
	var got []rehome.Suggestion
	calls := 0
	placer := func(ctx context.Context, mm *bundle.Manifest, sugg []rehome.Suggestion) ([]Placement, error) {
		calls++
		got = sugg
		if mm.BundleID != m.BundleID {
			t.Errorf("placer got bundle %s", mm.BundleID)
		}
		// Answer per project: a mapped for /Users/other/build/a with trust
		// carried, as-is for b (by entry pointer).
		var out []Placement
		for i := range mm.Entries {
			e := &mm.Entries[i]
			switch e.Cwd {
			case "/Users/other/build/a":
				out = append(out, Placement{Entry: e, NewCwd: movedA, Mode: PlaceMapped, CarryTrust: true})
			case "/Users/other/build/b":
				out = append(out, Placement{Entry: e, Mode: PlaceAsIs})
			}
		}
		return out, nil
	}
	opts := importOpts(dst)
	opts.Place = placer
	rep := doImport(t, dst, data, opts)
	if calls != 1 {
		t.Errorf("placer called %d times", calls)
	}
	if len(got) != 2 || got[0].OldCwd != "/Users/other/build/a" || got[1].OldCwd != "/Users/other/build/b" {
		t.Errorf("suggestions = %+v", got)
	}
	if !reflect.DeepEqual(rep.Rehomed, []string{sidA, sidC}) || !reflect.DeepEqual(rep.Pending, []string{sidB}) {
		t.Errorf("Rehomed=%v Pending=%v", rep.Rehomed, rep.Pending)
	}
	slugA := slugOf(t, movedA)
	if !exists(filepath.Join(dst.root.Dir, slugA, sidA+".jsonl")) || !exists(filepath.Join(dst.root.Dir, slugA, sidC+".jsonl")) {
		t.Error("mapped sessions not under the chosen directory")
	}
	if !exists(filepath.Join(dst.root.Dir, "-Users-other-build-b", sidB+".jsonl")) {
		t.Error("as-is session not under its original slug")
	}
	// Trust carried for the mapped, confirmed directory (the placer's
	// CarryTrust): the three answers at ProjectKey(movedA).
	key, _ := transcripts.ProjectKey(movedA)
	if !reflect.DeepEqual(rep.TrustCarried, []string{key}) {
		t.Errorf("TrustCarried = %v (warnings %v)", rep.TrustCarried, rep.Warnings)
	}
	flags, err := claudejson.ReadProjectFlags(dst.root.ClaudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	f := flags[key]
	if f.TrustAccepted == nil || !*f.TrustAccepted || f.ExternalIncludesApproved == nil || !*f.ExternalIncludesApproved || f.ExternalIncludesWarningShown == nil || !*f.ExternalIncludesWarningShown {
		t.Errorf("trust flags = %+v", f)
	}
	// Memory of the mapped project merged under the new directory with the
	// old home rewritten; the as-is memory kept its side-file shape.
	memA, _ := transcripts.MemoryDirFor(dst.root, movedA)
	home, _ := os.UserHomeDir()
	if got := readFile(t, filepath.Join(memA, "notes.md")); !strings.Contains(got, "see "+movedA+"/x.md and "+home+"/tools") {
		t.Errorf("notes = %q", got)
	}
	if !exists(filepath.Join(dst.root.Dir, "-Users-other-build-b", "memory", "notes.imported-6f1e2c0a.md")) {
		t.Error("as-is memory not written as a side file")
	}
	recs, _ := imports.Load(dst.cfgDir)
	if want := []imports.Mapping{{Old: "/Users/other/build/a", New: movedA}}; !reflect.DeepEqual(recs[0].Mapping, want) {
		t.Errorf("record mapping = %+v", recs[0].Mapping)
	}

	// A placer answer naming a missing directory is an error before
	// anything is written; an erroring placer stops the import.
	dst2 := newEnv(t)
	opts = importOpts(dst2)
	opts.Place = func(context.Context, *bundle.Manifest, []rehome.Suggestion) ([]Placement, error) {
		return []Placement{{Entry: &m.Entries[0], NewCwd: filepath.Join(t.TempDir(), "gone"), Mode: PlaceMapped}}, nil
	}
	if _, err := Import(context.Background(), dst2.cfgDir, bytes.NewReader(data), opts); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
	if exists(filepath.Join(dst2.root.Dir, "-Users-other-build-b")) {
		t.Error("written despite the placement error")
	}
	// Placer not consulted when every entry is decided by a rule.
	dst3 := newEnv(t)
	build := newDir(t, "build")
	mkdir(t, filepath.Join(build, "a"))
	mkdir(t, filepath.Join(build, "b"))
	opts = importOpts(dst3)
	opts.Map = []rehome.Mapping{{Old: "/Users/other/build", New: build}}
	opts.Place = func(context.Context, *bundle.Manifest, []rehome.Suggestion) ([]Placement, error) {
		t.Error("placer called although every entry was mapped")
		return nil, nil
	}
	rep = doImport(t, dst3, data, opts)
	if len(rep.Rehomed) != 3 {
		t.Errorf("Rehomed = %v (warnings %v)", rep.Rehomed, rep.Warnings)
	}
}

func TestImportCarryTrustOnlyMappedConfirmed(t *testing.T) {
	accepted := &bundle.TrustInfo{Accepted: true, ExternalIncludesApproved: true, ExternalIncludesWarningShown: true}
	// Identity placement with --carry-trust: nothing is written.
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	opts := importOpts(dst)
	opts.CarryTrust = true
	rep := doImport(t, dst, data, opts)
	if len(rep.TrustCarried) != 0 || exists(dst.root.ClaudeJSON) {
		t.Errorf("identity placement carried trust: %v", rep.TrustCarried)
	}
	// Mapped by rule with --carry-trust: written under T2 — a declined
	// external-imports answer on the target is never overridden.
	_, data = manualBundle(t, "/Users/other", []manualSession{{sid: sidA, cwd: "/Users/other/build/a", trust: accepted}})
	dst2 := newEnv(t)
	moved := newDir(t, "a")
	key, _ := transcripts.ProjectKey(moved)
	writeFile(t, dst2.root.ClaudeJSON, `{"projects":{`+jsonKey(key)+`:{"hasClaudeMdExternalIncludesApproved":false,"hasClaudeMdExternalIncludesWarningShown":true}}}`)
	opts = importOpts(dst2)
	opts.CarryTrust = true
	opts.Map = []rehome.Mapping{{Old: "/Users/other/build/a", New: moved}}
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.TrustCarried, []string{key}) {
		t.Errorf("TrustCarried = %v (warnings %v)", rep.TrustCarried, rep.Warnings)
	}
	flags, _ := claudejson.ReadProjectFlags(dst2.root.ClaudeJSON)
	f := flags[key]
	if f.TrustAccepted == nil || !*f.TrustAccepted {
		t.Errorf("folder trust not carried: %+v", f)
	}
	if f.ExternalIncludesApproved == nil || *f.ExternalIncludesApproved {
		t.Errorf("declined external imports overridden: %+v", f)
	}
	// Without --carry-trust a mapped placement writes nothing.
	dst3 := newEnv(t)
	opts = importOpts(dst3)
	opts.Map = []rehome.Mapping{{Old: "/Users/other/build/a", New: moved}}
	rep = doImport(t, dst3, data, opts)
	if len(rep.TrustCarried) != 0 || exists(dst3.root.ClaudeJSON) {
		t.Error("trust written without --carry-trust")
	}
}

// jsonKey renders a project key as a JSON object key.
func jsonKey(key string) string {
	b, _ := json.Marshal(key)
	return string(b)
}

func TestImportSetLastSessionTargetsAccountFile(t *testing.T) {
	src := newEnv(t)
	seedPool(t, src)
	_, data := exportPool(t, src)
	dst := secondEnv(t, src)
	// An oauth account under partial isolation has its own .claude.json.
	if err := store.SaveAccounts(dst.cfgDir, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}}); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(dst.cfgDir, "sessions", "work"))
	moved := newDir(t, "moved")
	opts := importOpts(dst)
	opts.Account = "work"
	opts.Map = []rehome.Mapping{{Old: src.cwd, New: moved}}
	opts.SetLastSession = true
	rep := doImport(t, dst, data, opts)
	key, _ := transcripts.ProjectKey(moved)
	acctJSON := filepath.Join(dst.cfgDir, "sessions", "work", claudejson.Filename)
	flags, err := claudejson.ReadProjectFlags(acctJSON)
	if err != nil {
		t.Fatal(err)
	}
	if flags[key].LastSessionID != sidB || rep.LastSession[moved] != sidB {
		t.Errorf("lastSessionId = %q, report %v (warnings %v)", flags[key].LastSessionID, rep.LastSession, rep.Warnings)
	}
	if exists(dst.root.ClaudeJSON) {
		t.Error("home .claude.json written for an oauth account")
	}
	// Dry run: nothing written, plan reported.
	dst2 := secondEnv(t, src)
	opts = importOpts(dst2)
	opts.Map = []rehome.Mapping{{Old: src.cwd, New: moved}}
	opts.SetLastSession = true
	opts.DryRun = true
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.Rehomed, []string{sidB, sidA}) || exists(dst2.root.ClaudeJSON) || exists(filepath.Join(dst2.root.Dir, slugOf(t, moved))) {
		t.Errorf("dry run wrote or misreported: %+v", rep)
	}
}

// rawBundle builds a one-session bundle of cwd whose transcript bytes are
// given verbatim, so a test can ship a torn last line.
func rawBundle(t *testing.T, cwd, sid string, transcript []byte) []byte {
	t.Helper()
	slug := slugOf(t, cwd)
	sum := sha256.Sum256(transcript)
	p := "projects/" + slug + "/" + sid + ".jsonl"
	m := &bundle.Manifest{
		Format: bundle.FormatVersion, BundleID: "6f1e2c0a-1111-4222-8333-444455556666", Created: fixedNow,
		Source: bundle.Source{Hostname: "mac-a", Home: "/Users/other"},
		Entries: []bundle.Entry{{Kind: bundle.EntrySession, Slug: slug, Cwd: cwd, ProjectKey: cwd, SessionID: sid, Last: fixedNow, Files: []bundle.File{
			{Path: p, Size: int64(len(transcript)), SHA256: hex.EncodeToString(sum[:]), ModTime: fixedNow},
		}}},
		Totals: bundle.Totals{Entries: 1, Files: 1, Bytes: int64(len(transcript))},
	}
	var buf bytes.Buffer
	if _, err := bundle.Build(context.Background(), &buf, m, memOpener{p: transcript}, bundle.CompNone, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A mapped session gets a relocated record; a transcript whose last line
// is torn (a live session exported mid-write) is not stamped over unless
// ForceStamp (plan §9.8) — it lands as-is, pending, with a warning; an
// overwrite that cannot fall back is skipped instead.
func TestImportMappedTornLastLine(t *testing.T) {
	const oldCwd = "/Users/other/build/a"
	torn := []byte(userRec(sidA, oldCwd, "2026-08-24T10:00:00Z", "prompt") + `{"type":"assist`)
	data := rawBundle(t, oldCwd, sidA, torn)
	oldSlug := "-Users-other-build-a"

	dst := newEnv(t)
	moved := newDir(t, "a")
	opts := importOpts(dst)
	opts.Map = []rehome.Mapping{{Old: oldCwd, New: moved}}
	rep := doImport(t, dst, data, opts)
	if !reflect.DeepEqual(rep.Pending, []string{sidA}) || len(rep.Rehomed) != 0 || len(rep.Imported) != 0 {
		t.Errorf("Pending=%v Rehomed=%v Imported=%v", rep.Pending, rep.Rehomed, rep.Imported)
	}
	if !containsWarning(rep.Warnings, "session "+sidA[:8]+": incomplete last line (exported from a running claude?); imported as-is under projects/"+oldSlug) || !containsWarning(rep.Warnings, "bffs rehome --bundle 6f1e2c0a --force-stamp") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	got := readFile(t, filepath.Join(dst.root.Dir, oldSlug, sidA+".jsonl"))
	if got != string(torn) {
		t.Errorf("as-is transcript = %q", got)
	}
	if exists(filepath.Join(dst.root.Dir, slugOf(t, moved))) {
		t.Error("mapped entry created for the fallback")
	}
	recs, _ := imports.Load(dst.cfgDir)
	if len(recs) != 1 || recs[0].Sessions[0].Status != imports.StatusPending || recs[0].Sessions[0].NewCwd != "" || recs[0].Sessions[0].Slug != oldSlug {
		t.Errorf("record = %+v", recs)
	}

	// ForceStamp: stamped after a separating newline, rehomed.
	dst2 := newEnv(t)
	opts = importOpts(dst2)
	opts.Map = []rehome.Mapping{{Old: oldCwd, New: moved}}
	opts.ForceStamp = true
	rep = doImport(t, dst2, data, opts)
	if !reflect.DeepEqual(rep.Rehomed, []string{sidA}) {
		t.Errorf("Rehomed = %v (warnings %v)", rep.Rehomed, rep.Warnings)
	}
	if got := readFile(t, filepath.Join(dst2.root.Dir, slugOf(t, moved), sidA+".jsonl")); got != string(torn)+"\n"+string(rehome.RelocatedRecord(sidA, moved)) {
		t.Errorf("forced transcript = %q", got)
	}

	// An overwrite under the mapped slug cannot fall back to the original
	// slug without leaving two copies: skipped, the existing one untouched.
	dst3 := newEnv(t)
	existing := filepath.Join(dst3.root.Dir, slugOf(t, moved), sidA+".jsonl")
	writeFile(t, existing, userRec(sidA, moved, "2026-08-01T10:00:00Z", "mine"))
	opts = importOpts(dst3)
	opts.Map = []rehome.Mapping{{Old: oldCwd, New: moved}}
	opts.OnConflict = ConflictOverwrite
	rep = doImport(t, dst3, data, opts)
	if !reflect.DeepEqual(rep.Skipped, []string{sidA}) || !strings.Contains(rep.Reasons[sidA], "--force-stamp") {
		t.Errorf("Skipped = %v reasons %v", rep.Skipped, rep.Reasons)
	}
	if !strings.Contains(readFile(t, existing), "mine") || exists(filepath.Join(dst3.root.Dir, oldSlug)) {
		t.Error("existing session displaced or fallback written")
	}
	if left, _ := os.ReadDir(filepath.Join(dst3.root.Dir, slugOf(t, moved))); len(left) != 1 {
		t.Errorf("set-aside left behind: %v", left)
	}
}
