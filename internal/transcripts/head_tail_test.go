package transcripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	sidA = "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	sidB = "1e005053-380a-4245-a145-52c2715afa73"
	sidC = "1e005053-aaaa-4bbb-8ccc-ddddeeeeffff"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// rec renders one transcript record as a JSON line.
func rec(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// userRec is a human prompt record with the usual envelope.
func userRec(sid, cwd, ts, text string) string {
	return rec(map[string]any{
		"type": "user", "cwd": cwd, "sessionId": sid, "timestamp": ts, "version": "2.1.259",
		"gitBranch": "main", "slug": "frolicking-drifting-pizza",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func writeTranscript(t *testing.T, path string, parts ...string) {
	t.Helper()
	writeFile(t, path, strings.Join(parts, ""))
}

func TestReadHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path,
		// Bookkeeping first: carries the session id but no cwd, a typed
		// mismatch on version (tolerated), and no prompt.
		rec(map[string]any{"type": "permission-mode", "sessionId": sidA, "version": 42}),
		// A meta user record must not become the first prompt.
		rec(map[string]any{"type": "user", "isMeta": true, "cwd": "/Users/a/proj", "sessionId": sidA,
			"timestamp": "2026-08-24T10:00:00.250Z", "version": "2.1.259",
			"message": map[string]any{"role": "user", "content": "Caveat: local commands"}}),
		// A tool result is not a prompt either.
		rec(map[string]any{"type": "user", "cwd": "/Users/a/proj", "sessionId": sidA, "gitBranch": "feat/x",
			"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": "ok"}}}}),
		// The first human prompt: a text block, multi-line.
		rec(map[string]any{"type": "user", "cwd": "/Users/a/other", "sessionId": sidA, "slug": "plan-slug",
			"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "  fix the shim  \nsecond line"}}}}),
		rec(map[string]any{"type": "assistant", "cwd": "/Users/a/proj", "sessionId": sidA, "gitBranch": "later"}),
	)
	h, err := ReadHead(path)
	if err != nil {
		t.Fatalf("ReadHead: %v", err)
	}
	want := Head{
		Cwd: "/Users/a/proj", SessionID: sidA, Version: "2.1.259", GitBranch: "feat/x", PlanSlug: "plan-slug",
		FirstPrompt: "fix the shim", FirstTS: time.Date(2026, 8, 24, 10, 0, 0, 250_000_000, time.UTC),
	}
	if h != want {
		t.Errorf("Head = %+v\nwant %+v", h, want)
	}
}

func TestReadHeadStringContentAndCap(t *testing.T) {
	long := strings.Repeat("é", 250)
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path, userRec(sidA, "/Users/a/proj", "2026-08-24T10:00:00Z", "\n\n"+long+"\nmore"))
	h, err := ReadHead(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := []rune(h.FirstPrompt); len(got) != promptCap || string(got) != strings.Repeat("é", promptCap) {
		t.Errorf("FirstPrompt = %d runes %q", len(got), h.FirstPrompt)
	}
}

func TestReadHeadMultiMBLine(t *testing.T) {
	dir := t.TempDir()
	huge := rec(map[string]any{"type": "user", "cwd": "/Users/a/huge", "sessionId": sidB,
		"message": map[string]any{"role": "user", "content": strings.Repeat("x", 3<<20)}})

	// A small record followed by a 3 MB one: only the first is parsed.
	p1 := filepath.Join(dir, sidA+".jsonl")
	writeTranscript(t, p1, rec(map[string]any{"type": "assistant", "cwd": "/Users/a/proj", "sessionId": sidA}), huge)
	h, err := ReadHead(p1)
	if err != nil {
		t.Fatal(err)
	}
	if h.Cwd != "/Users/a/proj" || h.SessionID != sidA || h.Torn || h.FirstPrompt != "" {
		t.Errorf("head after small+huge = %+v", h)
	}

	// A 3 MB first line: the window ends mid-record.
	p2 := filepath.Join(dir, sidB+".jsonl")
	writeTranscript(t, p2, huge)
	h, err = ReadHead(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Torn || h.Cwd != "" || h.SessionID != "" {
		t.Errorf("head of a huge first line = %+v, want Torn and empty", h)
	}
}

func TestReadHeadSmallFiles(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		wantCwd string
		torn    bool
	}{
		{"empty", "", "", false},
		{"whitespace", "\n\n", "", false},
		{"unterminated valid", `{"type":"user","cwd":"/Users/a/p","sessionId":"` + sidA + `"}`, "/Users/a/p", false},
		{"unterminated torn", `{"type":"user","cwd":"/Users/a/p","sessionId":"` + sidA, "", true},
		{"garbage line", "not json\n", "", false},
		{"garbage then record", "not json\n" + rec(map[string]any{"cwd": "/Users/a/q"}), "/Users/a/q", false},
		{"crlf", "{\"cwd\":\"/Users/a/r\"}\r\n", "/Users/a/r", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, sidA+".jsonl")
			writeFile(t, p, c.content)
			h, err := ReadHead(p)
			if err != nil {
				t.Fatal(err)
			}
			if h.Cwd != c.wantCwd || h.Torn != c.torn {
				t.Errorf("Head = %+v, want Cwd %q Torn %v", h, c.wantCwd, c.torn)
			}
		})
	}
}

func TestReadHeadMissing(t *testing.T) {
	if _, err := ReadHead(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestReadTailLastWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path,
		userRec(sidA, "/Users/a/proj", "2026-08-24T10:00:00Z", "hello"),
		rec(map[string]any{"type": "relocated", "sessionId": sidA, "relocatedCwd": "/Users/a/first-move"}),
		rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "old name"}),
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "AI one"}),
		rec(map[string]any{"type": "last-prompt", "sessionId": sidA, "lastPrompt": "what next?", "leafUuid": "x"}),
		rec(map[string]any{"type": "assistant", "sessionId": sidA, "timestamp": "2026-08-24T11:30:00Z"}),
		rec(map[string]any{"type": "relocated", "sessionId": sidA, "relocatedCwd": "/Users/a/second-move"}),
		rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "new name"}),
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "AI two"}),
		rec(map[string]any{"type": "summary", "summary": "Summary one", "leafUuid": "s1"}),
		rec(map[string]any{"type": "summary", "summary": "Summary two", "leafUuid": "s2"}),
		// A re-stamped last-prompt without the field (the usual shape) leaves
		// the last value in place, as Claude's backwards key scan does.
		rec(map[string]any{"type": "last-prompt", "sessionId": sidA, "leafUuid": "y"}),
		// A relocatedCwd outside a relocated record is not a relocation.
		rec(map[string]any{"type": "queue-operation", "relocatedCwd": "/Users/a/not-a-move"}),
	)
	tl, err := ReadTail(path)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	want := Tail{RelocatedCwd: "/Users/a/second-move", CustomTitle: "new name", AITitle: "AI two", LastPrompt: "what next?", Summary: "Summary two",
		LastTS: time.Date(2026, 8, 24, 11, 30, 0, 0, time.UTC), LastLineComplete: true}
	if tl != want {
		t.Errorf("Tail = %+v\nwant %+v", tl, want)
	}
}

// TestReadTailEmptyClears pins the other half of Claude's rule: a field
// present as an empty string clears the value, a field of another type is
// ignored.
func TestReadTailEmptyClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path,
		userRec(sidA, "/Users/a/proj", "2026-08-24T10:00:00Z", "hello"),
		rec(map[string]any{"type": "relocated", "sessionId": sidA, "relocatedCwd": "/Users/a/moved"}),
		rec(map[string]any{"type": "last-prompt", "sessionId": sidA, "lastPrompt": "what next?"}),
		rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "kept"}),
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "AI"}),
		rec(map[string]any{"type": "relocated", "sessionId": sidA, "relocatedCwd": ""}),
		rec(map[string]any{"type": "last-prompt", "sessionId": sidA, "lastPrompt": ""}),
		rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": 7}),
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": nil}),
	)
	tl, err := ReadTail(path)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	want := Tail{CustomTitle: "kept", AITitle: "AI", LastTS: time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), LastLineComplete: true}
	if tl != want {
		t.Errorf("Tail = %+v\nwant %+v", tl, want)
	}
	if got := EffectiveCwd(Head{Cwd: "/Users/a/proj"}, tl); got != "/Users/a/proj" {
		t.Errorf("EffectiveCwd after an empty relocation = %q", got)
	}
}

// TestReadHeadPromptRules pins the picker's prompt rules (Claude 2.1.259,
// function _8) on in-memory windows.
func TestReadHeadPromptRules(t *testing.T) {
	user := func(content any, extra map[string]any) string {
		m := map[string]any{"type": "user", "cwd": "/Users/a/proj", "sessionId": sidA, "message": map[string]any{"role": "user", "content": content}}
		for k, v := range extra {
			m[k] = v
		}
		return rec(m)
	}
	command := user("<command-name>/login</command-name>\n<command-message>login</command-message>\n<command-args></command-args>", nil)
	caveat := user("<local-command-caveat>Caveat: The messages below were generated by the user while running local commands.</local-command-caveat>", nil)
	stdout := user("<local-command-stdout>Set model to Fable</local-command-stdout>", nil)
	cases := []struct {
		name, window, want string
	}{
		{"slash commands only: the first command stands in", command + user("<command-name>/model</command-name>", nil), "/login"},
		{"command then a prompt", command + caveat + stdout + user("fix the shim\nplease", nil), "fix the shim"},
		{"caveat and stdout alone are nothing", caveat + stdout, ""},
		{"interruption notice skipped", user("[Request interrupted by user for tool use]", nil) + user("real one", nil), "real one"},
		{"bash-mode prompt", user("<bash-input>ls -la</bash-input>\n<bash-stdout>x</bash-stdout>", nil), "! ls -la"},
		{"tool_result anywhere makes it a tool turn", user([]map[string]any{{"type": "text", "text": "context"}, {"type": "tool_result", "content": "ok"}}, nil) + user("next", nil), "next"},
		{"second text block when the first is a tag", user([]map[string]any{{"type": "text", "text": "<system-reminder>x</system-reminder>"}, {"type": "text", "text": "the prompt"}}, nil), "the prompt"},
		{"compact summary skipped", user("Summary of the earlier conversation", map[string]any{"isCompactSummary": true}) + user("after compaction", nil), "after compaction"},
		{"meta skipped", user("meta text", map[string]any{"isMeta": true}), ""},
		{"a tag not at the start is a prompt", user("read <file> please", nil), "read <file> please"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := parseHead([]byte(c.window), true)
			if h.FirstPrompt != c.want {
				t.Errorf("FirstPrompt = %q, want %q", h.FirstPrompt, c.want)
			}
			if h.Cwd != "/Users/a/proj" {
				t.Errorf("Cwd = %q", h.Cwd)
			}
		})
	}
}

func TestReadTailTorn(t *testing.T) {
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path,
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "kept"}),
		`{"type":"custom-title","sessionId":"`+sidA+`","customTitle":"tor`, // crashed mid-write
	)
	tl, err := ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if tl.LastLineComplete || tl.AITitle != "kept" || tl.CustomTitle != "" {
		t.Errorf("Tail = %+v, want AITitle kept, no custom title, incomplete last line", tl)
	}

	// Complete last line without a trailing newline: metadata is read
	// but the line does not count as complete.
	writeTranscript(t, path,
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "kept"}),
		strings.TrimSuffix(rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "unterminated"}), "\n"),
	)
	tl, err = ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if tl.LastLineComplete || tl.CustomTitle != "unterminated" {
		t.Errorf("Tail = %+v, want custom title read and LastLineComplete false", tl)
	}

	// Empty file.
	writeFile(t, path, "")
	tl, err = ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if tl != (Tail{}) {
		t.Errorf("Tail of an empty file = %+v", tl)
	}
}

func TestReadTailSkipsPartialFirstLine(t *testing.T) {
	// A custom-title record that straddles the window start must not be
	// picked up; the ai-title inside the window must.
	straddle := rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "straddle", "pad": strings.Repeat("p", 8000)})
	var suffix strings.Builder
	suffix.WriteString(rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "inside"}))
	for suffix.Len() < 60_000 {
		suffix.WriteString(rec(map[string]any{"type": "assistant", "sessionId": sidA, "timestamp": "2026-08-24T11:00:00Z"}))
	}
	if len(straddle)+suffix.Len() <= windowSize || suffix.Len() >= windowSize {
		t.Fatalf("fixture sizes wrong: straddle %d suffix %d", len(straddle), suffix.Len())
	}
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path, userRec(sidA, "/Users/a/proj", "2026-08-24T10:00:00Z", "hi"), straddle, suffix.String())
	tl, err := ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if tl.CustomTitle != "" || tl.AITitle != "inside" || !tl.LastLineComplete {
		t.Errorf("Tail = %+v", tl)
	}
}

func TestReadTailHugeLastLine(t *testing.T) {
	huge := rec(map[string]any{"type": "assistant", "sessionId": sidA, "timestamp": "2026-08-24T11:45:00Z",
		"message": map[string]any{"content": strings.Repeat("x", 3<<20)}})
	path := filepath.Join(t.TempDir(), sidA+".jsonl")
	writeTranscript(t, path, rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "before"}), huge)
	tl, err := ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if !tl.LastLineComplete {
		t.Error("a complete multi-MB last line must report LastLineComplete")
	}
	if tl.CustomTitle != "" {
		t.Errorf("CustomTitle = %q; the window does not reach a record before a 3 MB last line", tl.CustomTitle)
	}
	if want := time.Date(2026, 8, 24, 11, 45, 0, 0, time.UTC); !tl.LastTS.Equal(want) {
		t.Errorf("LastTS = %v, want %v (read from the whole last line)", tl.LastTS, want)
	}

	// The same line torn: not complete.
	writeTranscript(t, path, rec(map[string]any{"type": "custom-title", "sessionId": sidA}), huge[:len(huge)-100])
	tl, err = ReadTail(path)
	if err != nil {
		t.Fatal(err)
	}
	if tl.LastLineComplete {
		t.Error("a torn multi-MB last line must not report LastLineComplete")
	}
}

func TestEffectiveCwd(t *testing.T) {
	h := Head{Cwd: "/Users/a/head"}
	if got := EffectiveCwd(h, Tail{}); got != "/Users/a/head" {
		t.Errorf("EffectiveCwd without relocation = %q", got)
	}
	if got := EffectiveCwd(h, Tail{RelocatedCwd: "/Users/a/moved"}); got != "/Users/a/moved" {
		t.Errorf("EffectiveCwd with relocation = %q", got)
	}
}

func TestTitlePrecedence(t *testing.T) {
	hist := HistoryIndex{sidA: "from history"}
	full := Tail{CustomTitle: "custom", AITitle: "ai", LastPrompt: "last"}
	head := Head{SessionID: sidA, FirstPrompt: "first"}
	cases := []struct {
		name        string
		h           Head
		t           Tail
		hist        HistoryIndex
		want, wantS string
	}{
		{"custom", head, full, hist, "custom", TitleSourceCustom},
		{"ai", head, Tail{AITitle: "ai", LastPrompt: "last"}, hist, "ai", TitleSourceAI},
		{"last-prompt", head, Tail{LastPrompt: "last", Summary: "sum"}, hist, "last", TitleSourceLastPrompt},
		{"summary", head, Tail{Summary: "sum"}, hist, "sum", TitleSourceSummary},
		{"first-prompt", head, Tail{}, hist, "first", TitleSourceFirstPrompt},
		{"history", Head{SessionID: sidA}, Tail{}, hist, "from history", TitleSourceHistory},
		{"history nil", Head{SessionID: sidA}, Tail{}, nil, "", ""},
		{"history unknown sid", Head{SessionID: sidB}, Tail{}, hist, "", ""},
		{"blank custom falls through", head, Tail{CustomTitle: "  \t ", AITitle: "ai"}, nil, "ai", TitleSourceAI},
		{"first line only", head, Tail{CustomTitle: "line one\nline two"}, nil, "line one", TitleSourceCustom},
		{"sanitised", head, Tail{CustomTitle: "\x1b]52;c;aGVsbG8=\x07evil \x1b[2Jtitle"}, nil, "evil title", TitleSourceCustom},
		{"only escapes", head, Tail{CustomTitle: "\x1b[2J", AITitle: "ai"}, nil, "ai", TitleSourceAI},
		{"nothing", Head{}, Tail{}, hist, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, src := Title(c.h, c.t, c.hist)
			if got != c.want || src != c.wantS {
				t.Errorf("Title = (%q, %q), want (%q, %q)", got, src, c.want, c.wantS)
			}
		})
	}
}

func TestLoadHistory(t *testing.T) {
	cfg := t.TempDir()
	idx, err := LoadHistory(cfg)
	if err != nil || idx == nil || len(idx) != 0 {
		t.Fatalf("missing history: (%v, %v), want empty non-nil, nil", idx, err)
	}

	long := strings.Repeat("w", 300)
	writeFile(t, filepath.Join(cfg, HistoryFile), strings.Join([]string{
		`{"display":"  first prompt\nsecond line","pastedContents":{},"timestamp":1771339168192,"project":"/Users/a/p","sessionId":"` + sidA + `"}`,
		`{"display":"later prompt","sessionId":"` + sidA + `"}`,
		`not json at all`,
		`{"display":"no session id"}`,
		`{"display":"   ","sessionId":"` + sidB + `"}`,
		`{"display":"` + long + `","sessionId":"` + sidB + `"}`,
		`{"display":"unterminated last","sessionId":"` + sidC + `"}`,
	}, "\n"))
	idx, err = LoadHistory(cfg)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if got := idx[sidA]; got != "first prompt" {
		t.Errorf("idx[sidA] = %q, want the first line of the first display", got)
	}
	if got := idx[sidB]; len([]rune(got)) != promptCap {
		t.Errorf("idx[sidB] = %d runes, want capped at %d (and the blank display skipped)", len([]rune(got)), promptCap)
	}
	if got := idx[sidC]; got != "unterminated last" {
		t.Errorf("idx[sidC] = %q", got)
	}
	if len(idx) != 3 {
		t.Errorf("index has %d entries: %v", len(idx), idx)
	}
}

func TestLoadHistoryUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reading a directory handle behaves differently on windows")
	}
	cfg := t.TempDir()
	if err := os.Mkdir(filepath.Join(cfg, HistoryFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHistory(cfg); err == nil {
		t.Error("a directory in place of history.jsonl must be an error, not an empty index")
	}
}
