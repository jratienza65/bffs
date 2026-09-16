package projectconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindInCurrentDir(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, Filename), `account = "personal"`)
	got, ok, err := Find(dir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !ok {
		t.Fatal("expected to find config")
	}
	if got.Config.Account != "personal" {
		t.Errorf("account: %q", got.Config.Account)
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, filepath.Join(root, Filename), `account = "work"`)
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Find(deep)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !ok {
		t.Fatal("expected to find config")
	}
	if got.Config.Account != "work" {
		t.Errorf("account: %q", got.Config.Account)
	}
	if got.Path != filepath.Join(root, Filename) {
		t.Errorf("path: %q", got.Path)
	}
}

func TestFindClosestWins(t *testing.T) {
	root := t.TempDir()
	writeConfig(t, filepath.Join(root, Filename), `account = "outer"`)
	inner := filepath.Join(root, "proj")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(inner, Filename), `account = "inner"`)
	deep := filepath.Join(inner, "src")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Find(deep)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	if got.Config.Account != "inner" {
		t.Errorf("expected inner to win, got %q", got.Config.Account)
	}
}

func TestFindReturnsFalseWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	got, ok, err := Find(dir)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if ok {
		t.Errorf("expected not found, got %+v", got)
	}
}

func TestFindRejectsBadToml(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, filepath.Join(dir, Filename), `not = valid = toml`)
	_, _, err := Find(dir)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFindStopsAtHomeBoundary documents a security expectation:
// an attacker-writable directory ABOVE $HOME (e.g., a planted file in /tmp
// when the user happens to cd somewhere under it) must NOT be able to pin
// the user's `claude` invocation to an account of the attacker's choosing.
// Find should bound its upward walk to $HOME (or git toplevel — whichever
// is shallower).
func TestFindStopsAtHomeBoundary(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	proj := filepath.Join(home, "proj", "sub")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	// Plant a config ABOVE $HOME — Find must ignore it.
	writeConfig(t, filepath.Join(root, Filename), `account = "attacker"`)

	t.Setenv("HOME", home)

	_, ok, err := Find(proj)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("Find should stop at $HOME=%q; instead it walked past and found a planted config above it", home)
	}
}
