//go:build bffs_notui

package cmd

import (
	"context"
	"errors"
)

// errTUIDisabled is what runTUI returns in a bffs_notui build; the bare
// `sessions` and `memory` commands then print the table instead.
var errTUIDisabled = errors.New("interactive browser disabled in this build (bffs_notui)")

// tuiSupported is always false without the browser linked in.
func tuiSupported() bool { return false }

// runTUI refuses: this build has no interactive browser.
func runTUI(context.Context, string, string, string) error { return errTUIDisabled }
