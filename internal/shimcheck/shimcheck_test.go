package shimcheck

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mkExec creates an executable file at dir/name and returns its path.
func mkExec(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnalyzeWinsWhenInstallDirComesFirst(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	otherDir := filepath.Join(root, "other")
	mkExec(t, otherDir, "claude")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := Analyze(strings.Join([]string{shimDir, otherDir}, string(filepath.ListSeparator)), shimDir, "", "claude")
	if got.Status != StatusWins {
		t.Errorf("status = %q, want %q", got.Status, StatusWins)
	}
}

// The dangerous case: the install dir IS on PATH, so everything looks
// installed, but a real claude earlier on PATH is what actually runs.
func TestAnalyzeShadowedWhenRealClaudeComesFirst(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	otherDir := filepath.Join(root, "other")
	blocker := mkExec(t, otherDir, "claude")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := Analyze(strings.Join([]string{otherDir, shimDir}, string(filepath.ListSeparator)), shimDir, "", "claude")
	if got.Status != StatusShadowed {
		t.Fatalf("status = %q, want %q", got.Status, StatusShadowed)
	}
	if got.Blocker != blocker {
		t.Errorf("blocker = %q, want %q", got.Blocker, blocker)
	}
}

// Nothing named claude anywhere on PATH, and the install dir absent too.
func TestAnalyzeNotOnPathWhenNoClaudeExists(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := Analyze(otherDir, shimDir, "", "claude")
	if got.Status != StatusNotOnPath {
		t.Errorf("status = %q, want %q", got.Status, StatusNotOnPath)
	}
	if got.InstallDirOnPath {
		t.Error("InstallDirOnPath = true, but the install dir is not in PATH")
	}
}

// "Install dir on PATH but too late" and "install dir absent" are both
// Shadowed, distinguished by InstallDirOnPath for remedy wording.
func TestAnalyzeRecordsWhetherInstallDirIsOnPathAtAll(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	otherDir := filepath.Join(root, "other")
	mkExec(t, otherDir, "claude")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)

	late := Analyze(otherDir+sep+shimDir, shimDir, "", "claude")
	if late.Status != StatusShadowed || !late.InstallDirOnPath {
		t.Errorf("late: status=%q onPath=%v, want shadowed/true", late.Status, late.InstallDirOnPath)
	}
	absent := Analyze(otherDir, shimDir, "", "claude")
	if absent.Status != StatusShadowed || absent.InstallDirOnPath {
		t.Errorf("absent: status=%q onPath=%v, want shadowed/false", absent.Status, absent.InstallDirOnPath)
	}
}

// A `claude` on PATH that resolves to the bffs binary is our own shim, not a
// competitor — otherwise a working install would report itself as blocked.
func TestAnalyzeOwnShimElsewhereIsNotABlocker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-based self-detection is exercised on unix")
	}
	root := t.TempDir()
	self := mkExec(t, filepath.Join(root, "opt"), "bffs")
	shimDir := filepath.Join(root, "shim")
	earlyDir := filepath.Join(root, "early")
	if err := os.MkdirAll(earlyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(earlyDir, "claude")); err != nil {
		t.Fatal(err)
	}

	got := Analyze(strings.Join([]string{earlyDir, shimDir}, string(filepath.ListSeparator)), shimDir, self, "claude")
	if got.Status != StatusWins {
		t.Errorf("status = %q (blocker %q), want %q", got.Status, got.Blocker, StatusWins)
	}
}

func TestAnalyzeSkipsDirectoriesAndNonExecutables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	// A *directory* named claude, and a non-executable file named claude.
	dirDir := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(dirDir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	nonExec := filepath.Join(root, "b")
	if err := os.MkdirAll(nonExec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonExec, "claude"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got := Analyze(strings.Join([]string{dirDir, nonExec, shimDir}, string(filepath.ListSeparator)), shimDir, "", "claude")
	if got.Status != StatusWins {
		t.Errorf("status = %q, want %q — neither a dir nor a non-executable should block", got.Status, StatusWins)
	}
}

// fakeProber returns a canned PATH per mode.
type fakeProber struct {
	byMode map[Mode]string
	err    map[Mode]error
}

func (f fakeProber) PATH(_ context.Context, m Mode) (string, error) {
	if e, ok := f.err[m]; ok {
		return "", e
	}
	return f.byMode[m], nil
}

// The whole point of the package: a shim that wins interactively but loses in
// login and non-interactive modes must be reported as blocked, not as fine.
func TestCheckCatchesModeSpecificShadowing(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	brewDir := filepath.Join(root, "brew")
	mkExec(t, brewDir, "claude")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)

	rep := Check(context.Background(), Options{
		InstallDir: shimDir,
		ShimName:   "claude",
		Shell:      "zsh",
		Prober: fakeProber{byMode: map[Mode]string{
			ModeInteractive:    shimDir + sep + brewDir, // .zshrc prepends
			ModeLogin:          brewDir + sep + shimDir, // .zprofile appends
			ModeNonInteractive: brewDir,                 // shim dir absent entirely
		}},
	})

	if rep.AllWin() {
		t.Error("AllWin() = true, but login and non-interactive are shadowed")
	}
	if !rep.Blocked() {
		t.Error("Blocked() = false, want true")
	}
	want := map[Mode]Status{
		ModeInteractive:    StatusWins,
		ModeLogin:          StatusShadowed,
		ModeNonInteractive: StatusShadowed,
	}
	for _, m := range rep.Modes {
		if want[m.Mode] != m.Status {
			t.Errorf("%s: status = %q, want %q", m.Mode, m.Status, want[m.Mode])
		}
	}
	if n := len(rep.Failing()); n != 2 {
		t.Errorf("Failing() returned %d modes, want 2", n)
	}
}

// Unknown must never be laundered into success.
func TestCheckUnknownIsNotSuccess(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	rep := Check(context.Background(), Options{
		InstallDir: shimDir,
		ShimName:   "claude",
		Shell:      "zsh",
		Prober: fakeProber{err: map[Mode]error{
			ModeInteractive:    context.DeadlineExceeded,
			ModeLogin:          context.DeadlineExceeded,
			ModeNonInteractive: context.DeadlineExceeded,
		}},
	})
	if rep.AllWin() {
		t.Error("AllWin() = true on an all-unknown report")
	}
	if rep.Blocked() {
		t.Error("Blocked() = true on an all-unknown report; unknown is not observed breakage")
	}
	for _, m := range rep.Modes {
		if m.Status != StatusUnknown {
			t.Errorf("%s: status = %q, want %q", m.Mode, m.Status, StatusUnknown)
		}
	}
}

func TestUnsupportedShellReportsUnknownNotFalseConfidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("probe is unavailable on Windows for a different reason")
	}
	rep := Check(context.Background(), Options{
		InstallDir: t.TempDir(),
		ShimName:   "claude",
		Shell:      "fish",
	})
	for _, m := range rep.Modes {
		if m.Status != StatusUnknown {
			t.Errorf("%s: status = %q, want %q for an unmodelled shell", m.Mode, m.Status, StatusUnknown)
		}
		if m.Note == "" {
			t.Errorf("%s: Unknown result carries no explanation", m.Mode)
		}
	}
}
