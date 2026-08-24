package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

// session accumulates one Claude Code session's scanned facts.
type session struct {
	id      string
	cwd     string
	owner   string // account name when scanned from a full-isolation root
	firstTS time.Time
	lastTS  time.Time
	short   Tokens // ShortWindow sum
	long    Tokens // horizon-window sum
	byModel map[string]Tokens
	byDay   map[string]Tokens
	limits  []LimitEvent
}

type scanner struct {
	now      time.Time
	horizon  time.Duration
	dedup    map[string]struct{} // message.id|requestId, global across all files
	sessions map[string]*session
}

// scan walks every transcript root and returns per-session aggregates.
// Roots: the (shared) home projects dir plus each oauth account's own
// projects dir when it is a real directory (full isolation) — symlinked ones
// point back into the shared pool and are deduplicated away.
func scan(cfgDir string, accs store.Accounts, homeClaudeDir string, now time.Time, horizon time.Duration) []*session {
	s := &scanner{
		now:      now,
		horizon:  horizon,
		dedup:    map[string]struct{}{},
		sessions: map[string]*session{},
	}

	type root struct{ dir, owner string }
	var roots []root
	seen := map[string]bool{}
	add := func(dir, owner string) {
		canon := dir
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			canon = r
		}
		if seen[canon] {
			return
		}
		seen[canon] = true
		roots = append(roots, root{dir, owner})
	}
	add(filepath.Join(homeClaudeDir, "projects"), "")
	for _, name := range accs.Names() {
		if accs.Accounts[name].Type != store.TypeOAuth {
			continue
		}
		p := filepath.Join(sessions.Dir(cfgDir, name), "projects")
		if info, err := os.Lstat(p); err == nil && info.IsDir() {
			add(p, name)
		}
	}

	for _, r := range roots {
		s.scanRoot(r.dir, r.owner)
	}

	out := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *scanner) scanRoot(rootDir, owner string) {
	slugs, err := os.ReadDir(rootDir)
	if err != nil {
		return
	}
	cutoff := s.now.Add(-s.horizon)
	for _, slug := range slugs {
		if !slug.IsDir() {
			continue
		}
		slugPath := filepath.Join(rootDir, slug.Name())
		entries, err := os.ReadDir(slugPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				if name := e.Name(); strings.HasSuffix(name, ".jsonl") {
					s.scanFile(filepath.Join(slugPath, name), strings.TrimSuffix(name, ".jsonl"), owner, cutoff, e)
				}
				continue
			}
			// <sid>/subagents/agent-*.jsonl share the parent session's id.
			subDir := filepath.Join(slugPath, e.Name(), "subagents")
			subs, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			for _, sub := range subs {
				if !sub.IsDir() && strings.HasSuffix(sub.Name(), ".jsonl") {
					s.scanFile(filepath.Join(subDir, sub.Name()), e.Name(), owner, cutoff, sub)
				}
			}
		}
	}
}

func (s *scanner) scanFile(path, sid, owner string, cutoff time.Time, entry os.DirEntry) {
	// Transcripts are append-only: a file untouched since the cutoff cannot
	// hold in-window records. This bounds scanning of a large history pool.
	if info, err := entry.Info(); err != nil || info.ModTime().Before(cutoff) {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Single transcript lines reach multiple MB — bufio.Scanner's token limit
	// would fail; ReadBytes has no cap.
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.scanLine(line, sid, owner)
		}
		if err != nil {
			return
		}
	}
}

var assistantMarker = []byte(`"type":"assistant"`)

// rec is the lean per-line decode target; everything else in a record is
// ignored without being materialized.
type rec struct {
	Type              string `json:"type"`
	Timestamp         string `json:"timestamp"`
	Cwd               string `json:"cwd"`
	RequestID         string `json:"requestId"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Message           struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func (s *scanner) scanLine(line []byte, sid, owner string) {
	if !bytes.Contains(line, assistantMarker) {
		return
	}
	var r rec
	if json.Unmarshal(line, &r) != nil || r.Type != "assistant" {
		return
	}
	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return
	}

	sess := s.session(sid, owner)
	if sess.cwd == "" && r.Cwd != "" {
		sess.cwd = r.Cwd
	}
	if sess.firstTS.IsZero() || ts.Before(sess.firstTS) {
		sess.firstTS = ts
	}
	if ts.After(sess.lastTS) {
		sess.lastTS = ts
	}

	if r.IsAPIErrorMessage {
		// Refused requests are limit signals, not consumption.
		if r.Error == "rate_limit" {
			sess.limits = append(sess.limits, parseLimitEvent(ts, r.Message.Content))
		}
		return
	}

	if r.Message.ID != "" {
		key := r.Message.ID + "|" + r.RequestID
		if _, dup := s.dedup[key]; dup {
			return
		}
		s.dedup[key] = struct{}{}
	}

	tok := Tokens{
		Input:         r.Message.Usage.InputTokens,
		Output:        r.Message.Usage.OutputTokens,
		CacheCreation: r.Message.Usage.CacheCreationInputTokens,
		CacheRead:     r.Message.Usage.CacheReadInputTokens,
		Messages:      1,
	}
	// Boundary instants are inclusive; future timestamps (clock skew) count
	// rather than dropping real usage.
	if !ts.Before(s.now.Add(-ShortWindow)) {
		sess.short = sess.short.add(tok)
	}
	if !ts.Before(s.now.Add(-s.horizon)) {
		sess.long = sess.long.add(tok)
		sess.byModel[r.Message.Model] = sess.byModel[r.Message.Model].add(tok)
		day := ts.UTC().Format("2006-01-02")
		sess.byDay[day] = sess.byDay[day].add(tok)
	}
}

func (s *scanner) session(sid, owner string) *session {
	if sess, ok := s.sessions[sid]; ok {
		return sess
	}
	sess := &session{
		id:      sid,
		owner:   owner,
		byModel: map[string]Tokens{},
		byDay:   map[string]Tokens{},
	}
	s.sessions[sid] = sess
	return sess
}
