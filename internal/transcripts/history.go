package transcripts

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// HistoryFile is Claude's prompt history inside a config dir: one JSON
// line per typed prompt {display, pastedContents, project, sessionId,
// timestamp}. It is the cheapest source of a session's first prompt.
const HistoryFile = "history.jsonl"

// HistoryIndex maps a session id to the first line of the first prompt
// typed in it, trimmed and capped at promptCap runes.
type HistoryIndex map[string]string

var sessionIDMarker = []byte(`"sessionId"`)

// LoadHistory reads <configDir>/history.jsonl once, sequentially, and
// indexes the first non-empty display per session id. A missing file is
// an empty index; lines that are not valid records are skipped.
func LoadHistory(configDir string) (HistoryIndex, error) {
	idx := HistoryIndex{}
	path := filepath.Join(configDir, HistoryFile)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return idx, nil
		}
		return nil, err
	}
	defer f.Close()

	// Single lines can be large (pasted content); ReadBytes has no cap,
	// bufio.Scanner does.
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			idx.absorb(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return idx, nil
			}
			return idx, fmt.Errorf("read %s: %w", path, err)
		}
	}
}

func (idx HistoryIndex) absorb(line []byte) {
	if !bytes.Contains(line, sessionIDMarker) {
		return
	}
	var rec struct {
		Display   string `json:"display"`
		SessionID string `json:"sessionId"`
	}
	if !decodeLenient(bytes.TrimSpace(line), &rec) || rec.SessionID == "" {
		return
	}
	if _, seen := idx[rec.SessionID]; seen {
		return
	}
	if d := firstLine(rec.Display, promptCap); d != "" {
		idx[rec.SessionID] = d
	}
}
