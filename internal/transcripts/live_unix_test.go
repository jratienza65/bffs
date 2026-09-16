//go:build !windows

package transcripts

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestPidExists(t *testing.T) {
	if !pidExists(os.Getpid()) {
		t.Error("own pid reported dead")
	}
	if pidExists(deadPID(t)) {
		t.Error("exited helper reported alive")
	}
}

func TestPidExistsEPERMCountsAsAlive(t *testing.T) {
	old := killFn
	t.Cleanup(func() { killFn = old })
	killFn = func(pid int, sig syscall.Signal) error {
		switch pid {
		case 111:
			return syscall.EPERM
		case 222:
			return syscall.ESRCH
		}
		return nil
	}
	if !pidExists(111) {
		t.Error("EPERM (someone else's process) must count as alive")
	}
	if pidExists(222) {
		t.Error("ESRCH must count as dead")
	}
	if !pidExists(333) {
		t.Error("signal 0 succeeding must count as alive")
	}
}

func TestLiveEPERMProcessIsLive(t *testing.T) {
	old := killFn
	t.Cleanup(func() { killFn = old })
	killFn = func(pid int, sig syscall.Signal) error { return syscall.EPERM }

	cfg := t.TempDir()
	writeRuntimeSession(t, cfg, 424242, "sid-other-user", "/w", fakeStartUnix)
	injectProcStart(t, map[int]string{424242: fakeStartUnix}, nil)
	live, err := Live(context.Background(), []string{cfg})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if ls, ok := live["sid-other-user"]; !ok || !ls.Verified || ls.PID != 424242 {
		t.Errorf("live = %+v, want sid-other-user verified", live)
	}
}

func TestProcStartRunsPS(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not on PATH")
	}
	got, err := procStart(os.Getpid())
	if err != nil {
		t.Fatalf("procStart(self): %v", err)
	}
	// "Sat Sep  5 00:56:45 2026": weekday month day time year.
	if f := strings.Fields(got); len(f) != 5 {
		t.Errorf("procStart(self) = %q, want an lstart line with 5 fields", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("procStart(self) = %q has surrounding whitespace", got)
	}
	if _, err := procStart(deadPID(t)); err == nil {
		t.Error("procStart(dead pid) = nil error, want error")
	}
}

func TestPSCommandPinsStdioAndEnv(t *testing.T) {
	t.Setenv(envTransferCode, "ABCD-EFGH")
	t.Setenv("TZ", "Europe/Berlin")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	cmd, out := psCommand(context.Background(), 1)
	if cmd.Stdout == nil || cmd.Stderr == nil || out == nil {
		t.Fatal("ps command must pin Stdout and Stderr")
	}
	if cmd.Env == nil {
		t.Fatal("ps command must set Env explicitly")
	}
	for _, kv := range cmd.Env {
		switch {
		case strings.HasPrefix(kv, envTransferCode+"="):
			t.Errorf("env leaks %s", envTransferCode)
		case strings.HasPrefix(kv, "TZ=") && kv != "TZ=UTC":
			t.Errorf("TZ = %q, want UTC", kv)
		case strings.HasPrefix(kv, "LC_ALL=") && kv != "LC_ALL=C":
			t.Errorf("LC_ALL = %q, want C", kv)
		}
	}
	if !containsEnv(cmd.Env, "TZ=UTC") || !containsEnv(cmd.Env, "LC_ALL=C") {
		t.Errorf("env %v lacks TZ=UTC / LC_ALL=C", cmd.Env)
	}
	if want := []string{"ps", "-o", "lstart=", "-p", "1"}; strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

func TestExpectedStartUnix(t *testing.T) {
	if s, ok := expectedStart(runtimeSession{ProcStart: " " + fakeStartUnix + "\n"}); !ok || s != fakeStartUnix {
		t.Errorf("expectedStart = (%q, %v)", s, ok)
	}
	if _, ok := expectedStart(runtimeSession{ProcStartFt: []byte("133")}); ok {
		t.Error("procStartFt alone is not a unix reference")
	}
	if !startMatches("Sat Sep  5 00:56:45 2026   ", "Sat Sep 5 00:56:45 2026") {
		t.Error("startMatches must ignore whitespace runs")
	}
	if startMatches("Sat Sep  5 00:56:45 2026", "Sat Sep  5 00:56:46 2026") {
		t.Error("startMatches must not equate different times")
	}
}
