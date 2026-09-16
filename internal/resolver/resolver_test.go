package resolver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jratienza65/bffs/internal/projectconfig"
	"github.com/jratienza65/bffs/internal/store"
)

func setupAccounts(t *testing.T) (configDir string) {
	t.Helper()
	configDir = t.TempDir()
	a := store.Accounts{Accounts: map[string]store.Account{
		"work":     {Type: store.TypeAPIKey, Secret: "sk-WORK"},
		"personal": {Type: store.TypeOAuth, Secret: "oauth-PERSONAL"},
	}}
	if err := store.SaveAccounts(configDir, a); err != nil {
		t.Fatal(err)
	}
	return configDir
}

func TestEnvWinsOverEverything(t *testing.T) {
	cfg := setupAccounts(t)
	if err := store.SaveState(cfg, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, projectconfig.Filename), []byte(`account = "work"`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAccount, "personal")
	r, err := Resolve(cfg, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceEnv || r.Account.Name != "personal" {
		t.Errorf("got %+v", r)
	}
}

func TestProjectWinsOverGlobal(t *testing.T) {
	cfg := setupAccounts(t)
	if err := store.SaveState(cfg, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, projectconfig.Filename), []byte(`account = "personal"`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAccount, "")
	r, err := Resolve(cfg, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceProject || r.Account.Name != "personal" {
		t.Errorf("got %+v", r)
	}
	if r.ProjectFile == "" {
		t.Error("ProjectFile should be set")
	}
}

func TestGlobalWhenNoProject(t *testing.T) {
	cfg := setupAccounts(t)
	if err := store.SaveState(cfg, store.State{Active: "work"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAccount, "")
	r, err := Resolve(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceGlobal || r.Account.Name != "work" {
		t.Errorf("got %+v", r)
	}
}

func TestNoneWhenNothingConfigured(t *testing.T) {
	cfg := setupAccounts(t)
	t.Setenv(EnvAccount, "")
	r, err := Resolve(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != SourceNone {
		t.Errorf("source: %q", r.Source)
	}
	if r.Account.Name != "" {
		t.Errorf("account should be zero, got %+v", r.Account)
	}
}

func TestUnknownAccountFromEnvErrors(t *testing.T) {
	cfg := setupAccounts(t)
	t.Setenv(EnvAccount, "ghost")
	_, err := Resolve(cfg, t.TempDir())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUnknownAccountFromProjectErrors(t *testing.T) {
	cfg := setupAccounts(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, projectconfig.Filename), []byte(`account = "ghost"`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAccount, "")
	_, err := Resolve(cfg, cwd)
	if err == nil {
		t.Fatal("expected error")
	}
}
