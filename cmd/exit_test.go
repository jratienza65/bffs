package cmd

import (
	"errors"
	"fmt"
	"testing"
)

var errBadCode = errors.New("wrong pairing code")

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: 0},
		{name: "plain error is 1", err: errors.New("boom"), want: 1},
		{name: "exitError carries its code", err: exitWith(2, errBadCode), want: 2},
		{name: "interrupt code", err: exitWith(130, errors.New("interrupted")), want: 130},
		{name: "wrapped exitError is found through the chain", err: fmt.Errorf("import: %w", exitWith(2, errBadCode)), want: 2},
		{name: "exitWith nil stays nil", err: exitWith(2, nil), want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// The message cobra prints is the cause's, and the cause stays reachable
// with errors.Is so callers can still branch on sentinel errors.
func TestExitErrorMessageAndUnwrap(t *testing.T) {
	err := exitWith(2, errBadCode)
	if err.Error() != errBadCode.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), errBadCode.Error())
	}
	if !errors.Is(err, errBadCode) {
		t.Error("errors.Is must see through exitError to the cause")
	}
	if !errors.Is(fmt.Errorf("outer: %w", err), errBadCode) {
		t.Error("errors.Is must see through an outer wrap and the exitError")
	}
}
