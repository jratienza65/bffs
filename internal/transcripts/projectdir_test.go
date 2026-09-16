package transcripts

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/store"
)

func TestDecodeCwd(t *testing.T) {
	dir := t.TempDir()
	if _, err := DecodeCwd(dir); err == nil {
		t.Error("an empty slug dir must be an error")
	}
	if _, err := DecodeCwd(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing slug dir must be an error")
	}

	old := filepath.Join(dir, sidA+".jsonl")
	writeTranscript(t, old, userRec(sidA, "/Users/a/old", "2026-08-01T09:00:00Z", "x"))
	touchAt(t, old, fixedNow.Add(-48*time.Hour))
	newer := filepath.Join(dir, sidB+".jsonl")
	writeTranscript(t, newer,
		userRec(sidB, "/Users/a/new", "2026-08-24T09:00:00Z", "y"),
		rec(map[string]any{"type": "relocated", "sessionId": sidB, "relocatedCwd": "/Users/a/moved"}),
	)
	touchAt(t, newer, fixedNow.Add(-1*time.Hour))
	writeFile(t, filepath.Join(dir, "notes.jsonl"), "{\"cwd\":\"/Users/a/ignored\"}\n")

	got, err := DecodeCwd(dir)
	if err != nil || got != "/Users/a/moved" {
		t.Errorf("DecodeCwd = (%q, %v), want the newest transcript's effective cwd", got, err)
	}

	// The newest transcript being empty falls back to the next one.
	empty := filepath.Join(dir, sidC+".jsonl")
	writeFile(t, empty, "")
	touchAt(t, empty, fixedNow)
	got, err = DecodeCwd(dir)
	if err != nil || got != "/Users/a/moved" {
		t.Errorf("DecodeCwd with an empty newest = (%q, %v)", got, err)
	}
}

func TestProjectDirFor(t *testing.T) {
	cfg := t.TempDir()
	root := Root{Dir: filepath.Join(cfg, ProjectsSubdir), ConfigDir: cfg}
	cwd := t.TempDir()
	norm, err := store.NormalizePath(cwd)
	if err != nil {
		t.Fatal(err)
	}
	slug, err := Slug(norm)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("env override", func(t *testing.T) {
		env := []string{"PATH=/bin", EnvClaudeConfigDir + "=" + cfg, EnvProjectDirName + "=my_proj-1"}
		got, err := ProjectDirFor(root, cwd, env)
		if err != nil || got != filepath.Join(root.Dir, "my_proj-1") {
			t.Errorf("ProjectDirFor = (%q, %v)", got, err)
		}
		for _, bad := range []string{"", "has space", "dots.md", "../x", strings.Repeat("a", 65)} {
			got, err := ProjectDirFor(root, cwd, []string{EnvClaudeConfigDir + "=" + cfg, EnvProjectDirName + "=" + bad})
			if err != nil || got != filepath.Join(root.Dir, slug) {
				t.Errorf("invalid name %q: (%q, %v), want the slug fallback", bad, got, err)
			}
		}
		// Claude ignores the name without CLAUDE_CONFIG_DIR.
		got, err = ProjectDirFor(root, cwd, []string{EnvProjectDirName + "=my_proj-1"})
		if err != nil || got != filepath.Join(root.Dir, slug) {
			t.Errorf("without CLAUDE_CONFIG_DIR = (%q, %v), want the slug fallback", got, err)
		}
	})

	t.Run("slug fallback on an empty root", func(t *testing.T) {
		got, err := ProjectDirFor(root, cwd, nil)
		if err != nil || got != filepath.Join(root.Dir, slug) {
			t.Errorf("ProjectDirFor = (%q, %v), want %s", got, err, filepath.Join(root.Dir, slug))
		}
	})

	t.Run("scanned match under another name", func(t *testing.T) {
		// A directory Claude named through CLAUDE_CODE_PROJECT_DIR_NAME
		// (or a hashed long slug): its newest transcript records cwd.
		alias := filepath.Join(root.Dir, "my_proj")
		writeTranscript(t, filepath.Join(alias, sidA+".jsonl"), userRec(sidA, cwd, "2026-08-24T10:00:00Z", "x"))
		// Decoys: a reserved entry, a dir for another cwd, and a dir
		// whose relocation points here only in an OLD transcript.
		writeTranscript(t, filepath.Join(root.Dir, "memory", sidB+".jsonl"), userRec(sidB, cwd, "2026-08-24T10:00:00Z", "x"))
		writeTranscript(t, filepath.Join(root.Dir, "-Users-z", sidB+".jsonl"), userRec(sidB, "/Users/z", "2026-08-24T10:00:00Z", "x"))
		decoy := filepath.Join(root.Dir, "-Users-y")
		writeTranscript(t, filepath.Join(decoy, sidB+".jsonl"), userRec(sidB, cwd, "2026-08-01T10:00:00Z", "x"))
		touchAt(t, filepath.Join(decoy, sidB+".jsonl"), fixedNow.Add(-48*time.Hour))
		writeTranscript(t, filepath.Join(decoy, sidC+".jsonl"), userRec(sidC, "/Users/y", "2026-08-24T10:00:00Z", "x"))
		touchAt(t, filepath.Join(decoy, sidC+".jsonl"), fixedNow)

		got, err := ProjectDirFor(root, cwd, []string{"HOME=/Users/a"})
		if err != nil || got != alias {
			t.Errorf("ProjectDirFor = (%q, %v), want %s", got, err, alias)
		}

		// Once the canonical slug dir exists and agrees, it wins over the alias.
		writeTranscript(t, filepath.Join(root.Dir, slug, sidC+".jsonl"), userRec(sidC, cwd, "2026-08-24T10:00:00Z", "x"))
		got, err = ProjectDirFor(root, cwd, nil)
		if err != nil || got != filepath.Join(root.Dir, slug) {
			t.Errorf("ProjectDirFor with the slug dir present = (%q, %v)", got, err)
		}
	})

	t.Run("empty cwd", func(t *testing.T) {
		if _, err := ProjectDirFor(root, "", nil); err == nil {
			t.Error("empty cwd must be an error")
		}
	})

	t.Run("slug too long", func(t *testing.T) {
		long := filepath.Join(cwd, strings.Repeat("d", 210))
		_, err := ProjectDirFor(root, long, nil)
		if !errors.Is(err, ErrSlugTooLong) {
			t.Errorf("err = %v, want ErrSlugTooLong", err)
		}
		// …unless a scanned directory records it.
		hashed := filepath.Join(root.Dir, "-hashed-abc")
		writeTranscript(t, filepath.Join(hashed, sidA+".jsonl"), userRec(sidA, long, "2026-08-24T10:00:00Z", "x"))
		got, err := ProjectDirFor(root, long, nil)
		if err != nil || got != hashed {
			t.Errorf("ProjectDirFor(long) = (%q, %v), want %s", got, err, hashed)
		}
	})
}
