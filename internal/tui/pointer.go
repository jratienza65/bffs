package tui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// Claude's proper-lockfile parameters for .claude.json, as rehome and
// trust use them.
const (
	claudeJSONLockStale = 10 * time.Second
	claudeJSONLockWait  = 3 * time.Second
)

// pointerRow is one .claude.json the pointer can be set in.
type pointerRow struct {
	name    string // account name, or "home"
	file    string
	current string // its lastSessionId for the key, "" when none
}

// pointerScreen sets an account's last-session pointer for the
// session's project (tui-v2 §1: the per-account fact `claude --continue`
// follows) — `bffs rehome --set-last-session` for one account, under
// Claude's lock.
type pointerScreen struct {
	svc     *services
	session transcripts.Session
	key     string
	rows    []pointerRow
	cursor  int
	confirm bool
	busy    bool
}

// readFlags is claudejson.ReadProjectFlags reduced to the pointer map.
func readFlags(file string) (map[string]string, error) {
	flags, err := claudejson.ReadProjectFlags(file)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(flags))
	for k, f := range flags {
		out[k] = f.LastSessionID
	}
	return out, nil
}

func newPointerScreen(svc *services, s transcripts.Session) (*pointerScreen, error) {
	if s.Cwd == "" {
		return nil, errors.New("the session records no cwd; no project key to point at it")
	}
	if s.Root.Orphan {
		return nil, errors.New("orphan session dir: no account resumes from it")
	}
	key, err := transcripts.ProjectKey(s.Cwd)
	if err != nil {
		return nil, err
	}
	files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if name != transcripts.HomeName {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if _, ok := files[transcripts.HomeName]; ok {
		names = append(names, transcripts.HomeName)
	}
	sc := &pointerScreen{svc: svc, session: s, key: key}
	for _, name := range names {
		r := pointerRow{name: name, file: files[name]}
		if flags, err := readFlags(r.file); err == nil {
			r.current = flags[key]
		}
		sc.rows = append(sc.rows, r)
	}
	return sc, nil
}

func (s *pointerScreen) Init() tea.Cmd { return nil }
func (s *pointerScreen) Title() string { return "last-session pointer" }
func (s *pointerScreen) Keys() []key.Binding {
	if s.confirm {
		return []key.Binding{keys.Yes, keys.No}
	}
	return []key.Binding{keys.Up, keys.Down, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")), keys.Cancel}
}
func (s *pointerScreen) running() bool { return s.busy }

// setPointer writes the pointer under Claude's lock.
func setPointer(file, key, sid, account string) tea.Cmd {
	return func() tea.Msg {
		release, err := fsutil.Lock(file+".lock", claudeJSONLockStale, claudeJSONLockWait)
		if err != nil {
			if errors.Is(err, fsutil.ErrLocked) {
				err = fmt.Errorf("%s is being written by a running claude; retry in a moment", shortPath(file))
			}
			return pointerDoneMsg{account: account, err: err}
		}
		defer release()
		return pointerDoneMsg{account: account, err: claudejson.SetLastSessionID(file, key, sid)}
	}
}

func (s *pointerScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case pointerDoneMsg:
		s.busy = false
		if msg.err != nil {
			return s, statusError(msg.err)
		}
		return s, tea.Batch(popRefresh(), status(fmt.Sprintf("%s: lastSessionId for %s now points at %s — claude --continue there reopens it", transcripts.Sanitize(msg.account), shortPath(s.session.Cwd), shortID(s.session.ID))))
	case tea.KeyPressMsg:
		if s.busy {
			return s, nil
		}
		if s.confirm {
			switch yesNo(msg) {
			case 1:
				s.busy = true
				r := s.rows[s.cursor]
				return s, setPointer(r.file, s.key, s.session.ID, r.name)
			case -1:
				s.confirm = false
			}
			return s, nil
		}
		switch {
		case key.Matches(msg, keys.Up):
			s.cursor = max(0, s.cursor-1)
		case key.Matches(msg, keys.Down):
			s.cursor = min(len(s.rows)-1, s.cursor+1)
		case key.Matches(msg, keys.Open):
			if len(s.rows) == 0 {
				return s, nil
			}
			if s.rows[s.cursor].current == s.session.ID {
				return s, status(transcripts.Sanitize(s.rows[s.cursor].name) + " already points at this session")
			}
			s.confirm = true
		case key.Matches(msg, keys.Cancel):
			return s, popScreen()
		}
	}
	return s, nil
}

func (s *pointerScreen) View(width, height int) string {
	lines := []string{styleFaint.Render(truncate(fmt.Sprintf("point an account's last session for %s at %s", shortPath(s.session.Cwd), shortID(s.session.ID)), width)), "",
		"  " + pad("ACCOUNT", 16) + " " + pad("CURRENT POINTER", 26) + " FILE"}
	for i, r := range s.rows {
		cur := "-"
		if r.current != "" {
			cur = shortID(transcripts.Sanitize(r.current))
			if r.current == s.session.ID {
				cur += " (this session)"
			}
		}
		line := pad(pad(transcripts.Sanitize(r.name), 16)+" "+pad(cur, 26)+" "+shortPath(r.file), max(0, width-2))
		if i == s.cursor {
			line = styleCursor.Render("> " + line)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	switch {
	case s.busy:
		lines = append(lines, "", "writing under Claude's lock…")
	case s.confirm:
		r := s.rows[s.cursor]
		lines = append(lines, "", fmt.Sprintf("point %s's lastSessionId for %s at %s? [y/N]", transcripts.Sanitize(r.name), shortPath(s.session.Cwd), shortID(s.session.ID)),
			styleFaint.Render("only the pointer changes; trust answers and everything else in the file stay"))
	default:
		lines = append(lines, "", styleFaint.Render("enter chooses the account · esc goes back"))
	}
	return strings.Join(lines, "\n")
}
