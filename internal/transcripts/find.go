package transcripts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// ErrSessionNotFound is returned (wrapped, with the id) by Find when no
// transcript matches.
var ErrSessionNotFound = errors.New("unknown session")

// minPrefixLen is the shortest session-id prefix Find accepts, in hex
// digits.
const minPrefixLen = 8

// Find locates a session by full id or by a prefix of at least eight hex
// digits, across every root and slug — a session that Claude relocated,
// or that was copied between roots, can exist in several places, and all
// of them are returned. A prefix matching more than one distinct id is
// ErrAmbiguousSession; nothing matching is ErrSessionNotFound. Results
// are newest first with Titles populated; Live, Account and Import are
// left for the caller.
func Find(roots []Root, idOrPrefix string) ([]Session, error) {
	q := strings.ToLower(strings.TrimSpace(idOrPrefix))
	var match func(sid string) bool
	switch {
	case isUUID(q):
		match = func(sid string) bool { return strings.ToLower(sid) == q }
	case isUUIDPrefix(q):
		match = func(sid string) bool { return strings.HasPrefix(strings.ToLower(sid), q) }
	default:
		return nil, fmt.Errorf("invalid session id %q: expected a UUID or at least %d hex digits of one", idOrPrefix, minPrefixLen)
	}

	ctx := context.Background()
	now := time.Now()
	var out []Session
	ids := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", root.Dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() || IsReserved(e.Name()) {
				continue
			}
			ss, err := listSlug(ctx, root, e.Name(), match, time.Time{}, nil, now)
			if err != nil {
				return nil, err
			}
			for _, s := range ss {
				ids[strings.ToLower(s.ID)] = true
			}
			out = append(out, ss...)
		}
	}
	if len(ids) > 1 {
		names := make([]string, 0, len(ids))
		for id := range ids {
			names = append(names, id)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%w: %q matches %s", ErrAmbiguousSession, idOrPrefix, strings.Join(names, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w %q", ErrSessionNotFound, idOrPrefix)
	}
	sortNewestFirst(out)
	loaders := map[string]func() HistoryIndex{}
	for i := range out {
		cfg := out[i].Root.ConfigDir
		hist, ok := loaders[cfg]
		if !ok {
			hist = historyLoader(cfg, nil)
			loaders[cfg] = hist
		}
		out[i].enrich(hist)
	}
	return out, nil
}

// isUUIDPrefix reports whether q could start a lower-case session id and
// carries at least minPrefixLen hex digits.
func isUUIDPrefix(q string) bool {
	if q == "" || len(q) > 36 {
		return false
	}
	digits := 0
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '-':
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			if i == 8 || i == 13 || i == 18 || i == 23 {
				return false
			}
			digits++
		default:
			return false
		}
	}
	return digits >= minPrefixLen
}
