package porter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/bundle"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/transcripts"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

const (
	sidA     = "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e"
	sidB     = "1e005053-380a-4245-a145-52c2715afa73"
	sidC     = "1e005053-aaaa-4bbb-8ccc-ddddeeeeffff"
	planSlug = "frolicking-drifting-pizza"
	day      = 24 * time.Hour
)

// env is one machine's worth of state: a bffs config dir and a Claude
// config dir (its home root), plus a project directory with a .git.
type env struct {
	cfgDir    string
	claudeDir string
	root      transcripts.Root
	cwd       string // normalised project directory
}

// neutralEnv clears every variable that would steer the code under test
// when the tests themselves run inside a bffs-launched claude.
func neutralEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		transcripts.EnvClaudeConfigDir, transcripts.EnvProjectDirName,
		transcripts.EnvRemoteMemoryDir, transcripts.EnvCoworkMemoryPathOverride,
		"BFFS_ACCOUNT", "BFFS_TRANSFER_CODE", "BFFS_NO_USAGE_LOG",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// newEnv builds a fresh machine under a temp HOME. The first call in a
// test also points HOME/USERPROFILE at it.
func newEnv(t *testing.T) *env {
	t.Helper()
	neutralEnv(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	mkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfgDir := filepath.Join(base, "bffs")
	mkdir(t, cfgDir)
	t.Setenv(store.EnvConfigDir, cfgDir)
	claudeDir := filepath.Join(home, ".claude")
	mkdir(t, claudeDir)
	cwd := filepath.Join(base, "proj")
	mkdir(t, filepath.Join(cwd, ".git"))
	norm, err := store.NormalizePath(cwd)
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		cfgDir:    cfgDir,
		claudeDir: claudeDir,
		root: transcripts.Root{
			Dir:        filepath.Join(claudeDir, transcripts.ProjectsSubdir),
			ConfigDir:  claudeDir,
			ClaudeJSON: filepath.Join(home, ".claude.json"),
		},
		cwd: norm,
	}
}

// secondEnv builds another machine beside e (its own bffs and Claude
// config dirs) that shares HOME and the project directory.
func secondEnv(t *testing.T, e *env) *env {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "bffs")
	mkdir(t, cfgDir)
	claudeDir := filepath.Join(base, "claude")
	mkdir(t, claudeDir)
	return &env{
		cfgDir:    cfgDir,
		claudeDir: claudeDir,
		root: transcripts.Root{
			Dir:        filepath.Join(claudeDir, transcripts.ProjectsSubdir),
			ConfigDir:  claudeDir,
			ClaudeJSON: filepath.Join(base, ".claude.json"),
		},
		cwd: e.cwd,
	}
}

func (e *env) slug(t *testing.T) string {
	t.Helper()
	s, err := transcripts.Slug(e.cwd)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) slugDir(t *testing.T) string { return filepath.Join(e.root.Dir, e.slug(t)) }

func (e *env) memDir(t *testing.T) string {
	t.Helper()
	d, err := transcripts.MemoryDirFor(e.root, e.cwd)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func chtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

func near(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < 2*time.Second
}

func rec(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func userRec(sid, cwd, ts, text string) string {
	return rec(map[string]any{
		"type": "user", "cwd": cwd, "sessionId": sid, "timestamp": ts, "version": "2.1.259",
		"gitBranch": "main", "slug": planSlug,
		"message": map[string]any{"role": "user", "content": text},
	})
}

func historyLine(sid, cwd, text string, ts int64) string {
	return rec(map[string]any{"display": text, "pastedContents": map[string]any{}, "project": cwd, "sessionId": sid, "timestamp": ts})
}

// seedPool fills e's root with two sessions of e.cwd (sidB newer than
// sidA), every artifact kind, history lines and a memory directory with a
// pinned topic file. Mtimes: sidA 45 d old, sidB 10 d old, sidA's
// tool-results file 40 d old.
func seedPool(t *testing.T, e *env) {
	t.Helper()
	slugDir := e.slugDir(t)
	trA := filepath.Join(slugDir, sidA+".jsonl")
	writeFile(t, trA, userRec(sidA, e.cwd, "2026-07-10T10:00:00Z", "first prompt A")+
		rec(map[string]any{"type": "ai-title", "sessionId": sidA, "aiTitle": "Title A"}))
	side := filepath.Join(slugDir, sidA)
	writeFile(t, filepath.Join(side, "subagents", "agent-1.jsonl"), rec(map[string]any{"type": "user", "cwd": e.cwd}))
	writeFile(t, filepath.Join(side, "subagents", "agent-1.meta.json"), `{"agentType":"x"}`)
	writeFile(t, filepath.Join(side, "tool-results", "abc.txt"), "saved output\n")
	writeFile(t, filepath.Join(side, "custom-title.json"), `{"title":"custom"}`)
	writeFile(t, filepath.Join(e.claudeDir, transcripts.FileHistorySubdir, sidA, "0123456789abcdef@v1"), "backup-v1")
	writeFile(t, filepath.Join(e.claudeDir, transcripts.PlansSubdir, planSlug+".md"), "# plan\n")
	writeFile(t, filepath.Join(e.claudeDir, transcripts.TasksSubdir, sidA, "1.json"), `{"id":1}`)
	writeFile(t, filepath.Join(e.claudeDir, transcripts.TasksSubdir, sidA, ".lock"), "")
	chtimes(t, filepath.Join(side, "tool-results", "abc.txt"), fixedNow.Add(-40*day))
	chtimes(t, trA, fixedNow.Add(-45*day))

	trB := filepath.Join(slugDir, sidB+".jsonl")
	writeFile(t, trB, userRec(sidB, e.cwd, "2026-08-14T10:00:00Z", "first prompt B"))
	chtimes(t, trB, fixedNow.Add(-10*day))

	writeFile(t, filepath.Join(e.claudeDir, transcripts.HistoryFile),
		historyLine(sidA, e.cwd, "first prompt A", 1752141600000)+
			historyLine(sidC, "/elsewhere", "not ours", 1752141600001)+
			historyLine(sidA, e.cwd, "second prompt A", 1752141600002)+
			historyLine(sidB, e.cwd, "first prompt B", 1755165600000))

	mem := e.memDir(t)
	writeFile(t, filepath.Join(mem, transcripts.MemoryIndexFile), "# Memory Index\n- [topic](topic.md) - notes\n")
	writeFile(t, filepath.Join(mem, "topic.md"), "---\nname: topic\npinned: true\n---\nSee /Users/jonas/build/x\n")
	writeFile(t, filepath.Join(mem, transcripts.MemoryLogsSubdir, "2026", "08", "24", "0f3b2c1e-fix.md"), "log\n")
	writeFile(t, filepath.Join(mem, transcripts.MemoryProposalsSubdir, "p.md"), "never\n")
	writeFile(t, filepath.Join(mem, "index_persist", "x.md"), "never\n")
	writeFile(t, filepath.Join(mem, "notes.txt"), "never\n")
	chtimes(t, filepath.Join(mem, "topic.md"), fixedNow.Add(-20*day))
}

func selectAll(t *testing.T, e *env, parts Parts, live map[string]transcripts.LiveSession) Selection {
	t.Helper()
	sel, err := Select(context.Background(), e.root, nil, live, SelectOptions{Projects: []string{e.cwd}, IncludeLive: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	sel.Parts = parts
	return sel
}

func exportOpts() ExportOptions {
	return ExportOptions{Version: "0.4.0", Account: store.Account{Name: "aviate", Type: store.TypeOAuth}, Isolation: store.IsolationPartial, Now: fixedNow}
}

// exportPool exports e's project (every part, tasks included) to bytes.
func exportPool(t *testing.T, e *env) (*bundle.Manifest, []byte) {
	t.Helper()
	parts := DefaultParts
	parts.Tasks = true
	sel := selectAll(t, e, parts, nil)
	var buf bytes.Buffer
	m, _, err := Export(context.Background(), sel, &buf, exportOpts())
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return m, buf.Bytes()
}

func importOpts(e *env) ImportOptions {
	return ImportOptions{Dest: e.root, Now: fixedNow, Limits: bundle.DefaultLimits}
}

func doImport(t *testing.T, e *env, data []byte, o ImportOptions) Report {
	t.Helper()
	rep, err := Import(context.Background(), e.cfgDir, bytes.NewReader(data), o)
	if err != nil {
		t.Fatalf("Import: %v\nreport: %+v", err, rep)
	}
	return rep
}

// digestTree maps every regular file under dir (slash-relative) to its
// sha256.
func digestTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func containsWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func entryFor(m *bundle.Manifest, kind, sid string) *bundle.Entry {
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Kind == kind && (kind != bundle.EntrySession || e.SessionID == sid) {
			return e
		}
	}
	return nil
}

func filePaths(e *bundle.Entry) []string {
	var out []string
	for _, f := range e.Files {
		out = append(out, f.Path)
	}
	return out
}

func hasPath(e *bundle.Entry, p string) bool {
	for _, f := range e.Files {
		if f.Path == p {
			return true
		}
	}
	return false
}
