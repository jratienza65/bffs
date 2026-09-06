package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
)

// The browser lives behind bare `bffs` and only on a terminal: under go
// test (stdout is a pipe) the root command prints its help, and the bare
// `sessions` and `memory` commands print their tables — the same as a
// bffs_notui build. Neither carries a --plain flag any more.
func TestBrowserEntryPoints(t *testing.T) {
	if tuiSupported() {
		t.Fatal("tuiSupported() must be false without a terminal on stdout")
	}
	if sessionsCmd.Flags().Lookup("plain") != nil || memoryCmd.Flags().Lookup("plain") != nil {
		t.Fatal("--plain must be gone from the bare catalog commands")
	}

	f := newCatalogFixture(t, store.Accounts{Accounts: map[string]store.Account{"work": {Type: store.TypeOAuth}}})
	f.transcript(filepath.Join(f.claudeDir, "projects"), testSID1, "first prompt of one", time.Now().Add(-time.Hour))
	t.Setenv("BFFS_HOME", f.cfgDir)
	t.Chdir(f.project)

	prevSessions, prevMemory := sessionsClaudeDir, memoryClaudeDir
	sessionsClaudeDir, memoryClaudeDir = f.claudeDir, f.claudeDir
	t.Cleanup(func() {
		sessionsClaudeDir, memoryClaudeDir = prevSessions, prevMemory
		sessionsCmd.SetOut(nil)
		memoryCmd.SetOut(nil)
		rootCmd.SetOut(nil)
	})

	var out bytes.Buffer
	sessionsCmd.SetOut(&out)
	if err := sessionsCmd.RunE(sessionsCmd, nil); err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "first prompt of one") || !strings.Contains(got, "TITLE") || strings.Contains(got, "\x1b[") {
		t.Errorf("sessions did not print the table:\n%s", got)
	}
	out.Reset()
	memoryCmd.SetOut(&out)
	if err := memoryCmd.RunE(memoryCmd, nil); err != nil {
		t.Fatalf("memory: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "memory") || strings.Contains(got, "\x1b[") {
		t.Errorf("memory did not print the table:\n%s", got)
	}
	out.Reset()
	rootCmd.SetOut(&out)
	if err := rootCmd.RunE(rootCmd, nil); err != nil {
		t.Fatalf("bare bffs: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "sessions") {
		t.Errorf("bare bffs without a terminal should print the help:\n%s", got)
	}
}
