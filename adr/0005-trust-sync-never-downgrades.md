# Trust sync only ever adds an answer

Claude records two answers per project inside `.claude.json` — whether
the folder is trusted, and whether external `CLAUDE.md` imports are
approved — and bffs keeps a `.claude.json` per account by design, so
both dialogs come back after a switch. `bffs trust sync` copies those
answers between accounts. Copying an answer is copying a *security
decision*, so we decided the copy is monotone: it may turn "never asked"
into "accepted", and it may never turn "accepted" into "declined" or
overwrite a decline that a person made on purpose. `--mirror` opts into
verbatim copying, and is the only way to move an answer downwards.

Two more limits fall out of the same reasoning. `lastSessionId` is never
carried — it points at a conversation, not at a permission, and a
pointer copied between accounts makes `claude --continue` open someone
else's session. Tool allow-lists and MCP approvals are not answers to a
dialog at all; they are a list of what may run without asking, so they
move only under `--include-permissions`, and every MCP command line is
printed before it is applied.

## Considered Options

- **A plain file copy of the three booleans** — the simplest thing, and
  it silently un-trusts a project on an account where the user had
  already said yes.
- **Syncing automatically on `bffs switch`** — the dialogs would stop
  appearing, which is the ask, but a switch is not consent, and the
  hint that `switch` and `show` print is enough to start the sync.
- **Carrying everything in `.claude.json`** — metrics, onboarding state
  and tips history are not decisions and differ per account by design.

## Consequences

`trust.Plan` is a pure function over both files and is what the tests
assert; `Apply` takes Claude's own mkdir lock (`<file>.lock`) so a
running session's write cannot be lost. A sync can therefore be a no-op
that says "no changes" — the expected outcome when both accounts already
agree. `bffs login` seeds a new account's `.claude.json` from home only
when the file does not exist yet (`--no-trust-carry` opts out), which is
the same rule at creation time. The MCP server exposes `trust_status`
and never a writer: a permission decision stays a human one on the CLI.
