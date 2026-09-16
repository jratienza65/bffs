# Account resolution is a fixed precedence, and the config walk is bounded

Every `claude` launch has to answer one question — which account — and
the answer has to be the same whether it comes from the shim, `bffs
show`, the MCP server or the browser. We decided on a single ordered
list in `internal/resolver`, first match wins:

1. `BFFS_ACCOUNT`
2. the nearest `bffs.toml`, walking up from the working directory
3. the most specific directory rule in `paths.toml` (`bffs path set`)
4. the global default in `state.toml` (`bffs switch`)
5. nothing — `claude` runs with its own credentials, untouched

The last entry is the one that matters most: bffs installed and
configured badly must still leave `claude` working exactly as it did
before. There is no "default account" that quietly captures a launch.

The `bffs.toml` walk stops at the first `.git` directory or at `$HOME`,
whichever comes first. Without a bound, a file anywhere above the
working directory — `/tmp/bffs.toml`, or one in a shared parent — would
decide which credentials a session runs on. `internal/projectconfig` is
read-only for that reason: nothing in bffs writes a `bffs.toml`, so a
file that appears there was put there by a person.

## Considered Options

- **Merging the sources** (project config overlaying global defaults
  field by field) — every launch would then depend on which fields were
  set where, and "why is this account active?" would have no short
  answer. `bffs show` prints one source today.
- **An unbounded upward walk**, like `.editorconfig` or `.npmrc` — the
  planting attack above, on a credential rather than a formatting rule.
- **Writing `bffs.toml` from the CLI** — a writer makes the file
  bffs-managed and invites bffs to fix up files it did not create;
  `bffs path set` covers the same need in bffs's own storage.

## Consequences

Per-project pinning has two spellings — a committed `bffs.toml` for a
team, a directory rule for one machine — and they are ranked rather than
merged. A project outside `$HOME` with no `.git` gets no project config,
which is deliberate. Any new source of truth has to take a place in this
list, and `bffs show` has to name it.
