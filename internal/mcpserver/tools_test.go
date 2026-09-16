package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

const testSecret = "sk-ant-test-000000001234"

// neutralEnv clears every env var the resolver and session detection read, so
// tests are deterministic even when the test process itself runs inside a
// bffs-launched claude session.
func neutralEnv(t *testing.T) {
	t.Helper()
	t.Setenv(resolver.EnvAccount, "")
	t.Setenv(shim.EnvClaudeCfgDir, "")
	t.Setenv(shim.EnvAPIKey, "")
}

// setupStore seeds a temp config dir with one oauth and one api_key account,
// the oauth one being the global default.
func setupStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	accs := store.Accounts{Accounts: map[string]store.Account{
		"work":     {Type: store.TypeOAuth, Email: "work@example.com"},
		"personal": {Type: store.TypeAPIKey, Secret: testSecret, Email: "me@example.com"},
	}}
	if err := store.SaveAccounts(dir, accs); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}
	if err := store.SaveState(dir, store.State{Active: "work"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	return dir
}

// normalized runs a path through the same canonicalization the handlers use,
// so expectations survive macOS's /var -> /private/var symlink.
func normalized(t *testing.T, p string) string {
	t.Helper()
	n, err := store.NormalizePath(p)
	if err != nil {
		t.Fatalf("NormalizePath(%s): %v", p, err)
	}
	return n
}

func TestListAccounts(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}

	_, out, err := h.listAccounts(context.Background(), nil, ListAccountsIn{})
	if err != nil {
		t.Fatalf("listAccounts: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(out.Accounts))
	}
	// Names() sorts, so order is deterministic.
	personal, work := out.Accounts[0], out.Accounts[1]
	if personal.Name != "personal" || work.Name != "work" {
		t.Fatalf("unexpected order: %q, %q", personal.Name, work.Name)
	}
	if !work.IsDefault || personal.IsDefault {
		t.Errorf("is_default: want work only (work=%v personal=%v)", work.IsDefault, personal.IsDefault)
	}
	if work.Isolation != string(store.IsolationPartial) {
		t.Errorf("oauth isolation: want partial, got %q", work.Isolation)
	}
	if personal.Isolation != "" {
		t.Errorf("api_key isolation: want empty, got %q", personal.Isolation)
	}
	if out.DefaultAccount != "work" {
		t.Errorf("default_account: want work, got %q", out.DefaultAccount)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testSecret) {
		t.Error("secret leaked into list_accounts output")
	}
}

func TestListAccountsEmptyStore(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: t.TempDir()}

	_, out, err := h.listAccounts(context.Background(), nil, ListAccountsIn{})
	if err != nil {
		t.Fatalf("listAccounts: %v", err)
	}
	if len(out.Accounts) != 0 {
		t.Errorf("want no accounts, got %d", len(out.Accounts))
	}
	if out.Note == "" {
		t.Error("want a note pointing at `bffs add`/`bffs login`")
	}
}

func TestResolveGlobal(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}
	dir := t.TempDir()

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: dir})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Source != "global" || out.Account != "work" {
		t.Errorf("want global/work, got %s/%s", out.Source, out.Account)
	}
	if out.Directory != normalized(t, dir) {
		t.Errorf("directory: want %q, got %q", normalized(t, dir), out.Directory)
	}
	if out.Isolation != string(store.IsolationPartial) {
		t.Errorf("isolation: want partial, got %q", out.Isolation)
	}
	if !strings.Contains(out.Note, "NEXT") {
		t.Errorf("note lacks next-launch caveat: %q", out.Note)
	}
}

func TestResolveProjectFile(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bffs.toml"), []byte("account = \"personal\"\n"), 0o600); err != nil {
		t.Fatalf("write bffs.toml: %v", err)
	}

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: dir})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Source != "project" || out.Account != "personal" {
		t.Errorf("want project/personal, got %s/%s", out.Source, out.Account)
	}
	if !strings.HasSuffix(out.ProjectFile, "bffs.toml") {
		t.Errorf("project_file: %q", out.ProjectFile)
	}
}

func TestResolvePathRule(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}
	ruleDir := t.TempDir()
	var paths store.Paths
	if _, err := paths.Set(ruleDir, "personal"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.SavePaths(cfg, paths); err != nil {
		t.Fatalf("SavePaths: %v", err)
	}
	child := filepath.Join(ruleDir, "sub")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: child})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Source != "path" || out.Account != "personal" {
		t.Errorf("want path/personal, got %s/%s", out.Source, out.Account)
	}
	if out.PathRule != normalized(t, ruleDir) {
		t.Errorf("path_rule: want %q, got %q", normalized(t, ruleDir), out.PathRule)
	}
}

func TestResolveNone(t *testing.T) {
	neutralEnv(t)
	cfg := t.TempDir()
	if err := store.SaveAccounts(cfg, store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth},
	}}); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}
	h := &handlers{cfgDir: cfg}

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Source != "none" || out.Account != "" {
		t.Errorf("want none/<empty>, got %s/%s", out.Source, out.Account)
	}
	if !strings.Contains(out.Note, "own untouched credentials") {
		t.Errorf("note: %q", out.Note)
	}
}

func TestResolveEnvOverride(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	t.Setenv(resolver.EnvAccount, "personal")

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Source != "env" || out.Account != "personal" {
		t.Errorf("want env/personal, got %s/%s", out.Source, out.Account)
	}
	if out.EnvOverride != "personal" {
		t.Errorf("env_override: want personal, got %q", out.EnvOverride)
	}
	if !strings.Contains(out.Note, resolver.EnvAccount) {
		t.Errorf("note should explain the env override: %q", out.Note)
	}
}

func TestResolveEnvUnknownAccount(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	t.Setenv(resolver.EnvAccount, "ghost")

	_, _, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("want unknown-account error, got %v", err)
	}
}

func TestResolveTildeDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based tilde expansion")
	}
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	home := t.TempDir()
	t.Setenv("HOME", home)
	sub := filepath.Join(home, "proj")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: "~/proj"})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Directory != normalized(t, sub) {
		t.Errorf("directory: want %q, got %q", normalized(t, sub), out.Directory)
	}
}

func TestResolveRelativeDirectory(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	dir := t.TempDir()
	t.Chdir(dir)

	_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: "."})
	if err != nil {
		t.Fatalf("resolveAccount: %v", err)
	}
	if out.Directory != normalized(t, dir) {
		t.Errorf("directory: want %q, got %q", normalized(t, dir), out.Directory)
	}
}

func TestDetectSessionAccount(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}

	t.Run("oauth via CLAUDE_CONFIG_DIR", func(t *testing.T) {
		t.Setenv(shim.EnvClaudeCfgDir, sessions.Dir(cfg, "work"))
		_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
		if err != nil {
			t.Fatalf("resolveAccount: %v", err)
		}
		if out.CurrentSessionAccount != "work" {
			t.Errorf("current_session_account: want work, got %q", out.CurrentSessionAccount)
		}
	})

	t.Run("api_key via ANTHROPIC_API_KEY", func(t *testing.T) {
		t.Setenv(shim.EnvAPIKey, testSecret)
		_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
		if err != nil {
			t.Fatalf("resolveAccount: %v", err)
		}
		if out.CurrentSessionAccount != "personal" {
			t.Errorf("current_session_account: want personal, got %q", out.CurrentSessionAccount)
		}
	})

	t.Run("undetectable", func(t *testing.T) {
		_, out, err := h.resolveAccount(context.Background(), nil, ResolveIn{Directory: t.TempDir()})
		if err != nil {
			t.Fatalf("resolveAccount: %v", err)
		}
		if out.CurrentSessionAccount != "" {
			t.Errorf("current_session_account: want empty, got %q", out.CurrentSessionAccount)
		}
	})
}

func TestSwitchAccount(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}

	_, out, err := h.switchAccount(context.Background(), nil, SwitchIn{Name: "personal"})
	if err != nil {
		t.Fatalf("switchAccount: %v", err)
	}
	if out.Active != "personal" || out.Previous != "work" {
		t.Errorf("want active=personal previous=work, got %+v", out)
	}
	state, err := store.LoadState(cfg)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Active != "personal" {
		t.Errorf("state.toml not updated: active=%q", state.Active)
	}
}

func TestSwitchAccountUnknown(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}

	_, _, err := h.switchAccount(context.Background(), nil, SwitchIn{Name: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Fatalf("want unknown-account error, got %v", err)
	}
}

func TestPinAccount(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}
	dir := t.TempDir()

	_, out, err := h.pinAccount(context.Background(), nil, PinIn{Account: "personal", Directory: dir})
	if err != nil {
		t.Fatalf("pinAccount: %v", err)
	}
	if out.Previous != "" || out.Account != "personal" {
		t.Errorf("first pin: %+v", out)
	}
	paths, err := store.LoadPaths(cfg)
	if err != nil {
		t.Fatalf("LoadPaths: %v", err)
	}
	rule, ok := paths.Match(dir)
	if !ok || rule.Account != "personal" {
		t.Fatalf("rule not saved: %+v ok=%v", rule, ok)
	}

	_, out, err = h.pinAccount(context.Background(), nil, PinIn{Account: "work", Directory: dir})
	if err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if out.Previous != "personal" {
		t.Errorf("re-pin previous: want personal, got %q", out.Previous)
	}
}

func TestPinAccountUnknown(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}

	_, _, err := h.pinAccount(context.Background(), nil, PinIn{Account: "ghost", Directory: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Fatalf("want unknown-account error, got %v", err)
	}
	paths, err := store.LoadPaths(cfg)
	if err != nil {
		t.Fatalf("LoadPaths: %v", err)
	}
	if len(paths.Rules) != 0 {
		t.Errorf("rule written despite unknown account: %+v", paths.Rules)
	}
}

func TestPinAccountWarnsAboutProjectFile(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t)}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bffs.toml"), []byte("account = \"work\"\n"), 0o600); err != nil {
		t.Fatalf("write bffs.toml: %v", err)
	}

	_, out, err := h.pinAccount(context.Background(), nil, PinIn{Account: "personal", Directory: dir})
	if err != nil {
		t.Fatalf("pinAccount: %v", err)
	}
	if !strings.Contains(out.Warning, "bffs.toml") || !strings.Contains(out.Warning, "work") {
		t.Errorf("warning should name the outranking bffs.toml: %q", out.Warning)
	}
}

func TestUnpinAccount(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}
	dir := t.TempDir()

	if _, _, err := h.pinAccount(context.Background(), nil, PinIn{Account: "personal", Directory: dir}); err != nil {
		t.Fatalf("pinAccount: %v", err)
	}
	_, out, err := h.unpinAccount(context.Background(), nil, UnpinIn{Directory: dir})
	if err != nil {
		t.Fatalf("unpinAccount: %v", err)
	}
	if !out.Removed || out.Account != "personal" {
		t.Errorf("want removed=true account=personal, got %+v", out)
	}
	paths, err := store.LoadPaths(cfg)
	if err != nil {
		t.Fatalf("LoadPaths: %v", err)
	}
	if len(paths.Rules) != 0 {
		t.Errorf("rule still present: %+v", paths.Rules)
	}

	_, out, err = h.unpinAccount(context.Background(), nil, UnpinIn{Directory: dir})
	if err != nil {
		t.Fatalf("unpinAccount (absent): %v", err)
	}
	if out.Removed {
		t.Error("want removed=false for absent rule")
	}
	if !strings.Contains(out.Note, "No directory rule") {
		t.Errorf("note: %q", out.Note)
	}
}

func TestAccountUsage(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	home := t.TempDir()

	// One attributable session: launch event for "personal" 30s before the
	// session's first record, same cwd.
	start := time.Now().UTC().Add(-time.Hour)
	if err := usagelog.Append(cfg, usagelog.Event{TS: start.Add(-30 * time.Second), Account: "personal", Type: "api_key", Source: "global", Cwd: "/proj/a"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/proj/a","requestId":"r1","message":{"id":"m1","model":"claude-opus-5","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[]}}`,
		start.Format(time.RFC3339))
	transcript := filepath.Join(home, "projects", "-proj-a", "sid-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(transcript, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	h := &handlers{cfgDir: cfg, homeClaudeDir: home}
	_, out, err := h.accountUsage(context.Background(), nil, AccountUsageIn{HorizonHours: 24})
	if err != nil {
		t.Fatalf("accountUsage: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out.Accounts))
	}
	var personal AccountUsageRow
	for _, row := range out.Accounts {
		if row.Name == "personal" {
			personal = row
		}
	}
	if personal.FiveHour.Messages != 1 || personal.FiveHour.Output != 20 {
		t.Errorf("personal five_hour: %+v", personal.FiveHour)
	}
	if personal.LastUsed == "" {
		t.Error("personal last_used empty")
	}
	// The oauth account is idle → suggested.
	if out.SuggestedAccount != "work" {
		t.Errorf("suggested_account: want work, got %q", out.SuggestedAccount)
	}
	if out.Note == "" {
		t.Error("note must always be set")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testSecret) {
		t.Error("secret leaked into account_usage output")
	}
}

func TestAccountUsageEmptyFixture(t *testing.T) {
	neutralEnv(t)
	h := &handlers{cfgDir: setupStore(t), homeClaudeDir: t.TempDir()}

	_, out, err := h.accountUsage(context.Background(), nil, AccountUsageIn{})
	if err != nil {
		t.Fatalf("accountUsage: %v", err)
	}
	if len(out.Accounts) != 2 || out.Unattributed.Messages != 0 {
		t.Errorf("empty fixture: %+v", out)
	}
}

func TestUnpinAccountParentRule(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	h := &handlers{cfgDir: cfg}
	parent := t.TempDir()
	child := filepath.Join(parent, "sub")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := h.pinAccount(context.Background(), nil, PinIn{Account: "personal", Directory: parent}); err != nil {
		t.Fatalf("pinAccount: %v", err)
	}

	_, out, err := h.unpinAccount(context.Background(), nil, UnpinIn{Directory: child})
	if err != nil {
		t.Fatalf("unpinAccount: %v", err)
	}
	if out.Removed {
		t.Error("child unpin must not remove the parent rule")
	}
	if !strings.Contains(out.Note, normalized(t, parent)) {
		t.Errorf("note should name the parent rule %q: %q", normalized(t, parent), out.Note)
	}
	paths, err := store.LoadPaths(cfg)
	if err != nil {
		t.Fatalf("LoadPaths: %v", err)
	}
	if len(paths.Rules) != 1 {
		t.Errorf("parent rule lost: %+v", paths.Rules)
	}
}
