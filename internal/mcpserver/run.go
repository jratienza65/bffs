package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/runner"
	"github.com/jratienza65/bffs/internal/usage"
)

const (
	// runStdoutCap is a hard cap, not a truncation point: with
	// --output-format json the child's stdout is one JSON object, and a
	// truncated object is unparseable, so overflowing is an error.
	runStdoutCap  = 512 << 10
	runStderrTail = 8 << 10

	runDefaultTimeoutSecs = 300
	runMinTimeoutSecs     = 10
	// runMaxTimeoutSecs stays under Claude Code's ~10-minute tool-call
	// ceiling with margin for spawn + marshaling. v1 is synchronous; if
	// longer runs are ever needed, the path is an async start/poll tool
	// pair, not a bigger clamp.
	runMaxTimeoutSecs = 570
)

type RunOnAccountIn struct {
	Prompt         string   `json:"prompt" jsonschema:"the task for the delegated session; it starts with NO context from this conversation, so include everything it needs"`
	Account        string   `json:"account,omitempty" jsonschema:"bffs account to run on; empty picks the account_usage suggested account (most headroom)"`
	Directory      string   `json:"directory,omitempty" jsonschema:"working directory for the child; defaults to the server's working directory, which may NOT be the project dir - pass the project root explicitly"`
	Model          string   `json:"model,omitempty" jsonschema:"claude --model override"`
	AllowedTools   []string `json:"allowed_tools,omitempty" jsonschema:"claude --allowedTools entries, e.g. Bash(git:*) or Edit; default headless permissions allow read-only tools only"`
	MaxTurns       int      `json:"max_turns,omitempty" jsonschema:"limit on the delegated session's agentic turns"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"default 300, clamped to 10..570 (MCP tool calls hard-cap near 10 minutes)"`
}

type ChildUsage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
}

type RunOnAccountOut struct {
	Account         string     `json:"account" jsonschema:"account the run was billed to (resolved, when auto-picked)"`
	Result          string     `json:"result"`
	SessionID       string     `json:"session_id,omitempty"`
	Model           string     `json:"model,omitempty"`
	Usage           ChildUsage `json:"usage"`
	CostUSD         float64    `json:"cost_usd,omitempty"`
	DurationSeconds float64    `json:"duration_seconds"`
	ExitCode        int        `json:"exit_code"`
	Note            string     `json:"note"`
}

// --permission-mode and --dangerously-skip-permissions are deliberately NOT
// surfaced here: this tool is a lever a model pulls on the user's other
// accounts, and permission escalation must stay a human decision — at most
// via explicit allowed_tools entries, never a blanket bypass the model can
// request for itself.
func (h *handlers) runOnAccount(ctx context.Context, req *mcp.CallToolRequest, in RunOnAccountIn) (*mcp.CallToolResult, RunOnAccountOut, error) {
	var out RunOnAccountOut
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, out, errors.New("prompt is required")
	}
	dir, err := normalizeDir(in.Directory)
	if err != nil {
		return nil, out, err
	}
	account := in.Account
	autoPicked := false
	if account == "" {
		rep, err := usage.Collect(h.cfgDir, usage.Options{HomeClaudeDir: h.homeClaudeDir})
		if err != nil {
			return nil, out, err
		}
		account = rep.Suggested
		autoPicked = true
		if account == "" {
			return nil, out, errors.New("no account specified and none has clear headroom; pass account explicitly (see account_usage)")
		}
	}

	args := []string{"-p", prompt, "--output-format", "json"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if len(in.AllowedTools) > 0 {
		for _, tool := range in.AllowedTools {
			if strings.Contains(tool, ",") {
				return nil, out, fmt.Errorf("allowed_tools entry %q must not contain a comma; pass one tool rule per entry", tool)
			}
		}
		args = append(args, "--allowedTools", strings.Join(in.AllowedTools, ","))
	}
	if in.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(in.MaxTurns))
	}
	timeoutSecs := runDefaultTimeoutSecs
	if in.TimeoutSeconds > 0 {
		timeoutSecs = min(max(in.TimeoutSeconds, runMinTimeoutSecs), runMaxTimeoutSecs)
	}

	stdout := &limitWriter{max: runStdoutCap}
	stderr := &tailWriter{max: runStderrTail}
	started := time.Now()
	exit, err := runner.Run(ctx, h.cfgDir, runner.Request{
		Account:      account,
		Dir:          dir,
		Args:         args,
		Timeout:      time.Duration(timeoutSecs) * time.Second,
		ProcessGroup: true,
		Stdout:       stdout,
		Stderr:       stderr,
	})
	out.DurationSeconds = time.Since(started).Seconds()
	out.Account = account
	out.ExitCode = exit
	if err != nil {
		if tail := strings.TrimSpace(string(stderr.buf)); tail != "" {
			return nil, out, fmt.Errorf("%w; stderr: %s", err, tail)
		}
		return nil, out, err
	}
	if stdout.overflow > 0 {
		return nil, out, fmt.Errorf("child output exceeded %dKB (result too large for an MCP tool response); narrow the prompt or lower max_turns", runStdoutCap>>10)
	}
	if exit != 0 {
		detail := strings.TrimSpace(string(stderr.buf))
		if detail == "" {
			detail = firstN(string(stdout.buf), 1024)
		}
		return nil, out, fmt.Errorf("claude exited %d on account %q: %s", exit, account, detail)
	}

	// Tolerant decode: field spellings vary a little across claude versions;
	// missing optional fields degrade to zero rather than failing the run.
	var res struct {
		Result       string  `json:"result"`
		SessionID    string  `json:"session_id"`
		IsError      bool    `json:"is_error"`
		Model        string  `json:"model"`
		CostUSD      float64 `json:"cost_usd"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(stdout.buf, &res); err != nil {
		return nil, out, fmt.Errorf("could not parse claude --output-format json output: %v; first bytes: %.200s", err, string(stdout.buf))
	}
	out.Result = res.Result
	out.SessionID = res.SessionID
	out.Model = res.Model
	out.CostUSD = res.CostUSD
	if out.CostUSD == 0 {
		out.CostUSD = res.TotalCostUSD
	}
	out.Usage = ChildUsage{
		Input:         res.Usage.InputTokens,
		Output:        res.Usage.OutputTokens,
		CacheCreation: res.Usage.CacheCreationInputTokens,
		CacheRead:     res.Usage.CacheReadInputTokens,
	}

	note := fmt.Sprintf("Ran as an independent headless claude session on account %q; it shared no conversation context with this session and its usage bills that account. The transcript (session_id %s) lives in that account's own config tree.", account, res.SessionID)
	if autoPicked {
		note = "Account auto-selected by headroom (account_usage suggestion). " + note
	}
	if res.IsError {
		note = "The delegated session reported an error; result contains its explanation. " + note
	}
	out.Note = note
	return nil, out, nil
}

// limitWriter keeps the first max bytes and counts everything beyond.
type limitWriter struct {
	buf      []byte
	max      int
	overflow int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		n := min(room, len(p))
		w.buf = append(w.buf, p[:n]...)
		w.overflow += int64(len(p) - n)
	} else {
		w.overflow += int64(len(p))
	}
	return len(p), nil
}

// tailWriter keeps the last max bytes — the head of a stderr stream is the
// least diagnostic part.
type tailWriter struct {
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
