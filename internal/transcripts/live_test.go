package transcripts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// deadPID returns the pid of a process that has already exited: the test
// binary itself, run with no tests selected.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn helper: %v", err)
	}
	return cmd.Process.Pid
}

// startField renders the per-OS reference start time for a sessions/<pid>
// .json file: procStart (a ps line) on unix, procStartFt (a FILETIME) on
// windows. fakeStart is the value the injected ProcStart returns for it.
const (
	fakeStartUnix    = "Sat Sep  5 00:56:45 2026"
	fakeStartWindows = "133700000000000000"
)

func fakeStart() string {
	if runtime.GOOS == "windows" {
		return fakeStartWindows
	}
	return fakeStartUnix
}

func startField(v string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`"procStartFt": %s`, v)
	}
	return fmt.Sprintf(`"procStart": %q`, v)
}

func writeRuntimeSession(t *testing.T, cfg string, pid int, sid, cwd, start string) {
	t.Helper()
	extra := ""
	if start != "" {
		extra = ", " + startField(start)
	}
	body := fmt.Sprintf(`{"pid": %d, "sessionId": %q, "cwd": %q, "kind": "interactive", "status": "idle"%s}`, pid, sid, cwd, extra)
	writeFile(t, filepath.Join(cfg, RuntimeSessionsSubdir, strconv.Itoa(pid)+".json"), body)
}

// injectProcStart replaces ProcStart for the test: pids in starts get
// their value, every other pid gets err.
func injectProcStart(t *testing.T, starts map[int]string, err error) *int {
	t.Helper()
	calls := 0
	old := ProcStart
	ProcStart = func(pid int) (string, error) {
		calls++
		if s, ok := starts[pid]; ok {
			return s, nil
		}
		return "", err
	}
	t.Cleanup(func() { ProcStart = old })
	return &calls
}

func TestLive(t *testing.T) {
	cfg := t.TempDir()
	own, dead := os.Getpid(), deadPID(t)
	writeRuntimeSession(t, cfg, own, "sid-live", "/work/live", fakeStart())
	writeRuntimeSession(t, cfg, dead, "sid-dead", "/work/dead", fakeStart())
	writeFile(t, filepath.Join(cfg, RuntimeSessionsSubdir, strconv.Itoa(own)+".deadbeef.key"), `{"peerToken":"never-read"}`)
	writeFile(t, filepath.Join(cfg, RuntimeSessionsSubdir, "garbage.json"), `{not json`)
	writeFile(t, filepath.Join(cfg, RuntimeSessionsSubdir, "nopid.json"), `{"sessionId":"sid-nopid"}`)
	mkdir(t, filepath.Join(cfg, RuntimeSessionsSubdir, "subdir.json"))

	// ps padding on unix must not matter; on windows the seam returns the
	// same decimal string.
	got := fakeStart()
	if runtime.GOOS != "windows" {
		got = "  Sat Sep 5  00:56:45 2026   \n"
	}
	// The dead pid, if ever probed, reports a different start time so a
	// reused pid can never turn the test green by accident.
	injectProcStart(t, map[int]string{own: got, dead: "Mon Jan  1 00:00:00 2001"}, nil)

	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("live = %+v, want exactly sid-live", live)
	}
	ls, ok := live["sid-live"]
	if !ok {
		t.Fatalf("sid-live missing from %+v", live)
	}
	want := LiveSession{PID: own, SessionID: "sid-live", Cwd: "/work/live", ConfigDir: cfg, Verified: true}
	if ls != want {
		t.Errorf("LiveSession = %+v, want %+v", ls, want)
	}
}

func TestLiveStartMismatchIsNotLive(t *testing.T) {
	cfg := t.TempDir()
	own := os.Getpid()
	writeRuntimeSession(t, cfg, own, "sid-reused", "/w", fakeStart())
	other := "Mon Jan  1 00:00:00 2001"
	if runtime.GOOS == "windows" {
		other = "1"
	}
	injectProcStart(t, map[int]string{own: other}, nil)
	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a pid whose start time differs from the record is a reused pid; got %+v", live)
	}
}

func TestLiveUnverifiedWhenCheckCannotRun(t *testing.T) {
	cfg := t.TempDir()
	own := os.Getpid()
	writeRuntimeSession(t, cfg, own, "sid-unv", "/w", fakeStart())
	injectProcStart(t, nil, errors.New("ps: not found"))
	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	ls, ok := live["sid-unv"]
	if !ok || ls.Verified {
		t.Errorf("live = %+v, want sid-unv present and Verified=false", live)
	}
}

func TestLiveNoReferenceStartIsUnverifiedLive(t *testing.T) {
	cfg := t.TempDir()
	own := os.Getpid()
	writeRuntimeSession(t, cfg, own, "sid-noref", "/w", "")
	calls := injectProcStart(t, nil, errors.New("must not be called"))
	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	ls, ok := live["sid-noref"]
	if !ok || ls.Verified || ls.PID != own {
		t.Errorf("live = %+v, want sid-noref present, unverified", live)
	}
	if *calls != 0 {
		t.Errorf("ProcStart called %d times without a reference start time", *calls)
	}
}

func TestLiveDedupesConfigDirs(t *testing.T) {
	cfgA, cfgB := t.TempDir(), t.TempDir()
	own := os.Getpid()
	writeRuntimeSession(t, cfgA, own, "sid-a", "/a", fakeStart())
	if runtime.GOOS == "windows" {
		// No symlinks: the same dir listed twice must still be scanned once.
		cfgB = cfgA
	} else if err := os.Symlink(filepath.Join(cfgA, RuntimeSessionsSubdir), filepath.Join(cfgB, RuntimeSessionsSubdir)); err != nil {
		t.Fatal(err)
	}
	calls := injectProcStart(t, map[int]string{own: fakeStart()}, nil)
	live, err := Live(context.Background(), []string{cfgA, cfgB, cfgA})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 1 || live["sid-a"].ConfigDir != cfgA {
		t.Errorf("live = %+v, want sid-a once, attributed to the first dir", live)
	}
	if *calls != 1 {
		t.Errorf("ProcStart called %d times, want 1 (dirs deduplicated)", *calls)
	}
}

func TestLiveMissingSessionsDir(t *testing.T) {
	injectProcStart(t, nil, errors.New("unused"))
	live, err := Live(context.Background(), []string{t.TempDir(), "", filepath.Join(t.TempDir(), "gone")})
	if err != nil || len(live) != 0 {
		t.Errorf("Live = (%+v, %v), want empty and nil", live, err)
	}
}

func TestLiveHonoursContext(t *testing.T) {
	cfg := t.TempDir()
	writeRuntimeSession(t, cfg, os.Getpid(), "sid-ctx", "/w", fakeStart())
	injectProcStart(t, map[int]string{os.Getpid(): fakeStart()}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Live(ctx, []string{cfg}); !errors.Is(err, context.Canceled) {
		t.Errorf("Live with a cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestParsePID(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		ok   bool
	}{
		{"2530", 2530, true},
		{`"2530"`, 2530, true},
		{" 7 ", 7, true},
		{"0", 0, false},
		{"-3", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"null", 0, false},
	}
	for _, c := range cases {
		got, ok := parsePID([]byte(c.raw))
		if got != c.want || ok != c.ok {
			t.Errorf("parsePID(%q) = (%d, %v), want (%d, %v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeWS(t *testing.T) {
	if got := normalizeWS("  Sat Sep  5 00:56:45 2026 \n"); got != "Sat Sep 5 00:56:45 2026" {
		t.Errorf("normalizeWS = %q", got)
	}
}
