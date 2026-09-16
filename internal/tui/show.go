package tui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// sessionDetail is everything the show screen prints for one
// transcript — the same facts as `bffs sessions show`.
type sessionDetail struct {
	Session          transcripts.Session
	Artifacts        transcripts.Artifacts
	LivePID          int
	SidecarSize      int64
	SidecarExists    bool
	FileHistoryExist bool
	TasksExist       bool
	Claimants        []string // accounts whose .claude.json lastSessionId is this session
}

// loadDetail completes s from its windows when the list had not yet,
// then gathers the artifacts, sizes and pointers.
func loadDetail(svc *services, s transcripts.Session, resolved bool) tea.Cmd {
	configDirs := svc.configDirs()
	attr := svc.attributor
	hist := svc.history[s.Root.ConfigDir]
	imp := svc.imports
	return func() tea.Msg {
		msg := showLoadedMsg{id: s.ID}
		d := sessionDetail{Session: s}
		if !resolved {
			m := readMeta(s.Path, hist)
			if !m.Failed {
				m.apply(&d.Session)
			}
		}
		if attr != nil {
			d.Session.Account, d.Session.AttribSource = attr.Attribute(d.Session.ID, d.Session.Cwd, d.Session.FirstTS, d.Session.Root.Owner)
			d.Claimants = attr.Claimants(d.Session.ID)
		}
		if ref, ok := imp[d.Session.ID]; ok {
			ref := ref
			d.Session.Import = &ref
		}
		if live, err := transcripts.Live(svc.ctx, configDirs); err == nil {
			if ls, ok := live[d.Session.ID]; ok {
				d.Session.Live = true
				d.LivePID = ls.PID
			}
		}
		d.Artifacts = transcripts.ArtifactsFor(d.Session.Root, d.Session)
		d.SidecarSize, d.SidecarExists = dirSize(d.Artifacts.SidecarDir)
		d.FileHistoryExist = isDir(d.Artifacts.FileHistoryDir)
		d.TasksExist = isDir(d.Artifacts.TasksDir)
		msg.detail = d
		return msg
	}
}

// dirSize sums the regular files under dir; ok is false when it does
// not exist.
func dirSize(dir string) (total int64, ok bool) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total, true
}

// detailLines renders the key/value screen of one session, ending with
// the command that resumes it. Every value passes through Sanitize:
// cwd, branch, title, plan slug and the import record are transcript-
// or peer-derived.
func detailLines(d sessionDetail, now time.Time) []string {
	s := d.Session
	var lines []string
	kv := func(label, value string) {
		lines = append(lines, fmt.Sprintf("%-14s%s", label+":", transcripts.Sanitize(value)))
	}
	kv("session", s.ID)
	title := "-"
	if t := transcripts.Sanitize(s.Title); t != "" {
		title = fmt.Sprintf("%s  (%s)", t, s.TitleSource)
	}
	kv("title", title)
	kv("root", fmt.Sprintf("%s  (%s)", shortPath(s.Root.Dir), shortRootLabel(s.Root)))
	kv("project dir", shortPath(filepath.Dir(s.Path)))
	switch {
	case s.Cwd == "":
		kv("cwd", "-  (no cwd recorded)")
	case s.CwdExists:
		kv("cwd", shortPath(s.Cwd)+"  (exists)")
	default:
		kv("cwd", shortPath(s.Cwd)+"  (missing on this machine)")
	}
	if s.Relocated {
		kv("head cwd", shortPath(s.HeadCwd)+"  (relocated since)")
	}
	if s.Account != "" {
		kv("account", fmt.Sprintf("%s  (%s)", transcripts.Sanitize(s.Account), s.AttribSource))
	} else {
		kv("account", "-  (unknown: no launch-log, lastSessionId or import evidence)")
	}
	state := sessionState(s)
	switch {
	case s.Live && d.LivePID > 0:
		state = fmt.Sprintf("live (pid %d)", d.LivePID)
	case state == "":
		state = "idle"
	}
	kv("state", state)
	if len(d.Claimants) > 0 {
		lines = append(lines, "last-session pointer: "+transcripts.Sanitize(strings.Join(d.Claimants, ", "))+" (claude-recorded)")
	}
	if !s.FirstTS.IsZero() {
		kv("first", fmt.Sprintf("%s  (%s)", s.FirstTS.Local().Format("2006-01-02 15:04"), humanizeAgo(s.FirstTS, now)))
	}
	kv("last", fmt.Sprintf("%s  (%s)", s.LastTS.Local().Format("2006-01-02 15:04"), humanizeAgo(s.LastTS, now)))
	kv("size", formatSize(s.Size))
	kv("branch", dashIfEmpty(transcripts.Sanitize(s.GitBranch)))
	kv("version", dashIfEmpty(transcripts.Sanitize(s.Version)))
	kv("transcript", shortPath(d.Artifacts.Transcript))
	sidecar := shortPath(d.Artifacts.SidecarDir) + "  (absent)"
	if d.SidecarExists {
		sidecar = fmt.Sprintf("%s  (%s, %s)", shortPath(d.Artifacts.SidecarDir), formatSize(d.SidecarSize), countNoun(s.Subagents, "subagent"))
	}
	kv("sidecar", sidecar)
	kv("file-history", shortPath(d.Artifacts.FileHistoryDir)+presence(d.FileHistoryExist))
	kv("tasks", shortPath(d.Artifacts.TasksDir)+presence(d.TasksExist))
	if len(d.Artifacts.PlanFiles) == 0 {
		kv("plan", "-")
	}
	for _, p := range d.Artifacts.PlanFiles {
		kv("plan", shortPath(p))
	}
	if s.Import != nil && s.Import.Record != nil {
		r := s.Import.Record
		src := transcripts.Sanitize(r.Source.Hostname)
		if r.Source.User != "" || r.Source.Home != "" {
			src += fmt.Sprintf(" (%s, %s)", transcripts.Sanitize(r.Source.User), transcripts.Sanitize(r.Source.Home))
		}
		acct := r.Account
		if acct == "" {
			acct = transcripts.HomeName
		}
		kind := r.Kind
		if !r.ImportedAt.IsZero() {
			kind += " " + r.ImportedAt.Local().Format("2006-01-02")
		}
		line := fmt.Sprintf("bundle %s  (%s) from %s, account %s", transcripts.Sanitize(r.BundleID), kind, src, transcripts.Sanitize(acct))
		if s.Import.Session != nil {
			line += ", status " + transcripts.Sanitize(s.Import.Session.Status)
		}
		kv("import", line)
		if s.Import.Session != nil && s.Import.Session.OldCwd != "" {
			kv("old cwd", transcripts.Sanitize(s.Import.Session.OldCwd))
		}
	}
	kv("resume", resumeLine(s))
	return lines
}

func presence(exists bool) string {
	if exists {
		return ""
	}
	return "  (absent)"
}

// resumeLine is the copy-pasteable command that reopens s: on a
// full-isolation root prefixed with BFFS_ACCOUNT so the shim picks the
// owning account; on an orphan root with the config dir itself.
func resumeLine(s transcripts.Session) string {
	var sb strings.Builder
	switch {
	case s.Root.Orphan:
		sb.WriteString("CLAUDE_CONFIG_DIR=" + shellWord(s.Root.ConfigDir) + " ")
	case s.Root.Owner != "":
		sb.WriteString("BFFS_ACCOUNT=" + shellWord(s.Root.Owner) + " ")
	}
	if cwd := transcripts.Sanitize(s.Cwd); cwd != "" {
		sb.WriteString("cd " + shellWord(cwd) + " && ")
	}
	sb.WriteString("claude --resume " + s.ID)
	return sb.String()
}

// shellWord quotes s for a POSIX shell when it needs it.
func shellWord(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-./~:@+,", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	if s == "" {
		return "''"
	}
	return s
}

// showScreen is the read-only detail of one session, in a viewport.
type showScreen struct {
	svc      *services
	session  transcripts.Session
	resolved bool
	vp       viewport.Model
	busy     bool
}

func newShowScreen(svc *services, s transcripts.Session, resolved bool) *showScreen {
	return &showScreen{svc: svc, session: s, resolved: resolved, vp: newViewport(), busy: true}
}

func (s *showScreen) Init() tea.Cmd       { return loadDetail(s.svc, s.session, s.resolved) }
func (s *showScreen) Title() string       { return "session " + shortID(s.session.ID) }
func (s *showScreen) loading() bool       { return s.busy }
func (s *showScreen) Keys() []key.Binding { return viewportKeys() }

func (s *showScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(msg.Height)
		return s, nil
	case showLoadedMsg:
		if msg.id != s.session.ID {
			return s, nil
		}
		s.busy = false
		if msg.err != nil {
			return s, statusError(msg.err)
		}
		s.session = msg.detail.Session
		s.vp.SetContentLines(detailLines(msg.detail, s.svc.now()))
		return s, nil
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *showScreen) View(width, height int) string {
	if s.busy {
		return styleFaint.Render("loading…")
	}
	return s.vp.View()
}
