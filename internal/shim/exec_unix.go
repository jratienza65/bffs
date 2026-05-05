//go:build !windows

package shim

import (
	"fmt"
	"syscall"
)

// execProcess replaces the current process with the target binary so signals
// and exit codes are forwarded transparently. On success it does not return.
func execProcess(path string, args, env []string) (int, error) {
	argv := append([]string{path}, args...)
	if err := syscall.Exec(path, argv, env); err != nil {
		return 1, fmt.Errorf("exec %s: %w", path, err)
	}
	return 0, nil
}
