package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/term"
)

// Options configures Run. CfgDir is the bffs config dir; HomeClaudeDir
// overrides ~/.claude ("" = the real one); Version is printed in the
// header; Start is the tab a project opens on: "sessions" (default) or
// "memories".
type Options struct {
	CfgDir, HomeClaudeDir, Version string
	Start                          string // "sessions" | "memories"
}

// ErrInterrupted is returned by Run when the program was interrupted by
// a signal (SIGINT/SIGTERM), so the command can exit 130. Ctrl-C typed
// into the browser is a normal quit.
var ErrInterrupted = errors.New("interrupted")

// Supported reports whether the browser can run here: stdin and stdout
// are terminals and TERM is not "dumb". Stricter than the prompts'
// isTTY on purpose — bubbletea takes over both streams.
func Supported() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// Run loads the store and the roots, then runs the browser on the real
// stdin/stdout until the user quits or ctx ends. A store or root
// problem is returned before the terminal is touched, so the caller can
// print it like any other command error.
func Run(ctx context.Context, o Options) error {
	svc, err := loadServices(ctx, o)
	if err != nil {
		return err
	}
	p := tea.NewProgram(newApp(svc), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		switch {
		case errors.Is(err, tea.ErrInterrupted):
			return ErrInterrupted
		case errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil:
			return ctx.Err()
		}
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
