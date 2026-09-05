package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

const (
	catSID1   = "1e005053-380a-4245-a145-52c2715afa73"
	catSID2   = "b19c4e20-1111-4222-8333-444455556666"
	catBundle = "6f1e2c0a-0000-4000-8000-000000000001"

	// catTitle is the custom title of the fixture session; osc52 is the
	// clipboard-write escape a hostile title could carry.
	catTitle = "Plan: session export"
	osc52    = "\x1b]52;c;aGVsbG8=\x07"

	// memoryBody marks the body of every memory file: the tools list files,
	// they never read them, so it must never reach a result.
	memoryBody = "MEMORY-BODY-MARKER-never-returned"

	// transcriptBody is a later prompt inside the fixture transcript. The
	// first prompt may become a title (Claude's own picker does that), but
	// nothing else of a conversation may ever reach a result.
	transcriptBody = "TRANSCRIPT-BODY-MARKER-never-returned"

	// toolRule and homeLastSID live inside the home .claude.json: grants and
	// pointers trust_status must count or ignore, never return.
	toolRule    = "Bash(go test:*)"
	homeLastSID = "sid-home-pointer"

	// oldCwd is the imported session's directory on the source machine; it
	// does not exist here, so the session is pending rehome.
	oldCwd = "/nonexistent/old/bffs"
)

// knownTitleSources is the title_source enum a consumer may rely on.
var knownTitleSources = map[string]bool{
	"":                                 true,
	transcripts.TitleSourceCustom:      true,
	transcripts.TitleSourceAI:          true,
	transcripts.TitleSourceLastPrompt:  true,
	transcripts.TitleSourceSummary:     true,
	transcripts.TitleSourceFirstPrompt: true,
	transcripts.TitleSourceHistory:     true,
}

// catalogFixture is a synthetic Claude tree: one shared pool under
// <home>/.claude with one titled session and one auto-memory dir for a
// project, one oauth account (work, partial isolation, active) whose
// .claude.json claims the session, one api_key account, and trust answers
// in the home file only.
type catalogFixture struct {
	t        *testing.T
	cfg      string
	home     string
	claude   string // <home>/.claude
	project  string // normalised, exists
	slug     string
	homeJSON string
	workJSON string
}

func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	neutralEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, v := range []string{transcripts.EnvProjectDirName, transcripts.EnvRemoteMemoryDir, transcripts.EnvCoworkMemoryPathOverride} {
		t.Setenv(v, "")
	}
	cfg := filepath.Join(home, "bffs")
	t.Setenv("BFFS_HOME", cfg)
	f := &catalogFixture{t: t, cfg: cfg, home: home, claude: filepath.Join(home, ".claude")}

	accs := store.Accounts{Accounts: map[string]store.Account{
		"work":     {Type: store.TypeOAuth, Email: "work@example.com"},
		"personal": {Type: store.TypeAPIKey, Secret: testSecret, Email: "me@example.com"},
	}}
	if err := store.SaveAccounts(cfg, accs); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(cfg, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}

	// Create before normalising: NormalizePath resolves symlinks only for
	// paths that exist (macOS: /var -> /private/var).
	if err := os.MkdirAll(filepath.Join(home, "build", "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	project, err := store.NormalizePath(filepath.Join(home, "build", "proj"))
	if err != nil {
		t.Fatal(err)
	}
	f.project = project
	if f.slug, err = transcripts.Slug(project); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	f.transcript(f.slug, catSID1, project, now.Add(-2*time.Hour),
		rec(map[string]any{"type": "custom-title", "sessionId": catSID1, "customTitle": osc52 + catTitle}))

	memDir := filepath.Join(f.claude, "projects", f.slug, "memory")
	f.write(filepath.Join(memDir, transcripts.MemoryIndexFile), "# Index\n- [Notes](notes.md) — see @~/notes.md\n"+memoryBody+"\n")
	f.write(filepath.Join(memDir, "notes.md"), "---\npinned: true\n---\nlives at /Users/jonas/x\n"+memoryBody+"\n")

	f.homeJSON = filepath.Join(home, claudejson.Filename)
	f.writeClaudeJSON(f.homeJSON, map[string]any{project: map[string]any{
		"hasTrustDialogAccepted":                  true,
		"hasClaudeMdExternalIncludesApproved":     true,
		"hasClaudeMdExternalIncludesWarningShown": true,
		"allowedTools":                            []string{toolRule},
		"enabledMcpjsonServers":                   []string{"bffs"},
		"lastSessionId":                           homeLastSID,
	}})
	// work's file knows the project only through lastSessionId - the
	// attribution pointer - and has answered no dialog.
	f.workJSON = filepath.Join(sessions.Dir(cfg, "work"), claudejson.Filename)
	f.writeClaudeJSON(f.workJSON, map[string]any{project: map[string]any{"lastSessionId": catSID1}})
	return f
}

// transcript writes a minimal transcript under <home>/.claude/projects/
// <slug>/: a user record with cwd and a first prompt, then extra lines.
func (f *catalogFixture) transcript(slug, sid, cwd string, mtime time.Time, extra ...string) string {
	f.t.Helper()
	path := filepath.Join(f.claude, "projects", slug, sid+transcripts.TranscriptExt)
	lines := []string{
		rec(map[string]any{"type": "user", "cwd": cwd, "sessionId": sid, "version": "2.1.259", "gitBranch": "main",
			"timestamp": mtime.Add(-time.Minute).Format(time.RFC3339), "message": map[string]any{"role": "user", "content": "first prompt"}}),
		rec(map[string]any{"type": "assistant", "sessionId": sid, "message": map[string]any{"role": "assistant", "content": transcriptBody}}),
		rec(map[string]any{"type": "user", "sessionId": sid, "message": map[string]any{"role": "user", "content": transcriptBody}}),
	}
	f.write(path, strings.Join(append(lines, extra...), "\n")+"\n")
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// rec renders one transcript record as a JSON line (json.Marshal, so a
// control character in a title is escaped the way Claude writes it).
func rec(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *catalogFixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// writeClaudeJSON writes a .claude.json with the given projects map plus
// unrelated top-level fields.
func (f *catalogFixture) writeClaudeJSON(path string, projects map[string]any) {
	f.t.Helper()
	b, err := json.MarshalIndent(map[string]any{"userID": "u-1", "theme": "dark", "projects": projects}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(path, string(b))
}

// importPending adds a second transcript whose cwd does not exist here and
// the import record that brought it.
func (f *catalogFixture) importPending() {
	f.t.Helper()
	slug, err := transcripts.Slug(oldCwd)
	if err != nil {
		f.t.Fatal(err)
	}
	f.transcript(slug, catSID2, oldCwd, time.Now().UTC().Add(-9*24*time.Hour))
	rec := imports.Record{
		BundleID:   catBundle,
		Kind:       imports.KindImport,
		ImportedAt: time.Now().UTC().Add(-8 * 24 * time.Hour),
		DestRoot:   filepath.Join(f.claude, "projects"),
		Source:     imports.Source{Hostname: "mac-a", User: "jonas", Home: "/Users/jonas"},
		Sessions: []imports.Session{{
			ID: catSID2, OldCwd: oldCwd, OldSlug: slug, NewCwd: oldCwd, Slug: slug,
			GitRemote: "git@github.com:x/bffs.git", Status: imports.StatusPending,
		}},
	}
	if err := imports.Save(f.cfg, rec); err != nil {
		f.t.Fatal(err)
	}
}

func (f *catalogFixture) handlers() *handlers {
	return &handlers{cfgDir: f.cfg}
}

// mustNotLeak marshals out and fails when any needle - the api_key secret,
// memory bodies, trust grants - reached the result.
func mustNotLeak(t *testing.T, out any, needles ...string) {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, n := range append([]string{testSecret}, needles...) {
		if strings.Contains(string(raw), n) {
			t.Errorf("%q leaked into the output:\n%s", n, raw)
		}
	}
	if strings.Contains(string(raw), "\\u001b") || strings.Contains(string(raw), "52;c;") {
		t.Errorf("escape sequence reached the output:\n%s", raw)
	}
}

func TestListSessions(t *testing.T) {
	f := newCatalogFixture(t)
	h := f.handlers()
	ctx := context.Background()

	_, out, err := h.listSessions(ctx, nil, ListSessionsIn{Directory: f.project})
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(out.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d: %+v", len(out.Sessions), out)
	}
	s := out.Sessions[0]
	if s.SessionID != catSID1 || s.Slug != f.slug || s.Cwd != f.project || !s.CwdExists {
		t.Errorf("identity fields: %+v", s)
	}
	if s.Title != catTitle || s.TitleSource != transcripts.TitleSourceCustom {
		t.Errorf("title: want %q/custom, got %q/%q", catTitle, s.Title, s.TitleSource)
	}
	if !knownTitleSources[s.TitleSource] {
		t.Errorf("title_source %q not in the known set", s.TitleSource)
	}
	if s.Account != "work" || s.AccountSource != "last-session" {
		t.Errorf("attribution: want work/last-session, got %q/%q", s.Account, s.AccountSource)
	}
	if s.GitBranch != "main" || s.ClaudeVersion != "2.1.259" || s.FirstAt == "" || s.LastAt == "" || s.SizeBytes == 0 {
		t.Errorf("head/tail fields: %+v", s)
	}
	if s.Live || s.BundleID != "" || s.OldCwd != "" {
		t.Errorf("not live, not imported: %+v", s)
	}
	wantRoot := filepath.Join(f.claude, "projects")
	if s.Root != wantRoot || len(out.Roots) != 1 || out.Roots[0] != wantRoot {
		t.Errorf("root: want %q, got session.root=%q roots=%v", wantRoot, s.Root, out.Roots)
	}
	if !strings.Contains(out.Note, "best-effort") || !strings.Contains(out.Note, "never a token scan") {
		t.Errorf("note must repeat the attribution caveat: %q", out.Note)
	}
	mustNotLeak(t, out, memoryBody, transcriptBody)

	// The wire shape is the CLI's --json shape: snake_case, every field
	// present.
	raw, _ := json.Marshal(s)
	for _, key := range []string{
		"session_id", "root", "account", "account_source", "cwd", "cwd_exists", "slug", "title", "title_source",
		"git_branch", "claude_version", "first_at", "last_at", "size_bytes", "subagents", "live",
		"bundle_id", "old_cwd", "old_home", "old_host", "git_remote",
	} {
		if !strings.Contains(string(raw), `"`+key+`":`) {
			t.Errorf("json lacks %q: %s", key, raw)
		}
	}

	t.Run("directory defaults to cwd", func(t *testing.T) {
		t.Chdir(f.project)
		_, out, err := h.listSessions(ctx, nil, ListSessionsIn{})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Sessions) != 1 || out.Sessions[0].SessionID != catSID1 {
			t.Errorf("want the project's session, got %+v", out.Sessions)
		}
	})

	t.Run("api_key account maps to home", func(t *testing.T) {
		for _, account := range []string{"personal", transcripts.HomeName, "work"} {
			_, out, err := h.listSessions(ctx, nil, ListSessionsIn{Account: account, Directory: f.project})
			if err != nil {
				t.Fatalf("account %q: %v", account, err)
			}
			if len(out.Roots) != 1 || out.Roots[0] != wantRoot || len(out.Sessions) != 1 {
				t.Errorf("account %q: roots=%v sessions=%d", account, out.Roots, len(out.Sessions))
			}
		}
	})

	t.Run("unknown account", func(t *testing.T) {
		_, _, err := h.listSessions(ctx, nil, ListSessionsIn{Account: "ghost", Directory: f.project})
		if err == nil || !strings.Contains(err.Error(), "unknown account") {
			t.Fatalf("want unknown-account error, got %v", err)
		}
	})

	t.Run("project without a slug dir", func(t *testing.T) {
		_, out, err := h.listSessions(ctx, nil, ListSessionsIn{Directory: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Sessions) != 0 || out.Sessions == nil || !strings.Contains(out.Note, "No sessions") {
			t.Errorf("empty listing: %+v", out)
		}
	})
}

func TestListSessionsPendingRehomeAndLimit(t *testing.T) {
	f := newCatalogFixture(t)
	f.importPending()
	h := f.handlers()
	ctx := context.Background()

	_, out, err := h.listSessions(ctx, nil, ListSessionsIn{Directory: f.project, AllProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 2 || out.Sessions[0].SessionID != catSID1 || out.Sessions[1].SessionID != catSID2 {
		t.Fatalf("all projects, newest first: %+v", out.Sessions)
	}
	imported := out.Sessions[1]
	if imported.BundleID != catBundle || imported.OldCwd != oldCwd || imported.OldHost != "mac-a" || imported.OldHome != "/Users/jonas" || imported.GitRemote != "git@github.com:x/bffs.git" {
		t.Errorf("import record fields: %+v", imported)
	}
	if imported.CwdExists || imported.Account != transcripts.HomeName || imported.AccountSource != "import" {
		t.Errorf("imported session: cwd_exists=%v account=%q/%q", imported.CwdExists, imported.Account, imported.AccountSource)
	}

	_, out, err = h.listSessions(ctx, nil, ListSessionsIn{Directory: f.project, AllProjects: true, PendingRehome: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != catSID2 {
		t.Errorf("pending_rehome: want only %s, got %+v", catSID2, out.Sessions)
	}

	_, out, err = h.listSessions(ctx, nil, ListSessionsIn{Directory: f.project, AllProjects: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != catSID1 {
		t.Errorf("limit 1: %+v", out.Sessions)
	}
	mustNotLeak(t, out)

	for n, want := range map[int]int{0: 50, -3: 50, 7: 7, 500: 500, 9999: 500} {
		if got := clampLimit(n); got != want {
			t.Errorf("clampLimit(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestListMemories(t *testing.T) {
	f := newCatalogFixture(t)
	h := f.handlers()
	ctx := context.Background()

	_, out, err := h.listMemories(ctx, nil, ListMemoriesIn{Directory: f.project})
	if err != nil {
		t.Fatalf("listMemories: %v", err)
	}
	if len(out.Memories) != 1 {
		t.Fatalf("want 1 memory dir, got %d: %+v", len(out.Memories), out)
	}
	m := out.Memories[0]
	wantDir := filepath.Join(f.claude, "projects", f.slug, "memory")
	if m.Dir != wantDir || m.Slug != f.slug || m.Root != filepath.Join(f.claude, "projects") || !m.HasIndex {
		t.Errorf("memory identity: %+v", m)
	}
	if m.Cwd != f.project || !m.CwdExists {
		t.Errorf("memory cwd: %+v", m)
	}
	files := map[string]MemoryFileInfo{}
	for _, mf := range m.Files {
		files[mf.Name] = mf
	}
	index, ok := files[transcripts.MemoryIndexFile]
	if !ok || index.Pinned || len(index.AtRefs) != 1 || index.AtRefs[0] != "@~/notes.md" || index.AbsolutePaths == nil || index.ModifiedAt == "" {
		t.Errorf("MEMORY.md row: %+v (present=%v)", index, ok)
	}
	notes, ok := files["notes.md"]
	if !ok || !notes.Pinned || len(notes.AbsolutePaths) != 1 || notes.AbsolutePaths[0] != "/Users/jonas/x" || notes.AtRefs == nil || notes.SizeBytes == 0 {
		t.Errorf("notes.md row: %+v (present=%v)", notes, ok)
	}
	if !strings.Contains(out.Note, "never returned") || !strings.Contains(out.Note, "UNTRUSTED") {
		t.Errorf("note: %q", out.Note)
	}
	mustNotLeak(t, out, memoryBody)

	t.Run("all projects", func(t *testing.T) {
		_, out, err := h.listMemories(ctx, nil, ListMemoriesIn{Directory: t.TempDir(), AllProjects: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Memories) != 1 || out.Memories[0].Dir != wantDir {
			t.Errorf("all_projects: %+v", out.Memories)
		}
	})

	t.Run("project without memory", func(t *testing.T) {
		_, out, err := h.listMemories(ctx, nil, ListMemoriesIn{Directory: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if out.Memories == nil || len(out.Memories) != 0 || !strings.Contains(out.Note, "0 auto-memory") {
			t.Errorf("empty listing: %+v", out)
		}
	})

	t.Run("api_key account maps to home", func(t *testing.T) {
		_, out, err := h.listMemories(ctx, nil, ListMemoriesIn{Account: "personal", Directory: f.project})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Memories) != 1 {
			t.Errorf("personal: %+v", out.Memories)
		}
	})
}

func TestTrustStatus(t *testing.T) {
	f := newCatalogFixture(t)
	h := f.handlers()
	ctx := context.Background()
	before := map[string][]byte{}
	for _, p := range []string{f.homeJSON, f.workJSON} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = b
	}

	_, out, err := h.trustStatus(ctx, nil, TrustStatusIn{Directory: f.project})
	if err != nil {
		t.Fatalf("trustStatus: %v", err)
	}
	if out.ProjectKey != f.project {
		t.Errorf("project_key: want %q, got %q", f.project, out.ProjectKey)
	}
	if len(out.Accounts) != 2 || out.Accounts[0].Account != "work" || out.Accounts[1].Account != trust.HomeName {
		t.Fatalf("rows (accounts by name, home last): %+v", out.Accounts)
	}
	work, home := out.Accounts[0], out.Accounts[1]
	if work.FolderTrust != "unset" || work.ExternalImports != "unset" || work.Tools != 0 || work.MCPEnabled != 0 {
		t.Errorf("work row: %+v", work)
	}
	if home.FolderTrust != "accepted" || home.ExternalImports != "accepted" || home.Tools != 1 || home.MCPEnabled != 1 || home.InheritedFrom != "" {
		t.Errorf("home row: %+v", home)
	}
	wantCmd := "bffs trust sync --to work --project " + shellWord(f.project)
	if out.SuggestedCommand != wantCmd {
		t.Errorf("suggested_command: want %q, got %q", wantCmd, out.SuggestedCommand)
	}
	if !strings.Contains(out.SuggestedCommand, f.project) {
		t.Errorf("suggested_command must name the project: %q", out.SuggestedCommand)
	}
	if !strings.Contains(out.Note, "human decision") || !strings.Contains(out.Note, "CLI") {
		t.Errorf("note must say trust writes are a human decision on the CLI: %q", out.Note)
	}
	// Counts only: neither the allowedTools rule, the lastSessionId pointer
	// nor anything else from .claude.json beyond the flags is returned.
	mustNotLeak(t, out, toolRule, homeLastSID, "u-1", "dark", catSID1)

	for p, b := range before {
		after, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(b) {
			t.Errorf("trust_status wrote to %s", p)
		}
	}
	if _, err := os.Stat(f.homeJSON + ".lock"); err == nil {
		t.Error("trust_status took Claude's lock")
	}

	t.Run("nothing to carry over", func(t *testing.T) {
		f.writeClaudeJSON(f.workJSON, map[string]any{f.project: map[string]any{
			"hasTrustDialogAccepted":                  true,
			"hasClaudeMdExternalIncludesApproved":     true,
			"hasClaudeMdExternalIncludesWarningShown": true,
			"lastSessionId":                           catSID1,
		}})
		_, out, err := h.trustStatus(ctx, nil, TrustStatusIn{Directory: f.project})
		if err != nil {
			t.Fatal(err)
		}
		if out.SuggestedCommand != "" {
			t.Errorf("suggested_command should be empty: %q", out.SuggestedCommand)
		}
		if out.Accounts[0].FolderTrust != "accepted" {
			t.Errorf("work row after answering: %+v", out.Accounts[0])
		}
	})

	t.Run("inherited from a trusted parent", func(t *testing.T) {
		child := filepath.Join(f.project, "sub")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		_, out, err := h.trustStatus(ctx, nil, TrustStatusIn{Directory: child})
		if err != nil {
			t.Fatal(err)
		}
		home := out.Accounts[len(out.Accounts)-1]
		if home.FolderTrust != "inherited" || home.InheritedFrom != f.project || home.ExternalImports != "unset" {
			t.Errorf("child row for home: %+v", home)
		}
	})
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "''",
		"work":             "work",
		"/Users/j/proj":    "/Users/j/proj",
		`C:\Users\j\proj`:  `C:\Users\j\proj`,
		"/Users/j/my proj": "'/Users/j/my proj'",
		"it's":             `'it'\''s'`,
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCatalogToolsOverTheWire drives the three tools through a real client
// session so the generated output schemas validate the rows (a nil slice
// would fail as null here).
func TestCatalogToolsOverTheWire(t *testing.T) {
	f := newCatalogFixture(t)
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
	for _, tool := range tools.Tools {
		switch tool.Name {
		case "list_sessions", "list_memories", "trust_status":
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s: must be annotated read-only", tool.Name)
			}
		}
	}

	calls := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"list_sessions", map[string]any{"directory": f.project, "all_projects": true}, []string{catSID1, catSID2, catTitle, `"title_source":"custom"`, catBundle}},
		{"list_sessions", map[string]any{"directory": t.TempDir()}, []string{`"sessions":[]`}},
		{"list_memories", map[string]any{"directory": f.project}, []string{`"pinned":true`, "@~/notes.md", `"has_index":true`}},
		{"list_memories", map[string]any{"directory": t.TempDir()}, []string{`"memories":[]`}},
		{"trust_status", map[string]any{"directory": f.project}, []string{`"project_key"`, "bffs trust sync --to work", `"folder_trust":"accepted"`}},
	}
	for _, c := range calls {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.name, Arguments: c.args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", c.name, err)
		}
		if res.IsError {
			t.Fatalf("%s errored: %+v", c.name, res.Content)
		}
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s %v: missing %q in %s", c.name, c.args, w, raw)
			}
		}
		for _, n := range []string{testSecret, memoryBody, transcriptBody, toolRule, homeLastSID, "\\u001b"} {
			if strings.Contains(string(raw), n) {
				t.Errorf("%s: %q leaked through the wire", c.name, n)
			}
		}
	}
}
