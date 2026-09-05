package rehome

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/fsutil"
)

func histLine(sid string, ts int64, display string) []byte {
	return []byte(`{"display":"` + display + `","pastedContents":{},"project":"/Users/jonas/old","sessionId":"` + sid + `","timestamp":` + itoa(ts) + `}`)
}

func itoa(n int64) string {
	var b [24]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestAppendHistory(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "history.jsonl")
	existing := `{"display":"old","pastedContents":{},"project":"/x","sessionId":"` + testSID + `","timestamp":1000}` + "\n" +
		`{"display":"other","sessionId":"` + otherSID + `","timestamp":2000}` + "\n"
	write(t, path, existing)

	big := append([]byte(`{"sessionId":"`+testSID+`","timestamp":5,"display":"`), bytes.Repeat([]byte("x"), 2<<20)...)
	big = append(big, []byte(`"}`)...)
	lines := [][]byte{
		histLine(testSID, 1000, "dup-of-existing"),
		histLine(testSID, 3000, "new one"),
		histLine(testSID, 3000, "dup-in-batch"),
		[]byte(`{"display":"wrong sid","sessionId":"` + otherSID + `","timestamp":4000}`),
		[]byte(`not json`),
		[]byte(`["array"]`),
		[]byte(`{"display":"no timestamp","sessionId":"` + testSID + `"}`),
		[]byte(`{"display":"extra","sessionId":"` + testSID + `","timestamp":6000,"secret":"drop","project":"/old"}`),
		big,
		[]byte("  \n"),
	}
	added, err := AppendHistory(path, testSID, lines, "/home/jonas/src/bffs")
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, existing) {
		t.Errorf("existing content changed:\n%s", got)
	}
	tail := strings.TrimPrefix(got, existing)
	want := `{"display":"new one","pastedContents":{},"project":"/home/jonas/src/bffs","sessionId":"` + testSID + `","timestamp":3000}` + "\n" +
		`{"display":"extra","project":"/home/jonas/src/bffs","sessionId":"` + testSID + `","timestamp":6000}` + "\n"
	if tail != want {
		t.Errorf("appended:\n%s\nwant:\n%s", tail, want)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "dup") || strings.Contains(got, "no timestamp") {
		t.Errorf("dropped lines leaked: %s", got)
	}
	if exists(historyLockPath(path)) {
		t.Errorf("lock dir left behind")
	}

	// Re-appending the same batch adds nothing.
	added, err = AppendHistory(path, testSID, lines, "/home/jonas/src/bffs")
	if err != nil || added != 0 {
		t.Errorf("second append: added=%d err=%v", added, err)
	}
	if readFile(t, path) != got {
		t.Errorf("file changed on a no-op append")
	}
}

func TestAppendHistoryCreatesFileAndKeepsProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	added, err := AppendHistory(path, testSID, [][]byte{histLine(testSID, 1, "hi")}, "")
	if err != nil || added != 1 {
		t.Fatalf("added=%d err=%v", added, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	if got := readFile(t, path); !strings.Contains(got, `"project":"/Users/jonas/old"`) {
		t.Errorf("project not kept when newProject is empty: %s", got)
	}
	if exists(historyLockPath(path)) {
		t.Errorf("lock dir left behind")
	}
}

func TestAppendHistoryRepairsMissingTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	write(t, path, `{"display":"torn","sessionId":"`+otherSID+`","timestamp":1}`)
	if _, err := AppendHistory(path, testSID, [][]byte{histLine(testSID, 2, "x")}, ""); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "\"timestamp\":1}\n{\"display\":\"x\"") {
		t.Errorf("separator missing:\n%s", got)
	}
}

func TestAppendHistoryCaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	lines := make([][]byte, historyMaxLines+5)
	for i := range lines {
		lines[i] = histLine(testSID, int64(i), "p")
	}
	added, err := AppendHistory(path, testSID, lines, "")
	if err != nil {
		t.Fatal(err)
	}
	if added != historyMaxLines {
		t.Errorf("added = %d, want %d", added, historyMaxLines)
	}
	if _, err := AppendHistory(path, "bad", lines[:1], ""); err == nil {
		t.Errorf("bad sid accepted")
	}
	if added, err := AppendHistory(path, testSID, nil, ""); err != nil || added != 0 {
		t.Errorf("empty batch: %d %v", added, err)
	}
}

func TestAppendHistoryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	write(t, path, "")
	lock := historyLockPath(path)
	if !strings.HasSuffix(lock, "history.jsonl.lock") {
		t.Fatalf("lock path = %q", lock)
	}
	// A stale holder (older than 10 s) is swept and the append proceeds.
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	chtimes(t, lock, time.Now().Add(-time.Minute))
	if added, err := AppendHistory(path, testSID, [][]byte{histLine(testSID, 1, "a")}, ""); err != nil || added != 1 {
		t.Fatalf("stale lock: added=%d err=%v", added, err)
	}
	if exists(lock) {
		t.Errorf("lock dir left behind")
	}
	// A live holder blocks: use a short wait through fsutil.Lock to prove
	// the same directory is contended, then release.
	release, err := fsutil.Lock(lock, 10*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsutil.Lock(lock, 10*time.Second, 0); !errors.Is(err, fsutil.ErrLocked) {
		t.Errorf("second holder err = %v", err)
	}
	release()
}

func TestHistoryLockPathFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	home := t.TempDir()
	real := filepath.Join(home, "history.jsonl")
	write(t, real, "")
	acct := t.TempDir()
	link := filepath.Join(acct, "history.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realResolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got := historyLockPath(link); got != realResolved+".lock" {
		t.Errorf("lock path = %q, want %q", got, realResolved+".lock")
	}
	// Missing file: the directory is still resolved.
	missing := filepath.Join(acct, "nope.jsonl")
	acctResolved, _ := filepath.EvalSymlinks(acct)
	if got := historyLockPath(missing); got != filepath.Join(acctResolved, "nope.jsonl.lock") {
		t.Errorf("missing file lock path = %q", got)
	}
}
