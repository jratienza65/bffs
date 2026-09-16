//go:build windows

package shim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// execProcess on Windows forks a child, forwards stdio + signals, and returns
// the child's exit code. There's no equivalent of syscall.Exec, so the shim
// stays alive until the child finishes.
func execProcess(path string, args, env []string) (int, error) {
	c := exec.Command(path, args...)
	c.Env = env
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr

	if err := c.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", path, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		for sig := range sigCh {
			_ = c.Process.Signal(sig)
		}
	}()

	err := c.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait %s: %w", path, err)
}
