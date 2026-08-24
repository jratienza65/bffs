//go:build windows

package runner

import "os/exec"

// setProcessGroup is a no-op on Windows: CommandContext's default cancel
// kills the direct child, and WaitDelay unblocks Wait if grandchildren hold
// the stdio pipes. Grandchild processes may survive a timeout — acceptable
// for v1 (Job Objects would fix it).
func setProcessGroup(c *exec.Cmd) {}
