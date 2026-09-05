package transcripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCleanupPeriodDays(t *testing.T) {
	cases := []struct {
		name       string
		user       string // settings.json content; "" = absent
		local      string // settings.local.json content; "" = absent
		wantDays   int
		wantSource string
	}{
		{"missing both", "", "", 30, "default"},
		{"user only", `{"cleanupPeriodDays": 7}`, "", 7, "settings.json"},
		{"local over user", `{"cleanupPeriodDays": 7}`, `{"cleanupPeriodDays": 3}`, 3, "settings.local.json"},
		{"local without key falls through", `{"cleanupPeriodDays": 9}`, `{"theme": "dark"}`, 9, "settings.json"},
		{"local null falls through", `{"cleanupPeriodDays": 9}`, `{"cleanupPeriodDays": null}`, 9, "settings.json"},
		{"zero means never", `{"cleanupPeriodDays": 0}`, "", 0, "settings.json"},
		{"negative is zero", `{"cleanupPeriodDays": -5}`, "", 0, "settings.json"},
		{"float truncates", `{"cleanupPeriodDays": 7.9}`, "", 7, "settings.json"},
		{"invalid json local", `{"cleanupPeriodDays": 7}`, `{not json`, 0, "invalid"},
		{"invalid json user", `{"cleanupPeriodDays": `, "", 0, "invalid"},
		{"empty file", "", " ", 0, "invalid"},
		{"string value", `{"cleanupPeriodDays": "7"}`, "", 0, "invalid"},
		{"array top level", `[]`, "", 0, "invalid"},
		{"user absent local set", "", `{"cleanupPeriodDays": 12}`, 12, "settings.local.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.user != "" {
				writeFile(t, filepath.Join(dir, SettingsFile), c.user)
			}
			if c.local != "" {
				writeFile(t, filepath.Join(dir, LocalSettingsFile), c.local)
			}
			days, source := CleanupPeriodDays(dir)
			if days != c.wantDays || source != c.wantSource {
				t.Errorf("CleanupPeriodDays = (%d, %q), want (%d, %q)", days, source, c.wantDays, c.wantSource)
			}
		})
	}
}

func TestCleanupPeriodDaysMissingDir(t *testing.T) {
	days, source := CleanupPeriodDays(filepath.Join(t.TempDir(), "nope"))
	if days != DefaultCleanupPeriodDays || source != CleanupSourceDefault {
		t.Errorf("CleanupPeriodDays(missing) = (%d, %q), want (30, default)", days, source)
	}
}

func clearMemoryEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvRemoteMemoryDir, "")
	t.Setenv(EnvCoworkMemoryPathOverride, "")
}

func TestMemoryDirFor(t *testing.T) {
	clearMemoryEnv(t)
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	proj := filepath.Join(t.TempDir(), "my proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := MemoryDirFor(root, proj)
	if err != nil {
		t.Fatalf("MemoryDirFor: %v", err)
	}
	key, err := ProjectKey(proj)
	if err != nil {
		t.Fatal(err)
	}
	slug, err := Slug(key)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root.Dir, slug, MemorySubdir)
	if got != want {
		t.Errorf("MemoryDirFor = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join("my-proj", "memory")) {
		t.Errorf("memory dir %q should end in my-proj/memory", got)
	}
}

func TestMemoryDirForCoworkOverride(t *testing.T) {
	clearMemoryEnv(t)
	want := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(EnvCoworkMemoryPathOverride, want+string(filepath.Separator))
	cfg := t.TempDir()
	// The override beats settings and the remote root, and does not depend
	// on the project directory (which need not even exist).
	writeFile(t, filepath.Join(cfg, LocalSettingsFile), `{"autoMemoryDirectory": "/srv/mem"}`)
	t.Setenv(EnvRemoteMemoryDir, t.TempDir())
	root := Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg}
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		got, err := MemoryDirFor(root, dir)
		if err != nil || got != want {
			t.Errorf("MemoryDirFor(%s) = (%q, %v), want %q", dir, got, err, want)
		}
	}

	t.Setenv(EnvCoworkMemoryPathOverride, "relative/mem")
	if _, err := MemoryDirFor(root, t.TempDir()); err == nil || !strings.Contains(err.Error(), EnvCoworkMemoryPathOverride) {
		t.Errorf("relative override: err = %v, want one naming %s", err, EnvCoworkMemoryPathOverride)
	}
}

func TestMemoryDirForRemoteRoot(t *testing.T) {
	clearMemoryEnv(t)
	t.Setenv(EnvRemoteMemoryDir, t.TempDir())
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg}
	// Claude keys memory under the remote dir with a slug function bffs has
	// not verified (disk.md §5): refuse rather than guess, naming the
	// variable so callers can say why no memory is placed.
	_, err := MemoryDirFor(root, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), ErrMemoryDirOverridden.Error()) || !strings.Contains(err.Error(), EnvRemoteMemoryDir) {
		t.Errorf("MemoryDirFor = %v, want ErrMemoryDirOverridden naming %s", err, EnvRemoteMemoryDir)
	}
	// A settings autoMemoryDirectory comes before the default layout, so it
	// still wins over the remote dir.
	want := filepath.Join(t.TempDir(), "mem")
	quoted, _ := json.Marshal(want)
	writeFile(t, filepath.Join(cfg, SettingsFile), `{"autoMemoryDirectory": `+string(quoted)+`}`)
	if got, err := MemoryDirFor(root, t.TempDir()); err != nil || got != want {
		t.Errorf("settings over remote: (%q, %v), want %q", got, err, want)
	}
}

func TestMemoryDirForSettingsOverride(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	abs := filepath.Join(t.TempDir(), "srv", "mem")
	cases := []struct {
		name  string
		files map[string]string
		want  string // "" = the default under root.Dir
	}{
		{"user settings", map[string]string{SettingsFile: `{"autoMemoryDirectory": ` + quoteJSON(abs) + `}`}, abs},
		{"local over user", map[string]string{
			SettingsFile:      `{"autoMemoryDirectory": ` + quoteJSON(filepath.Join(abs, "user")) + `}`,
			LocalSettingsFile: `{"autoMemoryDirectory": ` + quoteJSON(filepath.Join(abs, "local")) + `}`,
		}, filepath.Join(abs, "local")},
		{"tilde expands to home", map[string]string{SettingsFile: `{"autoMemoryDirectory": "~/mem/dir/"}`}, filepath.Join(home, "mem", "dir")},
		{"rejected local falls through to user", map[string]string{
			LocalSettingsFile: `{"autoMemoryDirectory": "/a/../b"}`,
			SettingsFile:      `{"autoMemoryDirectory": ` + quoteJSON(abs) + `}`,
		}, abs},
		{"empty string is unset", map[string]string{SettingsFile: `{"autoMemoryDirectory": ""}`}, ""},
		{"too short", map[string]string{SettingsFile: `{"autoMemoryDirectory": "~/"}`}, ""},
		{"dotdot", map[string]string{SettingsFile: `{"autoMemoryDirectory": "~/../x"}`}, ""},
		{"filesystem root", map[string]string{SettingsFile: `{"autoMemoryDirectory": ` + quoteJSON(filepath.VolumeName(abs)+strings.Repeat(string(filepath.Separator), 3)) + `}`}, ""},
		{"relative", map[string]string{SettingsFile: `{"autoMemoryDirectory": "mem/dir"}`}, ""},
		{"other keys only", map[string]string{SettingsFile: `{"cleanupPeriodDays": 3}`}, ""},
		{"invalid json ignored", map[string]string{SettingsFile: `{oops`}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMemoryEnv(t)
			cfg := t.TempDir()
			for name, content := range c.files {
				writeFile(t, filepath.Join(cfg, name), content)
			}
			root := Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg}
			proj := t.TempDir()
			got, err := MemoryDirFor(root, proj)
			if err != nil {
				t.Fatalf("MemoryDirFor: %v", err)
			}
			want := c.want
			if want == "" {
				slug, err := MemorySlug(proj)
				if err != nil {
					t.Fatal(err)
				}
				want = filepath.Join(root.Dir, slug, MemorySubdir)
			}
			if got != want {
				t.Errorf("MemoryDirFor = %q, want %q", got, want)
			}
		})
	}
}

// quoteJSON quotes s as a JSON string literal (backslashes escaped for
// Windows paths).
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestMemoryDirForEmptyConfigDirSkipsSettings(t *testing.T) {
	clearMemoryEnv(t)
	// A zero ConfigDir must not make MemoryDirFor read ./settings.json.
	got, err := MemoryDirFor(Root{Dir: filepath.Join(t.TempDir(), "projects")}, t.TempDir())
	if err != nil || got == "" {
		t.Fatalf("MemoryDirFor = (%q, %v)", got, err)
	}
}
