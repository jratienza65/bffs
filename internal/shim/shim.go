package shim

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/resolver"
	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

const (
	EnvRealClaude   = "BFFS_REAL_CLAUDE"
	EnvAPIKey       = "ANTHROPIC_API_KEY"
	EnvOAuthToken   = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvClaudeCfgDir = "CLAUDE_CONFIG_DIR"
)

// Run resolves the active account, sets the appropriate env var, and executes
// the real `claude` binary with the supplied args. It returns the child's exit
// code (or a non-zero code with a non-nil error if it could not even start).
func Run(args []string) (int, error) {
	cfgDir, err := store.ConfigDir()
	if err != nil {
		return 1, fmt.Errorf("locate config dir: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("getwd: %w", err)
	}

	r, err := resolver.Resolve(cfgDir, cwd)
	if err != nil {
		return 1, err
	}

	env := os.Environ()
	if r.Source != resolver.SourceNone {
		env = AccountEnv(env, r.Account, cfgDir)
		if r.Account.Type == store.TypeOAuth {
			SyncOAuthSessionDir(cfgDir, r.Account)
		}
	}

	realPath, err := FindRealClaude(cfgDir)
	if err != nil {
		return 1, err
	}

	// After FindRealClaude: only launches that will actually exec count as usage.
	logLaunch(cfgDir, r, cwd)

	return execProcess(realPath, args, env)
}

// logLaunch appends a launch event to <cfgDir>/launches.jsonl so `bffs usage`
// can attribute the resulting claude session to an account (transcripts carry
// no account identity). Best-effort and silent, like syncOAuthSessionDir: the
// shim must never fail or chat on the way to exec'ing claude. Users opt out
// with BFFS_NO_USAGE_LOG. SourceNone launches are recorded too (with an empty
// account) — the analyzer needs them to spot ambiguous attribution.
func logLaunch(cfgDir string, r resolver.Result, cwd string) {
	if usagelog.Disabled() {
		return
	}
	e := usagelog.Event{TS: time.Now().UTC(), Source: string(r.Source), Cwd: cwd}
	if r.Source != resolver.SourceNone {
		e.Account = r.Account.Name
		e.Type = string(r.Account.Type)
	}
	_ = usagelog.Append(cfgDir, e)
}

// SyncOAuthSessionDir reconciles symlinks in the per-account session dir so
// new entries claude added under ~/.claude after `bffs login` show up here
// too — keeping the partial preset's drop-in feel intact over time. Called
// by the shim on every oauth launch and by internal/runner before spawning
// a delegated child.
//
// Best-effort and silent: failures and skipped-conflict paths are not logged
// (the user discovers them via `bffs reisolate <name>` or `bffs show`, where
// they're surfaced explicitly). The shim's job is to not interfere with the
// claude UX; per-account sync drift is a non-fatal config issue.
func SyncOAuthSessionDir(cfgDir string, acc store.Account) {
	state, err := store.LoadState(cfgDir)
	if err != nil {
		return
	}
	preset := store.ResolveIsolation(acc.Isolation, state.Isolation)

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	homeClaude := filepath.Join(home, ".claude")
	_, _ = sessions.SyncSymlinks(sessions.Dir(cfgDir, acc.Name), homeClaude, preset)
}

// AccountEnv strips conflicting env vars and installs the ones the resolved
// account needs. Both account types are per-invocation; no global state
// is touched. Shared by the shim and internal/runner (delegated runs).
//
//   - api_key: sets ANTHROPIC_API_KEY to the secret.
//   - oauth:   sets CLAUDE_CONFIG_DIR to the per-account session dir under
//     cfgDir. Claude Code reads .claude.json + .credentials.json
//     from there and uses a Keychain service name derived (sha256)
//     from the dir, so concurrent oauth sessions on different
//     accounts are fully isolated. The session dir is set up by
//     `bffs login` (or `bffs reisolate`); the shim just points
//     claude at it.
//
// Stale ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, and CLAUDE_CONFIG_DIR
// values from the user's shell are dropped first so they can't override the
// resolved account.
func AccountEnv(env []string, acc store.Account, cfgDir string) []string {
	cleaned := env[:0]
	for _, kv := range env {
		k := kvKey(kv)
		if k == EnvAPIKey || k == EnvOAuthToken || k == EnvClaudeCfgDir {
			continue
		}
		cleaned = append(cleaned, kv)
	}
	switch acc.Type {
	case store.TypeAPIKey:
		cleaned = append(cleaned, EnvAPIKey+"="+acc.Secret)
	case store.TypeOAuth:
		cleaned = append(cleaned, EnvClaudeCfgDir+"="+sessions.Dir(cfgDir, acc.Name))
	}
	return cleaned
}

func kvKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// FindRealClaude locates the actual `claude` binary, skipping the shim itself.
// Resolution order:
//  1. BFFS_REAL_CLAUDE env var (explicit override, useful for tests)
//  2. Cached path at <configdir>/real-claude.path
//  3. Walk PATH; pick the first `claude` executable that does NOT resolve to
//     this binary. Cache it for next time.
func FindRealClaude(cfgDir string) (string, error) {
	if v := os.Getenv(EnvRealClaude); v != "" {
		return v, nil
	}
	cachePath := filepath.Join(cfgDir, store.RealClaudePath)
	if cached, err := os.ReadFile(cachePath); err == nil {
		p := strings.TrimSpace(string(cached))
		if p != "" {
			if _, statErr := os.Stat(p); statErr == nil {
				return p, nil
			}
			// stale cache; fall through and rescan
		}
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	selfReal, _ := filepath.EvalSymlinks(self)

	target := claudeBinaryName()
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, target)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || !isExecutable(info.Mode()) {
			continue
		}
		candReal, _ := filepath.EvalSymlinks(candidate)
		if pathsEqual(candReal, selfReal) || pathsEqual(candidate, self) {
			continue
		}
		_ = os.MkdirAll(cfgDir, 0o700)
		_ = os.WriteFile(cachePath, []byte(candidate+"\n"), 0o600)
		return candidate, nil
	}
	return "", fmt.Errorf("could not find a real %q on PATH (after skipping the shim). Install Claude Code first, or set %s to the absolute path", target, EnvRealClaude)
}

func claudeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "claude.exe"
	}
	return "claude"
}

func isExecutable(m fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return m&0o111 != 0
}

func pathsEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
