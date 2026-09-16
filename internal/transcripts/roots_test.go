package transcripts

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

// rootsFixture builds a bffs config dir + home ~/.claude with:
//   - alpha: oauth, partial — projects/ symlinked into home (unix) or absent (windows)
//   - bravo: oauth, full — a real projects/ dir
//   - charlie: oauth, nothing on disk, global preset partial
//   - delta: oauth, nothing on disk, per-account full
//   - key: api_key — never a root
//   - sessions/zed: orphan dir with a real projects/
//   - sessions/stray: orphan dir without projects/ — not a root
//   - sessions/notes.txt: a file — ignored
func rootsFixture(t *testing.T) (cfg, home string, accs store.Accounts, state store.State) {
	t.Helper()
	cfg, home = t.TempDir(), filepath.Join(t.TempDir(), ".claude")
	mkdir(t, filepath.Join(home, ProjectsSubdir))
	accs = store.Accounts{Accounts: map[string]store.Account{
		"alpha":   {Type: store.TypeOAuth},
		"bravo":   {Type: store.TypeOAuth, Isolation: store.IsolationFull},
		"charlie": {Type: store.TypeOAuth},
		"delta":   {Type: store.TypeOAuth, Isolation: store.IsolationFull},
		"key":     {Type: store.TypeAPIKey, Secret: "sk-ant-test"},
	}}
	state = store.State{Active: "alpha", Isolation: store.IsolationPartial}

	if runtime.GOOS != "windows" {
		mkdir(t, sessions.Dir(cfg, "alpha"))
		if err := os.Symlink(filepath.Join(home, ProjectsSubdir), filepath.Join(sessions.Dir(cfg, "alpha"), ProjectsSubdir)); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(t, filepath.Join(sessions.Dir(cfg, "bravo"), ProjectsSubdir))
	mkdir(t, filepath.Join(sessions.Dir(cfg, "zed"), ProjectsSubdir))
	mkdir(t, sessions.Dir(cfg, "stray"))
	writeFile(t, filepath.Join(cfg, sessions.SessionsSubdir, "notes.txt"), "x")
	return cfg, home, accs, state
}

func TestRoots(t *testing.T) {
	cfg, home, accs, state := rootsFixture(t)
	roots, err := Roots(cfg, home, accs, state)
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	var names []string
	for _, r := range roots {
		names = append(names, r.Owner)
	}
	if want := []string{"", "bravo", "delta", "zed"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("root owners = %v, want %v", names, want)
	}

	h := roots[0]
	if h.Dir != filepath.Join(home, ProjectsSubdir) || h.ConfigDir != home {
		t.Errorf("home root = %+v", h)
	}
	if want := filepath.Join(filepath.Dir(home), ".claude.json"); h.ClaudeJSON != want {
		t.Errorf("home ClaudeJSON = %q, want %q", h.ClaudeJSON, want)
	}
	if !h.Shared || h.Orphan {
		t.Errorf("home root Shared=%v Orphan=%v", h.Shared, h.Orphan)
	}
	if want := []string{"alpha", "charlie"}; !reflect.DeepEqual(h.Accounts, want) {
		t.Errorf("home Accounts = %v, want %v", h.Accounts, want)
	}

	b := roots[1]
	if b.Dir != filepath.Join(sessions.Dir(cfg, "bravo"), ProjectsSubdir) || b.ConfigDir != sessions.Dir(cfg, "bravo") {
		t.Errorf("bravo root = %+v", b)
	}
	if b.ClaudeJSON != filepath.Join(sessions.Dir(cfg, "bravo"), ".claude.json") {
		t.Errorf("bravo ClaudeJSON = %q", b.ClaudeJSON)
	}
	if b.Shared || b.Orphan || len(b.Accounts) != 0 {
		t.Errorf("bravo root Shared=%v Orphan=%v Accounts=%v", b.Shared, b.Orphan, b.Accounts)
	}

	d := roots[2]
	if d.Dir != filepath.Join(sessions.Dir(cfg, "delta"), ProjectsSubdir) || d.Shared || d.Orphan {
		t.Errorf("delta (full, never launched) root = %+v", d)
	}
	if _, err := os.Stat(d.Dir); err == nil {
		t.Error("Roots must not create the never-launched full account's projects dir")
	}

	z := roots[3]
	if !z.Orphan || z.Dir != filepath.Join(sessions.Dir(cfg, "zed"), ProjectsSubdir) || z.Shared {
		t.Errorf("orphan root = %+v", z)
	}
	for _, r := range roots {
		if r.Owner == "stray" || r.Owner == "key" || r.Owner == "notes.txt" {
			t.Errorf("unexpected root %+v", r)
		}
	}
}

func TestRootsDefaultHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	roots, err := Roots(t.TempDir(), "", store.Accounts{Accounts: map[string]store.Account{}}, store.State{})
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("roots = %+v, want just home", roots)
	}
	if want := filepath.Join(home, ".claude", ProjectsSubdir); roots[0].Dir != want {
		t.Errorf("home Dir = %q, want %q", roots[0].Dir, want)
	}
	if want := filepath.Join(home, ".claude.json"); roots[0].ClaudeJSON != want {
		t.Errorf("home ClaudeJSON = %q, want %q", roots[0].ClaudeJSON, want)
	}
	if roots[0].Shared {
		t.Error("a home root with no accounts is not Shared")
	}
}

func TestRootsDanglingSymlinkSharesHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	cfg, home := t.TempDir(), filepath.Join(t.TempDir(), ".claude")
	// home/projects does not exist yet; alpha's link dangles.
	mkdir(t, sessions.Dir(cfg, "alpha"))
	if err := os.Symlink(filepath.Join(home, ProjectsSubdir), filepath.Join(sessions.Dir(cfg, "alpha"), ProjectsSubdir)); err != nil {
		t.Fatal(err)
	}
	accs := store.Accounts{Accounts: map[string]store.Account{"alpha": {Type: store.TypeOAuth}}}
	roots, err := Roots(cfg, home, accs, store.State{})
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 1 || !roots[0].Shared || !reflect.DeepEqual(roots[0].Accounts, []string{"alpha"}) {
		t.Errorf("roots = %+v, want one shared home root with alpha", roots)
	}
}

func TestRootsMissingSessionsDir(t *testing.T) {
	roots, err := Roots(t.TempDir(), filepath.Join(t.TempDir(), ".claude"), store.Accounts{Accounts: map[string]store.Account{}}, store.State{})
	if err != nil || len(roots) != 1 {
		t.Fatalf("Roots = (%v, %v), want one home root", roots, err)
	}
}

func TestRootFor(t *testing.T) {
	cfg, home, accs, state := rootsFixture(t)
	roots, err := Roots(cfg, home, accs, state)
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	cases := []struct {
		account   string
		wantOwner string
		wantHome  bool
	}{
		{"", "", true},
		{"home", "", true},
		{"alpha", "", true},
		{"charlie", "", true},
		{"bravo", "bravo", false},
		{"delta", "delta", false},
		{"zed", "zed", false},
	}
	for _, c := range cases {
		t.Run("account="+c.account, func(t *testing.T) {
			r, err := RootFor(roots, c.account)
			if err != nil {
				t.Fatalf("RootFor(%q): %v", c.account, err)
			}
			if r.Owner != c.wantOwner || (r.Owner == "" && !r.Orphan) != c.wantHome {
				t.Errorf("RootFor(%q) = %+v", c.account, r)
			}
		})
	}
	if r, err := RootFor(roots, "zed"); err != nil || !r.Orphan {
		t.Errorf("RootFor(orphan) = (%+v, %v)", r, err)
	}

	_, err = RootFor(roots, "nope")
	if err == nil {
		t.Fatal("RootFor(unknown) = nil error")
	}
	want := `unknown account "nope"; known: [alpha bravo charlie delta home zed]`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestRootForRefusesAccountNamedHome(t *testing.T) {
	roots := []Root{
		{Dir: "/h/projects", ConfigDir: "/h"},
		{Dir: "/c/sessions/home/projects", ConfigDir: "/c/sessions/home", Owner: "home"},
	}
	for _, account := range []string{"", "home", "other"} {
		if _, err := RootFor(roots, account); err == nil || !strings.Contains(err.Error(), `"home" is reserved`) {
			t.Errorf("RootFor(%q) with an owner named home: err = %v", account, err)
		}
	}
	roots = []Root{{Dir: "/h/projects", ConfigDir: "/h", Shared: true, Accounts: []string{"home"}}}
	if _, err := RootFor(roots, ""); err == nil || !strings.Contains(err.Error(), `"home" is reserved`) {
		t.Errorf("RootFor with a shared account named home: err = %v", err)
	}
}

func TestRootForNoHome(t *testing.T) {
	if _, err := RootFor(nil, ""); err == nil {
		t.Fatal("RootFor(nil, \"\") = nil error")
	}
}
