# Accounts are selected by env vars on one child process

Claude Code reads its credentials from its own config tree, and a
subscription (oauth) login is not a token bffs could hold: it lives in
the OS keychain or in `.credentials.json`, written and refreshed by
Claude itself. Switching accounts therefore cannot mean moving
credentials around. We decided that switching means setting one
environment variable on the `claude` process bffs is about to exec, and
nothing else:

- an **api_key** account sets `ANTHROPIC_API_KEY` to the stored key;
- an **oauth** account sets `CLAUDE_CONFIG_DIR` to
  `<bffs-home>/sessions/<name>/`, and Claude reads its whole config tree
  — identity, credentials, history — from there.

No global state is touched at launch, so two shells can run two accounts
at once, a per-project pin works for both account types, and a bffs that
crashes mid-command leaves nothing half-switched. bffs never reads,
writes or copies an oauth credential; on macOS the per-account keychain
entry is derived *by Claude* from the config dir it was given.

## Considered Options

- **Copying credential files into place before launch** — a race between
  two shells, a refresh written to the wrong account, and bffs holding
  secrets it has no reason to hold.
- **Symlinking `~/.claude.json` at the active account** — global state
  again: one active account per machine, and a `claude` already running
  reads the new one on its next write.
- **Wrapping the keychain** — bffs would have to reimplement Claude's
  service-name derivation and keep up with it.

## Consequences

Everything is per-invocation, so every command that changes an account
says so: the change applies to the *next* `claude` launch, and the MCP
tools repeat it in their results. A running session cannot be moved to
another account. Because the account is fixed at exec time, an in-session
subagent cannot switch accounts either — delegation means spawning a
fresh headless child (`bffs run`, `internal/runner`), which strips the
session-instance markers so the child does not think it is the parent.
