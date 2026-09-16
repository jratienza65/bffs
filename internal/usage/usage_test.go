package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// fixture is a synthetic bffs config dir + fake home claude dir.
type fixture struct {
	t      *testing.T
	cfgDir string
	home   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, cfgDir: t.TempDir(), home: t.TempDir()}
	accs := store.Accounts{Accounts: map[string]store.Account{
		"work": {Type: store.TypeOAuth, OAuthAccountMeta: `{"userRateLimitTier":"max_20x","hasExtraUsageEnabled":false}`},
		"play": {Type: store.TypeOAuth},
		"api":  {Type: store.TypeAPIKey, Secret: "sk-ant-test-1234"},
	}}
	if err := store.SaveAccounts(f.cfgDir, accs); err != nil {
		t.Fatalf("SaveAccounts: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.home, "projects"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return f
}

func (f *fixture) collect() Report {
	f.t.Helper()
	rep, err := Collect(f.cfgDir, Options{Now: fixedNow, HomeClaudeDir: f.home})
	if err != nil {
		f.t.Fatalf("Collect: %v", err)
	}
	return rep
}

func (f *fixture) account(rep Report, name string) AccountUsage {
	f.t.Helper()
	for _, a := range rep.Accounts {
		if a.Name == name {
			return a
		}
	}
	f.t.Fatalf("account %q not in report", name)
	return AccountUsage{}
}

// writeTranscript writes lines to <root>/projects/<slug>/<rel> and stamps a
// fresh mtime (override with chtimes for horizon tests).
func (f *fixture) writeTranscript(root, slug, rel string, lines ...string) string {
	f.t.Helper()
	path := filepath.Join(root, "projects", slug, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatalf("write transcript: %v", err)
	}
	return path
}

// sessionClaudeJSON writes a per-account .claude.json claiming the given
// lastSessionIds.
func (f *fixture) sessionClaudeJSON(account string, sids ...string) {
	f.t.Helper()
	projects := map[string]any{}
	for i, sid := range sids {
		projects[fmt.Sprintf("/proj/%d", i)] = map[string]any{"lastSessionId": sid}
	}
	doc, err := json.Marshal(map[string]any{"projects": projects})
	if err != nil {
		f.t.Fatalf("marshal: %v", err)
	}
	dir := filepath.Join(f.cfgDir, "sessions", account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), doc, 0o600); err != nil {
		f.t.Fatalf("write .claude.json: %v", err)
	}
}

func (f *fixture) launch(ts time.Time, account, cwd string) {
	f.t.Helper()
	e := usagelog.Event{TS: ts, Account: account, Source: "global", Cwd: cwd}
	if account == "" {
		e.Source = "none"
	} else {
		e.Type = "oauth"
	}
	if err := usagelog.Append(f.cfgDir, e); err != nil {
		f.t.Fatalf("Append: %v", err)
	}
}

// record renders one assistant transcript line.
func record(ts time.Time, msgID, reqID, model string, in, out, cc, cr int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/proj/a","requestId":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d},"content":[{"type":"text","text":"x"}]}}`,
		ts.Format(time.RFC3339), reqID, msgID, model, in, out, cc, cr)
}

func errorRecord(ts time.Time, errKind, text string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/proj/a","isApiErrorMessage":true,"error":%q,"message":{"id":"","model":"","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":%q}]}}`,
		ts.Format(time.RFC3339), errKind, text)
}

func TestAttributionLastSessionID(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-1")
	f.writeTranscript(f.home, "-proj-a", "sid-1.jsonl",
		record(fixedNow.Add(-time.Hour), "msg-1", "req-1", "claude-opus-5", 10, 20, 30, 40))

	rep := f.collect()
	work := f.account(rep, "work")
	if work.Short.Messages != 1 || work.Short.Output != 20 {
		t.Errorf("work short window: %+v", work.Short)
	}
	if work.Attribution[SrcLastSession] != 1 {
		t.Errorf("attribution: %+v", work.Attribution)
	}
	if rep.Unattributed.Messages != 0 {
		t.Errorf("unexpected unattributed: %+v", rep.Unattributed)
	}
	if work.Tier.User != "max_20x" {
		t.Errorf("tier: %+v", work.Tier)
	}
}

func TestAttributionLaunchLog(t *testing.T) {
	f := newFixture(t)
	start := fixedNow.Add(-time.Hour)
	f.launch(start.Add(-30*time.Second), "play", "/proj/a")
	f.writeTranscript(f.home, "-proj-a", "sid-2.jsonl",
		record(start, "msg-1", "req-1", "claude-opus-5", 1, 2, 3, 4))

	rep := f.collect()
	play := f.account(rep, "play")
	if play.Attribution[SrcLaunchLog] != 1 || play.Short.Messages != 1 {
		t.Errorf("play: attribution=%+v short=%+v", play.Attribution, play.Short)
	}
	// LastUsed reflects the later of launch and last record.
	if !play.LastUsed.Equal(start) {
		t.Errorf("last used: want %v, got %v", start, play.LastUsed)
	}
}

func TestAttributionAmbiguity(t *testing.T) {
	cases := []struct {
		name     string
		accounts [2]string // two launches inside the ambiguity window
	}{
		{"two accounts", [2]string{"work", "play"}},
		{"managed plus unmanaged", [2]string{"work", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			start := fixedNow.Add(-time.Hour)
			f.launch(start.Add(-5*time.Minute), tc.accounts[0], "/proj/a")
			f.launch(start.Add(-1*time.Minute), tc.accounts[1], "/proj/a")
			f.writeTranscript(f.home, "-proj-a", "sid-3.jsonl",
				record(start, "msg-1", "req-1", "claude-opus-5", 1, 2, 3, 4))

			rep := f.collect()
			if rep.Unattributed.Messages != 1 {
				t.Errorf("want ambiguous session unattributed, got %+v", rep.Unattributed)
			}
		})
	}
}

func TestAttributionOldLaunchNotAmbiguous(t *testing.T) {
	// A different account's launch well before the ambiguity window must not
	// block attribution to the latest launch.
	f := newFixture(t)
	start := fixedNow.Add(-time.Hour)
	f.launch(start.Add(-2*time.Hour), "work", "/proj/a")
	f.launch(start.Add(-30*time.Second), "play", "/proj/a")
	f.writeTranscript(f.home, "-proj-a", "sid-4.jsonl",
		record(start, "msg-1", "req-1", "claude-opus-5", 1, 2, 3, 4))

	rep := f.collect()
	if f.account(rep, "play").Attribution[SrcLaunchLog] != 1 {
		t.Errorf("want play attributed, got unattributed=%+v", rep.Unattributed)
	}
}

func TestAttributionRootOwnershipWins(t *testing.T) {
	f := newFixture(t)
	// play claims the sid via lastSessionId, but the transcript lives in
	// work's own full-isolation tree — root ownership must win.
	f.sessionClaudeJSON("play", "sid-5")
	workRoot := filepath.Join(f.cfgDir, "sessions", "work")
	f.writeTranscript(workRoot, "-proj-a", "sid-5.jsonl",
		record(fixedNow.Add(-time.Hour), "msg-1", "req-1", "claude-opus-5", 1, 2, 3, 4))

	rep := f.collect()
	if f.account(rep, "work").Attribution[SrcRoot] != 1 {
		t.Errorf("want root attribution to work, got work=%+v play=%+v",
			f.account(rep, "work").Attribution, f.account(rep, "play").Attribution)
	}
}

func TestSymlinkedRootSkipped(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-6")
	// play's projects is a symlink into the shared pool — scanning it too
	// would double-count.
	playDir := filepath.Join(f.cfgDir, "sessions", "play")
	if err := os.MkdirAll(playDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(filepath.Join(f.home, "projects"), filepath.Join(playDir, "projects")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.writeTranscript(f.home, "-proj-a", "sid-6.jsonl",
		record(fixedNow.Add(-time.Hour), "msg-1", "req-1", "claude-opus-5", 10, 0, 0, 0))

	rep := f.collect()
	if got := f.account(rep, "work").Short.Input; got != 10 {
		t.Errorf("input tokens counted %d times (want once → 10)", got/10)
	}
}

func TestDedupAcrossSessionAndSubagentFiles(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-7")
	line := record(fixedNow.Add(-time.Hour), "msg-dup", "req-dup", "claude-opus-5", 5, 5, 5, 5)
	f.writeTranscript(f.home, "-proj-a", "sid-7.jsonl", line)
	f.writeTranscript(f.home, "-proj-a", filepath.Join("sid-7", "subagents", "agent-x.jsonl"), line)

	rep := f.collect()
	work := f.account(rep, "work")
	if work.Short.Messages != 1 || work.Short.Input != 5 {
		t.Errorf("duplicate (message.id, requestId) not deduped: %+v", work.Short)
	}
}

func TestWindowBoundaries(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-8")
	f.writeTranscript(f.home, "-proj-a", "sid-8.jsonl",
		record(fixedNow.Add(-ShortWindow), "m1", "r1", "claude-opus-5", 1, 0, 0, 0),                  // exactly on the 5h boundary → short
		record(fixedNow.Add(-ShortWindow-time.Second), "m2", "r2", "claude-opus-5", 10, 0, 0, 0),     // just outside 5h → long only
		record(fixedNow.Add(-DefaultHorizon-time.Second), "m3", "r3", "claude-opus-5", 100, 0, 0, 0), // outside horizon → neither
		record(fixedNow.Add(time.Minute), "m4", "r4", "claude-opus-5", 1000, 0, 0, 0),                // clock skew → both
	)

	rep := f.collect()
	work := f.account(rep, "work")
	if work.Short.Input != 1001 {
		t.Errorf("short input: want 1001, got %d", work.Short.Input)
	}
	if work.Long.Input != 1011 {
		t.Errorf("long input: want 1011, got %d", work.Long.Input)
	}
}

func TestMtimeHorizonSkipsStaleFiles(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-9")
	path := f.writeTranscript(f.home, "-proj-a", "sid-9.jsonl",
		record(fixedNow.Add(-time.Hour), "m1", "r1", "claude-opus-5", 7, 0, 0, 0))
	old := fixedNow.Add(-DefaultHorizon - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	rep := f.collect()
	if got := f.account(rep, "work").Long.Messages; got != 0 {
		t.Errorf("stale-mtime file was scanned: %d messages", got)
	}
}

func TestLimitEventAttributedAndExclusions(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-10")
	limitTS := fixedNow.Add(-30 * time.Minute)
	f.writeTranscript(f.home, "-proj-a", "sid-10.jsonl",
		record(fixedNow.Add(-time.Hour), "m1", "r1", "claude-opus-5", 1, 1, 1, 1),
		errorRecord(limitTS, "rate_limit", "You've hit your session limit · resets 3:00am (Asia/Manila)"),
		errorRecord(fixedNow.Add(-20*time.Minute), "server_error", "Internal server error"),
	)

	rep := f.collect()
	work := f.account(rep, "work")
	if work.Limit == nil {
		t.Fatal("limit event not attributed")
	}
	if work.Limit.Kind != "session" || !work.Limit.TS.Equal(limitTS) {
		t.Errorf("limit: %+v", work.Limit)
	}
	if work.Limit.ResetAt.IsZero() {
		t.Error("reset time not parsed")
	}
	// Error records must not count as consumption.
	if work.Short.Messages != 1 {
		t.Errorf("error records counted as usage: %+v", work.Short)
	}
	// A limited account is not suggested; play (idle oauth) is.
	if rep.Suggested != "play" {
		t.Errorf("suggested: want play, got %q", rep.Suggested)
	}
}

func TestParseLimitEventFormats(t *testing.T) {
	manila, err := time.LoadLocation("Asia/Manila")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	ts := time.Date(2026, 8, 24, 2, 30, 0, 0, manila).UTC()

	ev := parseLimitEvent(ts, json.RawMessage(`[{"type":"text","text":"You've hit your session limit · resets 3:00am (Asia/Manila)"}]`))
	if ev.Kind != "session" {
		t.Errorf("kind: %q", ev.Kind)
	}
	want := time.Date(2026, 8, 24, 3, 0, 0, 0, manila)
	if !ev.ResetAt.Equal(want) {
		t.Errorf("session reset: want %v, got %v", want, ev.ResetAt)
	}

	// Same clock time already past → rolls to tomorrow.
	tsLate := time.Date(2026, 8, 24, 4, 0, 0, 0, manila).UTC()
	ev = parseLimitEvent(tsLate, json.RawMessage(`[{"type":"text","text":"You've hit your session limit · resets 3:00am (Asia/Manila)"}]`))
	if want = time.Date(2026, 8, 25, 3, 0, 0, 0, manila); !ev.ResetAt.Equal(want) {
		t.Errorf("rolled reset: want %v, got %v", want, ev.ResetAt)
	}

	ev = parseLimitEvent(ts, json.RawMessage(`[{"type":"text","text":"You've hit your weekly limit · resets Aug 30 at 3pm (Asia/Manila)"}]`))
	if ev.Kind != "weekly" {
		t.Errorf("kind: %q", ev.Kind)
	}
	if want = time.Date(2026, 8, 30, 15, 0, 0, 0, manila); !ev.ResetAt.Equal(want) {
		t.Errorf("weekly reset: want %v, got %v", want, ev.ResetAt)
	}

	// Unparseable content still yields an event — the record proves the limit.
	ev = parseLimitEvent(ts, json.RawMessage(`"weird"`))
	if ev.Kind != "unknown" || !ev.ResetAt.IsZero() {
		t.Errorf("unparseable content: %+v", ev)
	}
}

func TestParseTier(t *testing.T) {
	cases := []struct {
		name, meta, display string
	}{
		{"user tier", `{"userRateLimitTier":"max_20x"}`, "max_20x"},
		{"org fallback", `{"organizationRateLimitTier":"team"}`, "team"},
		{"extra usage", `{"userRateLimitTier":"pro","hasExtraUsageEnabled":true}`, "pro+extra"},
		{"empty", "", "-"},
		{"garbage", "{not json", "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseTier(tc.meta).Display(); got != tc.display {
				t.Errorf("want %q, got %q", tc.display, got)
			}
		})
	}
}

func TestSuggestOrdering(t *testing.T) {
	now := fixedNow
	oauth := string(store.TypeOAuth)
	burned := AccountUsage{Name: "burned", Type: oauth, Short: WindowStats{Tokens: Tokens{Output: 1_000_000, Messages: 10}}}
	idle := AccountUsage{Name: "idle", Type: oauth}
	apiAcc := AccountUsage{Name: "api", Type: string(store.TypeAPIKey)}

	if got := suggest([]AccountUsage{burned, idle, apiAcc}, now); got != "idle" {
		t.Errorf("want idle, got %q", got)
	}
	// api_key accounts are never suggested even when everything else is limited.
	limitedAcc := idle
	limitedAcc.Limit = &LimitEvent{TS: now.Add(-time.Hour), ResetAt: now.Add(time.Hour)}
	if got := suggest([]AccountUsage{limitedAcc, apiAcc}, now); got != "" {
		t.Errorf("want no suggestion, got %q", got)
	}
	// Reset in the past → usable again.
	expired := idle
	expired.Limit = &LimitEvent{TS: now.Add(-6 * time.Hour), ResetAt: now.Add(-time.Hour)}
	if got := suggest([]AccountUsage{burned, expired}, now); got != "idle" {
		t.Errorf("want idle (expired limit), got %q", got)
	}
}

func TestCorruptAndOversizedLines(t *testing.T) {
	f := newFixture(t)
	f.sessionClaudeJSON("work", "sid-11")
	big := strings.Repeat("x", 100_000)
	f.writeTranscript(f.home, "-proj-a", "sid-11.jsonl",
		`{"type":"assistant","timestamp":"broken`,
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/proj/a","requestId":"r1","filler":%q,"message":{"id":"m1","model":"claude-opus-5","usage":{"input_tokens":3,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"content":[]}}`,
			fixedNow.Add(-time.Hour).Format(time.RFC3339), big),
	)

	rep := f.collect()
	if got := f.account(rep, "work").Short.Input; got != 3 {
		t.Errorf("oversized line not parsed / corrupt line not skipped: input=%d", got)
	}
}

func TestCollectEmpty(t *testing.T) {
	rep, err := Collect(t.TempDir(), Options{Now: fixedNow, HomeClaudeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rep.Accounts) != 0 || rep.Suggested != "" || rep.Unattributed.Messages != 0 {
		t.Errorf("empty collect: %+v", rep)
	}
}
