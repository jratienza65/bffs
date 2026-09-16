//go:build !bffs_notui

package cmd

import (
	"context"
	"errors"

	"github.com/jratienza65/bffs/internal/tui"
)

// errTUIDisabled is never returned by this build; it exists so the
// callers' fallback reads the same with and without bffs_notui.
var errTUIDisabled = errors.New("interactive browser disabled in this build")

// tuiSupported reports whether the interactive browser can run here:
// stdin and stdout are terminals and TERM is not "dumb".
func tuiSupported() bool { return tui.Supported() }

// runTUI runs the interactive browser over the store at dir. claudeDir
// overrides ~/.claude ("" = the real one); start is the tab a project
// opens on, "sessions" or "memories". A SIGINT/SIGTERM while it runs
// exits 130 like every other interrupted command.
func runTUI(ctx context.Context, dir, claudeDir, start string) error {
	err := tui.Run(ctx, tui.Options{CfgDir: dir, HomeClaudeDir: claudeDir, Version: Version, Start: start})
	if errors.Is(err, tui.ErrInterrupted) {
		return exitWith(130, err)
	}
	return err
}
