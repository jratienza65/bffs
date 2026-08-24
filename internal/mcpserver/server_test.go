package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jratienza65/bffs/internal/store"
)

// TestServerEndToEnd exercises the full MCP wire: a real client session over
// in-memory transports, schema-validated calls, and on-disk effects.
func TestServerEndToEnd(t *testing.T) {
	neutralEnv(t)
	ctx := context.Background()
	cfg := setupStore(t)

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := New(cfg, "test").Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"list_accounts": false, "resolve_account": false, "switch_account": false,
		"pin_account": false, "unpin_account": false, "check_shim": false,
		"account_usage": false, "run_on_account": false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("%s: nil input schema", tool.Name)
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s: nil output schema", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s: empty description", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q not listed", name)
		}
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "switch_account",
		Arguments: map[string]any{"name": "personal"},
	})
	if err != nil {
		t.Fatalf("CallTool(switch_account): %v", err)
	}
	if res.IsError {
		t.Fatalf("switch_account errored: %+v", res.Content)
	}
	state, err := store.LoadState(cfg)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state.Active != "personal" {
		t.Errorf("state.toml not updated through the wire: active=%q", state.Active)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_accounts"})
	if err != nil {
		t.Fatalf("CallTool(list_accounts): %v", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(raw), testSecret) {
		t.Error("secret leaked through the wire in list_accounts")
	}

	// A tool error must surface as IsError with content, not a protocol error.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "switch_account",
		Arguments: map[string]any{"name": "ghost"},
	})
	if err != nil {
		t.Fatalf("CallTool(switch_account ghost): %v", err)
	}
	if !res.IsError {
		t.Error("unknown account should produce IsError=true")
	}
}
