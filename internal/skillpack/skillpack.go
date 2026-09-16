// Package skillpack ships the bffs-rehome skill: the SKILL.md (plus its
// references) that teaches a Claude Code session how to rehome sessions and
// auto-memory that bffs imported from another machine or account root.
//
// The skill text is embedded in the bffs binary (single-binary philosophy;
// the text versions with bffs) and dropped into Claude Code's user skills
// dir, <config-dir>/skills/<name>/, by `bffs skill install`. Claude reads
// user skills from the config dir it was launched with, so the install
// mirrors internal/mcpserver's target discipline: the home ~/.claude always,
// plus a real copy for every oauth account under full isolation. Partial
// accounts need nothing — their session dir symlinks skills/ back to
// ~/.claude, so they already see the home copy.
//
// Every SKILL.md bffs writes carries Marker; Install refuses to overwrite a
// same-named skill that lacks it (a user-authored skill) unless forced, and
// Uninstall removes only marker-bearing dirs.
package skillpack

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
)

const (
	// SkillName is the skill's directory name and slash command (/bffs-rehome).
	SkillName = "bffs-rehome"
	// Marker is the HTML comment every bffs-written SKILL.md carries; its
	// absence means a user authored the file.
	Marker = "<!-- managed by bffs -->"
	// SkillsSubdir is the directory under a Claude config dir that holds
	// user skills (Claude reads <config-dir>/skills/<name>/SKILL.md).
	SkillsSubdir = "skills"
	// SkillFile is the file Claude Code reads a skill from.
	SkillFile = "SKILL.md"
	// ChecklistFile is the reference the skill body points at, relative to
	// the skill dir.
	ChecklistFile = "references/rehome-checklist.md"

	assetRoot = "assets/" + SkillName
	// embeddedVersion is the placeholder in the embedded frontmatter that
	// Install replaces with the bffs version.
	embeddedVersion = "version: 0.0.0"
)

// Assets holds the skill text as shipped in the binary. The SKILL.md
// frontmatter carries "version: 0.0.0"; Install stamps the real version.
//
//go:embed assets/bffs-rehome/SKILL.md assets/bffs-rehome/references/*.md
var Assets embed.FS

// SkillDir is where the skill lives under a Claude config dir (~/.claude or
// a per-account session dir).
func SkillDir(claudeDir string) string {
	return filepath.Join(claudeDir, SkillsSubdir, SkillName)
}

// Targets lists every skill dir the install must write: SkillDir(home) —
// homeClaudeDir "" means ~/.claude — plus SkillDir(<sessionDir>) for every
// accounts.toml oauth account whose effective isolation
// (store.ResolveIsolation of the account override and the state default) is
// full. Partial accounts see the home copy through their skills/ symlink and
// are not targets; api_key accounts run claude against ~/.claude. Only
// accounts.toml is consulted, never the sessions/ directory on disk, so an
// orphan session dir is never a target — the one full-isolation account that
// has never been launched still is, and Install creates its dir.
//
// The result is absolute, sorted and deduplicated. Nothing is created here.
func Targets(homeClaudeDir, cfgDir string, accs store.Accounts, state store.State) ([]string, error) {
	if homeClaudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home dir: %w", err)
		}
		homeClaudeDir = filepath.Join(home, ".claude")
	}
	absHome, err := filepath.Abs(homeClaudeDir)
	if err != nil {
		return nil, err
	}
	absCfg, err := filepath.Abs(cfgDir)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var targets []string
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		targets = append(targets, dir)
	}
	add(SkillDir(absHome))
	for _, name := range accs.Names() {
		acc, _ := accs.Get(name)
		if acc.Type != store.TypeOAuth {
			continue
		}
		if store.ResolveIsolation(acc.Isolation, state.Isolation) != store.IsolationFull {
			continue
		}
		add(SkillDir(sessions.Dir(absCfg, name)))
	}
	sort.Strings(targets)
	return targets, nil
}

// renderSkill returns the SKILL.md bytes to install: the embedded text with
// the frontmatter version stamped. An empty version keeps the placeholder.
func renderSkill(version string) ([]byte, error) {
	raw, err := readAsset(SkillFile)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return raw, nil
	}
	end := frontmatterEnd(raw)
	if end < 0 {
		return nil, fmt.Errorf("embedded %s has no frontmatter", SkillFile)
	}
	stamp := []byte("version: " + version)
	front := bytes.Replace(raw[:end], []byte(embeddedVersion), stamp, 1)
	if bytes.Equal(front, raw[:end]) {
		return nil, fmt.Errorf("embedded %s frontmatter lacks %q", SkillFile, embeddedVersion)
	}
	return append(front, raw[end:]...), nil
}

// readAsset reads one file of the skill (path relative to the skill dir,
// slash-separated) and normalises line endings, so a CRLF checkout on
// Windows installs the same bytes as any other platform.
func readAsset(rel string) ([]byte, error) {
	raw, err := Assets.ReadFile(assetRoot + "/" + rel)
	if err != nil {
		return nil, fmt.Errorf("embedded %s: %w", rel, err)
	}
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n")), nil
}

// frontmatterEnd returns the offset just past the frontmatter's opening
// "---" block content — the index of the closing "\n---\n" line — or -1 when
// the document does not start with a frontmatter block.
func frontmatterEnd(doc []byte) int {
	open := []byte("---\n")
	if !bytes.HasPrefix(doc, open) {
		return -1
	}
	i := bytes.Index(doc[len(open):], []byte("\n---\n"))
	if i < 0 {
		return -1
	}
	return len(open) + i
}
