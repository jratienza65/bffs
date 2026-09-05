package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	sidRoot   = "00000000-0000-4000-8000-000000000001"
	sidLast   = "00000000-0000-4000-8000-000000000002"
	sidImport = "00000000-0000-4000-8000-000000000003"
	sidLaunch = "00000000-0000-4000-8000-000000000004"
	sidNone   = "00000000-0000-4000-8000-000000000005"
	sidHome   = "00000000-0000-4000-8000-000000000006"
	sidSkip   = "00000000-0000-4000-8000-000000000007"
	sidDouble = "00000000-0000-4000-8000-000000000008"
)

// saveImport writes one import record under the fixture's cfgDir.
func (f *fixture) saveImport(bundleID, account, status string, sids ...string) {
	f.t.Helper()
	rec := imports.Record{BundleID: bundleID, Kind: imports.KindImport, ImportedAt: fixedNow.Add(-time.Hour), Account: account}
	for _, sid := range sids {
		rec.Sessions = append(rec.Sessions, imports.Session{ID: sid, OldCwd: "/proj/a", NewCwd: "/proj/a", Status: status})
	}
	if err := imports.Save(f.cfgDir, rec); err != nil {
		f.t.Fatalf("imports.Save: %v", err)
	}
}

func (f *fixture) attributor() *Attributor {
	f.t.Helper()
	accs, err := store.LoadAccounts(f.cfgDir)
	if err != nil {
		f.t.Fatalf("LoadAccounts: %v", err)
	}
	a, err := NewAttributor(f.cfgDir, accs)
	if err != nil {
		f.t.Fatalf("NewAttributor: %v", err)
	}
	return a
}

// The tiers rank root > lastSessionId > import > launch-log: every session
// here has evidence in the tier below it too, and the higher one wins.
func TestAttributorOrdering(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(-time.Hour)
	// play launched from /proj/a right before every session started — the
	// launch-log answer for all of them, when nothing outranks it.
	f.launch(start.Add(-30*time.Second), "play", "/proj/a")
	f.sessionClaudeJSON("work", sidRoot, sidLast)
	f.saveImport("6f1e2c0a-0000-4000-8000-000000000001", "api", imports.StatusPlaced, sidLast, sidImport)
	f.saveImport("6f1e2c0a-0000-4000-8000-000000000002", "", imports.StatusPending, sidHome)
	f.saveImport("6f1e2c0a-0000-4000-8000-000000000003", "work", imports.StatusSkipped, sidSkip)
	f.sessionClaudeJSON("play", sidDouble)
	f.sessionClaudeJSON("work", sidRoot, sidLast, sidDouble)

	a := f.attributor()
	cases := []struct {
		name, sid, cwd, owner string
		ts                    time.Time
		wantAcct, wantSrc     string
	}{
		{"root owner beats everything", sidRoot, "/proj/a", "play", start, "play", SrcRoot},
		{"lastSessionId beats import and launch", sidLast, "/proj/a", "", start, "work", SrcLastSession},
		{"import beats launch", sidImport, "/proj/a", "", start, "api", SrcImport},
		{"import to unmanaged home", sidHome, "/proj/a", "", start, transcripts.HomeName, SrcImport},
		{"skipped import record is ignored", sidSkip, "/proj/a", "", start, "play", SrcLaunchLog},
		{"launch log", sidLaunch, "/proj/a", "", start, "play", SrcLaunchLog},
		{"launch log needs cwd", sidLaunch, "", "", start, "", ""},
		{"launch log needs a first timestamp", sidLaunch, "/proj/a", "", time.Time{}, "", ""},
		{"other cwd is undecided", sidNone, "/proj/b", "", start, "", ""},
		{"two lastSessionId claimants is corrupt", sidDouble, "/proj/a", "", start, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct, src := a.Attribute(tc.sid, tc.cwd, tc.ts, tc.owner)
			if acct != tc.wantAcct || src != tc.wantSrc {
				t.Errorf("Attribute = (%q, %q), want (%q, %q)", acct, src, tc.wantAcct, tc.wantSrc)
			}
		})
	}
	if got := a.Claimants(sidLast); len(got) != 1 || got[0] != "work" {
		t.Errorf("Claimants(sidLast) = %v, want [work]", got)
	}
	if got := a.Claimants(sidNone); len(got) != 0 {
		t.Errorf("Claimants(sidNone) = %v, want none", got)
	}
}

// Import records must not feed the (cwd, time) tier: an import placed under
// /proj/a at the time a session started is not a launch from /proj/a, and
// a session that the launch log alone attributes stays attributed no
// matter how many records mention its cwd.
func TestAttributorLaunchTierIgnoresImports(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(-time.Hour)
	f.launch(start.Add(-30*time.Second), "play", "/proj/a")
	// A record for a different session from the same cwd, imported under
	// another account at the very same time.
	rec := imports.Record{BundleID: "6f1e2c0a-0000-4000-8000-0000000000aa", Kind: imports.KindImport, ImportedAt: start, Account: "work",
		Sessions: []imports.Session{{ID: sidImport, OldCwd: "/proj/a", NewCwd: "/proj/a", Status: imports.StatusPlaced}}}
	if err := imports.Save(f.cfgDir, rec); err != nil {
		t.Fatal(err)
	}
	a := f.attributor()

	if acct, src := a.Attribute(sidLaunch, "/proj/a", start, ""); acct != "play" || src != SrcLaunchLog {
		t.Errorf("launch-log attribution disturbed by an import record: (%q, %q)", acct, src)
	}
	// Without any launch, the record's cwd and time never become a launch.
	if err := os.Remove(filepath.Join(f.cfgDir, "launches.jsonl")); err != nil {
		t.Fatal(err)
	}
	a = f.attributor()
	if acct, src := a.Attribute(sidNone, "/proj/a", start, ""); acct != "" || src != "" {
		t.Errorf("import record acted as a launch: (%q, %q)", acct, src)
	}
	// The imported session itself is still attributed — by tier 3, not 4.
	if acct, src := a.Attribute(sidImport, "/proj/a", start, ""); acct != "work" || src != SrcImport {
		t.Errorf("imported session: (%q, %q)", acct, src)
	}
}

// Collect is untouched by import records: the same fixture attributes the
// imported session only through the launch log there.
func TestCollectIgnoresImportRecords(t *testing.T) {
	f := newFixture(t)
	f.saveImport("6f1e2c0a-0000-4000-8000-0000000000bb", "work", imports.StatusPlaced, sidImport)
	f.writeTranscript(f.home, "-proj-a", sidImport+".jsonl",
		record(fixedNow.Add(-time.Hour), "msg-1", "req-1", "claude-opus-5", 1, 2, 3, 4))

	rep := f.collect()
	if rep.Unattributed.Sessions != 1 {
		t.Errorf("Collect must not consult import records; unattributed=%+v work=%+v", rep.Unattributed, f.account(rep, "work").Attribution)
	}
}

func TestNewAttributorEmptyDir(t *testing.T) {
	a, err := NewAttributor(t.TempDir(), store.Accounts{})
	if err != nil {
		t.Fatalf("NewAttributor: %v", err)
	}
	if acct, src := a.Attribute(sidNone, "/proj/a", fixedNow, ""); acct != "" || src != "" {
		t.Errorf("empty attributor decided (%q, %q)", acct, src)
	}
}
