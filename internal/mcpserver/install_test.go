package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jratienza65/bffs/internal/claudejson"
)

func readServers(t *testing.T, path string) (map[string]json.RawMessage, map[string]json.RawMessage) {
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
			t.Fatalf("parse mcpServers in %s: %v", path, err)
		}
	}
	return doc, servers
}

func TestEntry(t *testing.T) {
	e := Entry("/opt/bffs/bffs", "/cfg")
	if e.Type != "stdio" || e.Command != "/opt/bffs/bffs" {
		t.Errorf("entry: %+v", e)
	}
	want := []string{"mcp", "serve", "--config-dir", "/cfg"}
	if len(e.Args) != len(want) {
		t.Fatalf("args: want %v, got %v", want, e.Args)
	}
	for i := range want {
		if e.Args[i] != want[i] {
			t.Fatalf("args: want %v, got %v", want, e.Args)
		}
	}
}

func TestTargetsWithoutSessionsDir(t *testing.T) {
	homeJSON := filepath.Join(t.TempDir(), claudejson.Filename)
	targets, err := Targets(homeJSON, t.TempDir())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0] != homeJSON {
		t.Errorf("want only homeJSON, got %v", targets)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	homeJSON := filepath.Join(t.TempDir(), claudejson.Filename)
	seed := `{"theme":"dark","mcpServers":{"other":{"type":"stdio","command":"x"}}}`
	if err := os.WriteFile(homeJSON, []byte(seed), 0o600); err != nil {
		t.Fatalf("write home fixture: %v", err)
	}

	cfg := t.TempDir()
	workDir := filepath.Join(cfg, "sessions", "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, claudejson.Filename), []byte(`{"userID":"abc"}`), 0o600); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}
	// A session dir without a .claude.json (crashed login) still gets one.
	if err := os.MkdirAll(filepath.Join(cfg, "sessions", "orphan"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Stray files under sessions/ are not targets.
	if err := os.WriteFile(filepath.Join(cfg, "sessions", "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "bffs")
	written, err := Install(homeJSON, cfg, bin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(written) != 3 {
		t.Fatalf("want 3 files written, got %v", written)
	}

	absCfg, _ := filepath.Abs(cfg)
	for _, target := range written {
		_, servers := readServers(t, target)
		raw, ok := servers[ServerName]
		if !ok {
			t.Errorf("%s: no bffs entry", target)
			continue
		}
		var e claudejson.MCPServer
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if !filepath.IsAbs(e.Command) {
			t.Errorf("%s: command not absolute: %q", target, e.Command)
		}
		if len(e.Args) != 4 || e.Args[2] != "--config-dir" || e.Args[3] != absCfg {
			t.Errorf("%s: args %v (want --config-dir %s)", target, e.Args, absCfg)
		}
	}

	// Home file: sibling server and unknown fields intact.
	doc, servers := readServers(t, homeJSON)
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server lost on install")
	}
	if _, ok := doc["theme"]; !ok {
		t.Error("unknown field lost on install")
	}
	// Session file: its own fields intact.
	sessionDoc, _ := readServers(t, filepath.Join(workDir, claudejson.Filename))
	if _, ok := sessionDoc["userID"]; !ok {
		t.Error("session userID lost on install")
	}

	changed, err := Uninstall(homeJSON, cfg)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if len(changed) != 3 {
		t.Fatalf("want 3 files changed, got %v", changed)
	}
	for _, target := range changed {
		_, servers := readServers(t, target)
		if _, ok := servers[ServerName]; ok {
			t.Errorf("%s: bffs entry still present after uninstall", target)
		}
	}
	_, servers = readServers(t, homeJSON)
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server lost on uninstall")
	}

	changed, err = Uninstall(homeJSON, cfg)
	if err != nil {
		t.Fatalf("Uninstall (again): %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("second uninstall should be a no-op, changed %v", changed)
	}
}
