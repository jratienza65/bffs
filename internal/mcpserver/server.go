// Package mcpserver exposes bffs over the Model Context Protocol so a Claude
// Code session can inspect and stage account changes itself: list accounts,
// explain which account a directory resolves to and why, set the global
// default, pin/unpin directory rules, estimate per-account usage headroom,
// and delegate headless runs to another account.
//
// Stdout discipline: on the stdio transport stdout carries JSON-RPC
// exclusively, so nothing in this package may write to stdout. Cobra wiring
// lives in cmd/mcp.go; this package must not import cmd (the version string
// is passed in instead).
package mcpserver

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the MCP server name Claude Code sees; tools surface to the
// model as mcp__bffs__<tool>.
const ServerName = "bffs"

// New builds the MCP server with all eight tools registered. version feeds
// the MCP handshake (callers pass cmd.Version).
func New(cfgDir, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Title:   "bffs Claude Code account switcher",
		Version: version,
	}, nil)
	h := &handlers{cfgDir: cfgDir}

	// All tools except run_on_account operate on local bffs config only.
	closedWorld := false
	openWorld := true
	nonDestructive := false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}
	write := &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &nonDestructive, OpenWorldHint: &closedWorld}
	// run_on_account leaves the local-config world: it spends real quota on
	// the Anthropic API and, with allowed_tools, can modify files.
	delegate := &mcp.ToolAnnotations{OpenWorldHint: &openWorld}

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_accounts",
		Description: "List the Claude Code accounts bffs manages: name, type (oauth or api_key), email, " +
			"isolation preset, and which one is the global default. Secrets are never returned.",
		Annotations: readOnly,
	}, h.listAccounts)

	mcp.AddTool(s, &mcp.Tool{
		Name: "resolve_account",
		Description: "Report which bffs account a `claude` launched from a directory would use, and why. " +
			"Precedence: BFFS_ACCOUNT env > bffs.toml in the project > directory rule > global default. " +
			"Pass the project directory explicitly; the server's own working directory may not be the project. " +
			nextLaunchCaveat,
		Annotations: readOnly,
	}, h.resolveAccount)

	mcp.AddTool(s, &mcp.Tool{
		Name: "switch_account",
		Description: "Set the bffs global default account — the one used when no bffs.toml, directory rule, " +
			"or BFFS_ACCOUNT applies. " + nextLaunchCaveat,
		Annotations: write,
	}, h.switchAccount)

	mcp.AddTool(s, &mcp.Tool{
		Name: "pin_account",
		Description: "Pin a directory (and everything beneath it) to a bffs account via a directory rule. " +
			"Note: a bffs.toml checked into the project outranks the rule, and BFFS_ACCOUNT outranks both. " +
			nextLaunchCaveat,
		Annotations: write,
	}, h.pinAccount)

	mcp.AddTool(s, &mcp.Tool{
		Name: "unpin_account",
		Description: "Remove the bffs directory rule for exactly this directory. A directory that only " +
			"inherits a parent rule is not changed; the result names the parent rule to remove instead. " +
			nextLaunchCaveat,
		Annotations: write,
	}, h.unpinAccount)

	mcp.AddTool(s, &mcp.Tool{
		Name: "account_usage",
		Description: "Estimate per-account usage headroom before Claude subscription rate limits: recent token " +
			"burn (5h and horizon windows), last-used times, detected limit events, plan tiers, and a suggested " +
			"account with the most headroom. Heuristic and best-effort — read the note field for caveats.",
		Annotations: readOnly,
	}, h.accountUsage)

	mcp.AddTool(s, &mcp.Tool{
		Name: "run_on_account",
		Description: "Delegate a prompt to an independent headless claude session running on another bffs-managed " +
			"account — an effective subagent that bills that account's rate limits instead of this session's. The " +
			"child shares NO conversation context: the prompt must be fully self-contained. Default permissions are " +
			"conservative (read-only tools only); grant more only via allowed_tools. Blocks until the run finishes, " +
			"up to timeout_seconds (default 300). Empty account auto-picks the account_usage suggestion.",
		Annotations: delegate,
	}, h.runOnAccount)

	mcp.AddTool(s, &mcp.Tool{
		Name: "check_shim",
		Description: "Diagnose whether the bffs `claude` shim is installed and actually wins on PATH in each " +
			"shell invocation mode (interactive, login, non-interactive). Use when account switching seems to " +
			"have no effect. May take ~15 seconds (it probes real shell startup files).",
		Annotations: readOnly,
	}, h.checkShim)

	return s
}

// Serve runs the server on stdio until stdin EOF or ctx cancellation. Both
// are how Claude Code routinely ends a session (close stdin / SIGTERM), so
// they exit cleanly rather than as errors.
func Serve(ctx context.Context, cfgDir, version string) error {
	err := New(cfgDir, version).Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
