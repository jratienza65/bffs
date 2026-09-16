package transcripts

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// MemoryIndexFile is the auto-memory index Claude injects at startup.
	MemoryIndexFile = "MEMORY.md"

	// MemoryLogsSubdir holds per-session memory logs (logs/YYYY/MM/DD/…);
	// MemoryProposalsSubdir holds swept scratch that is never loaded.
	MemoryLogsSubdir      = "logs"
	MemoryProposalsSubdir = "proposals"

	// PathKindAbs and PathKindAt classify a PathRef: a bare absolute path,
	// or an @-reference that Claude's include lexer would resolve.
	PathKindAbs = "abs"
	PathKindAt  = "at"

	// maxMemoryFile bounds how much of one memory file is scanned.
	maxMemoryFile = 8 << 20
)

// PathRef is one absolute path or @-reference found in a memory file.
// File is relative to the scanned directory, slash-separated; Line is
// 1-based; Path is the token with trailing punctuation removed (an
// @-reference keeps its "@").
type PathRef struct {
	File string
	Line int
	Path string
	Kind string // PathKindAbs | PathKindAt
}

// absPrefixes are the token starts that mark a machine-specific absolute
// path. Anything else starting with "/" is more often a URL path or a
// regex than a directory.
var absPrefixes = []string{"/Users/", "/home/", "/root/", "/tmp/", "/private/", "/opt/"}

// ScanAbsolutePaths walks the memory files under dir — *.md at the top
// level and under logs/, skipping proposals/ and any directory whose name
// starts with "index" — and reports every whitespace-separated token that
// is an absolute path (/Users/ /home/ /root/ /tmp/ /private/ /opt/ or
// [A-Z]:\) or an @-reference (@/ @~ @.). Leading quotes, backticks and
// brackets are stripped before matching; trailing ),.;:'" backticks and
// closing brackets after. These are the lines to review after a rehome, and the
// references that can raise Claude's external-includes dialog (§3).
func ScanAbsolutePaths(dir string) ([]PathRef, error) {
	files, err := memoryFiles(dir)
	if err != nil {
		return nil, err
	}
	var refs []PathRef
	for _, mf := range files {
		refs = append(refs, mf.refs...)
	}
	return refs, nil
}

// memoryEntry is one scanned memory file.
type memoryEntry struct {
	name   string // relative, slash-separated
	info   fs.FileInfo
	pinned bool
	refs   []PathRef
}

// memoryFiles lists and scans the memory files under dir, sorted by name.
// A missing dir is an error; unreadable files are skipped. Only top-level
// topic files can be pinned: Claude's pinned scan excludes MEMORY.md and
// logs/ (disk.md §5), so a "pinned: true" there is reported false.
func memoryFiles(dir string) ([]memoryEntry, error) {
	var out []memoryEntry
	add := func(rel string, info fs.FileInfo, pinnable bool) {
		e := memoryEntry{name: filepath.ToSlash(rel), info: info}
		e.pinned, e.refs = scanMemoryFile(filepath.Join(dir, rel), e.name)
		e.pinned = e.pinned && pinnable
		out = append(out, e)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !isMarkdown(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		add(e.Name(), info, e.Name() != MemoryIndexFile)
	}

	logs := filepath.Join(dir, MemoryLogsSubdir)
	err = filepath.WalkDir(logs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == logs && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return nil // unreadable subtree: skipped
		}
		if d.IsDir() {
			if path != logs && excludedMemoryDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !isMarkdown(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		add(rel, info, false)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", logs, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// excludedMemoryDir reports whether a subdirectory of a memory dir is
// never loaded by Claude: proposals/ and index caches.
func excludedMemoryDir(name string) bool {
	return name == MemoryProposalsSubdir || strings.HasPrefix(name, "index")
}

func isMarkdown(name string) bool {
	return strings.HasSuffix(name, ".md")
}

// scanMemoryFile reads one memory file and returns whether its YAML
// frontmatter pins it and every PathRef inside it.
func scanMemoryFile(path, rel string) (pinned bool, refs []PathRef) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, maxMemoryFile), 64*1024)
	lineNo := 0
	inFront, frontDone := false, false
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			trimmed := strings.TrimSpace(line)
			switch {
			case lineNo == 1 && trimmed == "---":
				inFront = true
			case inFront && trimmed == "---":
				inFront, frontDone = false, true
			case inFront && !frontDone:
				if k, v, ok := strings.Cut(trimmed, ":"); ok && k == "pinned" {
					pinned = strings.Trim(strings.TrimSpace(v), `"'`) == "true"
				}
			}
			for _, tok := range strings.Fields(line) {
				if p, kind, ok := classifyToken(tok); ok {
					refs = append(refs, PathRef{File: rel, Line: lineNo, Path: p, Kind: kind})
				}
			}
		}
		if err != nil {
			return pinned, refs
		}
	}
}

// leadingWrap and trailingWrap are the punctuation a token may be wrapped
// in on either side (markdown code spans, quotes, parentheses, brackets)
// that is never part of the path.
const (
	leadingWrap  = "`\"'([<"
	trailingWrap = "),.;:'\"`]>"
)

// classifyToken decides whether one whitespace-separated token is a path
// reference worth reporting.
func classifyToken(tok string) (path, kind string, ok bool) {
	tok = strings.TrimLeft(tok, leadingWrap)
	tok = strings.TrimRight(tok, trailingWrap)
	switch {
	case len(tok) > 1 && tok[0] == '@' && (tok[1] == '/' || tok[1] == '~' || tok[1] == '.'):
		return tok, PathKindAt, true
	case isAbsToken(tok):
		return tok, PathKindAbs, true
	}
	return "", "", false
}

func isAbsToken(tok string) bool {
	for _, p := range absPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return len(tok) >= 3 && tok[0] >= 'A' && tok[0] <= 'Z' && tok[1] == ':' && tok[2] == '\\'
}
