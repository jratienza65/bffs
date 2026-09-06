# BFFs

A small Go CLI for using the [Claude Code](https://claude.com/claude-code) `claude` command with multiple distinct accounts — globally or scoped per project.

## What it does

- Stores multiple Claude accounts (OAuth subscription logins *or* `sk-ant-...` API keys) under named entries.
- Picks one to be the **global default**.
- A project can pin its own account by dropping a `bffs.toml` in its root — every `claude` invocation made anywhere inside that tree uses that account.
- Installs a tiny `claude` shim at the front of `PATH`. The shim resolves the right account, sets the matching env var (`ANTHROPIC_API_KEY` for api_key, `CLAUDE_CONFIG_DIR` for oauth), and execs the real `claude`.

For oauth accounts, isolation works via Claude Code's own `CLAUDE_CONFIG_DIR` mechanism: each account gets its own session dir under `<bffs-config>/sessions/<name>/`, with its own `.claude.json`, its own `.credentials.json` (or hashed-suffix Keychain entry on macOS), and — depending on the chosen isolation preset — its own or shared settings/skills/plugins. Concurrent `claude` sessions on different oauth accounts cannot collide.

## Intended use

bffs is for switching between accounts **you personally own and are entitled
to use** — work vs. personal, one account per client so the right party gets
billed, an API key alongside a subscription. It is not for sharing accounts
between people (Anthropic's [Consumer Terms](https://www.anthropic.com/legal/consumer-terms)
prohibit making your account available to anyone else) or for stacking
accounts to route around subscription limits. Each account remains subject to
its own plan limits, and staying within Anthropic's terms is the account
holder's responsibility.

Note that bffs never touches OAuth credentials: the real `claude` binary owns
its own credential store per session dir and makes every API call itself —
bffs only sets `CLAUDE_CONFIG_DIR` (or `ANTHROPIC_API_KEY`) on the child
process.

## Install

```bash
go install github.com/jratienza65/bffs@latest
bffs init        # installs the shim, prints the PATH snippet
```

Or grab a prebuilt binary from [Releases](https://github.com/jratienza65/bffs/releases) (macOS, Linux, Windows × amd64/arm64):

```bash
tar -xzf bffs_<version>_<os>_<arch>.tar.gz
sudo install -m 0755 bffs /usr/local/bin/bffs
bffs init
```

Each release ships a `checksums.txt`; verify with `sha256sum -c checksums.txt --ignore-missing`.

`bffs init` verifies the shim would actually be the `claude` that runs before
installing it. Which directories are on `PATH` depends on how a shell was
started, so a shim can win in your terminal and lose everywhere else — IDE
integrations, launchd/systemd, cron and `ssh host 'cmd'` read different startup
files. init probes each invocation mode and refuses to install a shim that
would silently never run:

```
Would a claude shim in ~/.bffs/bin actually run?

  interactive      yes
  login            NO — /opt/homebrew/bin/claude comes first on PATH
  non-interactive  NO — no claude resolves at all in this mode
  current shell    yes
```

On a terminal it prompts for the install directory, offering any directory
already early enough on `PATH` in every mode (choosing one means no PATH edit
at all). `bffs init --auto` takes the per-OS default without prompting and
still verifies; with no terminal — CI, dotfile bootstrap — it behaves as
`--auto` rather than hanging. `--force` installs anyway.

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

## Directory rules — per-project pinning without a file in the project

If you would rather not have a `bffs.toml` sitting in every repo, keep the same
mapping in your bffs config instead:

```bash
bffs path set personal ~/code/oss   # covers the directory and everything under it
bffs path list                      # '*' marks the rule covering your cwd
bffs path remove ~/code/oss
bffs path import ~/code/oss         # turn an existing bffs.toml into a rule
```

Identical effect, but nothing lands in the working tree. The most specific
(longest) matching rule wins, matching is on path segments (a rule on `/a/b`
never captures `/a/bc`), and `bffs.toml` still wins where both exist.

## MCP server — let Claude Code drive bffs

A Claude Code session can inspect and stage account changes itself, over the
[Model Context Protocol](https://modelcontextprotocol.io):

```bash
bffs mcp install     # registers the server in Claude Code (user scope, all projects)
bffs mcp uninstall   # removes the registration
```

After a restart (or `/mcp`), the session gets eight tools, surfaced as
`mcp__bffs__<tool>`:

| Tool | What it does |
|------|--------------|
| `list_accounts` | Names, types, emails, isolation, current default — never secrets |
| `resolve_account` | Which account a directory resolves to, and *why* (source, matching `bffs.toml`/rule) |
| `switch_account` | Set the global default |
| `pin_account` / `unpin_account` | Add/remove a directory rule (the `bffs path set` mechanism) |
| `check_shim` | Probe whether the shim actually wins on PATH per shell mode |
| `account_usage` | Per-account headroom heuristics: 5h/7d token burn, limit events, suggested account |
| `run_on_account` | Delegate a prompt to a headless claude session billed to another account |

So "switch me to my work account" or "why is this repo using the personal
account?" work from inside a session. One caveat, which the tools also state in
their own results: changes apply to the **next** `claude` launched through the
shim — nothing can re-point a session that is already running, including the
one making the change.

`bffs mcp install` writes the registration into `~/.claude.json` *and* into
every per-account session `.claude.json` under the bffs config dir — both are
needed because each oauth account runs with its own isolated config dir and
reads its own copy. New oauth accounts inherit the entry automatically at
`bffs login` time. The registration bakes in the absolute path of the bffs
binary and config dir, so re-run `bffs mcp install` if you move either.

(`bffs mcp serve` is the actual stdio server; Claude Code launches it itself.)

## Usage heuristics — which account has headroom?

```bash
bffs usage           # per-account table: tier, last used, 5h/7d burn, limit status, suggestion
bffs usage work      # detail: per-model and per-day breakdown, attribution sources
```

`bffs usage` estimates each account's recent Claude consumption so you can
pick the right account for a job — say, starting a long refactor on the
account that won't be interrupted mid-task: token burn in the last 5 hours
(Anthropic's session-limit window) and 7 days, detected
"you've hit your limit" events with their reset times, the account's cached
plan tier, and a `suggested:` pick — the oauth account with the lowest recent
weighted burn and no active limit. `bffs list` and `bffs show` gain a cheap
LAST-USED column from the same data, and the `account_usage` MCP tool serves
it to Claude Code sessions ("switch me to whichever account has headroom").

How it works, honestly: Claude Code's transcripts carry full token counts but
no account identity, so bffs records one small JSON line per shim launch in
`<config>/launches.jsonl` (timestamp, account, working directory; `0600`;
disable with `BFFS_NO_USAGE_LOG=1`). Sessions are then attributed by, in
order: the config tree they live in (full-isolation accounts), the
per-account session metadata Claude itself records (`lastSessionId`), and
launch-log correlation by directory + time. Anything ambiguous — including
history from before this feature existed — is reported as *unattributed*,
never guessed. All numbers are heuristics, not billing truth; Anthropic
publishes no official limit API for subscriptions.

### Delegating runs to another account

Claude Code subagents always run on the session's own credentials — but a
fresh headless claude on a different account is an effective subagent that
bills that account instead:

```bash
bffs run work                                          # interactive claude on `work`, wait, propagate exit code
bffs run work -- -p "summarize this repo" --output-format json   # headless delegated run
```

From inside a Claude Code session, the `run_on_account` MCP tool does the
same ("delegate this to whichever account has headroom" — with no `account`
it auto-picks the `account_usage` suggestion). The delegated session shares
**no conversation context** (the prompt must be self-contained), starts with
conservative headless permissions (read-only tools unless `allowed_tools`
grants more; permission bypasses are deliberately not exposed), and is
recorded in the launch log so `bffs usage` attributes it. For plain shell
scripting the shim route works too: `BFFS_ACCOUNT=work claude -p "…"`.

## Isolation presets (oauth only)

`CLAUDE_CONFIG_DIR` isolates everything in a Claude Code config tree, not just credentials. To control how much actually gets isolated vs shared with your `~/.claude/`, pick a preset at `bffs login`-time (or change later with `bffs reisolate`):

| Preset             | What's per-account               | What's symlinked back to `~/.claude/`                                            |
|--------------------|----------------------------------|----------------------------------------------------------------------------------|
| `partial` (default)| `.claude.json`, `.credentials.json` only | Every other entry in `~/.claude/` (settings, skills, plugins, history, projects, todos, …) — drop-in feel |
| `full`             | Everything                       | Nothing — fresh Claude Code world per account                                    |

Default is `partial`. Override per-account with `bffs login --preset=full`, change later with `bffs reisolate <name> --preset=full`. Set a different global default by editing the `isolation` field in `state.toml`.

The shim re-runs the symlink reconciliation on every oauth invocation, so anything claude adds to `~/.claude/` later (a new skill, a new plugin) shows up in your per-account dirs automatically. If a real file is in the way of a desired symlink (e.g. claude wrote something into the session dir directly), that path is skipped, the user's file is preserved, and a one-line warning goes to stderr.

## Why does Claude ask me again after switching accounts?

Claude Code asks two questions the first time it runs in a directory — whether
you trust the files in the folder, and "Allow external CLAUDE.md file
imports?" (raised by an `@import` in a CLAUDE.md that reaches outside the
project) — and records the answers per project in the
`.claude.json` it reads from `CLAUDE_CONFIG_DIR`. Every bffs oauth account has
its own `.claude.json`, so the answers diverge per account: `bffs switch work`
puts a `.claude.json` in front of `claude` that never saw your answer, and the
dialog comes back. Nothing is missing or moved — under `partial` isolation the
transcripts and memory are the same files for every account; only these flags
differ.

`bffs trust` shows who has answered what for the current project
(`--project <dir>`, `--all-projects`, `--json`):

```
$ bffs trust
project:  ~/build/projects/bffs        (key: /Users/jonas/build/projects/bffs)

ACCOUNT      FOLDER-TRUST  EXTERNAL-IMPORTS  TOOLS  MCP
* aviate     accepted      allowed           3      2 enabled
  innomind   -             -                 -      -
  innomind2  -             declined          -      -
  (home)     accepted      allowed           3      2 enabled

"-" = never answered on that account (claude will ask); "inherited" = a parent directory is trusted (claude will not ask). Carry answers over with:
    bffs trust sync --to innomind
```

`(home)` is `~/.claude.json` — what an unmanaged `claude` and api_key accounts
read. Folder trust is inherited from a trusted parent directory — shown as
`inherited (from <dir>)` — but never from above a git root: inside a
repository the answer that counts is the repository's own. The
external-imports answer is per exact directory. A `live:` line lists any
`claude` currently running in the project.

`bffs trust sync --to <account>` copies the answers over. The source is the
active account when it accepted the project's folder trust, else the first
account (by name) that did, else `~/.claude.json`; `--from` overrides,
`--to all` fans out to every account, `--to home` targets `~/.claude.json`:

```
$ bffs trust sync --to innomind
project ~/build/projects/bffs → account "innomind" (from "aviate"):
  hasTrustDialogAccepted                    false -> true
  hasClaudeMdExternalIncludesApproved       false -> true
  hasClaudeMdExternalIncludesWarningShown   false -> true
apply to ~/Library/Application Support/bffs/sessions/innomind/.claude.json? [y/N] y
updated 1 project in "innomind". A running claude on that account picks the change up within about a second.
(allowedTools and MCP approvals were not copied; add --include-permissions to include them.)
```

The rules: a sync never downgrades (`false → true` only) and never overrides
an explicit decline — a declined external-imports answer is copied only onto
an account that never answered; `--mirror` copies the source values verbatim
instead. `allowedTools` and MCP server approvals travel only with
`--include-permissions`, and every `mcpServers` command line is printed before
you confirm. `--dry-run` shows the plan and writes nothing; `-y` skips the
confirmation (required when there is no terminal). Nothing else in the file is
touched — not `lastSessionId`, not the identity fields. The write takes
Claude's own lock on the file and holds it well under a second, and a `claude`
already running on that account re-reads the file about once a second, so the
change lands without a restart.

You rarely need to run it by hand. `bffs switch` and `bffs show` print a
one-line note when the account in front of you still has a dialog ahead of it
for the current directory that another account already answered
(`bffs switch <name> --sync-trust` carries it over on the spot; set
`trust_hint = false` in `state.toml` to silence the note). `bffs login` seeds a
new account's `.claude.json` from `~/.claude.json` once — never again on a
`--force` re-login, so synced answers survive — and then carries the
previously active account's answers over for every project whose directory
still exists (`--no-trust-carry` skips that).

## The browser: bare `bffs`

`bffs` with no arguments opens the browser in the terminal (elsewhere, and in a `bffs_notui` build, it prints the help; `bffs sessions` and `bffs memory` print their tables). It is laid out like lazygit: three stacked panels on the left and a preview on the right.

- **1 accounts** is the perspective: selecting an account browses the pool it reads (under partial isolation that is the shared `~/.claude/projects`, so several accounts share one pool) and marks its column in the per-account tables. The active account is marked; `space` makes another one active, the way `bffs switch` does.
- **2 projects** lists the pool's projects with their session count, a `mem` tag and the newest age. Its preview is the project's drift: sessions per root, whether the memory files are the same on the other roots (by checksum), and what each account's `.claude.json` records for it — folder trust, the external-imports answer and the last-session pointer.
- **3 sessions | memory** (`[` `]` switch) lists the project's sessions (a `●` for live, `↓` for imported, `!` for a missing directory) or its memory files. A session previews its title, a one-line summary, the resume command and an excerpt, then the per-account row, the files Claude keeps for it and the identifiers; `enter` opens the full transcript. A memory file previews who reads it, how the same file compares on the other roots, its path references and its contents.

Below 96 columns the panels take the width and `enter` shows the preview; `esc` always goes back. Colours come from a theme — `default`, `catppuccin`, `gruvbox`, `tokyo-night`, `solarized` or `mono` — with light and dark variants picked from your terminal's background; `T` cycles them and remembers the choice in `state.toml`, `BFFS_THEME=<name>` overrides it and `NO_COLOR` forces mono. Every coloured signal keeps its glyph or word, so mono loses nothing. Keys: `1`–`3` or `tab`/`h`/`l` move between panels, `/` filters, `x` opens the action menu, `?` lists every key, `+`/`_` force the preview or the panels. Actions run the same engines as the commands: `e` export to a file, `s` send over the LAN, `i` receive, `c` copy to another account, `r` rehome, `R` resume in claude, `t` trust matrix, `S` sync the project's memory into a full-isolation account, `L` point an account's last session at the selected one, `p` scan memory paths. Nothing in the browser deletes; that stays `bffs sessions rm`.

## Export & import

`bffs export` packs Claude Code sessions and auto-memory into one `.bffs`
file; `bffs import` lands that file in a Claude config dir on another machine
(or under another account here). The bundle is a plain tar stream with a
manifest first, so the same bytes work on disk, on stdout and over ssh.

```bash
bffs export --out ~/Desktop/mac-a.bffs             # the current project: sessions + memory, from the pool claude would use here
bffs export --all-projects --since 30d --out auto  # every project touched in 30 days → bffs-<host>-<date>.bffs in the cwd
bffs import --from ~/Desktop/mac-a.bffs --dry-run  # show the plan; nothing is written
bffs import --from ~/Desktop/mac-a.bffs            # confirm, then import into the account claude would use here
bffs sessions imports                              # every import: bundle, source, account, sessions, pending, memory
bffs sessions list --pending-rehome                # imported sessions whose directory does not exist on this machine yet
```

No shared disk? Stream it: `bffs export --project . --out - | ssh b 'bffs import --from - -y --as-is'`
(the bundle goes to stdout, everything else to stderr; `-y` is required on the
receiving side because stdin carries the bundle, not your answer).

**What a bundle holds.** Per session: the transcript, its sidecar directory
(subagents, workflows, tool results), the `file-history/` backups, plan files
and the session's `history.jsonl` lines; per project: the auto-memory
directory (`MEMORY.md`, topic files, `logs/`). `--no-tool-results`,
`--no-file-history`, `--no-history` and `--no-plans` leave parts out —
the export summary prints the size of the three parts that can carry pasted
secrets before asking to proceed — and `--with-tasks` adds task lists. Never
exported: `.claude.json`, credentials, Keychain entries, Claude's runtime
`sessions/` files, `session-env/`, `shell-snapshots/`, `debug/`, `telemetry/`,
`stats-cache.json`, `todos/`, `scratch/`, `tiny_memory/`, `memory/proposals/`,
index caches, `allowedTools` or MCP approvals.

**Where sessions land.** A session whose original directory exists on this
machine is placed by identity — same directory, nothing rewritten, `claude
--resume` sees it exactly as before. Every other session (and everything with
`--as-is`) lands under its original slug and is flagged *pending*:
`claude --resume <id>` still finds it from any directory, and a later
`bffs rehome` moves it to the right one. The destination is the pool of the
account claude would use in the current directory (the shared
`~/.claude/projects` under partial isolation), else the active account's;
`--account <name>` overrides and `home` means `~/.claude`.

**Conflicts.** A session that already exists under the target project is
skipped by default; `--on-conflict overwrite` sets the existing transcript
and sidecar aside as `<name>.bffs-replaced-<time>` (never deleted) and is
refused while a running claude has the session open. A session with the same
id under a *different* project directory is always refused, because two
copies would make `claude --resume <id>` ambiguous. Every file is verified
against the manifest's sha256 in a staging directory first, and each session
lands transactionally — its transcript is the last file to appear — so an
interrupted import leaves a whole session or none; the next import repairs
what was in flight and lists leftover staging directories (`--clean-staging`
removes them).

**Memory is prompt content.** Imported memory files are loaded into every
future claude session for that project, so the import says so before asking,
writes them only when the project has no memory directory yet (`--memory
overwrite` sets the existing one aside), keeps `MEMORY.md` untouched and
lands topic files as `<name>.imported-<bundle>.md`, and rewrites `pinned:`
frontmatter to `pinned-imported:` so nothing arrives pinned into every
session — `--trust-memory` keeps the pins.

**Retention.** Claude deletes any transcript not touched within
`cleanupPeriodDays` (30 by default); an import raises old transcripts to
half that window and prints the date they will be swept unless resumed, while
keeping their relative order so `claude --continue` still picks the right
one. Sidecar, file-history and plan files get a fresh mtime
(`--preserve-mtimes` keeps the source's; Claude then sweeps the old ones).

Every import ends with the command that verifies it (`cd <dir> && claude
--resume <id>`) and writes a record under `<config>/imports/`, which
`bffs usage` uses to attribute the imported sessions to the account chosen
at import time.

### Placing sessions on another machine

When a bundle's directories do not exist on the importing machine, tell
`bffs import` where they live:

- `--map OLD=NEW` (repeatable) is a prefix rule: sessions and memory recorded
  under `OLD`, or below it, land under `NEW`, which must exist here. The
  longest matching rule wins, so `--map /Users/jonas=/home/jonas` covers every
  project of the old home at once. `~` works on both sides.
- `--into <dir>` is the single-project shorthand for one rule.
- Without a rule, an interactive import asks per project: a candidate list
  (same git remote, same path relative to home, same folder name), a typed
  path, or import as-is and rehome later with `bffs rehome` or `/bffs-rehome`
  in claude. `-y` and non-interactive runs never ask.

A mapped or confirmed placement appends Claude's own `relocated` record to the
transcript, merges memory into the new project's memory directory (`--memory
merge` is the default there; conflicts are kept as `*.imported-<id>.md`,
`MEMORY.md` gains an index section) and rewrites old absolute paths inside the
merged memory files (`--no-rewrite-memory` keeps them). Two opt-ins follow a
confirmed placement, and their effect is printed before the confirmation:
`--carry-trust` copies the source's folder-trust and external-imports answers
for the mapped directory into the target account's `.claude.json` (never for
`--as-is`), and `--set-last-session` points the account's `lastSessionId` at
the newest imported session. A transcript exported from a session that was
still writing may end mid-line; such a session lands as-is unless
`--force-stamp` is given.

### Same machine: copy or move between accounts

Under the default partial isolation every oauth account shares one `projects/`
pool with `~/.claude`, so there is nothing to copy between two such accounts:

```
$ bffs copy --from aviate --to innomind --project .
nothing to copy: "aviate" and "innomind" share one projects pool (partial isolation) — the transcripts and the memory
for ~/build/projects/bffs are already the same files. What differs per account is trust:
    bffs trust sync --from aviate --to innomind --project /Users/jonas/build/projects/bffs
```

A real copy happens between different roots — into or out of a full-isolation
account, or from an orphan session dir (a read-only source):

```
$ bffs copy --from home --to work --project ~/build/projects/bffs
plan: 7 sessions, 1 memory dir  from ~/.claude/projects  to  ~/Library/Application Support/bffs/sessions/work/projects
copy 7 sessions? [y/N] y
copied 7 sessions, 1 memory dir
verify:  BFFS_ACCOUNT=work claude --resume 0c5e19b2-…
```

The copy runs through the same staged, verified pipeline as `bffs import`
(`--on-conflict`, `--memory`, `--set-last-session`, `--dry-run`, `-y` work the
same way). `--move` deletes the originals only after every landed file was read
back and its digest matched the manifest; sessions a running claude has open are
held back and never touched; a move asks you to type the number of sessions
when there is more than one. After `bffs reisolate <name> --preset full` the
new root is empty — `bffs copy --from home --to <name> --all-projects` takes
your history with you.

`bffs sessions rm <sid|prefix>...` deletes sessions from the root they live in:
it lists every path first (transcript, sidecar, file-history, tasks, plan
files), refuses sessions a running claude has open, asks for the typed count
when more than one is named, and never touches memory directories or
`history.jsonl`.

## Transfer between machines

Two machines on the same Wi-Fi or Ethernet can hand a bundle over directly:
one serves, the other pulls, and a short pairing code read off the first
screen and typed on the second is the whole secret. Nothing is written to
disk in between and nothing leaves the local network.

**Machine A** (the one that has the sessions):

```
A$ cd ~/build/projects/bffs && bffs export --serve
Exporting from the shared pool (~/.claude; partial isolation: aviate, innomind):
  project ~/build/projects/bffs
    12 sessions   (newest: "Plan: session export", 2h ago)   148.2 MB   1 live (may be truncated)
      tool-results 96 MB (saved tool outputs — may contain pasted secrets)   file-history 11 MB (backups of files Claude edited)   history 41 lines (prompt history)
    memory        6 files   14 KB
  total 148.2 MB
Proceed? [y/N] y

On the other machine, run:    bffs import --from 192.168.1.20

Pairing code:   7K3Q-M9XD          (this machine's key: 3f9a1c2e — the other side shows it as "peer key")

Waiting for the other machine…  code valid for 10:00, 3 attempts, one transfer.   (Ctrl-C cancels; --show-ipv6 lists link-local addresses)
  22:41:03  192.168.1.31 connected — waiting for its code
  22:41:11  192.168.1.31 code accepted; manifest sent (12 KB) — waiting for the other side to review
  22:41:39  manifest accepted by mac-b
  sending ████████████████████ 100%   148.2 MB   42.0 MB/s
  22:41:45  delivered: 137 files verified by mac-b in 4.1s
Done.
```

**Machine B** (the one that wants them):

```
B$ bffs import --from 192.168.1.20
connected to 192.168.1.20 (TLS 1.3, peer key 3f9a1c2e) — it asks for the pairing code: ********
code accepted — the other machine proved it knows the code too
Bundle 6f1e2c0a from mac-a (jonas, darwin/arm64, bffs 0.3.0, claude 2.1.259, account "aviate", partial):
  project /Users/jonas/build/projects/bffs        exists here ✓ (same directory — no rehome needed)
    12 sessions  148.2 MB   memory 6 files   (trust: accepted on mac-a — informational)
    note: memory files in this bundle will be loaded into every future claude session for /Users/jonas/build/projects/bffs
          (pinned files arrive unpinned; --trust-memory keeps them pinned)
Target: account "work" (resolved for ~/build/projects/bffs → shared pool ~/.claude)   limit 2.0 GB   retention: 30 days (default)
Import into ~/.claude? [y/N] y
  receiving ████████████████████ 100%   148.2 MB   137 files verified (sha256)
  sessions   12 committed to projects/-Users-jonas-build-projects-bffs/; 0 skipped
  memory     6 files into ~/.claude/projects/-Users-jonas-build-projects-bffs/memory (side files: *.imported-6f1e2c0a.md; MEMORY.md untouched)
  history    41 prompt lines added
Done in 2.8s. Check it:

    claude --resume 1e005053-380a-4245-a145-52c2715afa73
    bffs sessions list --project /Users/jonas/build/projects/bffs

note: the first claude launch there asks about folder trust (and external CLAUDE.md imports, if the project's CLAUDE.md
      imports files outside the directory) — once per bffs account. `bffs trust sync --to <acct>` carries the answer to other accounts.
Import record: ~/Library/Application Support/bffs/imports/6f1e2c0a-….json  (bffs sessions imports)
```

The code is asked for only once the connection is up, so a wrong address
fails first (`could not reach 192.168.1.99:7345 within 10s — is bffs export
--serve still running there …`). A wrong code exits 2 and burns one of the
three attempts on A; three wrong codes end the serve. The importing side
shows the whole manifest and asks before requesting a single payload byte;
`--dry-run` reviews the plan and sends a "dry run" reject that A survives.
For scripts, `BFFS_TRANSFER_CODE=7K3Q-M9XD bffs import --from 192.168.1.20 -y`
takes the code from the environment (read once, then cleared); there is
deliberately no `--code` flag, because `ps` shows every argument.

**The pairing model, in two sentences.** The 40-bit code never crosses the
wire: each side proves it knows the code with an HMAC keyed by a value
derived from the code *and* this exact TLS connection, and B proves first,
so a stranger connecting to A gets one guess per connection and three per
serve, and a relay sitting between the two cannot forward either proof.
Anyone who can see A's screen within the ten minutes can pull the bundle —
the code is the whole secret, so treat the screen accordingly.

**"Local network only" means on-link.** A listens only on addresses of up,
non-loopback, non-point-to-point interfaces (no `utun*`/VPN, no `awdl*`),
accepts only peers inside those interfaces' own prefixes, and B refuses to
dial anything outside *its* own prefixes before a byte is sent:

```
refusing to pair with 203.0.113.5: not on a local network of this machine (--allow-routed for multi-VLAN offices).
Use bffs export --out file.bffs, or bffs export --out - | ssh host bffs import --from -
```

Tailscale / CGNAT ranges (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) are refused
on both sides even with `--allow-routed`, which only relaxes the on-link test
for offices where the two machines sit on different VLANs of one private
network (a warning is printed). Names work too (`--from mac-a.local`,
`--from mac-a`): they are resolved once and every address must pass the
same check, but any host on the network can answer such a name, so the
IPv4 address shown on A is the safe form. `--show-ipv6` on A also lists
link-local addresses; append the interface on B (`[fe80::…%en0]`).

**Firewalls.** A has to accept an inbound connection on port 7345 (`--port`
changes it, `0` picks a free one). On macOS the application firewall asks
once per binary; an ad-hoc-signed `/opt/bffs/bffs` asks again after every
rebuild — allow it for good with
`sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add /opt/bffs/bffs --unblockapp /opt/bffs/bffs`
(Developer ID signing makes the prompt stick). On Windows a non-administrator
is blocked silently:
`netsh advfirewall firewall add rule name=bffs dir=in action=allow program="C:\path\to\bffs.exe" protocol=tcp localport=7345 profile=private`.
On Linux with ufw: `ufw allow from 192.168.0.0/16 to any port 7345 proto tcp`.
A prints `no connection yet — if a firewall prompt appeared, allow it` after
30 s without a connection.

**No inbound port at all?** Use the pipe, which needs nothing but ssh:

```
A$ bffs export --project . --out - | ssh b 'bffs import --from - -y --as-is'
```

## Account resolution order

Highest priority wins:

1. `BFFS_ACCOUNT` env var
2. The nearest `bffs.toml` walking up from CWD
3. The most specific directory rule from `bffs path set` (`paths.toml`)
4. The global default set by `bffs switch`
5. Fall through — `claude` runs with its own credentials, untouched

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

## Development

```bash
make hooks     # one-time: enable the pre-commit hook for this checkout
make fmt       # gofmt -w .
make lint      # gofmt check + go vet, same checks CI runs
go test ./...
```

`make hooks` points `core.hooksPath` at [`.githooks/`](.githooks). The
pre-commit hook runs gofmt and `go vet` — exactly what `ci.yml` checks, and
deliberately nothing more, so it can't block a commit CI would accept.
gofmt problems are fixed and re-staged for you; only vet findings, which
can't be auto-fixed, actually stop a commit. If a file has unstaged edits
alongside its staged ones the hook formats it but won't re-stage it, since
that would pull the unstaged edits into your commit.

Bypass once with `git commit --no-verify`, or skip just the vet step with
`BFFS_HOOK_SKIP_VET=1`. Git won't run hooks from a fresh clone on its own,
so `make hooks` is opt-in per checkout.

## Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) from a tag push:

```bash
git tag -a v0.2.0 -m v0.2.0
git push origin v0.2.0
```

`.github/workflows/release.yml` then builds all six OS/arch targets, stamps
`cmd.Version` via ldflags, and publishes archives plus `checksums.txt` to the
GitHub release, with a changelog grouped from conventional-commit subjects.
Tags like `v0.2.0-rc.1` publish as prereleases automatically.

To check the release locally before tagging:

```bash
make release-check   # validate .goreleaser.yaml
make snapshot        # full cross-platform build into ./dist, publishes nothing
```

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, and `go test -race` on
Linux/macOS/Windows for every push and PR, plus a GoReleaser snapshot build so
release-config breakage surfaces before a tag is cut.
