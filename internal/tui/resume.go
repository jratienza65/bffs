package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/runner"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// resumeDoneMsg ends a `claude --resume` the browser handed the terminal.
type resumeDoneMsg struct {
	id  string
	err error
}

// resumeCommand prepares `claude --resume <sid>` for s the way the plan
// specifies (§11): runner.Command on porter.ResumeAccount's account in
// the session's cwd, ProcessGroup false and nil stdio so bubbletea's
// ExecProcess wires the terminal in. It is refused when the session is
// open in a running claude or its cwd is missing here. A session no
// account claims (the unmanaged home, no rule for the cwd) runs the
// `claude` on PATH — the shim, which then injects nothing.
func resumeCommand(svc *services, s transcripts.Session) (*exec.Cmd, func(), error) {
	if s.Live {
		return nil, nil, fmt.Errorf("session %s is open in a running claude; nothing to resume", shortID(s.ID))
	}
	cwd := s.Cwd
	if cwd == "" {
		if m := readMeta(s.Path, nil); !m.Failed {
			cwd = m.Cwd
		}
	}
	switch {
	case cwd == "":
		return nil, nil, fmt.Errorf("session %s records no cwd; nothing to resume into", shortID(s.ID))
	case !isDir(cwd):
		return nil, nil, fmt.Errorf("cwd %s is missing on this machine — rehome the session first (r)", shortPath(cwd))
	case s.Root.Orphan:
		return nil, nil, errors.New("orphan session dir: run it by hand — " + resumeLine(s))
	}
	account := porter.ResumeAccount(svc.cfgDir, s.Root, cwd)
	args := []string{"--resume", s.ID}
	if account == "" {
		path, err := exec.LookPath("claude")
		if err != nil {
			return nil, nil, fmt.Errorf("no claude on PATH: %w", err)
		}
		c := exec.CommandContext(svc.ctx, path, args...)
		c.Dir = cwd
		c.Env = runner.StripSessionMarkers(os.Environ())
		return c, func() {}, nil
	}
	return runner.Command(svc.ctx, svc.cfgDir, runner.Request{Account: account, Dir: cwd, Args: args, ProcessGroup: false})
}

// resume is the command that hands the terminal to claude for s and
// reports when it exits.
func resume(svc *services, s transcripts.Session) tea.Cmd {
	c, cleanup, err := resumeCommand(svc, s)
	if err != nil {
		return statusError(err)
	}
	id := s.ID
	return tea.ExecProcess(c, func(err error) tea.Msg {
		cleanup()
		return resumeDoneMsg{id: id, err: err}
	})
}
