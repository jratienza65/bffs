// Package textdiff produces unified diffs of text, line by line.
//
// It exists so that "what differs between these two copies" can be shown
// without a diff binary on the PATH or a dependency in go.mod. The inputs are
// memory files — a few hundred lines at most — so the classic
// longest-common-subsequence
// table is more than fast enough once the common head and tail have been
// trimmed off, and a budget on the table keeps a pathological pair from
// costing memory: past it the middle is reported as a straight replacement.
package textdiff

import (
	"strconv"
	"strings"
)

// Op is one line of a diff: a context line both texts share, a line only the
// old text has, or a line only the new one has.
type Op struct {
	Kind byte // ' ' context, '-' removed, '+' added
	Text string
}

// Hunk is a run of ops around one or more changes, with where it starts in
// each text. Positions are 1-based line numbers, as patch counts them.
type Hunk struct {
	AStart, ALen int
	BStart, BLen int
	Ops          []Op
}

// Stat counts the lines a diff adds and removes.
type Stat struct{ Added, Removed int }

// Context is how many unchanged lines frame each change.
const Context = 3

// maxCells bounds the LCS table. A pair whose middles multiply past this is
// reported as a replacement rather than aligned line by line.
const maxCells = 4_000_000

// Lines splits text the way a diff reads it: one entry per line, the final
// newline not producing an empty last line, and empty text producing no
// lines at all. A missing newline at the end of a file is not a difference
// here — the diff is about what the lines say.
func Lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// Diff returns the hunks that turn a into b. Identical texts give none.
func Diff(a, b string) []Hunk {
	return hunks(edits(Lines(a), Lines(b)))
}

// Count totals what the hunks add and remove.
func Count(hunks []Hunk) Stat {
	var s Stat
	for _, h := range hunks {
		for _, op := range h.Ops {
			switch op.Kind {
			case '+':
				s.Added++
			case '-':
				s.Removed++
			}
		}
	}
	return s
}

// Unified renders hunks in the unified format diff and patch speak, headed by
// the two names. Identical texts render as nothing.
func Unified(aName, bName string, hunks []Hunk) string {
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- " + aName + "\n+++ " + bName + "\n")
	for _, h := range hunks {
		b.WriteString(h.Header() + "\n")
		for _, op := range h.Ops {
			b.WriteByte(op.Kind)
			b.WriteString(op.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Header is the hunk's "@@ -a,b +c,d @@" line.
func (h Hunk) Header() string {
	return "@@ -" + rng(h.AStart, h.ALen) + " +" + rng(h.BStart, h.BLen) + " @@"
}

// rng spells one side of a hunk header the way patch expects it: a lone
// number for a single line, and — for an empty range — the line *before* the
// position, since there is no line of its own to point at.
func rng(start, n int) string {
	switch n {
	case 1:
		return strconv.Itoa(start)
	case 0:
		return strconv.Itoa(start-1) + ",0"
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(n)
}

// edits is the whole edit script: every line of both texts, marked. Most
// edits touch a few lines in the middle of a file, so the common head and
// tail are matched directly and the table is only built for what is left.
func edits(a, b []string) []Op {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	out := make([]Op, 0, len(a)+len(b)-head-tail)
	for _, l := range a[:head] {
		out = append(out, Op{' ', l})
	}
	out = append(out, middle(a[head:len(a)-tail], b[head:len(b)-tail])...)
	for _, l := range a[len(a)-tail:] {
		out = append(out, Op{' ', l})
	}
	return out
}

// middle aligns the changed region by longest common subsequence. The table
// is filled from the back so the forward walk takes a match as soon as it
// sees one, which keeps the script stable and readable: a removed line is
// reported before the line that replaced it.
func middle(a, b []string) []Op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	if n > 0 && m > maxCells/n {
		return replace(a, b)
	}
	w := m + 1
	lcs := make([]int, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	out := make([]Op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, Op{' ', a[i]})
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			out = append(out, Op{'-', a[i]})
			i++
		default:
			out = append(out, Op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Op{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, Op{'+', b[j]})
	}
	return out
}

// replace is the script with no alignment at all: everything old goes,
// everything new comes.
func replace(a, b []string) []Op {
	out := make([]Op, 0, len(a)+len(b))
	for _, l := range a {
		out = append(out, Op{'-', l})
	}
	for _, l := range b {
		out = append(out, Op{'+', l})
	}
	return out
}

// hunks groups a script into hunks: each change with Context lines on either
// side, and changes closer than twice that folded into one hunk, as diff -u
// does.
func hunks(ops []Op) []Hunk {
	var out []Hunk
	aPos, bPos := 1, 1 // line numbers of ops[i] in a and in b
	for i := 0; i < len(ops); {
		if ops[i].Kind == ' ' {
			i++
			aPos++
			bPos++
			continue
		}
		// A change. Find where its hunk ends: the last change before a run
		// of context long enough to separate it from the next one.
		last := i
		for end := i; end < len(ops); end++ {
			if ops[end].Kind != ' ' {
				last = end
			} else if end-last > 2*Context {
				break
			}
		}
		start := max(0, i-Context)
		stop := min(len(ops), last+Context+1)
		// The lines between start and i are context — the previous hunk
		// ended more than 2*Context lines ago — so they count back evenly
		// on both sides.
		h := Hunk{AStart: aPos - (i - start), BStart: bPos - (i - start)}
		for _, op := range ops[start:stop] {
			h.Ops = append(h.Ops, op)
			switch op.Kind {
			case ' ':
				h.ALen++
				h.BLen++
			case '-':
				h.ALen++
			case '+':
				h.BLen++
			}
		}
		out = append(out, h)
		for _, op := range ops[i:stop] {
			switch op.Kind {
			case ' ':
				aPos++
				bPos++
			case '-':
				aPos++
			case '+':
				bPos++
			}
		}
		i = stop
	}
	return out
}
