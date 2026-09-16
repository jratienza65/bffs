// Package runner spawns claude as a *child process* on a named bffs account:
// env injected per-invocation exactly like the shim, but spawn-and-wait
// instead of exec, with output capture, a timeout, and process-tree kill for
// non-interactive callers. It backs `bffs run` and the MCP `run_on_account`
// tool.
//
// The child is a fresh, independent, top-level claude session. Session-
// instance markers a hosting Claude Code session plants in the environment
// are stripped so the child never mistakes itself for a subprocess of the
// current session.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
	"github.com/jratienza65/bffs/internal/store"
	"github.com/jratienza65/bffs/internal/usagelog"
)

// LaunchSource is the usagelog Source recorded for delegated runs, so `bffs
// usage` attributes them like shim launches.
const LaunchSource = "run"

// sessionMarkers are env vars a hosting Claude Code session plants in every
// child process. Inherited by a delegated run they would make the child think
// it is a subprocess of the current session (child-session mode, messaging
// socket, shared session id) instead of a fresh top-level run on another
// account, so they are removed. This is a deliberate explicit list, not a
// CLAUDE_CODE_ prefix strip — a prefix strip would destroy deliberate user
// configuration (CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_MAX_OUTPUT_TOKENS, …).
// Credential vars (ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN,
// CLAUDE_CONFIG_DIR) are stripped separately by shim.AccountEnv; BFFS_ACCOUNT
// is meaningless to real claude but stripped for hygiene — the account
// decision was already made by the caller.
var sessionMarkers = map[string]bool{
	"CLAUDECODE":                   true,
	"CLAUDE_PID":                   true,
	"CLAUDE_EFFORT":                true,
	"CLAUDE_CODE_ENTRYPOINT":       true,
	"CLAUDE_CODE_SESSION_ID":       true,
	"CLAUDE_CODE_CHILD_SESSION":    true,
	"CLAUDE_CODE_MESSAGING_SOCKET": true,
	"CLAUDE_CODE_MESSAGING_TOKEN":  true,
	"CLAUDE_CODE_EXECPATH":         true,
	"BFFS_ACCOUNT":                 true,
}

// Request describes one delegated claude run.
type Request struct {
	// Account is the bffs account to run on (required; callers resolve any
	// "pick for me" default before reaching the runner).
	Account string
	// Dir is the child's working directory. Required and pre-normalized by
	// the caller; it must exist (store.NormalizePath alone tolerates missing
	// dirs, a delegated run into one must not).
	Dir string
	// Args is the claude argv tail, passed verbatim.
	Args []string
	// Timeout, when >0, layers a deadline on top of ctx.
	Timeout time.Duration
	// ProcessGroup gives the child its own process group and kills the whole
	// group on cancellation. Set it when stdio is captured; leave it false
	// for interactive tty use — an own group would detach the child from the
	// terminal's foreground group (no Ctrl-C, SIGTTIN on tty reads).
	ProcessGroup bool

	Stdout, Stderr io.Writer // nil → discarded
	Stdin          io.Reader // nil → no stdin
}

// Run spawns claude on the requested account and waits. It returns the
// child's exit code; exitCode -1 with a non-nil error means the child never
// ran or was killed (timeout/cancel). A nonzero exit with nil error is a
// normal child failure for the caller to interpret.
func Run(ctx context.Context, cfgDir string, req Request) (int, error) {
	accs, err := store.LoadAccounts(cfgDir)
	if err != nil {
		return -1, err
	}
	acc, ok := accs.Get(req.Account)
	if !ok {
		return -1, fmt.Errorf("unknown account %q; known accounts: %v", req.Account, accs.Names())
	}
	if req.Dir == "" {
		return -1, errors.New("no working directory given")
	}
	if info, err := os.Stat(req.Dir); err != nil || !info.IsDir() {
		return -1, fmt.Errorf("directory %s does not exist", req.Dir)
	}
	// Before SyncOAuthSessionDir: the sync would EnsureDir a missing session
	// dir, and a bare one sends headless claude into its first-run wizard.
	if acc.Type == store.TypeOAuth {
		sessDir := sessions.Dir(cfgDir, acc.Name)
		if info, err := os.Stat(sessDir); err != nil || !info.IsDir() {
			return -1, fmt.Errorf("oauth account %q has not been logged in (no session dir at %s); run `bffs login %s` in a terminal first", acc.Name, sessDir, acc.Name)
		}
	}

	env := shim.AccountEnv(stripMarkers(os.Environ()), acc, cfgDir)
	if acc.Type == store.TypeOAuth {
		shim.SyncOAuthSessionDir(cfgDir, acc)
	}
	realPath, err := shim.FindRealClaude(cfgDir)
	if err != nil {
		return -1, err
	}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	c := exec.CommandContext(ctx, realPath, req.Args...)
	c.Dir = req.Dir
	c.Env = env
	c.Stdin = req.Stdin
	c.Stdout = orDiscard(req.Stdout)
	c.Stderr = orDiscard(req.Stderr)
	// Bounds Wait when a grandchild inherits the stdout pipe and outlives
	// the child.
	c.WaitDelay = 5 * time.Second
	if req.ProcessGroup {
		setProcessGroup(c)
	}

	// The spawn decision is final: record the delegated launch, same
	// best-effort contract as the shim's logLaunch.
	if !usagelog.Disabled() {
		_ = usagelog.Append(cfgDir, usagelog.Event{
			TS:      time.Now().UTC(),
			Account: acc.Name,
			Type:    string(acc.Type),
			Source:  LaunchSource,
			Cwd:     req.Dir,
		})
	}

	err = c.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ctx.Err() != nil {
			if req.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return -1, fmt.Errorf("claude run timed out after %s and was killed", req.Timeout)
			}
			return -1, fmt.Errorf("claude run canceled and killed: %w", ctx.Err())
		}
		return exitErr.ExitCode(), nil
	}
	return -1, fmt.Errorf("start claude: %w", err)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// stripMarkers drops the session-instance markers from env.
func stripMarkers(env []string) []string {
	out := env[:0]
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if sessionMarkers[key] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
