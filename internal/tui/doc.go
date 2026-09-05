// Package tui is the interactive browser behind `bffs sessions` and
// `bffs memory` when they run on a terminal: a bubbletea program (Elm
// architecture, one screen stack) over the same read model the tables
// use — internal/transcripts for the catalog, internal/usage for the
// best-effort account attribution, internal/imports for import records
// — and the same engines the commands drive for its actions: porter
// (export, import, copy), transfer (serve, fetch), rehome, trust and
// runner. It never shells out to bffs and never opens a transcript
// beyond the 64 KiB head and tail windows Claude's own picker reads.
//
// Screens: Roots (skipped when there is one) → Projects → Sessions |
// Memories (tab) → Show / file viewer / scan paths, and from the
// sessions and memories screens the action screens (plan §11): e
// ExportFile, s Serve over the LAN, i Receive (from every screen), c
// Copy to account, r Rehome, R Resume in claude (tea.ExecProcess hands
// the terminal to `claude --resume` on the right account), t Trust. Each
// action screen runs its long operation in a goroutine started by a
// tea.Cmd; bundle.Progress and transfer events flow back over a channel
// whose wait command is re-armed per message, a bar and a spinner show
// them, the screen owns the context.CancelFunc (esc and ctrl+c cancel; q
// asks before quitting), navigation is blocked meanwhile, and a Result
// screen ends the flow (esc reloads the list underneath). A pairing code
// is rendered on the serve screen only — never in the status line, a
// log line or a result. d never deletes: `bffs sessions rm` does.
//
// Every screen loads its data in a tea.Cmd and renders a placeholder
// until the message lands; titles are resolved lazily for the visible
// page ±1 and cached by (path, mtime, size). Every transcript-, peer-
// or manifest-derived string passes through transcripts.Sanitize before
// it is rendered — lipgloss does not strip control sequences.
//
// Dependency rule: the package is imported only by cmd/tui_enabled.go
// (build tag !bffs_notui); the shim and the MCP server never link it.
// It may import transcripts, porter, rehome, bundle, trust, transfer,
// store, usage, runner and imports — never cmd, shim or mcpserver.
package tui
