package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
)

// The bare `sessions` and `memory` commands open the browser only on a
// terminal: under go test (stdout is a pipe) they print the table with no
// error, exactly as they do with --plain or in a bffs_notui build. The
// flag exists on both bare commands and on nothing else.
func TestBareCatalogCommandsFallBackToTable(t *testing.T) {
	if tuiSupported() {
		t.Fatal("tuiSupported() must be false without a terminal on stdout")
	}
	if sessionsCmd.Flags().Lookup("plain") == nil || memoryCmd.Flags().Lookup("plain") == nil {
		t.Fatal("--plain missing on a bare catalog command")
	}
	if sessionsListCmd.Flags().Lookup("plain") != nil || memoryListCmd.Flags().Lookup("plain") != nil {
		t.Fatal("--plain must not leak onto the list subcommands")
	}

	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}})
	f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "first prompt of one", time.Now().Add(-time.Hour))
	t.Setenv("BFFS_HOME", f.cfgDir)
	t.Chdir(f.project)

	prevSessions, prevMemory := sessionsClaudeDir, memoryClaudeDir
	sessionsClaudeDir, memoryClaudeDir = f.claudeDir, f.claudeDir
	t.Cleanup(func() {
		sessionsClaudeDir, memoryClaudeDir = prevSessions, prevMemory
		sessionsPlain, memoryPlain = false, false
		sessionsCmd.SetOut(nil)
		memoryCmd.SetOut(nil)
	})

	for _, plain := range []bool{false, true} {
		sessionsPlain, memoryPlain = plain, plain
		var out bytes.Buffer
		sessionsCmd.SetOut(&out)
		if err := sessionsCmd.RunE(sessionsCmd, nil); err != nil {
			t.Fatalf("sessions (plain=%v): %v", plain, err)
		}
		if got := out.String(); !strings.Contains(got, "first prompt of one") || !strings.Contains(got, "TITLE") || strings.Contains(got, "\x1b[") {
			t.Errorf("sessions (plain=%v) did not print the table:\n%s", plain, got)
		}
		out.Reset()
		memoryCmd.SetOut(&out)
		if err := memoryCmd.RunE(memoryCmd, nil); err != nil {
			t.Fatalf("memory (plain=%v): %v", plain, err)
		}
		if got := out.String(); !strings.Contains(got, "memory") || strings.Contains(got, "\x1b[") {
			t.Errorf("memory (plain=%v) did not print the table:\n%s", plain, got)
		}
	}
}
