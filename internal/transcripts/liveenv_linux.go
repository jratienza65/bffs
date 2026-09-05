//go:build linux

package transcripts

import (
	"io"
	"os"
	"strconv"
)

// readProcEnviron reads /proc/<pid>/environ, which the kernel serves for
// the caller's own processes (ptrace access mode; root sees all).
func readProcEnviron(pid int) ([]byte, error) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxProcEnviron))
}
