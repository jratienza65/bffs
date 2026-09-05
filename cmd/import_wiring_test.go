package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// TestImportMappedEndToEnd exports a project, moves the directory away so
// the bundle's cwd no longer exists here, and imports it with a --map rule
// into a full-isolation account: the transcript lands under the new slug
// with the relocated record as its last line, memory is merged (confirmed
// placement), and --carry-trust / --set-last-session write the account's
// .claude.json.
func TestImportMappedEndToEnd(t *testing.T) {
	a := newExportFixture(t)
	home := filepath.Dir(a.cfgDir)
	trust := map[string]any{"projects": map[string]any{a.project: map[string]any{
		"hasTrustDialogAccepted":                  true,
		"hasClaudeMdExternalIncludesApproved":     true,
		"hasClaudeMdExternalIncludesWarningShown": true,
	}}}
	b, _ := json.Marshal(trust)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "a.bffs")
	c, pr, _, errOut := newSplitCmd("")
	if err := runExport(c, a.cfgDir, pr, a.request(file), false); err != nil {
		t.Fatalf("export: %v\n%s", err, errOut.String())
	}
	moved := a.project + "-moved"
	if err := os.Rename(a.project, moved); err != nil {
		t.Fatal(err)
	}
	moved, err := store.NormalizePath(moved)
	if err != nil {
		t.Fatal(err)
	}

	m := newImportMachine(t)
	if err := store.SaveAccounts(m.cfgDir, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth, Isolation: store.IsolationFull}}}); err != nil {
		t.Fatal(err)
	}
	workDir := sessions.Dir(m.cfgDir, "work")
	if err := os.MkdirAll(filepath.Join(workDir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, ".claude.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := m.request(file)
	req.Account = "work"
	req.Map = []string{a.project + "=" + moved}
	req.Memory = "merge"
	req.CarryTrust = true
	req.SetLastSession = true
	c, pr, out, errOut := newSplitCmd("")
	if err := runImport(c, m.cfgDir, pr, req, false); err != nil {
		t.Fatalf("import: %v\n%s\n%s", err, out.String(), errOut.String())
	}

	slug, err := transcripts.Slug(moved)
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(workDir, "projects", slug, testSID1+".jsonl")
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("mapped transcript: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var last struct {
		Type         string `json:"type"`
		SessionID    string `json:"sessionId"`
		RelocatedCwd string `json:"relocatedCwd"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != "relocated" || last.SessionID != testSID1 || last.RelocatedCwd != moved {
		t.Errorf("last line = %q (%v), want a relocated record for %s", lines[len(lines)-1], err, moved)
	}
	for _, want := range []string{
		"exists here ✗ → " + short(moved) + " (relocated record appended)",
		"→ sessions will be placed under projects/" + slug + "/ (relocated record appended)",
		"→ marks " + short(moved) + " trusted for \"work\"",
		"→ sets the last-session pointer for " + short(moved) + " in \"work\"",
		"(relocated → " + short(moved) + ")",
		"trust      carried over for " + short(moved) + " → \"work\"",
		"last-session pointer set for " + short(moved) + " in \"work\"",
		"merged into",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Memory merged under its own names, pinned neutralised, index kept.
	memDir := filepath.Join(workDir, "projects", slug, "memory")
	if _, err := os.Stat(filepath.Join(memDir, "MEMORY.md")); err != nil {
		t.Errorf("merged MEMORY.md: %v", err)
	}
	topic, err := os.ReadFile(filepath.Join(memDir, "topic.md"))
	if err != nil {
		t.Errorf("merged topic.md: %v", err)
	} else if !strings.Contains(string(topic), "pinned-imported: true") || strings.Contains(string(topic), "\npinned: true") {
		t.Errorf("pinned frontmatter not neutralised:\n%s", topic)
	}

	// The account's .claude.json carries the trust answers and the pointer
	// for the mapped directory only.
	flags, err := claudejson.ReadProjectFlags(filepath.Join(workDir, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := transcripts.ProjectKey(moved)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := flags[key]
	if !ok || f.TrustAccepted == nil || !*f.TrustAccepted || f.ExternalIncludesApproved == nil || !*f.ExternalIncludesApproved {
		t.Errorf("trust flags for %s = %+v (present %v)", key, f, ok)
	}
	if f.LastSessionID != testSID1 {
		t.Errorf("lastSessionId = %q, want %s", f.LastSessionID, testSID1)
	}
	if _, ok := flags[a.project]; ok {
		t.Errorf("the old directory must not gain an entry")
	}
}

// TestPlacementAsker drives the interactive prompt with piped answers.
func TestPlacementAsker(t *testing.T) {
	home := fakeHome(t)
	dir := filepath.Join(home, "src", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "/Users/elsewhere/proj"
	m := &bundle.Manifest{BundleID: testBundleID, Source: bundle.Source{Hostname: "mac-a", Home: "/Users/elsewhere"}, Entries: []bundle.Entry{
		{Kind: bundle.EntrySession, Slug: "-Users-elsewhere-proj", Cwd: old, SessionID: testSID1, Files: []bundle.File{{Path: "projects/-Users-elsewhere-proj/" + testSID1 + ".jsonl", Size: 10}}},
	}}
	dest := importDest{root: transcripts.Root{Dir: filepath.Join(home, ".claude", "projects"), ConfigDir: filepath.Join(home, ".claude")}, label: "home"}

	run := func(stdin string) (string, []string) {
		c, pr, _, errOut := newSplitCmd(stdin)
		_ = c
		sum := newImportSummary(m, dest, importRequest{})
		var sb strings.Builder
		rules := renderImportSummaryAsk(&sb, &sum, newPlacementAsker(pr, true))
		var got []string
		for _, r := range rules {
			got = append(got, r.Old+"="+r.New)
		}
		return sb.String() + errOut.String(), got
	}

	// The directory of the same name is offered first; then a typed path,
	// then as-is.
	want, _ := store.NormalizePath(dir)
	out, rules := run("1\n")
	if len(rules) != 1 || rules[0] != old+"="+want {
		t.Errorf("candidate: rules = %v\n%s", rules, out)
	}
	for _, s := range []string{"where does this project live on this machine?", "[1] " + short(dir), "(same folder name)", "[2] type a path", "[3] import as-is", "→ sessions will be placed under projects/"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}

	out, rules = run("2\n" + dir + "\n")
	if len(rules) != 1 || rules[0] != old+"="+want {
		t.Errorf("typed path: rules = %v\n%s", rules, out)
	}

	out, rules = run("3\n")
	if len(rules) != 0 || !strings.Contains(out, "→ imported as-is under projects/-Users-elsewhere-proj/") {
		t.Errorf("as-is: rules = %v\n%s", rules, out)
	}

	// Three unusable answers fall back to as-is; a bad path counts.
	out, rules = run("9\n2\n/definitely/not/here\nx\n")
	if len(rules) != 0 || !strings.Contains(out, "no usable answer; importing as-is") {
		t.Errorf("fallback: rules = %v\n%s", rules, out)
	}

	// An empty answer picks the default [1].
	out, rules = run("\n")
	if len(rules) != 1 {
		t.Errorf("default answer: rules = %v\n%s", rules, out)
	}

	// Inactive (no terminal, -y, --as-is, --dry-run): no question, as-is.
	var sb strings.Builder
	sum := newImportSummary(m, dest, importRequest{})
	mapped := renderImportSummaryAsk(&sb, &sum, newPlacementAsker(nil, false))
	if len(mapped) != 0 || strings.Contains(sb.String(), "where does this project live") {
		t.Errorf("inactive asker asked:\n%s", sb.String())
	}
}
