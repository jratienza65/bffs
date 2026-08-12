package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchLongestPrefixWins(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "projects")
	child := filepath.Join(parent, "client-a")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	p := Paths{Rules: []PathRule{
		{Path: parent, Account: "work"},
		{Path: child, Account: "client"},
	}}

	got, ok := p.Match(child)
	if !ok || got.Account != "client" {
		t.Errorf("child dir resolved to %q (ok=%v), want client", got.Account, ok)
	}
	got, ok = p.Match(parent)
	if !ok || got.Account != "work" {
		t.Errorf("parent dir resolved to %q (ok=%v), want work", got.Account, ok)
	}
}

func TestMatchAppliesToSubdirectories(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "repo", "services", "api")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Paths{Rules: []PathRule{{Path: root, Account: "personal"}}}

	got, ok := p.Match(deep)
	if !ok || got.Account != "personal" {
		t.Errorf("nested dir resolved to %q (ok=%v), want personal", got.Account, ok)
	}
}

// A rule on /a/b must not capture /a/bc — segment boundaries, not string
// prefixes. Getting this wrong would silently bill a sibling repo's work to
// the wrong account.
func TestMatchRespectsSegmentBoundaries(t *testing.T) {
	root := t.TempDir()
	ruleDir := filepath.Join(root, "app")
	sibling := filepath.Join(root, "app-main")
	for _, d := range []string{ruleDir, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	p := Paths{Rules: []PathRule{{Path: ruleDir, Account: "work"}}}
	if _, ok := p.Match(sibling); ok {
		t.Errorf("rule on %q wrongly matched sibling %q", ruleDir, sibling)
	}
	if _, ok := p.Match(ruleDir); !ok {
		t.Errorf("rule on %q did not match itself", ruleDir)
	}
}

func TestMatchNoRules(t *testing.T) {
	if _, ok := (Paths{}).Match(t.TempDir()); ok {
		t.Error("empty rule set matched something")
	}
}

func TestSetReplacesAndReportsPrevious(t *testing.T) {
	dir := t.TempDir()
	var p Paths

	if prev, err := p.Set(dir, "work"); err != nil || prev != "" {
		t.Fatalf("first Set returned prev=%q err=%v, want empty", prev, err)
	}
	prev, err := p.Set(dir, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "work" {
		t.Errorf("prev = %q, want work", prev)
	}
	if len(p.Rules) != 1 {
		t.Errorf("got %d rules, want 1 (Set must replace, not append)", len(p.Rules))
	}
}

func TestRemoveOnlyExactDirectory(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Paths{Rules: []PathRule{{Path: root, Account: "work"}}}

	// The child only inherits; removing it must not silently drop the parent.
	removed, err := p.Remove(child)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("Remove deleted an inherited rule; it should only match exactly")
	}
	if len(p.Rules) != 1 {
		t.Errorf("rules = %d, want 1", len(p.Rules))
	}

	if removed, _ = p.Remove(root); !removed || len(p.Rules) != 0 {
		t.Errorf("exact Remove failed: removed=%v rules=%d", removed, len(p.Rules))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	cfg := t.TempDir()
	work := t.TempDir()

	var p Paths
	if _, err := p.Set(work, "work"); err != nil {
		t.Fatal(err)
	}
	if err := SavePaths(cfg, p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPaths(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 1 || got.Rules[0].Account != "work" {
		t.Fatalf("round trip lost data: %+v", got.Rules)
	}
	if m, ok := got.Match(work); !ok || m.Account != "work" {
		t.Errorf("loaded rules did not match: %+v ok=%v", m, ok)
	}
}

func TestLoadPathsMissingFileIsNotAnError(t *testing.T) {
	got, err := LoadPaths(t.TempDir())
	if err != nil {
		t.Fatalf("LoadPaths on missing file: %v", err)
	}
	if len(got.Rules) != 0 {
		t.Errorf("got %d rules, want 0", len(got.Rules))
	}
}

func TestNormalizePathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := NormalizePath("~/somewhere")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := NormalizePath(filepath.Join(home, "somewhere"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
