package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// artifactLines lists what Claude keeps for a session besides the
// transcript — the sidecar and what it holds, file-history, tasks,
// plans (transcripts.Artifacts) — showing only what exists. Nothing is
// opened.
func artifactLines(root transcripts.Root, s transcripts.Session) []string {
	a := transcripts.ArtifactsFor(root, s)
	var lines []string
	add := func(label, detail, path string) {
		lines = append(lines, "  "+pad(label, 12)+" "+pad(detail, 22)+" "+styleFaint.Render(shortPath(path)))
	}
	if info, err := os.Stat(a.Transcript); err == nil {
		add("transcript", formatSize(info.Size()), a.Transcript)
	}
	if size, ok := dirSize(a.SidecarDir); ok {
		detail := formatSize(size)
		if n := countFiles(filepath.Join(a.SidecarDir, "subagents"), ".jsonl"); n > 0 {
			detail += ", " + countNoun(n, "subagent")
		}
		add("sidecar", detail, a.SidecarDir)
	}
	if size, ok := dirSize(a.FileHistoryDir); ok {
		add("file-history", formatSize(size), a.FileHistoryDir)
	}
	if size, ok := dirSize(a.TasksDir); ok {
		add("tasks", formatSize(size), a.TasksDir)
	}
	for _, p := range a.PlanFiles {
		add("plan", transcripts.Sanitize(filepath.Base(p)), p)
	}
	return lines
}

// countFiles counts the regular files in dir with the suffix.
func countFiles(dir, suffix string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == suffix {
			n++
		}
	}
	return n
}

// refLines lists the path references inside one memory file — the scan
// `bffs memory scan-paths` runs, filtered to that file — and whether
// each absolute path resolves on this machine.
func refLines(dir, name string) []string {
	refs, err := transcripts.ScanAbsolutePaths(dir)
	if err != nil {
		return []string{"  " + styleFaint.Render("scan failed: "+transcripts.Sanitize(err.Error()))}
	}
	var lines []string
	for _, r := range refs {
		if r.File != name {
			continue
		}
		kind := "path"
		if r.Kind == transcripts.PathKindAt {
			kind = "@ref"
		}
		state := ""
		if p := expandRef(r.Path); filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				state = "exists"
			} else {
				state = "missing here"
			}
		}
		lines = append(lines, fmt.Sprintf("  %s %-5d %s  %s", pad(kind, 4), r.Line, transcripts.Sanitize(r.Path), styleFaint.Render(state)))
	}
	return lines
}

// expandRef turns an @-reference or ~ path into a path to stat.
func expandRef(p string) string {
	p = strings.TrimPrefix(p, "@")
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
