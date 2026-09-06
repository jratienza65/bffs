package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/transcripts"
	"github.com/jratienza65/bffs/internal/trust"
)

// projectDrift is what differs for one project (tui-v2 §1): across
// roots — how many transcripts each pool holds under the slug and how
// its memory files compare with the reference root's, by sha256 — and
// across accounts — the trust answers and the last-session pointer each
// .claude.json records for the project key.
type projectDrift struct {
	slug, cwd    string
	key, gitRoot string
	ref          transcripts.Root
	roots        []rootDrift
	accounts     []accountDrift
	warnings     []string
	sessions     int       // on the reference root
	newest       time.Time // newest transcript on the reference root
}

// rootDrift is one root's side of the comparison.
type rootDrift struct {
	root      transcripts.Root
	here      bool // the reference root
	sessions  int
	memoryDir string             // "" when the root has no memory for the project
	files     map[string]memFile // by slash-relative name
	state     string             // "here", "same", "differs", "none", "only there"
	diffs     []fileDrift        // the files that differ, by name
}

// memFile is what the comparison keeps of one memory file.
type memFile struct {
	sha   string
	size  int64
	mtime time.Time
}

// fileDrift is one differing file: "differs (newer here)", "differs
// (newer there)", "differs", "only here" or "only there".
type fileDrift struct {
	name, state string
}

// accountDrift is one account's (or home's) .claude.json for the key.
type accountDrift struct {
	status      trust.Status
	lastSession string
	inProject   bool // lastSession names one of the project's sessions on the reference root
}

// memoryHashCap is the largest memory file the comparison hashes; Claude
// itself loads at most 25 000 characters of a memory file.
const memoryHashCap = 4 << 20

// memoryHashes maps every regular file under dir (slash-relative,
// symlinks skipped) to its sha256, size and mtime.
func memoryHashes(dir string) (map[string]memFile, error) {
	out := map[string]memFile{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mf := memFile{size: info.Size(), mtime: info.ModTime()}
		if info.Size() > memoryHashCap {
			mf.sha = "large"
		} else {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return err
			}
			mf.sha = hex.EncodeToString(h.Sum(nil))
		}
		out[filepath.ToSlash(rel)] = mf
		return nil
	})
	return out, err
}

// compareMemory compares other's files with the reference's: the state
// word and the differing files, by name.
func compareMemory(ref, other map[string]memFile) (string, []fileDrift) {
	if other == nil {
		return "none", nil
	}
	if ref == nil {
		return "only there", nil
	}
	names := map[string]bool{}
	for n := range ref {
		names[n] = true
	}
	for n := range other {
		names[n] = true
	}
	var diffs []fileDrift
	for n := range names {
		a, inRef := ref[n]
		b, inOther := other[n]
		switch {
		case inRef && !inOther:
			diffs = append(diffs, fileDrift{n, "only here"})
		case !inRef && inOther:
			diffs = append(diffs, fileDrift{n, "only there"})
		case a.sha != b.sha:
			switch {
			case b.mtime.After(a.mtime):
				diffs = append(diffs, fileDrift{n, "differs (newer there)"})
			case a.mtime.After(b.mtime):
				diffs = append(diffs, fileDrift{n, "differs (newer here)"})
			default:
				diffs = append(diffs, fileDrift{n, "differs"})
			}
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].name < diffs[j].name })
	if len(diffs) == 0 {
		return "same", nil
	}
	return "differs", diffs
}

// computeDrift gathers the comparison for slug/cwd on ref against every
// root of svc and every .claude.json trust sync can address. Nothing
// here opens a transcript: sessions are counted from the directory
// listing, and the reference root's ids feed the last-session check.
func computeDrift(ctx context.Context, svc *services, ref transcripts.Root, slug, cwd string) projectDrift {
	d := projectDrift{slug: slug, cwd: cwd, ref: ref}
	now := svc.now()
	ids := map[string]bool{}
	for _, root := range svc.roots {
		rd := rootDrift{root: root, here: root.Dir == ref.Dir && root.ConfigDir == ref.ConfigDir}
		ss, err := transcripts.List(ctx, root, transcripts.ListOptions{Slug: slug, Now: now})
		if err != nil {
			d.warnings = append(d.warnings, fmt.Sprintf("%s: %v", shortRootLabel(root), err))
		}
		rd.sessions = len(ss)
		if rd.here {
			d.sessions = len(ss)
			for _, s := range ss {
				ids[s.ID] = true
				if s.LastTS.After(d.newest) {
					d.newest = s.LastTS
				}
			}
		}
		if dir := memoryDirFor(root, slug, cwd); dir != "" {
			files, err := memoryHashes(dir)
			if err != nil {
				d.warnings = append(d.warnings, fmt.Sprintf("memory of %s: %v", shortRootLabel(root), err))
			} else {
				rd.memoryDir, rd.files = dir, files
			}
		}
		d.roots = append(d.roots, rd)
	}
	var refFiles map[string]memFile
	haveRef := false
	for i := range d.roots {
		if d.roots[i].here {
			refFiles, haveRef = d.roots[i].files, true
		}
	}
	for i := range d.roots {
		rd := &d.roots[i]
		switch {
		case rd.here:
			rd.state = "here"
		case !haveRef:
			rd.state = "-"
		default:
			rd.state, rd.diffs = compareMemory(refFiles, rd.files)
		}
	}
	if cwd == "" {
		d.warnings = append(d.warnings, "no cwd recorded for this project: per-account state unknown")
		return d
	}
	key, err := transcripts.ProjectKey(cwd)
	if err != nil {
		d.warnings = append(d.warnings, "project key: "+err.Error())
		return d
	}
	d.key = key
	if gr, ok := transcripts.GitRoot(cwd); ok {
		d.gitRoot = gr
	}
	files, err := trust.Files(svc.cfgDir, svc.homeJSON(), svc.accs)
	if err != nil {
		d.warnings = append(d.warnings, "trust: "+err.Error())
		return d
	}
	statuses, err := trust.Report(files, key, d.gitRoot)
	if err != nil {
		d.warnings = append(d.warnings, "trust: "+err.Error())
		return d
	}
	for _, st := range statuses {
		ad := accountDrift{status: st}
		if flags, err := claudejson.ReadProjectFlags(st.File); err == nil {
			ad.lastSession = flags[key].LastSessionID
			ad.inProject = ids[ad.lastSession]
		}
		d.accounts = append(d.accounts, ad)
	}
	return d
}

// answerCell renders a trust answer for a table cell: ✓ accepted,
// ✗ declined, ✓* inherited from an ancestor, – never answered.
func answerCell(a trust.Answer, inheritedFrom string) string {
	switch a {
	case trust.Accepted:
		return "✓"
	case trust.Declined:
		return "✗"
	case trust.Inherited:
		return "✓*"
	}
	if inheritedFrom != "" {
		return "✓*"
	}
	return "–"
}

const (
	driftRootW    = 26
	driftCountW   = 8
	driftAccountW = 11
	driftAnswerW  = 8
)

// memoryCell is the MEMORY column of one root.
func memoryCell(rd rootDrift) string {
	switch rd.state {
	case "here":
		if rd.files == nil {
			return "none"
		}
		return countNoun(len(rd.files), "file")
	case "differs":
		parts := make([]string, 0, len(rd.diffs))
		for _, f := range rd.diffs {
			parts = append(parts, transcripts.Sanitize(f.name)+" "+f.state)
		}
		return "differs — " + strings.Join(parts, ", ")
	case "same":
		return countNoun(len(rd.files), "file") + ", same"
	case "only there":
		return countNoun(len(rd.files), "file") + " only there"
	}
	return rd.state
}

// section renders a preview section header.
func section(title, note string) string {
	s := styleSection.Render(strings.ToUpper(title))
	if note != "" {
		s += "  " + styleFaint.Render(note)
	}
	return s
}

// accountTable renders the per-account rows, marking the perspective
// account (the one selected in panel 1) with an arrow; thisLabel is
// appended to a last-session pointer that names the subject ("(this
// project)", "(this session)").
func accountTable(accounts []accountDrift, perspective, thisLabel string) []string {
	lines := []string{"  " + pad("account", driftAccountW) + "   " + pad("trust", driftAnswerW) + " " + pad("external", driftAnswerW) + " last session"}
	for _, ad := range accounts {
		last := "–"
		if ad.lastSession != "" {
			last = shortID(transcripts.Sanitize(ad.lastSession))
			if ad.inProject {
				last += " " + thisLabel
			}
		}
		mark := "  "
		if ad.status.Account == perspective {
			mark = "← "
		}
		lines = append(lines, "  "+pad(transcripts.Sanitize(ad.status.Account), driftAccountW)+" "+mark+pad(answerCell(ad.status.Folder, ad.status.InheritedFrom), driftAnswerW)+" "+pad(answerCell(ad.status.External, ""), driftAnswerW)+" "+last)
	}
	return lines
}

// driftLines renders the project preview: the headline, a summary,
// the two tables and the actions that address them.
func driftLines(d projectDrift, perspective string, now time.Time) []string {
	label := transcripts.Sanitize(d.slug)
	if d.cwd != "" {
		label = transcripts.Sanitize(shortPath(d.cwd))
	}
	lines := []string{styleHeader.Render(label)}
	summary := countNoun(d.sessions, "session")
	for _, rd := range d.roots {
		if rd.here {
			if rd.files == nil {
				summary += " · no memory"
			} else {
				summary += " · memory " + countNoun(len(rd.files), "file")
			}
		}
	}
	if !d.newest.IsZero() {
		summary += " · newest " + humanizeAgo(d.newest, now)
	}
	if d.gitRoot != "" && d.gitRoot != d.cwd {
		summary += " · git root " + transcripts.Sanitize(shortPath(d.gitRoot))
	}
	if d.cwd != "" && !isDir(d.cwd) {
		summary += " · directory missing here"
	}
	lines = append(lines, summary, "", section("across roots", "reference: "+shortRootLabel(d.ref)),
		"  "+pad("root", driftRootW)+" "+pad("sessions", driftCountW)+" memory")
	for _, rd := range d.roots {
		lines = append(lines, "  "+pad(shortRootLabel(rd.root), driftRootW)+" "+pad(fmt.Sprint(rd.sessions), driftCountW)+" "+memoryCell(rd))
	}
	if len(d.roots) == 1 {
		lines = append(lines, styleFaint.Render("  one root on this machine: every account reads the same files"))
	}
	if len(d.accounts) > 0 {
		lines = append(lines, "", section("per account", "what each .claude.json records · ← selected account · ✓* inherited"))
		lines = append(lines, accountTable(d.accounts, perspective, "(this project)")...)
	}
	for _, w := range d.warnings {
		lines = append(lines, "warning: "+transcripts.Sanitize(w))
	}
	lines = append(lines, "", styleFaint.Render("t trust sync · L last-session pointer · S sync memory to a full-isolation account · c copy sessions"))
	return lines
}
