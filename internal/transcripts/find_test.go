package transcripts

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	root, _ := listFixture(t)
	// A second root (full isolation) holding a copy of sidA under another slug.
	cfg2 := t.TempDir()
	root2 := Root{Dir: filepath.Join(cfg2, ProjectsSubdir), ConfigDir: cfg2, Owner: "bravo"}
	writeTranscript(t, filepath.Join(root2.Dir, "-home-b-proj", sidA+".jsonl"),
		userRec(sidA, "/home/b/proj", "2026-08-24T10:00:00Z", "copied"),
		rec(map[string]any{"type": "custom-title", "sessionId": sidA, "customTitle": "Copied A"}),
	)
	touchAt(t, filepath.Join(root2.Dir, "-home-b-proj", sidA+".jsonl"), fixedNow.Add(-10*time.Minute))
	roots := []Root{root, root2}

	t.Run("full id in two roots", func(t *testing.T) {
		ss, err := Find(roots, strings.ToUpper(sidA))
		if err != nil {
			t.Fatal(err)
		}
		if len(ss) != 2 || ss[0].Root.Owner != "bravo" || ss[1].Root.Owner != "" {
			t.Fatalf("Find = %+v", ss)
		}
		if ss[0].Title != "Copied A" || ss[0].TitleSource != TitleSourceCustom || ss[1].Title != "Title A" || ss[1].Cwd != "/Users/a/proj" {
			t.Errorf("titles not populated: %q / %q", ss[0].Title, ss[1].Title)
		}
		if ss[1].Subagents != 2 {
			t.Errorf("Subagents = %d", ss[1].Subagents)
		}
	})
	t.Run("unique prefix", func(t *testing.T) {
		ss, err := Find(roots, sidC[:8]+"-aaaa")
		if err != nil {
			t.Fatal(err)
		}
		if len(ss) != 1 || ss[0].ID != sidC || !ss[0].Relocated {
			t.Errorf("Find = %+v", ss)
		}
	})
	t.Run("ambiguous prefix", func(t *testing.T) {
		// sidB and sidC share their first 8 hex digits.
		_, err := Find(roots, sidB[:8])
		if !errors.Is(err, ErrAmbiguousSession) {
			t.Fatalf("err = %v, want ErrAmbiguousSession", err)
		}
		if !strings.Contains(err.Error(), sidB) || !strings.Contains(err.Error(), sidC) {
			t.Errorf("error should name the candidates: %v", err)
		}
	})
	t.Run("not found", func(t *testing.T) {
		_, err := Find(roots, "ffffffff-0000-4000-8000-000000000000")
		if !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("err = %v, want ErrSessionNotFound", err)
		}
		if _, err := Find(roots, "ffffffff"); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("prefix err = %v, want ErrSessionNotFound", err)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, bad := range []string{"", "0f3b2c1", "0f3b2c1e4", "zzzzzzzz", "../../etc", sidA + "0"} {
			if _, err := Find(roots, bad); err == nil || errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrAmbiguousSession) {
				t.Errorf("Find(%q) err = %v, want an invalid-id error", bad, err)
			}
		}
	})
	t.Run("prefix ending in a dash", func(t *testing.T) {
		ss, err := Find(roots, sidA[:9])
		if err != nil || len(ss) != 2 || ss[0].ID != sidA {
			t.Errorf("Find(%q) = (%v, %v)", sidA[:9], ids(ss), err)
		}
	})
	t.Run("missing root dir", func(t *testing.T) {
		ss, err := Find([]Root{{Dir: filepath.Join(t.TempDir(), "none")}, root}, sidA)
		if err != nil || len(ss) != 1 {
			t.Errorf("Find with a missing root = (%v, %v)", ss, err)
		}
	})
}

func TestIsUUIDPrefix(t *testing.T) {
	cases := map[string]bool{
		"0f3b2c1e": true, "0f3b2c1e-4d5a": true, sidA: true, "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4": true,
		"0f3b2c1": false, "0f3b2c1e4d5a": false, "0f3b2c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e0": false, "0F3B2C1E": false, "-0f3b2c1e": false,
	}
	for q, want := range cases {
		if got := isUUIDPrefix(q); got != want {
			t.Errorf("isUUIDPrefix(%q) = %v, want %v", q, got, want)
		}
	}
	if !isUUID(sidA) || !isUUID(strings.ToUpper(sidA)) || isUUID(sidA[:35]) || isUUID(strings.Replace(sidA, "-", "_", 1)) {
		t.Error("isUUID shape check")
	}
}
