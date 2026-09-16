package claudejson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// MCPServer is one entry in the top-level mcpServers map of .claude.json, in
// the shape Claude Code expects for a stdio server.
type MCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SetMCPServer inserts or replaces mcpServers[name] in the .claude.json at
// path. Every other top-level field — and every sibling server entry — is
// preserved verbatim. Creates the file (0600) if missing; preserves existing
// permissions otherwise.
func SetMCPServer(path, name string, s MCPServer) error {
	doc, perm, err := readDoc(path)
	if err != nil {
		return err
	}
	servers, err := serversOf(doc, path)
	if err != nil {
		return err
	}
	if err := encodeInto(servers, name, s); err != nil {
		return err
	}
	return writeServers(path, doc, servers, perm)
}

// RemoveMCPServer deletes mcpServers[name] at path. A missing file or missing
// entry returns (false, nil). An emptied mcpServers map is left in place.
func RemoveMCPServer(path, name string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	doc, perm, err := readDoc(path)
	if err != nil {
		return false, err
	}
	if _, ok := doc["mcpServers"]; !ok {
		return false, nil
	}
	servers, err := serversOf(doc, path)
	if err != nil {
		return false, err
	}
	if _, ok := servers[name]; !ok {
		return false, nil
	}
	delete(servers, name)
	if err := writeServers(path, doc, servers, perm); err != nil {
		return false, err
	}
	return true, nil
}

// readDoc loads path as a top-level raw-JSON map, mirroring Patch: a missing
// file yields an empty doc with 0600 perms, an existing file keeps its perms.
func readDoc(path string) (map[string]json.RawMessage, os.FileMode, error) {
	var (
		doc              = map[string]json.RawMessage{}
		perm os.FileMode = 0o600
	)
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", path, err)
		}
		if info, err := os.Stat(path); err == nil {
			perm = info.Mode().Perm()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	return doc, perm, nil
}

// serversOf extracts the mcpServers map as raw JSON so sibling entries
// round-trip untouched. A present-but-non-object value is an error — never
// clobber a field claude wrote in a shape we don't understand.
func serversOf(doc map[string]json.RawMessage, path string) (map[string]json.RawMessage, error) {
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, fmt.Errorf("%s: mcpServers is not a JSON object: %w", path, err)
		}
	}
	return servers, nil
}

// writeServers stores servers as the top-level mcpServers field and writes
// the document back.
func writeServers(path string, doc, servers map[string]json.RawMessage, perm os.FileMode) error {
	if err := encodeInto(doc, "mcpServers", servers); err != nil {
		return err
	}
	return writeTop(path, doc, perm)
}

// encodeInto marshals v and stores it under key in a raw-JSON map — the one
// step every writer uses to put a decoded value back into a document or a
// project entry without touching its siblings.
func encodeInto(m map[string]json.RawMessage, key string, v any) error {
	enc, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	m[key] = enc
	return nil
}

// writeTop writes a whole top-level document back to path atomically with
// the given permissions. Untouched fields are json.RawMessage and so survive
// verbatim in value; the file is re-indented with sorted keys, which Claude
// tolerates.
func writeTop(path string, doc map[string]json.RawMessage, perm os.FileMode) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return atomicWrite(path, out, perm)
}
