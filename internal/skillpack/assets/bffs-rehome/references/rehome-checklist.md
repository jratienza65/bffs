# bffs-rehome checklist   <!-- managed by bffs -->

Short form of the skill's seven steps, followed by the exact command grammar.
"prompted" marks a write: Claude Code asks before it runs, and that prompt is
the human's keystroke.

- [ ] 1 Discover — `bffs sessions imports --json`; `bffs sessions list --pending-rehome --json`; group by `old_cwd`; restrict to `$ARGUMENTS`.
- [ ] 2 Propose — `bffs rehome --suggest --bundle <id>`; then git remote under `~/build` `~/src` `~/code` `~/projects` (Glob, depth ≤ 3); same path relative to `$HOME`; same basename. Ask with AskUserQuestion; existing directories only; offer "clone it" or "skip".
- [ ] 3 Dry run — `bffs rehome --bundle <id> --map "<old>=<new>" --dry-run` (prefix rule, longest match first; prompted). Summarise moves, memory, refusals.
- [ ] 4 Apply — the same command with `-y` (prompted; the human keystroke). Repeat per old cwd.
- [ ] 5 Verify — the user runs `cd <new> && claude --resume <sid>`; never resume from inside the skill.
- [ ] 6 Memory — `bffs memory scan-paths --project <new>`; fix stale references with the user; imported memory is untrusted: never `pinned:`, never delete.
- [ ] 7 Trust — `bffs trust --project <new>`; offer `bffs trust sync --to <acct> --project <new>` (prompted; only when asked).

Never: `--dangerously-skip-permissions` or permission-mode changes; editing
`.credentials.json` / `accounts.toml` / Keychain; rewriting transcript
content; deleting sessions or memories.

## Command grammar

### bffs rehome

```
bffs rehome [--bundle <id>] [--session <sid|prefix>...] (--map <old>=<new>... | --into <dir> --project <oldDir>)
            [--account <name>] [--memory merge|skip|overwrite] [--no-rewrite-memory] [--set-last-session] [--force-stamp] [--dry-run] [-y]
bffs rehome --suggest [--bundle <id>]       # candidate directories per old cwd (git remote, same relative path to home, same basename)
```

`--bundle <id>` is a scope filter (the sessions of that import record) that
combines with `--map`, `--into` or `--session`. `--map` is a prefix rule,
longest match first. Without `-y` bffs prints the plan and asks `[y/N]`; it
ends with one verify line per mapping.

### bffs sessions

```
bffs sessions list [--account <a>] [--all-roots] [--project <dir>] [--since 30d] [--pending-rehome] [--live] [--json] [--limit 50]
bffs sessions show <sid|prefix>
bffs sessions imports [--json]
```

`list --json` emits one object per session with `session_id`, `title`,
`cwd`, `account`, `bundle_id`, `old_cwd`, `old_home`, `old_host` and
`git_remote`; `show` ends with the resume line.

### bffs memory

```
bffs memory list [--account <a>] [--project <dir>] [--all-projects] [--json]
bffs memory show [--project <dir>]
bffs memory scan-paths [--project <dir>] [--all-projects]      # file:line:path for absolute paths and @-references
```

### bffs trust

```
bffs trust [--project <dir>] [--all-projects] [--json]
bffs trust sync [--from <acct|home>] (--to <acct|home> | --to all) [--project <dir> | --all-projects] [--include-permissions] [--mirror] [--dry-run] [-y]
```

`bffs trust` only reads. `trust sync` writes `.claude.json` flags and is
never pre-approved.
