package claudejson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), Filename)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func decodeServers(t *testing.T, path string) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	servers := map[string]json.RawMessage{}
	if v, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(v, &servers); err != nil {
			t.Fatalf("parse mcpServers: %v", err)
		}
	}
	return doc, servers
}

func TestSetMCPServerPreservesSiblingsAndUnknownFields(t *testing.T) {
	path := writeFixture(t, `{"theme":"dark","numStartups":5,"mcpServers":{"other":{"type":"stdio","command":"x","args":["y"]}}}`)

	err := SetMCPServer(path, "bffs", MCPServer{Type: "stdio", Command: "/opt/bffs/bffs", Args: []string{"mcp", "serve"}})
	if err != nil {
		t.Fatalf("SetMCPServer: %v", err)
	}

	doc, servers := decodeServers(t, path)
	var theme string
	if err := json.Unmarshal(doc["theme"], &theme); err != nil || theme != "dark" {
		t.Errorf("theme: want %q, got %s (err %v)", "dark", doc["theme"], err)
	}
	var startups int
	if err := json.Unmarshal(doc["numStartups"], &startups); err != nil || startups != 5 {
		t.Errorf("numStartups: want 5, got %s (err %v)", doc["numStartups"], err)
	}
	var other MCPServer
	if err := json.Unmarshal(servers["other"], &other); err != nil {
		t.Fatalf("sibling entry: %v", err)
	}
	if other.Command != "x" || len(other.Args) != 1 || other.Args[0] != "y" {
		t.Errorf("sibling entry mutated: %+v", other)
	}
	var bffs MCPServer
	if err := json.Unmarshal(servers["bffs"], &bffs); err != nil {
		t.Fatalf("bffs entry: %v", err)
	}
	if bffs.Type != "stdio" || bffs.Command != "/opt/bffs/bffs" {
		t.Errorf("bffs entry: %+v", bffs)
	}
}

func TestSetMCPServerCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), Filename)
	if err := SetMCPServer(path, "bffs", MCPServer{Type: "stdio", Command: "b"}); err != nil {
		t.Fatalf("SetMCPServer: %v", err)
	}
	_, servers := decodeServers(t, path)
	if _, ok := servers["bffs"]; !ok {
		t.Error("bffs entry missing from created file")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("created file perm: want 0600, got %o", perm)
		}
	}
}

func TestSetMCPServerIdempotentAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), Filename)
	entry := MCPServer{Type: "stdio", Command: "b", Args: []string{"mcp", "serve"}}
	if err := SetMCPServer(path, "bffs", entry); err != nil {
		t.Fatalf("SetMCPServer: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := SetMCPServer(path, "bffs", entry); err != nil {
		t.Fatalf("SetMCPServer (again): %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("re-set with same entry changed the file:\nfirst:  %s\nsecond: %s", first, second)
	}

	entry.Command = "/moved/bffs"
	if err := SetMCPServer(path, "bffs", entry); err != nil {
		t.Fatalf("SetMCPServer (replace): %v", err)
	}
	_, servers := decodeServers(t, path)
	var got MCPServer
	if err := json.Unmarshal(servers["bffs"], &got); err != nil {
		t.Fatalf("bffs entry: %v", err)
	}
	if got.Command != "/moved/bffs" {
		t.Errorf("replace: want command %q, got %q", "/moved/bffs", got.Command)
	}
}

func TestSetMCPServerRejectsNonObjectMCPServers(t *testing.T) {
	path := writeFixture(t, `{"mcpServers":"corrupt"}`)
	before, _ := os.ReadFile(path)
	if err := SetMCPServer(path, "bffs", MCPServer{Type: "stdio", Command: "b"}); err == nil {
		t.Fatal("SetMCPServer: want error for non-object mcpServers, got nil")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("file modified despite error")
	}
}

func TestSetMCPServerPreservesPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := writeFixture(t, `{}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := SetMCPServer(path, "bffs", MCPServer{Type: "stdio", Command: "b"}); err != nil {
		t.Fatalf("SetMCPServer: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("perm: want 0644, got %o", perm)
	}
}

func TestRemoveMCPServer(t *testing.T) {
	path := writeFixture(t, `{"theme":"dark","mcpServers":{"bffs":{"type":"stdio","command":"b"},"other":{"type":"stdio","command":"x"}}}`)

	removed, err := RemoveMCPServer(path, "bffs")
	if err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	if !removed {
		t.Error("want removed=true")
	}
	doc, servers := decodeServers(t, path)
	if _, ok := servers["bffs"]; ok {
		t.Error("bffs entry still present")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling entry lost")
	}
	if _, ok := doc["theme"]; !ok {
		t.Error("unknown field lost")
	}

	removed, err = RemoveMCPServer(path, "bffs")
	if err != nil {
		t.Fatalf("RemoveMCPServer (absent entry): %v", err)
	}
	if removed {
		t.Error("want removed=false for absent entry")
	}

	removed, err = RemoveMCPServer(filepath.Join(t.TempDir(), Filename), "bffs")
	if err != nil {
		t.Fatalf("RemoveMCPServer (missing file): %v", err)
	}
	if removed {
		t.Error("want removed=false for missing file")
	}
}
