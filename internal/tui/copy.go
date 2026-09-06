package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// copyState is where the Copy screen is.
type copyState int

const (
	copyPick     copyState = iota // choosing the destination account
	copyPlanning                  // porter.Select
	copyConfirm                   // plan line + [y/N]
	copyRunning                   // porter.CopyLocal
)

// copyRow is one destination the account list offers.
type copyRow struct {
	name string // account name, or "home"
	typ  string
	root transcripts.Root
	note string // why the row is greyed ("" = selectable)
}

// copyPlannedMsg ends the selection.
type copyPlannedMsg struct {
	sel      porter.Selection
	live     map[string]transcripts.LiveSession
	warnings []string
	err      error
}

// copyDoneMsg ends the copy.
type copyDoneMsg struct {
	rep porter.Report
	err error
}

// copyScreen is `bffs copy --to <account>` for the target (copy only —
// the move variant with its typed count stays a CLI affair): an account
// list with the destinations that share the source pool greyed, the plan
// line, [y/N], progress, the receipt.
type copyScreen struct {
	svc      *services
	tgt      actionTarget
	state    copyState
	rows     []copyRow
	cursor   int
	dest     copyRow
	sel      porter.Selection
	live     map[string]transcripts.LiveSession
	warnings []string
	op       *op
	prog     opView
	askQuit  bool
	box      scrollBox
}

// copyRows lists every account of accounts.toml plus "home" with the
// root each works in; a destination that is the source pool itself is
// greyed with the plan's hint (there is nothing to copy — what differs
// per account is trust), and so is an unresolvable one.
func copyRows(svc *services, src transcripts.Root) []copyRow {
	names := append([]string{}, svc.accs.Names()...)
	sort.Strings(names)
	names = append(names, transcripts.HomeName)
	rows := make([]copyRow, 0, len(names))
	for _, name := range names {
		r := copyRow{name: name, typ: string(transcripts.HomeName)}
		if acc, ok := svc.accs.Get(name); ok {
			r.typ = string(acc.Type)
			if acc.Type == store.TypeOAuth {
				r.typ += "/" + string(store.ResolveIsolation(acc.Isolation, svc.state.Isolation))
			}
		}
		root, err := rootForAccount(svc, name)
		switch {
		case err != nil:
			r.note = err.Error()
		case porter.SameRoot(src, root):
			r.note = "already shared under partial isolation — run trust sync (t)"
		}
		r.root = root
		rows = append(rows, r)
	}
	return rows
}

func newCopyScreen(svc *services, tgt actionTarget) *copyScreen {
	s := &copyScreen{svc: svc, tgt: tgt, rows: copyRows(svc, tgt.root), prog: newOpView("copying")}
	for i, r := range s.rows {
		if r.note == "" {
			s.cursor = i
			break
		}
	}
	return s
}

func (s *copyScreen) Init() tea.Cmd { return nil }
func (s *copyScreen) Title() string {
	if s.tgt.only == "memories" {
		return "sync memory to account"
	}
	return "copy to account"
}
func (s *copyScreen) running() bool { return s.op.active() }

func (s *copyScreen) Keys() []key.Binding {
	switch s.state {
	case copyPick:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose"))}
	case copyConfirm:
		return append([]key.Binding{keys.Yes, keys.No}, scrollKeys()...)
	}
	return []key.Binding{keys.Cancel}
}

// planCopy resolves the selection for the plan line.
func planCopy(svc *services, tgt actionTarget) tea.Cmd {
	ctx := svc.ctx
	return func() tea.Msg {
		sel, live, warnings, err := tgt.selection(ctx, svc)
		return copyPlannedMsg{sel: sel, live: live, warnings: warnings, err: err}
	}
}

func (s *copyScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return s, nil

	case progressMsg:
		s.prog.update(msg.p)
		return s, s.op.wait()

	case copyPlannedMsg:
		if s.state != copyPlanning {
			return s, nil
		}
		if msg.err != nil {
			s.state = copyPick
			return s, statusError(msg.err)
		}
		s.sel, s.live, s.warnings = msg.sel, msg.live, msg.warnings
		if len(s.sel.Sessions) == 0 && len(s.sel.Memories) == 0 {
			s.state = copyPick
			return s, status("nothing selected: no sessions or memory dirs matched")
		}
		s.state = copyConfirm
		return s, nil

	case copyDoneMsg:
		s.op.finish()
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || s.op.cancelled() {
				lines := copyReceiptLines(msg.rep)
				if len(msg.rep.Imported)+len(msg.rep.Pending)+len(msg.rep.MemoryDirs) == 0 {
					return s, tea.Batch(popScreen(), status("copy cancelled; nothing written"))
				}
				return s, replaceScreen(newResultScreen("copy", append([]string{"copy cancelled; what landed before:"}, lines...), nil))
			}
			lines := copyReceiptLines(msg.rep)
			if len(msg.rep.Imported)+len(msg.rep.Pending)+len(msg.rep.MemoryDirs) > 0 {
				lines = append([]string{"copy stopped; what landed before the failure:"}, lines...)
			}
			return s, replaceScreen(newResultScreen("copy", lines, msg.err))
		}
		return s, replaceScreen(newResultScreen("copy", copyReceiptLines(msg.rep), nil))

	case tea.KeyPressMsg:
		return s.key(msg)
	}
	return s, nil
}

func (s *copyScreen) key(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.askQuit {
		switch yesNo(msg) {
		case 1:
			return s, s.op.stopThenQuit()
		case -1:
			s.askQuit = false
		}
		return s, nil
	}
	switch s.state {
	case copyPick:
		switch {
		case key.Matches(msg, keys.Up):
			s.cursor = max(0, s.cursor-1)
		case key.Matches(msg, keys.Down):
			s.cursor = min(len(s.rows)-1, s.cursor+1)
		case key.Matches(msg, keys.Open):
			if len(s.rows) == 0 {
				return s, nil
			}
			r := s.rows[s.cursor]
			if r.note != "" {
				return s, status(fmt.Sprintf("%s: %s", r.name, r.note))
			}
			s.dest = r
			s.state = copyPlanning
			return s, planCopy(s.svc, s.tgt)
		}
		return s, nil

	case copyConfirm:
		if s.box.key(msg) {
			return s, nil
		}
		switch yesNo(msg) {
		case 1:
			return s.start()
		case -1:
			return s, tea.Batch(popScreen(), status("copy aborted; nothing written"))
		}
		return s, nil

	case copyPlanning:
		if key.Matches(msg, keys.Cancel) {
			// porter.Select is still running; the screen leaves and the
			// late plan lands on no screen.
			return s, popScreen()
		}

	case copyRunning:
		switch {
		case key.Matches(msg, keys.Cancel):
			s.op.stop()
		case key.Matches(msg, keys.Quit):
			s.askQuit = true
		}
	}
	return s, nil
}

// start runs porter.CopyLocal — the path `bffs copy` takes: the
// selection streamed through an io.Pipe into Import, the collision scan
// told to ignore the source pool.
func (s *copyScreen) start() (Screen, tea.Cmd) {
	s.state = copyRunning
	opts := porter.ImportOptions{
		Dest:      s.dest.root,
		Limits:    bundle.DefaultLimits,
		Now:       s.svc.now(),
		Live:      s.live,
		LaunchEnv: os.Environ(),
	}
	if s.dest.name != transcripts.HomeName {
		opts.Account = s.dest.name
	}
	sel, cfgDir := s.sel, s.svc.cfgDir
	var cmd tea.Cmd
	s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
		opts.Progress = func(p bundle.Progress) { emit(progressMsg{p: p}) }
		rep, err := porter.CopyLocal(ctx, cfgDir, sel, opts, false)
		return copyDoneMsg{rep: rep, err: err}
	})
	return s, cmd
}

// mouse scrolls the confirmation summary; every other state is
// read-only or driven by a prompt.
func (s *copyScreen) mouse(msg tea.MouseMsg, _, _ int) tea.Cmd {
	if s.state == copyConfirm {
		s.box.mouse(msg)
	}
	return nil
}

func (s *copyScreen) View(width, height int) string {
	head := styleFaint.Render(truncate("copy "+s.tgt.what()+"  from "+shortRootLabel(s.tgt.root), width))
	switch s.state {
	case copyPick, copyPlanning:
		lines := []string{head, "", "  " + pad("ACCOUNT", 16) + " " + pad("TYPE", 14) + " DESTINATION"}
		for i, r := range s.rows {
			line := pad(transcripts.Sanitize(r.name), 16) + " " + pad(r.typ, 14) + " "
			if r.note != "" {
				line += r.note
			} else {
				line += shortRootLabel(r.root) + "  (" + shortPath(r.root.Dir) + ")"
			}
			line = pad(line, max(0, width-2))
			switch {
			case i == s.cursor:
				line = styleCursor.Render("> " + line)
			case r.note != "":
				line = styleFaint.Render("  " + line)
			default:
				line = "  " + line
			}
			lines = append(lines, line)
		}
		if s.state == copyPlanning {
			lines = append(lines, "", "planning…")
		} else {
			lines = append(lines, "", styleFaint.Render(truncate("enter chooses the destination · greyed rows share the source pool · esc goes back", width)))
		}
		return strings.Join(lines, "\n")
	case copyConfirm:
		body := []string{fmt.Sprintf("plan: %s, %s  from %s  to  %s", countNoun(len(s.sel.Sessions), "session"), countNoun(len(s.sel.Memories), "memory dir"), shortPath(s.tgt.root.Dir), shortPath(s.dest.root.Dir))}
		for _, sess := range s.sel.Sessions {
			if sess.Live {
				body = append(body, fmt.Sprintf("  live (copied as is, may be truncated): %s", shortID(sess.ID)))
			}
		}
		for _, w := range s.warnings {
			body = append(body, "warning: "+w)
		}
		what := countNoun(len(s.sel.Sessions), "session")
		if len(s.sel.Sessions) == 0 {
			what = countNoun(len(s.sel.Memories), "memory dir") + " (merged into the destination's memory)"
		}
		return s.box.view([]string{head, ""}, body, []string{"", fmt.Sprintf("copy %s to %s? [y/N]", what, transcripts.Sanitize(s.dest.name))}, width, height)
	}
	lines := []string{head, "", s.prog.view(width), ""}
	switch {
	case s.askQuit:
		lines = append(lines, quitPrompt)
	case s.op.cancelled():
		lines = append(lines, cancelling)
	default:
		lines = append(lines, styleFaint.Render("esc cancels (a session in flight is rolled back; landed ones stay)"))
	}
	return strings.Join(lines, "\n")
}
