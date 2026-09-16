# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`bffs` is a Go CLI that lets a user keep multiple Claude Code accounts (OAuth subscription logins or `sk-ant-...` API keys) and switch between them globally or per-project. It works by installing a `claude` shim at the front of `PATH`; the shim resolves the active account, sets the right env var, and execs the real `claude` binary.

Module path: `github.com/jratienza65/bffs`. Go 1.26.

This file is the invariants and the map. The decisions behind them are one page each under [`adr/`](adr/), the walkthroughs are in the README, the threat model in SECURITY.md, and the detail of a package is its own doc comment (`go doc ./internal/<pkg>`). Skills worth loading first: `tui-design` and `bubbletea-v2` before touching the browser, `cli-design` before changing a command's output, `writing-for-agents` before editing this file.

## Commands

The version `bffs --version` reports is never a literal in the source: a release stamps `cmd.Version` at link time (goreleaser, and `make build` from `git describe`), and an unstamped build resolves it in `cmd.versionString` from the build info — the module version for `go install <module>@<version>`, else the commit as `dev+<sha12>[.dirty]`, else `dev`. A hardcoded default is how a v0.3.0 download reported 0.1.0. Only a release-shaped version reaches the skill frontmatter (`cmd.releaseVersion`).

```bash
make help               # every target, one line each
make build              # builds the `bffs` binary in repo root
make install            # builds, then sudo-installs to /opt/bffs/bffs
make check              # what CI gates on: lint (gofmt, vet ×2 tags, golangci-lint) + race tests
make tools              # installs the pinned golangci-lint through mise
go test ./...           # all tests
go test ./internal/resolver -run TestResolve   # single package / single test
go build -o bffs . && ./bffs <subcommand>      # iterate without installing
make golden             # rewrite the TUI frame goldens after a deliberate layout change
make bench-shim         # the init-cost guard (hyperfine; see ADR 0001)
```

Lint is `gofmt` + `go vet` (both build tags) + golangci-lint 2.13.2, pinned in `mise.toml` and in the CI `lint` job so a developer and CI see the same findings. `.golangci.yml` carries the reasoning for every exclusion; the short version is that executing another program (G204) and reading a path bffs itself derived (G304) are what this tool does, while `internal/bundle`, `internal/porter` and `internal/transfer` keep G304 live because their paths can come from another machine — each open there is answered at the site. `make lint` degrades to gofmt and vet when golangci-lint is absent, because the pre-commit hook runs it and must never be stricter than a fresh clone.

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

`bffs mcp serve` runs an MCP server on stdio (official `modelcontextprotocol/go-sdk`), so a Claude Code session can drive bffs itself. Fourteen tools: `list_accounts`, `resolve_account` (with source provenance), `switch_account`, `pin_account`/`unpin_account` (via `store.Paths`, never `bffs.toml` — projectconfig has no writer), `check_shim` (wraps `shimcheck.Check`), `account_usage` (wraps `usage.Collect`), `run_on_account` (wraps `runner.Run`), the read-only catalog tools `list_sessions` (the `bffs sessions list --json` shape), `list_memories`, `trust_status` (never writes; returns the `bffs trust sync` command to run instead — trust writes stay a human decision on the CLI), and three write tools that stay local-file-only: `export_bundle` (Output must be absolute, end in `.bffs`, sit outside `~/.claude`, every config dir, `<bffsHome>` and the temp dirs; created `O_EXCL` 0600 via temp+rename; bundles over 1 GiB are refused with a CLI hint), `import_bundle` (memory always skipped, `--on-conflict overwrite` and trust carry never exposed, `rehome` prefix mappings whose targets must exist, `as_is`, `set_last_session`, `dry_run`) and `rehome` (memory never merged, `rewrite_memory` only). All three run under the 570 s ceiling; nothing over MCP serves or receives on the network (that needs a human at both ends). Handlers never return secrets and never write to stdout (stdout is the JSON-RPC channel). All results repeat the caveat that changes apply to the *next* claude launch — env injection is per-invocation.

`bffs mcp install` registers the server by patching a `bffs` entry into the `mcpServers` map of `~/.claude.json` **and** every `sessions/<name>/.claude.json` (session dirs are scanned from disk) — each oauth account reads its own copy, never the home one. New oauth accounts inherit the entry because `bffs login` seeds their `.claude.json` from home via `claudejson.SeedFromHome`. The entry bakes in absolute paths: `<bffs> mcp serve --config-dir <cfgDir>`.

### Usage heuristics (`internal/usage`, `internal/usagelog`)

`bffs usage` / the `account_usage` MCP tool estimate per-account headroom. Claude Code transcripts (`projects/<slug>/<sid>.jsonl`) carry token counts but no account identity — and under partial isolation `projects/` is symlinked, so all accounts share one transcript pool. Attribution therefore needs bffs-side data:

- The shim appends one JSON line per launch to `<cfgDir>/launches.jsonl` (`usagelog.Append`, hooked in `shim.Run` between `FindRealClaude` and `execProcess`; best-effort and silent like `syncOAuthSessionDir`; opt-out `BFFS_NO_USAGE_LOG`). The log holds account names + cwds, 0600.
- `usage.Collect` scans in-horizon transcripts (mtime-bounded; `bufio.Reader`, never default `Scanner` — single lines reach multi-MB) and attributes each session, first tier wins: real full-isolation root > `lastSessionId` from the account's own `.claude.json` (`claudejson.LastSessionIDs`) > launch-log match by (cwd, time) with 2min slack and a 10min ambiguity window (two distinct accounts nearby, counting unmanaged launches → refuse to guess) > honest "unattributed" bucket.
- Limit events are `isApiErrorMessage` records with `error:"rate_limit"`; text matchers ("session limit"/"weekly limit", "resets …") live in a data table in `internal/usage/limits.go` since the wording will drift. Weighted burn = input·1 + output·5 + cache_write·1.25 + cache_read·0.1 (price-ratio heuristic). `bffs list`/`show` only read the launch log (fast path); they never parse transcripts.

### Delegated runs (`internal/runner`, `bffs run`, MCP `run_on_account`)

In-session subagents can never switch accounts (credentials are process-level, fixed at launch), so delegation means spawning a fresh headless claude child on the target account. `runner.Run` is the shared core: env built via `shim.AccountEnv` on top of `stripMarkers` (an explicit list of session-instance markers — `CLAUDECODE`, `CLAUDE_PID`, `CLAUDE_EFFORT`, the `CLAUDE_CODE_*` instance family — NOT a prefix strip, which would destroy deliberate config like `CLAUDE_CODE_USE_BEDROCK`), oauth login check *before* `SyncOAuthSessionDir` (sync would EnsureDir and trigger the first-run wizard), spawn-and-wait with `Request.ProcessGroup` opt-in (own pgid + group SIGKILL on cancel for captured runs; must stay false for tty use or Ctrl-C breaks), and a `launches.jsonl` event with source `"run"` for usage attribution. The MCP tool is synchronous with `timeout_seconds` clamped to 570s (under Claude Code's ~10min tool-call ceiling; async start/poll tools are the future path, not a bigger clamp) and deliberately does NOT expose `--permission-mode`/`--dangerously-skip-permissions` — escalation stays human, at most via explicit `allowed_tools`.

### Session/memory transfer

Sessions and auto-memory move between accounts and between machines. The decisions are recorded — the bundle format (ADR 0006), the move (ADR 0007), trust sync (ADR 0005) — the walkthroughs are in the README, the threat model in SECURITY.md, and the detail in each package's doc comment. What must stay true:

**Trust sync (`bffs trust`, `bffs trust sync`)** copies only the two dialog answers, never `lastSessionId`, and never downgrades one; permissions move only under `--include-permissions`, with every MCP command line printed first. `Apply` takes Claude's mkdir lock. `switch`/`show` hint when the active account lacks an answer another has (`trust_hint = false` silences it); `bffs login` seeds a new `.claude.json` from home only when the file does not exist yet. api_key accounts map to `home`.

**Export/import (`bffs export --out <file>|-|auto`, `bffs import --from <file>|-`)**: manifest strings (cwd, project_key, home, hostname…) are compared and printed sanitised, never joined into a path; slugs are joined only after `bundle.ClassifyName`; every suffix bffs creates derives from `bundle_id[:8]`; set-asides (`*.bffs-replaced-<epochms>`) are never deleted; the transcript lands last and stamped; an imported transcript gets `mtime = max(original, now − cleanupPeriodDays/2)` (0 = never swept) and the report prints the sweep date; memory topic files keep their source mtime; imported memory is prompt content (a disclosure line, `pinned-imported:` unless `--trust-memory`, and the MCP tools never merge it).

**LAN transfer (`bffs export --serve`, `bffs import --from <host>`)**: `internal/transfer` is stdlib + `x/crypto/argon2` and knows nothing about bundles. TLS 1.3 only with a pinned ALPN, the key derived per connection with the TLS exporter as salt, exporter-bound HMAC proofs (B first), one listener per on-link interface address with the peer re-checked on accept (Tailscale/CGNAT always denied; `--allow-routed` widens only the on-link test). The pairing code is rendered exactly once, by A's banner; B never prints it, there is no `--code` flag, and `BFFS_TRANSFER_CODE` is read once, unset, and stripped from every child env. The receiver pins the sha256 of the manifest it reviewed.

**Import placement (`--map`/`--into`, the interactive prompt)**: prefix rules and `--into` decide placements up front; in a terminal without `-y`/`--as-is`/`--dry-run`, every project no rule covers and that does not exist here is asked about, and an answer becomes an exact rule — a confirmed placement, which is what lets memory merge and `--carry-trust`/`--set-last-session` apply. Anything unconfirmed lands as-is and is recorded as pending.

**Rehome (`bffs rehome`)**: a failing liveness scan is an error — bffs never moves a transcript it cannot prove is closed. Mappings are prefix rules applied longest-first at a separator boundary; a mapping is a confirmation, so memory defaults to `merge`. `CheckLastLine` is the one stamp gate (a torn last line is refused unless `--force-stamp`; an import demotes such a session to as-is). `--rewrite-cwd`/`--rewrite-file-history` are off by default, run after the move commits, and replace only the top-level `cwd` — never the ones inside message content. `transcripts.ProjectDirFor` scans every slug dir when the entry does not exist yet: memoize it per target directory, never call it per session.

**Same-machine copy (`bffs copy --from --to [--move]`)** streams a manifest through an `io.Pipe` into `porter.Import` with the source pool excluded from the collision scan and every existing project confirmed as its own identity placement. Two accounts on one pool is `porter.ErrSameRoot`, answered with the trust-sync hint and exit 0. `--move` re-reads and checksums every landed file before removing any source file, holds back live sessions, and requires a typed count for more than one. `bffs sessions rm` is the only other remover (ADR 0008). Every printed resume line is `cd <dir> && BFFS_ACCOUNT=<a> claude --resume <sid>` — the assignment sits on the `claude` word.

**Catalog (`bffs sessions`, `bffs memory`)** is the read model (ADR 0009). `usage.NewAttributor` answers root owner > `lastSessionId` > import record > launch-log. `transcripts.MemoryDirFor` resolves Claude's overrides in Claude's order (`CLAUDE_COWORK_MEMORY_PATH_OVERRIDE`, then `autoMemoryDirectory` from `settings.local.json` over `settings.json`, then `<root.Dir>/<MemorySlug>/memory`); only `CLAUDE_CODE_REMOTE_MEMORY_DIR` is an error, because Claude keys that layout by a slug function bffs has not verified. Under an override every project maps to one directory, so a caller placing memory for several projects gets the same dir. `transcripts.LiveAccounts` reads the owning account from the process environment (darwin `kern.procargs2`, linux `/proc/<pid>/environ` bounded at 4 MiB): only the `CLAUDE_CONFIG_DIR` entry is kept, the block is cleared, no error carries environment bytes, and api_key accounts are never matched — that would mean comparing secrets.

### The browser (`internal/tui`)

Bare `bffs` on a terminal opens the lazygit-style browser: three stacked panels (accounts / projects / sessions|memory), a preview, overlays on a stack in the main pane, six themes, mouse support and a `?` overlay. `internal/tui/doc.go` is where its shape is written down and the README shows it in use. What must stay true:

- It opens only when `tuiSupported()` (stdin and stdout are terminals, `TERM != dumb`); otherwise — and in the `bffs_notui` build — bare `bffs` prints help and `bffs sessions`/`bffs memory` print their tables. `list`/`show`/`scan-paths`, the shim and `mcp serve` never reach it. SIGINT/SIGTERM exits 130.
- Every action calls the same `internal/*` engines as the CLI, never `bffs` itself; `d` never deletes (ADR 0008); the listing never opens a transcript and every rendered string is sanitised (ADR 0009); ownership is per root, never per account (ADR 0004).
- The shim budget governs what may be linked: `make bench-shim` must stay within +10 % p50 or +5 ms, `go-runewidth` is pinned at ≥ v0.0.29 (v0.0.27 costs ~16 ms of `init`), Glamour is not linked (+6.8 ms — the browser renders Markdown itself), and `internal/shim/graph_test.go` pins the shim's dependency set (ADR 0001).
- Rendering has its own harness: 23 golden frames under `internal/tui/testdata` (`make golden` rewrites them — the diff is the review), a sweep of every state at twelve sizes, a rung table of what each token becomes at 256 and 16 colours, a scan for holes in a painted bar, an ASCII sweep for `BFFS_ASCII`, benchmarks for the wheel path, and `tools/drive.py` for a real pty. `BFFS_DEBUG=<file>` logs one line per event.
- Test seams mirror `cmd`'s: `serveListen`, `serveLocal`, `serveGenerate`, `serveBindPort`, `fetchLocal`, `fetchDial`, `fetchLookup`, `lanOptions`, `tickEvery`, `osHostname`, `expireNote`.

### Finding the real `claude`

`internal/shim/FindRealClaude` walks `PATH`, skipping the bffs binary itself (by `EvalSymlinks` comparison against `os.Executable()`), and caches the result at `<configdir>/real-claude.path`. Override with `BFFS_REAL_CLAUDE` for tests.

### Storage (`internal/store`)

Plain TOML at `0600` under the OS user-config dir (`~/Library/Application Support/bffs` on macOS, `~/.config/bffs` on Linux, `%AppData%\bffs` on Windows). Override with `BFFS_HOME`.

- `accounts.toml` — account metadata (api_key secrets; oauth display metadata + isolation preset)
- `state.toml` — global active account, global isolation preset, `trust_hint` (tri-state; explicit `false` silences the trust hint in `switch`/`show`), `trust_sync` (reserved, unread), `theme` (the browser's colour theme; written by the TUI's `T` key)
- `sessions/<name>/` — per-oauth-account Claude Code config dirs (managed by `internal/sessions`)
- `real-claude.path` — cached path to the real `claude` binary
- `launches.jsonl` — append-only shim launch log for usage attribution (`internal/usagelog`)
- `imports/<bundleID>.json` — import records (0600) written by `porter.Import` after the commits (per-session status placed|pending|skipped), consulted for the bundle_id refusal (`--force` overrides), `bffs sessions list --pending-rehome`, `bffs sessions imports`, and usage attribution tier 3 (`internal/imports`)
- `staging/<bundleID>/` — import staging (0700, must not pre-exist; the unpacked bundle plus `journal.json` while a commit is in flight); removed after success, kept and printed after a failure past unpack; only `bffs import --clean-staging` removes leftovers, after listing them

The store package is intentionally pluggable behind one package boundary — see `SECURITY.md` for the planned migration to OS keystores for the api_key secret (Keychain / libsecret / DPAPI).

## Layout

Every package documents itself; `go doc ./internal/<pkg>` is the detail, and the ADRs under `adr/` carry the decisions. This is the map.

| Path | What lives there |
| --- | --- |
| `main.go` | dispatch on `argv[0]`: `claude` runs the shim, anything else the CLI |
| `cmd/` | the Cobra commands, one file each; bare `bffs` opens the browser |
| `internal/shim/` | shim-mode entry, `FindRealClaude`, and `graph_test.go` pinning its dependency set |
| `internal/shimcheck/` | whether a bffs shim wins on PATH per shell mode; `DefaultInstallDir` |
| `internal/resolver/` | the account precedence (ADR 0002) |
| `internal/projectconfig/` | the bounded, read-only `bffs.toml` walk |
| `internal/store/` | `accounts.toml`, `state.toml`, `paths.toml`, the isolation enum |
| `internal/sessions/` | per-account Claude config dirs (`Dir`, `EnsureDir`, `SyncSymlinks`) |
| `internal/claudejson/` | read/write helpers for `.claude.json` |
| `internal/mcpserver/` | the MCP server: tools, stdio serving, install targets |
| `internal/usagelog/` | the append-only launch log the shim writes |
| `internal/usage/` | transcript scanning, session→account attribution, limit events |
| `internal/runner/` | spawning a headless `claude` on a named account |
| `internal/fsutil/` | atomic write, copy, move, Claude's mkdir lock, `FreeSpace` |
| `internal/imports/` | import records under `<config>/imports/` |
| `internal/trust/` | the trust-answer engine: `Report`, `BestSource`, `Plan`/`Apply` (ADR 0005) |
| `internal/rehome/` | the write side of transfer: transactional commit, recovery, memory merge, the move (ADR 0007) |
| `internal/porter/` | what `export`, `import`, the LAN transfer, `copy` and the MCP tools share |
| `internal/bundle/` | the `.bffs` format: envelope, name grammar, manifest, `Unpack` (ADR 0006) |
| `internal/transfer/` | the LAN pairing and transport (stdlib + argon2; bundle-agnostic) |
| `internal/transcripts/` | the read model over Claude's tree: slugs, roots, liveness, memory dirs, `Sanitize` |
| `internal/textdiff/` | a unified diff in the standard library, for memory drift |
| `internal/skillpack/` | the embedded `bffs-rehome` skill and where it installs |
| `internal/tui/` | the browser (see its package doc) |
| `adr/` | the decisions: what forced them, what they rule out |
| `tools/drive.py` | drives the real binary under a pty (`pip install pyte`; Unix only) |

Two of these are deliberately leaves: `internal/bundle` (standard library only, so its fuzz targets are cheap) and `internal/transfer` (standard library plus `x/crypto/argon2`, and it knows nothing about bundles). `internal/bundle`'s copy of the reserved-name list is pinned equal to `transcripts`' by a test — update both together.

## Platform notes

The whole flow works on macOS, Linux, and Windows. The per-account Keychain hash on macOS happens inside Claude Code itself (the `FN(...)` function in the binary takes `sha256(CLAUDE_CONFIG_DIR)[:8]` as a service-name suffix); bffs only sets the env var.
