package transcripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// windowSize is how many bytes Claude Code reads from each end of a
// transcript to build a picker row (md = 65536 in 2.1.259). ReadHead and
// ReadTail read the same windows, so the cwd and title bffs shows are the
// ones the picker shows — and a multi-megabyte record never has to be
// parsed to list a session.
const windowSize = 64 * 1024

// promptCap is the longest prompt- or history-derived string kept, in
// runes. Titles are one line; the rest of a prompt is never needed.
const promptCap = 200

// maxLastLine bounds the reverse scan ReadTail performs when a
// transcript's last line is longer than the tail window: beyond this,
// LastLineComplete is reported false rather than reading further.
const maxLastLine = 64 << 20

// Head is what the first windowSize bytes of a transcript say about the
// session. Every field is taken from the first record that carries it.
type Head struct {
	Cwd         string    // "cwd" of the first record carrying one: the project the transcript belongs to
	SessionID   string    // first "sessionId"
	Version     string    // claude version that wrote the first record carrying one
	GitBranch   string    // first "gitBranch"
	PlanSlug    string    // first "slug": the plans/<slug>.md key
	FirstPrompt string    // first line of the first human prompt (else the first slash command), trimmed, at most promptCap runes
	FirstTS     time.Time // first "timestamp"
	Torn        bool      // the window ended mid-line before any full record was seen
}

// Tail is what the last windowSize bytes of a transcript say about the
// session. Claude re-stamps the metadata records on every resume so the
// tail always carries them. Each field is taken from the last record in
// the window that carries it as a JSON string, the way Claude's readers
// scan backwards for the key: a record without the field (the common
// re-stamped last-prompt shape) leaves it untouched, an empty string
// clears it. RelocatedCwd additionally requires the "relocated" type.
type Tail struct {
	RelocatedCwd     string    // last "relocated" record's relocatedCwd: overrides Head.Cwd
	CustomTitle      string    // last customTitle
	AITitle          string    // last aiTitle
	LastPrompt       string    // last lastPrompt
	Summary          string    // last summary (a "summary" record): Claude's tier between lastPrompt and the first prompt
	LastTS           time.Time // last "timestamp"
	LastLineComplete bool      // the file ends with '\n' and its last line is a JSON object
}

// headRec is the lean decode target for head lines. message stays raw:
// only human prompts need it opened, and it may be a very large object.
type headRec struct {
	Type             string          `json:"type"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Cwd              string          `json:"cwd"`
	SessionID        string          `json:"sessionId"`
	Version          string          `json:"version"`
	GitBranch        string          `json:"gitBranch"`
	Slug             string          `json:"slug"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message"`
}

// tailRec is the lean decode target for tail lines. The title fields stay
// raw so an absent field (leave the value alone) is told apart from an
// empty one (clear it) and from one of another type (ignored) — see
// jsonString.
type tailRec struct {
	Type         string          `json:"type"`
	RelocatedCwd json.RawMessage `json:"relocatedCwd"`
	CustomTitle  json.RawMessage `json:"customTitle"`
	AITitle      json.RawMessage `json:"aiTitle"`
	LastPrompt   json.RawMessage `json:"lastPrompt"`
	Summary      json.RawMessage `json:"summary"`
	Timestamp    string          `json:"timestamp"`
}

// jsonString decodes raw when it is a JSON string (Claude's
// typeof === "string" check); anything else — absent, null, a number —
// reports ok=false.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// headMarkers are the substrings a head line must contain to be worth
// decoding; a line with none of them cannot fill a Head field.
var headMarkers = [][]byte{
	[]byte(`"cwd"`), []byte(`"sessionId"`), []byte(`"timestamp"`), []byte(`"version"`),
	[]byte(`"gitBranch"`), []byte(`"slug"`), []byte(`"user"`),
}

// tailMarkers are the substrings a tail line must contain to be worth
// decoding: one of the keys Claude's readers look for, or a timestamp.
var tailMarkers = [][]byte{
	[]byte(`"relocatedCwd"`), []byte(`"customTitle"`), []byte(`"aiTitle"`), []byte(`"lastPrompt"`), []byte(`"summary"`), []byte(`"timestamp"`),
}

// The prompt rules of Claude's picker (2.1.259, function _8): a slash
// command is remembered as a fallback title and skipped, a bash-mode
// prompt is shown as "! <command>", and a text starting with a tag
// (<local-command-caveat>, <local-command-stdout>, …) or an interruption
// notice is skipped.
var (
	commandNameRE = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	bashInputRE   = regexp.MustCompile(`<bash-input>([\s\S]*?)</bash-input>`)
	skipPromptRE  = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)
)

// ReadHead reads the first windowSize bytes of the transcript at path and
// returns what its whole lines say. A line the window cuts is dropped;
// when the window holds the entire file, an unterminated last line counts
// if it is valid JSON (a torn one is not). Lines that are not JSON objects
// are skipped; a record whose field has an unexpected type keeps its other
// fields. FirstPrompt follows Claude's picker: the first non-meta human
// prompt that is not a slash command, a tag-led notice or a tool result;
// when the window holds none, the first slash command seen ("/login")
// stands in.
func ReadHead(path string) (Head, error) {
	f, err := os.Open(path)
	if err != nil {
		return Head{}, err
	}
	defer f.Close()
	buf := make([]byte, windowSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return Head{}, fmt.Errorf("read %s: %w", path, err)
	}
	return parseHead(buf[:n], n < windowSize), nil
}

// parseHead is ReadHead on an in-memory window; atEOF says the window
// reaches the end of the file.
func parseHead(window []byte, atEOF bool) Head {
	var h Head
	fallback := "" // the first slash command, shown when no prompt follows
	full := false  // a whole record was seen
	rest := window
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		var line []byte
		if i >= 0 {
			line, rest = rest[:i], rest[i+1:]
			full = true
		} else {
			if !atEOF {
				break // cut by the window: dropped
			}
			line, rest = rest, nil
			line = bytes.TrimSpace(line)
			if len(line) == 0 || !json.Valid(line) {
				break // torn (or nothing): dropped
			}
			full = true
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !containsAny(line, headMarkers) {
			continue
		}
		var r headRec
		if !decodeLenient(line, &r) {
			continue
		}
		h.absorb(r, &fallback)
		if h.complete() {
			break
		}
	}
	if h.FirstPrompt == "" {
		h.FirstPrompt = fallback
	}
	h.Torn = !full && len(bytes.TrimSpace(window)) > 0
	return h
}

func (h *Head) absorb(r headRec, fallback *string) {
	if h.Cwd == "" {
		h.Cwd = r.Cwd
	}
	if h.SessionID == "" {
		h.SessionID = r.SessionID
	}
	if h.Version == "" {
		h.Version = r.Version
	}
	if h.GitBranch == "" {
		h.GitBranch = r.GitBranch
	}
	if h.PlanSlug == "" {
		h.PlanSlug = r.Slug
	}
	if h.FirstTS.IsZero() && r.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, r.Timestamp); err == nil {
			h.FirstTS = ts
		}
	}
	if h.FirstPrompt == "" && r.Type == "user" && !r.IsMeta && !r.IsCompactSummary {
		h.FirstPrompt = promptText(r.Message, fallback)
	}
}

func (h *Head) complete() bool {
	return h.Cwd != "" && h.SessionID != "" && h.Version != "" && h.GitBranch != "" &&
		h.PlanSlug != "" && !h.FirstTS.IsZero() && h.FirstPrompt != ""
}

// promptText extracts the human-typed text of a user record's message the
// way Claude's picker does: a string content, or every {type:"text"}
// block — unless any block is a tool_result, which makes the record a
// tool turn. Texts are tried in order: a slash command sets *fallback
// (first one wins) and is skipped, a <bash-input> becomes "! <command>",
// a tag-led or interruption text is skipped, the first remaining text is
// the prompt (its first line, at most promptCap runes).
func promptText(msg json.RawMessage, fallback *string) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if len(msg) == 0 || json.Unmarshal(msg, &m) != nil {
		return ""
	}
	c := bytes.TrimSpace(m.Content)
	if len(c) == 0 {
		return ""
	}
	var texts []string
	switch c[0] {
	case '"':
		var s string
		if json.Unmarshal(c, &s) != nil {
			return ""
		}
		texts = append(texts, s)
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c, &blocks) != nil {
			return ""
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				return ""
			case "text":
				texts = append(texts, b.Text)
			}
		}
	}
	for _, text := range texts {
		joined := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
		if joined == "" {
			continue
		}
		if m := commandNameRE.FindStringSubmatch(joined); m != nil {
			if *fallback == "" {
				*fallback = firstLine(m[1], promptCap)
			}
			continue
		}
		if m := bashInputRE.FindStringSubmatch(joined); m != nil {
			return firstLine("! "+strings.TrimSpace(m[1]), promptCap)
		}
		if skipPromptRE.MatchString(joined) {
			continue
		}
		return firstLine(text, promptCap)
	}
	return ""
}

// ReadTail reads the last windowSize bytes of the transcript at path (the
// whole file when it is smaller) and returns what its whole lines say.
// When the window starts inside the file its first, partial line is
// skipped — unless that partial line is the file's last line, in which
// case it is read in full (up to maxLastLine) so LastLineComplete stays
// truthful for a transcript whose final record is larger than the window.
func ReadTail(path string) (Tail, error) {
	f, err := os.Open(path)
	if err != nil {
		return Tail{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Tail{}, fmt.Errorf("stat %s: %w", path, err)
	}
	size := info.Size()
	var start int64
	if size > windowSize {
		start = size - windowSize
	}
	buf := make([]byte, size-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return Tail{}, fmt.Errorf("read %s: %w", path, err)
	}
	t, needLast := parseTail(buf[:n], start > 0)
	if needLast {
		// The last line began before the window; only reading it whole
		// can say whether it is complete.
		if line, ok := readLastLine(f, start+int64(n)); ok {
			line = bytes.TrimSpace(line)
			t.LastLineComplete = isJSONObject(line)
			var r tailRec
			if containsAny(line, tailMarkers) && decodeLenient(line, &r) {
				t.absorb(r)
			}
		}
	}
	return t, nil
}

// parseTail is ReadTail on an in-memory window. skipFirst says the window
// starts inside the file, so its first line is the tail of an earlier one.
// needLast reports that the file ends with '\n' but the terminating line
// began before the window, so the caller must read it separately.
func parseTail(window []byte, skipFirst bool) (t Tail, needLast bool) {
	endsNL := len(window) > 0 && window[len(window)-1] == '\n'
	rest := window
	if skipFirst {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return t, false // one unterminated line spans the window: nothing whole, nothing complete
		}
		rest = rest[i+1:]
	}
	var last []byte
	sawLine := false
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		var line []byte
		if i >= 0 {
			line, rest = rest[:i], rest[i+1:]
			sawLine = true
			last = line
		} else {
			line, rest = rest, nil // unterminated: kept only if it decodes
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !containsAny(line, tailMarkers) {
			continue
		}
		var r tailRec
		if !decodeLenient(line, &r) {
			continue
		}
		t.absorb(r)
	}
	if endsNL {
		if sawLine {
			t.LastLineComplete = isJSONObject(bytes.TrimSpace(last))
		} else {
			needLast = skipFirst
		}
	}
	return t, needLast
}

func (t *Tail) absorb(r tailRec) {
	if v, ok := jsonString(r.RelocatedCwd); ok && r.Type == "relocated" {
		t.RelocatedCwd = v
	}
	if v, ok := jsonString(r.CustomTitle); ok {
		t.CustomTitle = v
	}
	if v, ok := jsonString(r.AITitle); ok {
		t.AITitle = v
	}
	if v, ok := jsonString(r.LastPrompt); ok {
		t.LastPrompt = v
	}
	if v, ok := jsonString(r.Summary); ok {
		t.Summary = v
	}
	if r.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, r.Timestamp); err == nil {
			t.LastTS = ts
		}
	}
}

// readLastLine returns the bytes of the last line of f — the file has
// size bytes and ends with '\n' — without that newline. It scans
// backwards in window-sized chunks for the previous newline and gives up
// past maxLastLine.
func readLastLine(f *os.File, size int64) ([]byte, bool) {
	if size <= 0 {
		return nil, false
	}
	end := size - 1 // the terminating newline
	pos := end
	chunk := make([]byte, windowSize)
	for pos > 0 {
		if end-pos > maxLastLine {
			return nil, false
		}
		n := int64(windowSize)
		if n > pos {
			n = pos
		}
		if _, err := f.ReadAt(chunk[:n], pos-n); err != nil && !errors.Is(err, io.EOF) {
			return nil, false
		}
		if i := bytes.LastIndexByte(chunk[:n], '\n'); i >= 0 {
			return readRange(f, pos-n+int64(i)+1, end)
		}
		pos -= n
	}
	return readRange(f, 0, end)
}

func readRange(f *os.File, from, to int64) ([]byte, bool) {
	if to < from || to-from > maxLastLine {
		return nil, false
	}
	buf := make([]byte, to-from)
	if _, err := f.ReadAt(buf, from); err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return buf, true
}

// EffectiveCwd is the directory a session belongs to: the last relocation
// in the tail when there is one, else the first cwd in the head — the
// order Claude's readers use.
func EffectiveCwd(h Head, t Tail) string {
	if t.RelocatedCwd != "" {
		return t.RelocatedCwd
	}
	return h.Cwd
}

// decodeLenient unmarshals one transcript line into v. A field of the
// wrong type is tolerated (encoding/json fills the rest and reports an
// UnmarshalTypeError); a syntax error — a torn line, or not JSON — is not.
func decodeLenient(line []byte, v any) bool {
	err := json.Unmarshal(line, v)
	if err == nil {
		return true
	}
	var te *json.UnmarshalTypeError
	return errors.As(err, &te)
}

func isJSONObject(line []byte) bool {
	return len(line) > 0 && line[0] == '{' && json.Valid(line)
}

func containsAny(line []byte, markers [][]byte) bool {
	for _, m := range markers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

// firstLine trims s, keeps its first line, trims that, and cuts it at
// capRunes runes.
func firstLine(s string, capRunes int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) > capRunes {
		rs := []rune(s)
		s = strings.TrimSpace(string(rs[:capRunes]))
	}
	return s
}
