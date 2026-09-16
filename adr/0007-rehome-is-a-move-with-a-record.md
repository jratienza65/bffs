# Rehoming moves a session and records that it moved

A session imported from another machine points at a directory that does
not exist here, and a project that moved on this machine leaves its
sessions behind. Both want the same operation: put the transcript where
Claude will look for it now. Claude itself already has an answer for the
second case — when a project directory moves, it appends a `relocated`
record to the transcript — so we decided to do exactly that rather than
invent a second convention: move the files to the new slug directory and
append one `relocated` record naming the new cwd.

The move is ordered so that a failure at any point leaves something a
person can act on: the sidecar directory first (staged as `.bffs-tmp`,
then renamed), then file history, plans and tasks under the collision
rule, then the transcript last — written to a temporary name, stamped,
fsynced, renamed — then the modification times, then the history lines.
Every step is journalled, and recovery runs *before* the next attempt
rather than after the failure, so an interrupted rehome is repaired by
running it again. Rollback restores; it never deletes.

Two gates protect the transcript itself. A session that a `claude` is
running is never moved — bffs will not move a file it cannot prove is
closed — and a transcript whose last line is torn is refused rather than
stamped, unless `--force-stamp` says otherwise; an import demotes such a
session to an as-is placement instead. Rewriting history (`--rewrite-cwd`,
`--rewrite-file-history`) is off by default and runs as a separate step
after the move has committed, because a failure there is a warning about
old records and not a lost session.

## Considered Options

- **Rewriting `cwd` in place and leaving the file where it is** — Claude
  finds sessions by the slug directory, so the session would stay
  invisible where it is and the edit would be for nothing.
- **Copy-then-delete** — two copies of a transcript, and a crash between
  them leaves the wrong one looking authoritative.
- **Always rewriting historical `cwd` values** — the nested ones inside
  message content are part of what was said, not part of where the
  session lives; changing them edits the conversation.

## Consequences

`bffs import` and `bffs rehome` share the commit path, so a bug is fixed
once. Mapping rules are prefix rules applied longest-first at a
separator boundary, and a mapping is treated as a confirmation: memory
merges, trust can be carried, the last-session pointer can be set.
Anything unmapped stays as-is and is recorded as pending, which is what
`bffs sessions list --pending-rehome` lists later.
