//go:build darwin

package transcripts

import (
	"golang.org/x/sys/unix"
)

// readProcEnviron reads pid's environment through kern.procargs2, which the
// kernel serves for the caller's own processes (root sees all). The buffer
// also carries the arguments; only the environment part is returned.
func readProcEnviron(pid int) ([]byte, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	env, err := procargs2Environ(buf)
	if err != nil {
		clear(buf)
		return nil, err
	}
	return env, nil
}
