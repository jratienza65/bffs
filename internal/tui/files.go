package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
)

// artifactRow is one file or directory Claude keeps for a session:
// the transcript, the sidecar and what it holds, file-history, tasks,
// plans (transcripts.Artifacts). Nothing is opened to list them.
type artifactRow struct {
	label  string
	path   string
	isDir  bool
	exists bool
	size   int64
	note   string // "3 subagents", "(absent)"
}

func (r *artifactRow) FilterValue() string { return r.label }

func (r *artifactRow) render(width int) string {
	right := r.note
	if right == "" && r.exists {
		right = formatSize(r.size)
	}
	if !r.exists {
		right = "(absent)"
	}
	labelW := width - 1 - len([]rune(right))
	if labelW < 8 {
		return truncate(r.label, width)
	}
	return pad(transcripts.Sanitize(r.label), labelW) + " " + right
}

// refRow is one absolute path or @-reference found in a memory file.
type refRow struct {
	ref transcripts.PathRef
}

func (r *refRow) FilterValue() string { return r.ref.Path }

func (r *refRow) render(width int) string {
	prefix := ""
	if r.ref.Kind == transcripts.PathKindAt {
		prefix = "@ "
	}
	return truncate(fmt.Sprintf("%s%d: %s", prefix, r.ref.Line, transcripts.Sanitize(r.ref.Path)), width)
}

// artifactKey names panel 4's contents for a session.
func artifactKey(s transcripts.Session) string { return "s:" + s.Path }

// refsKey names panel 4's contents for a memory file.
func refsKey(dir, name string) string { return "m:" + filepath.Join(dir, filepath.FromSlash(name)) }

// loadArtifacts lists a session's artifacts with their sizes.
func loadArtifacts(root transcripts.Root, s transcripts.Session) tea.Cmd {
	return func() tea.Msg {
		msg := filesLoadedMsg{key: artifactKey(s)}
		a := transcripts.ArtifactsFor(root, s)
		add := func(label, path string, dir bool) *artifactRow {
			r := &artifactRow{label: label, path: path, isDir: dir}
			if dir {
				r.size, r.exists = dirSize(path)
			} else if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				r.size, r.exists = info.Size(), true
			}
			msg.rows = append(msg.rows, r)
			return r
		}
		add("transcript "+shortID(s.ID)+".jsonl", a.Transcript, false)
		side := add("sidecar/", a.SidecarDir, true)
		if side.exists {
			entries, _ := os.ReadDir(a.SidecarDir)
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, e := range entries {
				p := filepath.Join(a.SidecarDir, e.Name())
				if e.IsDir() {
					r := add("  "+transcripts.Sanitize(e.Name())+"/", p, true)
					if e.Name() == "subagents" {
						if n := countFiles(p, ".jsonl"); n > 0 {
							r.note = countNoun(n, "subagent")
						}
					}
				} else {
					add("  "+transcripts.Sanitize(e.Name()), p, false)
				}
			}
		}
		add("file-history/", a.FileHistoryDir, true)
		add("tasks/", a.TasksDir, true)
		for _, p := range a.PlanFiles {
			add("plan "+transcripts.Sanitize(filepath.Base(p)), p, false)
		}
		return msg
	}
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

// loadRefs lists the path references of one memory file — the scan
// `bffs memory scan-paths` runs, filtered to that file.
func loadRefs(dir, name string) tea.Cmd {
	return func() tea.Msg {
		msg := filesLoadedMsg{key: refsKey(dir, name)}
		refs, err := transcripts.ScanAbsolutePaths(dir)
		if err != nil {
			msg.err = err
			return msg
		}
		for _, r := range refs {
			if r.File == name {
				msg.rows = append(msg.rows, &refRow{ref: r})
			}
		}
		return msg
	}
}
