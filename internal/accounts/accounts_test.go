package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"work", true}, {"work-2", true}, {"Work_2", true},
		{"", false}, {"home", false}, {"a b", false}, {"../etc", false}, {"a/b", false}, {"café", false},
	} {
		if err := ValidateName(tc.name); (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if err := ValidateName("home"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("the reserved name must say why: %v", err)
	}
}

func TestAddAPIKey(t *testing.T) {
	dir := t.TempDir()
	if err := AddAPIKey(dir, "work", "sk-ant-secret", "team@example.com", false); err != nil {
		t.Fatal(err)
	}
	accs, err := store.LoadAccounts(dir)
	if err != nil {
		t.Fatal(err)
	}
	acc := accs.Accounts["work"]
	if acc.Type != store.TypeAPIKey || acc.Secret != "sk-ant-secret" || acc.Email != "team@example.com" {
		t.Fatalf("saved = %+v", acc)
	}
	// The file holds a secret, so it is not world-readable.
	info, err := os.Stat(filepath.Join(dir, "accounts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("accounts.toml is %o, want 600", perm)
	}
	// A second add of the same name is refused unless forced.
	if err := AddAPIKey(dir, "work", "sk-ant-other", "", false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate = %v", err)
	}
	if err := AddAPIKey(dir, "work", "sk-ant-other", "", true); err != nil {
		t.Fatalf("force = %v", err)
	}
	// An empty secret is refused before anything is written.
	if err := AddAPIKey(dir, "fresh", "   ", "", false); err == nil {
		t.Error("an empty secret must be refused")
	}
	accs, _ = store.LoadAccounts(dir)
	if _, ok := accs.Accounts["fresh"]; ok {
		t.Error("a refused add wrote an account")
	}
}

// An oauth account is prepared before the login runs and recorded after
// it: an abandoned login leaves a session directory and no account.
func TestPrepareAndCompleteOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(home, "bffs")
	claude := filepath.Join(home, "fake-claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BFFS_REAL_CLAUDE", claude)

	prep, err := PrepareOAuth(cfg, "work", "", false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if prep.Bin != claude {
		t.Errorf("bin = %q", prep.Bin)
	}
	if got := strings.Join(prep.Args, " "); got != "auth login --claudeai" {
		t.Errorf("args = %q", got)
	}
	if _, err := os.Stat(prep.SessionDir); err != nil {
		t.Errorf("the session dir was not made: %v", err)
	}
	var seen bool
	for _, kv := range prep.Env {
		if kv == "CLAUDE_CONFIG_DIR="+prep.SessionDir {
			seen = true
		}
	}
	if !seen {
		t.Error("the login runs without CLAUDE_CONFIG_DIR pointing at the session dir")
	}
	// Nothing is recorded yet.
	if accs, err := store.LoadAccounts(cfg); err == nil {
		if _, ok := accs.Accounts["work"]; ok {
			t.Error("PrepareOAuth wrote an account before the login ran")
		}
	}

	acc, err := CompleteOAuth(cfg, prep, "me@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Type != store.TypeOAuth || acc.Email != "me@example.com" {
		t.Fatalf("acc = %+v", acc)
	}
	accs, err := store.LoadAccounts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := accs.Accounts["work"]; !ok {
		t.Error("the account was not saved")
	}
	state, err := store.LoadState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != "work" {
		t.Errorf("active = %q, want work", state.Active)
	}
	// The console and sso flags reach the command line.
	p2, err := PrepareOAuth(cfg, "other", store.IsolationFull, true, true, "x@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p2.Args, " "); got != "auth login --console --sso --email x@example.com" {
		t.Errorf("args = %q", got)
	}
	if p2.Preset != store.IsolationFull {
		t.Errorf("preset = %q", p2.Preset)
	}
	if _, err := PrepareOAuth(cfg, "bad", store.IsolationPreset("sideways"), false, false, ""); err == nil {
		t.Error("an invalid preset must be refused")
	}
}
