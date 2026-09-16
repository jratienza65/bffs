package cmd

import (
	"errors"

	"github.com/spf13/cobra"
)

// Exit codes follow the convention the cli-design skill sets out, so a
// wrapper script can tell the cases apart without reading the message:
//
//	0    the command did what it was asked
//	1    it failed
//	2    invalid usage — an unknown flag, a bad argument, a missing one
//	75   a failure that is safe to retry, with nothing written (EX_TEMPFAIL:
//	     a wrong pairing code, where the other machine keeps serving)
//	130  interrupted (128 + SIGINT)
//
// 75 is only for a run that changed nothing: once a transfer has landed
// anything, the answer is a report and a rehome, not a blind retry.
const (
	exitUsage     = 2
	exitRetry     = 75
	exitInterrupt = 130
)

// exitError carries a process exit code out of a RunE so Execute can end the
// process with it instead of the blanket 1. Wrap a cause with exitWith; the
// message cobra prints is the cause's.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitWith wraps err so that Execute exits the process with code. A nil err
// returns nil (no error, exit 0), so it can sit directly on a return.
func exitWith(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// usageError tags a cobra flag or argument error so it exits 2. Cobra
// reports both as ordinary errors, and a wrapper cannot tell "you typed
// it wrong" from "it did not work" out of exit 1.
func usageError(err error) error { return exitWith(exitUsage, err) }

// tagUsageErrors wraps every command's argument validator in the tree so
// a bad invocation exits 2. It runs once, at the end of the tree's
// construction; a command added later is covered by its parent's call.
func tagUsageErrors(c *cobra.Command) {
	inner := c.Args
	if inner == nil {
		inner = cobra.ArbitraryArgs
	}
	c.Args = func(cmd *cobra.Command, args []string) error { return usageError(inner(cmd, args)) }
	c.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return usageError(err) })
	for _, sub := range c.Commands() {
		tagUsageErrors(sub)
	}
}

// exitCode is the process exit status for an error returned by the cobra
// tree: 0 for nil, the wrapped code for an exitError anywhere in the chain,
// 1 for everything else.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}
