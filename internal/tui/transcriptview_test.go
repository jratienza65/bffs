package tui

import (
	"strings"
	"testing"
)

// The viewer renders prompts, replies, tool calls and results, notes,
// hides meta and progress records, shows what it cannot parse, and
// strips escapes from everything.
func TestRenderTranscript(t *testing.T) {
	src := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"hello\nworld"}}`,
		`{"type":"assistant","timestamp":"2026-08-24T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la","description":"list"}},{"type":"thinking","thinking":"..."}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"a\nb\nc","is_error":false}]}}`,
		`{"type":"summary","summary":"Sum \u001b]52;c;aGVsbG8=\u0007mary"}`,
		`{"type":"progress","data":{}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"relocated","relocatedCwd":"/new/place"}`,
		`not json at all`,
		``,
	}, "\n")
	lines, records, hidden, truncated, err := renderTranscript(strings.NewReader(src), transcriptReadCap)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if records != 8 || hidden != 2 {
		t.Errorf("records=%d hidden=%d, want 8 and 2", records, hidden)
	}
	out := strings.Join(lines, "\n")
	wantAll(t, out, "> hello", "  world", "< hi", "⚙ Bash command=ls -la", "(thinking)", "⇠ result: a (3 lines)", "— summary: Sum mary", "— relocated to /new/place", "? unparseable record: not json at all")
	wantNone(t, out, "\x1b]", "52;c;", "meta", "progress")
	// A blank line separates records; the timestamped reply carries a clock.
	if !strings.Contains(out, "\n\n") {
		t.Errorf("records not separated:\n%s", out)
	}

	// A cap cuts the stream and says so.
	_, _, _, truncated, err = renderTranscript(strings.NewReader(src), 40)
	if err != nil || !truncated {
		t.Errorf("cap: truncated=%v err=%v", truncated, err)
	}
}

func TestToolSummaries(t *testing.T) {
	if got := toolInputSummary([]byte(`{"file_path":"/a/b.go","content":"x"}`)); got != "file_path=/a/b.go" {
		t.Errorf("input summary = %q", got)
	}
	if got := toolInputSummary([]byte(`{"n":1}`)); got != `{"n":1}` {
		t.Errorf("fallback summary = %q", got)
	}
	if got := toolResultSummary([]byte(`[{"type":"text","text":"one\ntwo"}]`)); got != "one (2 lines)" {
		t.Errorf("block result = %q", got)
	}
	if got := toolResultSummary([]byte(`""`)); got != "(empty)" {
		t.Errorf("empty result = %q", got)
	}
	if got := clip("abcdef", 4); got != "abc…" {
		t.Errorf("clip = %q", got)
	}
}
