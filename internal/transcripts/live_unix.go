//go:build !windows

package transcripts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// psTimeout bounds the ps subprocess behind procStart.
const psTimeout = 5 * time.Second

// killFn is syscall.Kill, a variable so tests can simulate EPERM and ESRCH
// without owning such processes.
var killFn = syscall.Kill

// pidExists reports whether pid names a running process: signal 0 succeeds,
// or fails with EPERM (the process exists but belongs to someone else —
// counts as alive, the conservative answer).
func pidExists(pid int) bool {
	err := killFn(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// expectedStart returns the start time Claude recorded for the session
// (procStart, the `ps -o lstart=` line at launch), if any.
func expectedStart(rec runtimeSession) (string, bool) {
	s := strings.TrimSpace(rec.ProcStart)
	return s, s != ""
}

// startMatches compares two lstart lines after whitespace normalisation.
func startMatches(got, want string) bool {
	return normalizeWS(got) == normalizeWS(want)
}

// procStart runs `LC_ALL=C TZ=UTC ps -o lstart= -p <pid>` and returns the
// trimmed line, the same shape Claude stores in procStart.
func procStart(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	cmd, out := psCommand(ctx, pid)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ps -p %d: %w", pid, err)
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return "", fmt.Errorf("ps -p %d: no start time", pid)
	}
	return s, nil
}

// psCommand builds the ps invocation with stdout captured and stderr
// discarded — never inherited — and a C locale in UTC so the line is
// byte-comparable with what Claude recorded.
func psCommand(ctx context.Context, pid int) (*exec.Cmd, *bytes.Buffer) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.Env = childEnv("LC_ALL=C", "TZ=UTC")
	return cmd, &out
}
