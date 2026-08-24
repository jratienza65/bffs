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
	entry, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode server entry: %w", err)
	}
	servers[name] = entry
	return writeDoc(path, doc, servers, perm)
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
	if err := writeDoc(path, doc, servers, perm); err != nil {
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

func writeDoc(path string, doc, servers map[string]json.RawMessage, perm os.FileMode) error {
	enc, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("encode mcpServers: %w", err)
	}
	doc["mcpServers"] = enc
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return atomicWrite(path, out, perm)
}
