package porter

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jratienza65/bffs/internal/transcripts"
)

func sessionIDs(ss []transcripts.Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func TestSelectProject(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	// A second project in the same root, with its own memory.
	other := filepath.Join(t.TempDir(), "other")
	mkdir(t, other)
	writeFile(t, filepath.Join(e.root.Dir, "-other", sidC+".jsonl"), userRec(sidC, other, "2026-08-20T10:00:00Z", "C"))
	writeFile(t, filepath.Join(e.root.Dir, "-other", transcripts.MemorySubdir, "o.md"), "o\n")
	ctx := context.Background()

	sel, err := Select(ctx, e.root, nil, nil, SelectOptions{Projects: []string{e.cwd}, Now: fixedNow})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got, want := sessionIDs(sel.Sessions), []string{sidB, sidA}; !reflect.DeepEqual(got, want) {
		t.Errorf("sessions = %v, want newest first %v", got, want)
	}
	if sel.Sessions[1].Title != "Title A" || sel.Sessions[1].PlanSlug != planSlug || sel.Sessions[1].Cwd != e.cwd {
		t.Errorf("titles not resolved: %+v", sel.Sessions[1])
	}
	if len(sel.Memories) != 1 || sel.Memories[0].Dir != e.memDir(t) {
		t.Errorf("memories = %+v, want the project's memory dir", sel.Memories)
	}
	if sel.Parts != DefaultParts || sel.Root.Dir != e.root.Dir {
		t.Errorf("Parts/Root = %+v / %+v", sel.Parts, sel.Root)
	}

	// The same project given relative to HOME resolves to the same thing.
	rel := "~" + strings.TrimPrefix(e.cwd, filepath.Dir(filepath.Dir(e.claudeDir)))
	if home := filepath.Dir(e.claudeDir); strings.HasPrefix(e.cwd, home) {
		rel = "~" + strings.TrimPrefix(e.cwd, home)
		sel2, err := Select(ctx, e.root, nil, nil, SelectOptions{Projects: []string{rel}, Now: fixedNow})
		if err != nil {
			t.Fatalf("Select(%q): %v", rel, err)
		}
		if !reflect.DeepEqual(sessionIDs(sel2.Sessions), sessionIDs(sel.Sessions)) {
			t.Errorf("relative project selected %v", sessionIDs(sel2.Sessions))
		}
	}

	cases := []struct {
		name     string
		opts     SelectOptions
		sessions []string
		memories int
	}{
		{"only sessions", SelectOptions{Projects: []string{e.cwd}, Only: OnlySessions, Now: fixedNow}, []string{sidB, sidA}, 0},
		{"only memories", SelectOptions{Projects: []string{e.cwd}, Only: OnlyMemories, Now: fixedNow}, nil, 1},
		{"since", SelectOptions{Projects: []string{e.cwd}, Since: 20 * day, Now: fixedNow}, []string{sidB}, 1},
		{"all projects", SelectOptions{AllProjects: true, Now: fixedNow}, []string{sidC, sidB, sidA}, 2},
		{"session id", SelectOptions{Sessions: []string{sidA}, Now: fixedNow}, []string{sidA}, 0},
		{"session prefix", SelectOptions{Sessions: []string{"1e005053-380a"}, Now: fixedNow}, []string{sidB}, 0},
		{"project + session dedupes", SelectOptions{Projects: []string{e.cwd}, Sessions: []string{sidA}, Now: fixedNow}, []string{sidB, sidA}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := Select(ctx, e.root, nil, nil, c.opts)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if got := sessionIDs(sel.Sessions); !reflect.DeepEqual(got, c.sessions) && !(len(got) == 0 && len(c.sessions) == 0) {
				t.Errorf("sessions = %v, want %v", got, c.sessions)
			}
			if len(sel.Memories) != c.memories {
				t.Errorf("memories = %d, want %d", len(sel.Memories), c.memories)
			}
		})
	}
}

func TestSelectLiveAndErrors(t *testing.T) {
	e := newEnv(t)
	seedPool(t, e)
	ctx := context.Background()
	live := map[string]transcripts.LiveSession{sidB: {PID: 1, SessionID: sidB}}

	sel, err := Select(ctx, e.root, nil, live, SelectOptions{Projects: []string{e.cwd}, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionIDs(sel.Sessions); !reflect.DeepEqual(got, []string{sidA}) {
		t.Errorf("live session not dropped: %v", got)
	}
	sel, err = Select(ctx, e.root, nil, live, SelectOptions{Projects: []string{e.cwd}, IncludeLive: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionIDs(sel.Sessions); !reflect.DeepEqual(got, []string{sidB, sidA}) || !sel.Sessions[0].Live {
		t.Errorf("IncludeLive: %v live=%v", got, sel.Sessions[0].Live)
	}
	// A session selected by id is marked live too.
	sel, err = Select(ctx, e.root, nil, live, SelectOptions{Sessions: []string{sidB}, IncludeLive: true, Now: fixedNow})
	if err != nil || len(sel.Sessions) != 1 || !sel.Sessions[0].Live {
		t.Errorf("by-id live: %v %+v", err, sel.Sessions)
	}

	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{}); err == nil || !strings.Contains(err.Error(), "nothing selected") {
		t.Errorf("empty selection: %v", err)
	}
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Projects: []string{e.cwd}, Only: "everything"}); err == nil || !strings.Contains(err.Error(), "--only") {
		t.Errorf("bad --only: %v", err)
	}
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Sessions: []string{"1e00"}}); err == nil {
		t.Error("short prefix accepted")
	}
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Sessions: []string{"1e005053"}}); err != nil {
		t.Errorf("unique prefix: %v", err)
	}
	writeFile(t, filepath.Join(e.root.Dir, "-other", sidC+".jsonl"), userRec(sidC, "/other", "2026-08-20T10:00:00Z", "C"))
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Sessions: []string{"1e005053"}}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix: %v", err)
	}
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Sessions: []string{sidA[:9] + "ffff"}}); err == nil || !strings.Contains(err.Error(), "unknown session") {
		t.Errorf("unknown session: %v", err)
	}
	t.Setenv(transcripts.EnvRemoteMemoryDir, "/elsewhere")
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Projects: []string{e.cwd}}); err == nil || !strings.Contains(err.Error(), transcripts.EnvRemoteMemoryDir) {
		t.Errorf("memory override: %v", err)
	}
	if _, err := Select(ctx, e.root, nil, nil, SelectOptions{Projects: []string{e.cwd}, Only: OnlySessions}); err != nil {
		t.Errorf("sessions-only must not consult the memory dir: %v", err)
	}
}

func TestSortNewestFirst(t *testing.T) {
	ss := []transcripts.Session{{ID: "b", LastTS: fixedNow}, {ID: "a", LastTS: fixedNow}, {ID: "c", LastTS: fixedNow.Add(time.Hour)}}
	sortNewestFirst(ss)
	if got := sessionIDs(ss); !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Errorf("order = %v", got)
	}
}
