package bundle

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fuzzLimits keep every allocation the reader makes small: the manifest
// buffer is the only size-driven allocation and it is capped here.
var fuzzLimits = Limits{
	MaxTotalBytes:    4 << 20,
	MaxEntryBytes:    1 << 20,
	MaxManifestBytes: 1 << 20,
	MaxEntries:       100,
	MaxFiles:         1000,
}

func FuzzValidateEntryName(f *testing.F) {
	for _, s := range []string{
		"", "../x", "/abs", "a/../../b", "C:\\x", "a\\b", "NUL", "a/NUL/b", ".", "..", "x/./y", "x//y", "x/",
		strings.Repeat("a", 300), "a/" + strings.Repeat("a", 300), "a\x00b", "ünïcode", "a/Ⅼ", "ａ",
		"manifest.json", "projects/" + slugA + "/" + sidA + ".jsonl", longName,
		"projects/memory/" + sidA + ".jsonl", "projects/my_proj/" + sidA + ".jsonl",
		"file-history/" + sidA + "/deadbeefdeadbeef@v2", "plans/quirky-lemur-agent-a1b2c3d.md",
		"plans/quirky-lemur.workshop.md", "history/" + sidA + ".jsonl", "tasks/" + sidA + "/.lock",
		"tasks/" + sidA + "/todo.json", "memory/" + slugA + "/MEMORY.md", "memory/" + slugA + "/proposals/x.md",
		"memory/" + slugA + "/index.json", "claudejson/" + slugA + ".json", "session-env/" + sidA + "/x",
		"user-memory/CLAUDE.md", strings.Repeat("c/", 511) + "dd", "a-b_c.d@e", "x/.hidden",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateEntryName(name)
		kind, sid, slug, cerr := ClassifyName(name)
		if err != nil {
			if cerr == nil {
				t.Fatalf("ClassifyName(%q) accepted what ValidateEntryName rejected: %v", name, err)
			}
			return
		}
		if name == "" || len(name) > maxNameLen || !filepath.IsLocal(name) || path.Clean(name) != name ||
			strings.ContainsAny(name, "\\\x00") || name[0] == '/' {
			t.Fatalf("ValidateEntryName(%q) = nil but a lexical invariant fails", name)
		}
		for i := 0; i < len(name); i++ {
			if !nameByte(name[i]) {
				t.Fatalf("ValidateEntryName(%q) = nil with byte %q", name, name[i])
			}
		}
		for _, c := range strings.Split(name, "/") {
			if c == "" || c == "." || c == ".." || len(c) > maxComponentLen || isWindowsDevice(c) || strings.HasSuffix(c, ".") {
				t.Fatalf("ValidateEntryName(%q) = nil with component %q", name, c)
			}
		}
		if cerr != nil {
			return
		}
		switch kind {
		case NameManifest, NameTranscript, NameSidecar, NameFileHistory, NamePlans, NameHistory, NameTasks, NameMemory, NameReserved:
		default:
			t.Fatalf("ClassifyName(%q) kind %q", name, kind)
		}
		if sid != "" && !isUUID(sid) {
			t.Fatalf("ClassifyName(%q) sid %q", name, sid)
		}
		if slug != "" && checkSlug(slug) != nil {
			t.Fatalf("ClassifyName(%q) slug %q", name, slug)
		}
	})
}

// FuzzUnpack's invariants: Unpack never panics, never writes outside the
// staging root, and either fails or leaves every listed non-reserved file
// staged with the manifest's size and digest. Durability is not one of
// them, so the per-file fsync is stubbed for throughput (each exec stages
// into a fresh temp dir; F_FULLFSYNC on macOS costs milliseconds a file and
// left the minimizer at 0 execs/s).
func FuzzUnpack(f *testing.F) {
	syncFile = func(*os.File) error { return nil }
	f.Cleanup(func() { syncFile = (*os.File).Sync })
	seeds := seedBundles(f)
	names := make([]string, 0, len(seeds))
	for n := range seeds {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f.Add(seeds[n])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		outer, staging := stage(t)
		u, err := Unpack(context.Background(), bytes.NewReader(data), staging, "", fuzzLimits, true, nil)
		walkErr := filepath.WalkDir(outer, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == outer || p == staging || strings.HasPrefix(p, staging+string(filepath.Separator)) {
				return nil
			}
			t.Fatalf("path outside the staging root: %s", p)
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
		if err != nil {
			return
		}
		if u == nil || u.Manifest == nil {
			t.Fatal("nil Unpacked without an error")
		}
		for _, e := range u.Manifest.Entries {
			for _, fl := range e.Files {
				kind, _, _, err := ClassifyName(fl.Path)
				if err != nil {
					t.Fatalf("staged file with a bad name %q: %v", fl.Path, err)
				}
				p := filepath.Join(staging, filepath.FromSlash(fl.Path))
				if kind == NameReserved {
					if _, err := os.Lstat(p); err == nil {
						t.Fatalf("reserved entry %q was written", fl.Path)
					}
					continue
				}
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("listed file %q not staged: %v", fl.Path, err)
				}
				if int64(len(b)) != fl.Size || sha(b) != fl.SHA256 {
					t.Fatalf("listed file %q staged with the wrong bytes", fl.Path)
				}
				if _, ok := u.Files[fl.Path]; !ok {
					t.Fatalf("listed file %q missing from Files", fl.Path)
				}
			}
		}
	})
}
