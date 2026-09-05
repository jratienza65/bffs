package rehome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelocatedRecord(t *testing.T) {
	got := string(RelocatedRecord(testSID, "/home/jonas/src/bffs"))
	want := `{"type":"relocated","sessionId":"` + testSID + `","relocatedCwd":"/home/jonas/src/bffs"}` + "\n"
	if got != want {
		t.Errorf("RelocatedRecord = %q, want %q", got, want)
	}
	// A Windows path is escaped, never mangled.
	got = string(RelocatedRecord(testSID, `C:\Users\jonas\x`))
	if !strings.Contains(got, `"relocatedCwd":"C:\\Users\\jonas\\x"`) {
		t.Errorf("windows path = %q", got)
	}
}

func TestAppendRelocated(t *testing.T) {
	const line = `{"type":"user","cwd":"/old","sessionId":"` + testSID + `"}`
	stamp := string(RelocatedRecord(testSID, "/new"))
	cases := []struct {
		name    string
		content string
		force   bool
		want    string
		wantErr string
	}{
		{"trailing newline", line + "\n", false, line + "\n" + stamp, ""},
		{"no trailing newline, complete object", line, false, line + "\n" + stamp, ""},
		{"empty", "", false, "", "is empty"},
		{"empty forced", "", true, "", "is empty"},
		{"incomplete last line", line + "\n" + `{"type":"assist`, false, "", "incomplete last line (crashed session?); resume it once in claude or pass --force-stamp"},
		{"incomplete last line forced", line + "\n" + `{"type":"assist`, true, line + "\n" + `{"type":"assist` + "\n" + stamp, ""},
		{"non-object last line", line + "\n" + `[1,2]`, false, "", "incomplete last line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), testSID+".jsonl")
			write(t, p, c.content)
			err := AppendRelocated(p, testSID, "/new", c.force)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				if got := readFile(t, p); got != c.content {
					t.Errorf("refused append changed the file: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, p); got != c.want {
				t.Errorf("file = %q, want %q", got, c.want)
			}
		})
	}
	if err := AppendRelocated(filepath.Join(t.TempDir(), "missing.jsonl"), testSID, "/new", false); err == nil {
		t.Error("missing file accepted")
	}
}

// The last line can be larger than one tail window; the search keeps
// reading backwards until it finds the line start.
func TestAppendRelocatedLongLastLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), testSID+".jsonl")
	big := `{"type":"assistant","text":"` + strings.Repeat("x", 3*tailWindow) + `"}`
	write(t, p, `{"type":"user"}`+"\n"+big)
	if err := AppendRelocated(p, testSID, "/new", false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), big+"\n"+string(RelocatedRecord(testSID, "/new"))) {
		t.Errorf("tail = %q", string(got[len(got)-120:]))
	}
	// A torn long last line is refused.
	write(t, p, `{"type":"user"}`+"\n"+big[:len(big)-2])
	if err := AppendRelocated(p, testSID, "/new", false); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("torn long line err = %v", err)
	}
}
