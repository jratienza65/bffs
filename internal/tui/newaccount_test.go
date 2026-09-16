package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
)

// Adding an account was the one thing the browser sent people to the
// CLI for. n opens the screen; an api_key account is finished here, and
// an oauth one hands the terminal to `claude auth login`.
func TestNewAccountAPIKey(t *testing.T) {
	f := newFixture(t)
	f.transcript(f.slug, sid1, f.project, "first prompt of one", fixedNow.Add(-time.Hour))
	h := f.start("sessions")
	h.keys("1", "n")
	sc, ok := h.a.top().(*newAccountScreen)
	if !ok {
		t.Fatalf("n on the accounts panel should open the new-account screen, got %T", h.a.top())
	}
	wantAll(t, h.view(), "new account", "Claude subscription (oauth)", "API key", "accounts.toml at 0600")

	// The api key path: pick it, name it, type the key.
	h.keys("down", "enter")
	if sc.state != naName {
		t.Fatalf("enter should ask for the name, state = %v", sc.state)
	}
	h.keys("h", "o", "m", "e", "enter")
	if sc.state != naName || !strings.Contains(h.view(), "reserved") {
		t.Errorf("the reserved name must be refused here, not at save time:\n%s", h.view())
	}
	for _, k := range []string{"backspace", "backspace", "backspace", "backspace"} {
		h.keys(k)
	}
	h.keys("w", "o", "r", "k", "2", "enter")
	if sc.state != naSecret {
		t.Fatalf("a valid name should ask for the key, state = %v", sc.state)
	}
	if v := h.view(); strings.Contains(v, "sk-ant-secret") {
		t.Error("the key must not be echoed")
	}
	for _, r := range "sk-ant-secret" {
		h.keys(string(r))
	}
	h.keys("enter")

	if h.a.top() != nil {
		t.Errorf("saving should close the screen, top is %T", h.a.top())
	}
	if !strings.HasPrefix(h.a.status, "added account") {
		t.Errorf("status = %q", h.a.status)
	}
	accs, err := store.LoadAccounts(f.cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := accs.Accounts["work2"]
	if !ok || acc.Type != store.TypeAPIKey || acc.Secret != "sk-ant-secret" {
		t.Fatalf("saved account = %+v (ok=%v)", acc, ok)
	}
	// It is not made active by itself: this is the browser, not `bffs switch`.
	state, _ := store.LoadState(f.cfgDir)
	if state.Active == "work2" {
		t.Error("adding an account in the browser must not switch to it")
	}
	// And the accounts panel lists it after the refresh.
	if !strings.Contains(h.view(), "work2") {
		t.Errorf("the new account is not listed:\n%s", h.view())
	}
}

// A name that is already taken is refused before anything is written.
func TestNewAccountRefusesADuplicate(t *testing.T) {
	f := newFixture(t)
	h := f.start("sessions")
	h.keys("1", "n", "down", "enter")
	for _, r := range "work" {
		h.keys(string(r))
	}
	h.keys("enter")
	sc := h.a.top().(*newAccountScreen)
	if sc.state != naName {
		t.Fatalf("a taken name should stay on the name step, state = %v", sc.state)
	}
	if !strings.Contains(h.view(), "already exists") {
		t.Errorf("the refusal does not name the reason:\n%s", h.view())
	}
}
