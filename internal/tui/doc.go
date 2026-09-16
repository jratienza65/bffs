// Package tui is the interactive browser behind bare `bffs`: one
// lazygit-style workspace (docs/plans/tui-v2.md) over bffs's accounts and
// Claude Code's on-disk sessions and auto-memory.
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
// compares elsewhere, its path references and its contents. Below 96
// columns the panels take the width and enter shows the preview; below
// 40×12 a message names the minimum.
//
// Keys follow lazygit: 1-3 jump to a panel, tab/shift+tab (h/l) cycle,
// enter drills in, esc goes back, [ ] switch the tabs, / filters, space
// marks a session or makes an account active, x opens the action menu,
// ? the keys, +/_ force the preview or the panels. Actions: e export to
// file, s send over the LAN, i receive, c copy to account, r rehome, R
// resume in claude, t trust matrix, S sync memory to a full-isolation
// account, L set an account's last-session pointer, p scan memory paths.
// d never deletes. Every action calls the same internal/* engines the
// CLI does — porter, transfer, rehome, trust, runner, claudejson, store —
// never bffs itself.
//
// Overlays (action screens, results, the menu, the keys, the transcript)
// stack in the main pane; the long-op contract of ops.go is unchanged: a
// goroutine feeds one channel the screen re-arms per message and owns
// the cancel for; esc/ctrl+c cancel, q asks, and a confirmed quit waits
// for the goroutine. The listing never opens a transcript (ReadDir +
// Info); titles come from the 64 KiB head/tail windows for the visible
// page; the preview reads only the selected session's windows; every
// rendered string passes transcripts.Sanitize. Imported only from
// cmd/tui_enabled.go; the bffs_notui build tag drops the charm stack.
package tui
