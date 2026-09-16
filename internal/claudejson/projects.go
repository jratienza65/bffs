package claudejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Keys inside one projects[<key>] entry that bffs reads or writes. Every
// other key in an entry (metrics, caches, permission grants) is passed
// through verbatim.
const (
	keyTrustAccepted                = "hasTrustDialogAccepted"
	keyExternalIncludesApproved     = "hasClaudeMdExternalIncludesApproved"
	keyExternalIncludesWarningShown = "hasClaudeMdExternalIncludesWarningShown"
	keyLastSessionID                = "lastSessionId"
	keyAllowedTools                 = "allowedTools"
	keyEnabledMCPJSONServers        = "enabledMcpjsonServers"
)

// ProjectFlags is the bffs-relevant view of one projects[<key>] entry in a
// .claude.json. The three trust booleans are pointers so "never answered"
// (key absent or not a boolean) stays distinguishable from an explicit false:
// a declined external-includes dialog is ExternalIncludesWarningShown=true
// with ExternalIncludesApproved=false, while an unanswered one has both nil.
//
// AllowedTools is the number of allowedTools entries (Claude's legacy string
// form counts as one when non-empty); MCPEnabled is the number of
// enabledMcpjsonServers entries. Both are counts only — the grants themselves
// are never surfaced here.
type ProjectFlags struct {
	TrustAccepted                *bool
	ExternalIncludesApproved     *bool
	ExternalIncludesWarningShown *bool
	LastSessionID                string
	AllowedTools                 int
	MCPEnabled                   int
}

// ReadProjectFlags returns the ProjectFlags of every entry in the projects
// map of the .claude.json at path, keyed by the project key exactly as
// written in the file (Claude uses the canonical git root, else the realpath
// cwd — never a slug). A missing file, or a projects field that is not an
// object, yields an empty map and a nil error; an entry that is not an object
// is skipped, and a field with an unexpected type inside an entry reads as
// unset. Only an unreadable or unparseable file is an error.
func ReadProjectFlags(path string) (map[string]ProjectFlags, error) {
	out := map[string]ProjectFlags{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(doc["projects"], &projects); err != nil {
		return out, nil
	}
	for key, entry := range projects {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry, &fields); err != nil || fields == nil {
			continue
		}
		out[key] = flagsOf(fields)
	}
	return out, nil
}

func flagsOf(fields map[string]json.RawMessage) ProjectFlags {
	return ProjectFlags{
		TrustAccepted:                boolOf(fields[keyTrustAccepted]),
		ExternalIncludesApproved:     boolOf(fields[keyExternalIncludesApproved]),
		ExternalIncludesWarningShown: boolOf(fields[keyExternalIncludesWarningShown]),
		LastSessionID:                stringOf(fields[keyLastSessionID]),
		AllowedTools:                 allowedToolsCount(fields[keyAllowedTools]),
		MCPEnabled:                   arrayLen(fields[keyEnabledMCPJSONServers]),
	}
}

// boolOf returns a pointer to the decoded boolean, or nil when raw is absent,
// null or not a JSON boolean.
func boolOf(raw json.RawMessage) *bool {
	var b *bool
	if raw == nil || json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return b
}

func stringOf(raw json.RawMessage) string {
	var s string
	if raw == nil || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// arrayLen returns the element count of a JSON array, or 0 when raw is absent
// or not an array.
func arrayLen(raw json.RawMessage) int {
	var items []json.RawMessage
	if raw == nil || json.Unmarshal(raw, &items) != nil {
		return 0
	}
	return len(items)
}

// allowedToolsCount handles both shapes Claude accepts for allowedTools: the
// current array form and the legacy single string, which Claude parses into
// one rule and therefore counts as 1 when non-empty.
func allowedToolsCount(raw json.RawMessage) int {
	if raw == nil {
		return 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		return len(items)
	}
	if strings.TrimSpace(stringOf(raw)) != "" {
		return 1
	}
	return 0
}

// DefaultProjectEntry returns a fresh projects[<key>] entry in the shape
// Claude Code itself creates for a project it has not seen before. Callers
// that need to record an answer for a project the file does not know yet
// start from this so the entry looks native to Claude.
func DefaultProjectEntry() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		keyAllowedTools:                 json.RawMessage(`[]`),
		"mcpContextUris":                json.RawMessage(`[]`),
		"mcpServers":                    json.RawMessage(`{}`),
		keyEnabledMCPJSONServers:        json.RawMessage(`[]`),
		"disabledMcpjsonServers":        json.RawMessage(`[]`),
		keyTrustAccepted:                json.RawMessage(`false`),
		keyExternalIncludesApproved:     json.RawMessage(`false`),
		keyExternalIncludesWarningShown: json.RawMessage(`false`),
	}
}

// UpdateProjects applies fn to the projects map of the .claude.json at path
// and writes the file back if fn reports a change. fn receives every entry as
// raw JSON (keyed by project key as written) and may add, replace or delete
// entries; the map is created empty when the file or its projects field is
// absent. Every other top-level field is preserved verbatim, and so is every
// field inside an entry that fn does not touch. A projects field that is not
// an object is an error — never clobber something Claude wrote in a shape we
// do not understand.
//
// Nothing is written when fn returns changed=false or an error (the error is
// returned as is). The write is atomic (temp file + rename) and keeps the
// file's permissions; a missing file is created with 0600. The result reports
// whether the file was written.
//
// UpdateProjects does not take Claude's proper-lockfile lock (<path>.lock):
// the caller holds whatever lock the situation needs and keeps fn short — a
// running claude polls this file about once a second.
func UpdateProjects(path string, fn func(projects map[string]json.RawMessage) (changed bool, err error)) (bool, error) {
	doc, perm, err := readDoc(path)
	if err != nil {
		return false, err
	}
	projects, err := projectsOf(doc, path)
	if err != nil {
		return false, err
	}
	changed, err := fn(projects)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := encodeInto(doc, "projects", projects); err != nil {
		return false, err
	}
	if err := writeTop(path, doc, perm); err != nil {
		return false, err
	}
	return true, nil
}

// projectsOf extracts the projects map as raw JSON so untouched entries
// round-trip verbatim. Absent or null yields an empty map; a present
// non-object value is an error.
func projectsOf(doc map[string]json.RawMessage, path string) (map[string]json.RawMessage, error) {
	projects := map[string]json.RawMessage{}
	if raw, ok := doc["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return nil, fmt.Errorf("%s: projects is not a JSON object: %w", path, err)
		}
		if projects == nil {
			projects = map[string]json.RawMessage{}
		}
	}
	return projects, nil
}

// SetLastSessionID records sid as projects[projectKey].lastSessionId in the
// .claude.json at path — the pointer `claude --continue` follows — creating
// the entry from DefaultProjectEntry when the file does not know the project.
// Every other field of the entry is preserved. Nothing is written when the
// entry already points at sid.
func SetLastSessionID(path, projectKey, sid string) error {
	if projectKey == "" {
		return errors.New("project key must not be empty")
	}
	if sid == "" {
		return fmt.Errorf("session id for project %q must not be empty", projectKey)
	}
	_, err := UpdateProjects(path, func(projects map[string]json.RawMessage) (bool, error) {
		entry := DefaultProjectEntry()
		if raw, ok := projects[projectKey]; ok {
			var existing map[string]json.RawMessage
			if err := json.Unmarshal(raw, &existing); err != nil {
				return false, fmt.Errorf("%s: projects[%q] is not a JSON object: %w", path, projectKey, err)
			}
			if existing != nil {
				entry = existing
			}
		}
		if stringOf(entry[keyLastSessionID]) == sid {
			return false, nil
		}
		if err := encodeInto(entry, keyLastSessionID, sid); err != nil {
			return false, err
		}
		return true, encodeInto(projects, projectKey, entry)
	})
	return err
}
