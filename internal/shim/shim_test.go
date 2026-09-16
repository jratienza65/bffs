package shim

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

func TestApplyAccountReplacesExistingEnvVars(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		EnvAPIKey + "=stale-key",
		EnvOAuthToken + "=stale-token",
		EnvClaudeCfgDir + "=/some/stale/dir",
		"OTHER=keep",
	}
	got := applyAccount(env, store.Account{Name: "work", Type: store.TypeAPIKey, Secret: "fresh-key"}, "/cfg")

	hasFresh := false
	for _, kv := range got {
		switch {
		case kv == EnvAPIKey+"=fresh-key":
			hasFresh = true
		case kv == EnvAPIKey+"=stale-key",
			kv == EnvOAuthToken+"=stale-token",
			kv == EnvClaudeCfgDir+"=/some/stale/dir":
			t.Errorf("stale env still present: %q", kv)
		}
	}
	if !hasFresh {
		t.Error("fresh API key not set")
	}
}

func TestApplyAccountOAuthSetsConfigDir(t *testing.T) {
	// oauth accounts get isolated by setting CLAUDE_CONFIG_DIR to the
	// per-account session dir. Claude Code derives a per-account Keychain
	// service name from a sha256 of this dir, so concurrent oauth sessions
	// on different accounts can't collide.
	env := []string{
		"PATH=/usr/bin",
		EnvOAuthToken + "=stale-token",
		EnvClaudeCfgDir + "=/some/stale/dir",
		"OTHER=keep",
	}
	cfgDir := "/cfg"
	got := applyAccount(env, store.Account{Name: "work", Type: store.TypeOAuth}, cfgDir)

	wantConfigDir := EnvClaudeCfgDir + "=" + sessions.Dir(cfgDir, "work")
	hasConfigDir, hasOther := false, false
	for _, kv := range got {
		switch {
		case kv == wantConfigDir:
			hasConfigDir = true
		case kv == "OTHER=keep":
			hasOther = true
		case kv == EnvOAuthToken+"=stale-token", kv == EnvClaudeCfgDir+"=/some/stale/dir":
			t.Errorf("stale env still present: %q", kv)
		case kvKey(kv) == EnvOAuthToken:
			t.Errorf("oauth accounts should not inject CLAUDE_CODE_OAUTH_TOKEN, got %q", kv)
		}
	}
	if !hasConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR not set; want %q", wantConfigDir)
	}
	if !hasOther {
		t.Error("unrelated env var was dropped")
	}
}

func TestFindRealClaudeUsesEnvOverride(t *testing.T) {
	t.Setenv(EnvRealClaude, "/opt/real/claude")
	got, err := FindRealClaude(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/real/claude" {
		t.Errorf("got %q", got)
	}
}

func TestFindRealClaudeUsesCacheWhenValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix-style executable bit")
	}
	t.Setenv(EnvRealClaude, "")
	cfgDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "claude")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cfgDir, store.RealClaudePath)
	if err := os.WriteFile(cachePath, []byte(target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FindRealClaude(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("got %q want %q", got, target)
	}
}

// TestRealClaudeCachePathPerms documents that the cached real-claude.path
// file should be owner-only-readable (0600) like the other config-dir files.
// In practice the dir itself is 0700 so external readers can't reach the
// file anyway; the looser 0644 mode is inconsistent with the rest of the
// store (accounts.toml, state.toml — both 0600) and an easy hardening win.
func TestRealClaudeCachePathPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm bits don't apply on windows")
	}
	t.Setenv(EnvRealClaude, "")
	cfgDir := t.TempDir()
	binDir := t.TempDir()
	target := filepath.Join(binDir, "claude")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	if _, err := FindRealClaude(cfgDir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(cfgDir, store.RealClaudePath))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("real-claude.path perm = %o, want 0600 (consistent with accounts.toml/state.toml)", got)
	}
}

func TestFindRealClaudeSkipsSelfOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix path semantics")
	}
	t.Setenv(EnvRealClaude, "")
	cfgDir := t.TempDir()

	// Build a fake "self" binary and a fake "real claude" in two PATH dirs.
	selfDir := t.TempDir()
	realDir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Symlink the test binary as `claude` in selfDir to mimic the shim layout.
	shimSymlink := filepath.Join(selfDir, "claude")
	if err := os.Symlink(self, shimSymlink); err != nil {
		t.Fatal(err)
	}
	realClaude := filepath.Join(realDir, "claude")
	if err := os.WriteFile(realClaude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", selfDir+string(os.PathListSeparator)+realDir)
	got, err := FindRealClaude(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != realClaude {
		t.Errorf("got %q want %q", got, realClaude)
	}
}
