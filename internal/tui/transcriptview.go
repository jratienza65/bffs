package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// transcriptReadCap bounds what the full viewer reads of a transcript;
// beyond it the rendering ends with a note. Lines are read with a
// bufio.Reader — a single record can run to megabytes.
const transcriptReadCap = 8 << 20

// lineRuneCap bounds one rendered line (a pasted file inside a prompt).
const lineRuneCap = 2000

// transcriptRecord is the lenient shape of one JSONL record: only the
// fields the viewer renders.
type transcriptRecord struct {
	Type         string          `json:"type"`
	Timestamp    string          `json:"timestamp"`
	Message      json.RawMessage `json:"message"`
	Summary      string          `json:"summary"`
	CustomTitle  string          `json:"customTitle"`
	AITitle      string          `json:"aiTitle"`
	RelocatedCwd string          `json:"relocatedCwd"`
	IsMeta       bool            `json:"isMeta"`
	IsSidechain  bool            `json:"isSidechain"`
}

type transcriptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // a string or content blocks
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// renderTranscript reads at most cap bytes of r and renders every
// record it can parse: user prompts as "> …", assistant text as "< …",
// tool calls and results summarised, summaries and titles as notes.
// Meta and sidechain records, and kinds the viewer does not show, are
// counted as hidden. A line that fails to parse is shown as such.
func renderTranscript(r io.Reader, capBytes int64) (lines []string, records, hidden int, truncated bool, err error) {
	br := bufio.NewReaderSize(io.LimitReader(r, capBytes+1), 256*1024)
	var total int64
	for {
		line, rerr := br.ReadBytes('\n')
		total += int64(len(line))
		if total > capBytes {
			truncated = true
			break
		}
		if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
			records++
			out, shown := renderRecord(trimmed)
			if shown {
				if len(lines) > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, out...)
			} else {
				hidden++
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = rerr
			}
			break
		}
	}
	return lines, records, hidden, truncated, err
}

// renderRecord renders one record; shown is false for kinds the viewer
// hides.
func renderRecord(line string) (out []string, shown bool) {
	var rec transcriptRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return []string{styleFaint.Render("? unparseable record: " + clip(transcripts.Sanitize(line), 120))}, true
	}
	if rec.IsMeta || rec.IsSidechain {
		return nil, false
	}
	stamp := ""
	if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
		stamp = t.Local().Format("15:04") + " "
	}
	switch rec.Type {
	case "user", "assistant":
		var m transcriptMessage
		if err := json.Unmarshal(rec.Message, &m); err != nil {
			return nil, false
		}
		role := m.Role
		if role == "" {
			role = rec.Type
		}
		return renderMessage(stamp, role, m.Content), true
	case "summary":
		return []string{styleFaint.Render(stamp + "— summary: " + clip(transcripts.Sanitize(rec.Summary), lineRuneCap))}, true
	case "custom-title":
		return []string{styleFaint.Render(stamp + "— title: " + clip(transcripts.Sanitize(rec.CustomTitle), lineRuneCap))}, true
	case "ai-title":
		return []string{styleFaint.Render(stamp + "— title (ai): " + clip(transcripts.Sanitize(rec.AITitle), lineRuneCap))}, true
	case "relocated":
		return []string{styleFaint.Render(stamp + "— relocated to " + clip(transcripts.Sanitize(rec.RelocatedCwd), lineRuneCap))}, true
	}
	return nil, false
}

// renderMessage renders a user or assistant message: a plain string or
// content blocks (text, tool_use, tool_result, thinking, image).
func renderMessage(stamp, role string, content json.RawMessage) []string {
	prefix := "> "
	if role == "assistant" {
		prefix = "< "
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		lines := textLines(stamp+prefix, text)
		if role != "assistant" && len(lines) > 0 {
			lines[0] = stylePrompt.Render(lines[0])
		}
		return lines
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return []string{styleFaint.Render(stamp + prefix + "(unreadable content)")}
	}
	var out []string
	first := stamp + prefix
	cont := strings.Repeat(" ", len([]rune(stamp))) + "  "
	for _, b := range blocks {
		switch b.Type {
		case "text":
			out = append(out, textLinesWith(first, cont, b.Text)...)
		case "tool_use":
			out = append(out, styleFaint.Render(cont+"⚙ "+transcripts.Sanitize(b.Name)+" "+clip(transcripts.Sanitize(toolInputSummary(b.Input)), 160)))
		case "tool_result":
			label := "⇠ result"
			if b.IsError {
				label = "⇠ error"
			}
			out = append(out, styleFaint.Render(cont+label+": "+clip(transcripts.Sanitize(toolResultSummary(b.Content)), 160)))
		case "thinking":
			out = append(out, styleFaint.Render(cont+"(thinking)"))
		case "image":
			out = append(out, styleFaint.Render(cont+"(image)"))
		default:
			if b.Type != "" {
				out = append(out, styleFaint.Render(cont+"("+transcripts.Sanitize(b.Type)+")"))
			}
		}
		first = cont
	}
	if len(out) == 0 {
		out = append(out, styleFaint.Render(stamp+prefix+"(empty)"))
	}
	return out
}

// textLines renders text with prefix on its first line and matching
// indentation after it.
func textLines(prefix, text string) []string {
	return textLinesWith(prefix, strings.Repeat(" ", len([]rune(prefix))), text)
}

func textLinesWith(first, cont, text string) []string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return []string{first + "(empty)"}
	}
	var out []string
	for i, l := range strings.Split(text, "\n") {
		p := cont
		if i == 0 {
			p = first
		}
		out = append(out, p+clip(transcripts.Sanitize(strings.TrimRight(l, "\r")), lineRuneCap))
	}
	return out
}

// toolInputSummary picks the field that names what a tool call did
// (a command, a path, a pattern, a description), else the compact JSON.
func toolInputSummary(input json.RawMessage) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err == nil {
		for _, k := range []string{"command", "file_path", "path", "pattern", "query", "description", "prompt", "url"} {
			var s string
			if raw, ok := m[k]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
				return k + "=" + firstLine(s)
			}
		}
	}
	return firstLine(string(input))
}

// toolResultSummary is the first line of a tool result and its size.
func toolResultSummary(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err != nil {
		var blocks []contentBlock
		if err := json.Unmarshal(content, &blocks); err == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" {
					sb.WriteString(b.Text)
					sb.WriteString("\n")
				}
			}
			s = sb.String()
		} else {
			s = string(content)
		}
	}
	s = strings.TrimSpace(s)
	n := strings.Count(s, "\n") + 1
	if s == "" {
		return "(empty)"
	}
	if n > 1 {
		return fmt.Sprintf("%s (%s)", firstLine(s), countNoun(n, "line"))
	}
	return firstLine(s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// clip cuts s to n runes with an ellipsis.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// transcriptScreen is the full conversation of one session in a
// viewport, opened with enter from the sessions panel (tui-v2 §2).
type transcriptScreen struct {
	svc       *services
	session   transcripts.Session
	vp        viewport.Model
	busy      bool
	records   int
	hidden    int
	truncated bool
	err       error
}

func newTranscriptScreen(svc *services, s transcripts.Session) *transcriptScreen {
	return &transcriptScreen{svc: svc, session: s, vp: newViewport(), busy: true}
}

func loadTranscript(path string) tea.Cmd {
	return func() tea.Msg {
		msg := transcriptLoadedMsg{path: path}
		f, err := os.Open(path)
		if err != nil {
			msg.err = err
			return msg
		}
		defer f.Close()
		msg.lines, msg.records, msg.hidden, msg.truncated, msg.err = renderTranscript(f, transcriptReadCap)
		return msg
	}
}

func (s *transcriptScreen) Init() tea.Cmd       { return loadTranscript(s.session.Path) }
func (s *transcriptScreen) Title() string       { return "transcript " + shortID(s.session.ID) }
func (s *transcriptScreen) loading() bool       { return s.busy }
func (s *transcriptScreen) Keys() []key.Binding { return append(viewportKeys(), keys.Done) }

func (s *transcriptScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.vp.SetWidth(msg.Width)
		s.vp.SetHeight(max(0, msg.Height-1))
		return s, nil
	case transcriptLoadedMsg:
		if msg.path != s.session.Path {
			return s, nil
		}
		s.busy = false
		s.records, s.hidden, s.truncated, s.err = msg.records, msg.hidden, msg.truncated, msg.err
		if msg.err != nil && len(msg.lines) == 0 {
			return s, statusError(msg.err)
		}
		s.vp.SetContentLines(msg.lines)
		return s, nil
	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Done) {
			return s, popScreen()
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *transcriptScreen) View(width, height int) string {
	if s.busy {
		return styleFaint.Render("reading transcript…")
	}
	head := fmt.Sprintf("%s shown", countNoun(s.records-s.hidden, "record"))
	if s.hidden > 0 {
		head += fmt.Sprintf(", %d hidden (meta, sidechain, progress)", s.hidden)
	}
	if s.truncated {
		head += "  (first 8 MB)"
	}
	if s.err != nil {
		head += "  read error: " + transcripts.Sanitize(s.err.Error())
	}
	return styleFaint.Render(truncate(head, width)) + "\n" + s.vp.View()
}
