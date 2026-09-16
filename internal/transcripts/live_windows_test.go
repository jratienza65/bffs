//go:build windows

package transcripts

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestPidExistsWindows(t *testing.T) {
	if !pidExists(os.Getpid()) {
		t.Error("own pid reported dead")
	}
	if pidExists(deadPID(t)) {
		t.Error("exited helper reported alive")
	}
}

func TestProcStartWindows(t *testing.T) {
	got, err := procStart(os.Getpid())
	if err != nil {
		t.Fatalf("procStart(self): %v", err)
	}
	ft, err := strconv.ParseUint(got, 10, 64)
	if err != nil || ft == 0 {
		t.Fatalf("procStart(self) = %q, want a decimal FILETIME", got)
	}
	again, err := procStart(os.Getpid())
	if err != nil || again != got {
		t.Errorf("procStart(self) not stable: %q then %q (%v)", got, again, err)
	}
}

func TestLiveGetProcessTimesOwnPid(t *testing.T) {
	old := ProcStart
	t.Cleanup(func() { ProcStart = old })
	ProcStart = procStart // the real one

	ft, err := procStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	cfg := t.TempDir()
	writeRuntimeSession(t, cfg, os.Getpid(), "sid-self", `C:\w`, ft)
	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if ls, ok := live["sid-self"]; !ok || !ls.Verified {
		t.Errorf("live = %+v, want sid-self verified against GetProcessTimes", live)
	}

	// A different creation time is a reused pid.
	n, _ := strconv.ParseUint(ft, 10, 64)
	cfg2 := t.TempDir()
	writeRuntimeSession(t, cfg2, os.Getpid(), "sid-reused", `C:\w`, strconv.FormatUint(n+1, 10))
	live, err = Live(context.Background(), []string{cfg2})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("live = %+v, want none for a mismatching procStartFt", live)
	}
}

func TestExpectedStartWindows(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"133700000000000000", "133700000000000000", true},
		{`"133700000000000000"`, "133700000000000000", true},
		{"1.337e17", "133700000000000000", true},
		{"", "", false},
		{"null", "", false},
		{"abc", "", false},
	}
	for _, c := range cases {
		got, ok := expectedStart(runtimeSession{ProcStartFt: []byte(c.raw)})
		if got != c.want || ok != c.ok {
			t.Errorf("expectedStart(%q) = (%q, %v), want (%q, %v)", c.raw, got, ok, c.want, c.ok)
		}
	}
	if _, ok := expectedStart(runtimeSession{ProcStart: "Sat Sep  5 00:56:45 2026"}); ok {
		t.Error("procStart alone is not a windows reference")
	}
}
