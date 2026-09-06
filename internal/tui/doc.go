// Package tui is the interactive browser behind the bare `bffs sessions`
// and `bffs memory`: a lazygit-style workspace (docs/plans/tui-v2.md)
// over Claude Code's on-disk sessions and auto-memory.
//
// Layout: a side column of four stacked panels — roots, projects,
// sessions|memory (tabs), files (a session's artifacts or a memory
// file's path references) — and a main pane previewing the focused
// panel's selection: root facts, a project's drift tables (sessions and
// memory per root by sha256; trust answers and the last-session pointer
// per account), a session's facts with its per-account row and an
// excerpt from the head/tail windows, a memory file with who reads it
// and how it compares on the other roots. Enter opens a session's full
// transcript, rendered as a conversation, in an overlay.
//
// Keys follow lazygit: 1-4 jump to a panel, tab/shift+tab (h/l) cycle,
// [ ] switch the tabs, / filters, +/_ cycle the screen modes, x opens
// the action menu, ? the keys. Actions: e export to file, s send over
// the LAN, i receive, c copy to account, r rehome, R resume in claude,
// t trust matrix, S sync memory to a full-isolation account, L set an
// account's last-session pointer, p scan memory paths. d never deletes.
// Every action calls the same internal/* engines the CLI does — porter,
// transfer, rehome, trust, runner, claudejson — never bffs itself.
//
// Overlays (action screens, results, the menu, the keys) stack in the
// main pane; the long-op contract of ops.go is unchanged: a goroutine
// feeds one channel the screen re-arms per message and owns the cancel
// for; esc/ctrl+c cancel, q asks, and a confirmed quit waits for the
// goroutine. The listing never opens a transcript (ReadDir + Info);
// titles come from the 64 KiB head/tail windows for the visible page;
// the preview reads only the selected session's windows; every rendered
// string passes transcripts.Sanitize. Imported only from
// cmd/tui_enabled.go; the bffs_notui build tag drops the charm stack.
package tui
