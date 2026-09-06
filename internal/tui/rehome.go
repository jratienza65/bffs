package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/rehome"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// rehomeState is where the Rehome screen is.
type rehomeState int

const (
	rehomeLoading  rehomeState = iota // old cwds + rehome.Suggest
	rehomePick                        // choosing the new directory of one old cwd
	rehomePath                        // typing it
	rehomePlanning                    // transcripts.Live + rehome.PlanRehome
	rehomeConfirm                     // the dry-run plan + [y/N]
	rehomeRunning                     // rehome.Apply
)

// rehomeGroup is one old directory the chosen sessions belong to and the
// local directories it may live in now.
type rehomeGroup struct {
	oldCwd     string
	sessions   []string
	candidates []rehome.Candidate
	newCwd     string // the answer; "" while undecided or skipped
	decided    bool
}

// rehomeLoadedMsg ends the loading step.
type rehomeLoadedMsg struct {
	groups  []rehomeGroup
	oldHome string
	err     error
}

// rehomePlannedMsg ends the planning step.
type rehomePlannedMsg struct {
	plan rehome.Plan
	opts rehome.Options
	err  error
}

// rehomeDoneMsg ends the apply.
type rehomeDoneMsg struct {
	res rehome.Result
	err error
}

// rehomeScreen is `bffs rehome` for the chosen sessions (the selection,
// else the project's pending imports): the old cwd, the candidates
// rehome.Suggest proposes plus a typed path, the dry-run plan
// rehome.PlanRehome makes, [y/N], rehome.Apply, the result with its
// verify lines.
type rehomeScreen struct {
	svc     *services
	tgt     actionTarget
	chosen  []transcripts.Session
	state   rehomeState
	groups  []rehomeGroup
	current int // the group being decided
	cursor  int // the option under the cursor
	input   textinput.Model
	note    string
	oldHome string
	maps    []rehome.Mapping
	plan    rehome.Plan
	opts    rehome.Options
	planned []string
	op      *op
	askQuit bool
	width   int
	loadErr error
	box     scrollBox
}

func newRehomeScreen(svc *services, tgt actionTarget, chosen []transcripts.Session) *rehomeScreen {
	return &rehomeScreen{svc: svc, tgt: tgt, chosen: chosen, input: newInput("path: ", "an existing directory on this machine")}
}

func (s *rehomeScreen) Init() tea.Cmd        { return loadRehome(s.svc, s.chosen) }
func (s *rehomeScreen) Title() string        { return "rehome" }
func (s *rehomeScreen) running() bool        { return s.op.active() }
func (s *rehomeScreen) loading() bool        { return s.state == rehomeLoading || s.state == rehomePlanning }
func (s *rehomeScreen) capturingInput() bool { return s.state == rehomePath }

func (s *rehomeScreen) Keys() []key.Binding {
	switch s.state {
	case rehomePick:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose"))}
	case rehomePath:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "use this path")), keys.Cancel}
	case rehomeConfirm:
		return append([]key.Binding{keys.Yes, keys.No}, scrollKeys()...)
	case rehomeRunning:
		return []key.Binding{keys.Cancel}
	}
	return nil
}

// loadRehome reads the effective cwd of every chosen session whose
// windows were not read yet, groups the sessions by old directory and
// asks rehome.Suggest for candidates — with the import records the
// sessions came from where there are any (git remote, source home).
func loadRehome(svc *services, chosen []transcripts.Session) tea.Cmd {
	return func() tea.Msg {
		var msg rehomeLoadedMsg
		rec := imports.Record{}
		index := map[string]int{}
		for _, sess := range chosen {
			cwd := sess.Cwd
			if cwd == "" {
				if m := readMeta(sess.Path, nil); !m.Failed {
					cwd = m.Cwd
				}
			}
			if cwd == "" {
				continue
			}
			i, ok := index[cwd]
			if !ok {
				i = len(msg.groups)
				index[cwd] = i
				msg.groups = append(msg.groups, rehomeGroup{oldCwd: cwd})
			}
			msg.groups[i].sessions = append(msg.groups[i].sessions, sess.ID)
			is := imports.Session{ID: sess.ID, OldCwd: cwd, Title: sess.Title}
			if sess.Import != nil && sess.Import.Record != nil {
				if rec.Source.Home == "" {
					rec.Source = sess.Import.Record.Source
					msg.oldHome = sess.Import.Record.Source.Home
				}
				if sess.Import.Session != nil {
					is.GitRemote = sess.Import.Session.GitRemote
				}
			}
			rec.Sessions = append(rec.Sessions, is)
		}
		if len(msg.groups) == 0 {
			msg.err = errors.New("none of the chosen sessions records a cwd; nothing to rehome")
			return msg
		}
		home, _ := os.UserHomeDir()
		for _, sg := range rehome.Suggest(rec, home, nil) {
			if i, ok := index[sg.OldCwd]; ok {
				msg.groups[i].candidates = sg.Candidates
			}
		}
		return msg
	}
}

// rehomeOptions is the rehome.Options the browser plans and applies
// with: the chosen sessions only, memory merged and rewritten, the
// retention policy of the root's config dir, the account the verify
// lines name.
func rehomeOptions(svc *services, root transcripts.Root, ids []string, oldHome string) rehome.Options {
	days, _ := transcripts.CleanupPeriodDays(root.ConfigDir)
	newHome, _ := os.UserHomeDir()
	now := svc.now()
	return rehome.Options{
		Sessions:      ids,
		RewriteMemory: true,
		Mtime:         rehome.MtimePolicy{CleanupPeriodDays: days, Now: now},
		ClaudeJSON:    root.ClaudeJSON,
		OldHome:       oldHome,
		NewHome:       newHome,
		Now:           now,
		CfgDir:        svc.cfgDir,
		StagingDir:    filepath.Join(svc.cfgDir, porter.StagingSubdir),
		Account:       destAccount(svc, root),
		Env:           os.Environ(),
	}
}

// planRehome is the dry run: liveness (a rehome moves transcripts, so
// without it nothing may move) then rehome.PlanRehome.
func planRehome(svc *services, root transcripts.Root, maps []rehome.Mapping, opts rehome.Options) tea.Cmd {
	ctx := svc.ctx
	configDirs := svc.configDirs()
	return func() tea.Msg {
		live, err := transcripts.Live(ctx, configDirs)
		if err != nil {
			return rehomePlannedMsg{err: fmt.Errorf("cannot tell which sessions are open in a running claude: %w; nothing was moved", err)}
		}
		plan, err := rehome.PlanRehome(ctx, root, live, maps, opts)
		return rehomePlannedMsg{plan: plan, opts: opts, err: err}
	}
}

// options are the choices of the current group: its candidates, a typed
// path, and skipping the directory.
func (s *rehomeScreen) options() []string {
	g := s.groups[s.current]
	out := make([]string, 0, len(g.candidates)+2)
	for _, c := range g.candidates {
		out = append(out, fmt.Sprintf("%-44s (%s)", shortPath(c.Dir), transcripts.Sanitize(c.Reason)))
	}
	return append(out, "type a path", "skip this directory")
}

// decide records the answer for the current group and moves on; when
// every group is decided the plan is made.
func (s *rehomeScreen) decide(newCwd string) (Screen, tea.Cmd) {
	g := &s.groups[s.current]
	g.newCwd, g.decided = newCwd, true
	if newCwd != "" {
		s.maps = append(s.maps, rehome.Mapping{Old: g.oldCwd, New: newCwd})
	}
	s.cursor = 0
	if s.current+1 < len(s.groups) {
		s.current++
		s.state = rehomePick
		return s, nil
	}
	if len(s.maps) == 0 {
		return s, tea.Batch(popScreen(), status("nothing to rehome: every directory was skipped"))
	}
	var ids []string
	for _, g := range s.groups {
		if g.newCwd != "" {
			ids = append(ids, g.sessions...)
		}
	}
	s.state = rehomePlanning
	s.opts = rehomeOptions(s.svc, s.tgt.root, ids, s.oldHome)
	return s, planRehome(s.svc, s.tgt.root, s.maps, s.opts)
}

func (s *rehomeScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.input.SetWidth(max(10, msg.Width-8))
		return s, nil

	case rehomeLoadedMsg:
		if s.state != rehomeLoading {
			return s, nil
		}
		if msg.err != nil {
			return s, tea.Batch(popScreen(), statusError(msg.err))
		}
		s.groups, s.oldHome = msg.groups, msg.oldHome
		s.state = rehomePick
		return s, nil

	case rehomePlannedMsg:
		if s.state != rehomePlanning {
			return s, nil
		}
		if msg.err != nil {
			return s, replaceScreen(newResultScreen("rehome", nil, msg.err))
		}
		s.plan, s.opts = msg.plan, msg.opts
		s.planned = rehomePlanLines(msg.plan, s.maps)
		switch {
		case len(msg.plan.Moves) == 0 && len(msg.plan.Refusals) > 0:
			return s, replaceScreen(newResultScreen("rehome", s.planned, fmt.Errorf("nothing could be moved: %s refused", countNoun(len(msg.plan.Refusals), "session"))))
		case len(msg.plan.Moves) == 0 && len(msg.plan.Memory) == 0:
			return s, replaceScreen(newResultScreen("rehome", append(s.planned, "nothing to rehome"), nil))
		}
		s.state = rehomeConfirm
		return s, nil

	case rehomeDoneMsg:
		s.op.finish()
		lines := rehomeResultLines(msg.res)
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) || s.op.cancelled() {
				if len(msg.res.Moved) == 0 {
					return s, tea.Batch(popScreen(), status("rehome cancelled; nothing moved"))
				}
				return s, replaceScreen(newResultScreen("rehome", append([]string{"rehome cancelled; what moved before:"}, lines...), nil))
			}
			return s, replaceScreen(newResultScreen("rehome", lines, msg.err))
		}
		return s, replaceScreen(newResultScreen("rehome", lines, nil))

	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	if s.state == rehomePath {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	return s, nil
}

func (s *rehomeScreen) keyPress(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
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
	case rehomePick:
		opts := s.options()
		switch {
		case key.Matches(msg, keys.Up):
			s.cursor = max(0, s.cursor-1)
		case key.Matches(msg, keys.Down):
			s.cursor = min(len(opts)-1, s.cursor+1)
		case msg.Code == tea.KeyEnter:
			g := s.groups[s.current]
			switch {
			case s.cursor < len(g.candidates):
				dir := g.candidates[s.cursor].Dir
				if n, err := store.NormalizePath(dir); err == nil {
					dir = n
				}
				return s.decide(dir)
			case s.cursor == len(g.candidates):
				s.state = rehomePath
				s.note = ""
				s.input.SetValue("")
			default:
				return s.decide("")
			}
		}
		return s, nil

	case rehomePath:
		switch {
		case key.Matches(msg, keys.Cancel):
			s.state = rehomePick
			return s, nil
		case msg.Code == tea.KeyEnter:
			raw := strings.TrimSpace(s.input.Value())
			dir, err := store.NormalizePath(raw)
			if raw == "" || err != nil || !isDir(dir) {
				s.note = fmt.Sprintf("%q is not a directory here", raw)
				return s, nil
			}
			return s.decide(dir)
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd

	case rehomeConfirm:
		if s.box.key(msg) {
			return s, nil
		}
		switch yesNo(msg) {
		case 1:
			s.state = rehomeRunning
			root, plan, opts := s.tgt.root, s.plan, s.opts
			var cmd tea.Cmd
			s.op, cmd = startOp(s.svc.ctx, func(ctx context.Context, emit func(tea.Msg)) tea.Msg {
				res, err := rehome.Apply(ctx, root, plan, opts)
				return rehomeDoneMsg{res: res, err: err}
			})
			return s, cmd
		case -1:
			return s, tea.Batch(popScreen(), status("rehome aborted; nothing moved"))
		}

	case rehomeRunning:
		switch {
		case key.Matches(msg, keys.Cancel):
			s.op.stop()
		case key.Matches(msg, keys.Quit):
			s.askQuit = true
		}
	}
	return s, nil
}

func (s *rehomeScreen) View(width, height int) string {
	head := styleFaint.Render(truncate(fmt.Sprintf("rehome %s  in %s", countNoun(len(s.chosen), "session"), shortRootLabel(s.tgt.root)), width))
	switch s.state {
	case rehomeLoading:
		return head + "\n\nreading the sessions' directories and looking for candidates…"
	case rehomePick, rehomePath:
		g := s.groups[s.current]
		lines := []string{head, "",
			truncate(fmt.Sprintf("old cwd: %s   (%s; directory %d of %d)", transcripts.Sanitize(g.oldCwd), countNoun(len(g.sessions), "session"), s.current+1, len(s.groups)), width),
			"where does this project live on this machine?",
		}
		for i, o := range s.options() {
			line := pad(fmt.Sprintf("[%d] %s", i+1, o), max(0, width-2))
			if i == s.cursor && s.state == rehomePick {
				line = styleCursor.Render("> " + line)
			} else {
				line = "  " + line
			}
			lines = append(lines, truncate(line, width))
		}
		if s.state == rehomePath {
			lines = append(lines, "", s.input.View())
			if s.note != "" {
				lines = append(lines, styleError.Render(truncate(s.note, width)))
			}
		}
		return strings.Join(lines, "\n")
	case rehomePlanning:
		return head + "\n\nplanning (dry run)…"
	case rehomeConfirm:
		return s.box.view([]string{head, ""}, s.planned, []string{"", fmt.Sprintf("Rehome %s? [y/N]", rehomeCount(s.plan))}, width, height)
	}
	lines := append([]string{head, ""}, joinLines(s.planned, width), "", "applying…  (each session moves transactionally; a cancel rolls the one in flight back)")
	switch {
	case s.askQuit:
		lines = append(lines, "", quitPrompt)
	case s.op.cancelled():
		lines = append(lines, "", cancelling)
	}
	return strings.Join(lines, "\n")
}
