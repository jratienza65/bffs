package transcripts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LiveSession is a session open in a running claude, as recorded by
// <configDir>/sessions/<pid>.json and confirmed against the OS. Verified is
// false when the pid exists but the start-time check itself could not run
// (ps missing, or no reference start time in the file); such a session is
// still treated as live — bffs never moves a transcript it cannot prove
// closed.
//
// Account and AccountKnown are filled by LiveAccounts, never by Live:
// AccountKnown reports that the process's environment could be read, and
// Account is then the oauth account whose session dir is its
// CLAUDE_CONFIG_DIR, HomeName when the variable is absent (an unmanaged or
// api_key launch against ~/.claude), or "" for a config dir bffs does not
// manage.
type LiveSession struct {
	PID       int
	SessionID string
	Cwd       string
	ConfigDir string // the config dir whose sessions/ named it
	Verified  bool

	Account      string
	AccountKnown bool
}

// ProcStart returns the OS start time of pid in the form Claude records
// (unix: the `ps -o lstart=` line; windows: the process creation FILETIME
// as a decimal string). It is a variable so tests can inject start times.
var ProcStart = procStart

// maxRuntimeSessionFile bounds how much of a sessions/<pid>.json is read;
// real files are well under a kilobyte.
const maxRuntimeSessionFile = 1 << 20

// runtimeSession is the subset of Claude's sessions/<pid>.json bffs reads.
// Fields keep raw JSON where Claude's type is not pinned.
type runtimeSession struct {
	PID         json.RawMessage `json:"pid"`
	SessionID   string          `json:"sessionId"`
	Cwd         string          `json:"cwd"`
	ProcStart   string          `json:"procStart"`
	ProcStartFt json.RawMessage `json:"procStartFt"`
}

// Live returns the sessions currently open in a running claude, keyed by
// session id, across the given config dirs. Each <configDir>/sessions/*.json
// (the *.key siblings are never read) names a pid; the session is live when
// that pid exists and — where a reference is recorded and the check can run
// — its OS start time matches (unix: procStart vs `ps -o lstart=` after
// whitespace normalisation; windows: procStartFt vs GetProcessTimes).
// Config dirs are deduplicated by resolved path, so partial-isolation
// accounts whose sessions/ is a symlink into ~/.claude are scanned once;
// a session found in more than one dir keeps the first. A missing sessions/
// is simply empty; unreadable or malformed files are skipped.
func Live(ctx context.Context, configDirs []string) (map[string]LiveSession, error) {
	out := map[string]LiveSession{}
	seen := map[string]bool{}
	for _, cfg := range configDirs {
		if cfg == "" {
			continue
		}
		dir := filepath.Join(cfg, RuntimeSessionsSubdir)
		key := canonical(dir)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return out, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			rec, ok := readRuntimeSession(filepath.Join(dir, name))
			if !ok {
				continue
			}
			ls, live := probe(rec)
			if !live {
				continue
			}
			ls.ConfigDir = cfg
			if _, dup := out[ls.SessionID]; !dup {
				out[ls.SessionID] = ls
			}
		}
	}
	return out, nil
}

// probe decides whether the session a runtime file describes is live.
func probe(rec runtimeSession) (LiveSession, bool) {
	pid, ok := parsePID(rec.PID)
	if !ok || rec.SessionID == "" || !pidExists(pid) {
		return LiveSession{}, false
	}
	ls := LiveSession{PID: pid, SessionID: rec.SessionID, Cwd: rec.Cwd}
	want, ok := expectedStart(rec)
	if !ok {
		return ls, true // no reference recorded: pid exists ⇒ live, unverified
	}
	got, err := ProcStart(pid)
	if err != nil {
		return ls, true // the check could not run: still live, unverified
	}
	if !startMatches(got, want) {
		return LiveSession{}, false // the pid was reused by another process
	}
	ls.Verified = true
	return ls, true
}

func readRuntimeSession(path string) (runtimeSession, bool) {
	f, err := os.Open(path)
	if err != nil {
		return runtimeSession{}, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRuntimeSessionFile+1))
	if err != nil || len(raw) > maxRuntimeSessionFile {
		return runtimeSession{}, false
	}
	var rec runtimeSession
	if err := json.Unmarshal(raw, &rec); err != nil {
		return runtimeSession{}, false
	}
	return rec, true
}

// parsePID accepts a JSON number or a numeric string and returns a
// positive pid.
func parsePID(raw json.RawMessage) (int, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// normalizeWS collapses runs of whitespace to one space and trims the ends,
// so `ps` padding ("Sat Sep  5 …   ") and Claude's stored copy compare equal.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
