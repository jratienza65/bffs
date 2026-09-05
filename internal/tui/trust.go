package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// trustLoadedMsg carries the matrix of one project.
type trustLoadedMsg struct {
	project  string
	key      string
	files    map[string]string
	statuses []trust.Status
	err      error
}

// trustAppliedMsg ends one sync.
type trustAppliedMsg struct {
	row, source string
	n           int
	err         error
}

// trustState is where the Trust screen is.
type trustState int

const (
	trustMatrix  trustState = iota // the per-account table
	trustConfirm                   // the planned changes for one row + [y/N]
	trustBusy                      // loading or applying
)

// trustScreen is `bffs trust` for the project of the screen that
// pushed it: the per-account matrix (trust.Files/Report), and on a row
// the plan trust.Plan makes from trust.BestSource with [y/N] to apply it.
type trustScreen struct {
	svc      *services
	project  string
	key      string
	files    map[string]string
	statuses []trust.Status
	active   string // the row the active account reads
	cursor   int
	state    trustState
	changes  []trust.Change
	source   string
	err      error
	loaded   bool
}

func newTrustScreen(svc *services, project string) *trustScreen {
	return &trustScreen{svc: svc, project: project, state: trustBusy}
}

// loadTrust reads the matrix: the addressable .claude.json files, the
// project key Claude files the directory under, and every file's answers.
func loadTrust(svc *services, project string) tea.Cmd {
	return func() tea.Msg {
		msg := trustLoadedMsg{project: project}
		files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs)
		if err != nil {
			msg.err = err
			return msg
		}
		key, err := transcripts.ProjectKey(project)
		if err != nil {
			msg.err = err
			return msg
		}
		gitRoot := ""
		if r, ok := transcripts.GitRoot(key); ok {
			gitRoot = r
		}
		statuses, err := trust.Report(files, key, gitRoot)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.key, msg.files, msg.statuses = key, files, statuses
		return msg
	}
}

// trustRowFor maps an account name to the row it reads: its own entry
// for an oauth account (or "home" itself), "home" for an api_key account,
// "" for anything else.
func trustRowFor(files map[string]string, accs store.Accounts, name string) string {
	if _, ok := files[name]; ok {
		return name
	}
	if acc, ok := accs.Get(name); ok && acc.Type == store.TypeAPIKey {
		return trust.HomeName
	}
	return ""
}

func (s *trustScreen) Init() tea.Cmd { return loadTrust(s.svc, s.project) }
func (s *trustScreen) Title() string { return "trust" }
func (s *trustScreen) loading() bool { return s.state == trustBusy }

func (s *trustScreen) Keys() []key.Binding {
	if s.state == trustConfirm {
		return []key.Binding{keys.Yes, keys.No}
	}
	return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter", "y"), key.WithHelp("enter/y", "carry answers to this account"))}
}

func (s *trustScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return s, nil

	case trustLoadedMsg:
		if msg.project != s.project {
			return s, nil
		}
		s.state = trustMatrix
		s.loaded = true
		if msg.err != nil {
			s.err = msg.err
			return s, statusError(msg.err)
		}
		s.key, s.files, s.statuses = msg.key, msg.files, msg.statuses
		s.active = trustRowFor(msg.files, s.svc.accs, s.svc.state.Active)
		s.cursor = min(s.cursor, max(0, len(s.statuses)-1))
		return s, nil

	case trustAppliedMsg:
		s.state = trustBusy
		if msg.err != nil {
			return s, tea.Batch(loadTrust(s.svc, s.project), statusError(msg.err))
		}
		who := "a running claude on that account"
		if msg.row == trust.HomeName {
			who = "a running unmanaged claude"
		}
		return s, tea.Batch(loadTrust(s.svc, s.project),
			status(fmt.Sprintf("updated %s from %s (%s); %s picks the change up within about a second", msg.row, msg.source, countNoun(msg.n, "field"), who)))

	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	return s, nil
}

func (s *trustScreen) keyPress(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch s.state {
	case trustMatrix:
		switch {
		case key.Matches(msg, keys.Up):
			s.cursor = max(0, s.cursor-1)
		case key.Matches(msg, keys.Down):
			s.cursor = min(len(s.statuses)-1, s.cursor+1)
		case msg.Code == tea.KeyEnter || msg.String() == "y":
			return s.plan()
		}
	case trustConfirm:
		switch yesNo(msg) {
		case 1:
			return s.apply()
		case -1:
			s.state = trustMatrix
			s.changes = nil
		}
	}
	return s, nil
}

// plan computes what the cursor's row would receive from the best
// source (the active row first, then accounts by name, then home).
func (s *trustScreen) plan() (Screen, tea.Cmd) {
	if len(s.statuses) == 0 {
		return s, nil
	}
	row := s.statuses[s.cursor].Account
	src, ok := trust.BestSource(s.statuses, s.active)
	if !ok {
		return s, status(fmt.Sprintf("no account has answered the folder-trust dialog for %s; nothing to copy", shortPath(s.key)))
	}
	if src == row {
		return s, status(fmt.Sprintf("%s is the source of the answers; pick another row", trustRowLabel(row)))
	}
	changes, err := trust.Plan(s.files[src], s.files[row], trust.Options{ProjectKeys: []string{s.key}})
	if err != nil {
		return s, statusError(err)
	}
	if len(changes) == 0 {
		return s, status(fmt.Sprintf("no changes for %s: it has every answer %s has", trustRowLabel(row), trustRowLabel(src)))
	}
	s.changes, s.source = changes, src
	s.state = trustConfirm
	return s, nil
}

// apply writes the planned changes under Claude's lock (trust.Apply).
func (s *trustScreen) apply() (Screen, tea.Cmd) {
	row := s.statuses[s.cursor].Account
	path, changes, src := s.files[row], s.changes, s.source
	s.state = trustBusy
	s.changes = nil
	return s, func() tea.Msg {
		err := trust.Apply(path, changes)
		return trustAppliedMsg{row: row, source: src, n: len(changes), err: err}
	}
}

func trustRowLabel(name string) string {
	if name == trust.HomeName {
		return "(" + trust.HomeName + ")"
	}
	return transcripts.Sanitize(name)
}

func folderCell(st trust.Status) string {
	switch st.Folder {
	case trust.Accepted:
		return "accepted"
	case trust.Inherited:
		return "inherited (from " + shortPath(st.InheritedFrom) + ")"
	}
	return "-"
}

func externalCell(a trust.Answer) string {
	switch a {
	case trust.Accepted:
		return "allowed"
	case trust.Declined:
		return "declined"
	}
	return "-"
}

func countCell(n int, suffix string) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// rawCell renders a JSON value compactly for a change line.
func rawCell(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "(absent)"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return transcripts.Sanitize(string(raw))
	}
	return truncate(transcripts.Sanitize(buf.String()), 40)
}

// effect says what a change means for the dialogs.
func effect(changes []trust.Change) string {
	folder, external := false, false
	for _, c := range changes {
		switch c.Key {
		case "hasTrustDialogAccepted":
			folder = true
		case "hasClaudeMdExternalIncludesApproved", "hasClaudeMdExternalIncludesWarningShown":
			external = true
		}
	}
	var parts []string
	if folder {
		parts = append(parts, "the folder-trust dialog")
	}
	if external {
		parts = append(parts, "the external CLAUDE.md imports dialog")
	}
	if len(parts) == 0 {
		return ""
	}
	return "claude on that account will no longer ask " + strings.Join(parts, " or ") + " for this project"
}

func (s *trustScreen) View(width, height int) string {
	if !s.loaded {
		return styleFaint.Render("loading…")
	}
	if s.err != nil {
		return styleError.Render(truncate(transcripts.Sanitize(s.err.Error()), width))
	}
	lines := []string{
		truncate(fmt.Sprintf("project:  %s        (key: %s)", shortPath(s.key), transcripts.Sanitize(s.key)), width),
		"",
		"  " + pad("ACCOUNT", 18) + " " + pad("FOLDER-TRUST", 30) + " " + pad("EXTERNAL-IMPORTS", 17) + " " + pad("TOOLS", 6) + " MCP",
	}
	for i, st := range s.statuses {
		marker := " "
		if st.Account == s.active {
			marker = "*"
		}
		line := marker + " " + pad(trustRowLabel(st.Account), 18) + " " + pad(folderCell(st), 30) + " " + pad(externalCell(st.External), 17) + " " + pad(countCell(st.Tools, ""), 6) + " " + countCell(st.MCPEnabled, " enabled")
		line = pad(line, max(0, width-2))
		if i == s.cursor && s.state != trustConfirm {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", styleFaint.Render(truncate(`"-" = never answered on that account (claude will ask); "inherited" = a parent directory is trusted (claude will not ask); * = the active account`, width)))
	if s.state == trustConfirm && len(s.statuses) > 0 {
		row := s.statuses[s.cursor].Account
		lines = append(lines, "", fmt.Sprintf("project %s → %s (from %s):", shortPath(s.key), trustRowLabel(row), trustRowLabel(s.source)))
		for _, c := range s.changes {
			lines = append(lines, fmt.Sprintf("  %-41s %s -> %s", c.Key, rawCell(c.From), rawCell(c.To)))
		}
		if e := effect(s.changes); e != "" {
			lines = append(lines, "  "+e)
		}
		lines = append(lines, fmt.Sprintf("apply to %s? [y/N]", shortPath(s.files[row])))
	} else if s.state == trustMatrix {
		lines = append(lines, styleFaint.Render(truncate("enter or y on a row carries the answers of the best source to that account", width)))
	}
	return strings.Join(lines, "\n")
}
