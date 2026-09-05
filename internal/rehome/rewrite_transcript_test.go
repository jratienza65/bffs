package rehome

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// rewriteFixture writes a transcript exercising every rewrite rule and
// returns its path with the lines it holds.
func rewriteFixture(t *testing.T, oldCwd, oldHome string) (string, []string) {
	t.Helper()
	lines := []string{
		// 1: top-level cwd equals the old one AND a nested cwd inside a tool input.
		`{"type":"user","cwd":` + quote(oldCwd) + `,"sessionId":"` + sid1 + `","message":{"role":"user","content":[{"type":"tool_use","input":{"cwd":` + quote(oldCwd) + `,"command":"ls"}}]}}`,
		// 2: a different top-level cwd (a subdirectory) stays.
		`{"type":"assistant","cwd":` + quote(oldCwd+"/sub") + `,"message":{"role":"assistant","content":"see ` + oldCwd + `"}}`,
		// 3: snapshot keys: inside the old cwd; a sibling sharing the cwd
		// prefix without a boundary (only the home pair applies); a path
		// sharing the home prefix without a boundary; elsewhere.
		`{"type":"file-history-snapshot","messageId":"m1","snapshot":{"messageId":"m1","trackedFileBackups":{` +
			quote(oldCwd+"/a/x.md") + `:{"backupFileName":"x@v1","version":1,"backupTime":"t","realParentDir":` + quote(oldCwd+"/a") + `},` +
			quote(oldCwd+"-other/y.md") + `:{},` +
			quote(oldHome+"e/proj/y.md") + `:{},` +
			`"/elsewhere/z":{}},"timestamp":"t"},"isSnapshotUpdate":false}`,
		// 4: delta with the config-dir spelling of a memory file.
		`{"type":"file-history-delta","messageId":"m2","trackingPath":` + quote(oldCwd+"/a/x.md") + `,"backup":{"backupFileName":null,"version":2,"backupTime":"t","realParentDir":` + quote(oldHome+"/.claude/projects/-x/memory") + `},"timestamp":"t"}`,
		// 5: a non-file-history record carrying the same keys is left alone.
		`{"type":"user","trackingPath":` + quote(oldCwd+"/a/x.md") + `,"snapshot":{"trackedFileBackups":{` + quote(oldCwd+"/q") + `:{}}}}`,
		// 6: malformed line, copied verbatim.
		`{"type":"user","cwd":` + quote(oldCwd) + `,"broken":`,
		// 7: whitespace and a key order that puts cwd last, CRLF-terminated.
		`{ "type" : "system" , "message" : { "cwd" : ` + quote(oldCwd) + ` } , "cwd" : ` + quote(oldCwd) + ` }` + "\r",
		// 8: the relocated stamp; no trailing newline on the last line.
		strings.TrimSuffix(string(RelocatedRecord(sid1, "/new")), "\n"),
	}
	path := filepath.Join(t.TempDir(), sid1+".jsonl")
	write(t, path, strings.Join(lines, "\n"))
	return path, lines
}

func quote(s string) string { return string(encodeJSONString(s)) }

func TestRewriteTranscript(t *testing.T) {
	oldCwd, newCwd := "/Users/jo/proj", "/home/jo/work/proj"
	oldHome, newHome := "/Users/jo", "/home/jo"
	path, lines := rewriteFixture(t, oldCwd, oldHome)
	stamp := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	chtimes(t, path, stamp)
	before := readFile(t, path)

	rules := TranscriptRules{OldCwd: oldCwd, NewCwd: newCwd, Paths: [][2]string{{oldHome, newHome}}, Cwd: true, FileHistory: true}
	rw, err := RewriteTranscript(path, rules)
	if err != nil {
		t.Fatalf("RewriteTranscript: %v", err)
	}
	if rw.SessionID != sid1 || rw.CwdRecords != 2 || rw.FileHistoryPaths != 5 {
		t.Errorf("rewrite = %+v, want 2 cwd records and 5 paths", rw)
	}
	if mt := mtimeOf(t, path); !mt.Equal(stamp) {
		t.Errorf("mtime = %v, want %v restored", mt, stamp)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}

	got := strings.Split(readFile(t, path), "\n")
	if len(got) != len(lines) {
		t.Fatalf("line count %d, want %d:\n%s", len(got), len(lines), readFile(t, path))
	}
	want := make([]string, len(lines))
	copy(want, lines)
	want[0] = `{"type":"user","cwd":` + quote(newCwd) + `,"sessionId":"` + sid1 + `","message":{"role":"user","content":[{"type":"tool_use","input":{"cwd":` + quote(oldCwd) + `,"command":"ls"}}]}}`
	want[2] = `{"type":"file-history-snapshot","messageId":"m1","snapshot":{"messageId":"m1","trackedFileBackups":{` +
		quote(newCwd+"/a/x.md") + `:{"backupFileName":"x@v1","version":1,"backupTime":"t","realParentDir":` + quote(newCwd+"/a") + `},` +
		quote(newHome+"/proj-other/y.md") + `:{},` +
		quote(oldHome+"e/proj/y.md") + `:{},` +
		`"/elsewhere/z":{}},"timestamp":"t"},"isSnapshotUpdate":false}`
	want[3] = `{"type":"file-history-delta","messageId":"m2","trackingPath":` + quote(newCwd+"/a/x.md") + `,"backup":{"backupFileName":null,"version":2,"backupTime":"t","realParentDir":` + quote(newHome+"/.claude/projects/-x/memory") + `},"timestamp":"t"}`
	want[6] = `{ "type" : "system" , "message" : { "cwd" : ` + quote(oldCwd) + ` } , "cwd" : ` + quote(newCwd) + ` }` + "\r"
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i+1, got[i], want[i])
		}
	}

	// Cwd only: file-history records are untouched; nested cwd still is.
	path2, _ := rewriteFixture(t, oldCwd, oldHome)
	rw, err = RewriteTranscript(path2, TranscriptRules{OldCwd: oldCwd, NewCwd: newCwd, Cwd: true})
	if err != nil || rw.CwdRecords != 2 || rw.FileHistoryPaths != 0 {
		t.Errorf("cwd only: %+v, %v", rw, err)
	}
	got = strings.Split(readFile(t, path2), "\n")
	for _, i := range []int{1, 2, 3, 4, 5, 7} {
		if got[i] != lines[i] {
			t.Errorf("cwd only changed line %d:\n got %s\nwant %s", i+1, got[i], lines[i])
		}
	}
	if !strings.Contains(got[0], `"input":{"cwd":`+quote(oldCwd)) {
		t.Errorf("nested cwd changed: %s", got[0])
	}

	// Nothing to do: the file is untouched, byte for byte and in mtime.
	path3, _ := rewriteFixture(t, oldCwd, oldHome)
	chtimes(t, path3, stamp)
	rw, err = RewriteTranscript(path3, TranscriptRules{OldCwd: "/nowhere", NewCwd: newCwd, Paths: [][2]string{{"/nowhere", newHome}}, Cwd: true, FileHistory: true})
	if err != nil || rw.CwdRecords != 0 || rw.FileHistoryPaths != 0 {
		t.Errorf("no-op: %+v, %v", rw, err)
	}
	if readFile(t, path3) != before || !mtimeOf(t, path3).Equal(stamp) {
		t.Error("no-op rewrite changed the file")
	}
	if entries, _ := os.ReadDir(filepath.Dir(path3)); len(entries) != 1 {
		t.Errorf("leftover files after no-op: %v", entries)
	}

	// Rules off: nothing happens at all.
	if rw, err := RewriteTranscript(path3, TranscriptRules{OldCwd: oldCwd, NewCwd: newCwd}); err != nil || rw != (TranscriptRewrite{}) {
		t.Errorf("rules off: %+v, %v", rw, err)
	}
}

func TestWalkStringsSpans(t *testing.T) {
	line := []byte(` { "a" : "x\"y" , "b" : [ "c" , 1 , { "d" : "e" } , true , null ] , "f" : { "g" : "h" } } ` + "\n")
	type seen struct {
		value string
		isKey bool
		path  string
	}
	var got []seen
	err := walkStrings(line, func(s stringSpan) {
		if line[s.start] != '"' || line[s.end-1] != '"' {
			t.Errorf("span %d:%d of %q is not quoted: %q", s.start, s.end, s.value, line[s.start:s.end])
		}
		got = append(got, seen{s.value, s.isKey, strings.Join(s.path, ".")})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []seen{
		{"a", true, ""}, {`x"y`, false, "a"},
		{"b", true, ""}, {"c", false, "b.[]"}, {"d", true, "b.[]"}, {"e", false, "b.[].d"},
		{"f", true, ""}, {"g", true, "f"}, {"h", false, "f.g"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("spans:\n got %+v\nwant %+v", got, want)
	}
	if err := walkStrings([]byte(`{"a":`), func(stringSpan) {}); err == nil {
		t.Error("truncated line accepted")
	}
}

func TestApplyRewritesTranscripts(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	// sid1 also carries a file-history snapshot and a nested cwd.
	tr := filepath.Join(p.root.Dir, p.oldSlug, sid1+".jsonl")
	extra := jsonLine(map[string]any{"type": "file-history-snapshot", "messageId": "m", "snapshot": map[string]any{"trackedFileBackups": map[string]any{p.old + "/f.go": map[string]any{}}}}) +
		jsonLine(map[string]any{"type": "user", "cwd": p.old, "message": map[string]any{"content": []any{map[string]any{"input": map[string]any{"cwd": p.old}}}}})
	write(t, tr, readFile(t, tr)+extra)
	chtimes(t, tr, fixedNow.Add(-3*day))

	opts := p.opts()
	opts.RewriteCwd = true
	opts.RewriteFileHistory = true
	pl := plan(t, p, nil, opts)
	res, err := Apply(context.Background(), p.root, pl, opts)
	if err != nil {
		t.Fatalf("Apply: %v (warnings %v)", err, res.Warnings)
	}
	if len(res.Rewrites) != 3 {
		t.Fatalf("rewrites = %+v", res.Rewrites)
	}
	for _, rw := range res.Rewrites {
		wantCwd, wantPaths := 1, 0
		if rw.SessionID == sid1 {
			wantCwd, wantPaths = 2, 1
		}
		if rw.CwdRecords != wantCwd || rw.FileHistoryPaths != wantPaths {
			t.Errorf("rewrite %+v, want %d cwd / %d paths", rw, wantCwd, wantPaths)
		}
	}
	moved := filepath.Join(p.root.Dir, p.newSlug, sid1+".jsonl")
	got := readFile(t, moved)
	if strings.Count(got, `"cwd":`+quote(p.new)) != 2 || strings.Count(got, `"cwd":`+quote(p.old)) != 1 {
		t.Errorf("cwd rewrite:\n%s", got)
	}
	if !strings.Contains(got, quote(p.new+"/f.go")+":{}") || strings.Contains(got, quote(p.old+"/f.go")) {
		t.Errorf("file-history rewrite:\n%s", got)
	}
	if !strings.HasSuffix(got, string(RelocatedRecord(sid1, p.new))) {
		t.Errorf("stamp lost:\n%s", got)
	}
	if mt := mtimeOf(t, moved); !near(mt, fixedNow.Add(-3*day)) {
		t.Errorf("mtime = %v", mt)
	}
	entries, _ := os.ReadDir(filepath.Dir(moved))
	for _, e := range entries {
		if strings.Contains(e.Name(), tmpSuffix) {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestReplacePrefixSpelling(t *testing.T) {
	// Old prefixes match as the source machine spelled them: a trailing
	// separator is ignored, and a POSIX or Windows prefix is never
	// re-cleaned for the OS running the rewrite.
	lr := newLineRewriter(TranscriptRules{
		FileHistory: true,
		Paths:       [][2]string{{"/Users/jo/", "/home/jo"}, {`C:\Users\jo\`, `D:\jo`}},
	})
	if want := [][2]string{{`C:\Users\jo`, `D:\jo`}, {"/Users/jo", "/home/jo"}}; !reflect.DeepEqual(lr.prefix, want) {
		t.Errorf("prefixes = %q, want %q (longest first, trailing separator dropped, spelling kept)", lr.prefix, want)
	}
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"/Users/jo/proj/x.md", "/home/jo/proj/x.md", true},
		{"/Users/jo", "/home/jo", true},
		{"/Users/joe/x.md", "", false},
		{`C:\Users\jo\p\x.md`, `D:\jo\p\x.md`, true},
		{`C:\Users\jonas\x.md`, "", false},
	} {
		got, ok := lr.replacePrefix(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("replacePrefix(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	for in, want := range map[string]string{"/": "/", `C:\`: `C:\`, "/a/b/": "/a/b", `C:\x\`: `C:\x`, "/a": "/a"} {
		if got := trimTrailingSeparators(in); got != want {
			t.Errorf("trimTrailingSeparators(%q) = %q, want %q", in, got, want)
		}
	}
}
