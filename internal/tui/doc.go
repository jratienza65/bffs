// Package tui is the interactive browser behind bare `bffs`: one
// lazygit-style workspace over bffs's accounts and Claude Code's
// on-disk sessions and auto-memory. CLAUDE.md states the invariants;
// this is where the shape of the package is written down.
//
// Layout: a side column of three stacked panels — 1 accounts (the
// perspective: which pool is browsed and which column the drift tables
// mark), 2 projects, 3 sessions|memory (tabs) — and a main pane that
// previews the focused panel's selection: an account's pool and what its
// .claude.json records; a project's summary and drift tables (sessions
// and memory per root by sha256; trust answers and the last-session
// pointer per account); a session's title, one-line summary, resume
// command and excerpt from the head/tail windows, then its per-account
// row, files and details; a memory file with who reads it, how it
// compares elsewhere, its path references and its contents, rendered as
// the Markdown it is. Below 96 columns (sideAndMainMinWidth) the panels
// take the width and enter shows the preview; below 40×12 a message
// names the minimum and nothing else is drawn.
//
// Keys follow lazygit: 1-3 jump to a panel, tab/shift+tab (h/l) cycle,
// enter drills in, esc goes back, [ ] switch the tabs, / filters, space
// marks a session or makes an account active, x opens the action menu,
// ? the keys, +/_ force the preview or the panels, T cycles the theme,
// y copies the focused row's path. Actions: e export to file, s send
// over the LAN, i receive, c copy to account, r rehome, R resume in
// claude, t trust matrix, S sync memory to a full-isolation account, D
// diff a memory file against the other roots, L set an account's
// last-session pointer, p scan memory paths, w the transfer wizard.
// d never deletes (ADR 0008). Every action calls the same internal/*
// engines the CLI does — porter, transfer, rehome, trust, runner,
// claudejson, store — never bffs itself.
//
// Structure. app.go is the root model: the workspace plus a stack of
// overlays drawn in the main pane, a one-line status note and the help
// bubble. Screens never reach into the app or into each other: they
// emit a push/pop/replace message and the root switches. workspace.go
// owns the three panels, the selection chain (account → project → item,
// re-derived by sync after every cursor move, loading only what
// changed), the key routing and the hand-drawn frame. Each panel owns
// its view (panel.go: offset, rowsH, lastIndex) and draws its own rows
// rather than calling list.View, because a bubbles list derives Index
// from its page and cannot scroll without moving the cursor.
//
// The long-op contract (ops.go): a goroutine feeds one buffered channel
// that the screen re-arms per message and owns the cancel for; esc and
// ctrl+c cancel, q asks, and a confirmed quit waits up to five seconds
// for the goroutine. Every confirmation renders through scrollBox, so a
// long plan can never push the [y/N] off-screen.
//
// Mouse (tea.MouseModeCellMotion, declared on the view): a click
// focuses the panel and picks the row under the pointer, a click on the
// preview hands it the keys, and the wheel scrolls whatever is under
// the pointer without ever moving a selection. Dragging selects text in
// the preview and in the document overlays (selection.go) in document
// coordinates, auto-scrolling past an edge; release copies through
// OSC 52, y copies again, esc lets go. Paths are OSC 8 hyperlinks built
// only from absolute local paths, sanitised and escaped (link.go).
//
// Rendering. Frames are drawn by hand (cell/titled/bar over
// x/ansi) with a one-cell gutter between the side column and the main
// pane. Colour is a set of semantic tokens in styles.go set by
// applyTheme from one of six palettes, each with a hex pair per
// background and a 16-colour rung per role (theme.go); NO_COLOR gives
// mono, $BFFS_THEME and state.toml pick a palette, T cycles and saves.
// Colour is never the only signal: every symbol comes from one glyph
// set with a BFFS_ASCII opt-in (glyphs.go), notes carry a kind and a
// mark (notes.go), and the footer leads with the mode so it is always
// clear whether letters act or type. Markdown is rendered by
// markdown.go and codeblock.go rather than Glamour, whose package init
// costs the shim more than its budget (ADR 0001).
//
// Reading is bounded. The listing never opens a transcript (ReadDir +
// Info); titles come from Claude's 64 KiB head/tail windows for the
// visible page and are cached by path, size and mtime; the transcript
// viewer stops at 8 MiB and a memory file at 1 MB; every rendered
// string passes transcripts.Sanitize (ADR 0009). Nothing that reads a
// file runs in Update or View — the loaders are commands, and
// bench_test.go is the guard.
//
// Tests: model tests drive the app through a synchronous harness
// (app_test.go) and never start a program; frame_test.go renders golden
// frames and sweeps every state at twelve terminal sizes; paint_test.go
// reads the escapes the goldens strip; tools/drive.py drives the real
// binary under a pty. BFFS_DEBUG=<file> logs one line per event.
//
// Imported only from cmd/tui_enabled.go; the bffs_notui build tag drops
// the charm stack and CI vets both.
package tui
