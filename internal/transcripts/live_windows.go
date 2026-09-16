//go:build windows

package transcripts

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

const (
	// processQueryLimitedInformation is the least access that lets
	// OpenProcess succeed for another user's process on modern Windows.
	processQueryLimitedInformation = 0x1000
	// stillActive is the GetExitCodeProcess value of a running process.
	stillActive = 259
)

// pidExists reports whether pid names a running process. Access denied
// means the process exists but is not ours — alive, the conservative
// answer. A handle that opens but reports an exit code is a finished
// process someone still holds a handle to.
func pidExists(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// expectedStart returns the creation FILETIME Claude recorded for the
// session (procStartFt, a number or numeric string) as a decimal string.
func expectedStart(rec runtimeSession) (string, bool) {
	s := strings.Trim(strings.TrimSpace(string(rec.ProcStartFt)), `"`)
	if s == "" || s == "null" {
		return "", false
	}
	if ft, err := strconv.ParseUint(s, 10, 64); err == nil {
		return strconv.FormatUint(ft, 10), true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return strconv.FormatUint(uint64(f), 10), true
	}
	return "", false
}

// startMatches compares two decimal FILETIME strings.
func startMatches(got, want string) bool {
	g, err1 := strconv.ParseUint(got, 10, 64)
	w, err2 := strconv.ParseUint(want, 10, 64)
	return err1 == nil && err2 == nil && g == w
}

// procStart returns the process creation time of pid (GetProcessTimes) as
// a decimal FILETIME string.
func procStart(pid int) (string, error) {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return "", fmt.Errorf("process times of %d: %w", pid, err)
	}
	ft := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	return strconv.FormatUint(ft, 10), nil
}
