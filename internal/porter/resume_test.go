package porter

import (
	"testing"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

func TestResumeAccount(t *testing.T) {
	e := newEnv(t)
	if err := store.SaveAccounts(e.cfgDir, store.Accounts{Accounts: map[string]store.Account{
		"work":   {Type: store.TypeOAuth},
		"aviate": {Type: store.TypeOAuth},
	}}); err != nil {
		t.Fatal(err)
	}
	full := transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Owner: "work"}
	if got := ResumeAccount(e.cfgDir, full, e.cwd); got != "work" {
		t.Errorf("owned root: %q", got)
	}
	if got := ResumeAccount(e.cfgDir, e.root, e.cwd); got != "" {
		t.Errorf("nothing resolved: %q", got)
	}
	if err := store.SaveState(e.cfgDir, store.State{Active: "aviate"}); err != nil {
		t.Fatal(err)
	}
	if got := ResumeAccount(e.cfgDir, e.root, e.cwd); got != "aviate" {
		t.Errorf("global active: %q", got)
	}
	var p store.Paths
	if _, err := p.Set(e.cwd, "work"); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePaths(e.cfgDir, p); err != nil {
		t.Fatal(err)
	}
	if got := ResumeAccount(e.cfgDir, e.root, e.cwd); got != "work" {
		t.Errorf("path rule: %q", got)
	}
	// An unknown account anywhere in the chain is an error → no prefix.
	t.Setenv("BFFS_ACCOUNT", "ghost")
	if got := ResumeAccount(e.cfgDir, e.root, e.cwd); got != "" {
		t.Errorf("resolver error: %q", got)
	}
}
