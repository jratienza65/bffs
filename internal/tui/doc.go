// Package tui is the interactive browser behind `bffs sessions` and
// `bffs memory` when they run on a terminal: a bubbletea program (Elm
// architecture, one screen stack) over the same read model the tables
// use — internal/transcripts for the catalog, internal/usage for the
// best-effort account attribution, internal/imports for import records.
// It never shells out to bffs and never opens a transcript beyond the
// 64 KiB head and tail windows Claude's own picker reads.
//
// Screens: Roots (skipped when there is one) → Projects → Sessions |
// Memories (tab) → Show / file viewer / scan paths. Every screen loads
// its data in a tea.Cmd and renders a placeholder until the message
// lands; titles are resolved lazily for the visible page ±1 and cached
// by (path, mtime, size). Every transcript-derived string passes through
// transcripts.Sanitize before it is rendered — lipgloss does not strip
// control sequences.
//
// Dependency rule: the package is imported only by cmd/tui_enabled.go
// (build tag !bffs_notui); the shim and the MCP server never link it.
// Action keys (e s i c r R t d) are reserved for the action screens and
// answer with a status-line hint until then.
package tui
