package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jratienza65/bffs/internal/sessions"
	"github.com/jratienza65/bffs/internal/shim"
)

// fakeRealClaude installs a shell script as BFFS_REAL_CLAUDE.
func fakeRealClaude(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake claude")
	}
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv(shim.EnvRealClaude, script)
}

const cannedResultScript = `cat <<'EOF'
{"result":"the answer is 42","session_id":"sid-run-1","is_error":false,"model":"claude-opus-5","total_cost_usd":0.12,"usage":{"input_tokens":100,"output_tokens":200,"cache_creation_input_tokens":10,"cache_read_input_tokens":5000}}
EOF
exit 0
`

func TestRunOnAccountSuccess(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	fakeRealClaude(t, cannedResultScript)
	if err := os.MkdirAll(sessions.Dir(cfg, "work"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := &handlers{cfgDir: cfg}

	_, out, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{
		Prompt: "what is the answer?", Account: "work", Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runOnAccount: %v", err)
	}
	if out.Account != "work" || out.Result != "the answer is 42" || out.SessionID != "sid-run-1" {
		t.Errorf("out: %+v", out)
	}
	if out.CostUSD != 0.12 || out.Usage.Output != 200 || out.Usage.CacheRead != 5000 {
		t.Errorf("usage/cost: %+v", out)
	}
	if out.ExitCode != 0 || out.Note == "" {
		t.Errorf("exit/note: %+v", out)
	}
}

func TestRunOnAccountSecretNotLeaked(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	fakeRealClaude(t, cannedResultScript)
	h := &handlers{cfgDir: cfg}

	// "personal" is the api_key account; its secret goes into the child env
	// but must never surface in the tool output.
	_, out, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{
		Prompt: "task", Account: "personal", Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runOnAccount: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testSecret) {
		t.Error("secret leaked into run_on_account output")
	}
}

func TestRunOnAccountAutoPicksSuggested(t *testing.T) {
	neutralEnv(t)
	cfg := setupStore(t)
	// The script proves which account ran by echoing CLAUDE_CONFIG_DIR into
	// the JSON result.
	fakeRealClaude(t, `printf '{"result":"cfg=%s","session_id":"s","usage":{}}' "$CLAUDE_CONFIG_DIR"`+"\nexit 0\n")
	if err := os.MkdirAll(sessions.Dir(cfg, "work"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h := &handlers{cfgDir: cfg, homeClaudeDir: t.TempDir()}

	_, out, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{
		Prompt: "task", Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runOnAccount: %v", err)
	}
	// "work" is the only oauth account → the usage suggestion.
	if out.Account != "work" {
		t.Errorf("auto-picked account: want work, got %q", out.Account)
	}
	if want := "cfg=" + sessions.Dir(cfg, "work"); out.Result != want {
		t.Errorf("child ran with wrong config dir: %q (want %q)", out.Result, want)
	}
	if !strings.Contains(out.Note, "auto-selected") {
		t.Errorf("note should flag auto-selection: %q", out.Note)
	}
}

func TestRunOnAccountErrors(t *testing.T) {
	neutralEnv(t)

	t.Run("empty prompt does not spawn", func(t *testing.T) {
		cfg := setupStore(t)
		sentinel := filepath.Join(t.TempDir(), "ran")
		fakeRealClaude(t, "touch "+sentinel+"\nexit 0\n")
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{Prompt: "   ", Account: "personal"})
		if err == nil || !strings.Contains(err.Error(), "prompt is required") {
			t.Fatalf("want prompt error, got %v", err)
		}
		if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
			t.Error("child spawned despite empty prompt")
		}
	})

	t.Run("unknown account", func(t *testing.T) {
		cfg := setupStore(t)
		fakeRealClaude(t, "exit 0\n")
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{Prompt: "task", Account: "ghost"})
		if err == nil || !strings.Contains(err.Error(), "unknown account") {
			t.Fatalf("want unknown-account error, got %v", err)
		}
	})

	t.Run("nonzero exit carries stderr", func(t *testing.T) {
		cfg := setupStore(t)
		fakeRealClaude(t, "echo boom >&2\nexit 2\n")
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{Prompt: "task", Account: "personal"})
		if err == nil || !strings.Contains(err.Error(), "exited 2") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("want exit-2-with-stderr error, got %v", err)
		}
	})

	t.Run("garbage stdout", func(t *testing.T) {
		cfg := setupStore(t)
		fakeRealClaude(t, "echo not json\nexit 0\n")
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{Prompt: "task", Account: "personal"})
		if err == nil || !strings.Contains(err.Error(), "could not parse") {
			t.Fatalf("want parse error, got %v", err)
		}
	})

	t.Run("output cap exceeded", func(t *testing.T) {
		cfg := setupStore(t)
		// ~600KB of output blows the 512KB cap.
		fakeRealClaude(t, `i=0
while [ $i -lt 600 ]; do head -c 1024 /dev/zero | tr '\0' 'x'; i=$((i+1)); done
exit 0
`)
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{Prompt: "task", Account: "personal"})
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("want cap error, got %v", err)
		}
	})

	t.Run("comma in allowed_tools rejected", func(t *testing.T) {
		cfg := setupStore(t)
		fakeRealClaude(t, "exit 0\n")
		h := &handlers{cfgDir: cfg}
		_, _, err := h.runOnAccount(context.Background(), nil, RunOnAccountIn{
			Prompt: "task", Account: "personal", AllowedTools: []string{"Read,Edit"},
		})
		if err == nil || !strings.Contains(err.Error(), "comma") {
			t.Fatalf("want comma error, got %v", err)
		}
	})
}
