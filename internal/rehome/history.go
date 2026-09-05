package rehome

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jratienza65/bffs/internal/fsutil"
)

const (
	// historyMaxLine is the largest history line AppendHistory accepts;
	// historyMaxLines caps one call. Both match plan §9.10.
	historyMaxLine  = 1 << 20
	historyMaxLines = 10_000

	// historyLockStale and historyLockWait are the advisory lock windows on
	// history.jsonl. Claude's proper-lockfile lock on the same file uses a
	// 10 s stale window; 3 s is longer than any retention rewrite takes.
	historyLockStale = 10 * time.Second
	historyLockWait  = 3 * time.Second
)

// historyKeys are the fields Claude writes per prompt; everything else in
// an imported line is dropped.
var historyKeys = []string{"display", "pastedContents", "project", "sessionId", "timestamp"}

// AppendHistory appends the imported history.jsonl lines of session sid to
// historyPath under plan §9.10. Each line must parse as a JSON object whose
// sessionId is sid and carry a timestamp; it is reduced to the five known
// keys (display, pastedContents, project, sessionId, timestamp) with
// project replaced by newProject when that is non-empty, and re-serialised.
// Lines over 1 MiB, lines beyond the first 10 000, and lines whose
// (sessionId, timestamp) pair already exists — in the file or earlier in
// the batch — are dropped. The existing file is read once sequentially;
// the survivors are written in ONE O_APPEND write while the advisory lock
// "<realpath of historyPath>.lock" is held (fsutil.Lock, 10 s stale, 3 s
// wait); the file is created 0600 when missing. added counts the lines
// written.
func AppendHistory(historyPath, sid string, lines [][]byte, newProject string) (added int, err error) {
	if err := validateSessionID(sid); err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	if len(lines) == 0 {
		return 0, nil
	}
	type rec struct {
		key string
		out []byte
	}
	var batch []rec
	seen := map[string]bool{}
	for i, line := range lines {
		if i >= historyMaxLines {
			break
		}
		if len(line) > historyMaxLine {
			continue
		}
		out, key, ok := reduceHistoryLine(line, sid, newProject)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		batch = append(batch, rec{key, out})
	}
	if len(batch) == 0 {
		return 0, nil
	}

	release, err := fsutil.Lock(historyLockPath(historyPath), historyLockStale, historyLockWait)
	if err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	defer release()

	existing, endsWithNewline, err := existingHistoryKeys(historyPath, sid)
	if err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	var buf bytes.Buffer
	if !endsWithNewline {
		buf.WriteByte('\n')
	}
	for _, r := range batch {
		if existing[r.key] {
			continue
		}
		buf.Write(r.out)
		buf.WriteByte('\n')
		added++
	}
	if added == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(historyPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return 0, fmt.Errorf("history: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("history: write %q: %w", historyPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("history: fsync %q: %w", historyPath, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("history: close %q: %w", historyPath, err)
	}
	return added, nil
}

// reduceHistoryLine validates one imported line and returns its reduced
// serialisation plus the dedupe key (the compact timestamp).
func reduceHistoryLine(line []byte, sid, newProject string) (out []byte, key string, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil || m == nil {
		return nil, "", false
	}
	var got string
	if err := json.Unmarshal(m["sessionId"], &got); err != nil || got != sid {
		return nil, "", false
	}
	ts, ok := m["timestamp"]
	if !ok {
		return nil, "", false
	}
	key, ok = compactKey(ts)
	if !ok {
		return nil, "", false
	}
	reduced := map[string]json.RawMessage{}
	for _, k := range historyKeys {
		if v, ok := m[k]; ok {
			reduced[k] = v
		}
	}
	if newProject != "" {
		p, err := json.Marshal(newProject)
		if err != nil {
			return nil, "", false
		}
		reduced["project"] = p
	}
	out, err := json.Marshal(reduced)
	if err != nil {
		return nil, "", false
	}
	return out, key, true
}

// compactKey canonicalises a raw timestamp value for comparison.
func compactKey(raw json.RawMessage) (string, bool) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", false
	}
	return b.String(), true
}

// existingHistoryKeys reads historyPath once, sequentially, and returns
// the timestamps already recorded for sid and whether the file ends with a
// newline (true for a missing or empty file). Lines that are not valid
// records are skipped.
func existingHistoryKeys(historyPath, sid string) (keys map[string]bool, endsWithNewline bool, err error) {
	keys = map[string]bool{}
	f, err := os.Open(historyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return keys, true, nil
		}
		return nil, false, err
	}
	defer f.Close()
	marker := []byte(sid)
	r := bufio.NewReaderSize(f, 256*1024)
	endsWithNewline = true
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			endsWithNewline = line[len(line)-1] == '\n'
			if bytes.Contains(line, marker) {
				var rec struct {
					SessionID string          `json:"sessionId"`
					Timestamp json.RawMessage `json:"timestamp"`
				}
				if json.Unmarshal(bytes.TrimSpace(line), &rec) == nil && rec.SessionID == sid && rec.Timestamp != nil {
					if k, ok := compactKey(rec.Timestamp); ok {
						keys[k] = true
					}
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return keys, endsWithNewline, nil
			}
			return nil, false, fmt.Errorf("read %q: %w", historyPath, rerr)
		}
	}
}

// historyLockPath is "<realpath of historyPath>.lock": under partial
// isolation history.jsonl is a symlink into ~/.claude, and Claude's own
// lock (proper-lockfile, realpath: true) sits beside the real file.
func historyLockPath(historyPath string) string {
	if real, err := filepath.EvalSymlinks(historyPath); err == nil {
		return real + ".lock"
	}
	dir := filepath.Dir(historyPath)
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Join(dir, filepath.Base(historyPath)) + ".lock"
}
