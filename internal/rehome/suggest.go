package rehome

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/imports"
)

// Suggest search parameters. Variables so tests can shorten them.
var (
	// suggestBudget bounds the whole directory scan of one Suggest call.
	suggestBudget = 2 * time.Second
	// gitRemoteTimeout bounds each `git remote get-url origin`.
	gitRemoteTimeout = 2 * time.Second
	// gitRemoteFn resolves a checkout's origin URL; a seam for tests.
	gitRemoteFn = gitRemote
)

const (
	// suggestDepth is how many levels below a root the scan descends.
	suggestDepth = 3
	// envTransferCode never reaches a child process.
	envTransferCode = "BFFS_TRANSFER_CODE"
)

// defaultSuggestRoots are the directories under the home dir where
// checkouts usually live.
var defaultSuggestRoots = []string{"build", "src", "code", "projects", "dev"}

// skippedSuggestDirs are never descended into.
var skippedSuggestDirs = map[string]bool{"node_modules": true, "vendor": true}

// Suggest proposes, for every distinct old directory in rec, the local
// directories it may live in now (plan §9.3): first the checkouts under
// roots whose `git remote get-url origin` equals the sessions' recorded
// git remote ("same git remote <url>", plus ", same folder name" when the
// basenames agree), then <homeDir>/<old cwd relative to the source home>
// when that exists ("same path relative to home"), then directories under
// roots with the same basename ("same folder name"). roots defaults to
// ~/build ~/src ~/code ~/projects ~/dev; the scan goes three levels deep,
// skips hidden directories, node_modules and vendor, and stops after two
// seconds in total. Each git call runs with pinned stdio, a two-second
// timeout and an environment without the transfer code. An old directory
// that exists here as-is gets no candidates — nothing to rehome.
func Suggest(rec imports.Record, homeDir string, roots []string) []Suggestion {
	if len(roots) == 0 && homeDir != "" {
		for _, r := range defaultSuggestRoots {
			roots = append(roots, filepath.Join(homeDir, r))
		}
	}
	olds, remotes := oldCwds(rec)
	if len(olds) == 0 {
		return nil
	}
	scan := scanDirs(roots)
	out := make([]Suggestion, 0, len(olds))
	for _, old := range olds {
		s := Suggestion{OldCwd: old}
		if info, err := os.Stat(old); err == nil && info.IsDir() {
			out = append(out, s)
			continue
		}
		base := baseName(old)
		seen := map[string]bool{}
		add := func(dir, reason string) {
			key := canonicalDir(dir)
			if seen[key] {
				return
			}
			seen[key] = true
			s.Candidates = append(s.Candidates, Candidate{Dir: dir, Reason: reason})
		}
		if remote := remotes[old]; remote != "" {
			for _, d := range scan.byRemote[remote] {
				reason := "same git remote " + remote
				if filepath.Base(d) == base {
					reason += ", same folder name"
				}
				add(d, reason)
			}
		}
		if homeDir != "" && rec.Source.Home != "" {
			if rest, ok := cutPrefix(path.Clean(strings.ReplaceAll(old, "\\", "/")), path.Clean(strings.ReplaceAll(rec.Source.Home, "\\", "/"))); ok && rest != "" {
				dir := filepath.Join(homeDir, filepath.FromSlash(rest))
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					add(dir, "same path relative to home")
				}
			}
		}
		if base != "" {
			for _, d := range scan.byBase[base] {
				add(d, "same folder name")
			}
		}
		out = append(out, s)
	}
	return out
}

// oldCwds lists the distinct old directories of rec in first-seen order
// (sessions, then memories) and the git remote recorded for each.
func oldCwds(rec imports.Record) ([]string, map[string]string) {
	var olds []string
	remotes := map[string]string{}
	seen := map[string]bool{}
	note := func(cwd, remote string) {
		if cwd == "" {
			return
		}
		if !seen[cwd] {
			seen[cwd] = true
			olds = append(olds, cwd)
		}
		if remote != "" && remotes[cwd] == "" {
			remotes[cwd] = remote
		}
	}
	for _, s := range rec.Sessions {
		note(s.OldCwd, s.GitRemote)
	}
	for _, m := range rec.Memories {
		note(m.OldCwd, "")
	}
	return olds, remotes
}

// dirScan indexes the directories found under the roots.
type dirScan struct {
	byRemote map[string][]string
	byBase   map[string][]string
}

// scanDirs walks roots up to suggestDepth levels within suggestBudget,
// recording every directory by basename and every checkout by its origin
// URL. Results are sorted for stable output.
func scanDirs(roots []string) dirScan {
	scan := dirScan{byRemote: map[string][]string{}, byBase: map[string][]string{}}
	deadline := time.Now().Add(suggestBudget)
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		// Not After: on a coarse clock (Windows ticks every ~15 ms) the
		// first call can land on the same instant as the deadline, and a
		// zero budget has to stop rather than walk a level.
		if !time.Now().Before(deadline) {
			return false
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || skippedSuggestDirs[name] {
				continue
			}
			sub := filepath.Join(dir, name)
			scan.byBase[name] = append(scan.byBase[name], sub)
			if isCheckout(sub) {
				if remote := gitRemoteFn(sub); remote != "" {
					scan.byRemote[remote] = append(scan.byRemote[remote], sub)
				}
			}
			if depth+1 < suggestDepth && !walk(sub, depth+1) {
				return false
			}
		}
		return true
	}
	for _, r := range roots {
		if !walk(r, 0) {
			break
		}
	}
	for _, m := range []map[string][]string{scan.byRemote, scan.byBase} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return scan
}

// isCheckout reports whether dir has a .git entry (a directory, or a file
// for worktrees).
func isCheckout(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

// gitRemote runs `git remote get-url origin` in dir with pinned stdio, a
// timeout and an environment without the transfer code; "" when git is
// missing, unhappy or slow.
func gitRemote(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitRemoteTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")
	cmd.Stdin = nil
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.Env = childEnv("GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// childEnv is os.Environ() minus the transfer code, with extra KEY=VALUE
// entries replacing any inherited value of the same key.
func childEnv(extra ...string) []string {
	drop := []string{envTransferCode}
	for _, kv := range extra {
		k, _, _ := strings.Cut(kv, "=")
		drop = append(drop, k)
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		skip := false
		for _, d := range drop {
			if strings.EqualFold(k, d) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// baseName is the last component of a recorded directory in either
// separator convention.
func baseName(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// canonicalDir resolves symlinks when the directory exists, so one
// checkout reached two ways is one candidate.
func canonicalDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}
