package rehome

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/transcripts"
)

const otherSID = "0c5e19b2-2222-4222-8333-444455556666"

func TestWriteJournal(t *testing.T) {
	dir := t.TempDir()
	j := Journal{BundleID: "6f1e2c0a-0000-4000-8000-000000000000", SessionID: testSID, Origin: "/staging/x.jsonl", Target: "/cfg/projects/s/x.jsonl", Step: StepSidecar}
	if err := WriteJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, JournalFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	var got Journal
	if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if got != j {
		t.Errorf("round trip = %+v, want %+v", got, j)
	}
	if !strings.Contains(readFile(t, path), `"bundle_id"`) {
		t.Errorf("json keys are not snake_case: %s", readFile(t, path))
	}
	j.Step = StepDone
	if err := WriteJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	if rj, ok := readJournal(dir); !ok || rj.Step != StepDone {
		t.Errorf("rewritten journal = %+v ok=%v", rj, ok)
	}
	if _, ok := readJournal(""); ok {
		t.Error("empty staging dir must read as no journal")
	}
	if _, ok := readJournal(t.TempDir()); ok {
		t.Error("missing journal must read as absent")
	}
}

func projectsRoot(t *testing.T) (transcripts.Root, string) {
	t.Helper()
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return transcripts.Root{Dir: dir, ConfigDir: cfg}, dir
}

func TestRecoverRestoresLoneTranscript(t *testing.T) {
	root, dir := projectsRoot(t)
	tmp := filepath.Join(dir, testSlug, testSID+".jsonl"+tmpSuffix)
	write(t, tmp, "{}\n")
	restored, kept, err := Recover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, testSlug, testSID+".jsonl")
	if len(restored) != 1 || restored[0] != final {
		t.Errorf("restored = %v, want [%s]", restored, final)
	}
	if len(kept) != 0 {
		t.Errorf("kept = %v", kept)
	}
	if exists(tmp) || readFile(t, final) != "{}\n" {
		t.Errorf("transcript not renamed into place")
	}
}

func TestRecoverKeepsTranscriptWhoseSidExistsElsewhere(t *testing.T) {
	root, dir := projectsRoot(t)
	tmp := filepath.Join(dir, testSlug, testSID+".jsonl"+tmpSuffix)
	write(t, tmp, "{}\n")
	write(t, filepath.Join(dir, "-other-slug", testSID+".jsonl"), "{}\n")
	restored, kept, err := Recover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Errorf("restored = %v", restored)
	}
	if len(kept) != 1 || kept[0] != tmp {
		t.Errorf("kept = %v, want [%s]", kept, tmp)
	}
	if !exists(tmp) {
		t.Errorf("kept tmp was removed")
	}
}

func TestRecoverKeepsCopyInFlightPerJournal(t *testing.T) {
	root, dir := projectsRoot(t)
	staging := t.TempDir()
	origin := filepath.Join(staging, "projects", testSlug, testSID+".jsonl")
	write(t, origin, "{}\n{}\n")
	tmp := filepath.Join(dir, testSlug, testSID+".jsonl"+tmpSuffix)
	write(t, tmp, "{}\n") // a truncated copy
	if err := WriteJournal(staging, Journal{BundleID: "b", SessionID: testSID, Origin: origin, Step: StepTranscript}); err != nil {
		t.Fatal(err)
	}
	restored, kept, err := Recover(root, staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(kept) != 1 || kept[0] != tmp {
		t.Errorf("restored = %v kept = %v", restored, kept)
	}
	if !exists(origin) {
		t.Errorf("staging was cleaned")
	}

	// Origin gone (an in-place rehome moved the original): restore.
	if err := os.Remove(origin); err != nil {
		t.Fatal(err)
	}
	restored, kept, err = Recover(root, staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || len(kept) != 0 {
		t.Errorf("restored = %v kept = %v", restored, kept)
	}
}

func TestRecoverSidecarTmp(t *testing.T) {
	root, dir := projectsRoot(t)
	// With an original: the tmp is redundant and removed.
	write(t, filepath.Join(dir, testSlug, testSID, "subagents", "a.jsonl"), "{}\n")
	redundant := filepath.Join(dir, testSlug, testSID+tmpSuffix+"-deadbeef")
	write(t, filepath.Join(redundant, "subagents", "a.jsonl"), "{}\n")
	// Without an original: renamed into place, before the transcript.
	write(t, filepath.Join(dir, "-other", otherSID+tmpSuffix, "tool-results", "x.txt"), "out\n")
	write(t, filepath.Join(dir, "-other", otherSID+".jsonl"+tmpSuffix), "{}\n")
	// Reserved and set-aside entries are ignored.
	write(t, filepath.Join(dir, "memory", testSID+".jsonl"+tmpSuffix), "ignored\n")
	write(t, filepath.Join(dir, "-old"+replacedInfix+"1", testSID+".jsonl"+tmpSuffix), "ignored\n")

	restored, kept, err := Recover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	wantRestored := []string{
		filepath.Join(dir, "-other", otherSID),
		filepath.Join(dir, "-other", otherSID+".jsonl"),
		redundant,
	}
	sort.Strings(wantRestored)
	if strings.Join(restored, "\n") != strings.Join(wantRestored, "\n") {
		t.Errorf("restored = %v, want %v", restored, wantRestored)
	}
	if len(kept) != 0 {
		t.Errorf("kept = %v", kept)
	}
	if exists(redundant) {
		t.Errorf("redundant sidecar tmp not removed")
	}
	if !exists(filepath.Join(dir, testSlug, testSID, "subagents", "a.jsonl")) {
		t.Errorf("original sidecar touched")
	}
	if !exists(filepath.Join(dir, "-other", otherSID, "tool-results", "x.txt")) {
		t.Errorf("sidecar tmp not renamed into place")
	}
	if !exists(filepath.Join(dir, "memory", testSID+".jsonl"+tmpSuffix)) || !exists(filepath.Join(dir, "-old"+replacedInfix+"1", testSID+".jsonl"+tmpSuffix)) {
		t.Errorf("entries under reserved or set-aside slugs were touched")
	}
}

func TestRecoverMissingRootAndNothingToDo(t *testing.T) {
	root := transcripts.Root{Dir: filepath.Join(t.TempDir(), "nope")}
	if restored, kept, err := Recover(root, ""); err != nil || restored != nil || kept != nil {
		t.Errorf("missing root: %v %v %v", restored, kept, err)
	}
	root, dir := projectsRoot(t)
	write(t, filepath.Join(dir, testSlug, testSID+".jsonl"), "{}\n")
	if restored, kept, err := Recover(root, ""); err != nil || restored != nil || kept != nil {
		t.Errorf("clean root: %v %v %v", restored, kept, err)
	}
}

func TestClassifyTmp(t *testing.T) {
	cases := []struct {
		name   string
		isDir  bool
		ok     bool
		target string
	}{
		{testSID + ".jsonl" + tmpSuffix, false, true, testSID + ".jsonl"},
		{testSID + ".jsonl" + tmpSuffix + "-ab12", false, true, testSID + ".jsonl"},
		{testSID + ".jsonl" + tmpSuffix, true, false, ""},
		{testSID + tmpSuffix, true, true, testSID},
		{testSID + tmpSuffix + "-ab12", true, true, testSID},
		{testSID + tmpSuffix, false, false, ""},
		{"notes" + tmpSuffix, true, false, ""},
		{testSID + ".jsonl", false, false, ""},
		{testSID + ".jsonl" + replacedInfix + "1", false, false, ""},
	}
	for _, tc := range cases {
		got, ok := classifyTmp(tc.name, tc.isDir)
		if ok != tc.ok || got.target != tc.target {
			t.Errorf("classifyTmp(%q, dir=%v) = %+v ok=%v, want ok=%v target=%q", tc.name, tc.isDir, got, ok, tc.ok, tc.target)
		}
	}
}

// An interrupted in-place rehome journals the transcript's old path as
// Origin. While that file is still there the session has not moved, so
// the sidecar the run had already taken goes back beside it — from its
// tmp name or from the target entry — and is never removed; once the
// transcript has moved on, a tmp transcript is restored under the target
// entry like any other and the sidecar stays with it.
func TestRecoverInPlaceRehomeSidecar(t *testing.T) {
	root, dir := projectsRoot(t)
	origin := filepath.Join(dir, "-old", testSID+".jsonl")
	target := filepath.Join(dir, "-new", testSID+".jsonl")
	journal := t.TempDir()
	j := Journal{SessionID: testSID, Origin: origin, Target: target, Step: StepSidecar}
	if err := WriteJournal(journal, j); err != nil {
		t.Fatal(err)
	}
	oldSide := filepath.Join(dir, "-old", testSID)
	newSide := filepath.Join(dir, "-new", testSID)

	// Crash after the sidecar reached its tmp name under the target entry.
	write(t, origin, "{}\n")
	write(t, filepath.Join(newSide+tmpSuffix, "tool-results", "x.txt"), "out\n")
	restored, kept, err := Recover(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != oldSide || len(kept) != 0 {
		t.Errorf("tmp case: restored = %v kept = %v", restored, kept)
	}
	if !exists(filepath.Join(oldSide, "tool-results", "x.txt")) || exists(newSide+tmpSuffix) {
		t.Error("sidecar tmp not moved back beside its transcript")
	}

	// Crash after the sidecar reached the target entry itself.
	if err := os.Rename(oldSide, newSide); err != nil {
		t.Fatal(err)
	}
	restored, kept, err = Recover(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != oldSide || len(kept) != 0 {
		t.Errorf("target case: restored = %v kept = %v", restored, kept)
	}
	if !exists(filepath.Join(oldSide, "tool-results", "x.txt")) || exists(newSide) {
		t.Error("sidecar not moved back from the target entry")
	}

	// Something already at the old place: the tmp is kept, never removed.
	write(t, filepath.Join(newSide+tmpSuffix, "tool-results", "y.txt"), "out\n")
	restored, kept, err = Recover(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(kept) != 1 || kept[0] != newSide+tmpSuffix {
		t.Errorf("occupied case: restored = %v kept = %v", restored, kept)
	}
	if !exists(filepath.Join(newSide+tmpSuffix, "tool-results", "y.txt")) {
		t.Error("sidecar tmp removed although the old entry was occupied")
	}
	if err := os.RemoveAll(newSide + tmpSuffix); err != nil {
		t.Fatal(err)
	}

	// The transcript has moved on (Origin gone, a tmp transcript under the
	// target): restored there; the sidecar already under the target stays.
	if err := os.Rename(oldSide, newSide); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(origin, target+tmpSuffix); err != nil {
		t.Fatal(err)
	}
	j.Step = StepTranscript
	if err := WriteJournal(journal, j); err != nil {
		t.Fatal(err)
	}
	restored, kept, err = Recover(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != target || len(kept) != 0 {
		t.Errorf("moved-on case: restored = %v kept = %v", restored, kept)
	}
	if !exists(target) || !exists(filepath.Join(newSide, "tool-results", "x.txt")) {
		t.Error("transcript not restored under the target entry, or sidecar disturbed")
	}

	// A finished journal asks for nothing, whatever is on disk.
	write(t, origin, "{}\n")
	j.Step = StepDone
	if err := WriteJournal(journal, j); err != nil {
		t.Fatal(err)
	}
	if restored, kept, err := Recover(root, journal); err != nil || len(restored) != 0 || len(kept) != 0 {
		t.Errorf("done case: restored = %v kept = %v err = %v", restored, kept, err)
	}
	if !exists(newSide) {
		t.Error("finished journal moved the sidecar")
	}
}

func TestInsideDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects")
	cases := map[string]bool{
		filepath.Join(dir, "-x", "a.jsonl"): true,
		filepath.Join(dir, "a"):             true,
		dir:                                 false,
		filepath.Dir(dir):                   false,
		filepath.Join(dir, "..", "other"):   false,
		filepath.Join(dir+"-more", "a"):     false,
		"":                                  false,
	}
	for p, want := range cases {
		if got := insideDir(p, dir); got != want {
			t.Errorf("insideDir(%q, %q) = %v, want %v", p, dir, got, want)
		}
	}
}
