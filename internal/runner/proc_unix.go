//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and replaces the
// default context-cancel behavior (kill the leader only) with a kill of the
// whole group — claude spawns tool subprocesses that would otherwise survive
// a timeout. SIGKILL rather than SIGTERM: cancellation here means a hung
// headless run past its deadline; there is no state worth a graceful
// shutdown.
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil || c.Process.Pid <= 0 {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) // negative pid = whole group
	}
}
