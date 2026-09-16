package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/accounts"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// Adding an account is the one thing the browser used to send people to
// the CLI for, and it is the first thing a new user needs. Both kinds
// are here: an api_key account is a name and a secret, and an oauth
// account is a name, a session directory and `claude auth login` — a
// browser flow that needs the terminal, so it is handed over the way a
// resume is (tea.ExecProcess) and the account is recorded when it
// returns. The rules are internal/accounts', the same ones `bffs add`
// and `bffs login` follow.

// loginDoneMsg ends the `claude auth login` the browser handed over.
type loginDoneMsg struct {
	prep accounts.OAuthPrep
	err  error
}

// newAccountState is where the screen is.
type newAccountState int

const (
	naKind    newAccountState = iota // subscription or api key
	naName                           // the account name
	naSecret                         // the api key, echoed as nothing
	naRunning                        // claude auth login has the terminal
)

type newAccountScreen struct {
	svc   *services
	state newAccountState
	kind  int // 0 oauth, 1 api key
	name  textinput.Model
	key   textinput.Model
	note  string
}

func newNewAccountScreen(svc *services) *newAccountScreen {
	name := newInput("name: ", "letters, digits, - or _")
	secret := newInput("key:  ", "sk-ant-"+glyph.ellipsis)
	secret.EchoMode = textinput.EchoNone
	return &newAccountScreen{svc: svc, name: name, key: secret}
}

func (s *newAccountScreen) Init() tea.Cmd { return nil }
func (s *newAccountScreen) Title() string { return "new account" }
func (s *newAccountScreen) running() bool { return s.state == naRunning }
func (s *newAccountScreen) capturingInput() bool {
	return s.state == naName || s.state == naSecret
}

func (s *newAccountScreen) Keys() []key.Binding {
	switch s.state {
	case naKind:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")), keys.Cancel}
	case naName:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "next")), keys.Cancel}
	case naSecret:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "save the account")), keys.Cancel}
	}
	return []key.Binding{keys.Cancel}
}

func (s *newAccountScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case loginDoneMsg:
		return s.finish(msg)
	case tea.KeyPressMsg:
		return s.key2(msg)
	}
	return s, nil
}

func (s *newAccountScreen) key2(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if key.Matches(msg, keys.Cancel) && s.state != naRunning {
		return s, popScreen()
	}
	switch s.state {
	case naKind:
		switch {
		case key.Matches(msg, keys.Up):
			s.kind = 0
		case key.Matches(msg, keys.Down):
			s.kind = 1
		case msg.Code == tea.KeyEnter:
			s.state, s.note = naName, ""
			s.name.Focus()
		}
		return s, nil

	case naName:
		if msg.Code == tea.KeyEnter {
			name := strings.TrimSpace(s.name.Value())
			if err := accounts.ValidateName(name); err != nil {
				s.note = err.Error()
				return s, nil
			}
			if t, taken := accounts.Taken(s.svc.accs, name); taken {
				s.note = fmt.Sprintf("account %q already exists (type=%s); pick another name", name, t)
				return s, nil
			}
			s.note = ""
			if s.kind == 1 {
				s.state = naSecret
				s.name.Blur()
				s.key.Focus()
				return s, nil
			}
			return s.startLogin(name)
		}
		var cmd tea.Cmd
		s.name, cmd = s.name.Update(msg)
		return s, cmd

	case naSecret:
		if msg.Code == tea.KeyEnter {
			name, secret := strings.TrimSpace(s.name.Value()), s.key.Value()
			if err := accounts.AddAPIKey(s.svc.cfgDir, name, secret, "", false); err != nil {
				s.note = transcripts.Sanitize(err.Error())
				return s, nil
			}
			return s, tea.Batch(popRefresh(), statusDone(fmt.Sprintf("added account %q (api key)"+sepDot+"claude uses it from its next launch", transcripts.Sanitize(name))))
		}
		var cmd tea.Cmd
		s.key, cmd = s.key.Update(msg)
		return s, cmd
	}
	return s, nil
}

// startLogin prepares the session directory and hands the terminal to
// `claude auth login`. Nothing is written to accounts.toml until it
// comes back: an abandoned login leaves a session dir and no account.
func (s *newAccountScreen) startLogin(name string) (Screen, tea.Cmd) {
	prep, err := accounts.PrepareOAuth(s.svc.cfgDir, name, "", false, false, "")
	if err != nil {
		s.note = transcripts.Sanitize(err.Error())
		return s, nil
	}
	s.state = naRunning
	c := exec.CommandContext(s.svc.ctx, prep.Bin, prep.Args...)
	c.Env = prep.Env
	return s, tea.ExecProcess(c, func(err error) tea.Msg { return loginDoneMsg{prep: prep, err: err} })
}

// finish records the account the login produced, or says why there is
// none. The browser never makes the new account active by itself —
// `bffs login` does, but there the person asked for exactly that,
// while here they may be adding a second account to browse.
func (s *newAccountScreen) finish(msg loginDoneMsg) (Screen, tea.Cmd) {
	if msg.err != nil {
		s.state = naKind
		s.note = "claude auth login: " + transcripts.Sanitize(msg.err.Error())
		return s, nil
	}
	acc, err := accounts.CompleteOAuth(s.svc.cfgDir, msg.prep, "", false)
	if err != nil {
		s.state = naKind
		s.note = transcripts.Sanitize(err.Error())
		return s, nil
	}
	who := msg.prep.Name
	if acc.Email != "" {
		who += " (" + transcripts.Sanitize(acc.Email) + ")"
	}
	return s, tea.Batch(popRefresh(), statusDone("added account "+who+sepDot+"space makes it active"))
}

func (s *newAccountScreen) View(width, height int) string {
	lines := []string{styleFaint.Render("a new account for this machine" + sepDot + "esc goes back"), ""}
	switch s.state {
	case naKind:
		for i, o := range []struct{ label, detail string }{
			{"Claude subscription (oauth)", "opens `claude auth login` in this terminal; the credential stays in Claude's own store"},
			{"API key (sk-ant-" + glyph.ellipsis + ")", "stored in accounts.toml at 0600 on this machine"},
		} {
			mark := "  "
			if i == s.kind {
				mark = "> "
			}
			lines = append(lines, mark+o.label, "     "+styleFaint.Render(o.detail))
		}
		lines = append(lines, "", styleFaint.Render(glyph.up+"/"+glyph.down+" chooses"+sepDot+"enter continues"))
	case naName:
		lines = append(lines, s.name.View(), "", styleFaint.Render("the name becomes a directory under sessions/, so letters, digits, - and _ only"))
	case naSecret:
		lines = append(lines, styleFaint.Render("account "+transcripts.Sanitize(strings.TrimSpace(s.name.Value()))), "", s.key.View(), "",
			styleFaint.Render("the key is not echoed; it is written to accounts.toml at 0600"))
	case naRunning:
		lines = append(lines, "waiting for `claude auth login`"+glyph.ellipsis)
	}
	if s.note != "" {
		lines = append(lines, "", styleError.Render(s.note))
	}
	return joinLines(lines, width)
}
