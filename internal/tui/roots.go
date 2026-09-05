package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// rootRow is one projects/ pool.
type rootRow struct {
	root  transcripts.Root
	label string
}

func (r *rootRow) FilterValue() string { return r.label }
func (r *rootRow) render(width int) string {
	return truncate(r.label, width)
}

// rootLabel names a root for the roots screen: the shared pool with the
// accounts attached to it, a full-isolation account's own tree, an
// orphan session dir, or the unmanaged home dir. Names are sanitised: an
// orphan's is a directory name read from disk.
func rootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return fmt.Sprintf("orphan: %s (read-only)", transcripts.Sanitize(r.Owner))
	case r.Owner != "":
		return transcripts.Sanitize(r.Owner) + " [full isolation]"
	case r.Shared && len(r.Accounts) > 0:
		return fmt.Sprintf("shared pool (%s) — accounts: %s", shortPath(r.ConfigDir), accountList(r.Accounts))
	default:
		return fmt.Sprintf("home (%s)", shortPath(r.ConfigDir))
	}
}

// shortRootLabel is the compact form the other screens' headers use.
func shortRootLabel(r transcripts.Root) string {
	switch {
	case r.Orphan:
		return "orphan: " + transcripts.Sanitize(r.Owner) + ", read-only"
	case r.Owner != "":
		return "account: " + transcripts.Sanitize(r.Owner)
	case r.Shared && len(r.Accounts) > 0:
		return "shared pool: " + accountList(r.Accounts)
	default:
		return "home: " + shortPath(r.ConfigDir)
	}
}

// accountList joins account names for display.
func accountList(names []string) string {
	return transcripts.Sanitize(strings.Join(names, ", "))
}

// rootsScreen lists every pool; the app skips it when there is one.
type rootsScreen struct {
	svc  *services
	list list.Model
}

func newRootsScreen(svc *services, roots []transcripts.Root) *rootsScreen {
	items := make([]list.Item, 0, len(roots))
	for _, r := range roots {
		items = append(items, &rootRow{root: r, label: rootLabel(r)})
	}
	s := &rootsScreen{svc: svc, list: newList(items, "root", "roots")}
	s.list.Title = "ROOT"
	return s
}

func (s *rootsScreen) Init() tea.Cmd        { return nil }
func (s *rootsScreen) Title() string        { return "roots" }
func (s *rootsScreen) capturingInput() bool { return s.list.SettingFilter() }
func (s *rootsScreen) Keys() []key.Binding {
	return append([]key.Binding{keys.Up, keys.Down, keys.Open, keys.Filter, keys.Receive}, filterKeys(s.list)...)
}

func (s *rootsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.list.SetSize(msg.Width, msg.Height)
		return s, nil
	case tea.KeyPressMsg:
		if s.list.SettingFilter() {
			break
		}
		switch {
		case key.Matches(msg, keys.Open):
			if r, ok := s.list.SelectedItem().(*rootRow); ok {
				return s, pushScreen(newProjectsScreen(s.svc, r.root))
			}
			return s, nil
		case key.Matches(msg, keys.Receive):
			if r, ok := s.list.SelectedItem().(*rootRow); ok {
				return s, receiveInto(s.svc, r.root)
			}
			return s, nil
		}
	}
	var cmd tea.Cmd
	s.list, cmd = s.list.Update(msg)
	return s, cmd
}

func (s *rootsScreen) View(width, height int) string {
	return s.list.View()
}
