package rehome

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
)

const testRemote = "git@github.com:jratienza65/bffs.git"

// gitInit makes dir a checkout with origin set to testRemote; skips the
// test when git is not on PATH.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", testRemote}} {
		cmd := exec.Command(gitBin, append([]string{"-C", dir}, args...)...)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, errb.String())
		}
	}
}

func TestSuggestOrdering(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envTransferCode, "secret")
	gitInit(t, filepath.Join(home, "build", "bffs"))         // same remote, same name
	gitInit(t, filepath.Join(home, "src", "forks", "other")) // same remote, other name (depth 2)
	mkdirs := []string{
		filepath.Join(home, "code", "bffs"),                  // same name, no remote
		filepath.Join(home, "build", "projects", "bffs"),     // same path relative to home (+ same name)
		filepath.Join(home, "build", "node_modules", "bffs"), // skipped dir
		filepath.Join(home, "build", ".hidden", "bffs"),      // hidden
		filepath.Join(home, "dev", "a", "b", "c", "bffs"),    // too deep
		filepath.Join(home, "src", "unrelated"),
	}
	for _, d := range mkdirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := imports.Record{
		Source: imports.Source{Home: "/Users/nobody"},
		Sessions: []imports.Session{
			{ID: sid1, OldCwd: "/Users/nobody/build/projects/bffs", GitRemote: testRemote},
			{ID: sid2, OldCwd: "/Users/nobody/build/projects/bffs"},
			{ID: sid3, OldCwd: "/Users/nobody/elsewhere/tool"},
		},
		Memories: []imports.Memory{{OldCwd: "/Users/nobody/build/projects/bffs"}},
	}
	got := Suggest(rec, home, nil)
	if len(got) != 2 || got[0].OldCwd != "/Users/nobody/build/projects/bffs" || got[1].OldCwd != "/Users/nobody/elsewhere/tool" {
		t.Fatalf("suggestions = %+v", got)
	}
	want := []Candidate{
		{Dir: filepath.Join(home, "build", "bffs"), Reason: "same git remote " + testRemote + ", same folder name"},
		{Dir: filepath.Join(home, "src", "forks", "other"), Reason: "same git remote " + testRemote},
		{Dir: filepath.Join(home, "build", "projects", "bffs"), Reason: "same path relative to home"},
		{Dir: filepath.Join(home, "code", "bffs"), Reason: "same folder name"},
	}
	if !reflect.DeepEqual(got[0].Candidates, want) {
		t.Errorf("candidates =\n%+v\nwant\n%+v", got[0].Candidates, want)
	}
	if len(got[1].Candidates) != 0 {
		t.Errorf("tool candidates = %+v", got[1].Candidates)
	}
	// Explicit roots replace the defaults (the home-relative candidate
	// needs no scan); an old directory that exists here gets none.
	rec.Sessions = append(rec.Sessions, imports.Session{ID: "x", OldCwd: filepath.Join(home, "src", "unrelated")})
	got = Suggest(rec, home, []string{filepath.Join(home, "code")})
	want = []Candidate{
		{Dir: filepath.Join(home, "build", "projects", "bffs"), Reason: "same path relative to home"},
		{Dir: filepath.Join(home, "code", "bffs"), Reason: "same folder name"},
	}
	if len(got) != 3 || !reflect.DeepEqual(got[0].Candidates, want) || len(got[2].Candidates) != 0 {
		t.Errorf("explicit roots = %+v", got)
	}
	if got := Suggest(imports.Record{}, home, nil); got != nil {
		t.Errorf("empty record = %+v", got)
	}
}

// The git call never sees the transfer code and gives up on a hung git.
func TestGitRemoteEnvAndTimeout(t *testing.T) {
	var seen []string
	orig := gitRemoteFn
	t.Cleanup(func() { gitRemoteFn = orig })
	gitRemoteFn = func(dir string) string {
		seen = append(seen, dir)
		return ""
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "build", "x", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	Suggest(imports.Record{Sessions: []imports.Session{{ID: sid1, OldCwd: "/Users/other/x", GitRemote: "r"}}}, home, nil)
	if !reflect.DeepEqual(seen, []string{filepath.Join(home, "build", "x")}) {
		t.Errorf("git called for %v", seen)
	}
	env := childEnv("GIT_TERMINAL_PROMPT=0")
	for _, kv := range env {
		if strings.HasPrefix(kv, envTransferCode+"=") {
			t.Errorf("transfer code leaked: %q", kv)
		}
	}
	// The scan stops at the budget.
	oldBudget := suggestBudget
	t.Cleanup(func() { suggestBudget = oldBudget })
	suggestBudget = 0
	start := time.Now()
	got := Suggest(imports.Record{Sessions: []imports.Session{{ID: sid1, OldCwd: "/Users/other/x"}}}, home, nil)
	if time.Since(start) > time.Second {
		t.Error("scan ignored the budget")
	}
	if len(got) != 1 || len(got[0].Candidates) != 0 {
		t.Errorf("budget-limited scan = %+v", got)
	}
}

func TestBaseName(t *testing.T) {
	for in, want := range map[string]string{"/a/b/c": "c", "/a/b/c/": "c", `C:\x\y`: "y", "plain": "plain", "": ""} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}
