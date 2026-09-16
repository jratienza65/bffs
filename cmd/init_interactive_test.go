package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/shimcheck"
	"github.com/spf13/cobra"
)

// newTestCmd returns a cobra command wired to the given stdin, so the
// interactive helpers can be driven without a terminal.
func newTestCmd(stdin string) (*cobra.Command, *prompter, *bytes.Buffer) {
	c := &cobra.Command{}
	out := &bytes.Buffer{}
	c.SetOut(out)
	c.SetErr(out)
	c.SetIn(strings.NewReader(stdin))
	return c, newPrompter(c.InOrStdin(), out), out
}

func blockedReport(installDir, blocker string) shimcheck.Report {
	r := shimcheck.Report{InstallDir: installDir, ShimName: "claude", Shell: "zsh"}
	for _, m := range shimcheck.AllModes {
		r.Modes = append(r.Modes, shimcheck.ModeResult{Mode: m, Status: shimcheck.StatusShadowed, Blocker: blocker})
	}
	return r
}

func winningReport(installDir string) shimcheck.Report {
	r := shimcheck.Report{InstallDir: installDir, ShimName: "claude", Shell: "zsh"}
	for _, m := range shimcheck.AllModes {
		r.Modes = append(r.Modes, shimcheck.ModeResult{Mode: m, Status: shimcheck.StatusWins})
	}
	return r
}

func TestChooseInstallDirEmptyInputTakesDefault(t *testing.T) {
	def := t.TempDir()
	c, pr, _ := newTestCmd("\n")
	got, err := chooseInstallDir(pr, c, def, blockedReport(def, "/usr/bin/claude"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != def {
		t.Errorf("dir = %q, want default %q", got, def)
	}
}

func TestChooseInstallDirCustomPath(t *testing.T) {
	def := t.TempDir()
	custom := filepath.Join(t.TempDir(), "mybin")
	rep := blockedReport(def, "/usr/bin/claude")
	// With no SafeDirs, options are [default] + custom, so custom is "2".
	c, pr, _ := newTestCmd("2\n" + custom + "\n")
	got, err := chooseInstallDir(pr, c, def, rep, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != custom {
		t.Errorf("dir = %q, want %q", got, custom)
	}
}

func TestChooseInstallDirRejectsGarbage(t *testing.T) {
	def := t.TempDir()
	c, pr, _ := newTestCmd("banana\n")
	if _, err := chooseInstallDir(pr, c, def, blockedReport(def, "/usr/bin/claude"), ""); err == nil {
		t.Error("expected an error for a non-numeric choice")
	}
}

// A blocked report must not be installable non-interactively without --force:
// a silently useless install is the failure mode this check exists to close.
func TestConfirmOrRefuseFailsNonInteractively(t *testing.T) {
	initForce = false
	def := t.TempDir()
	c, pr, _ := newTestCmd("")
	err := confirmOrRefuse(pr, c, blockedReport(def, "/usr/bin/claude"), false)
	if err == nil {
		t.Fatal("expected refusal for a blocked report")
	}
	if !strings.Contains(err.Error(), "would not be") {
		t.Errorf("error should explain the verdict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/usr/bin/claude") {
		t.Errorf("error should name the blocker, got: %v", err)
	}
}

func TestConfirmOrRefuseInteractiveDeclineAborts(t *testing.T) {
	initForce = false
	def := t.TempDir()
	c, pr, _ := newTestCmd("n\n")
	if err := confirmOrRefuse(pr, c, blockedReport(def, "/usr/bin/claude"), true); err == nil {
		t.Error("declining should abort")
	}
}

func TestConfirmOrRefuseInteractiveAcceptProceeds(t *testing.T) {
	initForce = false
	def := t.TempDir()
	c, pr, _ := newTestCmd("y\n")
	if err := confirmOrRefuse(pr, c, blockedReport(def, "/usr/bin/claude"), true); err != nil {
		t.Errorf("accepting should proceed, got %v", err)
	}
}

func TestConfirmOrRefusePassesWhenNotBlocked(t *testing.T) {
	initForce = false
	def := t.TempDir()
	c, pr, _ := newTestCmd("")
	if err := confirmOrRefuse(pr, c, winningReport(def), false); err != nil {
		t.Errorf("a winning report must not be refused, got %v", err)
	}
}

func TestForceBypassesRefusalButWarns(t *testing.T) {
	initForce = true
	defer func() { initForce = false }()
	def := t.TempDir()
	c, pr, out := newTestCmd("")
	if err := confirmOrRefuse(pr, c, blockedReport(def, "/usr/bin/claude"), false); err != nil {
		t.Fatalf("--force should proceed, got %v", err)
	}
	if !strings.Contains(out.String(), "warning") {
		t.Errorf("--force should warn that the shim will not run; output was %q", out.String())
	}
}

func TestExpandHomeResolvesTilde(t *testing.T) {
	got, err := expandHome("~/somewhere")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("tilde not expanded: %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("path not absolute: %q", got)
	}
}
