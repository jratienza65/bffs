# The browser never deletes

The browser puts every session, memory file and account one keystroke
apart, which is the point of it — and it is also why a delete key there
is a different proposition than a delete command on the CLI. A
transcript is not recoverable from anywhere else: it is the record of
work that already happened. We decided the browser has no destructive
action at all. `d` is bound, but only to say so: it answers with "(not available
here; d never deletes — use bffs sessions rm)", and the same hint covers
every other reserved action key.

Deletion lives on the CLI, where it is deliberate and where the shell
history records it: `bffs sessions rm` lists every path it is about to
remove, refuses any session a `claude` is running, requires a typed
count when there is more than one, and removes through an `os.Root` over
the config dir so a name can never lead it out. `bffs copy --move` is
the only other remover, and it re-reads and checksums every landed file
against the manifest before it removes a single source file.

## Considered Options

- **A delete key with a confirmation** — the confirmation is the only
  thing between a mis-typed key and a lost transcript, and every other
  confirmation in the browser is one keystroke away from a yes.
- **A trash directory** — a second place for the same files, a sweeper
  to write, and Claude's own retention sweep already deletes on a clock.
- **Delete only what bffs imported** — the distinction is invisible at
  the moment of pressing the key, which is when it would matter.

## Consequences

The reserved-key hint is part of the browser's contract and is listed in
the `?` overlay, so the absence is discoverable rather than a silent
no-op. Anything destructive that is added later belongs on the CLI with
the same shape: list what will happen, refuse live sessions, require a
count. The browser may still *move* data — copy, rehome, import — because
each of those lands new files and leaves the originals alone.
