package rehome

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"github.com/jratienza65/bffs/internal/fsutil"
	"github.com/jratienza65/bffs/internal/transcripts"
)

// maxRewriteFile bounds the memory files RewriteMemoryPaths edits; larger
// ones are left alone (Claude never loads them whole either).
const maxRewriteFile = 8 << 20

// RewriteMemoryPaths replaces old directory prefixes with new ones in the
// memory files under dir — *.md at the top level (MEMORY.md included) and
// under logs/, never proposals/ or index* directories, the set Memories
// and ScanAbsolutePaths walk (plan §9.9). pairs are [old, new] prefixes
// applied longest old first; an occurrence is rewritten only when the old
// prefix is followed by '/', '\', whitespace, a quote, ')' or the end of
// the line, so "/home/jo" never touches "/home/jonas". A rewritten file is
// written atomically and keeps its mtime: Claude injects the newest four
// pinned topic files by mtime (plan §9.7), so a path rewrite must not
// promote a file over the user's own. changed lists the rewritten files
// (relative, slash-separated, sorted); remaining is
// transcripts.ScanAbsolutePaths(dir) afterwards — the lines that still
// mention absolute paths, for the user to review.
func RewriteMemoryPaths(dir string, pairs [][2]string) (changed []string, remaining []transcripts.PathRef, err error) {
	rules := rewriteRules(pairs)
	topics, index, err := listMemorySource(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("rewrite memory paths: %w", err)
	}
	files := topics
	if index != nil {
		files = append(files, *index)
	}
	if len(rules) > 0 {
		for _, f := range files {
			if f.info.Size() > maxRewriteFile {
				continue
			}
			data, err := os.ReadFile(f.abs)
			if err != nil {
				return changed, nil, fmt.Errorf("rewrite memory paths: %w", err)
			}
			out, n := rewritePrefixes(data, rules)
			if n == 0 {
				continue
			}
			if err := fsutil.AtomicWrite(f.abs, out, f.info.Mode().Perm()); err != nil {
				return changed, nil, fmt.Errorf("rewrite memory paths: %w", err)
			}
			if err := os.Chtimes(f.abs, f.info.ModTime(), f.info.ModTime()); err != nil {
				return changed, nil, fmt.Errorf("rewrite memory paths: %w", err)
			}
			changed = append(changed, f.rel)
		}
	}
	sort.Strings(changed)
	remaining, err = transcripts.ScanAbsolutePaths(dir)
	if err != nil {
		return changed, nil, fmt.Errorf("rewrite memory paths: %w", err)
	}
	return changed, remaining, nil
}

// rewriteRules drops empty and identity pairs and orders the rest longest
// old first.
func rewriteRules(pairs [][2]string) [][2][]byte {
	var rules [][2][]byte
	for _, p := range pairs {
		if p[0] == "" || p[1] == "" || p[0] == p[1] {
			continue
		}
		rules = append(rules, [2][]byte{[]byte(p[0]), []byte(p[1])})
	}
	sort.SliceStable(rules, func(i, j int) bool { return len(rules[i][0]) > len(rules[j][0]) })
	return rules
}

// rewritePrefixes replaces every bounded occurrence of a rule's old prefix
// in data and returns the result with the number of replacements.
func rewritePrefixes(data []byte, rules [][2][]byte) ([]byte, int) {
	var out bytes.Buffer
	n := 0
	i := 0
	for i < len(data) {
		matched := false
		for _, r := range rules {
			old := r[0]
			if !bytes.HasPrefix(data[i:], old) {
				continue
			}
			if !boundaryAfter(data, i+len(old)) {
				continue
			}
			out.Write(r[1])
			i += len(old)
			n++
			matched = true
			break
		}
		if !matched {
			out.WriteByte(data[i])
			i++
		}
	}
	if n == 0 {
		return data, 0
	}
	return out.Bytes(), n
}

// boundaryAfter reports whether position i in data ends a path prefix:
// the end of the data or line, a separator, whitespace, a quote or ')'.
func boundaryAfter(data []byte, i int) bool {
	if i >= len(data) {
		return true
	}
	switch data[i] {
	case '/', '\\', ' ', '\t', '\r', '\n', '\'', '"', '`', ')':
		return true
	}
	return false
}
