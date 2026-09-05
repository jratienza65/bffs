package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/skillpack"
)

func TestSkillInstallMessage(t *testing.T) {
	without := skillInstallMessage(false)
	want := `Installed skill "bffs-rehome" for Claude Code (user scope). Restart claude; invoke with /bffs-rehome, or just say "rehome the sessions I imported".`
	if without != want {
		t.Errorf("without full copies:\n want %q\n got  %q", want, without)
	}
	with := skillInstallMessage(true)
	want = `Installed skill "bffs-rehome" for Claude Code (user scope; full-isolation accounts got their own copy). Restart claude; invoke with /bffs-rehome, or just say "rehome the sessions I imported".`
	if with != want {
		t.Errorf("with full copies:\n want %q\n got  %q", want, with)
	}
}

func TestSkillInstallHintText(t *testing.T) {
	if skillInstallHint != "Optional: bffs skill install (adds /bffs-rehome for imported sessions)" {
		t.Errorf("hint text changed: %q", skillInstallHint)
	}
}

// noClaude makes shim.FindRealClaude fail: no override, an empty PATH and a
// config dir without a cached real-claude.path.
func noClaude(t *testing.T) string {
	t.Helper()
	t.Setenv(shim.EnvRealClaude, "")
	t.Setenv("PATH", t.TempDir())
	return t.TempDir()
}

func TestValidateInstalledSkillSkippedWithoutClaude(t *testing.T) {
	cfg := noClaude(t)
	warnings, ran := validateInstalledSkill(context.Background(), cfg, t.TempDir())
	if ran || warnings != nil {
		t.Errorf("want skipped silently, got ran=%v warnings=%v", ran, warnings)
	}

	// An override that points nowhere is skipped the same way.
	t.Setenv(shim.EnvRealClaude, filepath.Join(t.TempDir(), "missing-claude"))
	warnings, ran = validateInstalledSkill(context.Background(), cfg, t.TempDir())
	if ran || warnings != nil {
		t.Errorf("missing override: want skipped silently, got ran=%v warnings=%v", ran, warnings)
	}
}

func fakeValidator(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude")
	}
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv(shim.EnvRealClaude, script)
}

func TestValidateInstalledSkillFiltersToTheSkill(t *testing.T) {
	// $3 is the dir argument of `plugin validate <dir> --json`. The script
	// refuses to run with the transfer code in its environment.
	fakeValidator(t, `[ -z "$BFFS_TRANSFER_CODE" ] || { echo "leaked" >&2; exit 7; }
[ "$1 $2 $4" = "plugin validate --json" ] || { echo "bad args: $*" >&2; exit 8; }
printf '{"success":false,"strict":false,"target":"%s","manifest":null,"contents":[{"file":"%s/skills/bffs-rehome/SKILL.md","type":"skill","errors":[],"warnings":[{"path":"allowed-tools","message":"Unknown tool Frobnicate","code":null}]},{"file":"%s/commands/foo.md","type":"command","errors":[],"warnings":[{"path":"description","message":"No description in frontmatter","code":null}]},{"file":"%s/skills","type":"skill","errors":[],"warnings":[{"path":"directory","message":"1 entry here is a symlink and was not read","code":null}]}]}' "$3" "$3" "$3" "$3"
exit 1
`)
	t.Setenv("BFFS_TRANSFER_CODE", "7K3Q-M9XD")
	home := t.TempDir()
	warnings, ran := validateInstalledSkill(context.Background(), t.TempDir(), home)
	if !ran {
		t.Fatalf("validator not run")
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly the skill's own finding, got %v", warnings)
	}
	if want := "claude plugin validate: SKILL.md: allowed-tools: Unknown tool Frobnicate"; warnings[0] != want {
		t.Errorf("warning:\n want %q\n got  %q", want, warnings[0])
	}
	for _, w := range warnings {
		if strings.Contains(w, "7K3Q") || strings.Contains(w, "leaked") {
			t.Errorf("transfer code reached the validator: %q", w)
		}
	}
}

func TestValidateInstalledSkillReportsUnparseableFailure(t *testing.T) {
	fakeValidator(t, "echo 'could not find a real claude' >&2\nexit 1\n")
	warnings, ran := validateInstalledSkill(context.Background(), t.TempDir(), t.TempDir())
	if !ran || len(warnings) != 1 || !strings.Contains(warnings[0], "could not find a real claude") {
		t.Errorf("want one warning with the failure line, got ran=%v %v", ran, warnings)
	}
	// A clean exit without a report is silent.
	fakeValidator(t, "echo not json\nexit 0\n")
	warnings, ran = validateInstalledSkill(context.Background(), t.TempDir(), t.TempDir())
	if !ran || len(warnings) != 0 {
		t.Errorf("clean exit without a report: want no warnings, got ran=%v %v", ran, warnings)
	}
	// Control sequences in the failure line never reach the terminal.
	fakeValidator(t, "printf 'bad \\033[2J\\033]52;c;xyz\\007 thing\\n' >&2\nexit 1\n")
	warnings, ran = validateInstalledSkill(context.Background(), t.TempDir(), t.TempDir())
	if !ran || len(warnings) != 1 || warnings[0] != "claude plugin validate: bad  thing" {
		t.Errorf("escape sequences not stripped: ran=%v %q", ran, warnings)
	}
}

func TestValidateInstalledSkillTimesOut(t *testing.T) {
	// A validator that never answers (a bffs shim exec'ing itself forever
	// looks exactly like this) is killed and reported, never waited on.
	fakeValidator(t, "exec sleep 30\n")
	defer func(d time.Duration) { skillValidateTimeout = d }(skillValidateTimeout)
	skillValidateTimeout = 200 * time.Millisecond

	start := time.Now()
	warnings, ran := validateInstalledSkill(context.Background(), t.TempDir(), t.TempDir())
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("validator not killed at the timeout: took %s", took)
	}
	if !ran || len(warnings) != 1 || !strings.Contains(warnings[0], "no result after 200ms") || !strings.Contains(warnings[0], "not checked") {
		t.Errorf("want one timeout warning, got ran=%v %v", ran, warnings)
	}
}

func TestParsePluginValidateSanitizesMessages(t *testing.T) {
	home := t.TempDir()
	skillDir := skillpack.SkillDir(home)
	// \u001b (ESC) and \u0007 (BEL) are JSON escapes: a CSI colour change in
	// the path and an OSC title write plus a newline in the message.
	raw := `{"success":false,"manifest":null,"contents":[{"file":"` + filepath.ToSlash(home) + `/skills/bffs-rehome/SKILL.md","type":"skill","errors":[{"path":"na\u001b[31mme","message":"bad\u001b]0;evil\u0007 name\n"}],"warnings":[]}]}`
	got, ok := parsePluginValidate([]byte(raw), skillDir)
	if !ok || len(got) != 1 || got[0] != "claude plugin validate: SKILL.md: name: bad name" {
		t.Errorf("want the sanitized finding, got ok=%v %q", ok, got)
	}
}

func TestParsePluginValidate(t *testing.T) {
	home := t.TempDir()
	skillDir := skillpack.SkillDir(home)
	j := func(s string) string { return strings.ReplaceAll(s, "HOME", filepath.ToSlash(home)) }

	cases := []struct {
		name string
		raw  string
		ok   bool
		want []string
	}{
		{"not json", "Validating components in: /x\n", false, nil},
		{"clean", j(`{"success":true,"manifest":null,"contents":[]}`), true, nil},
		{"skill file finding", j(`{"success":false,"manifest":null,"contents":[{"file":"HOME/skills/bffs-rehome/SKILL.md","type":"skill","errors":[{"path":"name","message":"bad name"}],"warnings":[]}]}`), true,
			[]string{"claude plugin validate: SKILL.md: name: bad name"}},
		{"reference finding", j(`{"success":true,"manifest":null,"contents":[{"file":"HOME/skills/bffs-rehome/references/rehome-checklist.md","type":"skill","errors":[],"warnings":[{"path":"","message":"odd"}]}]}`), true,
			[]string{"claude plugin validate: references/rehome-checklist.md: odd"}},
		{"neighbour skill ignored", j(`{"success":false,"manifest":null,"contents":[{"file":"HOME/skills/other/SKILL.md","type":"skill","errors":[{"path":"name","message":"bad"}],"warnings":[]}]}`), true, nil},
		{"skills dir mention by name", j(`{"success":true,"manifest":null,"contents":[{"file":"HOME/skills","type":"skill","errors":[],"warnings":[{"path":"directory","message":"bffs-rehome: unreadable"},{"path":"directory","message":"symlink skipped"}]}]}`), true,
			[]string{"claude plugin validate: skills: directory: bffs-rehome: unreadable"}},
		{"manifest about the skill dir", j(`{"success":false,"manifest":{"file":"HOME/skills/bffs-rehome","type":"plugin","errors":[{"path":"directory","message":"No manifest found"}],"warnings":[]},"contents":[]}`), true,
			[]string{"claude plugin validate: bffs-rehome: directory: No manifest found"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePluginValidate([]byte(tc.raw), skillDir)
			if ok != tc.ok {
				t.Fatalf("ok: want %v, got %v", tc.ok, ok)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("warning %d:\n want %q\n got  %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}

func TestEnvWithout(t *testing.T) {
	env := []string{"A=1", "BFFS_TRANSFER_CODE=x", "bffs_transfer_code=y", "B=2"}
	got := envWithout(env, transferCodeEnv)
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("envWithout: %v", got)
	}
}
