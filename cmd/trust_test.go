package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// setHome points HOME (and USERPROFILE for Windows) at a fresh directory so
// short() and claudejson.Path() stay inside the test.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// projectDir returns a fresh directory as Claude would key it.
func projectDir(t *testing.T) string {
	t.Helper()
	key, err := store.NormalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// writeClaudeJSON writes a .claude.json with the given projects map and a
// couple of unrelated top-level fields that must survive a sync.
func writeClaudeJSON(t *testing.T, path string, projects map[string]any) {
	t.Helper()
	doc := map[string]any{
		"userID":   "u-1",
		"theme":    "dark",
		"projects": projects,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readProjects(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]map[string]any `json:"projects"`
		UserID   string                    `json:"userID"`
		Theme    string                    `json:"theme"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if doc.UserID != "u-1" || doc.Theme != "dark" {
		t.Errorf("unrelated fields not preserved in %s: %+v", path, doc)
	}
	return doc.Projects
}

// twoAccountEnv builds a trustEnv with oauth accounts aviate (active) and
// innomind plus home, all files under temp dirs, nothing written yet.
func twoAccountEnv(t *testing.T) *trustEnv {
	t.Helper()
	home := setHome(t)
	cfg := t.TempDir()
	accs := store.Accounts{Accounts: map[string]store.Account{
		"aviate":   {Type: store.TypeOAuth},
		"innomind": {Type: store.TypeOAuth},
		"keyed":    {Type: store.TypeAPIKey, Secret: "sk-ant-secret"},
	}}
	files, err := trust.Files(cfg, filepath.Join(home, claudejson.Filename), accs)
	if err != nil {
		t.Fatal(err)
	}
	return &trustEnv{cfgDir: cfg, homeJSON: files[trust.HomeName], accs: accs, files: files, activeRow: "aviate"}
}

func acceptedEntry() map[string]any {
	return map[string]any{
		"allowedTools":                            []string{"Bash(go test:*)", "Read"},
		"hasTrustDialogAccepted":                  true,
		"hasClaudeMdExternalIncludesApproved":     true,
		"hasClaudeMdExternalIncludesWarningShown": true,
		"lastSessionId":                           "sid-aviate",
		"enabledMcpjsonServers":                   []string{"bffs"},
		"mcpServers":                              map[string]any{"bffs": map[string]any{"type": "stdio", "command": "/opt/bffs/bffs", "args": []string{"mcp", "serve"}, "env": map[string]string{"BFFS_TEST_TOKEN": "hunter2-not-for-terminals"}}},
	}
}

func sampleBlock(key string) trustBlock {
	return trustBlock{
		Key: key,
		Statuses: []trust.Status{
			{Account: "aviate", ProjectKey: key, Present: true, Folder: trust.Accepted, External: trust.Accepted, Tools: 3, MCPEnabled: 2},
			{Account: "innomind", ProjectKey: key},
			{Account: "innomind2", ProjectKey: key, Present: true, Folder: trust.Inherited, InheritedFrom: filepath.Dir(key), External: trust.Declined},
			{Account: trust.HomeName, ProjectKey: key, Present: true, Folder: trust.Accepted, External: trust.Accepted, Tools: 3, MCPEnabled: 2},
		},
		Live: []liveLine{{PID: 38445, SessionID: "sid-1", Cwd: key}},
	}
}

func TestRenderTrustMatrix(t *testing.T) {
	home := setHome(t)
	key := filepath.Join(home, "build", "projects", "bffs")
	var sb strings.Builder
	renderTrustMatrix(&sb, sampleBlock(key), "aviate")
	out := sb.String()

	for _, want := range []string{
		"project:  " + filepath.Join("~", "build", "projects", "bffs"),
		"(key: " + key + ")",
		"ACCOUNT", "FOLDER-TRUST", "EXTERNAL-IMPORTS", "TOOLS", "MCP",
		"* aviate", "accepted", "allowed", "2 enabled",
		"inherited (from " + filepath.Dir(key) + ")", "declined",
		"(home)",
		"live: pid 38445 in " + filepath.Join("~", "build", "projects", "bffs") + " (shared runtime dir — account not determinable); a running claude picks changes up within about a second",
		`"-" = never answered on that account (claude will ask)`,
		"bffs trust sync --to innomind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("matrix missing %q:\n%s", want, out)
		}
	}
	// The unanswered row is all dashes and not marked active.
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  innomind ") {
			found = true
			if f := strings.Fields(line); len(f) != 5 || f[1] != "-" || f[2] != "-" || f[3] != "-" || f[4] != "-" {
				t.Errorf("innomind row = %q, want four dashes", line)
			}
		}
	}
	if !found {
		t.Errorf("no innomind row:\n%s", out)
	}
	if strings.Contains(out, "* innomind") {
		t.Errorf("only the active row carries *:\n%s", out)
	}
}

func TestRenderTrustMatrixLiveOwnerAndNothingMissing(t *testing.T) {
	home := setHome(t)
	key := filepath.Join(home, "p")
	b := trustBlock{
		Key: key,
		Statuses: []trust.Status{
			{Account: "aviate", Present: true, Folder: trust.Accepted, External: trust.Accepted},
			{Account: trust.HomeName, Present: true, Folder: trust.Accepted, External: trust.Accepted},
		},
		Live: []liveLine{{PID: 7, Cwd: key, Owner: "aviate"}},
	}
	var sb strings.Builder
	renderTrustMatrix(&sb, b, trust.HomeName)
	out := sb.String()
	if !strings.Contains(out, "live: pid 7 in "+filepath.Join("~", "p")+` (account "aviate")`) {
		t.Errorf("owned live line missing:\n%s", out)
	}
	if !strings.Contains(out, "* (home)") {
		t.Errorf("home row should be marked active for an api_key active account:\n%s", out)
	}
	for _, absent := range []string{"bffs trust sync", "never answered"} {
		if strings.Contains(out, absent) {
			t.Errorf("nothing is missing, yet output has %q:\n%s", absent, out)
		}
	}
}

func TestRenderTrustJSON(t *testing.T) {
	setHome(t)
	key := projectDir(t)
	var sb strings.Builder
	if err := renderTrustJSON(&sb, []trustBlock{sampleBlock(key), {Key: key + "2"}}); err != nil {
		t.Fatal(err)
	}
	var got []trustJSONBlock
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, sb.String())
	}
	if len(got) != 2 || got[0].ProjectKey != key {
		t.Fatalf("blocks = %+v", got)
	}
	rows := got[0].Accounts
	if len(rows) != 4 || rows[0].Account != "aviate" || rows[0].Folder != "accepted" || rows[0].External != "accepted" || rows[0].Tools != 3 || rows[0].MCPEnabled != 2 {
		t.Errorf("aviate row = %+v", rows[0])
	}
	if rows[1].Folder != "unset" || rows[1].Present {
		t.Errorf("innomind row = %+v", rows[1])
	}
	if rows[2].Folder != "inherited" || rows[2].InheritedFrom != filepath.Dir(key) || rows[2].External != "declined" {
		t.Errorf("innomind2 row = %+v", rows[2])
	}
	if len(got[0].Live) != 1 || got[0].Live[0].PID != 38445 || got[0].Live[0].Cwd != key {
		t.Errorf("live = %+v", got[0].Live)
	}
	// An empty block still encodes arrays, never null.
	if !strings.Contains(sb.String(), `"accounts": []`) || !strings.Contains(sb.String(), `"live": []`) {
		t.Errorf("empty block should carry empty arrays:\n%s", sb.String())
	}
}

func TestTrustSyncTargets(t *testing.T) {
	env := twoAccountEnv(t)
	all, err := trustSyncTargets(env, "aviate", trustSyncAll)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"innomind", trust.HomeName}; strings.Join(all, ",") != strings.Join(want, ",") {
		t.Errorf("--to all = %v, want %v", all, want)
	}
	if _, err := trustSyncTargets(env, "", "keyed"); err == nil || !strings.Contains(err.Error(), "--to home") || strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("api_key target: %v", err)
	}
	if _, err := trustSyncTargets(env, "", "ghost"); err == nil || !strings.Contains(err.Error(), `unknown account "ghost"`) {
		t.Errorf("unknown target: %v", err)
	}
	if _, err := trustSyncTargets(env, "aviate", "aviate"); err == nil || !strings.Contains(err.Error(), "nothing to copy") {
		t.Errorf("same from/to: %v", err)
	}
	if err := trustRowExists(env, "--from", "keyed"); err == nil || !strings.Contains(err.Error(), "--from home") {
		t.Errorf("api_key source should point at --from home: %v", err)
	}
}

func TestProjectKeysIn(t *testing.T) {
	setHome(t)
	exists := projectDir(t)
	gone := filepath.Join(t.TempDir(), "gone")
	path := filepath.Join(t.TempDir(), claudejson.Filename)
	writeClaudeJSON(t, path, map[string]any{exists: acceptedEntry(), gone: acceptedEntry()})
	keys, missing, err := projectKeysIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != exists {
		t.Errorf("keys = %v", keys)
	}
	if len(missing) != 1 || missing[0] != gone {
		t.Errorf("missing = %v", missing)
	}
	if k, m, err := projectKeysIn(filepath.Join(t.TempDir(), "absent.json")); err != nil || len(k) != 0 || len(m) != 0 {
		t.Errorf("missing file: %v %v %v", k, m, err)
	}
}

// syncFixture seeds aviate as an accepting source and returns the project
// key; innomind and home start with no file at all.
func syncFixture(t *testing.T, env *trustEnv) string {
	t.Helper()
	key := projectDir(t)
	writeClaudeJSON(t, env.files["aviate"], map[string]any{key: acceptedEntry()})
	return key
}

func TestTrustSyncConfirmYes(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	c, pr, out := newTestCmd("y\n")
	err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}}, true)
	if err != nil {
		t.Fatalf("runTrustSync: %v", err)
	}
	for _, want := range []string{
		`project ` + key + ` → account "innomind" (from "aviate"):`,
		"hasTrustDialogAccepted", "unset -> true",
		"hasClaudeMdExternalIncludesApproved",
		"hasClaudeMdExternalIncludesWarningShown",
		"apply to " + env.files["innomind"] + "? [y/N] ",
		`updated 1 project in "innomind". A running claude on that account picks the change up within about a second.`,
		"(allowedTools and MCP approvals were not copied; add --include-permissions to include them.)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	got := readProjectsLoose(t, env.files["innomind"])[key]
	if got["hasTrustDialogAccepted"] != true || got["hasClaudeMdExternalIncludesApproved"] != true || got["hasClaudeMdExternalIncludesWarningShown"] != true {
		t.Errorf("target entry = %v", got)
	}
	if _, ok := got["lastSessionId"]; ok && got["lastSessionId"] == "sid-aviate" {
		t.Errorf("lastSessionId must never be copied: %v", got)
	}
	if tools, _ := got["allowedTools"].([]any); len(tools) != 0 {
		t.Errorf("allowedTools copied without --include-permissions: %v", got)
	}
}

func TestTrustSyncConfirmNo(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	c, pr, out := newTestCmd("n\n")
	if err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}}, true); err != nil {
		t.Fatalf("runTrustSync: %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("declining should print aborted:\n%s", out.String())
	}
	if _, err := os.Stat(env.files["innomind"]); !os.IsNotExist(err) {
		t.Errorf("declined sync must not create the target file (stat err = %v)", err)
	}
}

func TestTrustSyncNonInteractiveNeedsYes(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	c, pr, _ := newTestCmd("")
	err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}}, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to write without -y in a non-interactive session") {
		t.Fatalf("want the non-TTY refusal, got %v", err)
	}
	if _, err := os.Stat(env.files["innomind"]); !os.IsNotExist(err) {
		t.Errorf("refusal must happen before any write")
	}

	// -y writes without asking, even without a terminal.
	c, pr, out := newTestCmd("")
	if err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}, Yes: true}, false); err != nil {
		t.Fatalf("with -y: %v", err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("-y must not prompt:\n%s", out.String())
	}
	if readProjectsLoose(t, env.files["innomind"])[key]["hasTrustDialogAccepted"] != true {
		t.Errorf("-y did not apply")
	}
}

func TestTrustSyncDryRun(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	c, pr, out := newTestCmd("y\n")
	if err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind", trust.HomeName}, Keys: []string{key}, DryRun: true}, false); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out.String(), "dry run: nothing written") || strings.Contains(out.String(), "[y/N]") {
		t.Errorf("dry run output:\n%s", out.String())
	}
	for _, name := range []string{"innomind", trust.HomeName} {
		if _, err := os.Stat(env.files[name]); !os.IsNotExist(err) {
			t.Errorf("dry run wrote %s", env.files[name])
		}
	}
}

func trustSyncAllTargets(env *trustEnv, from string) []string {
	out, _ := trustSyncTargets(env, from, trustSyncAll)
	return out
}

func TestTrustSyncNoChangesAndNoSource(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	// Target already matches the source.
	writeClaudeJSON(t, env.files["innomind"], map[string]any{key: acceptedEntry()})
	c, pr, out := newTestCmd("")
	if err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}}, false); err != nil {
		t.Fatalf("no-op sync: %v", err)
	}
	if strings.TrimSpace(out.String()) != "no changes" {
		t.Errorf("want just 'no changes', got %q", out.String())
	}
	// Nobody trusts a fresh project → no default source.
	other := projectDir(t)
	c, pr, _ = newTestCmd("")
	err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{other}}, false)
	if err == nil || !strings.Contains(err.Error(), "no account has answered the folder-trust dialog") {
		t.Errorf("want the no-source error, got %v", err)
	}
	// An explicit --from that does not know the project is simply empty.
	c, pr, out = newTestCmd("")
	if err := runTrustSync(c, pr, env, syncRequest{From: "aviate", To: []string{"innomind"}, Keys: []string{other}}, false); err != nil || !strings.Contains(out.String(), "no changes") {
		t.Errorf("explicit unknown source: err=%v out=%q", err, out.String())
	}
}

func TestTrustSyncIncludePermissions(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	c, pr, out := newTestCmd("y\n")
	if err := runTrustSync(c, pr, env, syncRequest{To: []string{"innomind"}, Keys: []string{key}, IncludePermissions: true}, true); err != nil {
		t.Fatalf("runTrustSync: %v", err)
	}
	for _, want := range []string{
		`mcpServers["bffs"]: (stdio) /opt/bffs/bffs mcp serve`,
		"this copies allowedTools and MCP server approvals; each mcpServers command line is printed before applying",
		"allowedTools",
		"unset -> {bffs}",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "were not copied") {
		t.Errorf("the not-copied note must not appear with --include-permissions:\n%s", out.String())
	}
	// A server definition's env block never reaches the terminal: the change
	// line names the servers, the command lines are printed separately.
	if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "BFFS_TEST_TOKEN") {
		t.Errorf("mcpServers env printed:\n%s", out.String())
	}
	got := readProjectsLoose(t, env.files["innomind"])[key]
	if tools, _ := got["allowedTools"].([]any); len(tools) != 2 {
		t.Errorf("allowedTools not copied: %v", got)
	}
	if _, ok := got["mcpServers"].(map[string]any)["bffs"]; !ok {
		t.Errorf("mcpServers not copied: %v", got)
	}
}

func TestTrustSyncFanOutAll(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	// home already declined external imports: it keeps that, gains folder trust only.
	writeClaudeJSON(t, env.files[trust.HomeName], map[string]any{key: map[string]any{
		"hasTrustDialogAccepted":                  false,
		"hasClaudeMdExternalIncludesApproved":     false,
		"hasClaudeMdExternalIncludesWarningShown": true,
	}})
	c, pr, out := newTestCmd("y\ny\n")
	targets := trustSyncAllTargets(env, "")
	if err := runTrustSync(c, pr, env, syncRequest{To: targets, Keys: []string{key}}, true); err != nil {
		t.Fatalf("runTrustSync: %v", err)
	}
	if !strings.Contains(out.String(), `updated 1 project in "innomind"`) || !strings.Contains(out.String(), `updated 1 project in "home". A running unmanaged claude`) {
		t.Errorf("fan-out output:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"aviate"`) && !strings.Contains(out.String(), `(from "aviate")`) || strings.Contains(out.String(), `no changes for "aviate"`) {
		t.Errorf("the source must not be a target, not even as a no-op:\n%s", out.String())
	}
	home := readProjects(t, env.files[trust.HomeName])[key]
	if home["hasTrustDialogAccepted"] != true || home["hasClaudeMdExternalIncludesApproved"] != false {
		t.Errorf("home: declined external imports must survive, folder trust must arrive: %v", home)
	}
}

// readProjectsLoose reads the projects map of a file that may have been
// created by a sync (so lacks the fixture's unrelated fields).
func readProjectsLoose(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Projects
}

func TestTrustHint(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)

	hint := trustHint(env.files, "innomind", key)
	for _, want := range []string{
		`note: "innomind" hasn't answered the folder-trust / external-imports dialogs for `,
		`("aviate" has).`,
		"Carry them over:  bffs trust sync --to innomind",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	// An api_key account reads the home file: the hint targets home.
	if h := trustHint(env.files, trustRowFor(env.files, env.accs, "keyed"), key); !strings.Contains(h, `note: "home" hasn't answered`) || !strings.Contains(h, "--to home") {
		t.Errorf("api_key hint = %q", h)
	}
	// Only one dialog missing → singular wording.
	writeClaudeJSON(t, env.files["innomind"], map[string]any{key: map[string]any{"hasTrustDialogAccepted": true}})
	if h := trustHint(env.files, "innomind", key); !strings.Contains(h, "the external-imports dialog for") {
		t.Errorf("single-dialog hint = %q", h)
	}
	// Everything answered → no note. The source itself never gets one.
	writeClaudeJSON(t, env.files["innomind"], map[string]any{key: acceptedEntry()})
	if h := trustHint(env.files, "innomind", key); h != "" {
		t.Errorf("answered account got a hint: %q", h)
	}
	if h := trustHint(env.files, "aviate", key); h != "" {
		t.Errorf("source got a hint: %q", h)
	}
	if h := trustHint(env.files, "", key); h != "" {
		t.Errorf("unknown row got a hint: %q", h)
	}
	// A project nobody knows → nothing to carry, no note.
	if h := trustHint(env.files, "innomind", projectDir(t)); h != "" {
		t.Errorf("unknown project got a hint: %q", h)
	}
	f := false
	if trustHintEnabled(store.State{TrustHint: &f}) || !trustHintEnabled(store.State{}) {
		t.Error("trust_hint: nil and true show, false hides")
	}
}

func TestSeedClaudeJSONOnce(t *testing.T) {
	home := setHome(t)
	if err := os.WriteFile(filepath.Join(home, claudejson.Filename), []byte(`{"userID":"u-home","hasCompletedOnboarding":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "s", claudejson.Filename)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}

	seeded, err := seedClaudeJSONOnce(target)
	if err != nil || !seeded {
		t.Fatalf("first seed: seeded=%v err=%v", seeded, err)
	}
	first, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "hasCompletedOnboarding") || strings.Contains(string(first), "u-home") {
		t.Errorf("seed should carry wizard markers and drop identity: %s", first)
	}

	// Simulate synced answers landing in the file; a re-login must keep them.
	synced := []byte(`{"projects":{"/p":{"hasTrustDialogAccepted":true}}}`)
	if err := os.WriteFile(target, synced, 0o600); err != nil {
		t.Fatal(err)
	}
	seeded, err = seedClaudeJSONOnce(target)
	if err != nil || seeded {
		t.Fatalf("second seed: seeded=%v err=%v", seeded, err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(synced) {
		t.Errorf("existing file rewritten:\n%s", after)
	}
}

func TestCarryTrustOnLogin(t *testing.T) {
	env := twoAccountEnv(t)
	exists := projectDir(t)
	gone := filepath.Join(t.TempDir(), "gone")
	writeClaudeJSON(t, env.files["aviate"], map[string]any{exists: acceptedEntry(), gone: acceptedEntry()})
	writeClaudeJSON(t, env.files["innomind"], map[string]any{})

	n, src, err := carryTrustOnLogin(env.cfgDir, env.homeJSON, env.accs, "aviate", "innomind")
	if err != nil {
		t.Fatalf("carry: %v", err)
	}
	if n != 1 || src != "aviate" {
		t.Errorf("n=%d src=%q, want 1 from aviate", n, src)
	}
	got := readProjects(t, env.files["innomind"])
	if got[exists]["hasTrustDialogAccepted"] != true {
		t.Errorf("existing project not carried: %v", got)
	}
	if _, ok := got[gone]; ok {
		t.Errorf("a project whose directory is gone must not be created: %v", got)
	}

	// Second run: nothing left to add.
	if n, _, err := carryTrustOnLogin(env.cfgDir, env.homeJSON, env.accs, "aviate", "innomind"); err != nil || n != 0 {
		t.Errorf("re-carry: n=%d err=%v", n, err)
	}
	// No previous account, or the account itself → home is the source.
	if _, src, err := carryTrustOnLogin(env.cfgDir, env.homeJSON, env.accs, "", "innomind"); err != nil || src != trust.HomeName {
		t.Errorf("no prevActive: src=%q err=%v", src, err)
	}
	if _, src, err := carryTrustOnLogin(env.cfgDir, env.homeJSON, env.accs, "innomind", "innomind"); err != nil || src != trust.HomeName {
		t.Errorf("self as prevActive: src=%q err=%v", src, err)
	}
	if _, src, err := carryTrustOnLogin(env.cfgDir, env.homeJSON, env.accs, "keyed", "innomind"); err != nil || src != trust.HomeName {
		t.Errorf("api_key prevActive: src=%q err=%v", src, err)
	}
}

func TestTrustBlockForAndLiveOwner(t *testing.T) {
	env := twoAccountEnv(t)
	key := syncFixture(t, env)
	live := []transcripts.LiveSession{
		{PID: 1, SessionID: "in", Cwd: filepath.Join(key, "sub"), ConfigDir: sessions.Dir(env.cfgDir, "aviate")},
		{PID: 2, SessionID: "out", Cwd: t.TempDir(), ConfigDir: sessions.Dir(env.cfgDir, "aviate")},
		{PID: 3, SessionID: "nocwd", ConfigDir: sessions.Dir(env.cfgDir, "aviate")},
	}
	b, err := trustBlockFor(env, key, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Statuses) != 3 || b.Statuses[0].Account != "aviate" || b.Statuses[2].Account != trust.HomeName {
		t.Errorf("statuses = %+v", b.Statuses)
	}
	if len(b.Live) != 1 || b.Live[0].SessionID != "in" {
		t.Fatalf("only the session inside the project belongs to the block: %+v", b.Live)
	}
	// aviate's own sessions/ dir is real (full isolation) → attributable.
	if err := os.MkdirAll(filepath.Join(sessions.Dir(env.cfgDir, "aviate"), transcripts.RuntimeSessionsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	if owner := liveOwner(env.files, live[0]); owner != "aviate" {
		t.Errorf("owner = %q, want aviate", owner)
	}
	if runtimeDirsCanShare() {
		// innomind's sessions/ symlinked to aviate's → shared, undeterminable.
		if err := os.MkdirAll(sessions.Dir(env.cfgDir, "innomind"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(sessions.Dir(env.cfgDir, "aviate"), transcripts.RuntimeSessionsSubdir), filepath.Join(sessions.Dir(env.cfgDir, "innomind"), transcripts.RuntimeSessionsSubdir)); err != nil {
			t.Fatal(err)
		}
		if owner := liveOwner(env.files, live[0]); owner != "" {
			t.Errorf("shared runtime dir: owner = %q, want \"\"", owner)
		}
	}
}

// runtimeDirsCanShare reports whether the test can symlink one account's
// runtime dir onto another's (partial isolation) — not on Windows.
func runtimeDirsCanShare() bool {
	return runtime.GOOS != "windows"
}
