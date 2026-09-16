package transcripts

import (
	"errors"
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

func TestMemoryDirForEnvOverride(t *testing.T) {
	for _, env := range []string{EnvRemoteMemoryDir, EnvCoworkMemoryPathOverride} {
		t.Run(env, func(t *testing.T) {
			clearMemoryEnv(t)
			t.Setenv(env, filepath.Join(t.TempDir(), "elsewhere"))
			cfg := t.TempDir()
			_, err := MemoryDirFor(Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg}, t.TempDir())
			if !errors.Is(err, ErrMemoryDirOverridden) {
				t.Fatalf("err = %v, want ErrMemoryDirOverridden", err)
			}
			if !strings.Contains(err.Error(), env) {
				t.Errorf("error %q should name %s", err, env)
			}
		})
	}
}

func TestMemoryDirForSettingsOverride(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		wantErr bool
	}{
		{"user settings", SettingsFile, `{"autoMemoryDirectory": "~/mem"}`, true},
		{"local settings", LocalSettingsFile, `{"autoMemoryDirectory": "/srv/mem"}`, true},
		{"empty string is unset", SettingsFile, `{"autoMemoryDirectory": ""}`, false},
		{"other keys only", SettingsFile, `{"cleanupPeriodDays": 3}`, false},
		{"invalid json ignored", SettingsFile, `{oops`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMemoryEnv(t)
			cfg := t.TempDir()
			writeFile(t, filepath.Join(cfg, c.file), c.content)
			root := Root{Dir: filepath.Join(cfg, "projects"), ConfigDir: cfg}
			got, err := MemoryDirFor(root, t.TempDir())
			if c.wantErr {
				if !errors.Is(err, ErrMemoryDirOverridden) {
					t.Fatalf("err = %v, want ErrMemoryDirOverridden", err)
				}
				if !strings.Contains(err.Error(), c.file) {
					t.Errorf("error %q should name %s", err, c.file)
				}
				return
			}
			if err != nil {
				t.Fatalf("MemoryDirFor: %v", err)
			}
			if !strings.HasPrefix(got, root.Dir) {
				t.Errorf("memory dir %q not under root %q", got, root.Dir)
			}
		})
	}
}

func TestMemoryDirForEmptyConfigDirSkipsSettings(t *testing.T) {
	clearMemoryEnv(t)
	// A zero ConfigDir must not make MemoryDirFor read ./settings.json.
	got, err := MemoryDirFor(Root{Dir: filepath.Join(t.TempDir(), "projects")}, t.TempDir())
	if err != nil || got == "" {
		t.Fatalf("MemoryDirFor = (%q, %v)", got, err)
	}
}
