package rehome

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jratienza65/bffs/internal/store"
)

// TranscriptRules says what RewriteTranscript changes inside one transcript
// (plan §9.8, disk.md §3 — both passes change historical records and are
// off by default in every caller).
//
// With Cwd, the top-level "cwd" value of every record that equals OldCwd
// (byte-for-byte or after store.NormalizePath) becomes NewCwd; a "cwd"
// nested anywhere else — inside message content, tool inputs, tool
// results — is never touched. With FileHistory, the absolute paths in
// file-history-snapshot records (the keys of snapshot.trackedFileBackups)
// and file-history-delta records (trackingPath and every realParentDir)
// have their leading directory replaced by the first Paths pair whose old
// prefix matches at a path boundary ('/', '\' or the end of the string),
// longest old prefix first; OldCwd→NewCwd is always one of the pairs. Old
// prefixes are matched as the source machine spelled them (a trailing
// separator is ignored, nothing is re-cleaned for the OS running the
// rewrite). Everything else in the file stays byte-identical.
type TranscriptRules struct {
	OldCwd, NewCwd string
	Paths          [][2]string
	Cwd            bool
	FileHistory    bool
}

// TranscriptRewrite counts what RewriteTranscript changed in one
// transcript: CwdRecords records whose top-level cwd was replaced and
// FileHistoryPaths path strings replaced in file-history records.
type TranscriptRewrite struct {
	SessionID        string
	CwdRecords       int
	FileHistoryPaths int
}

// RewriteTranscript streams the transcript at path line by line, applies
// rules, and — only when something changed — writes the result to
// <path>.bffs-tmp, renames it over path and restores the original mtime,
// so picker order and Claude's retention sweep see the file as before. A
// line that does not parse as one JSON object is copied verbatim. The
// transcript must not be open in a running claude: Apply reaches this pass
// only for moves PlanRehome accepted, and PlanRehome refuses live sessions
// (plan §9.5).
func RewriteTranscript(path string, rules TranscriptRules) (TranscriptRewrite, error) {
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return TranscriptRewrite{}, err
	}
	defer dir.Close()
	return rewriteTranscriptIn(dir, filepath.Base(path), rules)
}

// rewriteTranscriptIn is RewriteTranscript over rel inside dest.
func rewriteTranscriptIn(dest *os.Root, rel string, rules TranscriptRules) (rw TranscriptRewrite, err error) {
	if !rules.Cwd && !rules.FileHistory {
		return rw, nil
	}
	rw.SessionID = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	src, err := dest.Open(rel)
	if err != nil {
		return rw, err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return rw, err
	}
	if !info.Mode().IsRegular() {
		return rw, fmt.Errorf("%q is not a regular file", filepath.ToSlash(rel))
	}
	tmp := freeName(dest, rel+tmpSuffix)
	out, err := dest.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return rw, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = out.Close()
			_ = dest.Remove(tmp)
		}
	}()

	lr := newLineRewriter(rules)
	r := bufio.NewReaderSize(src, 64<<10)
	w := bufio.NewWriterSize(out, 64<<10)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			line, cwdN, pathN := lr.rewrite(line)
			rw.CwdRecords += cwdN
			rw.FileHistoryPaths += pathN
			if _, err := w.Write(line); err != nil {
				return rw, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rw, rerr
		}
	}
	if rw.CwdRecords == 0 && rw.FileHistoryPaths == 0 {
		return rw, nil // nothing changed: the deferred cleanup drops tmp
	}
	if err := w.Flush(); err != nil {
		return rw, err
	}
	if err := out.Sync(); err != nil {
		return rw, err
	}
	if err := out.Close(); err != nil {
		return rw, err
	}
	if err := dest.Rename(tmp, rel); err != nil {
		return rw, fmt.Errorf("rename %q to %q: %w", filepath.ToSlash(tmp), filepath.ToSlash(rel), err)
	}
	committed = true
	syncDir(dest, filepath.Dir(rel))
	if err := dest.Chtimes(rel, info.ModTime(), info.ModTime()); err != nil {
		return rw, fmt.Errorf("set mtime on %q: %w", filepath.ToSlash(rel), err)
	}
	return rw, nil
}

// lineRewriter applies TranscriptRules to one transcript line at a time.
type lineRewriter struct {
	rules   TranscriptRules
	oldCwds map[string]bool // OldCwd spellings a top-level cwd may carry
	prefix  [][2]string     // FileHistory pairs, longest old first
	newCwd  []byte          // NewCwd as a JSON string
}

func newLineRewriter(rules TranscriptRules) *lineRewriter {
	lr := &lineRewriter{rules: rules, oldCwds: map[string]bool{}}
	if rules.Cwd && rules.OldCwd != "" && rules.NewCwd != "" && rules.OldCwd != rules.NewCwd {
		lr.oldCwds[rules.OldCwd] = true
		lr.newCwd = encodeJSONString(rules.NewCwd)
	}
	if rules.FileHistory {
		pairs := append([][2]string{{rules.OldCwd, rules.NewCwd}}, rules.Paths...)
		for _, p := range pairs {
			if p[0] == "" || p[1] == "" || p[0] == p[1] {
				continue
			}
			lr.prefix = append(lr.prefix, [2]string{trimTrailingSeparators(p[0]), p[1]})
		}
		sort.SliceStable(lr.prefix, func(i, j int) bool { return len(lr.prefix[i][0]) > len(lr.prefix[j][0]) })
	}
	return lr
}

// rewrite returns line with the rules applied and the number of top-level
// cwd values and file-history paths replaced. A line the walker cannot
// parse, or whose string spans do not line up with the bytes (never seen;
// a guard against corrupting a transcript), is returned as it came.
func (lr *lineRewriter) rewrite(line []byte) ([]byte, int, int) {
	wantCwd := len(lr.oldCwds) > 0 && bytes.Contains(line, []byte(`"cwd"`))
	wantPaths := len(lr.prefix) > 0 && bytes.Contains(line, []byte(`"file-history-`))
	if !wantCwd && !wantPaths {
		return line, 0, 0
	}
	var (
		recordType string
		edits      []spanEdit
		cwdN       int
		pathN      int
	)
	err := walkStrings(line, func(s stringSpan) {
		switch {
		case !s.isKey && len(s.path) == 1 && s.path[0] == "type":
			recordType = s.value
		case wantCwd && !s.isKey && len(s.path) == 1 && s.path[0] == "cwd":
			if lr.isOldCwd(s.value) {
				edits = append(edits, spanEdit{s.start, s.end, lr.newCwd, true})
			}
		case wantPaths && s.isKey && len(s.path) >= 1 && s.path[len(s.path)-1] == "trackedFileBackups":
			if v, ok := lr.replacePrefix(s.value); ok {
				edits = append(edits, spanEdit{s.start, s.end, encodeJSONString(v), false})
			}
		case wantPaths && !s.isKey && len(s.path) >= 1 && (s.path[len(s.path)-1] == "trackingPath" || s.path[len(s.path)-1] == "realParentDir"):
			if v, ok := lr.replacePrefix(s.value); ok {
				edits = append(edits, spanEdit{s.start, s.end, encodeJSONString(v), false})
			}
		}
	})
	if err != nil || len(edits) == 0 {
		return line, 0, 0
	}
	isFileHistory := recordType == "file-history-snapshot" || recordType == "file-history-delta"
	var out bytes.Buffer
	out.Grow(len(line) + 64)
	pos := 0
	for _, e := range edits {
		if !e.cwd && !isFileHistory {
			continue
		}
		if e.start < pos || e.end > len(line) || line[e.start] != '"' || line[e.end-1] != '"' {
			return line, 0, 0
		}
		out.Write(line[pos:e.start])
		out.Write(e.repl)
		pos = e.end
		if e.cwd {
			cwdN++
		} else {
			pathN++
		}
	}
	if cwdN+pathN == 0 {
		return line, 0, 0
	}
	out.Write(line[pos:])
	return out.Bytes(), cwdN, pathN
}

// isOldCwd reports whether v is the old cwd, spelled as recorded or
// after normalisation (macOS /var vs /private/var, a trailing slash).
func (lr *lineRewriter) isOldCwd(v string) bool {
	if lr.oldCwds[v] {
		return true
	}
	if n, err := store.NormalizePath(v); err == nil && n != v {
		if lr.oldCwds[n] {
			lr.oldCwds[v] = true
			return true
		}
	}
	return false
}

// trimTrailingSeparators drops trailing '/' or '\' from an old prefix so
// the boundary check treats "/a/b/" like "/a/b"; a bare root ("/", `C:\`)
// keeps its separator. The separators inside the prefix are left alone:
// the paths come from the source machine and must match as spelled there,
// whatever the OS running the rewrite (filepath.Clean would turn a POSIX
// prefix into a backslash one on Windows).
func trimTrailingSeparators(p string) string {
	t := strings.TrimRight(p, `/\`)
	if len(t) < len(p) && (t == "" || (len(t) == 2 && t[1] == ':')) {
		return p[:len(t)+1]
	}
	return t
}

// replacePrefix replaces the longest matching old prefix at the start of
// the path v when it ends at a path boundary.
func (lr *lineRewriter) replacePrefix(v string) (string, bool) {
	for _, p := range lr.prefix {
		old := p[0]
		if !strings.HasPrefix(v, old) {
			continue
		}
		rest := v[len(old):]
		if rest != "" && rest[0] != '/' && rest[0] != '\\' {
			continue
		}
		return p[1] + rest, true
	}
	return "", false
}

// spanEdit replaces line[start:end] with repl; cwd marks a top-level cwd
// edit (always applied) as opposed to a file-history path edit (applied
// only when the record's type turns out to be a file-history one).
type spanEdit struct {
	start, end int
	repl       []byte
	cwd        bool
}

// stringSpan is one JSON string token in a line: the byte span of its
// encoding (quotes included), its decoded value, whether it is an object
// key, and the keys of the enclosing objects — innermost last, with "[]"
// for every array level; for a value the last element is its own key.
type stringSpan struct {
	start, end int
	value      string
	isKey      bool
	path       []string
}

// walkFrame is one open object or array during walkStrings.
type walkFrame struct {
	obj         bool
	key         string
	expectValue bool // object: a key was read, its value comes next
}

var errSpanMismatch = errors.New("string token span does not start and end with a quote")

// walkStrings visits every string token of the single JSON value in line,
// in order, with its exact byte span. Spans are derived from
// json.Decoder.InputOffset after each token (the end) and the first byte
// after the preceding token's end that is not whitespace, ',' or ':' (the
// start), and checked to be quotes.
func walkStrings(line []byte, visit func(stringSpan)) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	var frames []*walkFrame
	prevEnd := 0
	top := func() *walkFrame {
		if len(frames) == 0 {
			return nil
		}
		return frames[len(frames)-1]
	}
	pathOf := func() []string {
		var p []string
		for _, f := range frames {
			switch {
			case !f.obj:
				p = append(p, "[]")
			case f.expectValue:
				p = append(p, f.key)
			}
		}
		return p
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(frames) != 0 {
				return io.ErrUnexpectedEOF // a truncated line
			}
			return nil
		}
		if err != nil {
			return err
		}
		end := int(dec.InputOffset())
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				frames = append(frames, &walkFrame{obj: true})
			case '[':
				frames = append(frames, &walkFrame{})
			default:
				if len(frames) == 0 {
					return errors.New("unbalanced delimiter")
				}
				frames = frames[:len(frames)-1]
				if f := top(); f != nil && f.obj {
					f.expectValue = false
				}
			}
		case string:
			f := top()
			isKey := f != nil && f.obj && !f.expectValue
			start := skipSeparators(line, prevEnd)
			if start >= end || line[start] != '"' || line[end-1] != '"' {
				return errSpanMismatch
			}
			visit(stringSpan{start: start, end: end, value: v, isKey: isKey, path: pathOf()})
			if isKey {
				f.key, f.expectValue = v, true
			} else if f != nil && f.obj {
				f.expectValue = false
			}
		default:
			if f := top(); f != nil && f.obj {
				f.expectValue = false
			}
		}
		prevEnd = end
	}
}

// skipSeparators returns the index of the first byte at or after i that is
// not JSON whitespace, ',' or ':'.
func skipSeparators(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n', ',', ':':
			i++
		default:
			return i
		}
	}
	return i
}

// encodeJSONString encodes s the way Claude's JSON.stringify does for a
// path: quotes and backslashes escaped, '<', '>' and '&' left alone.
func encodeJSONString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		b, _ := json.Marshal(s)
		return b
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
