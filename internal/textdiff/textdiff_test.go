package textdiff

import (
	"strings"
	"testing"
)

func lines(n int, prefix string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(prefix + string(rune('a'+(i-1)%26)) + "\n")
	}
	return b.String()
}

func TestIdenticalTextsHaveNoHunks(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "a\n", "a\nb\nc", "a\nb\nc\n"} {
		if h := Diff(s, s); len(h) != 0 {
			t.Errorf("%q against itself: %d hunks", s, len(h))
		}
	}
	// A missing final newline is not a difference in what the lines say.
	if h := Diff("a\nb", "a\nb\n"); len(h) != 0 {
		t.Errorf("a final newline alone made %d hunks", len(h))
	}
}

func TestUnifiedMatchesDiffU(t *testing.T) {
	t.Parallel()
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\n"
	want := strings.Join([]string{
		"--- old",
		"+++ new",
		"@@ -2,7 +2,7 @@",
		" two",
		" three",
		" four",
		"-five",
		"+FIVE",
		" six",
		" seven",
		" eight",
		"",
	}, "\n")
	if got := Unified("old", "new", Diff(a, b)); got != want {
		t.Errorf("unified diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestInsertionAndDeletionAtTheEdges(t *testing.T) {
	t.Parallel()
	// Adding to an empty file points at line 0 on the old side.
	if got := Diff("", "a\nb\n")[0].Header(); got != "@@ -0,0 +1,2 @@" {
		t.Errorf("insert into empty: %s", got)
	}
	// Removing everything points at line 0 on the new side.
	if got := Diff("a\nb\n", "")[0].Header(); got != "@@ -1,2 +0,0 @@" {
		t.Errorf("delete all: %s", got)
	}
	// Appending after a long common head keeps only Context lines of it.
	h := Diff(lines(10, ""), lines(10, "")+"tail\n")
	if len(h) != 1 || h[0].Header() != "@@ -8,3 +8,4 @@" {
		t.Errorf("append: %+v", h)
	}
}

func TestChangesFarApartMakeSeparateHunks(t *testing.T) {
	t.Parallel()
	a := lines(20, "")
	b := strings.Replace(strings.Replace(a, "b\n", "B\n", 1), "s\n", "S\n", 1)
	h := Diff(a, b)
	if len(h) != 2 {
		t.Fatalf("%d hunks, want 2: %+v", len(h), h)
	}
	if h[0].Header() != "@@ -1,5 +1,5 @@" || h[1].Header() != "@@ -16,5 +16,5 @@" {
		t.Errorf("headers: %s / %s", h[0].Header(), h[1].Header())
	}
	// Changes within 2*Context of each other share a hunk.
	c := strings.Replace(strings.Replace(a, "b\n", "B\n", 1), "h\n", "H\n", 1)
	if h := Diff(a, c); len(h) != 1 {
		t.Errorf("neighbouring changes made %d hunks", len(h))
	}
	if s := Count(Diff(a, b)); s != (Stat{Added: 2, Removed: 2}) {
		t.Errorf("stat = %+v", s)
	}
}

func TestReplacementReportsOldBeforeNew(t *testing.T) {
	t.Parallel()
	ops := Diff("a\nx\ny\nb\n", "a\np\nq\nb\n")[0].Ops
	var kinds []byte
	for _, op := range ops {
		kinds = append(kinds, op.Kind)
	}
	if string(kinds) != " --++ " {
		t.Errorf("script %q, want removed lines before added ones", kinds)
	}
}

func TestHugeMiddleFallsBackToAReplacement(t *testing.T) {
	t.Parallel()
	a := lines(3000, "x")
	b := lines(3000, "y")
	h := Diff(a, b)
	if s := Count(h); s.Added != 3000 || s.Removed != 3000 {
		t.Errorf("stat = %+v, want every line replaced", s)
	}
}
