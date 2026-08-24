package mcpserver

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/sessions"
)

// Entry is the mcpServers registration for this bffs binary. cfgDir is baked
// in as --config-dir so the server resolves the same config no matter what
// environment Claude Code was launched from (a GUI launch may never have
// sourced the shell that exports BFFS_HOME).
func Entry(binPath, cfgDir string) claudejson.MCPServer {
	return claudejson.MCPServer{
		Type:    "stdio",
		Command: binPath,
		Args:    []string{"mcp", "serve", "--config-dir", cfgDir},
	}
}

// Targets lists every .claude.json that must carry the entry: homeJSON
// (normally ~/.claude.json, where Claude Code reads user-scope mcpServers)
// plus <cfgDir>/sessions/<dir>/.claude.json for every directory under
// sessions/ — each oauth account reads its own copy, never the home one.
// Session dirs are scanned from disk rather than accounts.toml so orphaned or
// renamed dirs are covered; a missing sessions/ dir means homeJSON only.
//
// New oauth accounts need no re-install: `bffs login` seeds their
// .claude.json from the (already patched) home file via
// claudejson.SeedFromHome.
func Targets(homeJSON, cfgDir string) ([]string, error) {
	targets := []string{homeJSON}
	sessionsDir := filepath.Join(cfgDir, sessions.SessionsSubdir)
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return targets, nil
		}
		return nil, fmt.Errorf("read %s: %w", sessionsDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		targets = append(targets, filepath.Join(sessionsDir, e.Name(), claudejson.Filename))
	}
	return targets, nil
}

// Install writes the entry into every target (creating missing files) and
// returns the files written.
func Install(homeJSON, cfgDir, binPath string) ([]string, error) {
	absCfg, err := filepath.Abs(cfgDir)
	if err != nil {
		return nil, err
	}
	absBin, err := filepath.Abs(binPath)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absBin); err == nil {
		absBin = resolved
	}
	targets, err := Targets(homeJSON, absCfg)
	if err != nil {
		return nil, err
	}
	entry := Entry(absBin, absCfg)
	var written []string
	for _, t := range targets {
		if err := claudejson.SetMCPServer(t, ServerName, entry); err != nil {
			return written, err
		}
		written = append(written, t)
	}
	return written, nil
}

// Uninstall removes the entry from every target and returns the files that
// actually changed.
func Uninstall(homeJSON, cfgDir string) ([]string, error) {
	absCfg, err := filepath.Abs(cfgDir)
	if err != nil {
		return nil, err
	}
	targets, err := Targets(homeJSON, absCfg)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, t := range targets {
		removed, err := claudejson.RemoveMCPServer(t, ServerName)
		if err != nil {
			return changed, err
		}
		if removed {
			changed = append(changed, t)
		}
	}
	return changed, nil
}
