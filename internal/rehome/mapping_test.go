package rehome

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

func TestParseMapping(t *testing.T) {
	dst := t.TempDir()
	dstNorm, err := store.NormalizePath(dst)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		in      string
		old     string
		dst     string
		wantErr string
	}{
		{"/srv/nobody/build/x=" + dst, "/srv/nobody/build/x", dstNorm, ""},
		{" /srv/nobody/build/x = " + dst + " ", "/srv/nobody/build/x", dstNorm, ""},
		{"/srv/nobody/build/x/=" + dst, "/srv/nobody/build/x", dstNorm, ""},
		{`C:\Users\jonas\x=` + dst, filepath.Clean(`C:\Users\jonas\x`), dstNorm, ""},
		{"/a=/b=c", "", "", "contains '='"},
		{"/a=/b=c", "", "", "interactive prompt"},
		{"noequals", "", "", "expected OLD=NEW"},
		{"=" + dst, "", "", "both OLD and NEW are required"},
		{"/a=", "", "", "both OLD and NEW are required"},
		{"relative/x=" + dst, "", "", `"relative/x" is not absolute`},
		{"/a=relative", "", "", `"relative" is not absolute`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			m, err := ParseMapping(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Old != c.old || m.New != c.dst {
				t.Errorf("ParseMapping = %+v, want Old %q New %q", m, c.old, c.dst)
			}
		})
	}
	// "~" expands on the NEW side.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	m, err := ParseMapping("/old=~/src/x")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "src", "x"); m.New != want {
		t.Errorf("New = %q, want %q", m.New, want)
	}
	// ... and on the OLD side (a local move), against this machine's home.
	homeNorm, err := store.NormalizePath(home)
	if err != nil {
		t.Fatal(err)
	}
	m, err = ParseMapping("~/old=" + dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(homeNorm, "old"); m.Old != want {
		t.Errorf("Old = %q, want %q", m.Old, want)
	}
}

func TestApplyMappings(t *testing.T) {
	x, y := filepath.Join(t.TempDir(), "x"), filepath.Join(t.TempDir(), "y")
	maps := []Mapping{
		{Old: "/a", New: x},
		{Old: "/a/b", New: y}, // longer, listed second: still wins for /a/b/...
	}
	join := func(base string, rest ...string) string { return filepath.Join(append([]string{base}, rest...)...) }
	cases := []struct {
		cwd, key         string
		wantCwd, wantKey string
		ok               bool
	}{
		{"/a/b/c", "/a/b", join(y, "c"), y, true},
		{"/a/b", "/a/b", y, y, true},
		{"/a/bb", "/a/bb", join(x, "bb"), join(x, "bb"), true},
		{"/a", "/a", x, x, true},
		{"/a/c/d", "/a", join(x, "c", "d"), x, true},
		{"/other/a/b", "/other", "", "", false},
		{"/ab", "/ab", "", "", false},
		{"", "/a/b/c", join(y, "c"), join(y, "c"), true}, // no cwd: the key decides
		{"", "/zzz", "", "", false},
		{"/a/b/c", "/zzz", join(y, "c"), "", true}, // key outside every rule: derived later
	}
	for _, c := range cases {
		t.Run(c.cwd+"|"+c.key, func(t *testing.T) {
			gotCwd, gotKey, ok := ApplyMappings(maps, c.cwd, c.key)
			if ok != c.ok || gotCwd != c.wantCwd || gotKey != c.wantKey {
				t.Errorf("ApplyMappings = %q, %q, %v; want %q, %q, %v", gotCwd, gotKey, ok, c.wantCwd, c.wantKey, c.ok)
			}
		})
	}
	if _, _, ok := ApplyMappings(nil, "/a", "/a"); ok {
		t.Error("no rules matched something")
	}
	// A rule whose Old is a Windows path matches Windows-spelled cwds on
	// every OS.
	win := []Mapping{{Old: `C:\Users\jonas\proj`, New: x}}
	got, _, ok := ApplyMappings(win, `C:\Users\jonas\proj\sub`, "")
	if !ok || got != join(x, "sub") {
		t.Errorf("windows rule = %q, %v", got, ok)
	}
}

// A rule matches the normalised spelling of a local directory: on macOS
// a session started in /tmp/x records "/tmp/x" while its realpath is
// /private/tmp/x — both sides normalise before the comparison.
func TestApplyMappingsNormalisesLocalPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, "dst")
	maps := []Mapping{{Old: link, New: dst}}
	got, _, ok := ApplyMappings(maps, filepath.Join(real, "sub"), "")
	if !ok || got != filepath.Join(dst, "sub") {
		t.Errorf("ApplyMappings through a symlinked rule = %q, %v", got, ok)
	}
}
