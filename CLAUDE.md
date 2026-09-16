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

`bffs mcp serve` runs an MCP server on stdio (official `modelcontextprotocol/go-sdk`), so a Claude Code session can drive bffs itself. Eleven tools: `list_accounts`, `resolve_account` (with source provenance), `switch_account`, `pin_account`/`unpin_account` (via `store.Paths`, never `bffs.toml` — projectconfig has no writer), `check_shim` (wraps `shimcheck.Check`), `account_usage` (wraps `usage.Collect`), `run_on_account` (wraps `runner.Run`), and the read-only catalog tools `list_sessions` (the `bffs sessions list --json` shape), `list_memories`, `trust_status` (never writes; returns the `bffs trust sync` command to run instead — trust writes stay a human decision on the CLI). Handlers never return secrets and never write to stdout (stdout is the JSON-RPC channel). All results repeat the caveat that changes apply to the *next* claude launch — env injection is per-invocation.

`bffs mcp install` registers the server by patching a `bffs` entry into the `mcpServers` map of `~/.claude.json` **and** every `sessions/<name>/.claude.json` (session dirs are scanned from disk) — each oauth account reads its own copy, never the home one. New oauth accounts inherit the entry because `bffs login` seeds their `.claude.json` from home via `claudejson.SeedFromHome`. The entry bakes in absolute paths: `<bffs> mcp serve --config-dir <cfgDir>`.

### Usage heuristics (`internal/usage`, `internal/usagelog`)

`bffs usage` / the `account_usage` MCP tool estimate per-account headroom. Claude Code transcripts (`projects/<slug>/<sid>.jsonl`) carry token counts but no account identity — and under partial isolation `projects/` is symlinked, so all accounts share one transcript pool. Attribution therefore needs bffs-side data:

- The shim appends one JSON line per launch to `<cfgDir>/launches.jsonl` (`usagelog.Append`, hooked in `shim.Run` between `FindRealClaude` and `execProcess`; best-effort and silent like `syncOAuthSessionDir`; opt-out `BFFS_NO_USAGE_LOG`). The log holds account names + cwds, 0600.
- `usage.Collect` scans in-horizon transcripts (mtime-bounded; `bufio.Reader`, never default `Scanner` — single lines reach multi-MB) and attributes each session, first tier wins: real full-isolation root > `lastSessionId` from the account's own `.claude.json` (`claudejson.LastSessionIDs`) > launch-log match by (cwd, time) with 2min slack and a 10min ambiguity window (two distinct accounts nearby, counting unmanaged launches → refuse to guess) > honest "unattributed" bucket.
- Limit events are `isApiErrorMessage` records with `error:"rate_limit"`; text matchers ("session limit"/"weekly limit", "resets …") live in a data table in `internal/usage/limits.go` since the wording will drift. Weighted burn = input·1 + output·5 + cache_write·1.25 + cache_read·0.1 (price-ratio heuristic). `bffs list`/`show` only read the launch log (fast path); they never parse transcripts.

### Delegated runs (`internal/runner`, `bffs run`, MCP `run_on_account`)

In-session subagents can never switch accounts (credentials are process-level, fixed at launch), so delegation means spawning a fresh headless claude child on the target account. `runner.Run` is the shared core: env built via `shim.AccountEnv` on top of `stripMarkers` (an explicit list of session-instance markers — `CLAUDECODE`, `CLAUDE_PID`, `CLAUDE_EFFORT`, the `CLAUDE_CODE_*` instance family — NOT a prefix strip, which would destroy deliberate config like `CLAUDE_CODE_USE_BEDROCK`), oauth login check *before* `SyncOAuthSessionDir` (sync would EnsureDir and trigger the first-run wizard), spawn-and-wait with `Request.ProcessGroup` opt-in (own pgid + group SIGKILL on cancel for captured runs; must stay false for tty use or Ctrl-C breaks), and a `launches.jsonl` event with source `"run"` for usage attribution. The MCP tool is synchronous with `timeout_seconds` clamped to 570s (under Claude Code's ~10min tool-call ceiling; async start/poll tools are the future path, not a bigger clamp) and deliberately does NOT expose `--permission-mode`/`--dangerously-skip-permissions` — escalation stays human, at most via explicit `allowed_tools`.

### Session/memory transfer (in progress on `feat/session-memory-transfer`)

The plan and its task tracker live in `docs/plans/` (deliberately untracked via `.git/info/exclude`; never commit `docs/`).

**Trust sync (`bffs trust`, `bffs trust sync`)** fixes the dialogs that re-appear after `bffs switch`: Claude records the folder-trust and external-CLAUDE.md-imports answers per project inside `.claude.json`, which bffs keeps per account by design. `bffs trust` prints the per-account matrix for the cwd's project key; `bffs trust sync --to <acct|home|all>` copies the three booleans (never `lastSessionId`, metrics, or permission grants unless `--include-permissions`). A running claude re-reads `.claude.json` within about a second, so there is no liveness refusal — only Claude's lock. `bffs switch`/`bffs show` print a hint when the active account lacks an answer another account has (`trust_hint = false` silences it); `bffs login` seeds `.claude.json` from home only when the file does not exist yet and carries trust answers over (`--no-trust-carry`). api_key accounts map to `home`. Verified in the 2.1.259 binary: auto-memory files are injected without an include parent, so `@`-references inside memory files never trigger the external-imports dialog.

**Export/import to file (`bffs export --out <file>|-|auto`, `bffs import --from <file>|-`)** moves sessions and auto-memory as a `.bffs` bundle. Invariants: manifest strings (cwd, project_key, home, hostname…) are compared and printed (sanitised) only, never joined into paths; slugs are joined only after `bundle.ClassifyName`; every on-disk suffix bffs creates derives from `bundle_id[:8]`; set-asides (`*.bffs-replaced-<epochms>`) are never deleted; the transcript lands last and stamped; imported transcripts get `mtime = max(original, now − cleanupPeriodDays/2)` (0 = never swept = no clamp) and the report prints the sweep date; sidecar/file-history/plans/tasks files get import time unless `--preserve-mtimes`; memory topic files keep their source mtime; imported memory is prompt content (disclosure line, `pinned-imported:` unless `--trust-memory`, MCP never merges); `--serve`/`--from <host>` (LAN), `--map`/`--into` (rehome), `--carry-trust`, `--set-last-session` are parsed but refused until their milestones land. `bffs skill install` drops the `bffs-rehome` skill (read/plan tools pre-approved only; every write goes through Claude Code's permission prompt) and runs `claude plugin validate` over the config dir when a real claude is found (20 s timeout, never fails the install).

**Catalog (`bffs sessions`, `bffs memory`)** is the read model over Claude's tree: `transcripts.List` never opens a transcript on the fast path (`ReadDir` + `Info`); titles come from Claude's own 64 KiB head/tail windows with field-keyed last-wins semantics and the picker precedence custom > ai > last-prompt > summary > first-prompt > history. `usage.NewAttributor` answers root owner > `lastSessionId` > import record > launch-log (cwd, time); `usage.Collect` and `bffs list` are unchanged. `bffs show` now also reads `.claude.json` files for the trust hint; nothing outside `bffs usage`/`sessions`/`memory` parses transcripts.

Foundations landed first: the account name `home` is reserved (it names `~/.claude.json` in trust sync and copy), `store.State` carries `trust_hint`/`trust_sync`, and `cmd/exit.go` maps `exitError{code}` to process exit codes (2 = retryable, 130 = interrupted).

### Finding the real `claude`

`internal/shim/FindRealClaude` walks `PATH`, skipping the bffs binary itself (by `EvalSymlinks` comparison against `os.Executable()`), and caches the result at `<configdir>/real-claude.path`. Override with `BFFS_REAL_CLAUDE` for tests.

### Storage (`internal/store`)

Plain TOML at `0600` under the OS user-config dir (`~/Library/Application Support/bffs` on macOS, `~/.config/bffs` on Linux, `%AppData%\bffs` on Windows). Override with `BFFS_HOME`.

- `accounts.toml` — account metadata (api_key secrets; oauth display metadata + isolation preset)
- `state.toml` — global active account, global isolation preset, `trust_hint` (tri-state; explicit `false` silences the trust hint in `switch`/`show`), `trust_sync` (reserved, unread)
- `sessions/<name>/` — per-oauth-account Claude Code config dirs (managed by `internal/sessions`)
- `real-claude.path` — cached path to the real `claude` binary
- `launches.jsonl` — append-only shim launch log for usage attribution (`internal/usagelog`)
- `imports/<bundleID>.json` — import records (0600) written by `porter.Import` after the commits (per-session status placed|pending|skipped), consulted for the bundle_id refusal (`--force` overrides), `bffs sessions list --pending-rehome`, `bffs sessions imports`, and usage attribution tier 3 (`internal/imports`)
- `staging/<bundleID>/` — import staging (0700, must not pre-exist; the unpacked bundle plus `journal.json` while a commit is in flight); removed after success, kept and printed after a failure past unpack; only `bffs import --clean-staging` removes leftovers, after listing them

The store package is intentionally pluggable behind one package boundary — see `SECURITY.md` for the planned migration to OS keystores for the api_key secret (Keychain / libsecret / DPAPI).

## Layout

- `main.go` — dispatch on argv[0]
- `cmd/` — Cobra commands (`add`, `login`, `switch`, `path`, `reisolate`, `show`, `list`, `rename`, `remove`, `init`, `exec`, `run`, `mcp`, `usage`, `trust`, `sessions`, `memory`, `export`, `import`, `skill`)
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
- `internal/runner/` — spawn-and-wait claude child on a named account (delegated runs); `Command` builds the `*exec.Cmd` (nil stdio stays nil so a TUI can hand over the terminal), `Run` = `Command` + wait
- `internal/fsutil/` — atomic write, copy, rename-or-copy move, mkdir lock (Claude's proper-lockfile scheme), touch (leaf)
- `internal/imports/` — import records under `<config>/imports/` (leaf)
- `internal/trust/` — trust-flag engine between accounts' `.claude.json` files: `Files` (oauth accounts + `home`), `Report`/`EffectiveFolderTrust` (exact key, else the ancestor walk bounded by the git root, matching Claude's own check; external-imports is exact-key only), `BestSource` (exact Accepted first: active, accounts by name, home; then Inherited), `Plan`/`Apply` (never downgrade, never override an explicit decline, `--mirror` verbatim; `Apply` takes Claude's mkdir lock `<file>.lock` for well under a second)
- `internal/rehome/` — the write side of transfer: `CommitSession` lands one staged session through an `os.Root` over the config dir in the plan's order (sidecar as `<sid>.bffs-tmp/` then rename → file-history/plans/tasks under the collision rule (same sha skipped, else `<name>.imported-<id8>`) → transcript as `<sid>.jsonl.bffs-tmp` + optional stamp, fsync, rename last → mtime floor → history lines), journalled with reverse rollback; `Recover` is restore-never-delete; `MergeMemory` (skip|overwrite; unconfirmed placements write `*.imported-<id8>.md` side files and never touch `MEMORY.md`; `pinned:` → `pinned-imported:` unless trusted; also through `os.Root`); `AppendHistory` (schema-checked, capped, deduped, advisory lock); `VerifyCommand`. The M6 half (moves, `relocated` stamp, merge, path rewrites, suggestions) is pending
- `internal/porter/` — orchestration shared by `bffs export`/`bffs import` (later LAN, copy, MCP): `Select` → `BuildManifest` (pre-pass sha256, every name validated with the bundle grammar and skipped with a warning when out of grammar, symlinks never followed, live transcripts hashed and streamed from a kept handle, history synthesised per sid, `git remote` best-effort with pinned stdio) → `Write`/`Export`; `Import` runs the plan's pipeline: recover → peek manifest → bundle_id refusal → destination by identity (a `transcripts.Roots` entry, orphans refused) → placement (identity = `filepath.IsAbs` + `store.NormalizePath` string equality of an existing local dir, else as-is/pending) → collisions scoped to the destination root → liveness → staging via `bundle.Unpack` → staged head `sessionId` == file name → memories first, then sessions, then the record → report with the verify block
- `internal/skillpack/` — the embedded `bffs-rehome` skill (`assets/bffs-rehome/{SKILL.md, references/rehome-checklist.md}`, `version: 0.0.0` stamped from `cmd.Version` at install); `Targets` = `<home>/skills/bffs-rehome` plus a real copy for every accounts.toml oauth account whose `store.ResolveIsolation` is `full` (partial accounts see the home copy through their `skills/` symlink; orphans never); `Install` is marker-guarded (`<!-- managed by bffs -->`, `--force` overwrites a user skill) and refuses up front; `Uninstall` removes only managed dirs
- `internal/bundle/` — the `.bffs` bundle format: envelope (`BFFS\x01` + compression byte), PAX tar namespace grammar (`ValidateEntryName`/`ClassifyName`), `Manifest.Validate`, `Build` (writer), `PeekManifest`, `Unpack` (reader into an `os.OpenRoot` staging dir with a manifest allow-list). Stdlib-only leaf; `reservedSlugs` is a private copy of `transcripts.ReservedProjectEntries` pinned equal by `TestReservedSlugsEqual` — update both together. The format description lives in `internal/bundle/doc.go`. `Unpack` never writes outside the staging dir, never deletes, drains reserved kinds into `Unpacked.Notes`, bounds the tar stream to what the manifest implies, and applies mtimes only inside [2020-01-01, now+24h]. Fuzz targets run via `make fuzz` and the CI `Fuzz smoke` step with `-fuzzminimizetime 0`
- `internal/transcripts/` — catalog of Claude Code's on-disk sessions and memories: slug rule, project key (git root), roots (shared pool vs per-account), reserved names, `cleanupPeriodDays`, liveness from `sessions/<pid>.json`, `Sanitize` for rendered strings. Not to be confused with `internal/sessions` (per-account config dirs)

## Platform notes

The whole flow works on macOS, Linux, and Windows. The per-account Keychain hash on macOS happens inside Claude Code itself (the `FN(...)` function in the binary takes `sha256(CLAUDE_CONFIG_DIR)[:8]` as a service-name suffix); bffs only sets the env var.
