package transcripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestGitRoot(t *testing.T) {
	base := t.TempDir()

	repo := filepath.Join(base, "repo")
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(base, "worktree")
	if err := os.MkdirAll(filepath.Join(wt, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: /somewhere/.git/worktrees/wt\n")

	plain := filepath.Join(base, "plain", "deeper")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, dir, want string
		ok              bool
	}{
		{"repo root", repo, repo, true},
		{"nested in repo", nested, repo, true},
		{"worktree pointer file", filepath.Join(wt, "src"), wt, true},
		{"nonexistent inside repo", filepath.Join(nested, "missing"), repo, true},
		{"no git", plain, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := GitRoot(c.dir)
			if ok != c.ok || got != c.want {
				t.Errorf("GitRoot(%q) = (%q, %v), want (%q, %v)", c.dir, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestGitRootStopsAtFilesystemRoot(t *testing.T) {
	// The temp dir tree has no .git above it in CI; the walk must terminate.
	if root, ok := GitRoot(t.TempDir()); ok {
		t.Logf("a .git above the temp dir at %s; walk still terminated", root)
	}
}

func realpathT(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return r
}

func TestProjectKeyNonGit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ProjectKey(dir)
	if err != nil {
		t.Fatalf("ProjectKey: %v", err)
	}
	if want := realpathT(t, dir); got != want {
		t.Errorf("ProjectKey = %q, want realpath %q", got, want)
	}
	// A directory that does not exist yet still gets a cleaned absolute key.
	missing := filepath.Join(dir, "later")
	got, err = ProjectKey(missing)
	if err != nil {
		t.Fatalf("ProjectKey(missing): %v", err)
	}
	if want := filepath.Join(realpathT(t, dir), "later"); got != want {
		t.Errorf("ProjectKey(missing) = %q, want %q", got, want)
	}
}

func TestProjectKeyEmpty(t *testing.T) {
	if _, err := ProjectKey(""); err == nil {
		t.Fatal("ProjectKey(\"\") = nil error, want error")
	}
}

func TestProjectKeyNFC(t *testing.T) {
	base := t.TempDir()
	decomposed := "cafe\u0301"
	dir := filepath.Join(base, decomposed)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Skipf("filesystem refused the decomposed name: %v", err)
	}
	got, err := ProjectKey(dir)
	if err != nil {
		t.Fatalf("ProjectKey: %v", err)
	}
	if !strings.HasSuffix(got, "caf\u00e9") {
		t.Errorf("ProjectKey = %q, want an NFC-composed tail", got)
	}
	if got != norm.NFC.String(got) {
		t.Errorf("ProjectKey %q is not NFC", got)
	}
}

// initRepo creates a real git repository and returns its path, or skips
// the test when git is not on PATH.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "init", "-q")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init failed: %v: %s", err, out)
	}
	return repo
}

func TestProjectKeyGit(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ProjectKey(sub)
	if err != nil {
		t.Fatalf("ProjectKey: %v", err)
	}
	if want := realpathT(t, repo); got != want {
		t.Errorf("ProjectKey(subdir) = %q, want git top level %q", got, want)
	}
	if key, err := MemorySlug(sub); err != nil || !strings.HasSuffix(key, "-repo") || strings.Contains(key, "deep") {
		t.Errorf("MemorySlug(subdir) = (%q, %v), want the repo's slug", key, err)
	}
}

func TestProjectKeyGitMissingFallsBackToWalk(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no git here
	got, err := ProjectKey(sub)
	if err != nil {
		t.Fatalf("ProjectKey: %v", err)
	}
	if want := realpathT(t, repo); got != want {
		t.Errorf("ProjectKey without git = %q, want walked root %q", got, want)
	}
}

func TestGitCommandPinsStdioAndEnv(t *testing.T) {
	t.Setenv(envTransferCode, "ABCD-EFGH")
	cmd, out := gitCommand(t.Context(), t.TempDir(), "rev-parse", "--show-toplevel")
	if cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatal("git command must pin Stdout and Stderr, never inherit them")
	}
	if out == nil {
		t.Fatal("no stdout buffer")
	}
	if cmd.Env == nil {
		t.Fatal("git command must set Env explicitly")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(strings.ToUpper(kv), envTransferCode+"=") {
			t.Fatalf("env leaks %s", envTransferCode)
		}
	}
	if !containsEnv(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Error("git command should disable terminal prompts")
	}
}

func TestChildEnvReplacesKeys(t *testing.T) {
	t.Setenv("TZ", "Europe/Berlin")
	t.Setenv(envTransferCode, "secret")
	env := childEnv("TZ=UTC", "LC_ALL=C")
	tz := 0
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "TZ="):
			tz++
			if kv != "TZ=UTC" {
				t.Errorf("TZ = %q, want TZ=UTC", kv)
			}
		case strings.HasPrefix(strings.ToUpper(kv), envTransferCode+"="):
			t.Errorf("childEnv kept %s", envTransferCode)
		}
	}
	if tz != 1 {
		t.Errorf("TZ appears %d times, want once", tz)
	}
	if !containsEnv(env, "LC_ALL=C") {
		t.Error("LC_ALL=C missing")
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
