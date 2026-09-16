# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`bffs` is a Go CLI that lets a user keep multiple Claude Code accounts (OAuth subscription logins or `sk-ant-...` API keys) and switch between them globally or per-project. It works by installing a `claude` shim at the front of `PATH`; the shim resolves the active account, sets the right env var, and execs the real `claude` binary.

Module path: `github.com/jratienza65/bffs`. Go 1.26.

## Commands

```bash
make build              # builds the `bffs` binary in repo root
make install            # builds, then sudo-installs to /opt/bffs/bffs
go test ./...           # all tests
go test ./internal/resolver -run TestResolve   # single package / single test
go build -o bffs . && ./bffs <subcommand>      # iterate without installing
```

After installing the binary, run `bffs init` once to drop the `claude` shim into `~/.bffs/bin/` (macOS/Linux) or `%LOCALAPPDATA%\bffs\bin` (Windows) — `--dir` / `$BFFS_SHIM_DIR` override, `internal/shimcheck.DefaultInstallDir` is the source of truth. Optionally run `bffs mcp install` once to register the MCP server with Claude Code.

## Architecture

### Multi-call binary (the `claude` shim trick)

`main.go` inspects `argv[0]`. If invoked as `claude` (or `claude.exe`), it runs `internal/shim.Run` instead of the Cobra root command. `bffs init` installs the shim by symlinking (or hardlinking, or wrapping in a sh script) `<install-dir>/claude` → the bffs binary. This keeps everything in one binary — there is no separate shim executable.

When invoked as `bffs`, it's a normal Cobra CLI in `cmd/`.

### Account resolution (`internal/resolver`)

Strict precedence — first match wins:

1. `BFFS_ACCOUNT` env var
2. Nearest `bffs.toml` walking up from CWD (bounded — see below)
3. Most specific directory rule in `paths.toml` (`bffs path set`, `store.Paths.Match`)
4. Global default in `state.toml`
5. None — `claude` runs with its own untouched credentials

The bffs.toml walk in `internal/projectconfig` deliberately stops at the first `.git` directory or `$HOME` — this prevents a planted `/tmp/bffs.toml` (or any directory above the user's working area) from hijacking which account `claude` uses.

### Two account types, one shape: per-invocation env injection

The shim handles both account types per-invocation via env vars on the child process. No global state is touched at runtime. Per-project pinning works for both.

- **`api_key`**: shim sets `ANTHROPIC_API_KEY=<secret>`. Secret is the bare `sk-ant-...` key.
- **`oauth`**: shim sets `CLAUDE_CONFIG_DIR=<bffs-config>/sessions/<account-name>/`. Claude Code reads its entire config tree (identity at `.claude.json`, credentials at `.credentials.json` or a Keychain entry whose service name is `Claude Code-credentials-<sha256(dir)[:8]>` on macOS) from there. Setting CLAUDE_CONFIG_DIR per-invocation gives full per-account isolation; concurrent oauth sessions on different accounts cannot collide.

For oauth accounts, the credential never lives in `accounts.toml` — Claude Code owns it inside the per-account config dir / hashed Keychain slot. `accounts.toml` only stores display metadata (email, OAuthAccountMeta cache, isolation preset).

### Isolation presets (oauth accounts)

`CLAUDE_CONFIG_DIR` isolates the *whole* Claude Code config tree, not just credentials. To control how much actually gets isolated vs symlinked back to `~/.claude/`, each oauth account picks one of two presets (`internal/store.IsolationPreset`):

- `partial` (default): every entry in `~/.claude/` *except* `.claude.json` and `.credentials.json` is symlinked into the per-account session dir. Drop-in feel — settings, skills, plugins, history, projects, todos all carry across accounts; only auth/identity is per-account.
- `full`: nothing symlinked. Each account is a fresh Claude Code world.

Resolution: per-account override wins, then global default in `state.toml`, then `partial`.

`internal/sessions.SyncSymlinks` is idempotent and lenient: it adds missing symlinks, removes ones we no longer want, and *skips* (returns in a slice rather than erroring) any path where claude wrote a real file in the way. The shim calls it on every oauth invocation so newly-added entries in `~/.claude/` show up automatically; `bffs login` and `bffs reisolate` call it explicitly when the user opts into a new preset.

### MCP server (`internal/mcpserver`)

`bffs mcp serve` runs an MCP server on stdio (official `modelcontextprotocol/go-sdk`), so a Claude Code session can drive bffs itself. Eight tools: `list_accounts`, `resolve_account` (with source provenance), `switch_account`, `pin_account`/`unpin_account` (via `store.Paths`, never `bffs.toml` — projectconfig has no writer), `check_shim` (wraps `shimcheck.Check`), `account_usage` (wraps `usage.Collect`), `run_on_account` (wraps `runner.Run`). Handlers never return secrets and never write to stdout (stdout is the JSON-RPC channel). All results repeat the caveat that changes apply to the *next* claude launch — env injection is per-invocation.

`bffs mcp install` registers the server by patching a `bffs` entry into the `mcpServers` map of `~/.claude.json` **and** every `sessions/<name>/.claude.json` (session dirs are scanned from disk) — each oauth account reads its own copy, never the home one. New oauth accounts inherit the entry because `bffs login` seeds their `.claude.json` from home via `claudejson.SeedFromHome`. The entry bakes in absolute paths: `<bffs> mcp serve --config-dir <cfgDir>`.

### Usage heuristics (`internal/usage`, `internal/usagelog`)

`bffs usage` / the `account_usage` MCP tool estimate per-account headroom. Claude Code transcripts (`projects/<slug>/<sid>.jsonl`) carry token counts but no account identity — and under partial isolation `projects/` is symlinked, so all accounts share one transcript pool. Attribution therefore needs bffs-side data:

- The shim appends one JSON line per launch to `<cfgDir>/launches.jsonl` (`usagelog.Append`, hooked in `shim.Run` between `FindRealClaude` and `execProcess`; best-effort and silent like `syncOAuthSessionDir`; opt-out `BFFS_NO_USAGE_LOG`). The log holds account names + cwds, 0600.
- `usage.Collect` scans in-horizon transcripts (mtime-bounded; `bufio.Reader`, never default `Scanner` — single lines reach multi-MB) and attributes each session, first tier wins: real full-isolation root > `lastSessionId` from the account's own `.claude.json` (`claudejson.LastSessionIDs`) > launch-log match by (cwd, time) with 2min slack and a 10min ambiguity window (two distinct accounts nearby, counting unmanaged launches → refuse to guess) > honest "unattributed" bucket.
- Limit events are `isApiErrorMessage` records with `error:"rate_limit"`; text matchers ("session limit"/"weekly limit", "resets …") live in a data table in `internal/usage/limits.go` since the wording will drift. Weighted burn = input·1 + output·5 + cache_write·1.25 + cache_read·0.1 (price-ratio heuristic). `bffs list`/`show` only read the launch log (fast path); they never parse transcripts.

### Delegated runs (`internal/runner`, `bffs run`, MCP `run_on_account`)

In-session subagents can never switch accounts (credentials are process-level, fixed at launch), so delegation means spawning a fresh headless claude child on the target account. `runner.Run` is the shared core: env built via `shim.AccountEnv` on top of `stripMarkers` (an explicit list of session-instance markers — `CLAUDECODE`, `CLAUDE_PID`, `CLAUDE_EFFORT`, the `CLAUDE_CODE_*` instance family — NOT a prefix strip, which would destroy deliberate config like `CLAUDE_CODE_USE_BEDROCK`), oauth login check *before* `SyncOAuthSessionDir` (sync would EnsureDir and trigger the first-run wizard), spawn-and-wait with `Request.ProcessGroup` opt-in (own pgid + group SIGKILL on cancel for captured runs; must stay false for tty use or Ctrl-C breaks), and a `launches.jsonl` event with source `"run"` for usage attribution. The MCP tool is synchronous with `timeout_seconds` clamped to 570s (under Claude Code's ~10min tool-call ceiling; async start/poll tools are the future path, not a bigger clamp) and deliberately does NOT expose `--permission-mode`/`--dangerously-skip-permissions` — escalation stays human, at most via explicit `allowed_tools`.

### Finding the real `claude`

`internal/shim/FindRealClaude` walks `PATH`, skipping the bffs binary itself (by `EvalSymlinks` comparison against `os.Executable()`), and caches the result at `<configdir>/real-claude.path`. Override with `BFFS_REAL_CLAUDE` for tests.

### Storage (`internal/store`)

Plain TOML at `0600` under the OS user-config dir (`~/Library/Application Support/bffs` on macOS, `~/.config/bffs` on Linux, `%AppData%\bffs` on Windows). Override with `BFFS_HOME`.

- `accounts.toml` — account metadata (api_key secrets; oauth display metadata + isolation preset)
- `state.toml` — global active account, global isolation preset
- `sessions/<name>/` — per-oauth-account Claude Code config dirs (managed by `internal/sessions`)
- `real-claude.path` — cached path to the real `claude` binary
- `launches.jsonl` — append-only shim launch log for usage attribution (`internal/usagelog`)

The store package is intentionally pluggable behind one package boundary — see `SECURITY.md` for the planned migration to OS keystores for the api_key secret (Keychain / libsecret / DPAPI).

## Layout

- `main.go` — dispatch on argv[0]
- `cmd/` — Cobra commands (`add`, `login`, `switch`, `path`, `reisolate`, `show`, `list`, `rename`, `remove`, `init`, `exec`, `run`, `mcp`, `usage`)
- `internal/shim/` — shim-mode entry, `FindRealClaude`
- `internal/shimcheck/` — probes whether the shim wins on PATH per shell mode; `DefaultInstallDir`
- `internal/resolver/` — account precedence
- `internal/projectconfig/` — bounded `bffs.toml` walk (read-only)
- `internal/store/` — TOML accounts/state, paths + directory rules (`paths.toml`), isolation preset enum
- `internal/sessions/` — per-account session-dir management (Dir, EnsureDir, SyncSymlinks)
- `internal/claudejson/` — read/write helpers for `.claude.json` (identity snapshot + `mcpServers` entry)
- `internal/mcpserver/` — MCP server (tools, stdio serving, install/uninstall targets)
- `internal/usagelog/` — append-only launch log (`launches.jsonl`), written by the shim
- `internal/usage/` — usage analyzer: transcript scanning, session→account attribution, limit events, headroom suggestion
- `internal/runner/` — spawn-and-wait claude child on a named account (delegated runs)

## Platform notes

The whole flow works on macOS, Linux, and Windows. The per-account Keychain hash on macOS happens inside Claude Code itself (the `FN(...)` function in the binary takes `sha256(CLAUDE_CONFIG_DIR)[:8]` as a service-name suffix); bffs only sets the env var.
