package skillpack

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/store"
)

func fullAccounts() store.Accounts {
	return accounts(
		store.Account{Name: "personal", Type: store.TypeOAuth, Isolation: store.IsolationPartial},
		store.Account{Name: "work", Type: store.TypeOAuth, Isolation: store.IsolationFull},
	)
}

func readSkill(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, SkillFile))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(dir, SkillFile), err)
	}
	return string(raw)
}

func TestInstallWritesEveryTargetAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	absCfg, _ := filepath.Abs(cfg)
	accs := fullAccounts()

	written, err := Install(home, cfg, accs, store.State{}, "9.9.9", false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	workSkill := SkillDir(filepath.Join(absCfg, "sessions", "work"))
	assertSameSet(t, "written", written, []string{SkillDir(home), workSkill})

	for _, dir := range written {
		doc := readSkill(t, dir)
		if !strings.Contains(doc, "version: 9.9.9") {
			t.Errorf("%s: version not stamped:\n%s", dir, doc[:200])
		}
		if !strings.Contains(doc, Marker) {
			t.Errorf("%s: marker missing", dir)
		}
		checklist := filepath.Join(dir, filepath.FromSlash(ChecklistFile))
		if _, err := os.Stat(checklist); err != nil {
			t.Errorf("%s: checklist missing: %v", dir, err)
		}
		if runtime.GOOS != "windows" {
			for _, f := range []string{filepath.Join(dir, SkillFile), checklist} {
				info, err := os.Stat(f)
				if err != nil {
					t.Fatal(err)
				}
				if perm := info.Mode().Perm(); perm != 0o644 {
					t.Errorf("%s: perm %o, want 0644", f, perm)
				}
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o755 {
				t.Errorf("%s: dir perm %o, want 0755", dir, perm)
			}
			// The never-launched session dir was created the way login does.
			sinfo, err := os.Stat(filepath.Join(absCfg, "sessions", "work"))
			if err != nil {
				t.Fatal(err)
			}
			if perm := sinfo.Mode().Perm(); perm != 0o700 {
				t.Errorf("session dir perm %o, want 0700", perm)
			}
		}
	}
	// The partial account got nothing of its own.
	if _, err := os.Stat(filepath.Join(cfg, "sessions", "personal")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("partial account's session dir was created: %v", err)
	}

	first := readSkill(t, SkillDir(home))
	again, err := Install(home, cfg, accs, store.State{}, "9.9.9", false)
	if err != nil {
		t.Fatalf("Install (again): %v", err)
	}
	if len(again) != len(written) {
		t.Errorf("re-install wrote %d dirs, want %d", len(again), len(written))
	}
	if second := readSkill(t, SkillDir(home)); second != first {
		t.Errorf("re-install changed the content")
	}
	// A new version restamps.
	if _, err := Install(home, cfg, accs, store.State{}, "10.0.0", false); err != nil {
		t.Fatalf("Install (new version): %v", err)
	}
	if doc := readSkill(t, SkillDir(home)); !strings.Contains(doc, "version: 10.0.0") || strings.Contains(doc, "9.9.9") {
		t.Errorf("version not restamped")
	}
}

func TestInstallRefusesUserSkillUnlessForced(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	accs := fullAccounts()

	// A user-authored skill of the same name on the full-isolation account.
	absCfg, _ := filepath.Abs(cfg)
	userDir := SkillDir(filepath.Join(absCfg, "sessions", "work"))
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, SkillFile), []byte("---\nname: bffs-rehome\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := Install(home, cfg, accs, store.State{}, "1.0.0", false)
	if err == nil {
		t.Fatalf("Install over a user skill succeeded: %v", written)
	}
	want := "a skill named \"bffs-rehome\" already exists at " + userDir + " and was not installed by bffs; --force overwrites it"
	if err.Error() != want {
		t.Errorf("error:\n want %q\n got  %q", want, err.Error())
	}
	if len(written) != 0 {
		t.Errorf("refusal wrote %v", written)
	}
	// Nothing landed anywhere — the home target was checked before writes.
	if _, err := os.Stat(filepath.Join(SkillDir(home), SkillFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home skill written despite refusal: %v", err)
	}
	if doc := readSkill(t, userDir); doc != "---\nname: bffs-rehome\n---\nmine\n" {
		t.Errorf("user skill modified: %q", doc)
	}

	written, err = Install(home, cfg, accs, store.State{}, "1.0.0", true)
	if err != nil {
		t.Fatalf("Install --force: %v", err)
	}
	if len(written) != 2 {
		t.Errorf("--force wrote %v", written)
	}
	if doc := readSkill(t, userDir); !strings.Contains(doc, Marker) || strings.Contains(doc, "mine") {
		t.Errorf("--force did not overwrite the user skill")
	}
}

func TestUninstallRemovesManagedOnly(t *testing.T) {
	home := t.TempDir()
	cfg := t.TempDir()
	accs := fullAccounts()
	absCfg, _ := filepath.Abs(cfg)
	workSkill := SkillDir(filepath.Join(absCfg, "sessions", "work"))

	if _, err := Install(home, cfg, accs, store.State{}, "1.0.0", false); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Replace the full account's copy with a user-authored one.
	if err := os.WriteFile(filepath.Join(workSkill, SkillFile), []byte("---\nname: bffs-rehome\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Uninstall(home, cfg, accs, store.State{})
	var nm *NotManagedError
	if !errors.As(err, &nm) {
		t.Fatalf("want *NotManagedError, got %v", err)
	}
	if len(nm.Paths) != 1 || nm.Paths[0] != workSkill {
		t.Errorf("reported paths: %v", nm.Paths)
	}
	if !strings.Contains(err.Error(), workSkill) || !strings.Contains(err.Error(), "not installed by bffs") {
		t.Errorf("error text: %v", err)
	}
	if len(removed) != 1 || removed[0] != SkillDir(home) {
		t.Errorf("removed: %v", removed)
	}
	if _, err := os.Stat(SkillDir(home)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home skill dir still present: %v", err)
	}
	if doc := readSkill(t, workSkill); doc != "---\nname: bffs-rehome\n---\nmine\n" {
		t.Errorf("user skill touched: %q", doc)
	}
	// Sibling skills are untouched.
	other := filepath.Join(home, "skills", "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err = Uninstall(home, cfg, store.Accounts{}, store.State{})
	if err != nil {
		t.Fatalf("Uninstall (nothing managed): %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("second uninstall removed %v", removed)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("sibling skill removed: %v", err)
	}
}
