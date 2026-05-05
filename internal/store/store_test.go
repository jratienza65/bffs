package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRoundTripAccounts(t *testing.T) {
	dir := t.TempDir()
	a := Accounts{Accounts: map[string]Account{
		"work":     {Type: TypeAPIKey, Secret: "sk-ant-WORK", Email: "team@reeledge.com"},
		"personal": {Type: TypeOAuth, Secret: "oauth-PERSONAL"},
	}}
	if err := SaveAccounts(dir, a); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}
	got, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(got.Accounts))
	}
	if got.Accounts["work"].Secret != "sk-ant-WORK" {
		t.Errorf("work secret mismatch: %q", got.Accounts["work"].Secret)
	}
	if got.Accounts["personal"].Type != TypeOAuth {
		t.Errorf("personal type: %q", got.Accounts["personal"].Type)
	}
}

func TestPermsAreRestrictiveOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions are not enforced on windows")
	}
	dir := t.TempDir()
	if err := SaveAccounts(dir, Accounts{Accounts: map[string]Account{"x": {Type: TypeAPIKey, Secret: "s"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, AccountsFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("want perm 0600, got %o", got)
	}
}

func TestLoadAccountsMissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(a.Accounts) != 0 {
		t.Errorf("want empty, got %v", a.Accounts)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveState(dir, State{Active: "work", Isolation: IsolationFull}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Active != "work" {
		t.Errorf("want active=work, got %q", got.Active)
	}
	if got.Isolation != IsolationFull {
		t.Errorf("want isolation=full, got %q", got.Isolation)
	}
}

func TestIsolationPresetValid(t *testing.T) {
	for _, valid := range []IsolationPreset{IsolationFull, IsolationPartial} {
		if !valid.Valid() {
			t.Errorf("%q should be valid", valid)
		}
	}
	for _, invalid := range []IsolationPreset{"", "none", "all", "FULL", "minimal"} {
		if invalid.Valid() {
			t.Errorf("%q should not be valid", invalid)
		}
	}
}

func TestResolveIsolation(t *testing.T) {
	cases := []struct {
		name             string
		acc, global, exp IsolationPreset
	}{
		{"per-account override wins", IsolationFull, IsolationPartial, IsolationFull},
		{"global used when account empty", "", IsolationFull, IsolationFull},
		{"default when both empty", "", "", IsolationDefault},
		{"invalid account falls through to global", "garbage", IsolationFull, IsolationFull},
		{"invalid both falls through to default", "garbage", "also-garbage", IsolationDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveIsolation(c.acc, c.global); got != c.exp {
				t.Errorf("ResolveIsolation(%q, %q) = %q, want %q", c.acc, c.global, got, c.exp)
			}
		})
	}
}

func TestRoundTripAccountWithIsolation(t *testing.T) {
	dir := t.TempDir()
	a := Accounts{Accounts: map[string]Account{
		"work": {Type: TypeOAuth, Isolation: IsolationFull, Email: "team@example.com"},
	}}
	if err := SaveAccounts(dir, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Accounts["work"].Isolation != IsolationFull {
		t.Errorf("isolation = %q, want full", got.Accounts["work"].Isolation)
	}
}

func TestConfigDirEnvOverride(t *testing.T) {
	t.Setenv(EnvConfigDir, "/tmp/cm-test-override")
	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if got != "/tmp/cm-test-override" {
		t.Errorf("want override, got %q", got)
	}
}

func TestAccountsGetSetsName(t *testing.T) {
	a := Accounts{Accounts: map[string]Account{"x": {Type: TypeAPIKey, Secret: "s"}}}
	got, ok := a.Get("x")
	if !ok {
		t.Fatal("not found")
	}
	if got.Name != "x" {
		t.Errorf("Name not set, got %q", got.Name)
	}
}
