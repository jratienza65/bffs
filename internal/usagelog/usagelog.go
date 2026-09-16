// Package usagelog records one event per claude launch through the bffs shim.
// The log is the attribution backbone for `bffs usage`: Claude Code's own
// transcripts carry no account identity, so the only way to know which
// account a session belonged to is that bffs was there at launch time.
//
// The file is append-only JSONL at <cfgDir>/launches.jsonl, mode 0600. It
// contains account names and working directories; users can opt out entirely
// with BFFS_NO_USAGE_LOG. There is no rotation — events are ~150 bytes, so
// even 10k launches stay under 2 MB. (A `bffs usage --prune` could compact it
// later if that ever matters.)
package usagelog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	// Filename is the launch log's name under the bffs config dir.
	Filename = "launches.jsonl"
	// EnvDisable disables launch logging when set to any non-empty value.
	EnvDisable = "BFFS_NO_USAGE_LOG"
)

// Event is one claude launch as the shim saw it.
type Event struct {
	TS      time.Time `json:"ts"`                // UTC
	Account string    `json:"account,omitempty"` // empty for source=none launches
	Type    string    `json:"type,omitempty"`    // "oauth" | "api_key"
	Source  string    `json:"source"`            // resolver.Source string
	Cwd     string    `json:"cwd"`
}

// Path returns the launch log location under cfgDir.
func Path(cfgDir string) string {
	return filepath.Join(cfgDir, Filename)
}

// Disabled reports whether the user opted out of launch logging.
func Disabled() bool {
	return os.Getenv(EnvDisable) != ""
}

// Append writes one event as a single JSON line. The line is marshaled before
// the file is opened and written in one Write call, so concurrent launches
// cannot interleave partial lines on POSIX (single write under PIPE_BUF);
// on Windows a rare torn line is tolerated because Read skips corrupt lines.
func Append(cfgDir string, e Event) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(cfgDir, 0o700)
	f, err := os.OpenFile(Path(cfgDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// Read returns all events in file order (chronological). A missing file is
// (nil, nil). Corrupt or zero-timestamp lines are skipped, including a torn
// final line from an interrupted write; a valid final line without a trailing
// newline is kept.
func Read(cfgDir string) ([]Event, error) {
	f, err := os.Open(Path(cfgDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []Event
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e Event
			if json.Unmarshal(line, &e) == nil && !e.TS.IsZero() {
				events = append(events, e)
			}
		}
		if err == io.EOF {
			return events, nil
		}
		if err != nil {
			return events, err
		}
	}
}
