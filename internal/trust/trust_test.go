package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// jkey JSON-encodes a project key so Windows backslashes survive in a
// fixture.
func jkey(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// docWith renders a .claude.json holding the given projects entries (key →
// raw entry JSON) next to a few unrelated top-level fields, so tests can
// check that those survive.
func docWith(projects map[string]string) string {
	keys := make([]string, 0, len(projects))
	for k := range projects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(`{"numStartups": 7, "oauthAccount": {"emailAddress": "a@example.com"}, "userID": "u-1", "projects": {`)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s: %s", jkey(k), projects[k])
	}
	sb.WriteString(`}}`)
	return sb.String()
}

// jsonFile writes docWith(projects) into a fresh dir and returns its path.
func jsonFile(t *testing.T, projects map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), claudejson.Filename)
	writeFile(t, path, docWith(projects))
	return path
}

// entryJSON renders a projects entry from the three dialog answers plus
// extra raw fields.
func entryJSON(trust, approved, shown bool, extra string) string {
	s := fmt.Sprintf(`{"hasTrustDialogAccepted": %t, "hasClaudeMdExternalIncludesApproved": %t, "hasClaudeMdExternalIncludesWarningShown": %t`, trust, approved, shown)
	if extra != "" {
		s += ", " + extra
	}
	return s + "}"
}

func boolp(b bool) *bool { return &b }

func TestAnswerString(t *testing.T) {
	want := map[Answer]string{Unset: "unset", Accepted: "accepted", Declined: "declined", Inherited: "inherited"}
	for a, s := range want {
		if a.String() != s {
			t.Errorf("Answer(%d).String() = %q, want %q", int(a), a.String(), s)
		}
	}
	if got := Answer(9).String(); got != "Answer(9)" {
		t.Errorf("unknown answer renders %q", got)
	}
}

func TestFiles(t *testing.T) {
	cfg := t.TempDir()
	homeJSON := filepath.Join(t.TempDir(), claudejson.Filename)
	accs := store.Accounts{Accounts: map[string]store.Account{
		"bravo": {Type: store.TypeOAuth},
		"alpha": {Type: store.TypeOAuth, Isolation: store.IsolationFull},
		"key":   {Type: store.TypeAPIKey, Secret: "sk-ant-test"},
	}}
	// An orphan session dir on disk: accounts.toml is authoritative, so it
	// must not appear.
	if err := os.MkdirAll(filepath.Join(sessions.Dir(cfg, "zed")), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessions.Dir(cfg, "zed"), claudejson.Filename), `{}`)

	files, err := Files(cfg, homeJSON, accs)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	want := map[string]string{
		HomeName: homeJSON,
		"alpha":  filepath.Join(sessions.Dir(cfg, "alpha"), claudejson.Filename),
		"bravo":  filepath.Join(sessions.Dir(cfg, "bravo"), claudejson.Filename),
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("Files = %v, want %v", files, want)
	}
	for _, absent := range []string{"key", "zed"} {
		if _, ok := files[absent]; ok {
			t.Errorf("%q must not be a trust file", absent)
		}
	}
}

func TestFilesDefaultsHomeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	files, err := Files(t.TempDir(), "", store.Accounts{})
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if got, want := files[HomeName], filepath.Join(home, claudejson.Filename); got != want {
		t.Errorf("home file = %q, want %q", got, want)
	}
	if len(files) != 1 {
		t.Errorf("Files with no accounts = %v, want only home", files)
	}
}

func TestFilesRefusesAccountNamedHome(t *testing.T) {
	accs := store.Accounts{Accounts: map[string]store.Account{HomeName: {Type: store.TypeOAuth}}}
	_, err := Files(t.TempDir(), filepath.Join(t.TempDir(), claudejson.Filename), accs)
	if err == nil || !strings.Contains(err.Error(), `"home" is reserved`) || !strings.Contains(err.Error(), "bffs rename") {
		t.Fatalf("want a reserved-name error pointing at bffs rename, got %v", err)
	}
}

func TestEffectiveFolderTrust(t *testing.T) {
	var (
		root = filepath.FromSlash("/")
		work = filepath.FromSlash("/work")
		repo = filepath.FromSlash("/work/repo")
		sub  = filepath.FromSlash("/work/repo/sub")
	)
	trusted := func(keys ...string) map[string]claudejson.ProjectFlags {
		m := map[string]claudejson.ProjectFlags{}
		for _, k := range keys {
			m[k] = claudejson.ProjectFlags{TrustAccepted: boolp(true)}
		}
		return m
	}
	cases := []struct {
		name     string
		flags    map[string]claudejson.ProjectFlags
		key      string
		gitRoot  string
		want     Answer
		wantFrom string
	}{
		{"exact true", trusted(sub), sub, repo, Accepted, ""},
		{"exact false is unset, never declined", map[string]claudejson.ProjectFlags{sub: {TrustAccepted: boolp(false)}}, sub, "", Unset, ""},
		{"nothing known", nil, sub, "", Unset, ""},
		{"parent inside the repo covers the child", trusted(repo), sub, repo, Inherited, repo},
		{"parent above the git root does not count", trusted(work, root), sub, repo, Unset, ""},
		{"outside a repo the walk is unbounded", trusted(work), sub, "", Inherited, work},
		{"the filesystem root can be the trusted ancestor", trusted(root), sub, "", Inherited, root},
		{"a key that is the git root has no ancestors", trusted(work), repo, repo, Unset, ""},
		{"explicit false at the key still inherits", map[string]claudejson.ProjectFlags{sub: {TrustAccepted: boolp(false)}, repo: {TrustAccepted: boolp(true)}}, sub, repo, Inherited, repo},
		{"nearest trusted ancestor wins", trusted(work, repo), sub, "", Inherited, repo},
		{"a gitRoot that is not an ancestor does not bound", trusted(work), sub, filepath.FromSlash("/elsewhere"), Inherited, work},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, from := EffectiveFolderTrust(tc.flags, tc.key, tc.gitRoot)
			if got != tc.want || from != tc.wantFrom {
				t.Errorf("EffectiveFolderTrust = (%v, %q), want (%v, %q)", got, from, tc.want, tc.wantFrom)
			}
		})
	}
}

func TestReport(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	key := filepath.Join(work, "proj")
	files := map[string]string{
		"alpha": jsonFile(t, map[string]string{
			key: entryJSON(true, true, true, `"allowedTools": ["a", "b", "c"], "enabledMcpjsonServers": ["m", "n"], "lastSessionId": "sid-1"`),
		}),
		"bravo": filepath.Join(t.TempDir(), claudejson.Filename), // never written
		"charlie": jsonFile(t, map[string]string{
			key:  entryJSON(false, false, true, ""),
			work: entryJSON(true, false, false, ""),
		}),
		HomeName: jsonFile(t, map[string]string{
			key: entryJSON(true, false, false, `"allowedTools": "Bash(git:*)"`),
		}),
	}

	got, err := Report(files, key, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	want := []Status{
		{Account: "alpha", File: files["alpha"], ProjectKey: key, Present: true, Folder: Accepted, External: Accepted, Tools: 3, MCPEnabled: 2},
		{Account: "bravo", File: files["bravo"], ProjectKey: key},
		{Account: "charlie", File: files["charlie"], ProjectKey: key, Present: true, Folder: Inherited, InheritedFrom: work, External: Declined},
		{Account: HomeName, File: files[HomeName], ProjectKey: key, Present: true, Folder: Accepted, External: Unset, Tools: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Report =\n%+v\nwant\n%+v", got, want)
	}
}

func TestReportGitBounded(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	repo := filepath.Join(work, "repo")
	key := filepath.Join(repo, "sub")
	files := map[string]string{
		"above": jsonFile(t, map[string]string{work: entryJSON(true, false, false, "")}),
		"inside": jsonFile(t, map[string]string{
			repo: entryJSON(true, false, false, ""),
			key:  entryJSON(false, false, false, ""),
		}),
	}
	got, err := Report(files, key, repo)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got[0].Account != "above" || got[0].Folder != Unset || got[0].InheritedFrom != "" {
		t.Errorf("a trusted parent above the git root must not count: %+v", got[0])
	}
	if got[1].Account != "inside" || got[1].Folder != Inherited || got[1].InheritedFrom != repo || !got[1].Present {
		t.Errorf("a trusted repo root covers its child: %+v", got[1])
	}

	unbounded, err := Report(files, key, "")
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if unbounded[0].Folder != Inherited || unbounded[0].InheritedFrom != work {
		t.Errorf("outside a repo the parent counts: %+v", unbounded[0])
	}
}

func TestReportCorruptFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), claudejson.Filename)
	writeFile(t, bad, `{not json`)
	_, err := Report(map[string]string{"delta": bad, HomeName: filepath.Join(t.TempDir(), "none.json")}, filepath.FromSlash("/x"), "")
	if err == nil || !strings.Contains(err.Error(), "delta") {
		t.Fatalf("want an error naming the account, got %v", err)
	}
}

func TestBestSource(t *testing.T) {
	st := func(name string, folder Answer) Status { return Status{Account: name, Folder: folder} }
	matrix := []Status{st("alpha", Accepted), st("bravo", Unset), st("charlie", Inherited), st(HomeName, Accepted)}
	cases := []struct {
		name     string
		statuses []Status
		active   string
		want     string
		ok       bool
	}{
		{"an exact answer beats the active account's inherited one", matrix, "charlie", "alpha", true},
		{"active accepted", matrix, "alpha", "alpha", true},
		{"active unset falls to the first account", matrix, "bravo", "alpha", true},
		{"no active", matrix, "", "alpha", true},
		{"unknown active", matrix, "ghost", "alpha", true},
		{"home's exact answer beats an inherited one", []Status{st("alpha", Inherited), st(HomeName, Accepted)}, "alpha", HomeName, true},
		{"active inherited when nobody accepted exactly", []Status{st("alpha", Inherited), st("charlie", Inherited), st(HomeName, Inherited)}, "charlie", "charlie", true},
		{"first inherited by name", []Status{st("alpha", Unset), st("charlie", Inherited), st("bravo", Inherited)}, "", "bravo", true},
		{"home only when no account trusts", []Status{st("alpha", Unset), st("bravo", Declined), st(HomeName, Inherited)}, "alpha", HomeName, true},
		{"active home", []Status{st("alpha", Accepted), st(HomeName, Accepted)}, HomeName, HomeName, true},
		{"nobody answered", []Status{st("alpha", Unset), st(HomeName, Unset)}, "alpha", "", false},
		{"empty", nil, "alpha", "", false},
		{"unsorted input picks by name", []Status{st("zulu", Accepted), st("bravo", Accepted)}, "", "bravo", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BestSource(tc.statuses, tc.active)
			if got != tc.want || ok != tc.ok {
				t.Errorf("BestSource = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestJSONPathFor(t *testing.T) {
	homeJSON := filepath.Join(t.TempDir(), claudejson.Filename)
	alpha := filepath.Join(t.TempDir(), "alpha", claudejson.Filename)
	files := map[string]string{HomeName: homeJSON, "alpha": alpha}
	accs := store.Accounts{Accounts: map[string]store.Account{
		"alpha": {Type: store.TypeOAuth},
		"key":   {Type: store.TypeAPIKey, Secret: "sk-ant-secret"},
	}}

	if p, err := JSONPathFor(files, accs, "alpha"); err != nil || p != alpha {
		t.Errorf("alpha → (%q, %v), want %q", p, err, alpha)
	}
	if p, err := JSONPathFor(files, accs, HomeName); err != nil || p != homeJSON {
		t.Errorf("home → (%q, %v), want %q", p, err, homeJSON)
	}

	_, err := JSONPathFor(files, accs, "key")
	if err == nil || !strings.Contains(err.Error(), `account "key" is an api_key account`) || !strings.Contains(err.Error(), "--to home") {
		t.Errorf("api_key account must be refused with a --to home hint, got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("error leaks the secret: %v", err)
	}

	_, err = JSONPathFor(files, accs, "ghost")
	if err == nil || err.Error() != `unknown account "ghost"; known: [alpha home]` {
		t.Errorf("unknown name → %v", err)
	}
}

func writeRuntimeSession(t *testing.T, cfg string, pid int, sid, cwd string) {
	t.Helper()
	body := fmt.Sprintf(`{"pid": %d, "sessionId": %q, "cwd": %q, "kind": "interactive", "status": "idle"}`, pid, sid, cwd)
	writeFile(t, filepath.Join(cfg, transcripts.RuntimeSessionsSubdir, strconv.Itoa(pid)+".json"), body)
}

func TestLiveLines(t *testing.T) {
	cfg := t.TempDir()
	homeDir := t.TempDir()
	homeJSON := filepath.Join(homeDir, claudejson.Filename)
	files := map[string]string{
		HomeName: homeJSON,
		"alpha":  filepath.Join(sessions.Dir(cfg, "alpha"), claudejson.Filename),
		"bravo":  filepath.Join(sessions.Dir(cfg, "bravo"), claudejson.Filename), // no sessions/ at all
	}
	// The home file's config dir is ~/.claude next to it, not its parent.
	own := os.Getpid()
	writeRuntimeSession(t, filepath.Join(homeDir, ".claude"), own, "sid-home", "/work/home")
	writeRuntimeSession(t, sessions.Dir(cfg, "alpha"), own, "sid-alpha", "/work/alpha")
	writeRuntimeSession(t, homeDir, own, "sid-wrong-dir", "/never") // <home>/sessions is not a config dir

	got, err := LiveLines(context.Background(), files)
	if err != nil {
		t.Fatalf("LiveLines: %v", err)
	}
	var sids []string
	for _, ls := range got {
		sids = append(sids, ls.SessionID)
		if ls.PID != own {
			t.Errorf("pid = %d, want %d", ls.PID, own)
		}
	}
	if want := []string{"sid-alpha", "sid-home"}; !reflect.DeepEqual(sids, want) {
		t.Errorf("live sessions = %v, want %v", sids, want)
	}
	for _, ls := range got {
		switch ls.SessionID {
		case "sid-home":
			if ls.ConfigDir != filepath.Join(homeDir, ".claude") {
				t.Errorf("home session config dir = %q", ls.ConfigDir)
			}
		case "sid-alpha":
			if ls.ConfigDir != sessions.Dir(cfg, "alpha") {
				t.Errorf("alpha session config dir = %q", ls.ConfigDir)
			}
		}
	}
}

func TestLiveLinesSortsByPID(t *testing.T) {
	// Two config dirs recording the same (live) pid under different session
	// ids sort by id after pid; a cancelled context is reported.
	cfg := t.TempDir()
	files := map[string]string{
		"b": filepath.Join(sessions.Dir(cfg, "b"), claudejson.Filename),
		"a": filepath.Join(sessions.Dir(cfg, "a"), claudejson.Filename),
	}
	own := os.Getpid()
	writeRuntimeSession(t, sessions.Dir(cfg, "b"), own, "sid-2", "/w")
	writeRuntimeSession(t, sessions.Dir(cfg, "a"), own, "sid-1", "/w")
	got, err := LiveLines(context.Background(), files)
	if err != nil {
		t.Fatalf("LiveLines: %v", err)
	}
	if len(got) != 2 || got[0].SessionID != "sid-1" || got[1].SessionID != "sid-2" {
		t.Errorf("order = %+v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LiveLines(ctx, files); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context → %v", err)
	}
}
