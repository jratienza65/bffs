package rehome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/imports"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

const (
	sid1 = "0c5e19b2-1111-4222-8333-444455556661"
	sid2 = "0c5e19b2-1111-4222-8333-444455556662"
	sid3 = "0c5e19b2-1111-4222-8333-444455556663"
	sid4 = "0c5e19b2-1111-4222-8333-444455556664"
	day  = 24 * time.Hour
)

// pool is one Claude config dir with a projects/ root, a bffs config dir
// for records and staging, an old project directory that no longer
// exists (its sessions and memory are still in the pool) and a new one
// the sessions are mapped to.
type pool struct {
	cfg, bffs string
	root      transcripts.Root
	old, new  string // normalised
	oldSlug   string
	newSlug   string
}

func newPool(t *testing.T) *pool {
	t.Helper()
	for _, k := range []string{transcripts.EnvClaudeConfigDir, transcripts.EnvProjectDirName, transcripts.EnvRemoteMemoryDir, transcripts.EnvCoworkMemoryPathOverride} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	base := t.TempDir()
	cfg := filepath.Join(base, "claude")
	bffs := filepath.Join(base, "bffs")
	old := filepath.Join(base, "old")
	nw := filepath.Join(base, "new")
	for _, d := range []string{cfg, bffs, old, nw} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldNorm, err := store.NormalizePath(old)
	if err != nil {
		t.Fatal(err)
	}
	newNorm, err := store.NormalizePath(nw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil { // the project moved away
		t.Fatal(err)
	}
	oldSlug, err := transcripts.Slug(oldNorm)
	if err != nil {
		t.Fatal(err)
	}
	newSlug, err := transcripts.Slug(newNorm)
	if err != nil {
		t.Fatal(err)
	}
	return &pool{
		cfg:  cfg,
		bffs: bffs,
		root: transcripts.Root{
			Dir:        filepath.Join(cfg, transcripts.ProjectsSubdir),
			ConfigDir:  cfg,
			ClaudeJSON: filepath.Join(base, ".claude.json"),
		},
		old:     oldNorm,
		new:     newNorm,
		oldSlug: oldSlug,
		newSlug: newSlug,
	}
}

func jsonLine(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// session writes a transcript for sid under slug, recording cwd, with a
// sidecar when sidecar is set, and stamps the transcript mtime.
func (p *pool) session(t *testing.T, slug, sid, cwd string, sidecar bool, mtime time.Time) string {
	t.Helper()
	tr := filepath.Join(p.root.Dir, slug, sid+transcripts.TranscriptExt)
	write(t, tr, jsonLine(map[string]any{"type": "user", "cwd": cwd, "sessionId": sid, "timestamp": "2026-08-01T10:00:00Z", "slug": "plan-" + sid[:8], "message": map[string]any{"role": "user", "content": "prompt " + sid[:8]}})+
		jsonLine(map[string]any{"type": "assistant", "sessionId": sid, "message": map[string]any{"role": "assistant", "content": "ok"}}))
	if sidecar {
		write(t, filepath.Join(p.root.Dir, slug, sid, "tool-results", "out.txt"), "output\n")
	}
	chtimes(t, tr, mtime)
	return tr
}

// seed lays out three sessions of the old directory (sid1 oldest, sid3
// newest) and its memory directory.
func (p *pool) seed(t *testing.T) {
	t.Helper()
	p.session(t, p.oldSlug, sid1, p.old, true, fixedNow.Add(-3*day))
	p.session(t, p.oldSlug, sid2, p.old, true, fixedNow.Add(-2*day))
	p.session(t, p.oldSlug, sid3, p.old, false, fixedNow.Add(-1*day))
	write(t, filepath.Join(p.cfg, transcripts.PlansSubdir, "plan-"+sid1[:8]+".md"), "# plan\n")
	mem := filepath.Join(p.root.Dir, p.oldSlug, transcripts.MemorySubdir)
	write(t, filepath.Join(mem, "MEMORY.md"), "- [notes](notes.md) - notes\n")
	write(t, filepath.Join(mem, "notes.md"), "---\nname: notes\npinned: true\n---\nsee "+p.old+"/x.md and /opt/other\n")
	chtimes(t, filepath.Join(mem, "notes.md"), fixedNow.Add(-30*day))
}

func (p *pool) opts() Options {
	return Options{
		Now:           fixedNow,
		Mtime:         MtimePolicy{CleanupPeriodDays: 30, Now: fixedNow},
		CfgDir:        p.bffs,
		RewriteMemory: true,
	}
}

func (p *pool) maps() []Mapping { return []Mapping{{Old: p.old, New: p.new}} }

func plan(t *testing.T, p *pool, live map[string]transcripts.LiveSession, opts Options) Plan {
	t.Helper()
	pl, err := PlanRehome(context.Background(), p.root, live, p.maps(), opts)
	if err != nil {
		t.Fatalf("PlanRehome: %v", err)
	}
	return pl
}

func moveIDs(moves []Move) []string {
	var ids []string
	for _, m := range moves {
		ids = append(ids, m.SessionID)
	}
	return ids
}

func refusalFor(pl Plan, sid string) string {
	for _, r := range pl.Refusals {
		if r.SessionID == sid {
			return r.Reason
		}
	}
	return ""
}

func TestPlanAndApplyRehome(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	opts := p.opts()
	pl := plan(t, p, nil, opts)
	if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid3, sid2, sid1}) {
		t.Fatalf("moves = %v (refusals %v, warnings %v)", got, pl.Refusals, pl.Warnings)
	}
	for _, mv := range pl.Moves {
		if mv.NewSlug != p.newSlug || mv.NewCwd != p.new || mv.SameSlug || mv.OldCwd != p.old {
			t.Errorf("move = %+v", mv)
		}
		if mv.Sidecar != (mv.SessionID != sid3) {
			t.Errorf("sidecar flag of %s = %v", mv.SessionID, mv.Sidecar)
		}
		if mv.To != filepath.Join(p.root.Dir, p.newSlug, mv.SessionID+".jsonl") {
			t.Errorf("To = %q", mv.To)
		}
		if mv.SessionID == sid1 && len(mv.PlanFiles) != 1 {
			t.Errorf("plan files of sid1 = %v", mv.PlanFiles)
		}
		if mv.Title == "" || mv.LastTS.IsZero() {
			t.Errorf("move lacks title/mtime: %+v", mv)
		}
	}
	if len(pl.Memory) != 1 || pl.Memory[0].From != filepath.Join(p.root.Dir, p.oldSlug, "memory") || pl.Memory[0].To != filepath.Join(p.root.Dir, p.newSlug, "memory") || pl.Memory[0].Mode != MemoryMerge || pl.Memory[0].OldCwd != p.old {
		t.Fatalf("memory = %+v", pl.Memory)
	}
	if err := validateID8(pl.Memory[0].ID8); err != nil {
		t.Errorf("memory id8 = %q", pl.Memory[0].ID8)
	}
	if len(pl.Refusals) != 0 || len(pl.Warnings) != 0 {
		t.Errorf("refusals %v warnings %v", pl.Refusals, pl.Warnings)
	}

	// Dry run writes nothing.
	dry := opts
	dry.DryRun = true
	res, err := Apply(context.Background(), p.root, pl, dry)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Verify) != 1 || !strings.Contains(res.Verify[0], "claude --resume "+sid3) {
		t.Errorf("dry-run verify = %v", res.Verify)
	}
	if exists(filepath.Join(p.root.Dir, p.newSlug)) {
		t.Fatal("dry run wrote")
	}

	res, err = Apply(context.Background(), p.root, pl, opts)
	if err != nil {
		t.Fatalf("Apply: %v (warnings %v)", err, res.Warnings)
	}
	if !reflect.DeepEqual(res.Moved, []string{sid3, sid2, sid1}) {
		t.Errorf("Moved = %v", res.Moved)
	}
	newDir := filepath.Join(p.root.Dir, p.newSlug)
	oldDir := filepath.Join(p.root.Dir, p.oldSlug)
	for i, sid := range []string{sid1, sid2, sid3} {
		tr := filepath.Join(newDir, sid+".jsonl")
		got := readFile(t, tr)
		if !strings.HasSuffix(got, string(RelocatedRecord(sid, p.new))) || strings.Count(got, `"relocated"`) != 1 {
			t.Errorf("%s: stamp missing or doubled:\n%s", sid, got)
		}
		if !strings.HasPrefix(got, `{"cwd":"`) && !strings.Contains(got, `"cwd":`) {
			t.Errorf("%s: head lost", sid)
		}
		want := fixedNow.Add(-time.Duration(3-i) * day)
		if mt := mtimeOf(t, tr); !near(mt, want) {
			t.Errorf("%s mtime = %v, want %v", sid, mt, want)
		}
		if exists(filepath.Join(oldDir, sid+".jsonl")) || exists(filepath.Join(oldDir, sid)) {
			t.Errorf("%s left under the old slug", sid)
		}
		if exists(tr + tmpSuffix) {
			t.Errorf("%s tmp left behind", sid)
		}
	}
	if readFile(t, filepath.Join(newDir, sid1, "tool-results", "out.txt")) != "output\n" || !exists(filepath.Join(newDir, sid2)) || exists(filepath.Join(newDir, sid3)) {
		t.Error("sidecars not moved as planned")
	}
	// Relative order kept: the newest of the three is still sid3, so
	// `claude --continue` picks the same session as before.
	if !mtimeOf(t, filepath.Join(newDir, sid3+".jsonl")).After(mtimeOf(t, filepath.Join(newDir, sid2+".jsonl"))) ||
		!mtimeOf(t, filepath.Join(newDir, sid2+".jsonl")).After(mtimeOf(t, filepath.Join(newDir, sid1+".jsonl"))) {
		t.Error("mtime order changed")
	}
	// The old slug dir holds only memory/ now; the plan file stayed.
	entries, _ := os.ReadDir(oldDir)
	if len(entries) != 1 || entries[0].Name() != "memory" {
		t.Errorf("old dir entries = %v", entries)
	}
	if !exists(filepath.Join(p.cfg, transcripts.PlansSubdir, "plan-"+sid1[:8]+".md")) {
		t.Error("plan file gone")
	}
	// Memory merged into the new dir, old paths rewritten, source kept.
	mem := res.Memory[0]
	if !reflect.DeepEqual(mem.Added, []string{"notes.md", "MEMORY.md"}) || mem.IndexAppended {
		t.Errorf("memory move = %+v", mem)
	}
	notes := readFile(t, filepath.Join(newDir, "memory", "notes.md"))
	if !strings.Contains(notes, "see "+p.new+"/x.md") || strings.Contains(notes, p.old) {
		t.Errorf("paths not rewritten:\n%s", notes)
	}
	if !strings.Contains(notes, "pinned: true") {
		t.Errorf("local memory demoted:\n%s", notes)
	}
	if !reflect.DeepEqual(mem.Rewritten, []string{"notes.md"}) {
		t.Errorf("Rewritten = %v", mem.Rewritten)
	}
	var remaining []string
	for _, r := range mem.Remaining {
		remaining = append(remaining, r.Path)
		if strings.HasPrefix(r.Path, p.old) {
			t.Errorf("old path still mentioned: %+v", r)
		}
	}
	if !strings.Contains(strings.Join(remaining, " "), "/opt/other") {
		t.Errorf("Remaining = %v", remaining)
	}
	if !exists(filepath.Join(oldDir, "memory", "notes.md")) {
		t.Error("old memory removed")
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "left in place") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if len(res.Verify) != 1 || res.Verify[0] != VerifyCommand(p.new, sid3, "") {
		t.Errorf("verify = %v", res.Verify)
	}
	if res.JournalDir != "" {
		t.Errorf("journal kept: %s", res.JournalDir)
	}
	if left, _ := os.ReadDir(filepath.Join(p.bffs, "staging")); len(left) != 0 {
		t.Errorf("staging left: %v", left)
	}
	// The pool now lists the sessions under the new directory.
	ss, err := transcripts.List(context.Background(), p.root, transcripts.ListOptions{Titles: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.Slug != p.newSlug || s.Cwd != p.new || !s.Relocated || s.HeadCwd != p.old {
			t.Errorf("listed session = slug %s cwd %s relocated %v head %s", s.Slug, s.Cwd, s.Relocated, s.HeadCwd)
		}
	}
	// A second plan finds nothing to do.
	pl2 := plan(t, p, nil, opts)
	if len(pl2.Moves) != 0 || len(pl2.Refusals) != 0 {
		t.Errorf("second plan = %+v", pl2)
	}
}

func TestApplyRollsBackOnRenameFailure(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	pl := plan(t, p, nil, p.opts())
	orig := renameFn
	t.Cleanup(func() { renameFn = orig })
	renameFn = func(r *os.Root, oldname, newname string) error {
		if strings.HasSuffix(newname, transcripts.TranscriptExt) && strings.Contains(newname, sid2) {
			return errors.New("injected")
		}
		return orig(r, oldname, newname)
	}
	res, err := Apply(context.Background(), p.root, pl, p.opts())
	if err == nil || !strings.Contains(err.Error(), "injected") || !strings.Contains(err.Error(), "journal kept at") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(res.Moved, []string{sid3}) {
		t.Errorf("Moved = %v", res.Moved)
	}
	oldDir := filepath.Join(p.root.Dir, p.oldSlug)
	newDir := filepath.Join(p.root.Dir, p.newSlug)
	// sid2 is back where it was, unstamped, sidecar included.
	tr := filepath.Join(oldDir, sid2+".jsonl")
	if got := readFile(t, tr); strings.Contains(got, "relocated") {
		t.Errorf("rolled-back transcript keeps the stamp:\n%s", got)
	}
	if mt := mtimeOf(t, tr); !near(mt, fixedNow.Add(-2*day)) {
		t.Errorf("rolled-back mtime = %v", mt)
	}
	if !exists(filepath.Join(oldDir, sid2, "tool-results", "out.txt")) || exists(filepath.Join(newDir, sid2)) {
		t.Error("sidecar not restored")
	}
	for _, name := range []string{sid2 + ".jsonl.bffs-tmp", sid2 + ".bffs-tmp"} {
		if exists(filepath.Join(newDir, name)) {
			t.Errorf("%s left behind", name)
		}
	}
	// sid3 (moved before the failure) stays moved; sid1 was never touched.
	if !exists(filepath.Join(newDir, sid3+".jsonl")) || !exists(filepath.Join(oldDir, sid1+".jsonl")) {
		t.Error("earlier/later sessions disturbed")
	}
	if res.JournalDir == "" || !exists(filepath.Join(res.JournalDir, JournalFile)) {
		t.Errorf("journal not kept: %q", res.JournalDir)
	}
	if len(res.Memory) != 1 || len(res.Memory[0].Added) != 0 {
		t.Errorf("memory merged despite the failure: %+v", res.Memory)
	}
}

// The sidecar moves before the transcript; the transcript is renamed to
// its final name last.
func TestApplyOrderSidecarFirst(t *testing.T) {
	p := newPool(t)
	p.session(t, p.oldSlug, sid1, p.old, true, fixedNow.Add(-day))
	pl := plan(t, p, nil, p.opts())
	orig := renameFn
	t.Cleanup(func() { renameFn = orig })
	var seq []string
	renameFn = func(r *os.Root, oldname, newname string) error {
		seq = append(seq, filepath.ToSlash(filepath.Base(newname)))
		return orig(r, oldname, newname)
	}
	if _, err := Apply(context.Background(), p.root, pl, p.opts()); err != nil {
		t.Fatal(err)
	}
	want := []string{sid1 + ".bffs-tmp", sid1, sid1 + ".jsonl.bffs-tmp", sid1 + ".jsonl"}
	if !reflect.DeepEqual(seq, want) {
		t.Errorf("rename order = %v, want %v", seq, want)
	}
}

func TestApplySameSlugStampsOnly(t *testing.T) {
	p := newPool(t)
	// The transcript already sits under the new directory's entry but
	// records the old cwd (a hand-copied session).
	tr := p.session(t, p.newSlug, sid1, p.old, true, fixedNow.Add(-2*day))
	before := readFile(t, tr)
	pl := plan(t, p, nil, p.opts())
	if len(pl.Moves) != 1 || !pl.Moves[0].SameSlug || pl.Moves[0].NewSlug != p.newSlug {
		t.Fatalf("plan = %+v", pl)
	}
	orig := renameFn
	t.Cleanup(func() { renameFn = orig })
	renameFn = func(r *os.Root, oldname, newname string) error {
		t.Errorf("rename %s → %s on a same-slug move", oldname, newname)
		return orig(r, oldname, newname)
	}
	res, err := Apply(context.Background(), p.root, pl, p.opts())
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tr); got != before+string(RelocatedRecord(sid1, p.new)) {
		t.Errorf("transcript = %q", got)
	}
	if mt := mtimeOf(t, tr); !near(mt, fixedNow.Add(-2*day)) {
		t.Errorf("mtime = %v", mt)
	}
	if !reflect.DeepEqual(res.Moved, []string{sid1}) || len(res.Memory) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestApplyRaisesMtimeToFloor(t *testing.T) {
	p := newPool(t)
	p.session(t, p.oldSlug, sid1, p.old, false, fixedNow.Add(-45*day))
	p.session(t, p.oldSlug, sid2, p.old, false, fixedNow.Add(-40*day))
	opts := p.opts()
	pl := plan(t, p, nil, opts)
	if _, err := Apply(context.Background(), p.root, pl, opts); err != nil {
		t.Fatal(err)
	}
	floor := fixedNow.Add(-15 * day)
	for _, sid := range []string{sid1, sid2} {
		if mt := mtimeOf(t, filepath.Join(p.root.Dir, p.newSlug, sid+".jsonl")); !near(mt, floor) {
			t.Errorf("%s mtime = %v, want floor %v", sid, mt, floor)
		}
	}
	// cleanupPeriodDays 0: preserved.
	p2 := newPool(t)
	p2.session(t, p2.oldSlug, sid1, p2.old, false, fixedNow.Add(-45*day))
	opts = p2.opts()
	opts.Mtime = MtimePolicy{Now: fixedNow}
	pl = plan(t, p2, nil, opts)
	if _, err := Apply(context.Background(), p2.root, pl, opts); err != nil {
		t.Fatal(err)
	}
	if mt := mtimeOf(t, filepath.Join(p2.root.Dir, p2.newSlug, sid1+".jsonl")); !near(mt, fixedNow.Add(-45*day)) {
		t.Errorf("mtime = %v, want preserved", mt)
	}
}

func TestPlanRefusals(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		p := newPool(t)
		p.seed(t)
		live := map[string]transcripts.LiveSession{sid2: {PID: 4242, SessionID: sid2}}
		pl := plan(t, p, live, p.opts())
		if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid3, sid1}) {
			t.Errorf("moves = %v", got)
		}
		if r := refusalFor(pl, sid2); !strings.Contains(r, "open in a running claude (pid 4242)") {
			t.Errorf("refusal = %q", r)
		}
		res, err := Apply(context.Background(), p.root, pl, Options{DryRun: true})
		if err != nil || !reflect.DeepEqual(res.Held, []string{sid2}) || len(res.Skipped) != 0 {
			t.Errorf("held = %v skipped = %v err = %v", res.Held, res.Skipped, err)
		}
	})
	t.Run("target missing", func(t *testing.T) {
		p := newPool(t)
		p.seed(t)
		missing := filepath.Join(t.TempDir(), "gone")
		pl, err := PlanRehome(context.Background(), p.root, nil, []Mapping{{Old: p.old, New: missing}}, p.opts())
		if err != nil {
			t.Fatal(err)
		}
		if len(pl.Moves) != 0 || len(pl.Refusals) != 3 || !strings.Contains(pl.Refusals[0].Reason, fmt.Sprintf("target directory %q does not exist", missing)) {
			t.Errorf("plan = %+v", pl)
		}
		res, _ := Apply(context.Background(), p.root, pl, Options{DryRun: true})
		if len(res.Skipped) != 3 || len(res.Held) != 0 {
			t.Errorf("skipped = %v", res.Skipped)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		p := newPool(t)
		p.seed(t)
		p.session(t, "-elsewhere", sid1, "/elsewhere", false, fixedNow.Add(-5*day))
		pl := plan(t, p, nil, p.opts())
		if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid3, sid2}) {
			t.Errorf("moves = %v", got)
		}
		if r := refusalFor(pl, sid1); !strings.Contains(r, "also exists under projects/-elsewhere") {
			t.Errorf("refusal = %q", r)
		}
	})
	t.Run("incomplete last line", func(t *testing.T) {
		p := newPool(t)
		tr := p.session(t, p.oldSlug, sid1, p.old, false, fixedNow.Add(-day))
		write(t, tr, readFile(t, tr)+`{"type":"assist`)
		chtimes(t, tr, fixedNow.Add(-day))
		pl := plan(t, p, nil, p.opts())
		if len(pl.Moves) != 0 || !strings.Contains(refusalFor(pl, sid1), "incomplete last line") {
			t.Errorf("plan = %+v", pl)
		}
		opts := p.opts()
		opts.ForceStamp = true
		pl = plan(t, p, nil, opts)
		if len(pl.Moves) != 1 {
			t.Fatalf("forced plan = %+v", pl)
		}
		if _, err := Apply(context.Background(), p.root, pl, opts); err != nil {
			t.Fatal(err)
		}
		got := readFile(t, filepath.Join(p.root.Dir, p.newSlug, sid1+".jsonl"))
		if !strings.HasSuffix(got, `{"type":"assist`+"\n"+string(RelocatedRecord(sid1, p.new))) {
			t.Errorf("forced stamp:\n%s", got)
		}
	})
	t.Run("already there", func(t *testing.T) {
		p := newPool(t)
		p.session(t, p.newSlug, sid1, p.new, false, fixedNow.Add(-day))
		pl, err := PlanRehome(context.Background(), p.root, nil, []Mapping{{Old: p.new, New: p.new}}, p.opts())
		if err != nil {
			t.Fatal(err)
		}
		if len(pl.Moves) != 0 || len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "already belongs") {
			t.Errorf("plan = %+v", pl)
		}
	})
	if _, err := PlanRehome(context.Background(), newPool(t).root, nil, nil, Options{}); err == nil || !strings.Contains(err.Error(), "no mapping") {
		t.Errorf("no-mapping err = %v", err)
	}
}

func TestPlanScope(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	// A session of another directory shares the prefix: named by
	// --session, it is a warning rather than silence.
	p.session(t, "-elsewhere", sid4, "/elsewhere", false, fixedNow.Add(-5*day))
	t.Run("sessions", func(t *testing.T) {
		opts := p.opts()
		opts.Sessions = []string{sid1[:8], "deadbeef"}
		pl := plan(t, p, nil, opts)
		// sid1..3 share the first eight characters: the prefix covers all
		// three; the unknown one is a warning.
		if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid3, sid2, sid1}) {
			t.Errorf("moves = %v", got)
		}
		warnings := strings.Join(pl.Warnings, "\n")
		if len(pl.Warnings) != 2 || !strings.Contains(warnings, "session "+sid4[:8]+" belongs to /elsewhere, which no rule covers") || !strings.Contains(warnings, `"deadbeef" not found`) {
			t.Errorf("warnings = %v", pl.Warnings)
		}
		// Not named: no warning about it.
		pl = plan(t, p, nil, p.opts())
		if len(pl.Warnings) != 0 {
			t.Errorf("unscoped warnings = %v", pl.Warnings)
		}
		opts.Sessions = []string{sid2}
		pl = plan(t, p, nil, opts)
		if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid2}) {
			t.Errorf("moves = %v", got)
		}
		opts.Sessions = []string{"zz"}
		if _, err := PlanRehome(context.Background(), p.root, nil, p.maps(), opts); err == nil || !strings.Contains(err.Error(), "invalid session id") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("bundle", func(t *testing.T) {
		rec := imports.Record{
			BundleID: "6f1e2c0a-1111-4222-8333-444455556666", Kind: imports.KindImport, ImportedAt: fixedNow,
			Source: imports.Source{Home: "/Users/other"},
			Sessions: []imports.Session{
				{ID: sid1, OldCwd: p.old, Status: imports.StatusPending},
				{ID: sid2, OldCwd: p.old, Status: imports.StatusSkipped},
			},
			Memories: []imports.Memory{{OldCwd: p.old, Dir: filepath.Join(p.root.Dir, p.oldSlug, "memory"), Status: imports.StatusPending}},
		}
		if err := imports.Save(p.bffs, rec); err != nil {
			t.Fatal(err)
		}
		opts := p.opts()
		opts.BundleID = "6f1e2c0a"
		pl := plan(t, p, nil, opts)
		if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid1}) {
			t.Errorf("moves = %v", got)
		}
		if len(pl.Memory) != 1 || pl.Memory[0].ID8 != "6f1e2c0a" {
			t.Errorf("memory = %+v", pl.Memory)
		}
		opts.BundleID = "deadbeef"
		if _, err := PlanRehome(context.Background(), p.root, nil, p.maps(), opts); err == nil || !strings.Contains(err.Error(), `no import record for bundle "deadbeef"`) {
			t.Errorf("err = %v", err)
		}
		opts.BundleID = "6f1e"
		if _, err := PlanRehome(context.Background(), p.root, nil, p.maps(), opts); err == nil || !strings.Contains(err.Error(), "too short") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestApplySetLastSession(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	opts := p.opts()
	opts.SetLastSession = true
	opts.ClaudeJSON = p.root.ClaudeJSON
	write(t, opts.ClaudeJSON, `{"projects":{},"other":1}`)
	pl := plan(t, p, nil, opts)
	res, err := Apply(context.Background(), p.root, pl, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.LastSessionSet != sid3 {
		t.Errorf("LastSessionSet = %q", res.LastSessionSet)
	}
	key, err := transcripts.ProjectKey(p.new)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := claudejson.ReadProjectFlags(opts.ClaudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	if flags[key].LastSessionID != sid3 {
		t.Errorf("lastSessionId = %q (keys %v)", flags[key].LastSessionID, flags)
	}
	if !strings.Contains(readFile(t, opts.ClaudeJSON), `"other":1`) && !strings.Contains(readFile(t, opts.ClaudeJSON), `"other": 1`) {
		t.Error("other fields lost")
	}
	if exists(opts.ClaudeJSON + ".lock") {
		t.Error("lock left behind")
	}
}

// Recovery is step 0 of a rehome: a sidecar an interrupted run left under
// the target entry — journalled under staging/rehome-*/ — goes back
// before the listing, so the session is planned instead of refused as a
// collision; the consumed journal is removed and the move then succeeds.
func TestPlanRecoversInterruptedRehome(t *testing.T) {
	p := newPool(t)
	p.seed(t)
	oldDir := filepath.Join(p.root.Dir, p.oldSlug)
	newDir := filepath.Join(p.root.Dir, p.newSlug)
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(oldDir, sid1), filepath.Join(newDir, sid1)); err != nil {
		t.Fatal(err)
	}
	jdir := filepath.Join(p.bffs, "staging", "rehome-1")
	if err := os.MkdirAll(jdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteJournal(jdir, Journal{SessionID: sid1, Origin: filepath.Join(oldDir, sid1+".jsonl"), Target: filepath.Join(newDir, sid1+".jsonl"), Step: StepSidecar}); err != nil {
		t.Fatal(err)
	}
	// Without the journal the stray sidecar is a refusal.
	pl, err := PlanRehome(context.Background(), p.root, nil, p.maps(), Options{Now: fixedNow, StagingDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r := refusalFor(pl, sid1); !strings.Contains(r, "already exists") {
		t.Fatalf("plan without the journal = %+v", pl)
	}
	opts := p.opts()
	pl = plan(t, p, nil, opts)
	if got := moveIDs(pl.Moves); !reflect.DeepEqual(got, []string{sid3, sid2, sid1}) || len(pl.Refusals) != 0 {
		t.Fatalf("plan = %+v", pl)
	}
	if len(pl.Warnings) != 1 || !strings.Contains(pl.Warnings[0], "recovered "+filepath.Join(oldDir, sid1)) {
		t.Errorf("warnings = %v", pl.Warnings)
	}
	if exists(jdir) {
		t.Error("consumed journal not removed")
	}
	if _, err := Apply(context.Background(), p.root, pl, opts); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(newDir, sid1, "tool-results", "out.txt")) || !exists(filepath.Join(newDir, sid1+".jsonl")) {
		t.Error("session not moved after recovery")
	}
}

// Memory is keyed by the git root: a session that ran in a subdirectory
// of its old repository keeps its memory under the root's slug, a
// directory the rule covers as well — the walk up from the cwd finds it
// and merges it into the mapped root's memory.
func TestPlanMemoryOfAncestor(t *testing.T) {
	p := newPool(t)
	sub := filepath.Join(p.old, "pkg", "x")
	subSlug, err := transcripts.Slug(sub)
	if err != nil {
		t.Fatal(err)
	}
	p.session(t, subSlug, sid1, sub, false, fixedNow.Add(-day))
	if err := os.MkdirAll(filepath.Join(p.new, "pkg", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	mem := filepath.Join(p.root.Dir, p.oldSlug, transcripts.MemorySubdir)
	write(t, filepath.Join(mem, "MEMORY.md"), "- [notes](notes.md) - notes\n")
	write(t, filepath.Join(mem, "notes.md"), "see "+p.old+"/x.md\n")
	pl := plan(t, p, nil, p.opts())
	if len(pl.Moves) != 1 || pl.Moves[0].NewCwd != filepath.Join(p.new, "pkg", "x") {
		t.Fatalf("moves = %+v (refusals %v)", pl.Moves, pl.Refusals)
	}
	if len(pl.Memory) != 1 || pl.Memory[0].From != mem || pl.Memory[0].OldCwd != p.old || pl.Memory[0].To != filepath.Join(p.root.Dir, p.newSlug, transcripts.MemorySubdir) {
		t.Fatalf("memory = %+v (warnings %v)", pl.Memory, pl.Warnings)
	}
	res, err := Apply(context.Background(), p.root, pl, p.opts())
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(p.root.Dir, p.newSlug, "memory", "notes.md")); got != "see "+p.new+"/x.md\n" {
		t.Errorf("merged memory = %q (warnings %v)", got, res.Warnings)
	}
	// A mapped ancestor that does not exist here is a warning, not a merge:
	// the session's own directory is mapped by one rule, its repository
	// root by another whose target is missing.
	p2 := newPool(t)
	sub2 := filepath.Join(p2.old, "pkg", "x")
	sub2Slug, err := transcripts.Slug(sub2)
	if err != nil {
		t.Fatal(err)
	}
	p2.session(t, sub2Slug, sid1, sub2, false, fixedNow.Add(-day))
	mem2 := filepath.Join(p2.root.Dir, p2.oldSlug, transcripts.MemorySubdir)
	write(t, filepath.Join(mem2, "notes.md"), "x\n")
	maps := []Mapping{{Old: sub2, New: p2.new}, {Old: p2.old, New: filepath.Join(t.TempDir(), "gone")}}
	pl, err = PlanRehome(context.Background(), p2.root, nil, maps, p2.opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Moves) != 1 || len(pl.Memory) != 0 {
		t.Errorf("plan = %+v", pl)
	}
	found := false
	for _, w := range pl.Warnings {
		if strings.Contains(w, "memory of "+p2.old+" not merged") && strings.Contains(w, "does not exist here") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v", pl.Warnings)
	}
}

func TestParentDir(t *testing.T) {
	cases := map[string]string{
		"/a/b/c": "/a/b", "/a/b/": "/a", "/x": "/", "/": "", "": "",
		`C:\Users\jonas\x`: `C:\Users\jonas`, `C:\x`: `C:\`, `C:\`: "", "C:/x": "C:/",
	}
	for in, want := range cases {
		if got := parentDir(in); got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}
