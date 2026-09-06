package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jratienza65/bffs/internal/claudejson"
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

// kvLine lays out one "label  value" detail line, the value sanitised.
func kvLine(label, value string) string {
	return "  " + pad(label, 12) + " " + transcripts.Sanitize(value)
}

// accountPreview describes one perspective: the account, the pool it
// browses, and what its .claude.json records.
func accountPreview(svc *services, r *accountRow) func() ([]string, error) {
	return func() ([]string, error) {
		lines := []string{styleHeader.Render(transcripts.Sanitize(r.name))}
		summary := r.kind
		switch r.kind {
		case "partial", "full":
			summary = "oauth · " + r.kind + " isolation"
		case "api key":
			summary = "api key · runs against the unmanaged ~/.claude"
		case "home":
			summary = "the unmanaged ~/.claude — no bffs account reads it"
		case "orphan":
			summary = "orphan session dir — no account behind it; read-only"
		}
		if r.active {
			summary += " · active"
		}
		if acc, ok := svc.accs.Get(r.name); ok && acc.Email != "" {
			summary += " · " + transcripts.Sanitize(acc.Email)
		}
		lines = append(lines, summary, "")
		root := r.root
		lines = append(lines, section("pool", ""), kvLine("root", shortRootLabel(root)), kvLine("projects dir", shortPath(root.Dir)), kvLine("config dir", shortPath(root.ConfigDir)))
		days, src := transcripts.CleanupPeriodDays(root.ConfigDir)
		if days == 0 {
			lines = append(lines, kvLine("retention", "never swept ("+src+")"))
		} else {
			lines = append(lines, kvLine("retention", fmt.Sprintf("%d days (%s)", days, src)))
		}
		if live, err := transcripts.Live(svc.ctx, []string{root.ConfigDir}); err == nil {
			lines = append(lines, kvLine("live sessions", fmt.Sprint(len(live))))
		}
		if file := accountJSON(svc, r); file != "" {
			lines = append(lines, "", section("recorded in its .claude.json", shortPath(file)))
			if flags, err := claudejson.ReadProjectFlags(file); err == nil {
				trusted, pointers := 0, 0
				for _, f := range flags {
					if f.TrustAccepted != nil && *f.TrustAccepted {
						trusted++
					}
					if f.LastSessionID != "" {
						pointers++
					}
				}
				lines = append(lines, kvLine("projects", fmt.Sprint(len(flags))), kvLine("trusted", fmt.Sprint(trusted)), kvLine("last sessions", fmt.Sprint(pointers)))
			} else {
				lines = append(lines, kvLine("projects", "(no file yet)"))
			}
		}
		hint := "enter opens its projects · i receives a bundle into its pool"
		if r.kind == "partial" || r.kind == "full" || r.kind == "api key" {
			hint = "space makes it the active account · " + hint
		}
		lines = append(lines, "", styleFaint.Render(hint))
		return lines, nil
	}
}

// accountJSON is the .claude.json of a perspective, "" for orphans.
func accountJSON(svc *services, r *accountRow) string {
	if r.kind == "orphan" {
		return ""
	}
	files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs)
	if err != nil {
		return ""
	}
	if f, ok := files[r.name]; ok {
		return f
	}
	if r.kind == "api key" || r.kind == "home" {
		return files[transcripts.HomeName]
	}
	return ""
}

// driftPreview is the project's summary and its two drift tables.
func driftPreview(svc *services, root transcripts.Root, slug, cwd, perspective string) func() ([]string, error) {
	return func() ([]string, error) {
		return driftLines(computeDrift(svc.ctx, svc, root, slug, cwd), perspective, svc.now()), nil
	}
}

// sessionPreview leads with what the session is about — title, a
// one-line summary, the resume command, the excerpt from the head and
// tail windows — then the per-account row, the files Claude keeps for
// it and the identifiers. Never the whole transcript.
func sessionPreview(svc *services, s transcripts.Session, resolved bool, perspective string) func() ([]string, error) {
	return func() ([]string, error) {
		msg, _ := loadDetail(svc, s, resolved)().(showLoadedMsg)
		if msg.err != nil {
			return nil, msg.err
		}
		d := msg.detail
		s := d.Session
		now := svc.now()
		title := transcripts.Sanitize(s.Title)
		if title == "" {
			title = "(untitled) " + shortID(s.ID)
		}
		lines := []string{styleHeader.Render(title)}
		var parts []string
		if s.Account != "" {
			parts = append(parts, transcripts.Sanitize(s.Account))
		} else {
			parts = append(parts, "account unknown")
		}
		switch {
		case s.Live && d.LivePID > 0:
			parts = append(parts, fmt.Sprintf("live (pid %d)", d.LivePID))
		case sessionState(s) != "":
			parts = append(parts, sessionState(s))
		}
		parts = append(parts, humanizeAgo(s.LastTS, now), formatSize(s.Size))
		if s.GitBranch != "" {
			parts = append(parts, transcripts.Sanitize(s.GitBranch))
		}
		switch {
		case s.Cwd == "":
			parts = append(parts, "no cwd recorded")
		case s.CwdExists:
			parts = append(parts, shortPath(s.Cwd))
		default:
			parts = append(parts, shortPath(s.Cwd)+" (missing here)")
		}
		lines = append(lines, strings.Join(parts, " · "), styleFaint.Render("resume  "+resumeLine(s)))

		head, _ := transcripts.ReadHead(s.Path)
		tail, _ := transcripts.ReadTail(s.Path)
		lines = append(lines, "", section("excerpt", "enter opens the full transcript"))
		lines = append(lines, excerptLines(head, tail)...)

		if s.Cwd != "" {
			if key, err := transcripts.ProjectKey(s.Cwd); err == nil {
				gitRoot, _ := transcripts.GitRoot(s.Cwd)
				if files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs); err == nil {
					if sts, err := trust.Report(files, key, gitRoot); err == nil {
						var rows []accountDrift
						for _, st := range sts {
							ad := accountDrift{status: st}
							if flags, err := readFlags(st.File); err == nil {
								ad.lastSession = flags[key]
								ad.inProject = ad.lastSession == s.ID
							}
							rows = append(rows, ad)
						}
						lines = append(lines, "", section("per account", "t trust sync · L point a last session here · ← selected account"))
						lines = append(lines, accountTable(rows, perspective, "(this session)")...)
					}
				}
			}
		}
		if fl := artifactLines(s.Root, s); len(fl) > 0 {
			lines = append(lines, "", section("files", ""))
			lines = append(lines, fl...)
		}
		lines = append(lines, "", section("details", ""), kvLine("id", s.ID), kvLine("root", shortRootLabel(s.Root)+"  "+shortPath(s.Root.Dir)))
		if !s.FirstTS.IsZero() {
			first := s.FirstTS.Local().Format("2006-01-02 15:04") + " (" + humanizeAgo(s.FirstTS, now) + ")"
			if s.Version != "" {
				first += " · claude " + s.Version
			}
			lines = append(lines, kvLine("started", first))
		}
		if s.Account != "" {
			attr := s.AttribSource
			if len(d.Claimants) > 0 {
				attr += " · pointer of " + transcripts.Sanitize(strings.Join(d.Claimants, ", "))
			}
			lines = append(lines, kvLine("attribution", attr))
		} else {
			lines = append(lines, kvLine("attribution", "unknown: no launch-log, lastSessionId or import evidence"))
		}
		if s.Relocated {
			lines = append(lines, kvLine("started in", shortPath(s.HeadCwd)+" (relocated since)"))
		}
		if s.Import != nil && s.Import.Record != nil {
			r := s.Import.Record
			src := transcripts.Sanitize(r.Source.Hostname)
			if r.Source.User != "" || r.Source.Home != "" {
				src += fmt.Sprintf(" (%s, %s)", transcripts.Sanitize(r.Source.User), transcripts.Sanitize(r.Source.Home))
			}
			acct := r.Account
			if acct == "" {
				acct = transcripts.HomeName
			}
			kind := r.Kind
			if !r.ImportedAt.IsZero() {
				kind += " " + r.ImportedAt.Local().Format("2006-01-02")
			}
			line := fmt.Sprintf("bundle %s (%s) from %s, account %s", transcripts.Sanitize(r.BundleID), kind, src, transcripts.Sanitize(acct))
			if s.Import.Session != nil {
				line += ", status " + transcripts.Sanitize(s.Import.Session.Status)
			}
			lines = append(lines, kvLine("import", line))
			if s.Import.Session != nil && s.Import.Session.OldCwd != "" {
				lines = append(lines, kvLine("old cwd", s.Import.Session.OldCwd))
			}
		}
		return lines, nil
	}
}

// excerptLines are the prompts and summary the windows carry.
func excerptLines(head transcripts.Head, tail transcripts.Tail) []string {
	var lines []string
	add := func(label, text string) {
		if text != "" {
			lines = append(lines, textLines("  "+pad(label, 15), transcripts.Sanitize(text))...)
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
// compares on the other roots, its path references, then its contents.
func memoryFilePreview(svc *services, root transcripts.Root, slug, cwd, dir, name string) func() ([]string, error) {
	path := joinName(dir, name)
	return func() ([]string, error) {
		lines := []string{styleHeader.Render(transcripts.Sanitize(name)) + "  " + styleFaint.Render(shortPath(path)), kvLine("read by", memoryVisibility(root))}
		if here, err := memoryHashes(dir); err == nil {
			var drift []string
			for _, other := range svc.roots {
				if other.Dir == root.Dir && other.ConfigDir == root.ConfigDir {
					continue
				}
				state := "none"
				if od := memoryDirFor(other, slug, cwd); od != "" {
					if there, err := memoryHashes(od); err == nil {
						otherFiles := map[string]memFile{}
						if mf, ok := there[name]; ok {
							otherFiles[name] = mf
						}
						_, diffs := compareMemory(map[string]memFile{name: here[name]}, otherFiles)
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
		if refs := refLines(dir, name); len(refs) > 0 {
			lines = append(lines, "", section("references", "absolute paths and @-refs inside the file"))
			lines = append(lines, refs...)
		}
		lines = append(lines, "", section("content", ""))
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
