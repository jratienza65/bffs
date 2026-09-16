package cmd

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/spf13/cobra"
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
		{name: "exitError carries its code", err: exitWith(exitRetry, errBadCode), want: exitRetry},
		{name: "interrupt code", err: exitWith(exitInterrupt, errors.New("interrupted")), want: exitInterrupt},
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

// The tree tags flag and argument errors so a wrapper can tell "you
// typed it wrong" (2) from "it did not work" (1).
func TestUsageErrorsExitTwo(t *testing.T) {
	root := &cobra.Command{Use: "root", Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error { return nil }}
	sub := &cobra.Command{Use: "sub", Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error { return errors.New("the work failed") }}
	sub.Flags().Bool("real", false, "")
	root.AddCommand(sub)
	tagUsageErrors(root)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"a clean run", []string{"sub", "x"}, 1}, // the work failed, not the usage
		{"an unknown flag", []string{"sub", "--nope", "x"}, exitUsage},
		{"too few arguments", []string{"sub"}, exitUsage},
		{"an unknown command", []string{"nosuch"}, exitUsage},
		{"arguments the root takes none of", []string{"a", "b"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root.SetArgs(tc.args)
			if got := exitCode(root.Execute()); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}
