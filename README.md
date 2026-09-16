# BFFs

A small Go CLI for using the [Claude Code](https://claude.com/claude-code) `claude` command with multiple distinct accounts — globally or scoped per project.

## What it does

- Stores multiple Claude accounts (OAuth subscription logins *or* `sk-ant-...` API keys) under named entries.
- Picks one to be the **global default**.
- A project can pin its own account by dropping a `bffs.toml` in its root — every `claude` invocation made anywhere inside that tree uses that account.
- Installs a tiny `claude` shim at the front of `PATH`. The shim resolves the right account, sets the matching env var (`ANTHROPIC_API_KEY` for api_key, `CLAUDE_CONFIG_DIR` for oauth), and execs the real `claude`.

For oauth accounts, isolation works via Claude Code's own `CLAUDE_CONFIG_DIR` mechanism: each account gets its own session dir under `<bffs-config>/sessions/<name>/`, with its own `.claude.json`, its own `.credentials.json` (or hashed-suffix Keychain entry on macOS), and — depending on the chosen isolation preset — its own or shared settings/skills/plugins. Concurrent `claude` sessions on different oauth accounts cannot collide.

## Install

```bash
go install github.com/jratienza65/bffs@latest
bffs init        # installs the shim, prints the PATH snippet
```

After adding the printed snippet to your shell rc and re-opening your terminal, `which claude` should point at the shim (default: `~/.bffs/bin/claude` on macOS/Linux, `%LOCALAPPDATA%\bffs\bin\claude.exe` on Windows), with the real `claude` still resolvable later on `PATH`.

The default install dir is intentionally a dedicated bffs directory rather than `~/.local/bin` so the shim doesn't collide with Claude Code's own install script. Override with `bffs init --dir <path>` (one-shot) or `BFFS_SHIM_DIR=<path>` (persistent — set in your shell rc).

## The core commands

```bash
bffs login [name]            # browser OAuth flow into a per-account session dir
bffs add <name>              # add an api_key account (sk-ant-...)
bffs switch <name>           # set the global default (no global side effects)
bffs reisolate <name> [--preset=...]  # change an oauth account's isolation level
bffs show                    # show what `claude` would use from CWD
bffs list                    # all configured accounts
```

Other commands: `rename`, `remove`, `init`, `exec`.

### Adding an OAuth (Claude subscription) account

```bash
bffs login work          # opens the browser, you sign in; saved as "work"
bffs login personal      # same again with a different account
bffs switch work         # next `claude` invocation uses "work"
```

Each login creates a fresh per-account session dir and runs `claude auth login` against it. The credentials Claude Code writes — Keychain entry on macOS (with a per-dir hash suffix), `.credentials.json` elsewhere — never collide with other accounts' credentials.

### Adding an api_key account

```bash
bffs add work --secret sk-ant-...       # or omit --secret to be prompted
```

## Per-project pinning

Drop a file named `bffs.toml` in your project root:

```toml
account = "personal"
```

Any `claude` invocation made anywhere inside that tree resolves to `personal`, no matter which account is the global default. This works for both `api_key` and `oauth` accounts — the shim's per-invocation env var injection covers both.

## Isolation presets (oauth only)

`CLAUDE_CONFIG_DIR` isolates everything in a Claude Code config tree, not just credentials. To control how much actually gets isolated vs shared with your `~/.claude/`, pick a preset at `bffs login`-time (or change later with `bffs reisolate`):

| Preset             | What's per-account               | What's symlinked back to `~/.claude/`                                            |
|--------------------|----------------------------------|----------------------------------------------------------------------------------|
| `partial` (default)| `.claude.json`, `.credentials.json` only | Every other entry in `~/.claude/` (settings, skills, plugins, history, projects, todos, …) — drop-in feel |
| `full`             | Everything                       | Nothing — fresh Claude Code world per account                                    |

Default is `partial`. Override per-account with `bffs login --preset=full`, change later with `bffs reisolate <name> --preset=full`. Set a different global default by editing the `isolation` field in `state.toml`.

The shim re-runs the symlink reconciliation on every oauth invocation, so anything claude adds to `~/.claude/` later (a new skill, a new plugin) shows up in your per-account dirs automatically. If a real file is in the way of a desired symlink (e.g. claude wrote something into the session dir directly), that path is skipped, the user's file is preserved, and a one-line warning goes to stderr.

## Account resolution order

Highest priority wins:

1. `BFFS_ACCOUNT` env var
2. The nearest `bffs.toml` walking up from CWD
3. The global default set by `bffs switch`
4. Fall through — `claude` runs with its own credentials, untouched

## Storage

Plain TOML at `0600` under your OS config dir (`~/.config/bffs` on Linux, `~/Library/Application Support/bffs` on macOS, `%AppData%\bffs` on Windows). Per-account session dirs live under `<config>/sessions/<name>/`. Override the config dir with `BFFS_HOME`.

For oauth accounts the actual credential never lives in `accounts.toml` — it's in Claude Code's own credential store (Keychain on macOS, `.credentials.json` elsewhere) under a per-account-derived service name / path. `accounts.toml` only stores display metadata (email, OAuthAccountMeta cache, isolation preset).

See [SECURITY.md](SECURITY.md) for the threat model and [`examples/`](examples/) for sample [`accounts.toml`](examples/accounts.toml), [`state.toml`](examples/state.toml), and [`bffs.toml`](examples/bffs.toml) files.

## Without the shim

If you don't want a shim on `PATH`, alias `claude` instead:

```bash
alias claude='bffs exec --'
```

Same resolution logic, no symlinks installed.

## Status

Per-project pinning works for both `api_key` and `oauth` accounts. The `oauth` flow uses per-account `CLAUDE_CONFIG_DIR` isolation; concurrent sessions don't race. macOS, Linux, and Windows are all supported (the per-dir Keychain hashing is macOS-specific but happens inside Claude Code itself, not bffs). Linux libsecret and Windows DPAPI backends for the api_key store are tracked in [SECURITY.md](SECURITY.md).
