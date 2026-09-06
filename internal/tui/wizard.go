package tui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/porter"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// wizardStep is where the transfer wizard is.
type wizardStep int

const (
	wizStart    wizardStep = iota // send or receive
	wizSendWhat                   // what to send: parts, memory, live sessions
	wizSendHow                    // over the LAN, to a file, through ssh
	wizSendSSH                    // the command to run in a terminal
	wizRecvFrom                   // a machine on the LAN, or a file
	wizRecvFile                   // typing the file's path
)

// wizardScreen walks a transfer between machines step by step: send or
// receive; what to send and which parts to leave out; how — over the
// local network with a pairing code, to a .bffs file, or through ssh —
// or where a received bundle comes from. Every step says what happens
// next and hands off to the screen that does the work (serve, export,
// receive) with the choices made here, so nothing runs before the last
// step.
type wizardScreen struct {
	svc        *services
	tgt        actionTarget
	root       transcripts.Root
	account    string // the perspective an import runs as ("" = the resolver's pick)
	hasProject bool
	step       wizardStep
	cursor     int
	parts      porter.Parts
	memory     bool
	live       bool
	input      textinput.Model
	note       string
	copied     bool
	width      int
}

func newWizardScreen(svc *services, tgt actionTarget, root transcripts.Root, account string, hasProject bool) *wizardScreen {
	in := newInput("file: ", "path of a .bffs file written by bffs export")
	return &wizardScreen{svc: svc, tgt: tgt, root: root, account: account, hasProject: hasProject, parts: porter.DefaultParts, memory: true, live: true, input: in}
}

func (s *wizardScreen) Init() tea.Cmd        { return nil }
func (s *wizardScreen) Title() string        { return "transfer wizard" }
func (s *wizardScreen) capturingInput() bool { return s.step == wizRecvFile }

func (s *wizardScreen) Keys() []key.Binding {
	switch s.step {
	case wizSendWhat:
		return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "toggle")), key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "continue")), key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))}
	case wizSendSSH:
		return []key.Binding{key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy the command")), key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))}
	case wizRecvFile:
		return []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "review the bundle")), key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))}
	}
	return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")), key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))}
}

// wizardOption is one choice of a step: a label, what it means, and
// for the toggles of the "what" step the current state.
type wizardOption struct {
	label string
	desc  string
	on    *bool
}

// options are the choices of the current step.
func (s *wizardScreen) options() []wizardOption {
	switch s.step {
	case wizStart:
		return []wizardOption{
			{label: "Send sessions from this machine", desc: "serve them over the local network with a pairing code, write a .bffs file, or pipe them through ssh"},
			{label: "Receive sessions from another machine", desc: "from a machine running the serve, or from a .bffs file that reached this one"},
		}
	case wizSendWhat:
		opts := []wizardOption{}
		if len(s.tgt.ids) == 0 && s.tgt.project != "" {
			opts = append(opts, wizardOption{label: "memory", desc: "the project's auto-memory directory (MEMORY.md and topic files)", on: &s.memory})
		}
		return append(opts,
			wizardOption{label: "tool results", desc: "saved tool outputs — may contain pasted secrets", on: &s.parts.ToolResults},
			wizardOption{label: "file history", desc: "backups of files Claude edited, what /rewind uses", on: &s.parts.FileHistory},
			wizardOption{label: "prompt history", desc: "the lines claude shows when you press up", on: &s.parts.History},
			wizardOption{label: "live sessions", desc: "sessions open in a running claude — exported read-only, possibly truncated", on: &s.live},
			wizardOption{label: "continue →", desc: "choose how to send"},
		)
	case wizSendHow:
		return []wizardOption{
			{label: "Over the local network", desc: "this machine shows an address and a pairing code; the other one runs bffs import --from <address> (or w → receive) and types the code. Needs an inbound port; macOS asks once whether bffs may accept connections"},
			{label: "To a .bffs file", desc: "written here; carry it however you like and import it on the other machine (w → receive → file, or bffs import --from <file>)"},
			{label: "Through ssh", desc: "no inbound port needed: a one-line command pipes the bundle straight into bffs import on the other machine; shown for you to run in a terminal"},
		}
	case wizRecvFrom:
		return []wizardOption{
			{label: "From another machine on the local network", desc: "it runs bffs export --serve (or w → send there) and shows an address and a pairing code"},
			{label: "From a .bffs file on this machine", desc: "written by bffs export on the other machine"},
		}
	}
	return nil
}

// target is the export target with the choices of the "what" step.
func (s *wizardScreen) target() actionTarget {
	t := s.tgt
	parts := s.parts
	t.parts = &parts
	t.noLive = !s.live
	if len(t.ids) == 0 && t.project != "" && !s.memory {
		t.only = "sessions"
	}
	return t
}

// sshCommand is the pipe the ssh step shows.
func (s *wizardScreen) sshCommand() string {
	t := s.target()
	var b strings.Builder
	b.WriteString("bffs export")
	if len(t.ids) > 0 {
		for _, id := range t.ids {
			b.WriteString(" --session " + shellWord(id))
		}
	} else if t.project != "" {
		b.WriteString(" --project " + shellWord(t.project))
	}
	if t.only == "sessions" {
		b.WriteString(" --only sessions")
	}
	if !t.parts.ToolResults {
		b.WriteString(" --no-tool-results")
	}
	if !t.parts.FileHistory {
		b.WriteString(" --no-file-history")
	}
	if !t.parts.History {
		b.WriteString(" --no-history")
	}
	if t.noLive {
		b.WriteString(" --no-live")
	}
	if t.root.Owner != "" {
		b.WriteString(" --account " + shellWord(t.root.Owner))
	}
	b.WriteString(" --out - | ssh <other-machine> 'bffs import --from - -y --as-is'")
	return b.String()
}

// canSend says whether there is anything to send.
func (s *wizardScreen) canSend() bool {
	return len(s.tgt.ids) > 0 || s.tgt.project != "" || (s.hasProject && len(s.tgt.rows) > 0)
}

func (s *wizardScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.input.SetWidth(max(10, msg.Width-8))
		return s, nil
	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	if s.step == wizRecvFile {
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	return s, nil
}

func (s *wizardScreen) keyPress(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if s.step == wizRecvFile {
		switch {
		case key.Matches(msg, keys.Cancel):
			s.step, s.cursor, s.note = wizRecvFrom, 1, ""
			return s, nil
		case msg.Code == tea.KeyEnter:
			raw := strings.TrimSpace(s.input.Value())
			path, err := validBundlePath(raw)
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			return s, replaceScreen(newReceiveFileScreen(s.svc, s.root, s.account, path))
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return s, cmd
	}
	opts := s.options()
	switch {
	case key.Matches(msg, keys.Cancel):
		return s.back()
	case key.Matches(msg, keys.Up):
		s.cursor = max(0, s.cursor-1)
	case key.Matches(msg, keys.Down):
		s.cursor = min(len(opts)-1, s.cursor+1)
	case msg.Code == tea.KeySpace && s.step == wizSendWhat:
		if o := opts[s.cursor]; o.on != nil {
			*o.on = !*o.on
		}
	case msg.String() == "c" && s.step == wizSendSSH:
		s.copied = true
		return s, tea.SetClipboard(s.sshCommand())
	case msg.Code == tea.KeyEnter:
		return s.choose()
	}
	return s, nil
}

// back is esc: one step up, or close.
func (s *wizardScreen) back() (Screen, tea.Cmd) {
	s.note = ""
	switch s.step {
	case wizStart:
		return s, popScreen()
	case wizSendWhat, wizRecvFrom:
		s.step, s.cursor = wizStart, 0
	case wizSendHow:
		s.step, s.cursor = wizSendWhat, 0
	case wizSendSSH:
		s.step, s.cursor, s.copied = wizSendHow, 2, false
	}
	return s, nil
}

// choose is enter on the current step.
func (s *wizardScreen) choose() (Screen, tea.Cmd) {
	s.note = ""
	switch s.step {
	case wizStart:
		if s.cursor == 0 {
			if !s.canSend() {
				s.note = "nothing to send: select a project (2) or mark sessions (space) first"
				return s, nil
			}
			s.step, s.cursor = wizSendWhat, 0
		} else {
			s.step, s.cursor = wizRecvFrom, 0
		}
	case wizSendWhat:
		opts := s.options()
		if o := opts[s.cursor]; o.on != nil {
			*o.on = !*o.on
			return s, nil
		}
		s.step, s.cursor = wizSendHow, 0
	case wizSendHow:
		switch s.cursor {
		case 0:
			sc, err := newServeScreen(s.svc, s.target())
			if err != nil {
				s.note = err.Error()
				return s, nil
			}
			return s, replaceScreen(sc)
		case 1:
			return s, replaceScreen(newExportScreen(s.svc, s.target()))
		default:
			s.step = wizSendSSH
		}
	case wizSendSSH:
		return s, nil
	case wizRecvFrom:
		if s.root.Orphan {
			s.note = "an orphan session dir is read-only; pick an account (1) to receive into"
			return s, nil
		}
		if s.cursor == 0 {
			return s, replaceScreen(newReceiveScreen(s.svc, s.root, s.account))
		}
		s.step = wizRecvFile
		s.input.SetValue("")
		s.input.Focus()
	}
	return s, nil
}

// validBundlePath resolves what was typed: an existing regular file.
func validBundlePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("type the path of the .bffs file")
	}
	path := raw
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist", shortPath(path))
	}
	if err != nil {
		return "", fmt.Errorf("%s: %v", shortPath(path), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a file", shortPath(path))
	}
	return path, nil
}

func (s *wizardScreen) View(width, height int) string {
	var lines []string
	head := func(step, text string) {
		lines = append(lines, styleFaint.Render(step)+"  "+styleHeader.Render(text), "")
	}
	switch s.step {
	case wizStart:
		head("step 1 of 3", "What do you want to do?")
	case wizSendWhat:
		head("step 2 of 3", "What to send: "+s.tgt.what())
		lines = append(lines, styleFaint.Render(truncate("from "+shortRootLabel(s.root)+" · space toggles a part · nothing is read yet", width)), "")
	case wizSendHow:
		head("step 3 of 3", "How to send it")
	case wizSendSSH:
		head("send through ssh", "Run this in a terminal on this machine")
		lines = append(lines, "", "  "+s.sshCommand(), "",
			styleFaint.Render("  replace <other-machine> with its ssh host; -y --as-is imports without questions and you rehome there (r) afterwards"))
		if s.copied {
			lines = append(lines, "", styleOK.Render("  copied to the clipboard (OSC 52)"))
		} else {
			lines = append(lines, "", styleFaint.Render("  c copies it to the clipboard · esc goes back"))
		}
		return joinLines(lines, width)
	case wizRecvFrom:
		acct := s.account
		if acct == "" {
			acct = destAccount(s.svc, s.root)
		}
		if acct == "" {
			acct = transcripts.HomeName
		}
		head("step 2 of 2", "Where from?")
		lines = append(lines, styleFaint.Render(truncate(fmt.Sprintf("into %s as account %q — 1 accounts changes that", shortRootLabel(s.root), acct), width)), "")
	case wizRecvFile:
		head("receive from a file", "Which file?")
		lines = append(lines, s.input.View())
		if s.note != "" {
			lines = append(lines, styleError.Render(truncate(s.note, width)))
		} else {
			lines = append(lines, styleFaint.Render("the manifest is shown and confirmed before anything is written"))
		}
		return strings.Join(lines, "\n")
	}
	for i, o := range s.options() {
		label := o.label
		if o.on != nil {
			state := "include"
			if !*o.on {
				state = "leave out"
			}
			label = pad(o.label, 16) + state
		}
		line := pad(label, max(0, width-2))
		if i == s.cursor {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line, styleFaint.Render(truncate("      "+o.desc, width)))
	}
	if s.note != "" {
		lines = append(lines, "", styleError.Render(truncate(s.note, width)))
	}
	return strings.Join(lines, "\n")
}
