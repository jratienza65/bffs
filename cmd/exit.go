package cmd

import "errors"

// exitError carries a process exit code out of a RunE so Execute can end the
// process with it instead of the blanket 1. Wrap a cause with exitWith; the
// message cobra prints is the cause's. Codes by convention: 1 any failure,
// 2 a retryable one a wrapper can act on (e.g. a wrong pairing code), 130
// interrupted (Ctrl-C).
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
