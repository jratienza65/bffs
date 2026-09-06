package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// previewCmd runs build in a command and tags the result with the
// selection key and generation it was built for.
func previewCmd(key string, gen int, build func() ([]string, error)) tea.Cmd {
	return func() tea.Msg {
		lines, err := build()
		return previewLoadedMsg{key: key, gen: gen, lines: lines, err: err}
	}
}

// kvLine lays out one "label: value" line, the value sanitised.
func kvLine(label, value string) string {
	return fmt.Sprintf("%-14s%s", label+":", transcripts.Sanitize(value))
}

// rootPreview describes one root: where it lives, who reads it, its
// retention and how many sessions are live in it.
func rootPreview(svc *services, root transcripts.Root, projects int) func() ([]string, error) {
	return func() ([]string, error) {
		lines := []string{styleHeader.Render(rootLabel(root)), ""}
		lines = append(lines, kvLine("projects dir", shortPath(root.Dir)), kvLine("config dir", shortPath(root.ConfigDir)))
		if root.ClaudeJSON != "" {
			lines = append(lines, kvLine(".claude.json", shortPath(root.ClaudeJSON)))
		}
		switch {
		case root.Orphan:
			lines = append(lines, kvLine("kind", "orphan session dir — no account behind it; read-only source"))
		case root.Owner != "":
			lines = append(lines, kvLine("kind", "full isolation — its own transcripts, memory and .claude.json"))
		case root.Shared:
			lines = append(lines, kvLine("kind", "shared pool — partial-isolation accounts symlink projects/ here"), kvLine("accounts", accountList(root.Accounts)))
		default:
			lines = append(lines, kvLine("kind", "home — the unmanaged ~/.claude"))
		}
		lines = append(lines, kvLine("projects", fmt.Sprint(projects)))
		days, src := transcripts.CleanupPeriodDays(root.ConfigDir)
		if days == 0 {
			lines = append(lines, kvLine("retention", "never swept ("+src+")"))
		} else {
			lines = append(lines, kvLine("retention", fmt.Sprintf("%d days (%s)", days, src)))
		}
		if live, err := transcripts.Live(svc.ctx, []string{root.ConfigDir}); err == nil {
			lines = append(lines, kvLine("live sessions", fmt.Sprint(len(live))))
		}
		lines = append(lines, "", styleFaint.Render("enter opens the projects · i receives a bundle into this root"))
		return lines, nil
	}
}

// driftPreview is the project's two drift tables.
func driftPreview(svc *services, root transcripts.Root, slug, cwd string) func() ([]string, error) {
	return func() ([]string, error) {
		return driftLines(computeDrift(svc.ctx, svc, root, slug, cwd)), nil
	}
}

// sessionPreview is the session's facts, its per-account state and an
// excerpt from the head and tail windows — never the whole transcript.
func sessionPreview(svc *services, s transcripts.Session, resolved bool) func() ([]string, error) {
	return func() ([]string, error) {
		msg, _ := loadDetail(svc, s, resolved)().(showLoadedMsg)
		if msg.err != nil {
			return nil, msg.err
		}
		d := msg.detail
		lines := []string{styleHeader.Render("session " + shortID(s.ID))}
		lines = append(lines, detailLines(d, svc.now())...)
		if d.Session.Cwd != "" {
			if key, err := transcripts.ProjectKey(d.Session.Cwd); err == nil {
				gitRoot, _ := transcripts.GitRoot(d.Session.Cwd)
				if files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs); err == nil {
					if sts, err := trust.Report(files, key, gitRoot); err == nil {
						lines = append(lines, "", styleHeader.Render("per account")+"  "+styleFaint.Render("(t syncs trust · L points an account's last-session here)"))
						lines = append(lines, accountRows(sts, s.ID)...)
					}
				}
			}
		}
		head, _ := transcripts.ReadHead(s.Path)
		tail, _ := transcripts.ReadTail(s.Path)
		lines = append(lines, "", styleHeader.Render("excerpt")+"  "+styleFaint.Render("(enter opens the full transcript)"))
		lines = append(lines, excerptLines(head, tail)...)
		return lines, nil
	}
}

// accountRows renders the trust matrix rows for a session's project,
// marking the accounts whose last-session pointer is this session.
func accountRows(sts []trust.Status, sid string) []string {
	var lines []string
	for _, st := range sts {
		folder := answerCell(st.Folder)
		if st.InheritedFrom != "" {
			folder += "*"
		}
		last := "-"
		if flags, err := readFlags(st.File); err == nil {
			if id := flags[st.ProjectKey]; id != "" {
				last = shortID(transcripts.Sanitize(id))
				if id == sid {
					last += " (this session)"
				}
			}
		}
		lines = append(lines, "  "+pad(transcripts.Sanitize(st.Account), driftAccountW)+" trust "+pad(folder, driftAnswerW)+" external "+pad(answerCell(st.External), driftAnswerW)+" last-session "+last)
	}
	return lines
}

// excerptLines are the prompts and summary the windows carry.
func excerptLines(head transcripts.Head, tail transcripts.Tail) []string {
	var lines []string
	add := func(label, text string) {
		if text != "" {
			lines = append(lines, textLines(pad(label+":", 15), transcripts.Sanitize(text))...)
		}
	}
	add("first prompt", head.FirstPrompt)
	add("summary", tail.Summary)
	add("last prompt", tail.LastPrompt)
	if len(lines) == 0 {
		lines = append(lines, styleFaint.Render("  (the windows carry no prompt text)"))
	}
	return lines
}

// memoryFilePreview is one memory file: who reads it, how the same file
// compares on the other roots, then its contents.
func memoryFilePreview(svc *services, root transcripts.Root, slug, cwd, dir, name string) func() ([]string, error) {
	path := filepath.Join(dir, filepath.FromSlash(name))
	return func() ([]string, error) {
		lines := []string{styleHeader.Render(transcripts.Sanitize(name)) + "  " + styleFaint.Render(shortPath(path)), kvLine("read by", memoryVisibility(root))}
		here, err := memoryHashes(dir)
		if err == nil {
			var drift []string
			for _, other := range svc.roots {
				if other.Dir == root.Dir && other.ConfigDir == root.ConfigDir {
					continue
				}
				od := memoryDirFor(other, slug, cwd)
				state := "none"
				if od != "" {
					if there, err := memoryHashes(od); err == nil {
						other := map[string]memFile{}
						if mf, ok := there[name]; ok {
							other[name] = mf
						}
						_, diffs := compareMemory(map[string]memFile{name: here[name]}, other)
						state = "same"
						for _, d := range diffs {
							if d.name == name {
								state = d.state
							}
						}
					}
				}
				drift = append(drift, shortRootLabel(other)+": "+state)
			}
			if len(drift) == 0 {
				drift = append(drift, "no other root on this machine")
			}
			lines = append(lines, kvLine("drift", strings.Join(drift, " · ")))
		}
		lines = append(lines, "")
		msg, _ := loadFile(path)().(fileLoadedMsg)
		if msg.err != nil {
			return lines, msg.err
		}
		lines = append(lines, msg.lines...)
		if msg.truncated {
			lines = append(lines, styleFaint.Render("… (first 1 MB)"))
		}
		return lines, nil
	}
}

// artifactPreview shows a regular file's head or a directory's entries.
func artifactPreview(r *artifactRow) func() ([]string, error) {
	return func() ([]string, error) {
		lines := []string{styleHeader.Render(transcripts.Sanitize(strings.TrimSpace(r.label))) + "  " + styleFaint.Render(shortPath(r.path))}
		switch {
		case !r.exists:
			lines = append(lines, styleFaint.Render("(absent)"))
		case r.isDir:
			entries, err := os.ReadDir(r.path)
			if err != nil {
				return lines, err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			lines = append(lines, kvLine("entries", fmt.Sprint(len(entries))), "")
			for i, e := range entries {
				if i == 200 {
					lines = append(lines, styleFaint.Render("… (first 200 entries)"))
					break
				}
				name := transcripts.Sanitize(e.Name())
				if e.IsDir() {
					lines = append(lines, "  "+name+"/")
				} else if info, err := e.Info(); err == nil {
					lines = append(lines, "  "+pad(name, 48)+" "+formatSize(info.Size()))
				} else {
					lines = append(lines, "  "+name)
				}
			}
		default:
			lines = append(lines, kvLine("size", formatSize(r.size)), "")
			msg, _ := loadFile(r.path)().(fileLoadedMsg)
			if msg.err != nil {
				return lines, msg.err
			}
			lines = append(lines, msg.lines...)
			if msg.truncated {
				lines = append(lines, styleFaint.Render("… (first 1 MB)"))
			}
		}
		return lines, nil
	}
}

// refPreview shows one path reference and whether it resolves here.
func refPreview(r *refRow) func() ([]string, error) {
	return func() ([]string, error) {
		ref := r.ref
		kind := "absolute path"
		if ref.Kind == transcripts.PathKindAt {
			kind = "@-reference"
		}
		lines := []string{styleHeader.Render(transcripts.Sanitize(ref.Path)), kvLine("kind", kind), kvLine("found in", fmt.Sprintf("%s:%d", ref.File, ref.Line))}
		p := strings.TrimPrefix(ref.Path, "@")
		if strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[2:])
			}
		}
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				lines = append(lines, kvLine("on this machine", "exists"))
			} else {
				lines = append(lines, kvLine("on this machine", "missing — after a rehome, r rewrites memory paths"))
			}
		}
		return lines, nil
	}
}
