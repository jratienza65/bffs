package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jratienza65/bffs/internal/claudejson"
	"github.com/jratienza65/bffs/internal/mcpserver"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Expose bffs to Claude Code as an MCP server",
	Long: `Lets a Claude Code session inspect and stage bffs account changes itself,
via the Model Context Protocol: list accounts, explain which account a
directory resolves to and why, set the global default, pin/unpin directory
rules, and diagnose the shim.

Changes staged this way take effect the next time claude is launched through
the shim — they never re-point a claude session that is already running.

Run ` + "`bffs mcp install`" + ` once to register the server; Claude Code then starts
` + "`bffs mcp serve`" + ` itself and the tools appear as mcp__bffs__<tool>.`,
}

var mcpServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the MCP server on stdio (Claude Code launches this)",
	Long: `Speaks the Model Context Protocol over stdin/stdout. Claude Code starts
this process itself once the server is registered (see ` + "`bffs mcp install`" + `);
it is not meant for interactive use and exits on stdin EOF.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return mcpserver.Serve(ctx, dir, Version)
	},
}

var mcpInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register the bffs MCP server with Claude Code (user scope, all projects)",
	Long: `Adds a bffs entry to the mcpServers map of ~/.claude.json and of every
per-account session .claude.json under the bffs config dir. Both are needed:
Claude Code reads user-scope MCP servers from the .claude.json inside its
config dir, and each bffs oauth account runs with its own isolated config dir.
New oauth accounts inherit the entry automatically when ` + "`bffs login`" + ` seeds
their session dir from ~/.claude.json.

The registration records the absolute path of this bffs binary and of the
config dir, so re-run install after moving either. Do not run it via
` + "`go run`" + ` — that would bake in a temporary binary path.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate self: %w", err)
		}
		homeJSON, err := claudejson.Path()
		if err != nil {
			return err
		}
		touched, err := mcpserver.Install(homeJSON, dir, self)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		for _, f := range touched {
			fmt.Fprintf(out, "registered in %s\n", short(f))
		}
		fmt.Fprintf(out, "\nRegistered %q with Claude Code. Restart claude (or run /mcp) to connect; tools appear as mcp__%s__<tool>.\n", mcpserver.ServerName, mcpserver.ServerName)
		fmt.Fprintln(out, "Re-run `bffs mcp install` if you move the bffs binary or the config dir; undo with `bffs mcp uninstall`.")
		return nil
	},
}

var mcpUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the bffs MCP server registration from Claude Code",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := mustConfigDir(cmd)
		homeJSON, err := claudejson.Path()
		if err != nil {
			return err
		}
		changed, err := mcpserver.Uninstall(homeJSON, dir)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(changed) == 0 {
			fmt.Fprintln(out, "bffs MCP server was not registered.")
			return nil
		}
		for _, f := range changed {
			fmt.Fprintf(out, "removed from %s\n", short(f))
		}
		return nil
	},
}

func init() {
	mcpCmd.AddCommand(mcpServeCmd, mcpInstallCmd, mcpUninstallCmd)
	rootCmd.AddCommand(mcpCmd)
}
