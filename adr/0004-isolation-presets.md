# An oauth account isolates identity, and symlinks the rest by default

`CLAUDE_CONFIG_DIR` moves Claude's *whole* configuration tree, not just
its credentials: settings, skills, plugins, projects, todos, history.
Pointing it at a per-account directory therefore isolates far more than
the account, and a user who adds a second account would lose their
settings in it. We decided to make how much is isolated an explicit
per-account choice with two presets (`internal/store.IsolationPreset`):

- **partial** (the default): every entry in `~/.claude/` except
  `.claude.json` and `.credentials.json` is symlinked into the account's
  session dir. Only identity and auth are per-account; everything else is
  the one tree the user already had.
- **full**: nothing is symlinked. The account is a fresh Claude world.

`internal/sessions.SyncSymlinks` is idempotent and lenient — it adds
what is missing, removes what is no longer wanted, and *skips* any path
where Claude wrote a real file in the way, returning the skips rather
than failing. The shim calls it on every oauth launch, so an entry added
to `~/.claude/` later shows up without a command.

The account name `home` is reserved: it names the unmanaged `~/.claude`
in trust sync, copy and the browser, so the tree that belongs to nobody
still has a name that can be typed.

## Considered Options

- **Full isolation only** — correct and unhelpful: a second account
  starts with no settings, no skills and no history, and the first-run
  wizard again.
- **No isolation, swapping `.claude.json` in place** — one account at a
  time, and a running session rewrites the file under the switch.
- **A fine-grained list of isolated paths** — every Claude release would
  move it, and a wrong entry silently isolates history or todos.

## Consequences

Under partial isolation, sessions and auto-memory are *shared* between
accounts, because `projects/` is one directory behind several symlinks.
That is why ownership in the browser is per root and never per account,
why `bffs copy` between two partial accounts is a no-op that prints the
trust-sync hint instead, and why usage attribution needs bffs-side
evidence (the launch log) rather than the transcript pool. Switching an
account's preset after the fact is `bffs reisolate`, which re-runs the
sync rather than moving files.
