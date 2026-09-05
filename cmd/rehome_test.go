package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	rehomeSidOld   = "0c5e19b2-1111-4222-8333-444455556661"
	rehomeSidNew   = "0c5e19b2-1111-4222-8333-444455556662"
	rehomeBundleID = "6f1e2c0a-1111-4222-8333-444455556666"
)

func TestParseRehomeMappings(t *testing.T) {
	dst := t.TempDir()
	dstNorm := mustNormalize(t, dst)
	cases := []struct {
		name          string
		maps          []string
		into, project string
		want          []rehome.Mapping
		wantErr       string
	}{
		{"map", []string{"/srv/old=" + dst}, "", "", []rehome.Mapping{{Old: "/srv/old", New: dstNorm}}, ""},
		{"two maps", []string{"/srv/old=" + dst, "/srv/other=" + dst}, "", "", []rehome.Mapping{{Old: "/srv/old", New: dstNorm}, {Old: "/srv/other", New: dstNorm}}, ""},
		{"into", nil, dst, "/srv/old", []rehome.Mapping{{Old: "/srv/old", New: dstNorm}}, ""},
		{"map and into", []string{"/srv/other=" + dst}, dst, "/srv/old", []rehome.Mapping{{Old: "/srv/other", New: dstNorm}, {Old: "/srv/old", New: dstNorm}}, ""},
		{"into without project", nil, dst, "", nil, "--into needs --project"},
		{"project without into", nil, "", "/srv/old", nil, "--project needs --into"},
		{"nothing", nil, "", "", nil, "nothing to map"},
		{"bad map", []string{"nope"}, "", "", nil, "expected OLD=NEW"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseRehomeMappings(c.maps, c.into, c.project)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("mapping %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// rehomeMtime is the transcript mtime rehomePool stamps: old enough to
// prove the move preserved it, young enough to sit above the retention
// floor (Claude's default window is 30 days; the floor is half of it).
var rehomeMtime = time.Now().Add(-48 * time.Hour).Truncate(time.Second)

// rehomePool lays out a home root the way an as-is import leaves it: one
// session recorded under an old directory that no longer exists here, its
// sidecar, its memory, the import record that flagged it pending, and a
// new directory to map it to. It returns the bffs config dir, the claude
// dir and both normalised paths.
func rehomePool(t *testing.T) (cfgDir, claudeDir, old, nw string) {
	t.Helper()
	for _, k := range []string{transcripts.EnvClaudeConfigDir, transcripts.EnvProjectDirName, "BFFS_ACCOUNT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	home := fakeHome(t)
	cfgDir = filepath.Join(home, "bffs")
	t.Setenv(store.EnvConfigDir, cfgDir)
	claudeDir = filepath.Join(home, ".claude")
	oldDir := filepath.Join(home, "old")
	newDir := filepath.Join(home, "src", "proj")
	for _, d := range []string{cfgDir, claudeDir, oldDir, newDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old = mustNormalize(t, oldDir)
	nw = mustNormalize(t, newDir)
	if err := os.Remove(oldDir); err != nil {
		t.Fatal(err)
	}
	slug, err := transcripts.Slug(old)
	if err != nil {
		t.Fatal(err)
	}
	line := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return string(b) + "\n"
	}
	slugDir := filepath.Join(claudeDir, "projects", slug)
	tr := filepath.Join(slugDir, rehomeSidOld+".jsonl")
	writeTestFile(t, tr, line(map[string]any{"type": "user", "cwd": old, "sessionId": rehomeSidOld, "timestamp": "2026-08-01T10:00:00Z", "message": map[string]any{"role": "user", "content": "fix the " + osc52 + "shim"}}))
	writeTestFile(t, filepath.Join(slugDir, rehomeSidOld, "tool-results", "x.txt"), "out\n")
	writeTestFile(t, filepath.Join(slugDir, "memory", "MEMORY.md"), "- [notes](notes.md) - notes\n")
	writeTestFile(t, filepath.Join(slugDir, "memory", "notes.md"), "see "+old+"/x.md\n")
	if err := os.Chtimes(tr, rehomeMtime, rehomeMtime); err != nil {
		t.Fatal(err)
	}
	rec := imports.Record{
		BundleID: rehomeBundleID, Kind: imports.KindImport, ImportedAt: time.Now(), DestRoot: filepath.Join(claudeDir, "projects"),
		Source:   imports.Source{Hostname: "mac-a", Home: "/Users/other"},
		Sessions: []imports.Session{{ID: rehomeSidOld, OldCwd: old, OldSlug: slug, Slug: slug, Title: "fix the shim", Status: imports.StatusPending, OrigMtime: rehomeMtime}},
		Memories: []imports.Memory{{OldCwd: old, Dir: filepath.Join(slugDir, "memory"), Status: imports.StatusPending}},
	}
	if err := imports.Save(cfgDir, rec); err != nil {
		t.Fatal(err)
	}
	return cfgDir, claudeDir, old, nw
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunRehomeEndToEnd(t *testing.T) {
	cfgDir, claudeDir, old, nw := rehomePool(t)
	homeJSON := filepath.Join(filepath.Dir(claudeDir), ".claude.json")
	req := rehomeRequest{Bundle: "6f1e2c0a", Maps: []string{old + "=" + nw}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), SetLastSession: true}
	newSlug, _ := transcripts.Slug(nw)
	verify := "verify: cd " + rehome.ShellQuote(nw) + " && claude --resume " + rehomeSidOld

	// Dry run: the plan with the --set-last-session effect and the verify
	// line, nothing written, a hostile title sanitised.
	c, pr, out := newTestCmd("")
	dry := req
	dry.DryRun = true
	if err := runRehome(c, cfgDir, pr, dry, false); err != nil {
		t.Fatalf("dry run: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"rehome in home:",
		"rule    " + old + " → " + short(nw),
		"move    0c5e19b2",
		"fix the shim",
		"projects/" + newSlug + " + sidecar",
		"memory  ",
		"(merge; the old directory stays)",
		"set     lastSessionId for " + short(nw) + " → 0c5e19b2 in " + short(homeJSON) + " (claude --continue there opens it)",
		"dry run: nothing is written",
		verify,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, osc52) {
		t.Errorf("escape sequence reached the output:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "projects", newSlug)); err == nil {
		t.Fatal("dry run wrote")
	}
	if _, err := os.Stat(homeJSON); err == nil {
		t.Fatal("dry run wrote .claude.json")
	}

	// Declined at the prompt: nothing written.
	c, pr, out = newTestCmd("n\n")
	if err := runRehome(c, cfgDir, pr, req, true); err != nil {
		t.Fatalf("declined run: %v", err)
	}
	if !strings.Contains(out.String(), "Rehome 1 session and 1 memory directory? [y/N] ") || !strings.Contains(out.String(), "aborted") {
		t.Errorf("prompt/abort missing:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "projects", newSlug)); err == nil {
		t.Fatal("declined run wrote")
	}
	// Piped stdin without -y is refused.
	c, pr, _ = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err == nil || !strings.Contains(err.Error(), "pass -y") {
		t.Errorf("non-tty err = %v", err)
	}

	// Accepted: moved with the stamp as the last line and its mtime kept,
	// sidecar along, memory merged and rewritten, lastSessionId set, one
	// verify line.
	c, pr, out = newTestCmd("y\n")
	if err := runRehome(c, cfgDir, pr, req, true); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	got = out.String()
	tr := filepath.Join(claudeDir, "projects", newSlug, rehomeSidOld+".jsonl")
	data, err := os.ReadFile(tr)
	if err != nil {
		t.Fatalf("transcript not moved: %v\n%s", err, got)
	}
	if !strings.HasSuffix(string(data), string(rehome.RelocatedRecord(rehomeSidOld, nw))) {
		t.Errorf("stamp missing:\n%s", data)
	}
	if st, err := os.Stat(tr); err != nil || !st.ModTime().Truncate(time.Second).Equal(rehomeMtime) {
		t.Errorf("mtime = %v, %v; want %v", st.ModTime(), err, rehomeMtime)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "projects", newSlug, rehomeSidOld, "tool-results", "x.txt")); err != nil {
		t.Error("sidecar not moved")
	}
	notes, err := os.ReadFile(filepath.Join(claudeDir, "projects", newSlug, "memory", "notes.md"))
	if err != nil || string(notes) != "see "+nw+"/x.md\n" {
		t.Errorf("memory = %q, %v", notes, err)
	}
	for _, want := range []string{
		"moved 1 session → projects/" + newSlug + " (relocated stamp appended; mtimes and picker order preserved)",
		"merged memory: 2 files (2 new); rewrote " + old + " → " + nw + " in 1 file",
		"lastSessionId → 0c5e19b2",
		verify,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Done. Check it") {
		t.Errorf("import-style footer in rehome output:\n%s", got)
	}
	// The home .claude.json now points --continue at the session.
	key, _ := transcripts.ProjectKey(nw)
	raw, err := os.ReadFile(homeJSON)
	if err != nil || !strings.Contains(string(raw), rehomeSidOld) || !strings.Contains(string(raw), key) {
		t.Errorf(".claude.json = %s, %v", raw, err)
	}

	// A second run by rule finds nothing: no session is under the old slug
	// any more. Scoped to the bundle, the record's memory entry still names
	// the old memory directory (never removed), so only a memory merge is
	// planned — idempotent, every file identical.
	again := dry
	again.Bundle = ""
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, again, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to rehome") {
		t.Errorf("second run:\n%s", out.String())
	}
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, dry, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "move    ") || !strings.Contains(got, "(merge; the old directory stays)") {
		t.Errorf("second bundle run:\n%s", got)
	}
}

func TestRunRehomeRefusalsAndScope(t *testing.T) {
	cfgDir, claudeDir, old, nw := rehomePool(t)
	// A target that does not exist is a refusal; with nothing else to move
	// the run fails with exit status 1 (dry run included).
	gone := filepath.Join(t.TempDir(), "gone")
	c, pr, out := newTestCmd("")
	req := rehomeRequest{Maps: []string{old + "=" + gone}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), DryRun: true}
	err := runRehome(c, cfgDir, pr, req, false)
	if err == nil || exitCode(err) != 1 || !strings.Contains(err.Error(), "nothing could be moved: 1 session refused") {
		t.Errorf("err = %v (exit %d)", err, exitCode(err))
	}
	if got := out.String(); !strings.Contains(got, `refused 0c5e19b2: target directory "`+gone+`" does not exist`) || strings.Contains(got, "nothing to rehome") {
		t.Errorf("output:\n%s", got)
	}
	// --session with an unknown prefix warns and finds nothing (exit 0);
	// --bundle needs a record.
	c, pr, out = newTestCmd("")
	req = rehomeRequest{Maps: []string{old + "=" + nw}, Sessions: []string{"deadbeef"}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), DryRun: true}
	if err := runRehome(c, cfgDir, pr, req, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, `warning: session "deadbeef" not found`) || !strings.Contains(got, "nothing to rehome") {
		t.Errorf("output:\n%s", got)
	}
	req.Sessions = nil
	req.Bundle = "aaaaaaaa"
	c, pr, _ = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err == nil || !strings.Contains(err.Error(), `no import record for bundle "aaaaaaaa"`) {
		t.Errorf("err = %v", err)
	}
	// --bundle scopes to the record's sessions: the record's own id finds
	// the pending session, another record would not.
	req.Bundle = "6f1e2c0a"
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "move    0c5e19b2") {
		t.Errorf("output:\n%s", got)
	}
	// Bad --memory and a missing mapping are refused up front.
	req.Bundle = ""
	req.Memory = "fork"
	c, pr, _ = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err == nil || !strings.Contains(err.Error(), `invalid --memory "fork"`) {
		t.Errorf("err = %v", err)
	}
	c, pr, _ = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, rehomeRequest{ClaudeDir: claudeDir, Cwd: t.TempDir()}, false); err == nil || !strings.Contains(err.Error(), "nothing to map") {
		t.Errorf("err = %v", err)
	}
	// An unknown --account is an error naming the known ones.
	c, pr, _ = newTestCmd("")
	req = rehomeRequest{Maps: []string{old + "=" + nw}, Account: "ghost", ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), DryRun: true}
	if err := runRehome(c, cfgDir, pr, req, false); err == nil || !strings.Contains(err.Error(), `unknown account "ghost"`) {
		t.Errorf("err = %v", err)
	}
}

func TestRunRehomeAccountMismatchWarning(t *testing.T) {
	cfgDir, claudeDir, old, nw := rehomePool(t)
	// The new directory pins another oauth account through bffs.toml while
	// the command runs from a directory with no pin: warn, unless
	// --account states the choice.
	if err := store.SaveAccounts(cfgDir, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth, Email: "w@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(nw, "bffs.toml"), "account = \"work\"\n")
	req := rehomeRequest{Maps: []string{old + "=" + nw}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), DryRun: true}
	c, pr, out := newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, `warning: claude in `+short(nw)+` runs as account "work" (project rule), not the unmanaged home (~/.claude.json): pass --account work`) {
		t.Errorf("output:\n%s", got)
	}
	req.Account = "work"
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "warning: claude in") {
		t.Errorf("warning with --account:\n%s", got)
	}
	// The verify line then names the account.
	if !strings.Contains(got, "verify: cd "+rehome.ShellQuote(nw)+" && BFFS_ACCOUNT=work claude --resume "+rehomeSidOld) {
		t.Errorf("output:\n%s", got)
	}
}

func TestRunRehomeSuggest(t *testing.T) {
	cfgDir, claudeDir, old, _ := rehomePool(t)
	// The pool's record names an old directory with no candidate here.
	c, pr, out := newTestCmd("")
	if err := runRehome(c, cfgDir, pr, rehomeRequest{Suggest: true, Bundle: "6f1e2c0a", ClaudeDir: claudeDir}, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, old+"\n") || !strings.Contains(got, "no candidate found") || !strings.Contains(got, "--map "+old+"=<dir>") {
		t.Errorf("output:\n%s", got)
	}
	// Without records there is nothing to suggest for.
	if err := os.RemoveAll(filepath.Join(cfgDir, "imports")); err != nil {
		t.Fatal(err)
	}
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, rehomeRequest{Suggest: true, ClaudeDir: claudeDir}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no import records") {
		t.Errorf("output:\n%s", out.String())
	}
	// A hostile old cwd is sanitised; without --bundle every record counts.
	rec := imports.Record{
		BundleID: rehomeBundleID, Kind: imports.KindImport, ImportedAt: time.Now(),
		Source:   imports.Source{Home: "/Users/other"},
		Sessions: []imports.Session{{ID: rehomeSidNew, OldCwd: "/Users/other/src/proj" + osc52, Status: imports.StatusPending}},
	}
	if err := imports.Save(cfgDir, rec); err != nil {
		t.Fatal(err)
	}
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, rehomeRequest{Suggest: true, ClaudeDir: claudeDir}, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, osc52) {
		t.Errorf("escape sequence reached the output:\n%q", got)
	}
	if !strings.Contains(got, "/Users/other/src/proj\n") || !strings.Contains(got, "no candidate found") {
		t.Errorf("output:\n%s", got)
	}
	// With a matching path relative to home, a numbered candidate.
	rec.Sessions[0].OldCwd = "/Users/other/src/proj"
	if err := imports.Save(cfgDir, rec); err != nil {
		t.Fatal(err)
	}
	c, pr, out = newTestCmd("")
	if err := runRehome(c, cfgDir, pr, rehomeRequest{Suggest: true, Bundle: rehomeBundleID, ClaudeDir: claudeDir}, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "[1] "+short(filepath.Join(filepath.Dir(claudeDir), "src", "proj"))+"  same path relative to home") {
		t.Errorf("output:\n%s", got)
	}
}

func TestRenderRehomeSuggestionsAligned(t *testing.T) {
	var sb strings.Builder
	renderRehomeSuggestions(&sb, []rehome.Suggestion{{OldCwd: "/old/a", Candidates: []rehome.Candidate{
		{Dir: "/x/short", Reason: "same folder name"},
		{Dir: "/x/a/much/longer/path", Reason: "same git remote"},
	}}, {OldCwd: "/old/b"}}, 1)
	got := sb.String()
	for _, want := range []string{
		"/old/a\n",
		"  [1] /x/short               same folder name\n",
		"  [2] /x/a/much/longer/path  same git remote\n",
		"\n/old/b\n  no candidate found",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRenderRehomePlan(t *testing.T) {
	root := transcripts.Root{Dir: "/x/projects", ConfigDir: "/x"}
	older := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	p := rehome.Plan{
		Root:     root,
		Mappings: []rehome.Mapping{{Old: "/old", New: "/home/j/src"}},
		Moves: []rehome.Move{
			{SessionID: rehomeSidOld, From: "/x/projects/-old/" + rehomeSidOld + ".jsonl", NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old", Title: "Fix " + osc52 + "shim", LastTS: older, Sidecar: true},
			{SessionID: testSID2, From: "/x/projects/-old/" + testSID2 + ".jsonl", NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old", Title: "Newer", LastTS: older.Add(time.Hour), SameSlug: true},
		},
		Memory: []rehome.MemoryMove{{From: "/x/projects/-old/memory" + osc52, To: "/x/projects/-home-j-src/memory", OldCwd: "/old"}},
		Refusals: []rehome.Refusal{
			{SessionID: "c0ffee00-1111-4222-8333-444455556666", Reason: "open in a running claude (pid 38445); close it, or use --fork-session there"},
			{SessionID: "d0d0d0d0-1111-4222-8333-444455556666", Reason: "incomplete last line (crashed session?); resume it once in claude or pass --force-stamp"},
		},
	}
	var sb strings.Builder
	renderRehomePlan(&sb, p, p.Mappings, "/x/.claude.json")
	got := sb.String()
	for _, want := range []string{
		"rehome in home: /x:",
		"  rule    /old → /home/j/src",
		"move    0c5e19b2  Fix shim",
		"projects/-old → projects/-home-j-src + sidecar",
		"projects/-home-j-src (already there; relocated stamp only)",
		"memory  /x/projects/-old/memory → /x/projects/-home-j-src/memory  (merge; the old directory stays)",
		"held (live): c0ffee00 (pid 38445); close it, or use --fork-session there",
		"refused d0d0d0d0: incomplete last line (crashed session?); resume it once in claude or pass --force-stamp",
		// The newer session wins lastSessionId, whatever the plan order.
		"set     lastSessionId for /home/j/src → b19c4e20 in /x/.claude.json (claude --continue there opens it)",
		"(a relocated record is appended to each transcript",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, osc52) {
		t.Errorf("escape sequence reached the output:\n%q", got)
	}
	// Without --set-last-session the pointer line is absent; refusals only
	// still render.
	sb.Reset()
	renderRehomePlan(&sb, rehome.Plan{Root: root, Refusals: p.Refusals}, p.Mappings, "")
	if got := sb.String(); strings.Contains(got, "lastSessionId") || strings.Contains(got, "relocated record is appended") || !strings.Contains(got, "held (live): c0ffee00") {
		t.Errorf("output:\n%s", got)
	}
}

func TestRenderRehomeResult(t *testing.T) {
	root := transcripts.Root{Dir: "/x/projects", ConfigDir: "/x"}
	res := rehome.Result{
		Plan: rehome.Plan{
			Root:     root,
			Mappings: []rehome.Mapping{{Old: "/old", New: "/home/j/src"}},
			Moves:    []rehome.Move{{SessionID: rehomeSidOld, NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old"}, {SessionID: rehomeSidNew, NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old"}},
			Memory:   []rehome.MemoryMove{{From: "/x/projects/-old/memory", To: "/x/projects/-home-j-src/memory", OldCwd: "/old", Mode: rehome.MemoryMerge, Added: []string{"a.md"}, Unchanged: []string{"b.md"}, Renamed: []string{"c.imported-6f1e2c0a.md" + osc52}, IndexAppended: true, Rewritten: []string{"a.md"}, Remaining: []transcripts.PathRef{{File: "a.md", Line: 1, Path: "/opt/x"}}, Warnings: []string{"MEMORY.md is 300 lines" + osc52}}},
			Refusals: []rehome.Refusal{{SessionID: "c0ffee00-1111-4222-8333-444455556666", Reason: "open in a running claude (pid 1)"}, {SessionID: "d0d0d0d0-1111-4222-8333-444455556666", Reason: "incomplete last line"}},
		},
		Moved:   []string{rehomeSidOld},
		Held:    []string{"c0ffee00-1111-4222-8333-444455556666"},
		Skipped: []string{"d0d0d0d0-1111-4222-8333-444455556666"},
		Verify:  []string{"cd /home/j/src && claude --resume " + rehomeSidOld},
	}
	var sb strings.Builder
	renderRehomeResult(&sb, res)
	got := sb.String()
	for _, want := range []string{
		"moved 1 session → projects/-home-j-src (relocated stamp appended; mtimes and picker order preserved)\n",
		"1 session not moved (see the error)\n",
		"merged memory: 3 files (1 new, 1 identical, 1 renamed c.imported-6f1e2c0a.md); MEMORY.md +1 section; rewrote /old → /home/j/src in 1 file\n",
		"  note: MEMORY.md is 300 lines\n",
		"1 memory line still mentions absolute paths: bffs memory scan-paths --project /home/j/src\n",
		"2 sessions not moved: 1 held by a running claude, 1 refused (listed above)\n",
		"verify: cd /home/j/src && claude --resume " + rehomeSidOld + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, osc52) {
		t.Errorf("escape sequence reached the output:\n%q", got)
	}

	// Plural lines, the lastSessionId pointer, two memory directories named
	// by project, and the other memory modes.
	res = rehome.Result{
		Plan: rehome.Plan{
			Root:     root,
			Mappings: []rehome.Mapping{{Old: "/old", New: "/home/j/src"}, {Old: "/other", New: "/home/j/other"}},
			Moves:    []rehome.Move{{SessionID: rehomeSidOld, NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old"}, {SessionID: rehomeSidNew, NewSlug: "-home-j-src", NewCwd: "/home/j/src", OldCwd: "/old"}},
			Memory: []rehome.MemoryMove{
				{From: "/x/projects/-old/memory", To: "/x/projects/-home-j-src/memory", OldCwd: "/old", Mode: rehome.MemorySkip, Unchanged: []string{"a.md", "b.md"}},
				{From: "/x/projects/-other/memory", To: "/x/projects/-home-j-other/memory", OldCwd: "/other", Mode: rehome.MemoryOverwrite, Added: []string{"MEMORY.md", "z.md"}, Remaining: []transcripts.PathRef{{File: "z.md", Line: 1, Path: "/opt/x"}, {File: "z.md", Line: 2, Path: "/opt/y"}}},
			},
		},
		Moved:          []string{rehomeSidOld, rehomeSidNew},
		LastSessionSet: rehomeSidNew,
		Verify:         []string{"cd /home/j/src && claude --resume " + rehomeSidNew},
	}
	sb.Reset()
	renderRehomeResult(&sb, res)
	got = sb.String()
	for _, want := range []string{
		"moved 2 sessions → projects/-home-j-src (relocated stamp appended; mtimes and picker order preserved)\n",
		"kept memory for /home/j/src: 2 files (2 identical, --memory skip)\n",
		"replaced memory for /home/j/other: 2 files (2 new, the previous directory set aside, never deleted)\n",
		"2 memory lines still mention absolute paths: bffs memory scan-paths --project /home/j/other\n",
		"lastSessionId → 0c5e19b2 (claude --continue opens it)\n",
		"verify: cd /home/j/src && claude --resume " + rehomeSidNew + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not moved") {
		t.Errorf("stray not-moved line:\n%s", got)
	}
}

// Without knowing which sessions a running claude owns nothing may move:
// a liveness scan that fails is an error, not a warning.
func TestRunRehomeRefusesWithoutLiveness(t *testing.T) {
	cfgDir, claudeDir, old, nw := rehomePool(t)
	writeTestFile(t, filepath.Join(claudeDir, transcripts.RuntimeSessionsSubdir), "not a directory\n")
	c, pr, out := newTestCmd("")
	req := rehomeRequest{Maps: []string{old + "=" + nw}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), Yes: true}
	err := runRehome(c, cfgDir, pr, req, false)
	if err == nil || !strings.Contains(err.Error(), "cannot tell which sessions are open in a running claude") {
		t.Errorf("err = %v\n%s", err, out.String())
	}
	newSlug, _ := transcripts.Slug(nw)
	if _, err := os.Stat(filepath.Join(claudeDir, "projects", newSlug)); err == nil {
		t.Error("moved without a liveness answer")
	}
}

func TestRunRehomeRewriteFlags(t *testing.T) {
	for _, name := range []string{"rewrite-cwd", "rewrite-file-history"} {
		fl := rehomeCmd.Flags().Lookup(name)
		if fl == nil || fl.DefValue != "false" || !strings.Contains(fl.Usage, "historical records") {
			t.Errorf("--%s: %+v", name, fl)
		}
	}
	cfgDir, claudeDir, old, nw := rehomePool(t)
	oldSlug, _ := transcripts.Slug(old)
	newSlug, _ := transcripts.Slug(nw)
	tr := filepath.Join(claudeDir, "projects", oldSlug, rehomeSidOld+".jsonl")
	data, err := os.ReadFile(tr)
	if err != nil {
		t.Fatal(err)
	}
	delta, _ := json.Marshal(map[string]any{"type": "file-history-delta", "messageId": "m", "trackingPath": old + "/x.txt", "backup": map[string]any{"backupFileName": "x@v1", "version": 1, "realParentDir": old}})
	nested, _ := json.Marshal(map[string]any{"type": "assistant", "cwd": old + "/sub", "message": map[string]any{"content": []any{map[string]any{"input": map[string]any{"cwd": old}}}}})
	writeTestFile(t, tr, string(data)+string(delta)+"\n"+string(nested)+"\n")
	if err := os.Chtimes(tr, rehomeMtime, rehomeMtime); err != nil {
		t.Fatal(err)
	}

	req := rehomeRequest{Bundle: "6f1e2c0a", Maps: []string{old + "=" + nw}, ClaudeDir: claudeDir, Cwd: t.TempDir(), Now: time.Now(), Yes: true, RewriteCwd: true, RewriteFileHistory: true}
	c, pr, out := newTestCmd("")
	if err := runRehome(c, cfgDir, pr, req, false); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	moved := filepath.Join(claudeDir, "projects", newSlug, rehomeSidOld+".jsonl")
	got, err := os.ReadFile(moved)
	if err != nil {
		t.Fatalf("transcript not moved: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d:\n%s", len(lines), got)
	}
	newQ, _ := json.Marshal(nw)
	oldQ, _ := json.Marshal(old)
	if !strings.Contains(lines[0], `"cwd":`+string(newQ)) || strings.Contains(lines[0], string(oldQ)) {
		t.Errorf("top-level cwd not rewritten: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"trackingPath":`+strings.TrimSuffix(string(newQ), `"`)+`/x.txt"`) || !strings.Contains(lines[1], `"realParentDir":`+string(newQ)) {
		t.Errorf("file-history paths not rewritten: %s", lines[1])
	}
	if lines[2] != string(nested) {
		t.Errorf("record with another cwd and a nested cwd changed:\n got %s\nwant %s", lines[2], nested)
	}
	if lines[3] != strings.TrimSuffix(string(rehome.RelocatedRecord(rehomeSidOld, nw)), "\n") {
		t.Errorf("stamp = %s", lines[3])
	}
	if st, err := os.Stat(moved); err != nil || !st.ModTime().Truncate(time.Second).Equal(rehomeMtime) {
		t.Errorf("mtime = %v, %v; want %v", st.ModTime(), err, rehomeMtime)
	}
	if want := "rewrote cwd in 1 record and 2 file-history paths across 1 transcript"; !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
}
