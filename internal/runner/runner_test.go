package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

const testSecret = "sk-ant-runner-test-1234"

// setupAccounts seeds a config dir with one oauth account (session dir
// created) and one api_key account.
func setupAccounts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	accs := store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth},
		"api":  {Type: store.TypeAPIKey, Secret: testSecret},
	}}
	if err := store.SaveAccounts(dir, accs); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}
	if err := os.MkdirAll(sessions.Dir(dir, "work"), 0o700); err != nil {
		t.Fatalf("mkdir session: %v", err)
	}
	return dir
}

// fakeClaude installs a shell script as the real claude and returns its path.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude")
	}
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv(shim.EnvRealClaude, script)
	return script
}

const echoEnvScript = `echo "CFG=$CLAUDE_CONFIG_DIR"
echo "KEY=$ANTHROPIC_API_KEY"
echo "MARKER_CLAUDECODE=$CLAUDECODE"
echo "MARKER_SID=$CLAUDE_CODE_SESSION_ID"
echo "MARKER_BRIDGE=$CLAUDE_CODE_BRIDGE_SESSION_ID"
echo "PWD=$(pwd)"
echo "ARGS=$*"
exit 0
`

func run(t *testing.T, cfgDir string, req Request) (int, string, error) {
	t.Helper()
	var out bytes.Buffer
	req.Stdout = &out
	exit, err := Run(context.Background(), cfgDir, req)
	return exit, out.String(), err
}

func TestRunOAuthEnvAndMarkerStripping(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)
	// Plant markers + stale creds the child must NOT see.
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent-session")
	t.Setenv("CLAUDE_CODE_BRIDGE_SESSION_ID", "parent-bridge")
	t.Setenv("ANTHROPIC_API_KEY", "stale-key")
	dir := t.TempDir()

	exit, out, err := run(t, cfg, Request{Account: "work", Dir: dir, Args: []string{"-p", "hi"}})
	if err != nil || exit != 0 {
		t.Fatalf("Run: exit=%d err=%v", exit, err)
	}
	wantCfg := "CFG=" + sessions.Dir(cfg, "work")
	if !strings.Contains(out, wantCfg) {
		t.Errorf("child CLAUDE_CONFIG_DIR wrong:\n%s", out)
	}
	for _, want := range []string{"KEY=\n", "MARKER_CLAUDECODE=\n", "MARKER_SID=\n", "MARKER_BRIDGE=\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("marker/cred not stripped (want %q):\n%s", want, out)
		}
	}
	if !strings.Contains(out, "ARGS=-p hi") {
		t.Errorf("args not passed verbatim:\n%s", out)
	}
}

func TestRunAPIKeyInjectsSecret(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)

	_, out, err := run(t, cfg, Request{Account: "api", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "KEY="+testSecret) {
		t.Errorf("api key not injected:\n%s", out)
	}
	if !strings.Contains(out, "CFG=\n") {
		t.Errorf("api_key run should not set CLAUDE_CONFIG_DIR:\n%s", out)
	}
}

func TestRunCwdHonored(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)
	dir := t.TempDir()

	_, out, err := run(t, cfg, Request{Account: "api", Dir: dir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	if !strings.Contains(out, "PWD="+resolved) && !strings.Contains(out, "PWD="+dir) {
		t.Errorf("cwd not honored (want %s):\n%s", dir, out)
	}
}

func TestRunWritesLaunchEvent(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)
	t.Setenv(usagelog.EnvDisable, "")
	dir := t.TempDir()

	if _, _, err := run(t, cfg, Request{Account: "work", Dir: dir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := usagelog.Read(cfg)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Source != LaunchSource || e.Account != "work" || e.Type != "oauth" || e.Cwd != dir {
		t.Errorf("event: %+v", e)
	}
}

func TestRunLaunchLogDisabled(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)
	t.Setenv(usagelog.EnvDisable, "1")

	if _, _, err := run(t, cfg, Request{Account: "work", Dir: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(usagelog.Path(cfg)); !os.IsNotExist(err) {
		t.Errorf("launch log written despite opt-out (stat err: %v)", err)
	}
}

func TestRunUnknownAccountDoesNotSpawn(t *testing.T) {
	sentinelDir := t.TempDir()
	fakeClaude(t, "touch "+filepath.Join(sentinelDir, "ran")+"\nexit 0\n")
	cfg := setupAccounts(t)

	_, _, err := run(t, cfg, Request{Account: "ghost", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Fatalf("want unknown-account error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sentinelDir, "ran")); !os.IsNotExist(err) {
		t.Error("child was spawned despite unknown account")
	}
}

func TestRunOAuthNotLoggedIn(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := t.TempDir()
	if err := store.SaveAccounts(cfg, store.Accounts{Accounts: map[string]store.Account{
		"fresh": {Type: store.TypeOAuth},
	}}); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}

	_, _, err := run(t, cfg, Request{Account: "fresh", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "bffs login") {
		t.Fatalf("want not-logged-in error, got %v", err)
	}
	// The check must run before the sync, which would have created the dir.
	if _, statErr := os.Stat(sessions.Dir(cfg, "fresh")); !os.IsNotExist(statErr) {
		t.Error("session dir was created by the failed run")
	}
}

func TestRunExitCodePropagates(t *testing.T) {
	fakeClaude(t, "exit 3\n")
	cfg := setupAccounts(t)

	exit, _, err := run(t, cfg, Request{Account: "api", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 3 {
		t.Errorf("exit: want 3, got %d", exit)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	fakeClaude(t, "sleep 30\n")
	cfg := setupAccounts(t)

	start := time.Now()
	exit, _, err := run(t, cfg, Request{
		Account: "api", Dir: t.TempDir(),
		Timeout: 500 * time.Millisecond, ProcessGroup: true,
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got exit=%d err=%v", exit, err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("kill took %s; process group not killed?", elapsed)
	}
}

func TestRunNonexistentDir(t *testing.T) {
	fakeClaude(t, echoEnvScript)
	cfg := setupAccounts(t)

	_, _, err := run(t, cfg, Request{Account: "api", Dir: filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want missing-dir error, got %v", err)
	}
}

// plantMarkers sets every session marker in the test env so a leak of any of
// them into the prepared command is caught.
func plantMarkers(t *testing.T) {
	t.Helper()
	for k := range sessionMarkers {
		t.Setenv(k, "leaked")
	}
}

// envKeys returns the KEY→VALUE map of an exec.Cmd env.
func envKeys(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// stubClaude installs an empty file as the real claude (Windows-safe: no
// shell script) and returns its path. Command never starts the process, so
// a stub that merely exists is all it needs.
func stubClaude(t *testing.T) string {
	t.Helper()
	name := "claude"
	if runtime.GOOS == "windows" {
		name = "claude.exe"
	}
	stub := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(stub, nil, 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(shim.EnvRealClaude, stub)
	return stub
}

func TestCommandOAuthEnvMarkersAndNilStdio(t *testing.T) {
	fake := stubClaude(t)
	t.Setenv(usagelog.EnvDisable, "")
	cfg := setupAccounts(t)
	plantMarkers(t)
	t.Setenv("ANTHROPIC_API_KEY", "stale-key")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1") // deliberate config: must survive
	dir := t.TempDir()

	c, cleanup, err := Command(context.Background(), cfg, Request{Account: "work", Dir: dir, Args: []string{"--resume", "sid-1"}})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup must be non-nil so callers can defer it unconditionally")
	}
	defer cleanup()

	env := envKeys(c.Env)
	if got, want := env[shim.EnvClaudeCfgDir], sessions.Dir(cfg, "work"); got != want {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}
	if v, ok := env[shim.EnvAPIKey]; ok {
		t.Errorf("stale ANTHROPIC_API_KEY leaked into the child env: %q", v)
	}
	for k := range sessionMarkers {
		if v, ok := env[k]; ok {
			t.Errorf("session marker %s=%q leaked into the child env", k, v)
		}
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] != "1" {
		t.Error("CLAUDE_CODE_USE_BEDROCK was stripped; markers must be an explicit list, not a prefix strip")
	}

	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Errorf("nil stdio must stay nil for the caller to wire: stdin=%v stdout=%v stderr=%v", c.Stdin, c.Stdout, c.Stderr)
	}
	if c.Process != nil {
		t.Error("Command must not start the process")
	}
	if c.Path != fake {
		t.Errorf("Path = %q, want %q", c.Path, fake)
	}
	if c.Dir != dir {
		t.Errorf("Dir = %q, want %q", c.Dir, dir)
	}
	if len(c.Args) != 3 || c.Args[1] != "--resume" || c.Args[2] != "sid-1" {
		t.Errorf("Args = %v, want [claude --resume sid-1]", c.Args)
	}
	if c.WaitDelay <= 0 {
		t.Error("WaitDelay not set")
	}

	// The launch is recorded at prepare time (the spawn decision is final).
	events, err := usagelog.Read(cfg)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 || events[0].Source != LaunchSource || events[0].Account != "work" || events[0].Cwd != dir {
		t.Errorf("launch events: %+v", events)
	}
}

func TestCommandAPIKeyEnv(t *testing.T) {
	stubClaude(t)
	cfg := setupAccounts(t)
	plantMarkers(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "stale-cfg"))

	c, cleanup, err := Command(context.Background(), cfg, Request{Account: "api", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	defer cleanup()
	env := envKeys(c.Env)
	if env[shim.EnvAPIKey] != testSecret {
		t.Error("api key not injected")
	}
	if v, ok := env[shim.EnvClaudeCfgDir]; ok {
		t.Errorf("api_key command must not carry CLAUDE_CONFIG_DIR, got %q", v)
	}
	for k := range sessionMarkers {
		if _, ok := env[k]; ok {
			t.Errorf("session marker %s leaked into the child env", k)
		}
	}
}

// Command keeps the caller's writers when given, and the cleanup for a
// timed request releases the deadline context without touching the command.
func TestCommandKeepsGivenStdioAndTimeoutCleanup(t *testing.T) {
	stubClaude(t)
	cfg := setupAccounts(t)
	var out, errBuf bytes.Buffer

	c, cleanup, err := Command(context.Background(), cfg, Request{
		Account: "api", Dir: t.TempDir(), Timeout: time.Minute,
		Stdout: &out, Stderr: &errBuf, Stdin: strings.NewReader("in"),
	})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if c.Stdout != &out || c.Stderr != &errBuf || c.Stdin == nil {
		t.Error("given stdio was not kept verbatim")
	}
	cleanup()
	cleanup() // idempotent: context.CancelFunc tolerates repeated calls
	if c.Process != nil {
		t.Error("cleanup must not start or touch the process")
	}
}

func TestCommandErrorsDoNotLogLaunch(t *testing.T) {
	stubClaude(t)
	t.Setenv(usagelog.EnvDisable, "")
	cfg := setupAccounts(t)

	c, cleanup, err := Command(context.Background(), cfg, Request{Account: "ghost", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Fatalf("want unknown-account error, got cmd=%v cleanup=%v err=%v", c, cleanup != nil, err)
	}
	if c != nil {
		t.Error("cmd must be nil on error")
	}
	if _, statErr := os.Stat(usagelog.Path(cfg)); !os.IsNotExist(statErr) {
		t.Errorf("launch log written for a failed prepare (stat err: %v)", statErr)
	}
}
