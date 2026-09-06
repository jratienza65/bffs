package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

func TestCompareMemory(t *testing.T) {
	t0 := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	ref := map[string]memFile{"MEMORY.md": {sha: "a", mtime: t0}, "notes.md": {sha: "b", mtime: t0}, "same.md": {sha: "s", mtime: t0}}
	other := map[string]memFile{"notes.md": {sha: "c", mtime: t0.Add(time.Hour)}, "same.md": {sha: "s", mtime: t0}, "extra.md": {sha: "e", mtime: t0}}
	state, diffs := compareMemory(ref, other)
	if state != "differs" || len(diffs) != 3 {
		t.Fatalf("state=%q diffs=%v", state, diffs)
	}
	want := []fileDrift{{"MEMORY.md", "only here"}, {"extra.md", "only there"}, {"notes.md", "differs (newer there)"}}
	for i, d := range diffs {
		if d != want[i] {
			t.Errorf("diff %d = %v, want %v", i, d, want[i])
		}
	}
	if state, diffs := compareMemory(ref, ref); state != "same" || diffs != nil {
		t.Errorf("same: %q %v", state, diffs)
	}
	if state, _ := compareMemory(ref, nil); state != "none" {
		t.Errorf("none: %q", state)
	}
	if state, _ := compareMemory(nil, other); state != "only there" {
		t.Errorf("only there: %q", state)
	}
	if state, diffs := compareMemory(map[string]memFile{"a": {sha: "1", mtime: t0.Add(time.Hour)}}, map[string]memFile{"a": {sha: "2", mtime: t0}}); state != "differs" || diffs[0].state != "differs (newer here)" {
		t.Errorf("newer here: %q %v", state, diffs)
	}
}

func TestDriftLines(t *testing.T) {
	pool := transcripts.Root{Dir: "/x/projects", ConfigDir: "/x", Shared: true, Accounts: []string{"a", "b"}}
	full := transcripts.Root{Dir: "/y/sessions/w/projects", ConfigDir: "/y/sessions/w", Owner: "w"}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	d := projectDrift{slug: "-p", cwd: "/p", key: "/p", ref: pool, sessions: 3, newest: now.Add(-time.Hour),
		roots: []rootDrift{
			{root: pool, here: true, sessions: 3, files: map[string]memFile{"MEMORY.md": {}, "n.md": {}}, state: "here"},
			{root: full, sessions: 1, files: map[string]memFile{"n.md": {}}, state: "differs", diffs: []fileDrift{{"MEMORY.md", "only here"}, {"n\x1b[2J.md", "differs (newer there)"}}},
		},
		accounts: []accountDrift{
			{status: trust.Status{Account: "a", Folder: trust.Accepted, External: trust.Accepted}, lastSession: "1e005053-380a-4245-a145-52c2715afa73", inProject: true},
			{status: trust.Status{Account: "b", Folder: trust.Declined}, lastSession: "b19c4e20-1111-4222-8333-444455556666"},
			{status: trust.Status{Account: "home", Folder: trust.Inherited, InheritedFrom: "/"}},
		},
		warnings: []string{"w\x1b[2Jarn"},
	}
	out := plain(strings.Join(driftLines(d, "a", now), "\n"))
	wantAll(t, out, "/p", "3 sessions · memory 2 files · newest 1h ago", "ACROSS ROOTS", "reference: shared pool: a, b",
		"shared pool: a, b", "3", "2 files",
		"account: w", "1", "differs — MEMORY.md only here, n.md differs (newer there)",
		"PER ACCOUNT", "← selected account",
		"a           ← ✓        ✓        1e005053 (this project)",
		"b             ✗        –        b19c4e20",
		"home          ✓*       –        –", "warning: warn",
		"t trust sync · L last-session pointer · S sync memory")
	wantNone(t, out, "\x1b]", "[2J", "(elsewhere)")
	if answerCell(trust.Unset, "") != "–" || answerCell(trust.Unset, "/") != "✓*" {
		t.Errorf("unset cells = %q %q", answerCell(trust.Unset, ""), answerCell(trust.Unset, "/"))
	}
}
