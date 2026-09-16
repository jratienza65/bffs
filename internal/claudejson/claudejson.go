// Package claudejson reads and patches the per-user ~/.claude.json file that
// Claude Code maintains. bffs only touches two top-level fields —
// oauthAccount (cached account metadata: email, orgUuid, etc.) and userID
// (the per-user hash) — because Claude Code reads identity from those caches
// rather than re-deriving it from the Keychain on every invocation. All other
// fields (projects, MCP, plugin data, etc.) are passed through verbatim.
package claudejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const Filename = ".claude.json"

// Path returns the absolute path to the user's ~/.claude.json.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, Filename), nil
}

// Snapshot is the subset of ~/.claude.json that identifies an OAuth account.
// OAuthAccount is stored as raw JSON so we can round-trip it verbatim.
type Snapshot struct {
	OAuthAccount json.RawMessage
	UserID       string
}

// Empty reports whether the snapshot has nothing to apply.
func (s Snapshot) Empty() bool {
	return len(s.OAuthAccount) == 0 && s.UserID == ""
}

// Read returns the current oauthAccount + userID from ~/.claude.json. If the
// file does not exist, returns an empty Snapshot, nil error.
func Read() (Snapshot, error) {
	p, err := Path()
	if err != nil {
		return Snapshot{}, err
	}
	return ReadFrom(p)
}

// ReadFrom returns the snapshot from a specific .claude.json path (e.g. one
// inside a per-account session dir). Missing file returns an empty Snapshot,
// nil error.
func ReadFrom(p string) (Snapshot, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Snapshot{}, nil
		}
		return Snapshot{}, fmt.Errorf("read %s: %w", p, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Snapshot{}, fmt.Errorf("parse %s: %w", p, err)
	}
	var s Snapshot
	if v, ok := doc["oauthAccount"]; ok {
		s.OAuthAccount = append(json.RawMessage(nil), v...)
	}
	if v, ok := doc["userID"]; ok {
		var u string
		if err := json.Unmarshal(v, &u); err == nil {
			s.UserID = u
		}
	}
	return s, nil
}

// Patch reads ~/.claude.json, replaces the oauthAccount and userID fields
// with the snapshot's values, and atomically writes the file back. All other
// fields are preserved verbatim. The file's permissions are preserved.
//
// If the file does not exist, Patch creates it containing only the snapshot
// fields — Claude Code will fill in the rest on next use.
func Patch(s Snapshot) error {
	p, err := Path()
	if err != nil {
		return err
	}

	var (
		doc  = map[string]json.RawMessage{}
		perm os.FileMode = 0o600
	)
	if raw, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		if info, err := os.Stat(p); err == nil {
			perm = info.Mode().Perm()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", p, err)
	}

	if len(s.OAuthAccount) > 0 {
		doc["oauthAccount"] = s.OAuthAccount
	} else {
		delete(doc, "oauthAccount")
	}
	if s.UserID != "" {
		b, err := json.Marshal(s.UserID)
		if err != nil {
			return err
		}
		doc["userID"] = b
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, filepath.Base(p)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
