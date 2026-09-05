package rehome

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// relocatedRecord is Claude's own three-field relocation record
// (disk.md §2.5), in the order Claude writes it.
type relocatedRecord struct {
	Type         string `json:"type"`
	SessionID    string `json:"sessionId"`
	RelocatedCwd string `json:"relocatedCwd"`
}

// tailWindow is how much of a transcript's end is read per step while
// looking for the start of its last line; maxLastLine bounds the search.
const (
	tailWindow  = 64 << 10
	maxLastLine = 64 << 20
)

// RelocatedRecord renders the record a rehome appends to a transcript:
// exactly {"type":"relocated","sessionId":"<sid>","relocatedCwd":"<newCwd>"}
// followed by a newline (plan §9.8). newCwd is the user-facing mapped path,
// the way Claude writes the raw process cwd; every reader prefers the last
// such record over the head's cwd.
func RelocatedRecord(sid, newCwd string) []byte {
	b, err := json.Marshal(relocatedRecord{Type: "relocated", SessionID: sid, RelocatedCwd: newCwd})
	if err != nil { // a string-only struct never fails to marshal
		panic(err)
	}
	return append(b, '\n')
}

// AppendRelocated appends RelocatedRecord(sid, newCwd) to the transcript
// at path in ONE O_APPEND write, preceded by "\n" when the file does not
// end with one. An empty file is refused. When the file's last line is not
// newline-terminated and does not parse as a JSON object the session most
// likely crashed mid-write; the append is refused unless force, so the
// user can resume it once in claude (which repairs the tail) or pass
// --force-stamp.
func AppendRelocated(path, sid, newCwd string, force bool) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return appendRelocated(f, sid, newCwd, force)
}

// appendRelocated is AppendRelocated on an open handle (O_RDWR|O_APPEND),
// so a caller writing through an os.Root can use it too.
func appendRelocated(f *os.File, sid, newCwd string, force bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("transcript %s is empty; nothing to relocate", sid)
	}
	complete, endsWithNewline, err := lastLineState(f, info.Size())
	if err != nil {
		return fmt.Errorf("transcript %s: %w", sid, err)
	}
	if !endsWithNewline && !complete && !force {
		return fmt.Errorf("transcript %s has an incomplete last line (crashed session?); resume it once in claude or pass --force-stamp", sid)
	}
	var buf []byte
	if !endsWithNewline {
		buf = append(buf, '\n')
	}
	buf = append(buf, RelocatedRecord(sid, newCwd)...)
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("transcript %s: write: %w", sid, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("transcript %s: fsync: %w", sid, err)
	}
	return nil
}

// lastLineState reads the end of f (size bytes long) and reports whether
// its last line is a JSON object and whether the file ends with '\n'. A
// last line longer than maxLastLine counts as incomplete.
func lastLineState(f io.ReaderAt, size int64) (complete, endsWithNewline bool, err error) {
	var tail []byte
	end := size
	for {
		start := max(end-tailWindow, 0)
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return false, false, err
		}
		tail = append(chunk, tail...)
		if len(tail) > 0 && tail[len(tail)-1] == '\n' {
			return true, true, nil
		}
		if i := bytes.LastIndexByte(tail, '\n'); i >= 0 {
			return isJSONObject(tail[i+1:]), false, nil
		}
		if start == 0 {
			return isJSONObject(tail), false, nil
		}
		if len(tail) >= maxLastLine {
			return false, false, nil
		}
		end = start
	}
}

// CheckLastLine reports whether the transcript at path can be stamped
// without --force-stamp: ok when it ends with '\n' or its last line is a
// JSON object, false when the last line is torn (a session that crashed
// mid-write, or a live one exported before it finished the line). An
// unreadable or empty file is an error. PlanRehome and porter.Import ask
// it before a relocated record is appended (plan §9.8).
func CheckLastLine(path string) (ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, errors.New("transcript is empty")
	}
	complete, endsWithNewline, err := lastLineState(f, info.Size())
	if err != nil {
		return false, err
	}
	return complete || endsWithNewline, nil
}

// isJSONObject reports whether line (trimmed) is one JSON object.
func isJSONObject(line []byte) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var v map[string]json.RawMessage
	return json.Unmarshal(line, &v) == nil
}
