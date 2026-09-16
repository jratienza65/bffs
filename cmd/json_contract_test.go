package cmd

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// --json is the contract; the human-readable tables are not. These
// tests pin the field names of every JSON surface: the Go types are
// where the schema is written down (sessionInfo, memoryInfo,
// imports.Record, trustJSONBlock), and a rename or a removal has to
// fail here before it reaches a caller's jq expression or the MCP tool
// that returns the same shape.
//
// Two properties per surface: the document is an array — never null,
// even when there is nothing to report, so `| length` always works —
// and every object carries exactly the documented keys.

// jsonKeys is the union of the keys of every object in a JSON array,
// and the keys of one named nested array on each of them.
func jsonKeys(t *testing.T, doc string, nested string) (top, inner []string) {
	t.Helper()
	var rows []map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(doc))
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, doc)
	}
	if rows == nil {
		t.Errorf("the document is null; an empty result must still be []:\n%s", doc)
	}
	seen, seenInner := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		for k, v := range row {
			seen[k] = true
			if k != nested {
				continue
			}
			var sub []map[string]json.RawMessage
			if err := json.Unmarshal(v, &sub); err != nil {
				continue
			}
			for _, s := range sub {
				for k := range s {
					seenInner[k] = true
				}
			}
		}
	}
	for k := range seen {
		top = append(top, k)
	}
	for k := range seenInner {
		inner = append(inner, k)
	}
	sort.Strings(top)
	sort.Strings(inner)
	return top, inner
}

func wantKeys(t *testing.T, what string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s keys are the contract:\n got: %s\nwant: %s", what, strings.Join(got, " "), strings.Join(want, " "))
	}
}

func TestSessionsListJSONContract(t *testing.T) {
	var sb bytes.Buffer
	if err := writeSessionsJSON(&sb, nil); err != nil || strings.TrimSpace(sb.String()) != "[]" {
		t.Fatalf("empty = %q, %v — an empty listing must still be an array", sb.String(), err)
	}
	sb.Reset()
	s := transcripts.Session{
		ID: testSID1, Slug: "-home-d-p", Cwd: "/home/d/p", CwdExists: true, Title: "t", TitleSource: "custom",
		GitBranch: "main", Version: "2.1.259", Size: 10, Subagents: 1, Live: true,
		FirstTS: catalogNow.Add(-time.Hour), LastTS: catalogNow, Account: "work", AttribSource: "last-session",
		Root:   transcripts.Root{Owner: "work", Dir: "/home/d/.claude/projects", ConfigDir: "/home/d/.claude"},
		Import: &imports.SessionRef{Record: &imports.Record{BundleID: "b"}, Session: &imports.Session{OldCwd: "/old"}},
	}
	if err := writeSessionsJSON(&sb, []sessionBlock{{Sessions: []transcripts.Session{s}}}); err != nil {
		t.Fatal(err)
	}
	top, _ := jsonKeys(t, sb.String(), "")
	wantKeys(t, "sessions list --json", top, []string{
		"session_id", "root", "account", "account_source", "cwd", "cwd_exists", "slug",
		"title", "title_source", "git_branch", "claude_version", "first_at", "last_at",
		"size_bytes", "subagents", "live", "bundle_id", "old_cwd", "old_home", "old_host", "git_remote",
	})
}

func TestMemoryListJSONContract(t *testing.T) {
	var sb bytes.Buffer
	if err := writeMemoriesJSON(&sb, nil); err != nil || strings.TrimSpace(sb.String()) != "[]" {
		t.Fatalf("empty = %q, %v", sb.String(), err)
	}
	sb.Reset()
	m := transcripts.Memory{
		Root: transcripts.Root{Owner: "work"}, Cwd: "/home/d/p", CwdExists: true, GitRoot: "/home/d/p",
		Slug: "-home-d-p", Dir: "/home/d/.claude/projects/-home-d-p/memory", HasIndex: true,
		Files: []transcripts.MemoryFile{{Name: "MEMORY.md", Size: 12, ModTime: catalogNow, Pinned: true,
			AbsolutePaths: []string{"/x"}, AtRefs: []string{"@~/y"}}},
	}
	if err := writeMemoriesJSON(&sb, []transcripts.Memory{m}); err != nil {
		t.Fatal(err)
	}
	top, files := jsonKeys(t, sb.String(), "files")
	wantKeys(t, "memory list --json", top, []string{"root", "cwd", "cwd_exists", "git_root", "slug", "dir", "has_index", "files"})
	wantKeys(t, "memory list --json files[]", files, []string{"name", "size_bytes", "modified_at", "pinned", "absolute_paths", "at_refs"})
}

func TestImportsJSONContract(t *testing.T) {
	var sb bytes.Buffer
	if err := writeImportsJSON(&sb, nil); err != nil || strings.TrimSpace(sb.String()) != "[]" {
		t.Fatalf("empty = %q, %v", sb.String(), err)
	}
	sb.Reset()
	rec := imports.Record{
		BundleID: testBundleID, Kind: imports.KindImport, ImportedAt: catalogNow, Account: "work",
		DestRoot: "/home/d/.claude", Source: imports.Source{Hostname: "mac-a"},
		Sessions: []imports.Session{{ID: testSID1, Status: imports.StatusPending}},
	}
	if err := writeImportsJSON(&sb, []imports.Record{rec}); err != nil {
		t.Fatal(err)
	}
	top, sessions := jsonKeys(t, sb.String(), "sessions")
	wantKeys(t, "sessions imports --json", top, []string{"bundle_id", "kind", "imported_at", "account", "dest_root", "source", "sessions"})
	if len(sessions) == 0 {
		t.Error("the session records carry no keys")
	}
}

func TestTrustJSONContract(t *testing.T) {
	var sb bytes.Buffer
	blocks := []trustBlock{{
		Key: "/home/d/p",
		Statuses: []trust.Status{{Account: "work", File: "/home/d/.claude.json", Present: true,
			Folder: trust.Inherited, InheritedFrom: "/home/d", External: trust.Declined, Tools: 2, MCPEnabled: 1}},
		Live: []liveLine{{PID: 42, SessionID: testSID1, Cwd: "/home/d/p"}},
	}}
	if err := renderTrustJSON(&sb, blocks); err != nil {
		t.Fatal(err)
	}
	top, accounts := jsonKeys(t, sb.String(), "accounts")
	wantKeys(t, "trust --json", top, []string{"project_key", "accounts", "live"})
	wantKeys(t, "trust --json accounts[]", accounts, []string{"account", "file", "present", "folder", "external", "inherited_from", "tools", "mcp_enabled"})
}
